package service

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type fixture struct {
	t               *testing.T
	s               *Server
	http            *httptest.Server
	admin, token, p string
}

func setup(t *testing.T) *fixture {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set TEST_DATABASE_URL for isolated PostgreSQL integration tests")
	}
	ctx := context.Background()
	root, e := pgxpool.New(ctx, dsn)
	if e != nil {
		t.Fatal(e)
	}
	schema := id("test")
	if _, e = root.Exec(ctx, "CREATE SCHEMA "+schema); e != nil {
		t.Fatal(e)
	}
	cfg, e := pgxpool.ParseConfig(dsn)
	if e != nil {
		t.Fatal(e)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	cfg.MaxConns = 16
	db, e := pgxpool.NewWithConfig(ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	s := &Server{DB: db}
	if e = s.Migrate(ctx); e != nil {
		t.Fatal(e)
	}
	f := &fixture{t: t, s: s}
	f.http = httptest.NewServer(s)
	t.Cleanup(func() {
		f.http.Close()
		db.Close()
		root.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
		root.Close()
	})
	f.admin, e = s.Bootstrap(ctx, false, "")
	if e != nil {
		t.Fatal(e)
	}
	out := f.ok("POST", "/v1/projects", f.admin, M{"name": "team"}, nil, 201)
	f.p = out["id"].(string)
	f.token = out["token"].(string)
	return f
}
func (f *fixture) path(s string) string { return "/v1/projects/" + f.p + s }
func (f *fixture) request(method, path, token string, body any, headers map[string]string) (int, M) {
	var r io.Reader
	if body != nil {
		r = strings.NewReader(string(jsonBytes(body)))
	}
	req, e := http.NewRequest(method, f.http.URL+path, r)
	if e != nil {
		f.t.Fatal(e)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Idempotency-Key", id("key"))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, e := http.DefaultClient.Do(req)
	if e != nil {
		f.t.Fatal(e)
	}
	defer resp.Body.Close()
	b, e := io.ReadAll(resp.Body)
	if e != nil {
		f.t.Fatal(e)
	}
	var out M
	if e = json.Unmarshal(b, &out); e != nil {
		f.t.Fatalf("HTTP %d invalid response %s", resp.StatusCode, b)
	}
	return resp.StatusCode, out
}
func (f *fixture) ok(method, path, token string, body any, headers map[string]string, status int) M {
	f.t.Helper()
	got, out := f.request(method, path, token, body, headers)
	if got != status {
		f.t.Fatalf("%s %s: got %d want %d: %v", method, path, got, status, out)
	}
	return out
}
func (f *fixture) context() M {
	return f.ok("POST", f.path("/contexts"), f.token, M{"kind": "state", "title": "採用構成", "body": "日本語本文", "tags": []string{"database"}}, nil, 201)
}
func (f *fixture) task(deps []string) M {
	return f.ok("POST", f.path("/tasks"), f.token, M{"title": "implement", "acceptance": "tests pass", "depends_on": deps}, nil, 201)
}
func (f *fixture) issue(role string) M {
	return f.ok("POST", f.path("/tokens"), f.token, M{"display_name": role, "role": role}, nil, 201)
}
func match(v int) map[string]string { return map[string]string{"If-Match": fmt.Sprintf(`"%d"`, v)} }
func TestConcurrentUpdatesHistoryAndRollback(t *testing.T) {
	f := setup(t)
	c := f.context()
	i := c["id"].(string)
	path := f.path("/contexts/" + i)
	var wg sync.WaitGroup
	codes := make(chan int, 2)
	for n := 0; n < 2; n++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			code, _ := f.request("PATCH", path, f.token, M{"body": fmt.Sprint(n)}, match(1))
			codes <- code
		}(n)
	}
	wg.Wait()
	close(codes)
	counts := map[int]int{}
	for c := range codes {
		counts[c]++
	}
	if counts[200] != 1 || counts[412] != 1 {
		t.Fatal(counts)
	}
	f.ok("GET", path+"?version=1", f.token, nil, nil, 200)
	f.ok("PATCH", path, f.token, M{"body": "missing"}, nil, 428)
	before := f.ok("GET", f.path("/snapshot"), f.token, nil, nil, 200)["revision"]
	if _, e := f.s.DB.Exec(context.Background(), "ALTER TABLE project_events ADD CONSTRAINT reject_updates CHECK(type <> 'context.updated') NOT VALID"); e != nil {
		t.Fatal(e)
	}
	f.ok("PATCH", path, f.token, M{"body": "rollback"}, match(2), 500)
	got := f.ok("GET", path, f.token, nil, nil, 200)
	if got["version"] != float64(2) {
		t.Fatal(got)
	}
	after := f.ok("GET", f.path("/snapshot"), f.token, nil, nil, 200)["revision"]
	if before != after {
		t.Fatal("rollback revision changed")
	}
	var versions int
	f.s.DB.QueryRow(context.Background(), "SELECT count(*) FROM context_versions WHERE context_id=$1", i).Scan(&versions)
	if versions != 2 {
		t.Fatal(versions)
	}
}
func TestIdempotencySecretsAndPermissions(t *testing.T) {
	f := setup(t)
	path := f.path("/contexts")
	key := map[string]string{"Idempotency-Key": "stable"}
	body := M{"kind": "plan", "title": "test", "body": "hello"}
	a := f.ok("POST", path, f.token, body, key, 201)
	b := f.ok("POST", path, f.token, body, key, 201)
	if a["id"] != b["id"] {
		t.Fatal("duplicate create")
	}
	body["body"] = "different"
	f.ok("POST", path, f.token, body, key, 409)
	tok := f.ok("POST", f.path("/tokens"), f.token, M{"display_name": "one", "role": "editor"}, key, 409)
	if tok["error"] != "idempotency_conflict" {
		t.Fatal(tok)
	}
	key["Idempotency-Key"] = "token-key"
	body = M{"display_name": "editor", "role": "editor"}
	tok = f.ok("POST", f.path("/tokens"), f.token, body, key, 201)
	replay := f.ok("POST", f.path("/tokens"), f.token, body, key, 409)
	if replay["error"] != "secret_not_replayable" || strings.Contains(string(jsonBytes(replay)), tok["token"].(string)) {
		t.Fatal(replay)
	}
	viewer := f.issue("viewer")["token"].(string)
	f.ok("POST", f.path("/contexts"), viewer, M{}, nil, 403)
	editor := tok["token"].(string)
	f.ok("POST", f.path("/tokens"), editor, M{"display_name": "root", "role": "instance_admin"}, nil, 403)
	f.ok("POST", f.path("/tokens"), f.token, M{"display_name": "root", "role": "instance_admin"}, nil, 400)
	f.ok("GET", "/v1/projects/another/snapshot", editor, nil, nil, 404)
	f.ok("POST", f.path("/tokens/"+tok["id"].(string)+"/revoke"), f.token, M{}, nil, 200)
	f.ok("GET", f.path("/snapshot"), editor, nil, nil, 401)
	var count int
	f.s.DB.QueryRow(context.Background(), "SELECT count(*) FROM tokens WHERE secret_hash=$1", hash(strings.Split(tok["token"].(string), "_")[2])).Scan(&count)
	if count != 1 {
		t.Fatal("token hash absent")
	}
	expired := f.issue("viewer")
	f.s.DB.Exec(context.Background(), "UPDATE tokens SET expires_at=clock_timestamp()-interval '1 second' WHERE id=$1", expired["id"])
	f.ok("GET", f.path("/snapshot"), expired["token"].(string), nil, nil, 401)
}
func TestPinsSummariesSearchAndPack(t *testing.T) {
	f := setup(t)
	c := f.context()
	i := c["id"].(string)
	f.ok("PUT", f.path("/state-pins/current"), f.token, M{"context_id": i, "version": 1}, map[string]string{"If-None-Match": "*"}, 200)
	summary := f.ok("POST", f.path("/contexts"), f.token, M{"kind": "handoff", "title": "summary", "summary": true, "references": []M{{"id": i, "version": 1}}}, nil, 201)
	f.ok("PATCH", f.path("/contexts/"+i), f.token, M{"body": "new body"}, match(1), 200)
	snap := f.ok("GET", f.path("/snapshot"), f.token, nil, nil, 200)
	pin := snap["state_contexts"].([]any)[0].(map[string]any)
	if pin["context_version"] != float64(1) || pin["stale"] != true {
		t.Fatal(pin)
	}
	stale := f.ok("GET", f.path("/contexts/"+summary["id"].(string)), f.token, nil, nil, 200)
	if stale["stale"] != true {
		t.Fatal(stale)
	}
	found := f.ok("GET", f.path("/contexts?q="+url.QueryEscape("採用")), f.token, nil, nil, 200)
	if len(found["items"].([]any)) != 1 {
		t.Fatal(found)
	}
	for _, n := range []int{512, 1024, 2048, 32768} {
		_, pack := f.request("GET", f.path(fmt.Sprintf("/context-pack?max_bytes=%d", n)), f.token, nil, nil)
		if pack["error"] == nil {
			if len(jsonBytes(pack)) > n {
				t.Fatal("pack exceeds byte limit")
			}
			for _, x := range pack["items"].([]any) {
				v := x.(map[string]any)
				if v["pinned"] == true && v["id"] == i && v["version"] != float64(1) {
					t.Fatal("pin advanced")
				}
			}
		}
	}
	f.ok("POST", f.path("/contexts"), f.token, M{"kind": "handoff", "title": "bad summary", "summary": true}, nil, 400)
	editor := f.issue("editor")["token"].(string)
	f.ok("PUT", f.path("/state-pins/other"), editor, M{"context_id": i, "version": 1}, map[string]string{"If-None-Match": "*"}, 403)
}
func TestDAGBlockersClaimAndRecovery(t *testing.T) {
	f := setup(t)
	a := f.task(nil)["id"].(string)
	b := f.task(nil)["id"].(string)
	var wg sync.WaitGroup
	codes := make(chan int, 2)
	for _, pair := range [][2]string{{a, b}, {b, a}} {
		wg.Add(1)
		go func(pair [2]string) {
			defer wg.Done()
			c, _ := f.request("PATCH", f.path("/tasks/"+pair[0]), f.token, M{"depends_on": []string{pair[1]}}, match(1))
			codes <- c
		}(pair)
	}
	wg.Wait()
	close(codes)
	counts := map[int]int{}
	for c := range codes {
		counts[c]++
	}
	if counts[200] != 1 || counts[409] != 1 {
		t.Fatal(counts)
	}
	task := f.task(nil)["id"].(string)
	path := f.path("/tasks/" + task)
	blk := f.ok("POST", path+"/blockers", f.token, M{"reason": "CI", "check_after": time.Now().UTC().Format(time.RFC3339)}, nil, 200)
	f.ok("POST", path+"/claim", f.token, M{}, nil, 409)
	evidence := f.context()
	f.ok("POST", f.path("/blockers/"+blk["id"].(string)+"/resolve"), f.token, M{"evidence": M{"id": evidence["id"], "version": 1}}, nil, 200)
	actors := []string{f.token, f.issue("editor")["token"].(string)}
	codes = make(chan int, 2)
	results := make(chan M, 2)
	for _, tok := range actors {
		wg.Add(1)
		go func(tok string) {
			defer wg.Done()
			c, v := f.request("POST", path+"/claim", tok, M{}, nil)
			codes <- c
			if c == 200 {
				v["actor_token"] = tok
				results <- v
			}
		}(tok)
	}
	wg.Wait()
	close(codes)
	counts = map[int]int{}
	for c := range codes {
		counts[c]++
	}
	if counts[200] != 1 || counts[409] != 1 {
		t.Fatal(counts)
	}
	lease := <-results
	owner := lease["actor_token"].(string)
	bad := M{"lease_generation": lease["lease_generation"], "lease_secret": "bad"}
	f.ok("POST", path+"/renew", owner, bad, nil, 409)
	f.s.DB.Exec(context.Background(), "UPDATE tasks SET lease_expires_at=clock_timestamp()-interval '1 second' WHERE project_id=$1 AND id=$2", f.p, task)
	f.ok("PATCH", path, f.token, M{"state": "todo"}, match(2), 409)
	f.ok("POST", path+"/claim", f.token, M{}, nil, 409)
	newLease := f.ok("POST", path+"/claim", f.token, M{"recovery_note": "checked portal", "external_state": "no running operation"}, nil, 200)
	old := M{"lease_generation": lease["lease_generation"], "lease_secret": lease["lease_secret"], "evidence": M{"id": evidence["id"], "version": 1}}
	f.ok("POST", path+"/complete", owner, old, nil, 409)
	rotated := f.ok("POST", path+"/rotate-lease", f.token, M{}, nil, 200)
	old["lease_generation"] = newLease["lease_generation"]
	old["lease_secret"] = newLease["lease_secret"]
	f.ok("POST", path+"/complete", f.token, old, nil, 409)
	old["lease_generation"] = rotated["lease_generation"]
	old["lease_secret"] = rotated["lease_secret"]
	f.ok("POST", path+"/complete", f.token, old, nil, 200)
}
func TestResourceCheckpointRotateRestore(t *testing.T) {
	f := setup(t)
	path := f.path("/resources/benchmark/team-env")
	key := map[string]string{"Idempotency-Key": "claim"}
	lease := f.ok("POST", path+"/claim", f.token, M{}, key, 200)
	replay := f.ok("POST", path+"/claim", f.token, M{}, key, 409)
	if replay["error"] != "secret_not_replayable" {
		t.Fatal(replay)
	}
	proof := M{"lease_generation": lease["lease_generation"], "lease_secret": lease["lease_secret"], "external_job": M{"state": "submitting", "idempotency_key": "external-job-key", "subject": "sha"}}
	f.ok("POST", path+"/checkpoint", f.token, proof, nil, 200)
	proof["external_job"] = M{"state": "running", "id": "job-42"}
	f.ok("POST", path+"/checkpoint", f.token, proof, nil, 200)
	snap := f.ok("GET", f.path("/snapshot"), f.token, nil, nil, 200)
	before := snap["cursor"].(string)
	if _, e := f.s.RestoreEpoch(context.Background(), "restored backup"); e != nil {
		t.Fatal(e)
	}
	f.ok("GET", f.path("/events?after="+before), f.token, nil, nil, 409)
	f.ok("POST", path+"/renew", f.token, proof, nil, 409)
	f.ok("POST", path+"/claim", f.token, M{}, nil, 409)
	f.ok("POST", path+"/claim", f.token, M{"recovery_note": "job checked", "external_state": "job-42 completed"}, nil, 200)
	// A fresh application instance reads durable state, not process memory.
	restarted := httptest.NewServer(&Server{DB: f.s.DB})
	defer restarted.Close()
	old := f.http
	f.http = restarted
	out := f.ok("GET", f.path("/snapshot"), f.token, nil, nil, 200)
	f.http = old
	if out["resources"].([]any)[0].(map[string]any)["external_job"].(map[string]any)["id"] != "job-42" {
		t.Fatal(out)
	}
}
func TestCursorReplayRetentionAndIsolation(t *testing.T) {
	f := setup(t)
	snap := f.ok("GET", f.path("/snapshot"), f.token, nil, nil, 200)
	after := snap["cursor"].(string)
	for i := 0; i < 3; i++ {
		f.context()
	}
	events := f.ok("GET", f.path("/events?limit=2&after="+after), f.token, nil, nil, 200)
	if len(events["items"].([]any)) != 2 {
		t.Fatal(events)
	}
	events2 := f.ok("GET", f.path("/events?after="+events["next_cursor"].(string)), f.token, nil, nil, 200)
	if len(events2["items"].([]any)) != 1 {
		t.Fatal(events2)
	}
	f.ok("GET", f.path("/events?after="+strings.Replace(after, f.p, "another", 1)), f.token, nil, nil, 400)
	other := f.ok("POST", "/v1/projects", f.admin, M{"name": "private"}, nil, 201)
	op, ot := other["id"].(string), other["token"].(string)
	foreign := f.ok("POST", "/v1/projects/"+op+"/contexts", ot, M{"kind": "state", "title": "private"}, nil, 201)
	f.ok("GET", f.path("/contexts/"+foreign["id"].(string)), f.token, nil, nil, 404)
	f.ok("PUT", f.path("/state-pins/foreign"), f.token, M{"context_id": foreign["id"], "version": 1}, map[string]string{"If-None-Match": "*"}, 404)
	foreignTask := f.ok("POST", "/v1/projects/"+op+"/tasks", ot, M{"title": "private", "acceptance": "pass"}, nil, 201)
	f.ok("POST", f.path("/tasks"), f.token, M{"title": "bad", "acceptance": "pass", "depends_on": []any{foreignTask["id"]}}, nil, 404)
	f.s.DB.Exec(context.Background(), "UPDATE project_events SET created_at=clock_timestamp()-interval '8 days' WHERE project_id=$1", f.p)
	if e := f.s.Prune(context.Background()); e != nil {
		t.Fatal(e)
	}
	f.ok("GET", f.path("/events?after="+after), f.token, nil, nil, 410)
}
func TestSSESnapshotGapReconnectAndRevocation(t *testing.T) {
	f := setup(t)
	viewer := f.issue("viewer")
	token := viewer["token"].(string)
	snap := f.ok("GET", f.path("/snapshot"), token, nil, nil, 200)
	after := snap["cursor"].(string)
	c := f.context()
	open := func(after string) *http.Response {
		req, _ := http.NewRequest("GET", f.http.URL+f.path("/events/stream"), nil)
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Last-Event-ID", after)
		resp, e := (&http.Client{Timeout: 12 * time.Second}).Do(req)
		if e != nil {
			t.Fatal(e)
		}
		if resp.StatusCode != 200 {
			t.Fatal(resp.StatusCode)
		}
		return resp
	}
	resp := open(after)
	scanner := bufio.NewScanner(resp.Body)
	last := ""
	found := false
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "id: ") {
			last = strings.TrimPrefix(line, "id: ")
		}
		if strings.Contains(line, c["id"].(string)) {
			found = true
			break
		}
	}
	resp.Body.Close()
	if !found || last == "" {
		t.Fatal("snapshot/SSE gap lost")
	}
	c2 := f.context()
	resp = open(last)
	scanner = bufio.NewScanner(resp.Body)
	found = false
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "id: ") {
			last = strings.TrimPrefix(line, "id: ")
		}
		if strings.Contains(line, c2["id"].(string)) {
			found = true
			break
		}
	}
	resp.Body.Close()
	if !found {
		t.Fatal("replay lost")
	}
	resp = open(last)
	start := time.Now()
	f.ok("POST", f.path("/tokens/"+viewer["id"].(string)+"/revoke"), f.token, M{}, nil, 200)
	_, e := io.ReadAll(resp.Body)
	resp.Body.Close()
	if e != nil {
		t.Fatal(e)
	}
	if time.Since(start) > 7*time.Second {
		t.Fatal("SSE revocation delayed")
	}
}
func TestSlowSubscriberBounded(t *testing.T) {
	h := &feedHub{clients: map[chan feedMessage]bool{}}
	ch := make(chan feedMessage, 100)
	h.clients[ch] = true
	for i := 0; i < 101; i++ {
		h.broadcast(feedMessage{})
	}
	if len(ch) != 100 || len(h.clients) != 0 {
		t.Fatal("unbounded subscriber")
	}
	for range ch {
	}
}
func TestBootstrapAndRecoverySerialized(t *testing.T) {
	f := setup(t)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, e := f.s.Bootstrap(context.Background(), false, ""); e == nil {
				t.Error("bootstrap repeated")
			}
		}()
	}
	wg.Wait()
	if _, e := f.s.Bootstrap(context.Background(), true, ""); e == nil {
		t.Fatal("reason missing")
	}
	newAdmin, e := f.s.Bootstrap(context.Background(), true, "lost token")
	if e != nil {
		t.Fatal(e)
	}
	f.ok("GET", "/v1/me", f.admin, nil, nil, 401)
	f.ok("GET", "/v1/me", newAdmin, nil, nil, 200)
}

func TestTokenRotationRedactionAndProjectRecovery(t *testing.T) {
	f := setup(t)
	tok := f.issue("editor")
	newToken := f.ok("POST", f.path("/tokens/"+tok["id"].(string)+"/rotate"), f.token, M{}, nil, 201)
	f.ok("GET", "/v1/me", tok["token"].(string), nil, nil, 401)
	me := f.ok("GET", "/v1/me", newToken["token"].(string), nil, nil, 200)
	if me["principal_id"] != tok["principal_id"] {
		t.Fatal("rotate changed principal")
	}
	c := f.context()
	path := f.path("/contexts/" + c["id"].(string))
	f.ok("PATCH", path, f.token, M{"body": "another sensitive version"}, match(1), 200)
	f.ok("POST", path+"/redact", f.token, M{"reason": "test removal"}, match(2), 200)
	for _, v := range []string{"1", "2"} {
		got := f.ok("GET", path+"?version="+v, f.token, nil, nil, 200)
		if got["document"].(map[string]any)["body"] != "[redacted]" {
			t.Fatal(got)
		}
	}
	recovered, e := f.s.RecoverProjectAdmin(context.Background(), f.p, "recovered", "lost all project admin tokens")
	if e != nil {
		t.Fatal(e)
	}
	f.ok("GET", f.path("/snapshot"), f.token, nil, nil, 401)
	f.ok("GET", f.path("/snapshot"), recovered, nil, nil, 200)
}
func TestLiveSSEAndHeaderValidation(t *testing.T) {
	f := setup(t)
	after := f.ok("GET", f.path("/snapshot"), f.token, nil, nil, 200)["cursor"].(string)
	req, _ := http.NewRequest("GET", f.http.URL+f.path("/events/stream?after="+after), nil)
	req.Header.Set("Authorization", "Bearer "+f.token)
	req.Header.Set("Last-Event-ID", "different")
	resp, e := http.DefaultClient.Do(req)
	if e != nil {
		t.Fatal(e)
	}
	resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatal(resp.StatusCode)
	}
	req, _ = http.NewRequest("GET", f.http.URL+f.path("/events/stream?after="+after), nil)
	req.Header.Set("Authorization", "Bearer "+f.token)
	resp, e = (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if e != nil {
		t.Fatal(e)
	}
	defer resp.Body.Close()
	created := f.context()
	scanner := bufio.NewScanner(resp.Body)
	found := false
	for scanner.Scan() {
		if strings.Contains(scanner.Text(), created["id"].(string)) {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("live notification missing")
	}
}

func TestSnapshotTruncationAndPagination(t *testing.T) {
	f := setup(t)
	_, e := f.s.DB.Exec(context.Background(), "INSERT INTO tasks(project_id,id,title,acceptance,state) SELECT $1,'tsk'||lpad(n::text,4,'0'),'task','pass','todo' FROM generate_series(1,205) n", f.p)
	if e != nil {
		t.Fatal(e)
	}
	snap := f.ok("GET", f.path("/snapshot"), f.token, nil, nil, 200)
	if snap["truncated"] != true || len(snap["tasks"].([]any)) != 200 || snap["continuations"] == nil {
		t.Fatal("silent snapshot truncation")
	}
	total := 0
	next := ""
	for {
		v := f.ok("GET", f.path("/tasks?cursor="+next), f.token, nil, nil, 200)
		total += len(v["items"].([]any))
		next = v["next_cursor"].(string)
		if next == "" {
			break
		}
	}
	if total != 205 {
		t.Fatal(total)
	}
}
func TestWriteRechecksRevocationAfterProjectLock(t *testing.T) {
	f := setup(t)
	editor := f.issue("editor")
	tok := editor["token"].(string)
	ctx := context.Background()
	tx, e := f.s.DB.Begin(ctx)
	if e != nil {
		t.Fatal(e)
	}
	defer tx.Rollback(ctx)
	if _, e = tx.Exec(ctx, "SELECT revision FROM projects WHERE id=$1 FOR UPDATE", f.p); e != nil {
		t.Fatal(e)
	}
	done := make(chan int, 1)
	go func() {
		code, _ := f.request("POST", f.path("/contexts"), tok, M{"kind": "plan", "title": "must not write"}, nil)
		done <- code
	}()
	deadline := time.Now().Add(3 * time.Second)
	blocked := false
	for time.Now().Before(deadline) {
		var n int
		e = f.s.DB.QueryRow(ctx, "SELECT count(*) FROM pg_stat_activity WHERE wait_event_type='Lock' AND query LIKE 'SELECT revision FROM projects WHERE id=%'").Scan(&n)
		if e != nil {
			t.Fatal(e)
		}
		if n > 0 {
			blocked = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !blocked {
		t.Fatal("writer never waited on serialization lock")
	}
	if _, e = tx.Exec(ctx, "UPDATE tokens SET revoked_at=clock_timestamp() WHERE id=$1", editor["id"]); e != nil {
		t.Fatal(e)
	}
	if e = tx.Commit(ctx); e != nil {
		t.Fatal(e)
	}
	if code := <-done; code != 401 {
		t.Fatalf("queued revoked writer got %d", code)
	}
	var n int
	if e = f.s.DB.QueryRow(ctx, "SELECT count(*) FROM contexts WHERE project_id=$1", f.p).Scan(&n); e != nil {
		t.Fatal(e)
	}
	if n != 0 {
		t.Fatal("revoked write committed")
	}
}

func TestRedactionFencesCachedFullDocument(t *testing.T) {
	f := setup(t)
	c := f.context()
	path := f.path("/contexts/" + c["id"].(string))
	cached := M{"kind": "state", "title": "old", "body": "sensitive cached content"}
	f.ok("POST", path+"/redact", f.token, M{"reason": "remove sensitive content"}, match(1), 200)
	f.ok("PATCH", path, f.token, cached, match(1), 412)
	f.ok("PATCH", path, f.token, cached, match(2), 409)
	for _, v := range []string{"1", "2"} {
		got := f.ok("GET", path+"?version="+v, f.token, nil, nil, 200)
		if got["document"].(map[string]any)["body"] != "[redacted]" {
			t.Fatal(got)
		}
	}
}
func TestContextPackDisclosesOmittedAdoptedVersion(t *testing.T) {
	f := setup(t)
	c := f.ok("POST", f.path("/contexts"), f.token, M{"kind": "state", "title": "large adopted version", "body": strings.Repeat("a", 8000)}, nil, 201)
	i := c["id"].(string)
	f.ok("PUT", f.path("/state-pins/current"), f.token, M{"context_id": i, "version": 1}, map[string]string{"If-None-Match": "*"}, 200)
	f.ok("PATCH", f.path("/contexts/"+i), f.token, M{"body": "short current version"}, match(1), 200)
	pack := f.ok("GET", f.path("/context-pack?max_bytes=2048"), f.token, nil, nil, 200)
	if pack["truncated"] != true || pack["omitted_pin_count"] != float64(1) || pack["excluded_count"].(float64) < 1 {
		t.Fatal(pack)
	}
}
func TestStructuredEvidencePreservesLargeIntegers(t *testing.T) {
	f := setup(t)
	n := json.Number("9007199254740993")
	c := f.ok("POST", f.path("/contexts"), f.token, M{"kind": "observation", "title": "precise subject", "sources": []M{{"uri": "https://example.invalid/result", "subject": M{"sequence": n}}}}, nil, 201)
	path := f.path("/contexts/" + c["id"].(string))
	f.ok("PATCH", path, f.token, M{"title": "only title changed"}, match(1), 200)
	for _, version := range []int{1, 2} {
		var raw []byte
		if e := f.s.DB.QueryRow(context.Background(), "SELECT document FROM context_versions WHERE project_id=$1 AND context_id=$2 AND version=$3", f.p, c["id"], version).Scan(&raw); e != nil {
			t.Fatal(e)
		}
		if !strings.Contains(string(raw), string(n)) {
			t.Fatalf("integer rounded in version %d", version)
		}
	}
}

func TestEpochResyncReplacesStoppedFeedHub(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	first, cancel, e := f.s.subscribe(ctx, f.p)
	if e != nil {
		t.Fatal(e)
	}
	defer cancel()
	if _, e = f.s.RestoreEpoch(ctx, "feed reconnection drill"); e != nil {
		t.Fatal(e)
	}
	select {
	case message := <-first:
		if !message.Resync {
			t.Fatal("missing resync")
		}
	case <-time.After(4 * time.Second):
		t.Fatal("epoch change not observed")
	}
	second, cancelSecond, e := f.s.subscribe(ctx, f.p)
	if e != nil {
		t.Fatal(e)
	}
	defer cancelSecond()
	created := f.context()
	select {
	case message := <-second:
		if message.Resync || message.Event["resource_id"] != created["id"] {
			t.Fatal(message)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("reconnection joined a stopped poller")
	}
}
