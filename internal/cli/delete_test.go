package cli

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tcurtsinger/GlideMind/internal/audit"
	"github.com/tcurtsinger/GlideMind/internal/config"
	"github.com/tcurtsinger/GlideMind/internal/exit"
)

// TestDeleteGateReadOnlyProfile: gate 1 (W1) fires before the resolve GET,
// credentials, and the DELETE — on both a named read-only profile and the
// always-read-only env profile.
func TestDeleteGateReadOnlyProfile(t *testing.T) {
	hits := map[string]int{}
	srv := fakeInstance(t, hits)

	_, _, err := runGlmErr(t, srv, "", "delete", "incident", sysIDa, "--yes")
	if err == nil || !strings.Contains(err.Error(), "read-only") || !strings.Contains(err.Error(), "profile add") {
		t.Fatalf("env profile must refuse delete with the profile-add remedy, got: %v", err)
	}

	isolateConfig(t)
	f := &config.File{Profiles: map[string]config.Profile{
		"ro": {Instance: srv.URL, Auth: "basic", Username: "svc.glm"},
	}}
	if err := f.Save(); err != nil {
		t.Fatalf("save config: %v", err)
	}
	_, _, err = runGlmErr(t, srv, "", "delete", "incident", sysIDa, "-p", "ro", "--yes")
	if err == nil || !strings.Contains(err.Error(), "glm profile write-enable ro") {
		t.Fatalf("read-only profile must refuse delete naming the fix, got: %v", err)
	}
	if hits["get"] != 0 || hits["delete"] != 0 || hits["list"] != 0 {
		t.Errorf("refused delete must never reach the instance: %v", hits)
	}
}

// TestDeleteResolvesAndSends: a human key (record number) is resolved through
// get's resolver to a sys_id, previewed with both identifiers, then deleted.
func TestDeleteResolvesAndSends(t *testing.T) {
	hits := map[string]int{}
	srv := fakeInstance(t, hits)
	writableProfile(t, srv, "w")

	_, stderr := runGlm(t, srv, "", "delete", "incident", "INC0000001", "-p", "w", "--yes")
	if !strings.Contains(stderr, "DELETE "+srv.URL+"/api/now/table/incident/"+sysIDa) {
		t.Errorf("preview must show the exact DELETE target: %q", stderr)
	}
	if !strings.Contains(stderr, "as svc.glm @ ") || !strings.Contains(stderr, "(profile w)") {
		t.Errorf("preview must name the acting identity (W7): %q", stderr)
	}
	if !strings.Contains(stderr, "delete incident/INC0000001 (sys_id "+sysIDa+")") {
		t.Errorf("preview must name the record being destroyed: %q", stderr)
	}
	if !strings.Contains(stderr, "deleted incident/INC0000001") {
		t.Errorf("confirmation line missing: %q", stderr)
	}
	if hits["list"] != 1 || hits["delete"] != 1 {
		t.Errorf("want one resolution GET and one DELETE: %v", hits)
	}
}

// TestDeleteYesSysIDSkipsResolve pins the economy rule: --yes on a sys_id key
// sends exactly one request — the DELETE — with no read-before-delete.
func TestDeleteYesSysIDSkipsResolve(t *testing.T) {
	hits := map[string]int{}
	srv := fakeInstance(t, hits)
	writableProfile(t, srv, "w")

	_, stderr := runGlm(t, srv, "", "delete", "incident", sysIDa, "-p", "w", "--yes")
	if hits["list"] != 0 || hits["schema"] != 0 {
		t.Errorf("--yes on a sys_id must skip the resolve GET and schema fetch: %v", hits)
	}
	if hits["delete"] != 1 {
		t.Errorf("want exactly one DELETE: %v", hits)
	}
	if !strings.Contains(stderr, "deleted incident/"+sysIDa) {
		t.Errorf("confirmation must name the record by sys_id when unresolved: %q", stderr)
	}
}

// TestDeleteNotFoundExitCode: an unresolvable key is exit 5, before any delete
// is attempted (W8).
func TestDeleteNotFoundExitCode(t *testing.T) {
	hits := map[string]int{}
	srv := fakeInstance(t, hits)
	writableProfile(t, srv, "w")

	_, _, err := runGlmErr(t, srv, "", "delete", "incident", "INC9999999", "-p", "w", "--yes")
	if err == nil {
		t.Fatal("unknown key must fail")
	}
	var ec exitCoder
	if !errors.As(err, &ec) || ec.ExitCode() != exit.NotFound {
		t.Errorf("unknown key must map to exit %d, got: %v", exit.NotFound, err)
	}
	if hits["delete"] != 0 {
		t.Errorf("nothing may be deleted for an unresolved key: %v", hits)
	}
}

// TestDeleteDryRun: full preview, exit 0, nothing sent, nothing audited.
func TestDeleteDryRun(t *testing.T) {
	hits := map[string]int{}
	srv := fakeInstance(t, hits)
	writableProfile(t, srv, "w")
	logPath := filepath.Join(t.TempDir(), "audit.jsonl")
	t.Setenv(audit.EnvLogPath, logPath)
	t.Setenv("GLM_TEST_AUDIT_OWNER", t.Name())

	_, stderr := runGlm(t, srv, "", "delete", "incident", sysIDa, "-p", "w", "--dry-run")
	if !strings.Contains(stderr, "DELETE ") || !strings.Contains(stderr, "dry run") {
		t.Errorf("dry run must show the full preview: %q", stderr)
	}
	if hits["delete"] != 0 {
		t.Errorf("dry run must not DELETE: %v", hits)
	}
	if _, err := os.Stat(logPath); !os.IsNotExist(err) {
		t.Errorf("dry run must not append to the audit log (stat err: %v)", err)
	}
}

// TestDeleteNonInteractiveNeedsYes: gate 2 (W5) — without --yes and without a
// TTY the delete refuses AFTER previewing (the typed-confirm prompt only
// applies on a real terminal).
func TestDeleteNonInteractiveNeedsYes(t *testing.T) {
	hits := map[string]int{}
	srv := fakeInstance(t, hits)
	writableProfile(t, srv, "w")

	_, stderr, err := runGlmErr(t, srv, "", "delete", "incident", sysIDa, "-p", "w")
	if err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("non-interactive delete without --yes must refuse, got: %v", err)
	}
	if !strings.Contains(stderr, "DELETE ") {
		t.Errorf("the full preview must print before refusing: %q", stderr)
	}
	if hits["delete"] != 0 {
		t.Errorf("refused delete must not send the DELETE: %v", hits)
	}
}

// TestDeleteAuditTrail: a sent delete appends one entry with no field names
// (W6) — a delete sets nothing.
func TestDeleteAuditTrail(t *testing.T) {
	hits := map[string]int{}
	srv := fakeInstance(t, hits)
	writableProfile(t, srv, "w")
	logPath := filepath.Join(t.TempDir(), "audit.jsonl")
	t.Setenv(audit.EnvLogPath, logPath)
	t.Setenv("GLM_TEST_AUDIT_OWNER", t.Name())

	runGlm(t, srv, "", "delete", "incident", sysIDa, "-p", "w", "--yes")

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("audit log not written: %v", err)
	}
	var e audit.Entry
	if err := json.Unmarshal(data, &e); err != nil {
		t.Fatalf("audit line is not valid JSON: %v", err)
	}
	if e.Command != "delete" || e.Method != "DELETE" || e.Profile != "w" || e.User != "svc.glm" || e.Result != "ok" {
		t.Errorf("audit entry mismatch: %+v", e)
	}
	if e.Target != "/api/now/table/incident/"+sysIDa {
		t.Errorf("audit target must be the record path: %q", e.Target)
	}
	if len(e.Fields) != 0 {
		t.Errorf("a delete must record no field names: %v", e.Fields)
	}

	// --no-audit skips the trail for one call.
	runGlm(t, srv, "", "delete", "incident", sysIDa, "-p", "w", "--yes", "--no-audit")
	data, _ = os.ReadFile(logPath)
	if got := len(strings.Split(strings.TrimSpace(string(data)), "\n")); got != 1 {
		t.Errorf("--no-audit must not append, have %d entries", got)
	}
}

// TestMatchesRecordKey pins the typed-confirm logic directly (confirmDelete's
// TTY check is unreachable under `go test`): only the exact number or sys_id
// confirms, an empty line never does, and an empty number is not matchable.
func TestMatchesRecordKey(t *testing.T) {
	cases := []struct {
		typed, number, sysID string
		want                 bool
	}{
		{"INC0000001", "INC0000001", "abc", true},
		{"abc", "INC0000001", "abc", true},
		{"", "INC0000001", "abc", false},
		{"INC0000002", "INC0000001", "abc", false},
		{"", "", "abc", false},   // empty typed must not match an empty number
		{"abc", "", "abc", true}, // numberless record: sys_id still confirms
	}
	for _, c := range cases {
		if got := matchesRecordKey(c.typed, c.number, c.sysID); got != c.want {
			t.Errorf("matchesRecordKey(%q,%q,%q) = %v, want %v", c.typed, c.number, c.sysID, got, c.want)
		}
	}
}
