package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestWatchReconnectPersistsCursorAndResnapshots(t *testing.T) {
	var streams atomic.Int32
	var snapshots atomic.Int32
	var bad atomic.Bool
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer private" || r.URL.Query().Get("token") != "" {
			bad.Store(true)
		}
		if r.URL.Path == "/v1/projects/p/snapshot" {
			n := snapshots.Add(1)
			w.Header().Set("Content-Type", "application/json")
			if n == 1 {
				fmt.Fprintln(w, `{"cursor":"ep:p:1"}`)
			} else {
				fmt.Fprintln(w, `{"cursor":"ep2:p:1"}`)
			}
			return
		}
		n := streams.Add(1)
		if n == 1 {
			if r.Header.Get("Last-Event-ID") != "ep:p:1" {
				bad.Store(true)
			}
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "id: ep:p:2\nevent: context.created\ndata: {\"revision\":2}\n\n")
			return
		}
		if n == 2 {
			if r.Header.Get("Last-Event-ID") != "ep:p:2" {
				bad.Store(true)
			}
			w.WriteHeader(409)
			return
		}
		if r.Header.Get("Last-Event-ID") != "ep2:p:1" {
			bad.Store(true)
		}
		cancel()
	}))
	defer srv.Close()
	t.Setenv("MULTI_USER_AI_CONTEXT_MANAGER_URL", srv.URL)
	t.Setenv("MULTI_USER_AI_CONTEXT_MANAGER_TOKEN", "private")
	file := filepath.Join(t.TempDir(), "watch.cursor")
	if e := watch(ctx, []string{"--project", "p", "--cursor-file", file}); e != nil {
		t.Fatal(e)
	}
	b, e := os.ReadFile(file)
	if e != nil {
		t.Fatal(e)
	}
	if string(b) != "ep2:p:1\n" || bad.Load() || snapshots.Load() != 2 || streams.Load() < 3 {
		t.Fatalf("cursor=%s snapshots=%d streams=%d bad=%t", b, snapshots.Load(), streams.Load(), bad.Load())
	}
}

func TestInvalidEndpointReturnsError(t *testing.T) {
	t.Setenv("MULTI_USER_AI_CONTEXT_MANAGER_URL", "http://[broken")
	t.Setenv("MULTI_USER_AI_CONTEXT_MANAGER_TOKEN", "private")
	if e := watch(context.Background(), []string{"--project", "p"}); e == nil {
		t.Fatal("invalid endpoint accepted")
	}
}
func TestSecretOutputReservedBeforeIssuance(t *testing.T) {
	dir := t.TempDir()
	existing := filepath.Join(dir, "admin.token")
	if e := os.WriteFile(existing, []byte("old"), 0600); e != nil {
		t.Fatal(e)
	}
	called := false
	issue := func() (string, error) { called = true; return "new", nil }
	if e := emitAdminToken(existing, issue); e == nil || called {
		t.Fatal("credentials changed before destination reserved")
	}
	if e := emitAdminToken(filepath.Join(dir, "missing", "secret"), issue); e == nil || called {
		t.Fatal("invalid parent changed credentials")
	}
	b, _ := os.ReadFile(existing)
	if string(b) != "old" {
		t.Fatal("overwrote secret")
	}
}
func TestWatchRetriesTransientFailureAndSilentConnection(t *testing.T) {
	var calls atomic.Int32
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		if n == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(503)
			return
		}
		if n == 2 {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(200)
			w.(http.Flusher).Flush()
			<-r.Context().Done()
			return
		}
		if r.Header.Get("Last-Event-ID") != "ep:p:1" {
			t.Error("retry lost cursor")
		}
		cancel()
	}))
	defer srv.Close()
	t.Setenv("MULTI_USER_AI_CONTEXT_MANAGER_URL", srv.URL)
	t.Setenv("MULTI_USER_AI_CONTEXT_MANAGER_TOKEN", "private")
	if e := watchWithIdleTimeout(ctx, []string{"--project", "p", "--after", "ep:p:1"}, 50*time.Millisecond); e != nil {
		t.Fatal(e)
	}
	if calls.Load() < 3 {
		t.Fatal("did not recover from HTTP failure and stalled stream")
	}
}

func TestConnectionEnvironmentCompatibility(t *testing.T) {
	t.Setenv("AGENT_CONTEXT_URL", "http://legacy.invalid:8080")
	t.Setenv("AGENT_CONTEXT_TOKEN", "legacy-token")
	t.Setenv("AGENT_CONTEXT_PROJECT", "legacy-project")
	for _, suffix := range []string{"URL", "TOKEN", "PROJECT"} {
		primary := "MULTI_USER_AI_CONTEXT_MANAGER_" + suffix
		prior, exists := os.LookupEnv(primary)
		os.Unsetenv(primary)
		t.Cleanup(func() {
			if exists {
				os.Setenv(primary, prior)
			} else {
				os.Unsetenv(primary)
			}
		})
	}
	base, token, e := connection()
	if e != nil || base != "http://legacy.invalid:8080" || token != "legacy-token" || connectionEnv("PROJECT") != "legacy-project" {
		t.Fatal("existing session environment no longer works")
	}
	t.Setenv("MULTI_USER_AI_CONTEXT_MANAGER_URL", "http://new.invalid:8080")
	t.Setenv("MULTI_USER_AI_CONTEXT_MANAGER_TOKEN", "new-token")
	t.Setenv("MULTI_USER_AI_CONTEXT_MANAGER_PROJECT", "new-project")
	base, token, e = connection()
	if e != nil || base != "http://new.invalid:8080" || token != "new-token" || connectionEnv("PROJECT") != "new-project" {
		t.Fatal("new environment must take precedence")
	}
}
