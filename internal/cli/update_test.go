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

// TestUpdateGateReadOnlyProfile: gate 1 (W1) fires before schema fetches,
// credentials, and any GET — on both a named read-only profile and the
// always-read-only env profile.
func TestUpdateGateReadOnlyProfile(t *testing.T) {
	hits := map[string]int{}
	srv := fakeInstance(t, hits)

	// Env profile (runGlm's default selection): read-only, period.
	_, _, err := runGlmErr(t, srv, "", "update", "incident", sysIDa, "-f", "state=6", "--yes")
	if err == nil || !strings.Contains(err.Error(), "read-only") || !strings.Contains(err.Error(), "profile add") {
		t.Fatalf("env profile must refuse update with the profile-add remedy, got: %v", err)
	}

	// Named read-only profile: refusal names write-enable.
	isolateConfig(t)
	f := &config.File{Profiles: map[string]config.Profile{
		"ro": {Instance: srv.URL, Auth: "basic", Username: "svc.glm"},
	}}
	if err := f.Save(); err != nil {
		t.Fatalf("save config: %v", err)
	}
	// A bogus field proves ordering: the gate answers before validation
	// would — a profile that can never write gets the refusal, not a
	// schema fetch on its behalf.
	_, _, err = runGlmErr(t, srv, "", "update", "incident", sysIDa, "-f", "bogus=1", "-p", "ro", "--yes")
	if err == nil || !strings.Contains(err.Error(), "glm profile write-enable ro") {
		t.Fatalf("read-only profile must refuse update naming the fix, got: %v", err)
	}
	if hits["schema"] != 0 || hits["get"] != 0 || hits["list"] != 0 {
		t.Errorf("refused update must never reach the instance: %v", hits)
	}
}

// TestUpdateFieldArgErrors: malformed -f input dies before anything runs.
func TestUpdateFieldArgErrors(t *testing.T) {
	hits := map[string]int{}
	srv := fakeInstance(t, hits)

	cases := []struct {
		args []string
		want string
	}{
		{[]string{"update", "incident", sysIDa, "--yes"}, "nothing to set"},
		{[]string{"update", "incident", sysIDa, "-f", "noequals", "--yes"}, "field=value"},
		{[]string{"update", "incident", sysIDa, "-f", "=6", "--yes"}, "field=value"},
		{[]string{"update", "incident", sysIDa, "-f", "state=1", "-f", "state=2", "--yes"}, "set twice"},
		{[]string{"update", "incident", sysIDa, "-f", "assigned_to.name=x", "--yes"}, "dot-walked"},
		{[]string{"update", "incident", sysIDa, "-f", "state=6", "--diff", "--no-diff", "--yes"}, ""},
	}
	for _, c := range cases {
		_, _, err := runGlmErr(t, srv, "", c.args...)
		if err == nil || (c.want != "" && !strings.Contains(err.Error(), c.want)) {
			t.Errorf("%v: want error containing %q, got: %v", c.args, c.want, err)
		}
	}
	if hits["get"] != 0 || hits["patch"] != 0 {
		t.Errorf("rejected input must never reach the instance: %v", hits)
	}
}

// TestUpdateStrictFieldValidation pins W3: unlike reads (which skip
// validation on a cold cache), a write FETCHES the schema and hard-fails on
// an unknown field — ServiceNow would silently ignore it, losing the change.
func TestUpdateStrictFieldValidation(t *testing.T) {
	hits := map[string]int{}
	srv := fakeInstance(t, hits)
	writableProfile(t, srv, "w")

	_, _, err := runGlmErr(t, srv, "", "update", "incident", sysIDa, "-f", "priorty=1", "-p", "w", "--yes")
	if err == nil || !strings.Contains(err.Error(), `"priority"`) {
		t.Fatalf("cold-cache write must still catch the typo with a suggestion, got: %v", err)
	}
	if hits["schema"] == 0 {
		t.Error("write validation must fetch the schema on a cold cache")
	}
	if hits["patch"] != 0 || hits["get"] != 0 {
		t.Errorf("a typo'd field must never reach the record endpoint: %v", hits)
	}
}

// TestUpdateStrictSysFieldTypo pins the write path's strictness on sys_*
// names (DESIGN-WRITES.md W3): the read-path bypass that blanket-accepts any
// sys_-prefixed name would let a typo like "sys_update_on" (for
// "sys_updated_on") through, and ServiceNow silently drops it on the PATCH —
// silent data loss. The write validator must catch it, suggesting the real
// field, before the record endpoint is touched.
func TestUpdateStrictSysFieldTypo(t *testing.T) {
	hits := map[string]int{}
	srv := fakeInstance(t, hits)
	writableProfile(t, srv, "w")

	_, _, err := runGlmErr(t, srv, "", "update", "incident", sysIDa, "-f", "sys_update_on=x", "-p", "w", "--yes")
	if err == nil || !strings.Contains(err.Error(), `"sys_updated_on"`) {
		t.Fatalf("a sys_ typo must be caught with a suggestion, got: %v", err)
	}
	if hits["schema"] == 0 {
		t.Error("write validation must fetch the schema to check the sys_ name")
	}
	if hits["patch"] != 0 || hits["get"] != 0 {
		t.Errorf("a typo'd sys_ field must never reach the record endpoint: %v", hits)
	}
}

// TestUpdateValidationSelfHeals: a field newer than the cached schema
// triggers one refetch instead of a false hard error (W3 keeps read
// validation's self-heal).
func TestUpdateValidationSelfHeals(t *testing.T) {
	hits := map[string]int{}
	srv := fakeInstance(t, hits)
	writableProfile(t, srv, "w")

	runGlm(t, srv, "", "schema", "evolving") // warm the cache pre-"tier"
	if hits["evolving-dict"] != 1 {
		t.Fatalf("expected 1 dictionary fetch after warming, got %v", hits)
	}

	_, stderr := runGlm(t, srv, "", "update", "evolving", sysIDa, "-f", "tier=3", "-p", "w", "--no-diff", "--dry-run")
	if hits["evolving-dict"] != 2 {
		t.Errorf("stale-cache miss must refetch exactly once, got %v", hits)
	}
	if !strings.Contains(stderr, "tier = 3") || !strings.Contains(stderr, "dry run") {
		t.Errorf("dry-run preview missing: %q", stderr)
	}
}

// TestUpdateStrictRefetchesStaleCache pins the other staleness direction
// (DESIGN-WRITES.md W3): a warm cache can still list a field removed or
// renamed on the instance since it was written. A write must NOT trust that
// cached pass — it refetches and hard-fails, or ServiceNow would silently
// ignore the now-unknown field and glm would report a false success.
func TestUpdateStrictRefetchesStaleCache(t *testing.T) {
	hits := map[string]int{}
	srv := fakeInstance(t, hits)
	writableProfile(t, srv, "w")

	runGlm(t, srv, "", "schema", "evolving") // warm: caches "legacy" as valid
	if hits["evolving-dict"] != 1 {
		t.Fatalf("expected 1 dictionary fetch after warming, got %v", hits)
	}

	_, _, err := runGlmErr(t, srv, "", "update", "evolving", sysIDa, "-f", "legacy=x", "-p", "w", "--yes")
	if err == nil || !strings.Contains(err.Error(), "unknown field") || !strings.Contains(err.Error(), "legacy") {
		t.Fatalf("a field removed since the cache was written must be refused on write, got: %v", err)
	}
	if hits["evolving-dict"] != 2 {
		t.Errorf("write must refetch to settle a cached pass, got %v", hits)
	}
	if hits["patch"] != 0 || hits["get"] != 0 {
		t.Errorf("an unknown field must never reach the record endpoint: %v", hits)
	}
}

// TestUpdateDiffPreviewAndSend: the flagship W4 flow — key resolution via
// get's resolver, a field-level old → new diff, identity in the preview
// (W7), then exactly one PATCH carrying exactly the requested change.
func TestUpdateDiffPreviewAndSend(t *testing.T) {
	hits := map[string]int{}
	srv := fakeInstance(t, hits)
	writableProfile(t, srv, "w")

	_, stderr := runGlm(t, srv, "", "update", "incident", "INC0000001", "-f", "state=6", "-p", "w", "--yes", "--diff")
	if !strings.Contains(stderr, "PATCH "+srv.URL+"/api/now/table/incident/"+sysIDa) {
		t.Errorf("preview must show the exact PATCH target: %q", stderr)
	}
	if !strings.Contains(stderr, "as svc.glm @ ") || !strings.Contains(stderr, "(profile w)") {
		t.Errorf("preview must name the acting identity (W7): %q", stderr)
	}
	if !strings.Contains(stderr, "state: In Progress → 6") {
		t.Errorf("field-level diff missing: %q", stderr)
	}
	if !strings.Contains(stderr, "updated incident/INC0000001 (state)") {
		t.Errorf("confirmation line missing: %q", stderr)
	}
	if hits["list"] != 1 || hits["patch"] != 1 || hits["set-state=6"] != 1 {
		t.Errorf("want one resolution GET and one PATCH with state=6: %v", hits)
	}

	// A no-op change is visible before it is approved.
	_, stderr = runGlm(t, srv, "", "update", "incident", sysIDa, "-f", "state=In Progress", "-p", "w", "--yes", "--diff")
	if !strings.Contains(stderr, "(unchanged)") {
		t.Errorf("no-op diff should be flagged: %q", stderr)
	}
}

// TestUpdateDiffMarksUnreadableField pins that the diff never fabricates an
// old value (Codex review, W4): when the pre-write read does not return a
// requested field — a read ACL can hide a field the caller may still write —
// the preview marks it "(unreadable)" instead of rendering "" as the old
// value, which would fabricate record state and could mislabel a clear as
// unchanged. "secret_field" is in the dictionary (so it validates) but the
// incident record fixture never returns it.
func TestUpdateDiffMarksUnreadableField(t *testing.T) {
	hits := map[string]int{}
	srv := fakeInstance(t, hits)
	writableProfile(t, srv, "w")

	_, stderr := runGlm(t, srv, "", "update", "incident", sysIDa, "-f", "secret_field=classified", "-p", "w", "--diff", "--dry-run")
	if !strings.Contains(stderr, "secret_field: (unreadable) → classified") {
		t.Errorf("an absent read field must render as unreadable, not a fabricated empty old value: %q", stderr)
	}
	if strings.Contains(stderr, "(unchanged)") {
		t.Errorf("an unreadable field must never be labelled unchanged: %q", stderr)
	}
	// A clear (empty new value) of an unreadable field must also not claim
	// "unchanged" — the whole bug is that a fabricated empty old == empty new.
	_, stderr = runGlm(t, srv, "", "update", "incident", sysIDa, "-f", "secret_field=", "-p", "w", "--diff", "--dry-run")
	if !strings.Contains(stderr, "secret_field: (unreadable) → ") || strings.Contains(stderr, "(unchanged)") {
		t.Errorf("clearing an unreadable field must not be mislabelled unchanged: %q", stderr)
	}
}

// TestUpdateYesSkipsDiffGet pins the W4 economy rule: --yes on a sys_id key
// sends exactly one request — the PATCH — with no read-before-write.
func TestUpdateYesSkipsDiffGet(t *testing.T) {
	hits := map[string]int{}
	srv := fakeInstance(t, hits)
	writableProfile(t, srv, "w")

	_, stderr := runGlm(t, srv, "", "update", "incident", sysIDa, "-f", "state=6", "-p", "w", "--yes")
	if hits["get"] != 1 || hits["patch"] != 1 || hits["list"] != 0 {
		t.Errorf("--yes must skip the diff GET (want the PATCH as the only request): %v", hits)
	}
	if strings.Contains(stderr, "→") {
		t.Errorf("no diff should render without a read: %q", stderr)
	}
	if !strings.Contains(stderr, "state = 6") {
		t.Errorf("preview must still list the fields being set: %q", stderr)
	}
}

// TestUpdateNonInteractiveNeedsYes: gate 2 (W5) — without --yes and without
// a TTY the update refuses AFTER previewing (so an agent that forgot --yes
// still sees exactly what would happen, diff included).
func TestUpdateNonInteractiveNeedsYes(t *testing.T) {
	hits := map[string]int{}
	srv := fakeInstance(t, hits)
	writableProfile(t, srv, "w")

	_, stderr, err := runGlmErr(t, srv, "", "update", "incident", sysIDa, "-f", "state=6", "-p", "w")
	if err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("non-interactive update without --yes must refuse, got: %v", err)
	}
	if !strings.Contains(stderr, "PATCH ") || !strings.Contains(stderr, "state: In Progress → 6") {
		t.Errorf("the full preview (diff on by default) must print before refusing: %q", stderr)
	}
	if hits["patch"] != 0 {
		t.Errorf("refused update must not send the PATCH: %v", hits)
	}
}

// TestUpdateDryRun: full preview, exit 0, nothing sent, nothing audited.
func TestUpdateDryRun(t *testing.T) {
	hits := map[string]int{}
	srv := fakeInstance(t, hits)
	writableProfile(t, srv, "w")
	logPath := filepath.Join(t.TempDir(), "audit.jsonl")
	t.Setenv(audit.EnvLogPath, logPath)
	t.Setenv("GLM_TEST_AUDIT_OWNER", t.Name())

	_, stderr := runGlm(t, srv, "", "update", "incident", "INC0000001", "-f", "state=6", "-p", "w", "--dry-run")
	if !strings.Contains(stderr, "state: In Progress → 6") || !strings.Contains(stderr, "dry run") {
		t.Errorf("dry run must show the full diff preview: %q", stderr)
	}
	if hits["patch"] != 0 || hits["get"] != 0 {
		t.Errorf("dry run must not touch the record endpoint: %v", hits)
	}
	if _, err := os.Stat(logPath); !os.IsNotExist(err) {
		t.Errorf("dry run must not append to the audit log (stat err: %v)", err)
	}
}

// TestUpdateNotFoundExitCode: an unresolvable key is exit 5, before any
// write is attempted (W8).
func TestUpdateNotFoundExitCode(t *testing.T) {
	hits := map[string]int{}
	srv := fakeInstance(t, hits)
	writableProfile(t, srv, "w")

	_, _, err := runGlmErr(t, srv, "", "update", "incident", "INC9999999", "-f", "state=6", "-p", "w", "--yes", "--diff")
	if err == nil {
		t.Fatal("unknown key must fail")
	}
	var ec exitCoder
	if !errors.As(err, &ec) || ec.ExitCode() != exit.NotFound {
		t.Errorf("unknown key must map to exit %d, got: %v", exit.NotFound, err)
	}
	if hits["patch"] != 0 {
		t.Errorf("nothing may be written for an unresolved key: %v", hits)
	}
}

// TestUpdateAuditTrail: a sent update appends one names-only entry (W6).
func TestUpdateAuditTrail(t *testing.T) {
	hits := map[string]int{}
	srv := fakeInstance(t, hits)
	writableProfile(t, srv, "w")
	logPath := filepath.Join(t.TempDir(), "audit.jsonl")
	t.Setenv(audit.EnvLogPath, logPath)
	t.Setenv("GLM_TEST_AUDIT_OWNER", t.Name())

	runGlm(t, srv, "", "update", "incident", sysIDa, "-f", "state=6", "-f", "description=resolved by glm", "-p", "w", "--yes")

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("audit log not written: %v", err)
	}
	var e audit.Entry
	if err := json.Unmarshal(data, &e); err != nil {
		t.Fatalf("audit line is not valid JSON: %v", err)
	}
	if e.Command != "update" || e.Method != "PATCH" || e.Profile != "w" || e.User != "svc.glm" || e.Result != "ok" {
		t.Errorf("audit entry mismatch: %+v", e)
	}
	if e.Target != "/api/now/table/incident/"+sysIDa {
		t.Errorf("audit target must be the record path: %q", e.Target)
	}
	if len(e.Fields) != 2 || e.Fields[0] != "description" || e.Fields[1] != "state" {
		t.Errorf("audit must record sorted field names: %v", e.Fields)
	}
	if strings.Contains(string(data), "resolved by glm") {
		t.Errorf("audit log must never contain field values:\n%s", data)
	}

	// --no-audit skips the trail for one call.
	runGlm(t, srv, "", "update", "incident", sysIDa, "-f", "state=7", "-p", "w", "--yes", "--no-audit")
	data, _ = os.ReadFile(logPath)
	if got := len(strings.Split(strings.TrimSpace(string(data)), "\n")); got != 1 {
		t.Errorf("--no-audit must not append, have %d entries", got)
	}
}
