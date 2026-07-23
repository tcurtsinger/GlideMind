package cli

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tcurtsinger/GlideMind/internal/config"
	"github.com/tcurtsinger/GlideMind/internal/exit"
)

// diffFake is one instance's responses for a diff test.
type diffFake struct {
	dict   []map[string]any // sys_dictionary rows; empty => table absent
	record map[string]any   // the record; nil => record absent
}

func snResult(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"result": v}) //nolint:errcheck
}

// diffServer serves the minimum for `glm diff` against one table on one
// instance: table existence (sys_db_object), the dictionary, and the record
// by sys_id or number.
func diffServer(t *testing.T, table string, f diffFake) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/now/table/sys_db_object", func(w http.ResponseWriter, r *http.Request) {
		if len(f.dict) > 0 && strings.Contains(r.URL.Query().Get("sysparm_query"), "name="+table) {
			snResult(w, []map[string]any{{"name": table, "super_class.name": ""}})
			return
		}
		snResult(w, []map[string]any{})
	})
	mux.HandleFunc("/api/now/table/sys_dictionary", func(w http.ResponseWriter, r *http.Request) {
		snResult(w, f.dict)
	})
	mux.HandleFunc("/api/now/table/"+table+"/", func(w http.ResponseWriter, r *http.Request) {
		if f.record == nil {
			http.Error(w, `{"error":{"message":"No Record found"}}`, http.StatusNotFound)
			return
		}
		snResult(w, f.record)
	})
	mux.HandleFunc("/api/now/table/"+table, func(w http.ResponseWriter, r *http.Request) {
		if f.record == nil {
			snResult(w, []map[string]any{})
			return
		}
		snResult(w, []map[string]any{f.record})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// twoProfiles writes an isolated config with profiles "a" and "b" at the two
// servers; diff selects them with -p a -p b.
func twoProfiles(t *testing.T, srvA, srvB *httptest.Server) {
	t.Helper()
	isolateConfig(t)
	f := &config.File{Profiles: map[string]config.Profile{
		"a": {Instance: srvA.URL, Auth: "basic", Username: "svc.glm"},
		"b": {Instance: srvB.URL, Auth: "basic", Username: "svc.glm"},
	}}
	if err := f.Save(); err != nil {
		t.Fatalf("save config: %v", err)
	}
}

// TestDiffFlagCount: diff requires exactly two distinct -p flags.
func TestDiffFlagCount(t *testing.T) {
	srvA := diffServer(t, "widget", diffFake{})
	srvB := diffServer(t, "widget", diffFake{})
	twoProfiles(t, srvA, srvB)

	cases := []struct {
		args []string
		want string
	}{
		{[]string{"diff", "widget", sysIDa, "-p", "a"}, "exactly twice"},
		{[]string{"diff", "widget", sysIDa, "-p", "a", "-p", "b", "-p", "a"}, "exactly twice"},
		{[]string{"diff", "widget", sysIDa}, "exactly twice"},
		{[]string{"diff", "widget", sysIDa, "-p", "a", "-p", "a"}, "two different instances"},
		{[]string{"diff", "widget", "-p", "a", "-p", "b", "--fields", "state"}, "record diff"},
	}
	for _, c := range cases {
		_, _, err := runGlmErr(t, srvA, "", c.args...)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%v: want error containing %q, got: %v", c.args, c.want, err)
		}
	}
}

// TestDiffRecordDifferences: only differing fields are shown; equal fields and
// sys_id (the cross-instance key, not a data field) are omitted.
func TestDiffRecordDifferences(t *testing.T) {
	recA := map[string]any{"sys_id": sysIDa, "number": "WID0001", "state": "1", "short_description": "Alpha", "priority": "2"}
	recB := map[string]any{"sys_id": sysIDa, "number": "WID0001", "state": "3", "short_description": "Beta", "priority": "2"}
	srvA := diffServer(t, "widget", diffFake{record: recA})
	srvB := diffServer(t, "widget", diffFake{record: recB})
	twoProfiles(t, srvA, srvB)

	stdout, stderr := runGlm(t, srvA, "", "diff", "widget", sysIDa, "-p", "a", "-p", "b")
	if !strings.Contains(stdout, "state") || !strings.Contains(stdout, "short_description") {
		t.Errorf("differing fields must appear: %q", stdout)
	}
	if !strings.Contains(stdout, "Alpha") || !strings.Contains(stdout, "Beta") {
		t.Errorf("both sides' values must appear: %q", stdout)
	}
	if strings.Contains(stdout, "priority") {
		t.Errorf("an equal field must not appear: %q", stdout)
	}
	if strings.Contains(stdout, sysIDa) {
		t.Errorf("sys_id is the cross-instance key, not a diff field: %q", stdout)
	}
	if !strings.Contains(stderr, "2 differing field(s)") {
		t.Errorf("summary should count differences: %q", stderr)
	}
}

// TestDiffRecordFieldsNarrow: --fields limits the comparison.
func TestDiffRecordFieldsNarrow(t *testing.T) {
	recA := map[string]any{"sys_id": sysIDa, "state": "1", "short_description": "Alpha"}
	recB := map[string]any{"sys_id": sysIDa, "state": "3", "short_description": "Beta"}
	srvA := diffServer(t, "widget", diffFake{record: recA})
	srvB := diffServer(t, "widget", diffFake{record: recB})
	twoProfiles(t, srvA, srvB)

	stdout, _ := runGlm(t, srvA, "", "diff", "widget", sysIDa, "-p", "a", "-p", "b", "--fields", "state")
	if !strings.Contains(stdout, "state") {
		t.Errorf("requested field must appear: %q", stdout)
	}
	if strings.Contains(stdout, "short_description") || strings.Contains(stdout, "Alpha") {
		t.Errorf("--fields must exclude other fields: %q", stdout)
	}
}

// TestDiffRecordIdentical: no differences → an "identical" summary, exit 0, no
// table on stdout.
func TestDiffRecordIdentical(t *testing.T) {
	rec := map[string]any{"sys_id": sysIDa, "state": "1", "short_description": "Alpha"}
	srvA := diffServer(t, "widget", diffFake{record: rec})
	srvB := diffServer(t, "widget", diffFake{record: rec})
	twoProfiles(t, srvA, srvB)

	stdout, stderr := runGlm(t, srvA, "", "diff", "widget", sysIDa, "-p", "a", "-p", "b")
	if strings.TrimSpace(stdout) != "" {
		t.Errorf("identical records must print no table: %q", stdout)
	}
	if !strings.Contains(stderr, "is identical") {
		t.Errorf("summary should report identity: %q", stderr)
	}
}

// TestDiffRecordMissingOneSide: a record present on one instance only is a diff
// result, exit 0.
func TestDiffRecordMissingOneSide(t *testing.T) {
	rec := map[string]any{"sys_id": sysIDa, "state": "1"}
	srvA := diffServer(t, "widget", diffFake{record: rec})
	srvB := diffServer(t, "widget", diffFake{record: nil}) // 404 on B
	twoProfiles(t, srvA, srvB)

	stdout, stderr := runGlm(t, srvA, "", "diff", "widget", sysIDa, "-p", "a", "-p", "b")
	if strings.TrimSpace(stdout) != "" {
		t.Errorf("a one-sided miss prints no table: %q", stdout)
	}
	if !strings.Contains(stderr, "not found in b") || !strings.Contains(stderr, "present in a") {
		t.Errorf("summary should name the side missing it: %q", stderr)
	}
}

// TestDiffRecordMissingBoth: absent on both instances → exit 5.
func TestDiffRecordMissingBoth(t *testing.T) {
	srvA := diffServer(t, "widget", diffFake{record: nil})
	srvB := diffServer(t, "widget", diffFake{record: nil})
	twoProfiles(t, srvA, srvB)

	_, _, err := runGlmErr(t, srvA, "", "diff", "widget", sysIDa, "-p", "a", "-p", "b")
	var ec exitCoder
	if err == nil || !errors.As(err, &ec) || ec.ExitCode() != exit.NotFound {
		t.Errorf("missing on both sides must be exit %d, got: %v", exit.NotFound, err)
	}
}

func widgetDict(stateType, only, ownerRef string) []map[string]any {
	return []map[string]any{
		{"name": "widget", "element": "sys_id", "internal_type": "GUID"},
		{"name": "widget", "element": "name", "internal_type": "string", "display": "true"},
		{"name": "widget", "element": "state", "internal_type": stateType},
		{"name": "widget", "element": only, "internal_type": "string"},
		{"name": "widget", "element": "owner", "internal_type": "reference", "reference.name": ownerRef},
	}
}

// TestDiffSchemaDifferences: fields present on one side only plus type and
// reference mismatches are reported; matching fields are omitted.
func TestDiffSchemaDifferences(t *testing.T) {
	srvA := diffServer(t, "widget", diffFake{dict: widgetDict("integer", "only_a", "sys_user")})
	srvB := diffServer(t, "widget", diffFake{dict: widgetDict("string", "only_b", "sys_user_group")})
	twoProfiles(t, srvA, srvB)

	stdout, stderr := runGlm(t, srvA, "", "diff", "widget", "-p", "a", "-p", "b")
	for _, want := range []string{"state", "only_a", "only_b", "owner"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("expected %q in schema diff: %q", want, stdout)
		}
	}
	if !strings.Contains(stdout, "reference→sys_user") || !strings.Contains(stdout, "reference→sys_user_group") {
		t.Errorf("reference mismatch must render both targets: %q", stdout)
	}
	if !strings.Contains(stderr, "4 schema difference(s)") {
		t.Errorf("summary should count schema differences: %q", stderr)
	}
}

// TestDiffSchemaIdentical: identical dictionaries → "identical" summary.
func TestDiffSchemaIdentical(t *testing.T) {
	dict := widgetDict("integer", "extra", "sys_user")
	srvA := diffServer(t, "widget", diffFake{dict: dict})
	srvB := diffServer(t, "widget", diffFake{dict: dict})
	twoProfiles(t, srvA, srvB)

	stdout, stderr := runGlm(t, srvA, "", "diff", "widget", "-p", "a", "-p", "b")
	if strings.TrimSpace(stdout) != "" {
		t.Errorf("identical schemas print no table: %q", stdout)
	}
	if !strings.Contains(stderr, "schema is identical") {
		t.Errorf("summary should report identity: %q", stderr)
	}
}

// TestDiffSchemaTableMissingOneSide: a table on one instance only is a diff
// result, exit 0.
func TestDiffSchemaTableMissingOneSide(t *testing.T) {
	srvA := diffServer(t, "widget", diffFake{dict: widgetDict("integer", "extra", "sys_user")})
	srvB := diffServer(t, "widget", diffFake{}) // table absent on B
	twoProfiles(t, srvA, srvB)

	stdout, stderr := runGlm(t, srvA, "", "diff", "widget", "-p", "a", "-p", "b")
	if strings.TrimSpace(stdout) != "" {
		t.Errorf("a one-sided missing table prints no table: %q", stdout)
	}
	if !strings.Contains(stderr, "not found in b") || !strings.Contains(stderr, "present in a") {
		t.Errorf("summary should name the side missing the table: %q", stderr)
	}
}

// TestDiffSchemaTableMissingBoth: table absent on both → exit 5.
func TestDiffSchemaTableMissingBoth(t *testing.T) {
	srvA := diffServer(t, "widget", diffFake{})
	srvB := diffServer(t, "widget", diffFake{})
	twoProfiles(t, srvA, srvB)

	_, _, err := runGlmErr(t, srvA, "", "diff", "widget", "-p", "a", "-p", "b")
	var ec exitCoder
	if err == nil || !errors.As(err, &ec) || ec.ExitCode() != exit.NotFound {
		t.Errorf("missing table on both sides must be exit %d, got: %v", exit.NotFound, err)
	}
}
