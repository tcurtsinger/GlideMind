// Package audit keeps a local, append-only JSONL trail of glm's own writes
// (DESIGN-WRITES.md W6). It records what glm did — timestamp, instance,
// identity, method, target, and field NAMES — never field values, so no
// sensitive record data accumulates at rest locally. ServiceNow's server-side
// sys_audit/history keeps the values; this log answers "what did glm touch,
// as whom, from here."
package audit

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// EnvLogPath overrides the default audit log location.
const EnvLogPath = "GLM_AUDIT_LOG"

// Entry is one write attempt. Result records the outcome ("ok" or "error"),
// never response data.
type Entry struct {
	Time     time.Time `json:"time"`
	Instance string    `json:"instance"`
	Profile  string    `json:"profile"`
	User     string    `json:"user"`
	Command  string    `json:"command"` // e.g. "api"; later "create"/"update"/"delete"
	Method   string    `json:"method"`
	Target   string    `json:"target"`           // path (api) or table/key (verbs)
	Fields   []string  `json:"fields,omitempty"` // field names only — no values
	Result   string    `json:"result"`
}

// Path returns the audit log location: GLM_AUDIT_LOG, or
// <user cache dir>/glidemind/audit.jsonl (%LOCALAPPDATA% on Windows).
func Path() (string, error) {
	if p := os.Getenv(EnvLogPath); p != "" {
		return p, nil
	}
	dir, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("locate audit log dir: %w", err)
	}
	return filepath.Join(dir, "glidemind", "audit.jsonl"), nil
}

// Append writes one entry. The log is best-effort by design: a failure here
// must never block or fail the write itself — the caller surfaces the error
// as a warning and moves on.
func Append(e Entry) error {
	p, err := Path()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return fmt.Errorf("create audit dir: %w", err)
	}
	line, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("encode audit entry: %w", err)
	}
	f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open audit log: %w", err)
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		f.Close()
		return fmt.Errorf("append audit log: %w", err)
	}
	return f.Close()
}

// BodyFieldNames extracts top-level key names from a JSON object body for
// the audit trail. Non-object bodies (arrays, scalars, empty) yield nil —
// the method and target still identify the write.
func BodyFieldNames(body []byte) []string {
	if len(body) == 0 {
		return nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return nil
	}
	names := make([]string, 0, len(m))
	for k := range m {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}
