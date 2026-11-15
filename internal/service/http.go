package service

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
)

type mutation struct {
	tx    pgx.Tx
	a     access
	p, ep string
	rev   int64
	now   time.Time
	r     *http.Request
	body  []byte
}
type result struct {
	body         M
	status       int
	kind, target string
	version      int64
	secret       bool
}

func outcome(kind, target string, v int64, b M) result {
	if b == nil {
		b = M{"id": target, "version": v}
	}
	return result{b, 200, kind, target, v, false}
}
func decode(b []byte, v any) error {
	if len(b) == 0 {
		b = []byte("{}")
	}
	decoder := json.NewDecoder(bytes.NewReader(b))
	decoder.UseNumber()
	if e := decoder.Decode(v); e != nil {
		return fail(400, "invalid_json")
	}
	return nil
}
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	if r.URL.Path == "/healthz" {
		writeJSON(w, 200, M{"ok": true})
		return
	}
	if r.URL.Path == "/readyz" {
		if s.Ready(r.Context()) != nil {
			writeJSON(w, 503, M{"error": "schema_unavailable"})
		} else {
			writeJSON(w, 200, M{"ready": true})
		}
		return
	}
	if r.URL.Path == "/" && r.Method == "GET" {
		s.ui(w, r)
		return
	}
	a, e := authenticate(r.Context(), s.DB, r.Header.Get("Authorization"))
	if e != nil {
		writeError(w, e)
		return
	}
	if !s.allow(a.ID, time.Now()) {
		w.Header().Set("Retry-After", "1")
		writeError(w, fail(429, "rate_limited"))
		return
	}
	if r.URL.Path == "/v1/me" && r.Method == "GET" {
		writeJSON(w, 200, M{"token_id": a.ID, "project_id": a.Project, "principal_id": a.Actor, "role": a.Role, "expires_at": a.Expires})
		return
	}
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) == 2 && parts[0] == "v1" && parts[1] == "projects" && r.Method == "POST" {
		if a.Role != "instance_admin" {
			writeError(w, fail(403, "forbidden"))
			return
		}
		s.mutate(w, r, a, "")
		return
	}
	if len(parts) < 4 || parts[0] != "v1" || parts[1] != "projects" {
		writeError(w, fail(404, "not_found"))
		return
	}
	p := parts[2]
	if a.Project != p {
		writeError(w, fail(404, "not_found"))
		return
	}
	if r.Method == "GET" {
		if len(parts) == 5 && parts[3] == "events" && parts[4] == "stream" {
			s.stream(w, r, a, p)
			return
		}
		v, e := s.read(r, a, p, parts[3:])
		if e != nil {
			writeError(w, e)
		} else {
			if obj, ok := v.(M); ok {
				if ver, ok := obj["version"]; ok {
					w.Header().Set("ETag", `"`+toString(ver)+`"`)
				}
			}
			writeJSON(w, 200, v)
		}
		return
	}
	if ranks[a.Role] < 2 {
		writeError(w, fail(403, "forbidden"))
		return
	}
	s.mutate(w, r, a, p)
}
func (s *Server) mutate(w http.ResponseWriter, r *http.Request, a access, p string) {
	b, e := io.ReadAll(http.MaxBytesReader(w, r.Body, 256*1024))
	if e != nil {
		writeError(w, fail(413, "request_too_large"))
		return
	}
	if len(b) == 0 {
		b = []byte("{}")
	}
	if !utf8.Valid(b) || !json.Valid(b) {
		writeError(w, fail(400, "invalid_json"))
		return
	}
	key := r.Header.Get("Idempotency-Key")
	if key == "" || len(key) > 200 {
		writeError(w, fail(400, "idempotency_key_required"))
		return
	}
	ctx := r.Context()
	tx, e := s.DB.Begin(ctx)
	if e != nil {
		writeError(w, e)
		return
	}
	defer tx.Rollback(ctx)
	m := mutation{tx: tx, a: a, p: p, r: r, body: b}
	e = tx.QueryRow(ctx, "SELECT epoch FROM instance_metadata WHERE singleton FOR SHARE").Scan(&m.ep)
	if e == nil {
		if p == "" {
			_, e = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(6791833)")
		} else {
			e = tx.QueryRow(ctx, "SELECT revision FROM projects WHERE id=$1 FOR UPDATE", p).Scan(&m.rev)
		}
	}
	if e != nil {
		writeError(w, e)
		return
	}
	m.a, e = authenticate(ctx, tx, r.Header.Get("Authorization"))
	if e != nil {
		writeError(w, e)
		return
	}
	// clock_timestamp is sampled after acquiring the serialization lock.
	if e = tx.QueryRow(ctx, "SELECT clock_timestamp()").Scan(&m.now); e != nil {
		writeError(w, e)
		return
	}
	reqHash := hash(r.Method + "\n" + r.URL.RequestURI() + "\n" + r.Header.Get("If-Match") + "\n" + r.Header.Get("If-None-Match") + "\n" + string(b))
	var saved []byte
	var savedHash []byte
	var status int
	var sec bool
	e = tx.QueryRow(ctx, "SELECT request_hash,response_status,response_body,secret FROM idempotency_keys WHERE token_id=$1 AND key=$2 AND expires_at>clock_timestamp()", a.ID, key).Scan(&savedHash, &status, &saved, &sec)
	if e == nil {
		if subtle.ConstantTimeCompare(savedHash, reqHash) != 1 {
			writeError(w, fail(409, "idempotency_conflict"))
			return
		}
		if sec {
			var obj M
			decode(saved, &obj)
			writeJSON(w, 409, M{"error": "secret_not_replayable", "created": obj})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(status)
		w.Write(saved)
		return
	}
	if !errors.Is(e, pgx.ErrNoRows) {
		writeError(w, e)
		return
	}
	m.rev++
	out, e := m.dispatch()
	if e != nil {
		writeError(w, e)
		return
	}
	if m.p != "" {
		if _, e = tx.Exec(ctx, "UPDATE projects SET revision=$2 WHERE id=$1", m.p, m.rev); e == nil {
			_, e = tx.Exec(ctx, "INSERT INTO project_events(project_id,revision,type,resource_id,resource_version,actor_id) VALUES($1,$2,$3,$4,$5,$6)", m.p, m.rev, out.kind, out.target, out.version, m.a.Actor)
		}
		if e == nil {
			_, e = tx.Exec(ctx, "SELECT pg_notify('agent_context_changes',$1)", cursor(m.ep, m.p, m.rev))
		}
	}
	if e != nil {
		writeError(w, e)
		return
	}
	out.body["revision"] = m.rev
	out.body["epoch"] = m.ep
	storeBody := M{}
	for k, v := range out.body {
		if k != "token" && k != "lease_secret" {
			storeBody[k] = v
		}
	}
	_, e = tx.Exec(ctx, "INSERT INTO idempotency_keys(token_id,key,request_hash,response_status,response_body,secret) VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT(token_id,key) DO UPDATE SET request_hash=excluded.request_hash,response_status=excluded.response_status,response_body=excluded.response_body,secret=excluded.secret,expires_at=excluded.expires_at", a.ID, key, reqHash, out.status, jsonBytes(storeBody), out.secret)
	if e == nil {
		e = tx.Commit(ctx)
	}
	if e != nil {
		writeError(w, e)
		return
	}
	writeJSON(w, out.status, out.body)
}
func (m *mutation) ctx() context.Context { return m.r.Context() }
func (m *mutation) audit(action, target, reason string) error {
	_, e := m.tx.Exec(m.ctx(), "INSERT INTO audit_entries(project_id,actor_id,action,target,reason) VALUES(NULLIF($1,''),$2,$3,$4,$5)", m.p, m.a.Actor, action, target, reason)
	return e
}
func (m *mutation) dispatch() (result, error) {
	if m.p == "" {
		return m.createProject()
	}
	parts := strings.Split(strings.TrimPrefix(m.r.URL.Path, "/v1/projects/"+m.p+"/"), "/")
	switch parts[0] {
	case "tokens":
		return m.token(parts[1:])
	case "contexts":
		return m.context(parts[1:])
	case "state-pins":
		return m.pin(parts[1:])
	case "tasks":
		return m.task(parts[1:])
	case "blockers":
		return m.resolve(parts[1:])
	case "resources":
		return m.resource(parts[1:])
	}
	return result{}, fail(404, "not_found")
}
