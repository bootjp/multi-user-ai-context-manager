package service

import (
	"crypto/subtle"
	"errors"
	"github.com/jackc/pgx/v5"
	"strings"
	"time"
)

type taskInput struct {
	Title      string   `json:"title"`
	Acceptance string   `json:"acceptance"`
	State      string   `json:"state"`
	DependsOn  []string `json:"depends_on"`
}

func (m *mutation) task(parts []string) (result, error) {
	if len(parts) > 2 {
		return result{}, fail(404, "not_found")
	}
	if len(parts) > 1 {
		return m.lease("tasks", parts[0], parts[1])
	}
	var in taskInput
	i := id("tsk")
	v := int64(1)
	if len(parts) == 0 {
		if m.r.Method != "POST" {
			return result{}, fail(405, "method_not_allowed")
		}
		if e := decode(m.body, &in); e != nil {
			return result{}, e
		}
		if in.State == "" {
			in.State = "todo"
		}
		if in.State != "todo" {
			return result{}, fail(400, "invalid_initial_state")
		}
	} else {
		if m.r.Method != "PATCH" {
			return result{}, fail(405, "method_not_allowed")
		}
		i = parts[0]
		var old taskInput
		var active bool
		e := m.tx.QueryRow(m.ctx(), "SELECT title,acceptance,state,version,owner_id IS NOT NULL FROM tasks WHERE project_id=$1 AND id=$2", m.p, i).Scan(&old.Title, &old.Acceptance, &old.State, &v, &active)
		if errors.Is(e, pgx.ErrNoRows) {
			return result{}, fail(404, "not_found")
		}
		if e != nil {
			return result{}, e
		}
		if e = checkVersion(m.r, v); e != nil {
			return result{}, e
		}
		rows, e := m.tx.Query(m.ctx(), "SELECT depends_on_id FROM task_dependencies WHERE project_id=$1 AND task_id=$2", m.p, i)
		if e != nil {
			return result{}, e
		}
		for rows.Next() {
			var t string
			rows.Scan(&t)
			old.DependsOn = append(old.DependsOn, t)
		}
		e = rows.Err()
		rows.Close()
		if e != nil {
			return result{}, e
		}
		in = old
		if e = decode(m.body, &in); e != nil {
			return result{}, e
		}
		if in.State != old.State {
			if in.State != "todo" && in.State != "cancelled" {
				return result{}, fail(409, "use_lease_operation")
			}
			if active {
				return result{}, fail(409, "active_lease")
			}
		}
		v++
	}
	if strings.TrimSpace(in.Title) == "" || strings.TrimSpace(in.Acceptance) == "" {
		return result{}, fail(400, "title_and_acceptance_required")
	}
	if len(in.DependsOn) > 200 {
		return result{}, fail(400, "too_many_dependencies")
	}
	if len(parts) == 0 {
		if _, e := m.tx.Exec(m.ctx(), "INSERT INTO tasks(project_id,id,title,acceptance,state,version) VALUES($1,$2,$3,$4,$5,$6)", m.p, i, in.Title, in.Acceptance, in.State, v); e != nil {
			return result{}, e
		}
	} else {
		if _, e := m.tx.Exec(m.ctx(), "UPDATE tasks SET title=$3,acceptance=$4,state=$5,version=$6,owner_id=CASE WHEN state<>$5 THEN NULL ELSE owner_id END,lease_hash=CASE WHEN state<>$5 THEN NULL ELSE lease_hash END,lease_expires_at=CASE WHEN state<>$5 THEN NULL ELSE lease_expires_at END WHERE project_id=$1 AND id=$2", m.p, i, in.Title, in.Acceptance, in.State, v); e != nil {
			return result{}, e
		}
	}
	if _, e := m.tx.Exec(m.ctx(), "DELETE FROM task_dependencies WHERE project_id=$1 AND task_id=$2", m.p, i); e != nil {
		return result{}, e
	}
	for _, dep := range in.DependsOn {
		var ok bool
		if e := m.tx.QueryRow(m.ctx(), "SELECT EXISTS(SELECT 1 FROM tasks WHERE project_id=$1 AND id=$2)", m.p, dep).Scan(&ok); e != nil {
			return result{}, e
		}
		if !ok {
			return result{}, fail(404, "dependency_not_found")
		}
		if dep == i {
			return result{}, fail(409, "dependency_cycle")
		}
		if _, e := m.tx.Exec(m.ctx(), "INSERT INTO task_dependencies VALUES($1,$2,$3) ON CONFLICT DO NOTHING", m.p, i, dep); e != nil {
			return result{}, e
		}
	}
	var cycle bool
	e := m.tx.QueryRow(m.ctx(), `WITH RECURSIVE reachable(id) AS (SELECT depends_on_id FROM task_dependencies WHERE project_id=$1 AND task_id=$2 UNION SELECT d.depends_on_id FROM task_dependencies d JOIN reachable r ON d.task_id=r.id WHERE d.project_id=$1) SELECT EXISTS(SELECT 1 FROM reachable WHERE id=$2)`, m.p, i).Scan(&cycle)
	if e != nil {
		return result{}, e
	}
	if cycle {
		return result{}, fail(409, "dependency_cycle")
	}
	kind := "task.updated"
	if len(parts) == 0 {
		kind = "task.created"
	}
	out := outcome(kind, i, v, nil)
	if len(parts) == 0 {
		out.status = 201
	}
	return out, nil
}
func (m *mutation) taskBlocked(i string) (bool, error) {
	var blocked bool
	e := m.tx.QueryRow(m.ctx(), `SELECT EXISTS(SELECT 1 FROM task_dependencies d JOIN tasks t ON t.project_id=d.project_id AND t.id=d.depends_on_id WHERE d.project_id=$1 AND d.task_id=$2 AND t.state<>'done') OR EXISTS(SELECT 1 FROM blockers WHERE project_id=$1 AND task_id=$2 AND resolved_at IS NULL)`, m.p, i).Scan(&blocked)
	return blocked, e
}
func (m *mutation) resource(parts []string) (result, error) {
	if len(parts) < 2 {
		return result{}, fail(404, "not_found")
	}
	return m.lease("resource_leases", strings.Join(parts[:len(parts)-1], "/"), parts[len(parts)-1])
}

type leaseInput struct {
	Generation    int64       `json:"lease_generation"`
	Secret        string      `json:"lease_secret"`
	TTL           int         `json:"ttl_seconds"`
	RecoveryNote  string      `json:"recovery_note"`
	ExternalState string      `json:"external_state"`
	Progress      M           `json:"progress"`
	ExternalJob   M           `json:"external_job"`
	Evidence      *contextRef `json:"evidence"`
	Reason        string      `json:"reason"`
}

func (m *mutation) lease(table, i, op string) (result, error) {
	if m.r.Method != "POST" {
		return result{}, fail(405, "method_not_allowed")
	}
	if table == "tasks" && op == "blockers" {
		return m.blocker(i)
	}
	switch op {
	case "claim", "renew", "release", "complete", "rotate-lease", "checkpoint", "force-release":
	default:
		return result{}, fail(404, "not_found")
	}
	if table == "resource_leases" && op == "complete" || table == "tasks" && op == "checkpoint" {
		return result{}, fail(404, "not_found")
	}
	var in leaseInput
	if e := decode(m.body, &in); e != nil {
		return result{}, e
	}
	if in.TTL == 0 {
		in.TTL = 300
	}
	if in.TTL < 60 || in.TTL > 900 {
		return result{}, fail(400, "invalid_lease_ttl")
	}
	col := "id"
	kind := "task"
	if table == "resource_leases" {
		col = "name"
		kind = "resource"
		if op == "claim" {
			if len(i) > 200 || i == "" {
				return result{}, fail(400, "invalid_resource_name")
			}
			if _, e := m.tx.Exec(m.ctx(), "INSERT INTO resource_leases(project_id,name) VALUES($1,$2) ON CONFLICT DO NOTHING", m.p, i); e != nil {
				return result{}, e
			}
		}
	}
	var v, gen int64
	var owner *string
	var h []byte
	var expiry *time.Time
	var state string
	sql := "SELECT version,owner_id,lease_generation,lease_hash,lease_expires_at"
	if table == "tasks" {
		sql += ",state"
	}
	sql += " FROM " + table + " WHERE project_id=$1 AND " + col + "=$2"
	args := []any{&v, &owner, &gen, &h, &expiry}
	if table == "tasks" {
		args = append(args, &state)
	}
	e := m.tx.QueryRow(m.ctx(), sql, m.p, i).Scan(args...)
	if errors.Is(e, pgx.ErrNoRows) {
		return result{}, fail(404, "not_found")
	}
	if e != nil {
		return result{}, e
	}
	active := owner != nil && expiry != nil && expiry.After(m.now)
	var sec string
	switch op {
	case "claim":
		if active {
			return result{}, fail(409, "already_claimed")
		}
		if table == "tasks" {
			if state == "done" || state == "cancelled" {
				return result{}, fail(409, "task_closed")
			}
			blocked, e := m.taskBlocked(i)
			if e != nil {
				return result{}, e
			}
			if blocked {
				return result{}, fail(409, "task_blocked")
			}
		}
		if owner != nil && (strings.TrimSpace(in.RecoveryNote) == "" || strings.TrimSpace(in.ExternalState) == "") {
			return result{}, fail(409, "recovery_required")
		}
		own := m.a.Actor
		owner = &own
		gen++
		sec = secret()
		h = hash(sec)
		until := m.now.Add(time.Duration(in.TTL) * time.Second)
		expiry = &until
	case "force-release":
		if ranks[m.a.Role] < 3 {
			return result{}, fail(403, "forbidden")
		}
		if strings.TrimSpace(in.Reason) == "" {
			return result{}, fail(400, "reason_required")
		}
		if e = m.audit(kind+".force_released", i, in.Reason); e != nil {
			return result{}, e
		}
		gen++
		h = nil
		until := m.now.Add(-time.Second)
		expiry = &until // retain owner, requiring recovery verification
	default:
		if !active || *owner != m.a.Actor {
			return result{}, fail(409, "ownership_lost")
		}
		if op != "rotate-lease" && (in.Generation != gen || subtle.ConstantTimeCompare(h, hash(in.Secret)) != 1) {
			return result{}, fail(409, "lease_fenced")
		}
		if op == "rotate-lease" {
			gen++
			sec = secret()
			h = hash(sec)
		}
		if op == "renew" {
			until := m.now.Add(time.Duration(in.TTL) * time.Second)
			expiry = &until
		}
		if op == "release" || op == "complete" {
			if op == "complete" {
				blocked, e := m.taskBlocked(i)
				if e != nil {
					return result{}, e
				}
				if blocked {
					return result{}, fail(409, "task_blocked")
				}
				if in.Evidence == nil {
					return result{}, fail(400, "completion_evidence_required")
				}
				var exists bool
				if e = m.tx.QueryRow(m.ctx(), "SELECT EXISTS(SELECT 1 FROM context_versions WHERE project_id=$1 AND context_id=$2 AND version=$3)", m.p, in.Evidence.ID, in.Evidence.Version).Scan(&exists); e != nil {
					return result{}, e
				}
				if !exists {
					return result{}, fail(404, "evidence_not_found")
				}
			}
			owner = nil
			expiry = nil
			h = nil
		}
	}
	v++
	if table == "tasks" {
		if op == "claim" {
			state = "doing"
		}
		if op == "release" {
			state = "todo"
		}
		if op == "complete" {
			state = "done"
		}
		progress := in.Progress
		if progress == nil {
			progress = M{}
		}
		if in.Evidence != nil {
			progress["completion_evidence"] = in.Evidence
		}
		if in.RecoveryNote != "" {
			progress["recovery_note"] = in.RecoveryNote
			progress["external_state"] = in.ExternalState
		}
		_, e = m.tx.Exec(m.ctx(), "UPDATE tasks SET version=$3,owner_id=$4,lease_generation=$5,lease_hash=$6,lease_expires_at=$7,state=$8,progress=progress || $9::jsonb WHERE project_id=$1 AND id=$2", m.p, i, v, owner, gen, h, expiry, state, jsonBytes(progress))
	} else {
		job := in.ExternalJob
		if job == nil {
			job = M{}
		}
		if op == "checkpoint" && len(job) == 0 {
			return result{}, fail(400, "external_job_required")
		}
		if in.RecoveryNote != "" {
			job["recovery_note"] = in.RecoveryNote
			job["external_state"] = in.ExternalState
		}
		_, e = m.tx.Exec(m.ctx(), "UPDATE resource_leases SET version=$3,owner_id=$4,lease_generation=$5,lease_hash=$6,lease_expires_at=$7,external_job=external_job || $8::jsonb WHERE project_id=$1 AND name=$2", m.p, i, v, owner, gen, h, expiry, jsonBytes(job))
	}
	if e != nil {
		return result{}, e
	}
	out := outcome(kind+"."+op, i, v, M{"id": i, "version": v, "lease_generation": gen, "lease_expires_at": expiry})
	if sec != "" {
		out.body["lease_secret"] = sec
		out.secret = true
	}
	return out, nil
}
func (m *mutation) blocker(task string) (result, error) {
	var in struct {
		Reason     string    `json:"reason"`
		Owner      string    `json:"owner_id"`
		CheckAfter time.Time `json:"check_after"`
	}
	if e := decode(m.body, &in); e != nil {
		return result{}, e
	}
	if strings.TrimSpace(in.Reason) == "" || in.CheckAfter.IsZero() {
		return result{}, fail(400, "blocker_fields_required")
	}
	if in.Owner == "" {
		in.Owner = m.a.Actor
	}
	var ok bool
	if e := m.tx.QueryRow(m.ctx(), "SELECT EXISTS(SELECT 1 FROM tasks WHERE project_id=$1 AND id=$2) AND EXISTS(SELECT 1 FROM principals WHERE project_id=$1 AND id=$3)", m.p, task, in.Owner).Scan(&ok); e != nil {
		return result{}, e
	}
	if !ok {
		return result{}, fail(404, "not_found")
	}
	i := id("blk")
	if _, e := m.tx.Exec(m.ctx(), "INSERT INTO blockers(project_id,id,task_id,reason,owner_id,check_after) VALUES($1,$2,$3,$4,$5,$6)", m.p, i, task, in.Reason, in.Owner, in.CheckAfter); e != nil {
		return result{}, e
	}
	return outcome("blocker.created", i, 1, nil), nil
}
func (m *mutation) resolve(parts []string) (result, error) {
	if len(parts) != 2 || parts[1] != "resolve" || m.r.Method != "POST" {
		return result{}, fail(404, "not_found")
	}
	var in struct {
		Evidence contextRef `json:"evidence"`
	}
	if e := decode(m.body, &in); e != nil {
		return result{}, e
	}
	var ok bool
	if e := m.tx.QueryRow(m.ctx(), "SELECT EXISTS(SELECT 1 FROM context_versions WHERE project_id=$1 AND context_id=$2 AND version=$3)", m.p, in.Evidence.ID, in.Evidence.Version).Scan(&ok); e != nil {
		return result{}, e
	}
	if !ok {
		return result{}, fail(404, "evidence_not_found")
	}
	tag, e := m.tx.Exec(m.ctx(), "UPDATE blockers SET resolved_at=$3,resolution_context_id=$4,resolution_version=$5 WHERE project_id=$1 AND id=$2 AND resolved_at IS NULL", m.p, parts[0], m.now, in.Evidence.ID, in.Evidence.Version)
	if e != nil {
		return result{}, e
	}
	if tag.RowsAffected() == 0 {
		return result{}, fail(404, "open_blocker_not_found")
	}
	return outcome("blocker.resolved", parts[0], 1, nil), nil
}
