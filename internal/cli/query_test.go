package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tcurtsinger/GlideMind/internal/config"
	"github.com/tcurtsinger/GlideMind/internal/secret"
)

const (
	sysIDa = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	sysIDb = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

// fakeInstance serves schema metadata, incident rows, and count stats;
// hits tracks which surfaces were touched.
func fakeInstance(t *testing.T, hits map[string]int) *httptest.Server {
	t.Helper()
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
		hits["schema"]++
		q := r.URL.Query().Get("sysparm_query")
		var rows []map[string]any
		switch {
		case strings.Contains(q, "name=incident"):
			rows = []map[string]any{{"name": "incident", "super_class.name": "task"}}
		case strings.Contains(q, "name=task"):
			rows = []map[string]any{{"name": "task", "super_class.name": ""}}
		}
		writeResult(w, rows)
	})
	mux.HandleFunc("/api/now/table/sys_dictionary", func(w http.ResponseWriter, r *http.Request) {
		hits["schema"]++
		writeResult(w, []map[string]any{
			{"name": "task", "element": "number", "internal_type": "string", "display": "true", "reference.name": ""},
			{"name": "task", "element": "short_description", "internal_type": "string", "display": "false", "reference.name": ""},
			{"name": "task", "element": "assigned_to", "internal_type": "reference", "display": "false", "reference.name": "sys_user"},
			{"name": "task", "element": "active", "internal_type": "boolean", "display": "false", "reference.name": ""},
			{"name": "task", "element": "sys_updated_on", "internal_type": "glide_date_time", "display": "false", "reference.name": ""},
			{"name": "incident", "element": "state", "internal_type": "integer", "display": "false", "reference.name": ""},
			{"name": "incident", "element": "priority", "internal_type": "integer", "display": "false", "reference.name": ""},
			{"name": "incident", "element": "description", "internal_type": "string", "display": "false", "reference.name": ""},
		})
	})
	mux.HandleFunc("/api/now/stats/incident", func(w http.ResponseWriter, r *http.Request) {
		hits["stats"]++
		writeResult(w, map[string]any{"stats": map[string]any{"count": "42"}})
	})
	mux.HandleFunc("/api/now/table/incident/", func(w http.ResponseWriter, r *http.Request) {
		hits["get"]++
		id := strings.TrimPrefix(r.URL.Path, "/api/now/table/incident/")
		writeResult(w, incidentRow(id, "INC0000001", "Printer on fire"))
	})
	mux.HandleFunc("/api/now/table/incident", func(w http.ResponseWriter, r *http.Request) {
		hits["list"]++
		q := r.URL.Query().Get("sysparm_query")
		if strings.Contains(q, "number=INC0000001") {
			writeResult(w, []map[string]any{incidentRow(sysIDa, "INC0000001", "Printer on fire")})
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

// runGlm executes the real command tree against the fake instance using pure
// env config, returning stdout and stderr.
func runGlm(t *testing.T, srv *httptest.Server, stdin string, args ...string) (string, string) {
	t.Helper()
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
	if err := root.Execute(); err != nil {
		t.Fatalf("glm %v: %v (stderr: %s)", args, err, errOut.String())
	}
	return out.String(), errOut.String()
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

func TestCountPrintsBareNumber(t *testing.T) {
	hits := map[string]int{}
	srv := fakeInstance(t, hits)

	stdout, _ := runGlm(t, srv, "", "count", "incident", "-q", "active=true")
	if strings.TrimRight(stdout, "\n") != "42" {
		t.Errorf("count output = %q, want 42", stdout)
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
