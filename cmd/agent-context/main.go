package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"agent-context/internal/service"
	"github.com/jackc/pgx/v5/pgxpool"
)

func main() {
	if e := run(); e != nil {
		fmt.Fprintln(os.Stderr, "agent-context:", e)
		os.Exit(1)
	}
}
func run() error {
	if len(os.Args) < 2 {
		return errors.New("usage: agent-context serve|migrate|bootstrap|admin recover|restore-epoch|prune|backup-manifest|project create|token issue|api|watch")
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	args := os.Args[2:]
	cmd := os.Args[1]
	if cmd == "health" {
		addr := os.Getenv("LISTEN_ADDR")
		if addr == "" {
			addr = "127.0.0.1:8080"
		}
		addr = strings.Replace(addr, "0.0.0.0:", "127.0.0.1:", 1)
		resp, e := (&http.Client{Timeout: 3 * time.Second}).Get("http://" + addr + "/readyz")
		if e != nil {
			return e
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			return errors.New("not ready")
		}
		return nil
	}
	if cmd == "watch" {
		return watch(ctx, args)
	}
	if cmd == "api" || cmd == "project" || cmd == "token" {
		return api(ctx, cmd, args)
	}
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	reason := fs.String("reason", "", "audit reason")
	output := fs.String("output-file", "", "explicit private output file for one-time admin token")
	projectID := fs.String("project", "", "project ID for admin project-recover")
	displayName := fs.String("name", "recovered administrator", "recovered principal display name")
	recoverProject := false
	recover := false
	if cmd == "admin" {
		if len(args) == 0 || (args[0] != "recover" && args[0] != "project-recover") {
			return errors.New("usage: admin recover --reason ...")
		}
		recover = true
		recoverProject = args[0] == "project-recover"
		args = args[1:]
	}
	if e := fs.Parse(args); e != nil {
		return e
	}
	cfg, e := pgxpool.ParseConfig(os.Getenv("DATABASE_URL"))
	if e != nil {
		return e
	}
	cfg.MaxConns = 12
	cfg.ConnConfig.ConnectTimeout = 5 * time.Second
	db, e := pgxpool.NewWithConfig(ctx, cfg)
	if e != nil {
		return e
	}
	defer db.Close()
	s := &service.Server{DB: db}
	switch cmd {
	case "migrate":
		return s.Migrate(ctx)
	case "bootstrap", "admin":
		return emitAdminToken(*output, func() (string, error) {
			if recoverProject {
				return s.RecoverProjectAdmin(ctx, *projectID, *displayName, *reason)
			}
			return s.Bootstrap(ctx, recover, *reason)
		})
	case "restore-epoch":
		ep, e := s.RestoreEpoch(ctx, *reason)
		if e == nil {
			fmt.Println(ep)
		}
		return e
	case "prune":
		return s.Prune(ctx)
	case "backup-manifest":
		var ep string
		var schema int
		if e = db.QueryRow(ctx, "SELECT epoch,schema_version FROM instance_metadata").Scan(&ep, &schema); e != nil {
			return e
		}
		return json.NewEncoder(os.Stdout).Encode(map[string]any{"epoch": ep, "schema_version": schema, "created_at": time.Now().UTC(), "restore_requires": "offline restore-epoch and external-job recovery"})
	case "serve":
		if e = s.Ready(ctx); e != nil {
			return errors.New("schema unavailable: run migrate with a compatible binary before serving")
		}
		addr := os.Getenv("LISTEN_ADDR")
		if addr == "" {
			if port := os.Getenv("PORT"); port != "" {
				addr = "0.0.0.0:" + port
			} else {
				addr = "127.0.0.1:8080"
			}
		}
		server := &http.Server{Addr: addr, Handler: s, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 * 1024}
		go func() {
			<-ctx.Done()
			stop, c := context.WithTimeout(context.Background(), 10*time.Second)
			defer c()
			server.Shutdown(stop)
		}()
		go func() {
			ticker := time.NewTicker(time.Hour)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					s.Prune(ctx)
				}
			}
		}()
		fmt.Fprintln(os.Stderr, "listening on", addr)
		e = server.ListenAndServe()
		if errors.Is(e, http.ErrServerClosed) {
			return nil
		}
		return e
	default:
		return errors.New("unknown command")
	}
}
func connection() (string, string, error) {
	base := strings.TrimRight(os.Getenv("AGENT_CONTEXT_URL"), "/")
	if base == "" {
		base = "http://127.0.0.1:8080"
	}
	parsed, err := url.Parse(base)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", "", errors.New("AGENT_CONTEXT_URL must be an absolute http(s) URL without credentials, query or fragment")
	}
	token := os.Getenv("AGENT_CONTEXT_TOKEN")
	if token == "" {
		return "", "", errors.New("AGENT_CONTEXT_TOKEN required")
	}
	return base, token, nil
}
func api(ctx context.Context, cmd string, args []string) error {
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	method := fs.String("method", "GET", "HTTP method")
	data := fs.String("data-file", "", "JSON request file (or - for stdin)")
	key := fs.String("key", "", "stable idempotency key")
	match := fs.String("if-match", "", "quoted entity version")
	none := fs.Bool("if-none-match", false, "create pin only")
	path := fs.String("path", "/v1/me", "API path")
	name := fs.String("name", "", "project or principal display name")
	role := fs.String("role", "editor", "project role")
	project := fs.String("project", os.Getenv("AGENT_CONTEXT_PROJECT"), "project ID")
	if cmd == "project" || cmd == "token" {
		if len(args) < 1 || cmd == "project" && args[0] != "create" || cmd == "token" && args[0] != "issue" {
			return errors.New("expected project create or token issue")
		}
		args = args[1:]
	}
	if e := fs.Parse(args); e != nil {
		return e
	}
	base, token, e := connection()
	if e != nil {
		return e
	}
	var body []byte
	if *data != "" {
		if *data == "-" {
			body, e = io.ReadAll(io.LimitReader(os.Stdin, 256*1024+1))
		} else {
			body, e = os.ReadFile(*data)
		}
		if e != nil {
			return e
		}
	}
	if cmd == "project" {
		*path = "/v1/projects"
		*method = "POST"
		body, _ = json.Marshal(map[string]string{"name": *name})
	}
	if cmd == "token" {
		if *project == "" {
			return errors.New("--project required")
		}
		*path = "/v1/projects/" + *project + "/tokens"
		*method = "POST"
		body, _ = json.Marshal(map[string]string{"display_name": *name, "role": *role})
	}
	if !strings.HasPrefix(*path, "/v1/") {
		return errors.New("path must begin /v1/")
	}
	if *method != "GET" && *key == "" {
		return errors.New("--key required for mutations; reuse the same key for retries")
	}
	req, e := http.NewRequestWithContext(ctx, *method, base+*path, strings.NewReader(string(body)))
	if e != nil {
		return e
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", *key)
	req.Header.Set("If-Match", *match)
	if *none {
		req.Header.Set("If-None-Match", "*")
	}
	c := &http.Client{Timeout: 30 * time.Second}
	resp, e := c.Do(req)
	if e != nil {
		return e
	}
	defer resp.Body.Close()
	if _, e = io.Copy(os.Stdout, resp.Body); e != nil {
		return e
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return nil
}
func persist(path, cursor string) error {
	if path == "" {
		return nil
	}
	dir := filepath.Dir(path)
	f, e := os.CreateTemp(dir, ".cursor-*")
	if e != nil {
		return e
	}
	temp := f.Name()
	defer os.Remove(temp)
	if e = f.Chmod(0600); e == nil {
		_, e = f.WriteString(cursor + "\n")
	}
	if e == nil {
		e = f.Sync()
	}
	closeErr := f.Close()
	if e != nil {
		return e
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(temp, path)
}
func watch(ctx context.Context, args []string) error {
	return watchWithIdleTimeout(ctx, args, 45*time.Second)
}
func watchWithIdleTimeout(ctx context.Context, args []string, idle time.Duration) error {
	fs := flag.NewFlagSet("watch", flag.ContinueOnError)
	p := fs.String("project", os.Getenv("AGENT_CONTEXT_PROJECT"), "project ID")
	file := fs.String("cursor-file", "", "private durable cursor file; advance only after stdout write succeeds")
	after := fs.String("after", "", "snapshot cursor")
	if e := fs.Parse(args); e != nil {
		return e
	}
	if *p == "" {
		return errors.New("--project required")
	}
	base, token, e := connection()
	if e != nil {
		return e
	}
	if *file != "" {
		b, e := os.ReadFile(*file)
		if e == nil {
			*after = strings.TrimSpace(string(b))
		} else if !errors.Is(e, os.ErrNotExist) {
			return e
		}
	}
	client := &http.Client{Transport: &http.Transport{ResponseHeaderTimeout: 15 * time.Second}}
	prefix := base + "/v1/projects/" + url.PathEscape(*p)
	snapshot := func() error {
		req, err := http.NewRequestWithContext(ctx, "GET", prefix+"/snapshot", nil)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		r, e := (&http.Client{Timeout: 30 * time.Second}).Do(req)
		if e != nil {
			return e
		}
		defer r.Body.Close()
		if r.StatusCode != 200 {
			return fmt.Errorf("snapshot HTTP %d", r.StatusCode)
		}
		b, e := io.ReadAll(io.LimitReader(r.Body, 4*1024*1024))
		if e != nil {
			return e
		}
		var v struct {
			Cursor string `json:"cursor"`
		}
		if e = json.Unmarshal(b, &v); e != nil {
			return e
		}
		if _, e = fmt.Fprintln(os.Stdout, string(b)); e != nil {
			return e
		}
		if e = persist(*file, v.Cursor); e != nil {
			return e
		}
		*after = v.Cursor
		return nil
	}
	if *after == "" {
		if e = snapshot(); e != nil {
			return e
		}
	}
	for ctx.Err() == nil {
		streamCtx, stopStream := context.WithCancel(ctx)
		timer := time.AfterFunc(idle, stopStream)
		req, err := http.NewRequestWithContext(streamCtx, "GET", prefix+"/events/stream", nil)
		if err != nil {
			timer.Stop()
			stopStream()
			return err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Last-Event-ID", *after)
		resp, e := client.Do(req)
		if e != nil {
			timer.Stop()
			stopStream()
		}
		if e == nil {
			if resp.StatusCode == 401 || resp.StatusCode == 403 || resp.StatusCode == 404 {
				resp.Body.Close()
				timer.Stop()
				stopStream()
				return fmt.Errorf("stream HTTP %d", resp.StatusCode)
			}
			if resp.StatusCode == 409 || resp.StatusCode == 410 {
				resp.Body.Close()
				timer.Stop()
				stopStream()
				if e = snapshot(); e != nil {
					return e
				}
				continue
			}
			if resp.StatusCode == 200 {
				scanner := bufio.NewScanner(resp.Body)
				scanner.Buffer(make([]byte, 4096), 65536)
				eventID, eventType, data := "", "", ""
				resync := false
				for scanner.Scan() {
					timer.Reset(idle)
					line := scanner.Text()
					if strings.HasPrefix(line, "id: ") {
						eventID = strings.TrimPrefix(line, "id: ")
					}
					if strings.HasPrefix(line, "event: ") {
						eventType = strings.TrimPrefix(line, "event: ")
					}
					if strings.HasPrefix(line, "data: ") {
						data = strings.TrimPrefix(line, "data: ")
					}
					if line == "" {
						if eventType == "resync_required" {
							resync = true
							break
						}
						if eventID != "" {
							if _, e = fmt.Fprintln(os.Stdout, data); e != nil {
								resp.Body.Close()
								timer.Stop()
								stopStream()
								return e
							}
							if e = persist(*file, eventID); e != nil {
								resp.Body.Close()
								timer.Stop()
								stopStream()
								return e
							}
							*after = eventID
						}
						eventID, eventType, data = "", "", ""
					}
				}
				resp.Body.Close()
				timer.Stop()
				stopStream()
				if resync {
					if e = snapshot(); e != nil {
						return e
					}
					continue
				}
			} else {
				code := resp.StatusCode
				delay := retryDelay(resp.Header.Get("Retry-After"), time.Now())
				resp.Body.Close()
				timer.Stop()
				stopStream()
				if code != 429 && code != 408 && code < 500 {
					return fmt.Errorf("stream HTTP %d", code)
				}
				select {
				case <-ctx.Done():
					return nil
				case <-time.After(delay):
				}
				continue
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(time.Second):
		}
	}
	return nil
}

// Reserve the secret destination before changing any administrator credentials.
func emitAdminToken(path string, issue func() (string, error)) error {
	if path == "" {
		info, e := os.Stdout.Stat()
		if e != nil || info.Mode()&os.ModeCharDevice == 0 {
			return errors.New("use a terminal or explicit --output-file for the one-time token")
		}
		token, e := issue()
		if e != nil {
			return e
		}
		_, e = fmt.Fprintln(os.Stdout, token)
		return e
	}
	f, e := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if e != nil {
		return e
	}
	success := false
	defer func() {
		f.Close()
		if !success {
			os.Remove(path)
		}
	}()
	token, e := issue()
	if e != nil {
		return e
	}
	if _, e = f.WriteString(token + "\n"); e != nil {
		return e
	}
	if e = f.Sync(); e != nil {
		return e
	}
	if e = f.Close(); e != nil {
		return e
	}
	success = true
	return nil
}
func retryDelay(value string, now time.Time) time.Duration {
	if seconds, e := strconv.Atoi(value); e == nil && seconds >= 0 {
		return min(time.Duration(seconds)*time.Second, time.Minute)
	}
	if at, e := http.ParseTime(value); e == nil {
		return min(max(time.Second, at.Sub(now)), time.Minute)
	}
	return time.Second
}
