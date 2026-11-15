package service

import (
	"context"
	"github.com/jackc/pgx/v5/pgxpool"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestLogicalBackupRestoredIntoFreshDatabase(t *testing.T) {
	f := setup(t)
	bin := os.Getenv("TEST_PG_BIN")
	if bin == "" {
		t.Skip("set TEST_PG_BIN to run pg_dump/pg_restore drill")
	}
	ctx := context.Background()
	c := f.context()
	path := f.path("/resources/benchmark/restored")
	lease := f.ok("POST", path+"/claim", f.token, M{}, nil, 200)
	f.ok("POST", path+"/checkpoint", f.token, M{"lease_generation": lease["lease_generation"], "lease_secret": lease["lease_secret"], "external_job": M{"id": "surviving-job", "state": "running"}}, nil, 200)
	before := f.ok("GET", f.path("/snapshot"), f.token, nil, nil, 200)["cursor"].(string)
	cfg := f.s.DB.Config()
	conn := cfg.ConnConfig
	schema := conn.RuntimeParams["search_path"]
	env := append(os.Environ(), "PGHOST="+conn.Host, "PGPORT="+toString(conn.Port), "PGUSER="+conn.User, "PGPASSWORD="+conn.Password, "PGDATABASE="+conn.Database, "PGSSLMODE=disable")
	dump := filepath.Join(t.TempDir(), "backup.dump")
	command := exec.Command(filepath.Join(bin, "pg_dump"), "--format=custom", "--no-owner", "--no-acl", "--schema="+schema, "--file="+dump)
	command.Env = env
	if out, e := command.CombinedOutput(); e != nil {
		t.Fatalf("pg_dump: %v %s", e, out)
	}
	// Diverge the original after the backup; a restored cursor must never skip updates.
	f.ok("PATCH", f.path("/contexts/"+c["id"].(string)), f.token, M{"body": "after backup"}, match(1), 200)
	name := id("restore")
	if _, e := f.s.DB.Exec(ctx, "CREATE DATABASE "+name); e != nil {
		t.Fatal(e)
	}
	defer f.s.DB.Exec(ctx, "DROP DATABASE "+name)
	command = exec.Command(filepath.Join(bin, "pg_restore"), "--exit-on-error", "--no-owner", "--no-acl")
	command.Args = append(command.Args, "--dbname="+name, dump)
	command.Env = env
	if out, e := command.CombinedOutput(); e != nil {
		t.Fatalf("pg_restore: %v %s", e, out)
	}
	cfg.ConnConfig.Database = name
	db, e := pgxpool.NewWithConfig(ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	restored := &Server{DB: db}
	if _, e = restored.RestoreEpoch(ctx, "logical backup acceptance drill"); e != nil {
		t.Fatal(e)
	}
	http := httptest.NewServer(restored)
	defer http.Close()
	old := f.http
	f.http = http
	defer func() { f.http = old }()
	f.ok("GET", f.path("/events?after="+before), f.token, nil, nil, 409)
	got := f.ok("GET", f.path("/contexts/"+c["id"].(string)), f.token, nil, nil, 200)
	if got["version"] != float64(1) {
		t.Fatal("restored history differs")
	}
	f.ok("POST", path+"/claim", f.token, M{}, nil, 409)
	f.ok("POST", path+"/renew", f.token, M{"lease_generation": lease["lease_generation"], "lease_secret": lease["lease_secret"]}, nil, 409)
	resources := f.ok("GET", f.path("/resources"), f.token, nil, nil, 200)["items"].([]any)
	if resources[0].(map[string]any)["external_job"].(map[string]any)["id"] != "surviving-job" {
		t.Fatal(resources)
	}
}
