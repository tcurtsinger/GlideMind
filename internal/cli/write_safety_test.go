package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tcurtsinger/GlideMind/internal/audit"
	"github.com/tcurtsinger/GlideMind/internal/config"
	"github.com/tcurtsinger/GlideMind/internal/secret"
)

// TestWriteGateReadOnlyProfile pins DESIGN-WRITES.md W1 gate 1: a profile
// writes nothing until write-enabled, even with --yes, and the refusal names
// the fix. Reads on the same profile are untouched.
func TestWriteGateReadOnlyProfile(t *testing.T) {
	hits := map[string]int{}
	srv := fakeInstance(t, hits)
	isolateConfig(t)
	f := &config.File{Profiles: map[string]config.Profile{
		"ro": {Instance: srv.URL, Auth: "basic", Username: "svc.glm"},
	}}
	if err := f.Save(); err != nil {
		t.Fatalf("save config: %v", err)
	}

	_, stderr, err := runGlmErr(t, srv, "", "api", "DELETE", "/api/now/table/incident/"+sysIDa, "-p", "ro", "--yes")
	if err == nil || !strings.Contains(err.Error(), "read-only") || !strings.Contains(err.Error(), "glm profile write-enable ro") {
		t.Fatalf("read-only profile must refuse writes and name the fix, got: %v", err)
	}
	// Gate 1 fires before the confirm flow: no preview for a profile that
	// cannot write at all.
	if strings.Contains(stderr, "DELETE ") {
		t.Errorf("no preview should print before the profile gate: %q", stderr)
	}
	if hits["get"] != 0 {
		t.Errorf("refused write must never reach the instance: %v", hits)
	}

	// The gate also fires before the body is touched: a missing @file (or a
	// blocking @- stdin) must never be read for a write that could not run.
	if _, _, err := runGlmErr(t, srv, "", "api", "POST", "/api/x", "--body", "@no-such-file.json", "-p", "ro", "--yes"); err == nil || !strings.Contains(err.Error(), "read-only") {
		t.Errorf("gate must refuse before reading the body, got: %v", err)
	}

	// Reads are unaffected by the write gate.
	if _, _, err := runGlmErr(t, srv, "", "api", "GET", "/api/now/table/incident/"+sysIDa, "-p", "ro"); err != nil {
		t.Errorf("reads must work on a read-only profile: %v", err)
	}
}

// TestWriteGateFiresWithoutCredentials: the W1 refusal must not depend on a
// credential existing — a read-only profile with no keyring entry and no
// GLM_PASSWORD still gets the one-line remedy, not a credential-lookup
// error. (Passing also proves the gate runs before the client is built:
// a credential lookup for this profile would fail.)
func TestWriteGateFiresWithoutCredentials(t *testing.T) {
	hits := map[string]int{}
	srv := fakeInstance(t, hits)
	pointConfigAt(t) // clears ambient GLM_* env, including any password path
	t.Setenv(secret.EnvPassword, "")
	f := &config.File{Profiles: map[string]config.Profile{
		"ro": {Instance: srv.URL, Auth: "basic", Username: "svc.glm"},
	}}
	if err := f.Save(); err != nil {
		t.Fatalf("save config: %v", err)
	}

	var out, errOut bytes.Buffer
	root := newRootCmd()
	root.SetOut(&out)
	root.SetErr(&errOut)
	root.SetArgs([]string{"api", "DELETE", "/api/now/table/incident/" + sysIDa, "-p", "ro", "--yes"})
	err := root.Execute()
	if err == nil || !strings.Contains(err.Error(), "read-only") {
		t.Fatalf("gate must refuse before credential lookup, got: %v", err)
	}
}

// TestWriteGateEnvProfileAlwaysReadOnly: the GLM_INSTANCE env profile has no
// stored writable property, so it is read-only, period — env-only write
// access would be the invisible-state gate W1 rejects.
func TestWriteGateEnvProfileAlwaysReadOnly(t *testing.T) {
	hits := map[string]int{}
	srv := fakeInstance(t, hits)

	_, _, err := runGlmErr(t, srv, "", "api", "POST", "/api/now/table/incident", "--body", `{"a":"b"}`, "--yes")
	if err == nil || !strings.Contains(err.Error(), "read-only") {
		t.Fatalf("env profile must be read-only, got: %v", err)
	}
	if hits["get"] != 0 {
		t.Errorf("refused write must never reach the instance: %v", hits)
	}
}

// TestWriteAuditTrail pins DESIGN-WRITES.md W6: a confirmed write appends one
// JSONL entry with identity, method, target, and field NAMES — never values.
func TestWriteAuditTrail(t *testing.T) {
	hits := map[string]int{}
	srv := fakeInstance(t, hits)
	writableProfile(t, srv, "w")
	logPath := filepath.Join(t.TempDir(), "audit.jsonl")
	t.Setenv(audit.EnvLogPath, logPath)
	t.Setenv("GLM_TEST_AUDIT_OWNER", t.Name()) // keep runGlm from re-pointing it

	runGlm(t, srv, "", "api", "DELETE", "/api/now/table/incident/"+sysIDa, "-p", "w", "--yes")
	// Query values ride in both the embedded path query and -f — the audit
	// must keep their NAMES only (encoded queries can carry record data).
	runGlm(t, srv, "", "api", "PATCH", "/api/now/table/incident/"+sysIDa+"?sysparm_query=numberLIKEsecret-query-value", "-p", "w", "--yes",
		"-f", "sysparm_fields=state",
		"--body", `{"state":"6","close_notes":"resolved by glm"}`)

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("audit log not written: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 {
		t.Fatalf("want 2 audit entries, got %d:\n%s", len(lines), data)
	}

	var e audit.Entry
	if err := json.Unmarshal([]byte(lines[1]), &e); err != nil {
		t.Fatalf("audit line is not valid JSON: %v", err)
	}
	if e.Method != "PATCH" || e.Command != "api" || e.Profile != "w" || e.User != "svc.glm" || e.Result != "ok" {
		t.Errorf("audit entry mismatch: %+v", e)
	}
	if e.Target != "/api/now/table/incident/"+sysIDa {
		t.Errorf("audit target must be the query-stripped path: %q", e.Target)
	}
	if len(e.Params) != 2 || e.Params[0] != "sysparm_fields" || e.Params[1] != "sysparm_query" {
		t.Errorf("audit must record sorted query parameter names: %v", e.Params)
	}
	if len(e.Fields) != 2 || e.Fields[0] != "close_notes" || e.Fields[1] != "state" {
		t.Errorf("audit must record sorted field names: %v", e.Fields)
	}
	// Names only — no body or query values at rest in the local log.
	for _, leak := range []string{"resolved by glm", "secret-query-value"} {
		if strings.Contains(string(data), leak) {
			t.Errorf("audit log must not contain value %q:\n%s", leak, data)
		}
	}

	// GETs never audit.
	runGlm(t, srv, "", "api", "GET", "/api/now/table/incident/"+sysIDa, "-p", "w")
	data, _ = os.ReadFile(logPath)
	if got := len(strings.Split(strings.TrimSpace(string(data)), "\n")); got != 2 {
		t.Errorf("GET must not append audit entries, have %d", got)
	}
}

// TestWriteAuditOptOut: --no-audit skips the trail for one call.
func TestWriteAuditOptOut(t *testing.T) {
	hits := map[string]int{}
	srv := fakeInstance(t, hits)
	writableProfile(t, srv, "w")
	logPath := filepath.Join(t.TempDir(), "audit.jsonl")
	t.Setenv(audit.EnvLogPath, logPath)
	t.Setenv("GLM_TEST_AUDIT_OWNER", t.Name())

	runGlm(t, srv, "", "api", "DELETE", "/api/now/table/incident/"+sysIDa, "-p", "w", "--yes", "--no-audit")
	if _, err := os.Stat(logPath); !os.IsNotExist(err) {
		t.Errorf("--no-audit must not write the log (stat err: %v)", err)
	}
}

// TestPreserveWritable pins the update semantics of `profile add`: a new
// profile takes the flag; an update (password rotation, username change)
// keeps the stored value unless --writable was explicitly passed — a
// deliberately write-enabled profile must not silently lose its gate.
func TestPreserveWritable(t *testing.T) {
	cases := []struct {
		name                          string
		oldWritable                   bool
		existed, flagChanged, flagVal bool
		want                          bool
	}{
		{"new profile default ro", false, false, false, false, false},
		{"new profile --writable", false, false, true, true, true},
		{"update keeps rw", true, true, false, false, true},
		{"update keeps ro", false, true, false, false, false},
		{"update explicit --writable", false, true, true, true, true},
		{"update explicit --writable=false", true, true, true, false, false},
	}
	for _, c := range cases {
		got := preserveWritable(config.Profile{Writable: c.oldWritable}, c.existed, c.flagChanged, c.flagVal)
		if got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

// TestProfileWritableLifecycle: the write-enable/write-disable commands flip
// the stored flag, and profile list surfaces it as rw/ro.
func TestProfileWritableLifecycle(t *testing.T) {
	hits := map[string]int{}
	srv := fakeInstance(t, hits)
	isolateConfig(t)
	f := &config.File{Profiles: map[string]config.Profile{
		"dev": {Instance: srv.URL, Auth: "basic", Username: "svc.glm"},
	}}
	if err := f.Save(); err != nil {
		t.Fatalf("save config: %v", err)
	}

	if _, _, err := runGlmErr(t, srv, "", "profile", "write-enable", "nope"); err == nil {
		t.Error("write-enable on an unknown profile must fail")
	}

	runGlm(t, srv, "", "profile", "write-enable", "dev")
	got, err := config.Load()
	if err != nil || !got.Profiles["dev"].Writable {
		t.Fatalf("write-enable did not persist: %+v, %v", got, err)
	}
	stdout, _ := runGlm(t, srv, "", "profile", "list")
	if !strings.Contains(stdout, "\trw") {
		t.Errorf("profile list must mark writable profiles: %q", stdout)
	}

	runGlm(t, srv, "", "profile", "write-disable", "dev")
	got, err = config.Load()
	if err != nil || got.Profiles["dev"].Writable {
		t.Fatalf("write-disable did not persist: %+v, %v", got, err)
	}
	stdout, _ = runGlm(t, srv, "", "profile", "list")
	if !strings.Contains(stdout, "\tro") {
		t.Errorf("profile list must mark read-only profiles: %q", stdout)
	}
}
