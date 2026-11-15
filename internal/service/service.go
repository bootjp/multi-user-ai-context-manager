package service

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed schema.sql
var schema string

type Server struct {
	DB     *pgxpool.Pool
	hubMu  sync.Mutex
	hubs   map[string]*feedHub
	rateMu sync.Mutex
	rates  map[string]bucket
}
type M = map[string]any
type problem struct {
	Status int
	Code   string
	Detail any
}

func (e *problem) Error() string         { return e.Code }
func fail(status int, code string) error { return &problem{status, code, nil} }
func id(prefix string) string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return prefix + hex.EncodeToString(b)
}
func secret() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}
func hash(s string) []byte          { v := sha256.Sum256([]byte(s)); return v[:] }
func tokenValue(i, s string) string { return "sctx_" + i + "_" + s }
func jsonBytes(v any) []byte {
	b, e := json.Marshal(v)
	if e != nil {
		panic(e)
	}
	return b
}
func (s *Server) Migrate(ctx context.Context) error {
	tx, e := s.DB.Begin(ctx)
	if e != nil {
		return e
	}
	defer tx.Rollback(ctx)
	if _, e = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(6791832)"); e != nil {
		return e
	}
	var initialized bool
	if e = tx.QueryRow(ctx, "SELECT to_regclass('instance_metadata') IS NOT NULL").Scan(&initialized); e != nil {
		return e
	}
	if initialized {
		var version int
		if e = tx.QueryRow(ctx, "SELECT schema_version FROM instance_metadata WHERE singleton").Scan(&version); e != nil {
			return e
		}
		if version != 1 {
			return errors.New("schema incompatible")
		}
	}
	if _, e = tx.Exec(ctx, schema); e != nil {
		return e
	}
	_, e = tx.Exec(ctx, "INSERT INTO instance_metadata(singleton,schema_version,epoch) VALUES(true,1,$1) ON CONFLICT DO NOTHING", id("ep"))
	if e != nil {
		return e
	}
	return tx.Commit(ctx)
}
func (s *Server) Ready(ctx context.Context) error {
	var v int
	e := s.DB.QueryRow(ctx, "SELECT schema_version FROM instance_metadata WHERE singleton").Scan(&v)
	if e == nil && v != 1 {
		return errors.New("schema incompatible")
	}
	return e
}
func (s *Server) Bootstrap(ctx context.Context, recover bool, reason string) (string, error) {
	tx, e := s.DB.Begin(ctx)
	if e != nil {
		return "", e
	}
	defer tx.Rollback(ctx)
	if _, e = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(6791832)"); e != nil {
		return "", e
	}
	var n int
	if e = tx.QueryRow(ctx, "SELECT count(*) FROM instance_tokens").Scan(&n); e != nil {
		return "", e
	}
	if n > 0 && !recover {
		return "", errors.New("already initialized; explicit admin recover required")
	}
	if recover && strings.TrimSpace(reason) == "" {
		return "", errors.New("recovery reason required")
	}
	if recover {
		if _, e = tx.Exec(ctx, "UPDATE instance_tokens SET revoked_at=clock_timestamp() WHERE revoked_at IS NULL"); e != nil {
			return "", e
		}
	}
	i, sec := id("adm"), secret()
	if _, e = tx.Exec(ctx, "INSERT INTO instance_tokens VALUES($1,$2,clock_timestamp()+interval '30 days',NULL)", i, hash(sec)); e != nil {
		return "", e
	}
	if _, e = tx.Exec(ctx, "INSERT INTO audit_entries(actor_id,action,target,reason) VALUES('db-operator',$1,$2,$3)", map[bool]string{true: "admin.recover", false: "admin.bootstrap"}[recover], i, reason); e != nil {
		return "", e
	}
	if e = tx.Commit(ctx); e != nil {
		return "", e
	}
	return tokenValue(i, sec), nil
}

// RestoreEpoch MUST run before reopening traffic to a restored database.
func (s *Server) RestoreEpoch(ctx context.Context, reason string) (string, error) {
	if strings.TrimSpace(reason) == "" {
		return "", errors.New("reason required")
	}
	tx, e := s.DB.Begin(ctx)
	if e != nil {
		return "", e
	}
	defer tx.Rollback(ctx)
	if _, e = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(6791832)"); e != nil {
		return "", e
	}
	ep := id("ep")
	if _, e = tx.Exec(ctx, "UPDATE instance_metadata SET epoch=$1 WHERE singleton", ep); e != nil {
		return "", e
	}
	if _, e = tx.Exec(ctx, "UPDATE tasks SET lease_expires_at=clock_timestamp()-interval '1 second',lease_hash=NULL,lease_generation=lease_generation+1,version=version+1 WHERE owner_id IS NOT NULL; UPDATE resource_leases SET lease_expires_at=clock_timestamp()-interval '1 second',lease_hash=NULL,lease_generation=lease_generation+1,version=version+1 WHERE owner_id IS NOT NULL; DELETE FROM idempotency_keys"); e != nil {
		return "", e
	}
	if _, e = tx.Exec(ctx, "INSERT INTO audit_entries(actor_id,action,target,reason) VALUES('db-operator','restore.epoch',$1,$2)", ep, reason); e != nil {
		return "", e
	}
	if e = tx.Commit(ctx); e != nil {
		return "", e
	}
	return ep, nil
}

type access struct {
	ID, Project, Actor, Role string
	Expires                  time.Time
}

var ranks = map[string]int{"viewer": 1, "editor": 2, "coordinator": 3, "project_admin": 4, "instance_admin": 5}

type querier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func authenticate(ctx context.Context, q querier, bearer string) (access, error) {
	a := access{}
	parts := strings.Split(strings.TrimPrefix(bearer, "Bearer "), "_")
	if !strings.HasPrefix(bearer, "Bearer ") || len(parts) != 3 || parts[0] != "sctx" {
		return a, fail(401, "unauthorized")
	}
	var h []byte
	var active bool
	a.ID = parts[1]
	e := q.QueryRow(ctx, "SELECT project_id,principal_id,role,secret_hash,expires_at,revoked_at IS NULL AND expires_at>clock_timestamp() FROM tokens WHERE id=$1", a.ID).Scan(&a.Project, &a.Actor, &a.Role, &h, &a.Expires, &active)
	if errors.Is(e, pgx.ErrNoRows) {
		a.Actor = a.ID
		a.Role = "instance_admin"
		e = q.QueryRow(ctx, "SELECT secret_hash,expires_at,revoked_at IS NULL AND expires_at>clock_timestamp() FROM instance_tokens WHERE id=$1", a.ID).Scan(&h, &a.Expires, &active)
	}
	if e != nil && !errors.Is(e, pgx.ErrNoRows) {
		return a, e
	}
	if e != nil || !active || subtle.ConstantTimeCompare(h, hash(parts[2])) != 1 {
		return a, fail(401, "unauthorized")
	}
	return a, nil
}
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
func writeError(w http.ResponseWriter, e error) {
	var p *problem
	if errors.As(e, &p) {
		v := M{"error": p.Code}
		if p.Detail != nil {
			v["detail"] = p.Detail
		}
		writeJSON(w, p.Status, v)
		return
	}
	writeJSON(w, 500, M{"error": "internal_error"})
}
func checkVersion(r *http.Request, v int64) error {
	h := r.Header.Get("If-Match")
	if h == "" {
		return fail(428, "precondition_required")
	}
	if h != fmt.Sprintf("\"%d\"", v) {
		return &problem{412, "version_conflict", M{"current_version": v}}
	}
	return nil
}
func cursor(ep, p string, rev int64) string { return fmt.Sprintf("%s:%s:%d", ep, p, rev) }
func parseCursor(c, ep, p string, rev, floor int64) (int64, error) {
	a := strings.Split(c, ":")
	if len(a) != 3 {
		return 0, fail(400, "invalid_cursor")
	}
	if a[1] != p {
		return 0, fail(400, "wrong_project_cursor")
	}
	if a[0] != ep {
		return 0, &problem{409, "epoch_changed", M{"snapshot": "/v1/projects/" + p + "/snapshot"}}
	}
	n, e := strconv.ParseInt(a[2], 10, 64)
	if e != nil || n < 0 || n > rev {
		return 0, fail(400, "invalid_cursor")
	}
	if n < floor {
		return 0, &problem{410, "cursor_expired", M{"snapshot": "/v1/projects/" + p + "/snapshot"}}
	}
	return n, nil
}
