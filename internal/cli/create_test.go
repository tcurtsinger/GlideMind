package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tcurtsinger/GlideMind/internal/audit"
	"github.com/tcurtsinger/GlideMind/internal/config"
)

// TestCreateGateReadOnlyProfile: gate 1 (W1) fires before schema fetches,
// credentials, and the POST — on both a named read-only profile and the
// always-read-only env profile.
func TestCreateGateReadOnlyProfile(t *testing.T) {
	hits := map[string]int{}
	srv := fakeInstance(t, hits)

	_, _, err := runGlmErr(t, srv, "", "create", "incident", "-f", "short_description=x", "--yes")
	if err == nil || !strings.Contains(err.Error(), "read-only") || !strings.Contains(err.Error(), "profile add") {
		t.Fatalf("env profile must refuse create with the profile-add remedy, got: %v", err)
	}

	isolateConfig(t)
	f := &config.File{Profiles: map[string]config.Profile{
		"ro": {Instance: srv.URL, Auth: "basic", Username: "svc.glm"},
	}}
	if err := f.Save(); err != nil {
		t.Fatalf("save config: %v", err)
	}
	// A bogus field proves ordering: the gate answers before validation would.
	_, _, err = runGlmErr(t, srv, "", "create", "incident", "-f", "bogus=1", "-p", "ro", "--yes")
	if err == nil || !strings.Contains(err.Error(), "glm profile write-enable ro") {
		t.Fatalf("read-only profile must refuse create naming the fix, got: %v", err)
	}
	if hits["schema"] != 0 || hits["post"] != 0 {
		t.Errorf("refused create must never reach the instance: %v", hits)
	}
}

// TestCreateFieldArgErrors: create shares parseFieldArgs, so malformed -f dies
// before anything runs.
func TestCreateFieldArgErrors(t *testing.T) {
	hits := map[string]int{}
	srv := fakeInstance(t, hits)

	cases := []struct {
		args []string
		want string
	}{
		{[]string{"create", "incident", "--yes"}, "nothing to set"},
		{[]string{"create", "incident", "-f", "noequals", "--yes"}, "field=value"},
		{[]string{"create", "incident", "-f", "state=1", "-f", "state=2", "--yes"}, "set twice"},
		{[]string{"create", "incident", "-f", "assigned_to.name=x", "--yes"}, "dot-walked"},
	}
	for _, c := range cases {
		_, _, err := runGlmErr(t, srv, "", c.args...)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%v: want error containing %q, got: %v", c.args, c.want, err)
		}
	}
	if hits["post"] != 0 {
		t.Errorf("rejected input must never reach the instance: %v", hits)
	}
}

// TestCreateStrictFieldValidation pins W3 on create: a cold cache FETCHES the
// schema and an unknown field hard-fails before the POST.
func TestCreateStrictFieldValidation(t *testing.T) {
	hits := map[string]int{}
	srv := fakeInstance(t, hits)
	writableProfile(t, srv, "w")

	_, _, err := runGlmErr(t, srv, "", "create", "incident", "-f", "priorty=1", "-p", "w", "--yes")
	if err == nil || !strings.Contains(err.Error(), `"priority"`) {
		t.Fatalf("cold-cache create must catch the typo with a suggestion, got: %v", err)
	}
	if hits["schema"] == 0 {
		t.Error("write validation must fetch the schema on a cold cache")
	}
	if hits["post"] != 0 {
		t.Errorf("a typo'd field must never reach the POST: %v", hits)
	}
}

// TestCreateSendsPost: the flagship flow — preview (POST URL + identity +
// fields), exactly one POST carrying exactly the requested fields, and the
// new record's number reported back from the echoed response.
func TestCreateSendsPost(t *testing.T) {
	hits := map[string]int{}
	srv := fakeInstance(t, hits)
	writableProfile(t, srv, "w")

	_, stderr := runGlm(t, srv, "", "create", "incident", "-f", "short_description=New box", "-f", "priority=2", "-p", "w", "--yes")
	if !strings.Contains(stderr, "POST "+srv.URL+"/api/now/table/incident") {
		t.Errorf("preview must show the exact POST target: %q", stderr)
	}
	if !strings.Contains(stderr, "as svc.glm @ ") || !strings.Contains(stderr, "(profile w)") {
		t.Errorf("preview must name the acting identity (W7): %q", stderr)
	}
	if !strings.Contains(stderr, "priority = 2") || !strings.Contains(stderr, "short_description = New box") {
		t.Errorf("preview must list the fields being set: %q", stderr)
	}
	if !strings.Contains(stderr, "created incident/INC0000042 (priority, short_description)") {
		t.Errorf("confirmation must name the new record and fields: %q", stderr)
	}
	if hits["post"] != 1 || hits["set-priority=2"] != 1 || hits["set-short_description=New box"] != 1 {
		t.Errorf("want one POST carrying both fields: %v", hits)
	}
}

// TestCreateDryRun: full preview, exit 0, nothing sent, nothing audited.
func TestCreateDryRun(t *testing.T) {
	hits := map[string]int{}
	srv := fakeInstance(t, hits)
	writableProfile(t, srv, "w")
	logPath := filepath.Join(t.TempDir(), "audit.jsonl")
	t.Setenv(audit.EnvLogPath, logPath)
	t.Setenv("GLM_TEST_AUDIT_OWNER", t.Name())

	_, stderr := runGlm(t, srv, "", "create", "incident", "-f", "short_description=x", "-p", "w", "--dry-run")
	if !strings.Contains(stderr, "short_description = x") || !strings.Contains(stderr, "dry run") {
		t.Errorf("dry run must show the full preview: %q", stderr)
	}
	if hits["post"] != 0 {
		t.Errorf("dry run must not POST: %v", hits)
	}
	if _, err := os.Stat(logPath); !os.IsNotExist(err) {
		t.Errorf("dry run must not append to the audit log (stat err: %v)", err)
	}
}

// TestCreateNonInteractiveNeedsYes: gate 2 (W5) — without --yes and without a
// TTY the create refuses AFTER previewing.
func TestCreateNonInteractiveNeedsYes(t *testing.T) {
	hits := map[string]int{}
	srv := fakeInstance(t, hits)
	writableProfile(t, srv, "w")

	_, stderr, err := runGlmErr(t, srv, "", "create", "incident", "-f", "short_description=x", "-p", "w")
	if err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("non-interactive create without --yes must refuse, got: %v", err)
	}
	if !strings.Contains(stderr, "POST ") || !strings.Contains(stderr, "short_description = x") {
		t.Errorf("the full preview must print before refusing: %q", stderr)
	}
	if hits["post"] != 0 {
		t.Errorf("refused create must not POST: %v", hits)
	}
}

// TestCreateAuditTrail: a sent create appends one names-only entry (W6).
func TestCreateAuditTrail(t *testing.T) {
	hits := map[string]int{}
	srv := fakeInstance(t, hits)
	writableProfile(t, srv, "w")
	logPath := filepath.Join(t.TempDir(), "audit.jsonl")
	t.Setenv(audit.EnvLogPath, logPath)
	t.Setenv("GLM_TEST_AUDIT_OWNER", t.Name())

	runGlm(t, srv, "", "create", "incident", "-f", "short_description=secret text", "-f", "priority=2", "-p", "w", "--yes")

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("audit log not written: %v", err)
	}
	var e audit.Entry
	if err := json.Unmarshal(data, &e); err != nil {
		t.Fatalf("audit line is not valid JSON: %v", err)
	}
	if e.Command != "create" || e.Method != "POST" || e.Profile != "w" || e.User != "svc.glm" || e.Result != "ok" {
		t.Errorf("audit entry mismatch: %+v", e)
	}
	if e.Target != "/api/now/table/incident" {
		t.Errorf("audit target must be the collection path: %q", e.Target)
	}
	if len(e.Fields) != 2 || e.Fields[0] != "priority" || e.Fields[1] != "short_description" {
		t.Errorf("audit must record sorted field names: %v", e.Fields)
	}
	if strings.Contains(string(data), "secret text") {
		t.Errorf("audit log must never contain field values:\n%s", data)
	}
}
