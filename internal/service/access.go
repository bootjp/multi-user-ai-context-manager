package service

import (
	"context"
	"errors"
	"github.com/jackc/pgx/v5"
	"net/http"
	"strings"
	"time"
)

func (m *mutation) createProject() (result, error) {
	var in struct {
		Name string `json:"name"`
	}
	if e := decode(m.body, &in); e != nil {
		return result{}, e
	}
	if strings.TrimSpace(in.Name) == "" {
		return result{}, fail(400, "name_required")
	}
	m.p = id("prj")
	m.rev = 1
	actor := id("act")
	i, sec := id("tok"), secret()
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{"INSERT INTO projects(id,name) VALUES($1,$2)", []any{m.p, in.Name}},
		{"INSERT INTO principals VALUES($1,$2,$3)", []any{m.p, actor, "project administrator"}},
		{"INSERT INTO tokens(id,project_id,principal_id,secret_hash,role,expires_at) VALUES($1,$2,$3,$4,'project_admin',$5)", []any{i, m.p, actor, hash(sec), m.now.Add(30 * 24 * time.Hour)}},
	} {
		if _, e := m.tx.Exec(m.ctx(), q.sql, q.args...); e != nil {
			return result{}, e
		}
	}
	if e := m.audit("project.created", m.p, ""); e != nil {
		return result{}, e
	}
	out := outcome("project.created", m.p, 1, M{"id": m.p, "token_id": i, "principal_id": actor, "token": tokenValue(i, sec)})
	out.status = 201
	out.secret = true
	return out, nil
}
func (m *mutation) token(parts []string) (result, error) {
	if ranks[m.a.Role] < 4 {
		return result{}, fail(403, "forbidden")
	}
	var in struct {
		DisplayName string     `json:"display_name"`
		Role        string     `json:"role"`
		ExpiresAt   *time.Time `json:"expires_at"`
		Reason      string     `json:"reason"`
	}
	if e := decode(m.body, &in); e != nil {
		return result{}, e
	}
	if m.r.Method != "POST" {
		return result{}, fail(405, "method_not_allowed")
	}
	var i, actor, role string
	var expiry time.Time
	if len(parts) == 0 {
		if ranks[in.Role] < 1 || ranks[in.Role] > 4 || strings.TrimSpace(in.DisplayName) == "" {
			return result{}, fail(400, "invalid_token_parameters")
		}
		actor = id("act")
		role = in.Role
		expiry = m.now.Add(7 * 24 * time.Hour)
		if role == "project_admin" {
			expiry = m.now.Add(30 * 24 * time.Hour)
		}
		if in.ExpiresAt != nil {
			expiry = *in.ExpiresAt
		}
		if !expiry.After(m.now) {
			return result{}, fail(400, "invalid_expiry")
		}
		if _, e := m.tx.Exec(m.ctx(), "INSERT INTO principals VALUES($1,$2,$3)", m.p, actor, in.DisplayName); e != nil {
			return result{}, e
		}
	} else {
		if len(parts) != 2 || (parts[1] != "revoke" && parts[1] != "rotate") {
			return result{}, fail(404, "not_found")
		}
		i = parts[0]
		e := m.tx.QueryRow(m.ctx(), "SELECT principal_id,role,expires_at FROM tokens WHERE project_id=$1 AND id=$2", m.p, i).Scan(&actor, &role, &expiry)
		if errors.Is(e, pgx.ErrNoRows) {
			return result{}, fail(404, "not_found")
		}
		if e != nil {
			return result{}, e
		}
		if _, e = m.tx.Exec(m.ctx(), "UPDATE tokens SET revoked_at=$3 WHERE project_id=$1 AND id=$2", m.p, i, m.now); e != nil {
			return result{}, e
		}
		if e = m.audit("token."+parts[1], i, in.Reason); e != nil {
			return result{}, e
		}
		if parts[1] == "revoke" {
			return outcome("token.revoked", i, 1, nil), nil
		}
		expiry = m.now.Add(7 * 24 * time.Hour)
		if in.ExpiresAt != nil {
			expiry = *in.ExpiresAt
		}
		if !expiry.After(m.now) {
			return result{}, fail(400, "invalid_expiry")
		}
	}
	i, sec := id("tok"), secret()
	if _, e := m.tx.Exec(m.ctx(), "INSERT INTO tokens(id,project_id,principal_id,secret_hash,role,expires_at) VALUES($1,$2,$3,$4,$5,$6)", i, m.p, actor, hash(sec), role, expiry); e != nil {
		return result{}, e
	}
	if e := m.audit("token.issued", i, ""); e != nil {
		return result{}, e
	}
	out := outcome("token.issued", i, 1, M{"id": i, "principal_id": actor, "role": role, "expires_at": expiry, "token": tokenValue(i, sec)})
	out.secret = true
	out.status = http.StatusCreated
	return out, nil
}

// RecoverProjectAdmin is an explicit offline-capable DB-operator action.
func (s *Server) RecoverProjectAdmin(ctx context.Context, p, name, reason string) (string, error) {
	if strings.TrimSpace(reason) == "" || strings.TrimSpace(name) == "" {
		return "", errors.New("display name and reason required")
	}
	tx, e := s.DB.Begin(ctx)
	if e != nil {
		return "", e
	}
	defer tx.Rollback(ctx)
	if _, e = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(6791832)"); e != nil {
		return "", e
	}
	var ep string
	if e = tx.QueryRow(ctx, "SELECT epoch FROM instance_metadata WHERE singleton FOR SHARE").Scan(&ep); e != nil {
		return "", e
	}
	var rev int64
	if e = tx.QueryRow(ctx, "SELECT revision FROM projects WHERE id=$1 FOR UPDATE", p).Scan(&rev); e != nil {
		return "", e
	}
	rev++
	actor, i, sec := id("act"), id("tok"), secret()
	if _, e = tx.Exec(ctx, "UPDATE tokens SET revoked_at=clock_timestamp() WHERE project_id=$1 AND role='project_admin' AND revoked_at IS NULL", p); e != nil {
		return "", e
	}
	if _, e = tx.Exec(ctx, "INSERT INTO principals VALUES($1,$2,$3)", p, actor, name); e != nil {
		return "", e
	}
	if _, e = tx.Exec(ctx, "INSERT INTO tokens(id,project_id,principal_id,secret_hash,role,expires_at) VALUES($1,$2,$3,$4,'project_admin',clock_timestamp()+interval '30 days')", i, p, actor, hash(sec)); e != nil {
		return "", e
	}
	if _, e = tx.Exec(ctx, "UPDATE projects SET revision=$2 WHERE id=$1", p, rev); e != nil {
		return "", e
	}
	if _, e = tx.Exec(ctx, "INSERT INTO project_events(project_id,revision,type,resource_id,resource_version,actor_id) VALUES($1,$2,'admin.recovered',$3,1,'db-operator')", p, rev, i); e != nil {
		return "", e
	}
	if _, e = tx.Exec(ctx, "INSERT INTO audit_entries(project_id,actor_id,action,target,reason) VALUES($1,'db-operator','project.admin_recovered',$2,$3)", p, i, reason); e != nil {
		return "", e
	}
	if _, e = tx.Exec(ctx, "SELECT pg_notify('agent_context_changes',$1)", cursor(ep, p, rev)); e != nil {
		return "", e
	}
	if e = tx.Commit(ctx); e != nil {
		return "", e
	}
	return tokenValue(i, sec), nil
}
