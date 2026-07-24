package cli

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/tcurtsinger/GlideMind/internal/config"
	"github.com/tcurtsinger/GlideMind/internal/exit"
)

// diffFake is one instance's responses for a diff test.
type diffFake struct {
	dict   []map[string]any // sys_dictionary rows; empty => table absent
	record map[string]any   // the record; nil => record absent
	// dictNext, when set, is served from the 2nd dictionary fetch onward —
	// simulating a live schema change after a cache was warmed, so tests can
	// exercise refetch/self-heal.
	dictNext []map[string]any
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
	hasTable := len(f.dict) > 0 || len(f.dictNext) > 0
	var mu sync.Mutex
	dictHits := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/api/now/table/sys_db_object", func(w http.ResponseWriter, r *http.Request) {
		if hasTable && strings.Contains(r.URL.Query().Get("sysparm_query"), "name="+table) {
			snResult(w, []map[string]any{{"name": table, "super_class.name": ""}})
			return
		}
		snResult(w, []map[string]any{})
	})
	mux.HandleFunc("/api/now/table/sys_dictionary", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		dictHits++
		n := dictHits
		mu.Unlock()
		rows := f.dict
		if n >= 2 && f.dictNext != nil {
			rows = f.dictNext
		}
		snResult(w, rows)
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

// TestDiffRejectsIdsFormat: --format ids is rejected up front, so the outcome
// is the same whether or not the two sides differ (Codex review). Routing diff
// rows through the ids renderer would error only when rows exist, letting the
// presence of differences change the exit code (I5: differences are data).
func TestDiffRejectsIdsFormat(t *testing.T) {
	same := map[string]any{"sys_id": sysIDa, "state": "1"}
	differ := map[string]any{"sys_id": sysIDa, "state": "9"}
	for _, recB := range []map[string]any{same, differ} {
		srvA := diffServer(t, "widget", diffFake{record: same})
		srvB := diffServer(t, "widget", diffFake{record: recB})
		twoProfiles(t, srvA, srvB)
		_, _, err := runGlmErr(t, srvA, "", "diff", "widget", sysIDa, "-p", "a", "-p", "b", "--format", "ids")
		if err == nil || !strings.Contains(err.Error(), "ids is not meaningful") {
			t.Errorf("--format ids must be rejected regardless of differences, got: %v", err)
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
// result (exit 0). A pipe consumer (default TSV) must still see the present
// side's rows so a one-sided miss is distinguishable from identical; the
// interactive table suppresses them in favour of the stderr line.
func TestDiffRecordMissingOneSide(t *testing.T) {
	rec := map[string]any{"sys_id": sysIDa, "state": "1"}
	srvA := diffServer(t, "widget", diffFake{record: rec})
	srvB := diffServer(t, "widget", diffFake{record: nil}) // 404 on B
	twoProfiles(t, srvA, srvB)

	stdout, stderr := runGlm(t, srvA, "", "diff", "widget", sysIDa, "-p", "a", "-p", "b")
	if !strings.Contains(stdout, "state") {
		t.Errorf("default (piped) output must render the present side's rows: %q", stdout)
	}
	if !strings.Contains(stderr, "not found in b") || !strings.Contains(stderr, "present in a") {
		t.Errorf("summary should name the side missing it: %q", stderr)
	}

	// The interactive table suppresses the rows — the summary is the answer.
	stdout, _ = runGlm(t, srvA, "", "diff", "widget", sysIDa, "-p", "a", "-p", "b", "--format", "table")
	if strings.TrimSpace(stdout) != "" {
		t.Errorf("table format must suppress the one-sided rows: %q", stdout)
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

// TestDiffEmptyJSONIsValidArray: an identical diff with --format json must
// emit a valid empty array, not empty stdout that fails to unmarshal (Codex
// review) — the same zero-row JSON convention as query.
func TestDiffEmptyJSONIsValidArray(t *testing.T) {
	rec := map[string]any{"sys_id": sysIDa, "state": "1"}
	srvA := diffServer(t, "widget", diffFake{record: rec})
	srvB := diffServer(t, "widget", diffFake{record: rec})
	twoProfiles(t, srvA, srvB)

	stdout, _ := runGlm(t, srvA, "", "diff", "widget", sysIDa, "-p", "a", "-p", "b", "--format", "json")
	var rows []map[string]string
	if err := json.Unmarshal([]byte(stdout), &rows); err != nil {
		t.Fatalf("identical diff --format json must be valid JSON, got %q: %v", stdout, err)
	}
	if len(rows) != 0 {
		t.Errorf("identical diff must be an empty array, got %v", rows)
	}
}

// TestDiffJSONRendersRows: the non-empty JSON path stays a valid array of the
// differing fields.
func TestDiffJSONRendersRows(t *testing.T) {
	recA := map[string]any{"sys_id": sysIDa, "state": "1"}
	recB := map[string]any{"sys_id": sysIDa, "state": "9"}
	srvA := diffServer(t, "widget", diffFake{record: recA})
	srvB := diffServer(t, "widget", diffFake{record: recB})
	twoProfiles(t, srvA, srvB)

	stdout, _ := runGlm(t, srvA, "", "diff", "widget", sysIDa, "-p", "a", "-p", "b", "--format", "json")
	var rows []map[string]string
	if err := json.Unmarshal([]byte(stdout), &rows); err != nil {
		t.Fatalf("diff --format json must be valid JSON, got %q: %v", stdout, err)
	}
	if len(rows) != 1 || rows[0]["field"] != "state" || rows[0]["a"] != "1" || rows[0]["b"] != "9" {
		t.Errorf("expected one state row (a=1, b=9), got %v", rows)
	}
}

// TestDiffOneSidedJSONIsValid: a record present on one side only must still
// produce valid, informative JSON (the present side's fields vs empty), not
// empty stdout (Codex review) — the same contract as the identical case.
func TestDiffOneSidedJSONIsValid(t *testing.T) {
	recA := map[string]any{"sys_id": sysIDa, "state": "1", "short_description": "Alpha"}
	srvA := diffServer(t, "widget", diffFake{record: recA})
	srvB := diffServer(t, "widget", diffFake{record: nil}) // 404 on B
	twoProfiles(t, srvA, srvB)

	stdout, stderr := runGlm(t, srvA, "", "diff", "widget", sysIDa, "-p", "a", "-p", "b", "--format", "json")
	var rows []map[string]string
	if err := json.Unmarshal([]byte(stdout), &rows); err != nil {
		t.Fatalf("one-sided diff --format json must be valid JSON, got %q: %v", stdout, err)
	}
	if len(rows) != 2 {
		t.Fatalf("expected the present side's fields as rows, got %v", rows)
	}
	for _, r := range rows {
		if r["a"] == "" || r["b"] != "" {
			t.Errorf("present side should have a value, absent side empty: %v", r)
		}
	}
	if !strings.Contains(stderr, "not found in b") {
		t.Errorf("stderr should still name the missing side: %q", stderr)
	}
}

// TestDiffRecordNumberKeyTableMissingOneSide: with a number key, a table that
// exists on only one instance is a one-sided miss (exit 0), not a fatal
// not-found — the resolver's schema fetch 404s on the absent side (Codex
// review).
func TestDiffRecordNumberKeyTableMissingOneSide(t *testing.T) {
	dictA := []map[string]any{
		{"name": "widget", "element": "sys_id", "internal_type": "GUID"},
		{"name": "widget", "element": "number", "internal_type": "string", "display": "true"},
		{"name": "widget", "element": "state", "internal_type": "integer"},
	}
	recA := map[string]any{"sys_id": sysIDa, "number": "WID0001", "state": "1"}
	srvA := diffServer(t, "widget", diffFake{dict: dictA, record: recA})
	srvB := diffServer(t, "widget", diffFake{}) // table absent on B
	twoProfiles(t, srvA, srvB)

	// runGlm fails the test on a non-zero exit, so this asserts exit 0.
	_, stderr := runGlm(t, srvA, "", "diff", "widget", "WID0001", "-p", "a", "-p", "b")
	if !strings.Contains(stderr, "not found in b") || !strings.Contains(stderr, "present in a") {
		t.Errorf("a table absent on one side must be a one-sided miss, got: %q", stderr)
	}
}

// TestDiffFieldsValidation: --fields is validated against the union of both
// schemas (Codex review). A field present on only one instance is legitimate
// (drift is what diff surfaces); a name unknown on both is a typo that would
// otherwise be silently reported as identical.
func TestDiffFieldsValidation(t *testing.T) {
	dictA := []map[string]any{
		{"name": "widget", "element": "sys_id", "internal_type": "GUID"},
		{"name": "widget", "element": "name", "internal_type": "string", "display": "true"},
		{"name": "widget", "element": "severity", "internal_type": "integer"},
	}
	dictB := []map[string]any{
		{"name": "widget", "element": "sys_id", "internal_type": "GUID"},
		{"name": "widget", "element": "name", "internal_type": "string", "display": "true"},
	}
	recA := map[string]any{"sys_id": sysIDa, "severity": "high"}
	recB := map[string]any{"sys_id": sysIDa}
	srvA := diffServer(t, "widget", diffFake{dict: dictA, record: recA})
	srvB := diffServer(t, "widget", diffFake{dict: dictB, record: recB})
	twoProfiles(t, srvA, srvB)

	// Drift: severity exists on A only — a valid comparison, not a rejection.
	stdout, _ := runGlm(t, srvA, "", "diff", "widget", sysIDa, "-p", "a", "-p", "b", "--fields", "severity")
	if !strings.Contains(stdout, "severity") || !strings.Contains(stdout, "high") {
		t.Errorf("a field present on one side must be comparable, got: %q", stdout)
	}

	// Typo: unknown on both → hard error, not a false "identical".
	_, _, err := runGlmErr(t, srvA, "", "diff", "widget", sysIDa, "-p", "a", "-p", "b", "--fields", "severty")
	if err == nil || !strings.Contains(err.Error(), "severty") {
		t.Fatalf("a field unknown on both instances must be rejected, got: %v", err)
	}
}

// TestDiffSchemaReferenceTypeMismatch: a reference vs a glide_list to the same
// target is a real type difference and must be reported (Codex review) — the
// descriptor keeps the internal type, not just the target.
func TestDiffSchemaReferenceTypeMismatch(t *testing.T) {
	dictA := []map[string]any{
		{"name": "widget", "element": "sys_id", "internal_type": "GUID"},
		{"name": "widget", "element": "name", "internal_type": "string", "display": "true"},
		{"name": "widget", "element": "watchers", "internal_type": "reference", "reference.name": "sys_user"},
	}
	dictB := []map[string]any{
		{"name": "widget", "element": "sys_id", "internal_type": "GUID"},
		{"name": "widget", "element": "name", "internal_type": "string", "display": "true"},
		{"name": "widget", "element": "watchers", "internal_type": "glide_list", "reference.name": "sys_user"},
	}
	srvA := diffServer(t, "widget", diffFake{dict: dictA})
	srvB := diffServer(t, "widget", diffFake{dict: dictB})
	twoProfiles(t, srvA, srvB)

	stdout, stderr := runGlm(t, srvA, "", "diff", "widget", "-p", "a", "-p", "b")
	if !strings.Contains(stdout, "watchers") {
		t.Errorf("the differing field must be listed: %q", stdout)
	}
	if !strings.Contains(stdout, "reference→sys_user") || !strings.Contains(stdout, "glide_list→sys_user") {
		t.Errorf("both internal types must show (same target, different type): %q", stdout)
	}
	if !strings.Contains(stderr, "1 schema difference(s)") {
		t.Errorf("the type mismatch must be counted: %q", stderr)
	}
}

// TestDiffSchemaRefetchesLiveDictionary: a schema diff must reflect the live
// dictionaries, not cache within the TTL (Codex review) — a column added since
// a cache warmed must not let diff keep reporting a stale difference. Server A
// gains "extra" from its 2nd dictionary fetch; B always has it. The first diff
// (A's fetch #1, no extra) reports the difference; the second diff refetches
// (A's fetch #2, extra present) and reports identical.
func TestDiffSchemaRefetchesLiveDictionary(t *testing.T) {
	base := []map[string]any{
		{"name": "widget", "element": "sys_id", "internal_type": "GUID"},
		{"name": "widget", "element": "name", "internal_type": "string", "display": "true"},
	}
	withExtra := append(append([]map[string]any{}, base...),
		map[string]any{"name": "widget", "element": "extra", "internal_type": "string"})
	srvA := diffServer(t, "widget", diffFake{dict: base, dictNext: withExtra})
	srvB := diffServer(t, "widget", diffFake{dict: withExtra})
	twoProfiles(t, srvA, srvB)

	_, stderr := runGlm(t, srvA, "", "diff", "widget", "-p", "a", "-p", "b")
	if !strings.Contains(stderr, "1 schema difference(s)") {
		t.Fatalf("first diff should see A's stale dictionary lacking extra: %q", stderr)
	}
	_, stderr = runGlm(t, srvA, "", "diff", "widget", "-p", "a", "-p", "b")
	if !strings.Contains(stderr, "schema is identical") {
		t.Errorf("second diff must refetch and see A's live dictionary (with extra): %q", stderr)
	}
}

// TestDiffFieldsSelfHeal: a --fields name created after the caches warmed must
// not be falsely rejected (Codex review) — validation self-heals with a
// refetch before concluding the field is unknown everywhere. "tier" is absent
// from the first dictionary fetch and present from the second.
func TestDiffFieldsSelfHeal(t *testing.T) {
	base := []map[string]any{
		{"name": "widget", "element": "sys_id", "internal_type": "GUID"},
		{"name": "widget", "element": "name", "internal_type": "string", "display": "true"},
	}
	withTier := append(append([]map[string]any{}, base...),
		map[string]any{"name": "widget", "element": "tier", "internal_type": "integer"})
	recA := map[string]any{"sys_id": sysIDa, "tier": "1"}
	recB := map[string]any{"sys_id": sysIDa, "tier": "2"}
	srvA := diffServer(t, "widget", diffFake{dict: base, dictNext: withTier, record: recA})
	srvB := diffServer(t, "widget", diffFake{dict: base, dictNext: withTier, record: recB})
	twoProfiles(t, srvA, srvB)

	// runGlm fails on a non-zero exit, so this asserts the field was accepted.
	stdout, _ := runGlm(t, srvA, "", "diff", "widget", sysIDa, "-p", "a", "-p", "b", "--fields", "tier")
	if !strings.Contains(stdout, "tier") || !strings.Contains(stdout, "1") || !strings.Contains(stdout, "2") {
		t.Errorf("a field added after the caches warmed must be accepted and compared: %q", stdout)
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

	// Default (piped) output renders the present side's fields as present-vs-
	// absent so a pipe consumer can tell the table is one-sided, not identical.
	stdout, stderr := runGlm(t, srvA, "", "diff", "widget", "-p", "a", "-p", "b")
	if !strings.Contains(stdout, "state") {
		t.Errorf("default (piped) output must render the present side's schema: %q", stdout)
	}
	if !strings.Contains(stderr, "not found in b") || !strings.Contains(stderr, "present in a") {
		t.Errorf("summary should name the side missing the table: %q", stderr)
	}

	stdout, _ = runGlm(t, srvA, "", "diff", "widget", "-p", "a", "-p", "b", "--format", "table")
	if strings.TrimSpace(stdout) != "" {
		t.Errorf("table format must suppress the one-sided rows: %q", stdout)
	}
}

// TestDiffSchemaRefusesPartialDictionary: an ACL-filtered/partial dictionary
// (no sys_id sentinel) can't back a trustworthy schema diff, so diff refuses
// rather than risk a false "identical" (Codex review).
func TestDiffSchemaRefusesPartialDictionary(t *testing.T) {
	partial := []map[string]any{
		{"name": "widget", "element": "name", "internal_type": "string", "display": "true"},
		{"name": "widget", "element": "state", "internal_type": "integer"},
	}
	srvA := diffServer(t, "widget", diffFake{dict: partial}) // no sys_id row
	srvB := diffServer(t, "widget", diffFake{dict: widgetDict("integer", "extra", "sys_user")})
	twoProfiles(t, srvA, srvB)

	_, _, err := runGlmErr(t, srvA, "", "diff", "widget", "-p", "a", "-p", "b")
	if err == nil || !strings.Contains(err.Error(), "sys_id") || !strings.Contains(err.Error(), "a:") {
		t.Fatalf("a partial dictionary must refuse the schema diff, naming the profile: %v", err)
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
