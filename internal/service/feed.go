package service

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/jackc/pgx/v5"
	"net/http"
	"strconv"
	"sync"
	"time"
)

type feedMessage struct {
	Cursor string
	Event  M
	Resync bool
}
type feedHub struct {
	mu      sync.Mutex
	clients map[chan feedMessage]bool
	stop    chan struct{}
	last    int64
	epoch   string
}

func (s *Server) subscribe(ctx context.Context, p string) (chan feedMessage, func(), error) {
	s.hubMu.Lock()
	defer s.hubMu.Unlock()
	if s.hubs == nil {
		s.hubs = map[string]*feedHub{}
	}
	h := s.hubs[p]
	if h == nil {
		h = &feedHub{clients: map[chan feedMessage]bool{}, stop: make(chan struct{})}
		e := s.DB.QueryRow(ctx, "SELECT m.epoch,p.revision FROM projects p CROSS JOIN instance_metadata m WHERE p.id=$1", p).Scan(&h.epoch, &h.last)
		if e != nil {
			return nil, nil, e
		}
		s.hubs[p] = h
		go s.poll(p, h)
	}
	ch := make(chan feedMessage, 100)
	h.mu.Lock()
	h.clients[ch] = true
	h.mu.Unlock()
	cancel := func() {
		s.hubMu.Lock()
		defer s.hubMu.Unlock()
		h.mu.Lock()
		delete(h.clients, ch)
		empty := len(h.clients) == 0
		h.mu.Unlock()
		if empty && s.hubs[p] == h {
			delete(s.hubs, p)
			close(h.stop)
		}
	}
	return ch, cancel, nil
}
func (s *Server) poll(p string, h *feedHub) {
	defer s.retireHub(p, h)
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-h.stop:
			return
		case <-ticker.C:
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		tx, e := s.DB.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
		if e != nil {
			cancel()
			continue
		}
		var ep string
		var floor int64
		e = tx.QueryRow(ctx, "SELECT m.epoch,p.event_floor_revision FROM projects p CROSS JOIN instance_metadata m WHERE p.id=$1", p).Scan(&ep, &floor)
		if e == nil && (ep != h.epoch || h.last < floor) {
			s.retireHub(p, h)
			h.broadcast(feedMessage{Resync: true})
			tx.Rollback(ctx)
			cancel()
			return
		}
		var events []M
		if e == nil {
			events, e = objects(ctx, tx, "SELECT revision,type,resource_id,resource_version,actor_id,created_at FROM project_events WHERE project_id=$1 AND revision>$2 ORDER BY revision LIMIT 100", p, h.last)
		}
		tx.Rollback(ctx)
		cancel()
		if e != nil {
			continue
		}
		for _, ev := range events {
			rev := number(ev["revision"])
			h.last = rev
			h.broadcast(feedMessage{Cursor: cursor(ep, p, rev), Event: ev})
		}
	}
}
func number(v any) int64 {
	switch n := v.(type) {
	case json.Number:
		i, _ := n.Int64()
		return i
	case float64:
		return int64(n)
	}
	i, _ := strconv.ParseInt(fmt.Sprint(v), 10, 64)
	return i
}
func (h *feedHub) broadcast(m feedMessage) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.clients {
		select {
		case ch <- m:
		default:
			close(ch)
			delete(h.clients, ch)
		}
	}
}
func (s *Server) stream(w http.ResponseWriter, r *http.Request, a access, p string) {
	after := r.URL.Query().Get("after")
	last := r.Header.Get("Last-Event-ID")
	if last != "" {
		if after != "" && after != last {
			writeError(w, fail(400, "ambiguous_cursor"))
			return
		}
		after = last
	}
	ch, cancel, e := s.subscribe(r.Context(), p)
	if e != nil {
		writeError(w, e)
		return
	}
	defer cancel()
	// Register before reading the snapshot; queued events cover the replay/live boundary.
	replay := r.Clone(r.Context())
	u := *r.URL
	replay.URL = &u
	query := u.Query()
	query.Set("after", after)
	query.Set("limit", "100")
	replay.URL.RawQuery = query.Encode()
	v, e := s.read(replay, a, p, []string{"events"})
	if e != nil {
		writeError(w, e)
		return
	}
	batch := v.(M)
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(200)
	rc := http.NewResponseController(w)
	started, lastAuth := time.Now(), time.Now()
	send := func(text string) error {
		if time.Since(started) >= 5*time.Minute {
			return context.DeadlineExceeded
		}
		if time.Since(lastAuth) >= 5*time.Second {
			ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
			_, err := authenticate(ctx, s.DB, r.Header.Get("Authorization"))
			cancel()
			if err != nil {
				return err
			}
			lastAuth = time.Now()
		}
		if e := rc.SetWriteDeadline(time.Now().Add(5 * time.Second)); e != nil {
			return e
		}
		if _, e := fmt.Fprint(w, text); e != nil {
			return e
		}
		return rc.Flush()
	}
	done := time.NewTimer(5 * time.Minute)
	defer done.Stop()
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	auth := time.NewTicker(5 * time.Second)
	defer auth.Stop()
	current := after
	var rev int64
	if after != "" {
		parts := splitCursor(after)
		rev = parts
	}
	for {
		for _, ev := range batch["items"].([]M) {
			n := number(ev["revision"])
			if n <= rev {
				continue
			}
			current = cursor(batch["epoch"].(string), p, n)
			if e = send(fmt.Sprintf("id: %s\nevent: %s\ndata: %s\n\n", current, ev["type"], jsonBytes(ev))); e != nil {
				return
			}
			rev = n
		}
		if len(batch["items"].([]M)) < 100 {
			break
		}
		query.Set("after", batch["next_cursor"].(string))
		replay.URL.RawQuery = query.Encode()
		v, e = s.read(replay, a, p, []string{"events"})
		if e != nil {
			send("event: resync_required\ndata: {}\n\n")
			return
		}
		batch = v.(M)
		select {
		case <-r.Context().Done():
			return
		case <-done.C:
			return
		case <-auth.C:
			if _, e = authenticate(r.Context(), s.DB, r.Header.Get("Authorization")); e != nil {
				return
			}
		default:
		}
	}
	if e = send(": connected\n\n"); e != nil {
		return
	}
	for {
		select {
		case <-r.Context().Done():
			return
		case <-done.C:
			return
		case <-heartbeat.C:
			if send(": heartbeat\n\n") != nil {
				return
			}
		case <-auth.C:
			if _, e = authenticate(r.Context(), s.DB, r.Header.Get("Authorization")); e != nil {
				return
			}
			var ep string
			var floor int64
			if e = s.DB.QueryRow(r.Context(), "SELECT m.epoch,p.event_floor_revision FROM projects p CROSS JOIN instance_metadata m WHERE p.id=$1", p).Scan(&ep, &floor); e != nil {
				return
			}
			if ep != batch["epoch"].(string) || rev < floor {
				send("event: resync_required\ndata: {}\n\n")
				return
			}
		case msg, ok := <-ch:
			if !ok {
				return
			}
			if msg.Resync {
				send("event: resync_required\ndata: {}\n\n")
				return
			}
			n := number(msg.Event["revision"])
			if n <= rev {
				continue
			}
			if send(fmt.Sprintf("id: %s\nevent: %s\ndata: %s\n\n", msg.Cursor, msg.Event["type"], jsonBytes(msg.Event))) != nil {
				return
			}
			rev = n
			current = msg.Cursor
		}
	}
}
func splitCursor(s string) int64 {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == ':' {
			n, _ := strconv.ParseInt(s[i+1:], 10, 64)
			return n
		}
	}
	return 0
}

// Prune preserves an explicit replay floor even when no events remain.
func (s *Server) Prune(ctx context.Context) error {
	rows, e := s.DB.Query(ctx, "SELECT id FROM projects")
	if e != nil {
		return e
	}
	ps := []string{}
	for rows.Next() {
		var p string
		rows.Scan(&p)
		ps = append(ps, p)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return e
	}
	for _, p := range ps {
		e = s.pruneProject(ctx, p)
		if e != nil {
			return e
		}
	}
	_, e = s.DB.Exec(ctx, "DELETE FROM idempotency_keys WHERE expires_at<clock_timestamp()")
	return e
}
func (s *Server) pruneProject(ctx context.Context, p string) error {
	tx, e := s.DB.Begin(ctx)
	if e != nil {
		return e
	}
	defer tx.Rollback(ctx)
	if _, e = tx.Exec(ctx, "SELECT revision FROM projects WHERE id=$1 FOR UPDATE", p); e != nil {
		return e
	}
	_, e = tx.Exec(ctx, `WITH boundary AS (SELECT COALESCE(max(revision),0) AS floor FROM project_events WHERE project_id=$1 AND (created_at<clock_timestamp()-interval '7 days' OR revision<=(SELECT revision-1000000 FROM projects WHERE id=$1))) UPDATE projects SET event_floor_revision=GREATEST(event_floor_revision,(SELECT floor FROM boundary)) WHERE id=$1`, p)
	if e != nil {
		return e
	}
	if _, e = tx.Exec(ctx, "DELETE FROM project_events WHERE project_id=$1 AND revision<=(SELECT event_floor_revision FROM projects WHERE id=$1)", p); e != nil {
		return e
	}
	return tx.Commit(ctx)
}

func (s *Server) retireHub(p string, h *feedHub) {
	s.hubMu.Lock()
	defer s.hubMu.Unlock()
	if s.hubs[p] == h {
		delete(s.hubs, p)
	}
}
