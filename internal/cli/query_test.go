package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/tcurtsinger/GlideMind/internal/audit"
	"github.com/tcurtsinger/GlideMind/internal/config"
	"github.com/tcurtsinger/GlideMind/internal/schema"
	"github.com/tcurtsinger/GlideMind/internal/secret"
)

const (
	sysIDa = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	sysIDb = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	sysIDc = "cccccccccccccccccccccccccccccccc"
	sysIDd = "dddddddddddddddddddddddddddddddd"
)

// fakeInstance serves schema metadata, incident rows, stats, and script
// tables; hits tracks which surfaces were touched (mutex-guarded — grep
// queries tables concurrently).
func fakeInstance(t *testing.T, hits map[string]int) *httptest.Server {
	t.Helper()
	var hitsMu sync.Mutex
	bump := func(key string) {
		hitsMu.Lock()
		hits[key]++
		hitsMu.Unlock()
	}
	writeResult := func(w http.ResponseWriter, v any) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"result": v}) //nolint:errcheck
	}
	incidentRow := func(id, number, desc string) map[string]any {
		return map[string]any{
			"sys_id": id, "number": number, "short_description": desc,
			"state": "In Progress", "priority": "1 - Critical",
			"assigned_to": "Travis Curtsinger", "sys_updated_on": "2026-07-22 04:00:00",
			"active": "true", "description": "long form text", "close_notes": "",
		}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/now/table/sys_db_object", func(w http.ResponseWriter, r *http.Request) {
		bump("schema")
		q := r.URL.Query().Get("sysparm_query")
		var rows []map[string]any
		switch {
		case strings.Contains(q, "LIKE"):
			rows = []map[string]any{{"name": "incident", "label": "Incident", "super_class": "Task", "sys_id": sysIDa}}
		case strings.Contains(q, "name=incident"):
			rows = []map[string]any{{"name": "incident", "super_class.name": "task"}}
		case strings.Contains(q, "name=task"):
			rows = []map[string]any{{"name": "task", "super_class.name": ""}}
		case strings.Contains(q, "name=evolving"):
			rows = []map[string]any{{"name": "evolving", "super_class.name": ""}}
		}
		writeResult(w, rows)
	})
	mux.HandleFunc("/api/now/table/sys_dictionary", func(w http.ResponseWriter, r *http.Request) {
		bump("schema")
		if strings.Contains(r.URL.Query().Get("sysparm_query"), "evolving") {
			bump("evolving-dict")
			rows := []map[string]any{
				{"name": "evolving", "element": "sys_id", "internal_type": "GUID", "display": "false"},
				{"name": "evolving", "element": "name", "internal_type": "string", "display": "true"},
			}
			// "tier" exists only from the 2nd dictionary fetch on — simulating
			// a field created after the first cache populate.
			if hits["evolving-dict"] > 1 {
				rows = append(rows, map[string]any{"name": "evolving", "element": "tier", "internal_type": "choice", "display": "false"})
			}
			// "legacy" exists ONLY on the first fetch — a field removed or
			// renamed after the cache was written (the opposite staleness
			// direction from "tier").
			if hits["evolving-dict"] == 1 {
				rows = append(rows, map[string]any{"name": "evolving", "element": "legacy", "internal_type": "string", "display": "false"})
			}
			writeResult(w, rows)
			return
		}
		writeResult(w, []map[string]any{
			{"name": "task", "element": "sys_id", "internal_type": "GUID", "display": "false", "reference.name": ""},
			{"name": "task", "element": "number", "internal_type": "string", "display": "true", "reference.name": ""},
			{"name": "task", "element": "short_description", "internal_type": "string", "display": "false", "reference.name": "", "mandatory": "true"},
			{"name": "task", "element": "assigned_to", "internal_type": "reference", "display": "false", "reference.name": "sys_user"},
			{"name": "task", "element": "active", "internal_type": "boolean", "display": "false", "reference.name": ""},
			{"name": "task", "element": "sys_updated_on", "internal_type": "glide_date_time", "display": "false", "reference.name": ""},
			{"name": "incident", "element": "state", "internal_type": "integer", "display": "false", "reference.name": ""},
			{"name": "incident", "element": "priority", "internal_type": "integer", "display": "false", "reference.name": ""},
			{"name": "incident", "element": "description", "internal_type": "string", "display": "false", "reference.name": ""},
			// In the dictionary (so a write validates) but deliberately never
			// returned by incidentRow — models a field a caller may write yet
			// cannot read (read ACL), the unreadable-diff case.
			{"name": "incident", "element": "secret_field", "internal_type": "string", "display": "false", "reference.name": ""},
		})
	})
	mux.HandleFunc("/api/now/stats/incident", func(w http.ResponseWriter, r *http.Request) {
		bump("stats")
		if r.URL.Query().Get("sysparm_group_by") == "" {
			writeResult(w, map[string]any{"stats": map[string]any{"count": "42"}})
			return
		}
		stats1 := map[string]any{"count": "5"}
		stats2 := map[string]any{"count": "3"}
		if r.URL.Query().Get("sysparm_sum_fields") != "" {
			stats1["sum"] = map[string]any{"reassignment_count": "7"}
			stats2["sum"] = map[string]any{"reassignment_count": "2"}
		}
		// Deliberately unsorted: New (3) before In Progress (5).
		writeResult(w, []map[string]any{
			{"stats": stats2, "groupby_fields": []map[string]any{{"field": "state", "value": "New"}}},
			{"stats": stats1, "groupby_fields": []map[string]any{{"field": "state", "value": "In Progress"}}},
		})
	})
	scriptRec := func(id, name, script string) map[string]any {
		return map[string]any{"sys_id": id, "name": name, "script": script}
	}
	mux.HandleFunc("/api/now/table/sys_script", func(w http.ResponseWriter, r *http.Request) {
		bump("grep")
		if strings.Contains(r.URL.Query().Get("sysparm_query"), "sys_updated_on>=javascript:gs.minutesAgoStart(120)") {
			bump("grep-since-120")
		}
		writeResult(w, []map[string]any{scriptRec(sysIDa, "Incident autoclose",
			"// closes stale incidents\nvar repo = new C1Repository('incident');\nrepo.closeStale();")})
	})
	mux.HandleFunc("/api/now/table/sys_script_include", func(w http.ResponseWriter, r *http.Request) {
		bump("grep")
		lines := make([]string, 7)
		for i := range lines {
			lines[i] = "C1Repository.prototype.method" + strconv.Itoa(i) + " = function() {};"
		}
		writeResult(w, []map[string]any{scriptRec(sysIDb, "C1Repository", strings.Join(lines, "\n"))})
	})
	mux.HandleFunc("/api/now/table/sys_script_client", func(w http.ResponseWriter, r *http.Request) {
		bump("grep")
		writeResult(w, []map[string]any{})
	})
	mux.HandleFunc("/api/now/table/sys_ui_action", func(w http.ResponseWriter, r *http.Request) {
		bump("grep")
		writeResult(w, []map[string]any{})
	})
	// No name column (like the real table) — grep must fall back to
	// short_description. Only the script_true LIKE query matches, mirroring
	// the server-side filter.
	mux.HandleFunc("/api/now/table/sys_ui_policy", func(w http.ResponseWriter, r *http.Request) {
		bump("grep")
		if !strings.HasPrefix(r.URL.Query().Get("sysparm_query"), "script_trueLIKE") {
			writeResult(w, []map[string]any{})
			return
		}
		writeResult(w, []map[string]any{{"sys_id": sysIDc, "short_description": "Hide VIP fields",
			"script_true": "g_form.setDisplay('vip', false);\nvar repo = new C1Repository('incident');"}})
	})
	mux.HandleFunc("/api/now/table/sys_attachment", func(w http.ResponseWriter, r *http.Request) {
		bump("attach")
		if v := r.URL.Query().Get("sysparm_offset"); v != "" {
			bump("attach-offset-" + v)
		}
		if r.URL.Query().Get("sysparm_query") != "table_name=incident^table_sys_id="+sysIDa+"^ORDERBYfile_name^ORDERBYsys_id" {
			writeResult(w, []map[string]any{})
			return
		}
		writeResult(w, []map[string]any{
			{"sys_id": sysIDc, "file_name": "error.log", "size_bytes": "22", "content_type": "text/plain", "sys_updated_on": "2026-07-22 04:00:00"},
			{"sys_id": sysIDd, "file_name": "screenshot.png", "size_bytes": "34518", "content_type": "image/png", "sys_updated_on": "2026-07-22 04:00:00"},
		})
	})
	mux.HandleFunc("/api/now/table/long_story", func(w http.ResponseWriter, r *http.Request) {
		bump("list")
		writeResult(w, []map[string]any{{"sys_id": sysIDa, "tale": strings.Repeat("x", 2500)}})
	})
	mux.HandleFunc("/api/now/table/evolving", func(w http.ResponseWriter, r *http.Request) {
		bump("evolving-list")
		w.Header().Set("X-Total-Count", "1")
		writeResult(w, []map[string]any{{"sys_id": sysIDa, "name": "Evolving One"}})
	})
	mux.HandleFunc("/api/now/table/fidelity", func(w http.ResponseWriter, r *http.Request) {
		bump("list")
		// Raw bytes, not a Go map: exercises large-int precision, native
		// bool, an explicit null, and a reference object with extra keys.
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"result":{"big":9007199254740993,"flag":true,"empty":null,` + //nolint:errcheck
			`"ref":{"value":"abc","display_value":"ABC","extra":"keep me"}}}`))
	})
	mux.HandleFunc("/api/now/table/trailing", func(w http.ResponseWriter, r *http.Request) {
		bump("list")
		// A JSON value with appended bytes: not a clean document, so glm
		// must pass the whole body through verbatim, not drop the tail.
		w.Write([]byte(`{"result":{"a":1}} then junk`)) //nolint:errcheck
	})
	mux.HandleFunc("/api/now/attachment/", func(w http.ResponseWriter, r *http.Request) {
		bump("attach")
		// sysIDd is the attachment whose download always fails.
		broken := strings.Contains(r.URL.Path, sysIDd)
		if strings.HasSuffix(r.URL.Path, "/file") {
			if broken {
				w.WriteHeader(http.StatusInternalServerError)
				w.Write([]byte(`{"error":{"message":"attachment storage offline"}}`)) //nolint:errcheck
				return
			}
			w.Write([]byte("log line 1\nlog line 2\n")) //nolint:errcheck
			return
		}
		name := "error.log"
		if broken {
			name = "broken.log"
		}
		writeResult(w, map[string]any{
			"sys_id": sysIDc, "file_name": name, "size_bytes": "22", "content_type": "text/plain",
		})
	})
	mux.HandleFunc("/api/now/table/incident/", func(w http.ResponseWriter, r *http.Request) {
		bump("get") // every record-endpoint hit, any method (historic key)
		if r.Method != http.MethodGet {
			bump(strings.ToLower(r.Method))
			var body map[string]string
			if json.NewDecoder(r.Body).Decode(&body) == nil {
				for k, v := range body {
					bump("set-" + k + "=" + v)
				}
			}
		}
		id := strings.TrimPrefix(r.URL.Path, "/api/now/table/incident/")
		writeResult(w, incidentRow(id, "INC0000001", "Printer on fire"))
	})
	mux.HandleFunc("/api/now/table/sys_user", func(w http.ResponseWriter, r *http.Request) {
		bump("whoami-user")
		writeResult(w, []map[string]any{{"user_name": "svc.glm", "name": "SVC GLM"}})
	})
	mux.HandleFunc("/api/now/table/sys_user_has_role", func(w http.ResponseWriter, r *http.Request) {
		bump("whoami-roles")
		off, _ := strconv.Atoi(r.URL.Query().Get("sysparm_offset"))
		// 250 grants across two 200-row pages, to exercise pagination.
		var rows []map[string]any
		for i := off; i < off+200 && i < 250; i++ {
			rows = append(rows, map[string]any{"role.name": "role_" + strconv.Itoa(i)})
		}
		writeResult(w, rows)
	})
	mux.HandleFunc("/api/now/table/incident", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			bump("post")
			var body map[string]string
			if json.NewDecoder(r.Body).Decode(&body) == nil {
				for k, v := range body {
					bump("set-" + k + "=" + v)
				}
			}
			// Echo the created record's identity, as the Table API does.
			writeResult(w, map[string]any{"sys_id": sysIDa, "number": "INC0000042"})
			return
		}
		bump("list")
		q := r.URL.Query().Get("sysparm_query")
		if strings.Contains(q, "ORDERBYsys_id") {
			bump("stable-sort")
		}
		if strings.Contains(q, "number=INC0000001") {
			writeResult(w, []map[string]any{incidentRow(sysIDa, "INC0000001", "Printer on fire")})
			return
		}
		// Any other number lookup misses — the not-found path for key
		// resolution (get, update).
		if strings.HasPrefix(q, "number=") {
			writeResult(w, []map[string]any{})
			return
		}
		w.Header().Set("X-Total-Count", "47")
		writeResult(w, []map[string]any{
			incidentRow(sysIDa, "INC0000001", "Printer on fire"),
			incidentRow(sysIDb, "INC0000002", "Coffee machine DDoS"),
		})
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// isolateCache points the schema cache at a per-test temp dir, stable across
// runGlm calls within one test so cache-warming scenarios work.
func isolateCache(t *testing.T) {
	t.Helper()
	if os.Getenv("GLM_TEST_CACHE_OWNER") == t.Name() {
		return
	}
	t.Setenv(schema.EnvCacheDir, t.TempDir())
	t.Setenv("GLM_TEST_CACHE_OWNER", t.Name())
}

// isolateConfig points the config dir at a per-test temp dir, stable across
// runGlm calls within one test so a test can write a config file first (e.g.
// a writable profile) and still see it inside runGlm.
func isolateConfig(t *testing.T) {
	t.Helper()
	if os.Getenv("GLM_TEST_CONF_OWNER") == t.Name() {
		return
	}
	dir := t.TempDir()
	t.Setenv("APPDATA", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("GLM_TEST_CONF_OWNER", t.Name())
}

// runGlm executes the real command tree against the fake instance using pure
// env config, returning stdout and stderr; failures are fatal.
func runGlm(t *testing.T, srv *httptest.Server, stdin string, args ...string) (string, string) {
	t.Helper()
	stdout, stderr, err := runGlmErr(t, srv, stdin, args...)
	if err != nil {
		t.Fatalf("glm %v: %v (stderr: %s)", args, err, stderr)
	}
	return stdout, stderr
}

// runGlmErr is runGlm for scenarios where the command is expected to fail.
func runGlmErr(t *testing.T, srv *httptest.Server, stdin string, args ...string) (string, string, error) {
	t.Helper()
	isolateCache(t)
	// Isolate the config file too: commands that read it (prime's profile
	// line, the write gate) must see the test's world, not the developer's
	// real profiles.
	isolateConfig(t)
	// And the write audit log — a test write must never append to the
	// developer's real audit trail. Tests asserting audit content set
	// audit.EnvLogPath themselves after this.
	if os.Getenv("GLM_TEST_AUDIT_OWNER") != t.Name() {
		t.Setenv(audit.EnvLogPath, filepath.Join(t.TempDir(), "audit.jsonl"))
		t.Setenv("GLM_TEST_AUDIT_OWNER", t.Name())
	}
	t.Setenv(config.EnvProfile, "")
	t.Setenv(config.EnvInstance, srv.URL)
	t.Setenv(config.EnvUsername, "svc.glm")
	t.Setenv(secret.EnvPassword, "pw")

	var out, errOut bytes.Buffer
	root := newRootCmd()
	root.SetOut(&out)
	root.SetErr(&errOut)
	if stdin != "" {
		root.SetIn(strings.NewReader(stdin))
	}
	root.SetArgs(args)
	err := root.Execute()
	return out.String(), errOut.String(), err
}

func TestQueryZeroConfigDefaults(t *testing.T) {
	hits := map[string]int{}
	srv := fakeInstance(t, hits)

	stdout, stderr := runGlm(t, srv, "", "query", "incident", "--limit", "2")

	lines := strings.Split(strings.TrimRight(stdout, "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("want header + 2 rows, got:\n%s", stdout)
	}
	wantHeader := "number\tshort_description\tstate\tpriority\tassigned_to\tsys_updated_on\tactive"
	if lines[0] != wantHeader {
		t.Errorf("derived header = %q, want %q", lines[0], wantHeader)
	}
	if !strings.Contains(lines[1], "In Progress") {
		t.Errorf("display values expected: %q", lines[1])
	}
	if strings.Contains(stdout, sysIDa) {
		t.Errorf("tabular output should not include sys_id by default:\n%s", stdout)
	}
	if !strings.Contains(stderr, "rows 1-2 of 47 - next: --offset 2") {
		t.Errorf("pagination meta missing on stderr: %q", stderr)
	}
	if hits["schema"] == 0 {
		t.Error("expected schema derivation calls")
	}
}

func TestQueryIDsFormat(t *testing.T) {
	hits := map[string]int{}
	srv := fakeInstance(t, hits)

	stdout, _ := runGlm(t, srv, "", "query", "incident", "--format", "ids")
	if stdout != sysIDa+"\n"+sysIDb+"\n" {
		t.Errorf("ids output = %q", stdout)
	}
	if hits["schema"] != 0 {
		t.Errorf("ids format must not trigger schema lookups (got %d)", hits["schema"])
	}
}

func TestQueryExplicitFieldsSkipSchema(t *testing.T) {
	hits := map[string]int{}
	srv := fakeInstance(t, hits)

	runGlm(t, srv, "", "query", "incident", "--fields", "number")
	if hits["schema"] != 0 {
		t.Errorf("--fields must not trigger schema lookups (got %d)", hits["schema"])
	}
}

func TestGetBySysIDShowsNonEmptyOnly(t *testing.T) {
	hits := map[string]int{}
	srv := fakeInstance(t, hits)

	stdout, _ := runGlm(t, srv, "", "get", "incident", sysIDa)
	if !strings.Contains(stdout, "number") || !strings.Contains(stdout, "INC0000001") {
		t.Errorf("detail missing number:\n%s", stdout)
	}
	if strings.Contains(stdout, "close_notes") {
		t.Errorf("empty fields must be omitted:\n%s", stdout)
	}
	if hits["get"] != 1 {
		t.Errorf("expected direct sys_id fetch, hits=%v", hits)
	}
}

func TestGetByNumberLookup(t *testing.T) {
	hits := map[string]int{}
	srv := fakeInstance(t, hits)

	stdout, _ := runGlm(t, srv, "", "get", "incident", "INC0000001")
	if !strings.Contains(stdout, "Printer on fire") {
		t.Errorf("number lookup failed:\n%s", stdout)
	}
	if hits["get"] != 0 || hits["list"] != 1 {
		t.Errorf("number key should query the list endpoint once, hits=%v", hits)
	}
}

func TestGetSingleFieldPrintsBareValue(t *testing.T) {
	hits := map[string]int{}
	srv := fakeInstance(t, hits)

	stdout, _ := runGlm(t, srv, "", "get", "incident", sysIDa, "--fields", "description")
	if strings.TrimRight(stdout, "\n") != "long form text" {
		t.Errorf("single-field get should print the bare value, got %q", stdout)
	}
}

func TestGetExplicitTSVIsDelimited(t *testing.T) {
	hits := map[string]int{}
	srv := fakeInstance(t, hits)

	stdout, _ := runGlm(t, srv, "", "get", "incident", sysIDa, "--format", "tsv")
	lines := strings.Split(strings.TrimRight(stdout, "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("explicit tsv get should be header + one row, got:\n%s", stdout)
	}
	if !strings.Contains(lines[0], "\t") || !strings.Contains(lines[1], "\t") {
		t.Errorf("expected tab-delimited output:\n%s", stdout)
	}
	// Default (no --format) stays the key/value detail view even when piped.
	stdout, _ = runGlm(t, srv, "", "get", "incident", sysIDa)
	if strings.Count(strings.TrimRight(stdout, "\n"), "\n") < 3 {
		t.Errorf("default get should be the multi-line detail view:\n%s", stdout)
	}
}

func TestGetStdinBatchEmitsJSONL(t *testing.T) {
	hits := map[string]int{}
	srv := fakeInstance(t, hits)

	stdout, _ := runGlm(t, srv, sysIDa+"\n"+sysIDb+"\n", "get", "incident", "-")
	lines := strings.Split(strings.TrimRight(stdout, "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("want 2 jsonl lines, got %d:\n%s", len(lines), stdout)
	}
	for _, line := range lines {
		var obj map[string]string
		if err := json.Unmarshal([]byte(line), &obj); err != nil {
			t.Fatalf("not jsonl: %v (%q)", err, line)
		}
	}
	if hits["get"] != 2 {
		t.Errorf("expected 2 direct fetches, hits=%v", hits)
	}
}

func TestAggGroupBySortsByCountDesc(t *testing.T) {
	hits := map[string]int{}
	srv := fakeInstance(t, hits)

	stdout, stderr := runGlm(t, srv, "", "agg", "incident", "--group-by", "state")
	lines := strings.Split(strings.TrimRight(stdout, "\n"), "\n")
	want := []string{"state\tcount", "In Progress\t5", "New\t3"}
	if strings.Join(lines, "|") != strings.Join(want, "|") {
		t.Errorf("agg output = %q, want %q", lines, want)
	}
	if !strings.Contains(stderr, "groups 1-2 of 2") {
		t.Errorf("groups meta missing: %q", stderr)
	}
}

func TestAggSumColumns(t *testing.T) {
	hits := map[string]int{}
	srv := fakeInstance(t, hits)

	stdout, _ := runGlm(t, srv, "", "agg", "incident", "--group-by", "state", "--sum", "reassignment_count")
	lines := strings.Split(strings.TrimRight(stdout, "\n"), "\n")
	if lines[0] != "state\tsum(reassignment_count)" {
		t.Errorf("sum header = %q (count must not be implied alongside --sum)", lines[0])
	}
	if lines[1] != "In Progress\t7" {
		t.Errorf("sum rows should sort by the aggregate desc: %q", lines[1])
	}
}

func TestAggWithoutGroupIsSingleRow(t *testing.T) {
	hits := map[string]int{}
	srv := fakeInstance(t, hits)

	stdout, stderr := runGlm(t, srv, "", "agg", "incident")
	if strings.TrimRight(stdout, "\n") != "count\n42" {
		t.Errorf("ungrouped agg = %q", stdout)
	}
	if strings.Contains(stderr, "groups") {
		t.Errorf("no groups meta expected without --group-by: %q", stderr)
	}
}

func TestGrepTextOutput(t *testing.T) {
	hits := map[string]int{}
	srv := fakeInstance(t, hits)

	stdout, stderr := runGlm(t, srv, "", "grep", "C1Repository")
	if !strings.Contains(stdout, "sys_script:Incident autoclose:2: var repo = new C1Repository('incident');") {
		t.Errorf("business-rule match line missing:\n%s", stdout)
	}
	// 7 matching lines capped at 5 with a remainder marker naming the remedy.
	if !strings.Contains(stdout, "sys_script_include:C1Repository: +2 more matches (glm get sys_script_include "+sysIDb+" --fields script --full)") {
		t.Errorf("cap marker missing:\n%s", stdout)
	}
	// sys_ui_policy has no name column — short_description stands in.
	if !strings.Contains(stdout, "sys_ui_policy:Hide VIP fields:2: var repo = new C1Repository('incident');") {
		t.Errorf("ui policy match missing:\n%s", stdout)
	}
	if !strings.Contains(stderr, "7 matching lines in 3 records - searched sys_script, sys_script_include, sys_script_client, sys_ui_action, sys_ui_policy") {
		t.Errorf("summary wrong: %q", stderr)
	}
}

func TestGrepMissingFieldSkipsRecords(t *testing.T) {
	hits := map[string]int{}
	srv := fakeInstance(t, hits)

	// The incident fixture returns rows for any query but has no script
	// field — like an instance that ignores invalid query fields. Those
	// rows must not surface as matches.
	stdout, stderr := runGlm(t, srv, "", "grep", "C1Repository", "--tables", "sys_script,incident")
	if strings.Contains(stdout, "Printer on fire") || strings.Contains(stdout, "(match spans lines)") {
		t.Errorf("records without the searched field must not become matches:\n%s", stdout)
	}
	if !strings.Contains(stdout, "sys_script:Incident autoclose:2:") {
		t.Errorf("real matches must survive:\n%s", stdout)
	}
	if !strings.Contains(stderr, `incident: field "script" empty or missing in 2 record(s)`) {
		t.Errorf("want missing-field warning, got: %q", stderr)
	}

	// Every target useless -> hard error with the schema remedy.
	_, _, err := runGlmErr(t, srv, "", "grep", "C1Repository", "--tables", "incident")
	if err == nil || !strings.Contains(err.Error(), "glm schema incident") {
		t.Errorf("grep on a table without the field should fail with a remedy, got: %v", err)
	}
}

func TestGrepSince(t *testing.T) {
	hits := map[string]int{}
	srv := fakeInstance(t, hits)

	runGlm(t, srv, "", "grep", "C1Repository", "--since", "2h")
	if hits["grep-since-120"] != 1 {
		t.Errorf("--since 2h must add the minutesAgoStart(120) clause to the table queries")
	}
	if _, _, err := runGlmErr(t, srv, "", "grep", "x", "--since", "soon"); err == nil || !strings.Contains(err.Error(), "invalid --since") {
		t.Errorf("bad --since should be rejected, got: %v", err)
	}
}

func TestGrepJSONL(t *testing.T) {
	hits := map[string]int{}
	srv := fakeInstance(t, hits)

	stdout, _ := runGlm(t, srv, "", "grep", "C1Repository", "--json")
	lines := strings.Split(strings.TrimRight(stdout, "\n"), "\n")
	if len(lines) != 7 {
		t.Fatalf("want 7 jsonl match lines, got %d:\n%s", len(lines), stdout)
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &obj); err != nil {
		t.Fatalf("not jsonl: %v", err)
	}
	for _, key := range []string{"table", "field", "sys_id", "name", "line", "text"} {
		if _, ok := obj[key]; !ok {
			t.Errorf("jsonl match missing %q: %s", key, lines[0])
		}
	}
}

func TestCaretValuesRejected(t *testing.T) {
	hits := map[string]int{}
	srv := fakeInstance(t, hits)

	if _, _, err := runGlmErr(t, srv, "", "grep", "active=true^priority=1"); err == nil || !strings.Contains(err.Error(), "encoded-query separator") {
		t.Errorf("grep pattern with ^ should be rejected, got: %v", err)
	}
	if _, _, err := runGlmErr(t, srv, "", "tables", "a^b"); err == nil || !strings.Contains(err.Error(), "encoded-query separator") {
		t.Errorf("tables pattern with ^ should be rejected, got: %v", err)
	}
	if _, _, err := runGlmErr(t, srv, "", "get", "incident", "a^b"); err == nil || !strings.Contains(err.Error(), "sys_id") {
		t.Errorf("get key with ^ should be rejected with sys_id remedy, got: %v", err)
	}
}

func TestGrepFormatJSONIsSingleDocument(t *testing.T) {
	hits := map[string]int{}
	srv := fakeInstance(t, hits)

	stdout, _ := runGlm(t, srv, "", "grep", "C1Repository", "--format", "json")
	var objs []map[string]any
	if err := json.Unmarshal([]byte(stdout), &objs); err != nil {
		t.Fatalf("--format json must be one JSON document: %v\n%s", err, stdout)
	}
	if len(objs) != 7 {
		t.Errorf("want 7 match objects, got %d", len(objs))
	}
}

func TestCountPrintsBareNumber(t *testing.T) {
	hits := map[string]int{}
	srv := fakeInstance(t, hits)

	stdout, _ := runGlm(t, srv, "", "count", "incident", "-q", "active=true")
	if strings.TrimRight(stdout, "\n") != "42" {
		t.Errorf("count output = %q, want 42", stdout)
	}
}

func TestGetExplicitFieldsDelimitedSchema(t *testing.T) {
	hits := map[string]int{}
	srv := fakeInstance(t, hits)

	stdout, _ := runGlm(t, srv, "", "get", "incident", sysIDa, "--fields", "number,short_description", "--format", "tsv")
	lines := strings.Split(strings.TrimRight(stdout, "\n"), "\n")
	if lines[0] != "number\tshort_description" {
		t.Errorf("delimited get must honor the requested field list exactly, got header %q", lines[0])
	}
	if strings.Contains(stdout, sysIDa) {
		t.Errorf("sys_id must not leak into delimited output unless requested:\n%s", stdout)
	}
}

func TestQueryFieldTypoDidYouMean(t *testing.T) {
	hits := map[string]int{}
	srv := fakeInstance(t, hits)

	runGlm(t, srv, "", "query", "incident") // warms the schema cache
	_, _, err := runGlmErr(t, srv, "", "query", "incident", "--fields", "priorty")
	if err == nil {
		t.Fatal("typo'd field should fail pre-flight")
	}
	if !strings.Contains(err.Error(), `"priority"`) || !strings.Contains(err.Error(), "glm schema incident") {
		t.Errorf("want did-you-mean with remedy, got: %v", err)
	}
}

func TestQueryEncodedClauseTypo(t *testing.T) {
	hits := map[string]int{}
	srv := fakeInstance(t, hits)

	runGlm(t, srv, "", "query", "incident")
	_, _, err := runGlmErr(t, srv, "", "query", "incident", "-q", "stat=1")
	if err == nil {
		t.Fatal("typo'd query field should fail pre-flight")
	}
	if !strings.Contains(err.Error(), `"state"`) {
		t.Errorf("want state suggestion, got: %v", err)
	}
}

func TestQueryValidationColdCacheNeverBlocks(t *testing.T) {
	hits := map[string]int{}
	srv := fakeInstance(t, hits)

	// Cold cache: no validation, no schema calls — the typo goes through to
	// the instance rather than blocking or costing extra requests.
	stdout, _ := runGlm(t, srv, "", "query", "incident", "--fields", "priorty")
	if hits["schema"] != 0 {
		t.Errorf("cold-cache validation must not trigger schema calls (got %d)", hits["schema"])
	}
	if stdout == "" {
		t.Error("query should still have executed")
	}
}

func TestGetFieldTypoDidYouMean(t *testing.T) {
	hits := map[string]int{}
	srv := fakeInstance(t, hits)

	runGlm(t, srv, "", "query", "incident") // warm cache
	_, _, err := runGlmErr(t, srv, "", "get", "incident", sysIDa, "--fields", "nmber")
	if err == nil {
		t.Fatal("typo'd get field should fail pre-flight")
	}
	if !strings.Contains(err.Error(), `"number"`) {
		t.Errorf("want number suggestion, got: %v", err)
	}
}

func TestSchemaCommand(t *testing.T) {
	hits := map[string]int{}
	srv := fakeInstance(t, hits)

	stdout, stderr := runGlm(t, srv, "", "schema", "incident")
	if !strings.Contains(stdout, "assigned_to\treference\tsys_user") {
		t.Errorf("schema rows missing reference info:\n%s", stdout)
	}
	if !strings.Contains(stdout, "short_description\tstring\t\ttrue") {
		t.Errorf("schema rows missing mandatory flag:\n%s", stdout)
	}
	if !strings.Contains(stderr, "display field: number") || !strings.Contains(stderr, "chain: incident < task") {
		t.Errorf("schema meta line wrong: %q", stderr)
	}

	// Second run must serve from cache — no further schema endpoints hits.
	before := hits["schema"]
	runGlm(t, srv, "", "schema", "incident")
	if hits["schema"] != before {
		t.Errorf("second schema run should hit the cache (calls %d -> %d)", before, hits["schema"])
	}
}

func TestTablesCommand(t *testing.T) {
	hits := map[string]int{}
	srv := fakeInstance(t, hits)

	stdout, _ := runGlm(t, srv, "", "tables", "inc")
	if !strings.Contains(stdout, "incident\tIncident\tTask") {
		t.Errorf("tables output wrong:\n%s", stdout)
	}
}

func TestEmitPageMeta(t *testing.T) {
	cases := []struct {
		offset, got, total, limit int
		want                      string
	}{
		{0, 2, 47, 2, "rows 1-2 of 47 - next: --offset 2"},
		// ACL-shrunk window: advance by limit, not by visible rows.
		{0, 2, 47, 5, "rows 1-2 of 47 - next: --offset 5"},
		{45, 2, 47, 5, "rows 46-47 of 47"},
		{0, 0, 0, 25, "no rows"},
		// Fully ACL-hidden first window with a known total still advances.
		{0, 0, 47, 25, "no rows - next: --offset 25"},
		// Empty final window: next would pass the total, so no hint.
		{25, 0, 47, 25, "no rows in this window"},
		// Empty window with unknown total must NOT hint at itself.
		{25, 0, -1, 25, "no rows in this window"},
		{0, 25, -1, 25, "rows 1-25 - more may exist - next: --offset 25"},
		{0, 3, -1, 25, "rows 1-3 - more may exist - next: --offset 25"},
	}
	for _, tc := range cases {
		var buf bytes.Buffer
		emitPageMeta(&buf, tc.offset, tc.got, tc.total, tc.limit)
		if got := strings.TrimRight(buf.String(), "\n"); got != tc.want {
			t.Errorf("emitPageMeta(offset=%d, got=%d, total=%d, limit=%d) = %q, want %q",
				tc.offset, tc.got, tc.total, tc.limit, got, tc.want)
		}
	}
}

func TestParseSince(t *testing.T) {
	cases := map[string]int{"15m": 15, "2h": 120, "3d": 4320, "90s": 2}
	for in, want := range cases {
		got, err := parseSince(in)
		if err != nil || got != want {
			t.Errorf("parseSince(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "-5m", "yesterday", "0d"} {
		if _, err := parseSince(bad); err == nil {
			t.Errorf("parseSince(%q) should error", bad)
		}
	}
}
