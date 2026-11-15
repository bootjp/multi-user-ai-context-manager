package service

import (
	"bytes"
	"context"
	"encoding/json"

	"fmt"
	"net/http"

	"github.com/jackc/pgx/v5"
	"strconv"
	"time"
)

func toString(v any) string { return fmt.Sprint(v) }
func objects(ctx context.Context, tx pgx.Tx, sql string, args ...any) ([]M, error) {
	rows, e := tx.Query(ctx, "SELECT row_to_json(x) FROM ("+sql+") x", args...)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []M{}
	for rows.Next() {
		var b []byte
		if e = rows.Scan(&b); e != nil {
			return nil, e
		}
		var m M
		decoder := json.NewDecoder(bytes.NewReader(b))
		decoder.UseNumber()
		if e = decoder.Decode(&m); e != nil {
			return nil, e
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

const contextSelect = `SELECT c.id,c.kind,c.current_version AS version,c.archived,v.document,(SELECT json_build_object('principal_id',a.id,'display_name',a.display_name) FROM principals a WHERE a.project_id=c.project_id AND a.id=v.recorded_by) AS recorded_by,v.revision,v.created_at,EXISTS(SELECT 1 FROM context_links l JOIN contexts target ON target.project_id=l.project_id AND target.id=l.target_id WHERE l.project_id=c.project_id AND l.source_id=c.id AND l.source_version=c.current_version AND target.current_version<>l.target_version) AS stale FROM contexts c JOIN context_versions v ON v.project_id=c.project_id AND v.context_id=c.id AND v.version=c.current_version`
const taskSelect = `SELECT t.id,t.title,t.acceptance,t.state,t.version,t.owner_id,(SELECT display_name FROM principals a WHERE a.project_id=t.project_id AND a.id=t.owner_id) AS owner_display_name,t.lease_generation,t.lease_expires_at,t.progress,COALESCE((SELECT json_agg(d.depends_on_id ORDER BY d.depends_on_id) FROM task_dependencies d WHERE d.project_id=t.project_id AND d.task_id=t.id),'[]') AS depends_on,COALESCE((SELECT json_agg(b.id ORDER BY b.id) FROM blockers b WHERE b.project_id=t.project_id AND b.task_id=t.id AND b.resolved_at IS NULL),'[]') AS unresolved_blockers,EXISTS(SELECT 1 FROM task_dependencies d JOIN tasks dep ON dep.project_id=d.project_id AND dep.id=d.depends_on_id WHERE d.project_id=t.project_id AND d.task_id=t.id AND dep.state<>'done') OR EXISTS(SELECT 1 FROM blockers b WHERE b.project_id=t.project_id AND b.task_id=t.id AND b.resolved_at IS NULL) AS blocked, t.owner_id IS NOT NULL AND t.lease_expires_at<=clock_timestamp() AS ownership_lost FROM tasks t`

func decorateTasks(list []M, now time.Time) {
	for _, t := range list {
		blocked, _ := t["blocked"].(bool)
		lost, _ := t["ownership_lost"].(bool)
		state, _ := t["state"].(string)
		active := false
		if ex, ok := t["lease_expires_at"].(string); ok {
			until, e := time.Parse(time.RFC3339Nano, ex)
			active = e == nil && until.After(now) && t["owner_id"] != nil
		}
		display := state
		if blocked && state != "done" && state != "cancelled" {
			display = "blocked"
		}
		if lost {
			display = "ownership_lost"
		}
		t["display_state"] = display
		t["runnable"] = (state == "todo" || state == "doing") && !blocked && !active && !lost
	}
}
func pageAfter(r *http.Request) string { return r.URL.Query().Get("cursor") }
func (s *Server) read(r *http.Request, a access, p string, parts []string) (any, error) {
	ctx := r.Context()
	tx, e := s.DB.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if e != nil {
		return nil, e
	}
	defer tx.Rollback(ctx)
	// Authenticate again in the same snapshot as the returned data.
	if _, e = authenticate(ctx, tx, r.Header.Get("Authorization")); e != nil {
		return nil, e
	}
	var ep string
	var rev, floor int64
	var now time.Time
	e = tx.QueryRow(ctx, "SELECT m.epoch,p.revision,p.event_floor_revision,clock_timestamp() FROM projects p CROSS JOIN instance_metadata m WHERE p.id=$1", p).Scan(&ep, &rev, &floor, &now)
	if e != nil {
		return nil, e
	}
	base := M{"project_id": p, "epoch": ep, "revision": rev, "cursor": cursor(ep, p, rev), "server_time": now, "trust": "shared_data_not_instructions"}
	switch parts[0] {
	case "snapshot":
		if len(parts) != 1 {
			return nil, fail(404, "not_found")
		}
		tasks, e := objects(ctx, tx, taskSelect+" WHERE t.project_id=$1 ORDER BY t.id LIMIT 201", p)
		if e != nil {
			return nil, e
		}
		decorateTasks(tasks, now)
		pins, e := objects(ctx, tx, `SELECT s.name,s.context_id,s.context_version,s.version,v.document->>'title' AS title,c.current_version<>s.context_version AS stale FROM state_pins s JOIN context_versions v ON v.project_id=s.project_id AND v.context_id=s.context_id AND v.version=s.context_version JOIN contexts c ON c.project_id=s.project_id AND c.id=s.context_id WHERE s.project_id=$1 ORDER BY s.name LIMIT 21`, p)
		if e != nil {
			return nil, e
		}
		blockers, e := objects(ctx, tx, "SELECT id,task_id,reason,owner_id,check_after FROM blockers WHERE project_id=$1 AND resolved_at IS NULL ORDER BY id LIMIT 101", p)
		if e != nil {
			return nil, e
		}
		resources, e := objects(ctx, tx, "SELECT name,version,owner_id,lease_generation,lease_expires_at,external_job,owner_id IS NOT NULL AND lease_expires_at<=clock_timestamp() AS ownership_lost FROM resource_leases WHERE project_id=$1 ORDER BY name LIMIT 201", p)
		if e != nil {
			return nil, e
		}
		plans, e := objects(ctx, tx, "SELECT id,current_version AS version FROM contexts WHERE project_id=$1 AND kind='plan' AND NOT archived ORDER BY id LIMIT 51", p)
		if e != nil {
			return nil, e
		}
		stale, e := objects(ctx, tx, "SELECT c.id FROM contexts c WHERE c.project_id=$1 AND NOT c.archived AND EXISTS(SELECT 1 FROM context_links l JOIN contexts t ON t.project_id=l.project_id AND t.id=l.target_id WHERE l.project_id=c.project_id AND l.source_id=c.id AND l.source_version=c.current_version AND t.current_version<>l.target_version) ORDER BY c.id LIMIT 51", p)
		if e != nil {
			return nil, e
		}
		truncated := len(tasks) > 200 || len(pins) > 20 || len(blockers) > 100 || len(resources) > 200 || len(plans) > 50 || len(stale) > 50
		base["tasks"] = tasks[:min(len(tasks), 200)]
		base["state_contexts"] = pins[:min(len(pins), 20)]
		base["open_blockers"] = blockers[:min(len(blockers), 100)]
		base["resources"] = resources[:min(len(resources), 200)]
		base["active_plans"] = plans[:min(len(plans), 50)]
		base["stale_contexts"] = stale[:min(len(stale), 50)]
		base["truncated"] = truncated
		if truncated {
			base["continuations"] = M{"tasks": "/v1/projects/" + p + "/tasks", "state_pins": "/v1/projects/" + p + "/state-pins", "blockers": "/v1/projects/" + p + "/blockers", "resources": "/v1/projects/" + p + "/resources", "contexts": "/v1/projects/" + p + "/contexts"}
		}
		return base, nil
	case "contexts":
		if len(parts) == 2 {
			var version int64
			v := r.URL.Query().Get("version")
			if v != "" {
				version, e = strconv.ParseInt(v, 10, 64)
				if e != nil || version < 1 {
					return nil, fail(400, "invalid_version")
				}
			}
			list, e := objects(ctx, tx, `SELECT c.id,c.kind,v.version,c.archived,v.document,(SELECT json_build_object('principal_id',a.id,'display_name',a.display_name) FROM principals a WHERE a.project_id=c.project_id AND a.id=v.recorded_by) AS recorded_by,v.created_at,v.revision,EXISTS(SELECT 1 FROM context_links l JOIN contexts target ON target.project_id=l.project_id AND target.id=l.target_id WHERE l.project_id=c.project_id AND l.source_id=c.id AND l.source_version=v.version AND target.current_version<>l.target_version) AS stale FROM contexts c JOIN context_versions v ON v.project_id=c.project_id AND v.context_id=c.id WHERE c.project_id=$1 AND c.id=$2 AND v.version=CASE WHEN $3::bigint=0 THEN c.current_version ELSE $3 END`, p, parts[1], version)
			if e != nil {
				return nil, e
			}
			if len(list) == 0 {
				return nil, fail(404, "not_found")
			}
			return list[0], nil
		}
		if len(parts) != 1 {
			return nil, fail(404, "not_found")
		}
		q := r.URL.Query()
		list, e := objects(ctx, tx, contextSelect+` WHERE c.project_id=$1 AND NOT c.archived AND c.id>$2 AND ($3='' OR c.kind=$3) AND ($4='' OR v.document->'tags' ? $4) AND ($5='' OR strpos(lower((v.document->>'title')||' '||(v.document->>'body')),lower($5))>0) ORDER BY c.id LIMIT 51`, p, pageAfter(r), q.Get("kind"), q.Get("tag"), q.Get("q"))
		if e != nil {
			return nil, e
		}
		base["items"], base["next_cursor"] = paginate(list, 50, "id")
		return base, nil
	case "tasks":
		if len(parts) > 2 {
			return nil, fail(404, "not_found")
		}
		query := taskSelect + " WHERE t.project_id=$1 AND t.id>$2"
		args := []any{p, pageAfter(r)}
		if len(parts) == 2 {
			query = taskSelect + " WHERE t.project_id=$1 AND t.id=$2"
			args[1] = parts[1]
		}
		q := r.URL.Query()
		query += " AND ($3='' OR t.state=$3) AND ($4='' OR t.owner_id=$4)"
		args = append(args, q.Get("state"), q.Get("owner"))
		if q.Get("runnable") == "true" {
			query += ` AND t.state IN ('todo','doing') AND t.owner_id IS NULL AND NOT EXISTS(SELECT 1 FROM task_dependencies d JOIN tasks dep ON dep.project_id=d.project_id AND dep.id=d.depends_on_id WHERE d.project_id=t.project_id AND d.task_id=t.id AND dep.state<>'done') AND NOT EXISTS(SELECT 1 FROM blockers b WHERE b.project_id=t.project_id AND b.task_id=t.id AND b.resolved_at IS NULL)`
		}
		list, e := objects(ctx, tx, query+" ORDER BY t.id LIMIT 51", args...)
		if e != nil {
			return nil, e
		}
		decorateTasks(list, now)
		if len(parts) == 2 {
			if len(list) == 0 {
				return nil, fail(404, "not_found")
			}
			return list[0], nil
		}
		base["items"], base["next_cursor"] = paginate(list, 50, "id")
		return base, nil
	case "blockers", "resources", "state-pins", "tokens", "audit":
		if len(parts) != 1 {
			return nil, fail(404, "not_found")
		}
		var query, key string
		switch parts[0] {
		case "blockers":
			query = "SELECT id,task_id,reason,owner_id,check_after,resolved_at,resolution_context_id,resolution_version FROM blockers WHERE project_id=$1 AND id>$2 ORDER BY id LIMIT 51"
			key = "id"
		case "resources":
			query = "SELECT name,version,owner_id,lease_generation,lease_expires_at,external_job FROM resource_leases WHERE project_id=$1 AND name>$2 ORDER BY name LIMIT 51"
			key = "name"
		case "state-pins":
			query = "SELECT name,context_id,context_version,version FROM state_pins WHERE project_id=$1 AND name>$2 ORDER BY name LIMIT 51"
			key = "name"
		case "tokens":
			if ranks[a.Role] < 4 {
				return nil, fail(403, "forbidden")
			}
			query = "SELECT t.id,t.principal_id,p.display_name,t.role,t.expires_at,t.revoked_at FROM tokens t JOIN principals p ON p.project_id=t.project_id AND p.id=t.principal_id WHERE t.project_id=$1 AND t.id>$2 ORDER BY t.id LIMIT 51"
			key = "id"
		case "audit":
			if ranks[a.Role] < 4 {
				return nil, fail(403, "forbidden")
			}
			query = "SELECT id,actor_id,action,target,reason,created_at FROM audit_entries WHERE project_id=$1 AND id>COALESCE(NULLIF($2,'')::bigint,0) ORDER BY id LIMIT 51"
			key = "id"
		}
		list, e := objects(ctx, tx, query, p, pageAfter(r))
		if e != nil {
			return nil, e
		}
		base["items"], base["next_cursor"] = paginate(list, 50, key)
		return base, nil
	case "events":
		after := r.URL.Query().Get("after")
		if after == "" {
			after = cursor(ep, p, 0)
		}
		n, e := parseCursor(after, ep, p, rev, floor)
		if e != nil {
			return nil, e
		}
		limit := 100
		if raw := r.URL.Query().Get("limit"); raw != "" {
			limit, e = strconv.Atoi(raw)
			if e != nil || limit < 1 || limit > 100 {
				return nil, fail(400, "invalid_limit")
			}
		}
		list, e := objects(ctx, tx, "SELECT revision,type,resource_id,resource_version,actor_id,created_at FROM project_events WHERE project_id=$1 AND revision>$2 ORDER BY revision LIMIT $3", p, n, limit)
		if e != nil {
			return nil, e
		}
		if len(list) > 0 {
			n = number(list[len(list)-1]["revision"])
		}
		base["items"] = list
		base["next_cursor"] = cursor(ep, p, n)
		return base, nil
	case "context-pack":
		return contextPack(ctx, tx, r, p, base)
	}
	return nil, fail(404, "not_found")
}
func paginate(list []M, limit int, key string) ([]M, string) {
	next := ""
	if len(list) > limit {
		list = list[:limit]
		next = toString(list[len(list)-1][key])
	}
	return list, next
}
func contextPack(ctx context.Context, tx pgx.Tx, r *http.Request, p string, base M) (any, error) {
	maxBytes := 32768
	if v := r.URL.Query().Get("max_bytes"); v != "" {
		n, e := strconv.Atoi(v)
		if e != nil || n < 512 || n > 256*1024 {
			return nil, fail(400, "invalid_max_bytes")
		}
		maxBytes = n
	}
	task := r.URL.Query().Get("task_id")
	if task != "" {
		var ok bool
		if e := tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM tasks WHERE project_id=$1 AND id=$2)", p, task).Scan(&ok); e != nil {
			return nil, e
		}
		if !ok {
			return nil, fail(404, "task_not_found")
		}
	}
	// Bounded candidates; total and excluded counts disclose omissions.
	list, e := objects(ctx, tx, `SELECT c.id,c.current_version AS version,v.document,EXISTS(SELECT 1 FROM state_pins pin WHERE pin.project_id=c.project_id AND pin.context_id=c.id AND pin.context_version=v.version) AS pinned,EXISTS(SELECT 1 FROM context_links l JOIN contexts t ON t.project_id=l.project_id AND t.id=l.target_id WHERE l.project_id=c.project_id AND l.source_id=c.id AND l.source_version=v.version AND t.current_version<>l.target_version) AS stale FROM contexts c JOIN context_versions v ON v.project_id=c.project_id AND v.context_id=c.id AND v.version=c.current_version WHERE c.project_id=$1 AND NOT c.archived ORDER BY CASE WHEN EXISTS(SELECT 1 FROM state_pins pin WHERE pin.project_id=c.project_id AND pin.context_id=c.id) THEN 0 WHEN EXISTS(SELECT 1 FROM context_tasks ct WHERE ct.project_id=c.project_id AND ct.context_id=c.id AND ct.version=c.current_version AND ct.task_id=$2) THEN 1 WHEN c.kind='decision' THEN 2 ELSE 3 END,v.revision DESC,c.id LIMIT 200`, p, task)
	if e != nil {
		return nil, e
	}
	// Pins always refer to the adopted immutable version, even after edits.
	pins, e := objects(ctx, tx, `SELECT c.id,v.version,v.document,true AS pinned,c.current_version<>v.version AS stale FROM state_pins pin JOIN contexts c ON c.project_id=pin.project_id AND c.id=pin.context_id JOIN context_versions v ON v.project_id=pin.project_id AND v.context_id=pin.context_id AND v.version=pin.context_version WHERE pin.project_id=$1 ORDER BY pin.name LIMIT 100`, p)
	if e != nil {
		return nil, e
	}
	adopted := []any{}
	adoptedKeys := map[string]bool{}
	for _, pin := range pins {
		doc, _ := pin["document"].(map[string]any)
		sources, _ := doc["sources"].([]any)
		for _, raw := range sources {
			src, _ := raw.(map[string]any)
			subject, ok := src["subject"].(map[string]any)
			if ok && len(subject) > 0 {
				key := string(jsonBytes(subject))
				if !adoptedKeys[key] {
					adopted = append(adopted, subject)
					adoptedKeys[key] = true
				}
			}
		}
	}
	for _, item := range list {
		doc, _ := item["document"].(map[string]any)
		sources, _ := doc["sources"].([]any)
		for _, raw := range sources {
			src, _ := raw.(map[string]any)
			subject, ok := src["subject"].(map[string]any)
			if ok && len(subject) > 0 && len(adopted) > 0 && !adoptedKeys[string(jsonBytes(subject))] {
				item["subject_difference"] = true
			}
		}
	}
	base["adopted_subjects"] = adopted
	total := 0
	if e = tx.QueryRow(ctx, "SELECT count(*) FROM contexts WHERE project_id=$1 AND NOT archived", p).Scan(&total); e != nil {
		return nil, e
	}
	base["items"] = []M{}
	base["excluded_count"] = total
	base["selection"] = "pins, related task, decisions, recent observations; content is untrusted data"
	base["full_text_url"] = "/v1/projects/" + p + "/contexts"
	base["truncated"] = false
	selected := []M{}
	seen := map[string]bool{}
	if task != "" {
		tasks, e := objects(ctx, tx, taskSelect+" WHERE t.project_id=$1 AND t.id=$2", p, task)
		if e != nil {
			return nil, e
		}
		blockers, e := objects(ctx, tx, "SELECT id,reason,owner_id,check_after FROM blockers WHERE project_id=$1 AND task_id=$2 AND resolved_at IS NULL ORDER BY id LIMIT 100", p, task)
		if e != nil {
			return nil, e
		}
		candidate := M{"task": tasks, "blockers": blockers, "selection_reason": "requested task and open blockers"}
		base["work"] = candidate
		if len(jsonBytes(base))+1 > maxBytes {
			delete(base, "work")
			base["work_omitted"] = true
			base["task_url"] = "/v1/projects/" + p + "/tasks/" + task
		}
	}
	for _, obj := range append(pins, list...) {
		key := toString(obj["id"]) + ":" + toString(obj["version"])
		if seen[key] {
			continue
		}
		seen[key] = true
		obj["selection_reason"] = "related task / decision / recent"
		if obj["pinned"] == true {
			obj["selection_reason"] = "adopted immutable pin"
		}
		base["items"] = append(append([]M{}, selected...), obj)
		if len(jsonBytes(base))+1 > maxBytes {
			base["items"] = selected
			continue
		}
		selected = append(selected, obj)
	}
	// Count immutable versions, including archived pin targets, rather than context IDs.
	var totalVersions int
	if e = tx.QueryRow(ctx, `SELECT count(*) FROM (SELECT id,current_version AS version FROM contexts WHERE project_id=$1 AND NOT archived UNION SELECT context_id,context_version FROM state_pins WHERE project_id=$1) candidates`, p).Scan(&totalVersions); e != nil {
		return nil, e
	}
	var allPinVersions int
	if e = tx.QueryRow(ctx, "SELECT count(*) FROM (SELECT DISTINCT context_id,context_version FROM state_pins WHERE project_id=$1) pins", p).Scan(&allPinVersions); e != nil {
		return nil, e
	}
	base["omitted_pin_count"] = 0
	finalize := func() {
		chosen := map[string]bool{}
		for _, item := range selected {
			chosen[toString(item["id"])+":"+toString(item["version"])] = true
		}
		omitted := 0
		pinKeys := map[string]bool{}
		for _, pin := range pins {
			key := toString(pin["id"]) + ":" + toString(pin["version"])
			if !pinKeys[key] && !chosen[key] {
				omitted++
			}
			pinKeys[key] = true
		}
		omitted += max(0, allPinVersions-len(pinKeys))
		base["excluded_count"] = max(0, totalVersions-len(chosen))
		base["omitted_pin_count"] = omitted
		base["truncated"] = base["excluded_count"].(int) > 0 || base["work_omitted"] == true
		base["items"] = selected
	}
	finalize()
	for len(jsonBytes(base))+1 > maxBytes && len(selected) > 0 {
		selected = selected[:len(selected)-1]
		finalize()
	}

	if len(jsonBytes(base))+1 > maxBytes {
		return nil, fail(400, "max_bytes_too_small_for_envelope")
	}
	return base, nil
}
