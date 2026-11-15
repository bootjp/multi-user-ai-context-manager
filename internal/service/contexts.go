package service

import (
	"encoding/hex"
	"errors"
	"github.com/jackc/pgx/v5"
	"strings"
	"time"
)

type contextRef struct {
	ID      string `json:"id"`
	Version int64  `json:"version"`
}
type source struct {
	URI           string `json:"uri"`
	ContentSHA256 string `json:"content_sha256,omitempty"`
	ObservedAt    string `json:"observed_at,omitempty"`
	Subject       M      `json:"subject,omitempty"`
	MediaType     string `json:"media_type,omitempty"`
	LocalOnly     bool   `json:"local_only,omitempty"`
}
type contextDocument struct {
	Kind       string       `json:"kind"`
	Title      string       `json:"title"`
	Body       string       `json:"body"`
	Tags       []string     `json:"tags"`
	TaskIDs    []string     `json:"task_ids"`
	Sources    []source     `json:"sources"`
	References []contextRef `json:"references"`
	Supersedes []contextRef `json:"supersedes"`
	Summary    bool         `json:"summary"`
}

func (m *mutation) validateDocument(d *contextDocument) error {
	if !strings.Contains("|state|plan|observation|decision|handoff|", "|"+d.Kind+"|") || d.Kind == "" || strings.TrimSpace(d.Title) == "" {
		return fail(400, "invalid_context")
	}
	if len(d.Body) > 64*1024 {
		return fail(413, "body_too_large")
	}
	meta := *d
	meta.Body = ""
	if len(jsonBytes(meta)) > 16*1024 {
		return fail(413, "metadata_too_large")
	}
	if d.Summary && len(d.References) == 0 {
		return fail(400, "summary_references_required")
	}
	for i := range d.Sources {
		s := &d.Sources[i]
		if s.ObservedAt != "" {
			if _, e := time.Parse(time.RFC3339Nano, s.ObservedAt); e != nil {
				return fail(400, "invalid_observed_at")
			}
		}
		if s.ContentSHA256 != "" {
			b, e := hex.DecodeString(s.ContentSHA256)
			if e != nil || len(b) != 32 {
				return fail(400, "invalid_content_sha256")
			}
		}
		s.LocalOnly = strings.HasPrefix(s.URI, "file://") || strings.HasPrefix(s.URI, "/")
	}
	for _, ref := range append(append([]contextRef{}, d.References...), d.Supersedes...) {
		var exists bool
		e := m.tx.QueryRow(m.ctx(), "SELECT EXISTS(SELECT 1 FROM context_versions WHERE project_id=$1 AND context_id=$2 AND version=$3)", m.p, ref.ID, ref.Version).Scan(&exists)
		if e != nil {
			return e
		}
		if !exists {
			return fail(404, "reference_not_found")
		}
	}
	for _, t := range d.TaskIDs {
		var exists bool
		if e := m.tx.QueryRow(m.ctx(), "SELECT EXISTS(SELECT 1 FROM tasks WHERE project_id=$1 AND id=$2)", m.p, t).Scan(&exists); e != nil {
			return e
		}
		if !exists {
			return fail(404, "task_not_found")
		}
	}
	return nil
}
func (m *mutation) context(parts []string) (result, error) {
	i := id("ctx")
	v := int64(1)
	var doc contextDocument
	if len(parts) == 0 {
		if m.r.Method != "POST" {
			return result{}, fail(405, "method_not_allowed")
		}
		if e := decode(m.body, &doc); e != nil {
			return result{}, e
		}
	} else {
		if len(parts) > 2 {
			return result{}, fail(404, "not_found")
		}
		i = parts[0]
		var raw []byte
		var archived bool
		e := m.tx.QueryRow(m.ctx(), "SELECT c.current_version,c.archived,v.document FROM contexts c JOIN context_versions v ON v.project_id=c.project_id AND v.context_id=c.id AND v.version=c.current_version WHERE c.project_id=$1 AND c.id=$2", m.p, i).Scan(&v, &archived, &raw)
		if errors.Is(e, pgx.ErrNoRows) {
			return result{}, fail(404, "not_found")
		}
		if e != nil {
			return result{}, e
		}
		if e = checkVersion(m.r, v); e != nil {
			return result{}, e
		}
		if len(parts) == 2 {
			if m.r.Method != "POST" {
				return result{}, fail(405, "method_not_allowed")
			}
			if parts[1] == "archive" {
				if _, e = m.tx.Exec(m.ctx(), "UPDATE contexts SET archived=true,current_version=current_version+1 WHERE project_id=$1 AND id=$2", m.p, i); e != nil {
					return result{}, e
				}
				v++
				decode(raw, &doc)
			} else if parts[1] == "redact" {
				if ranks[m.a.Role] < 4 {
					return result{}, fail(403, "forbidden")
				}
				var in struct {
					Reason string `json:"reason"`
				}
				decode(m.body, &in)
				if strings.TrimSpace(in.Reason) == "" {
					return result{}, fail(400, "reason_required")
				}
				// Redaction invalidates all cached write preconditions and archives the record.
				if e = decode(raw, &doc); e != nil {
					return result{}, e
				}
				clean := contextDocument{Kind: doc.Kind, Title: "[redacted]", Body: "[redacted]"}
				if _, e = m.tx.Exec(m.ctx(), "UPDATE context_versions SET document=$3 WHERE project_id=$1 AND context_id=$2", m.p, i, jsonBytes(clean)); e != nil {
					return result{}, e
				}
				v++
				if _, e = m.tx.Exec(m.ctx(), "UPDATE contexts SET current_version=$3,archived=true WHERE project_id=$1 AND id=$2", m.p, i, v); e != nil {
					return result{}, e
				}
				if _, e = m.tx.Exec(m.ctx(), "INSERT INTO context_versions(project_id,context_id,version,document,recorded_by,revision) VALUES($1,$2,$3,$4,$5,$6)", m.p, i, v, jsonBytes(clean), m.a.Actor, m.rev); e != nil {
					return result{}, e
				}
				if e = m.audit("context.redacted", i, in.Reason); e != nil {
					return result{}, e
				}
				return outcome("context.redacted", i, v, nil), nil
			} else {
				return result{}, fail(404, "not_found")
			}
		} else {
			if m.r.Method != "PATCH" {
				return result{}, fail(405, "method_not_allowed")
			}
			if archived {
				return result{}, fail(409, "context_archived")
			}
			var original, patch M
			if e = decode(raw, &original); e != nil {
				return result{}, e
			}
			if e = decode(m.body, &patch); e != nil {
				return result{}, e
			}
			for k, val := range patch {
				original[k] = val
			}
			if e = decode(jsonBytes(original), &doc); e != nil {
				return result{}, fail(400, "invalid_context")
			}
			v++
		}
	}
	if e := m.validateDocument(&doc); e != nil {
		return result{}, e
	}
	if len(parts) == 0 {
		if _, e := m.tx.Exec(m.ctx(), "INSERT INTO contexts(project_id,id,kind,current_version) VALUES($1,$2,$3,$4)", m.p, i, doc.Kind, v); e != nil {
			return result{}, e
		}
	} else {
		if _, e := m.tx.Exec(m.ctx(), "UPDATE contexts SET current_version=$3,kind=$4 WHERE project_id=$1 AND id=$2", m.p, i, v, doc.Kind); e != nil {
			return result{}, e
		}
	}
	if _, e := m.tx.Exec(m.ctx(), "INSERT INTO context_versions(project_id,context_id,version,document,recorded_by,revision) VALUES($1,$2,$3,$4,$5,$6)", m.p, i, v, jsonBytes(doc), m.a.Actor, m.rev); e != nil {
		return result{}, e
	}
	for _, ref := range append(append([]contextRef{}, doc.References...), doc.Supersedes...) {
		if _, e := m.tx.Exec(m.ctx(), "INSERT INTO context_links VALUES($1,$2,$3,$4,$5) ON CONFLICT DO NOTHING", m.p, i, v, ref.ID, ref.Version); e != nil {
			return result{}, e
		}
	}
	for _, task := range doc.TaskIDs {
		if _, e := m.tx.Exec(m.ctx(), "INSERT INTO context_tasks VALUES($1,$2,$3,$4) ON CONFLICT DO NOTHING", m.p, i, v, task); e != nil {
			return result{}, e
		}
	}
	kind := "context.updated"
	if len(parts) == 0 {
		kind = "context.created"
	}
	if len(parts) == 2 {
		kind = "context.archived"
	}
	out := outcome(kind, i, v, nil)
	if len(parts) == 0 {
		out.status = 201
	}
	return out, nil
}
func (m *mutation) pin(parts []string) (result, error) {
	if ranks[m.a.Role] < 3 {
		return result{}, fail(403, "forbidden")
	}
	if len(parts) != 1 || m.r.Method != "PUT" {
		return result{}, fail(404, "not_found")
	}
	var in struct {
		ContextID string `json:"context_id"`
		Version   int64  `json:"version"`
	}
	if e := decode(m.body, &in); e != nil {
		return result{}, e
	}
	var exists bool
	if e := m.tx.QueryRow(m.ctx(), "SELECT EXISTS(SELECT 1 FROM context_versions WHERE project_id=$1 AND context_id=$2 AND version=$3)", m.p, in.ContextID, in.Version).Scan(&exists); e != nil {
		return result{}, e
	}
	if !exists {
		return result{}, fail(404, "reference_not_found")
	}
	var v int64
	e := m.tx.QueryRow(m.ctx(), "SELECT version FROM state_pins WHERE project_id=$1 AND name=$2", m.p, parts[0]).Scan(&v)
	if errors.Is(e, pgx.ErrNoRows) {
		if m.r.Header.Get("If-None-Match") != "*" {
			return result{}, fail(428, "precondition_required")
		}
	} else if e != nil {
		return result{}, e
	} else {
		if e = checkVersion(m.r, v); e != nil {
			return result{}, e
		}
	}
	v++
	_, e = m.tx.Exec(m.ctx(), "INSERT INTO state_pins VALUES($1,$2,$3,$4,$5) ON CONFLICT(project_id,name) DO UPDATE SET context_id=excluded.context_id,context_version=excluded.context_version,version=excluded.version", m.p, parts[0], in.ContextID, in.Version, v)
	if e != nil {
		return result{}, e
	}
	return outcome("state_pin.updated", parts[0], v, nil), nil
}
