package cli

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tcurtsinger/GlideMind/internal/config"
)

func TestAttachListBySysIDAndByNumber(t *testing.T) {
	hits := map[string]int{}
	srv := fakeInstance(t, hits)

	stdout, stderr := runGlm(t, srv, "", "attach", "list", "incident", sysIDa, "--format", "table")
	if !strings.Contains(stdout, "error.log") || !strings.Contains(stdout, "screenshot.png") {
		t.Errorf("attachment rows missing:\n%s", stdout)
	}
	// sys_id is the handle attach get needs — visible in every format.
	if !strings.Contains(stdout, sysIDc) {
		t.Errorf("attachment sys_id missing from table output:\n%s", stdout)
	}
	if !strings.Contains(stderr, "rows 1-2") {
		t.Errorf("pagination summary missing: %q", stderr)
	}

	// Human keys resolve through the same lookup path as glm get.
	stdout, _ = runGlm(t, srv, "", "attach", "list", "incident", "INC0000001", "--format", "ids")
	want := sysIDc + "\n" + sysIDd + "\n"
	if stdout != want {
		t.Errorf("ids format = %q, want %q", stdout, want)
	}
}

func TestAttachListOffsetPaginates(t *testing.T) {
	hits := map[string]int{}
	srv := fakeInstance(t, hits)

	_, stderr := runGlm(t, srv, "", "attach", "list", "incident", sysIDa, "--offset", "1")
	if hits["attach-offset-1"] != 1 {
		t.Errorf("--offset must reach the query as sysparm_offset: %v", hits)
	}
	// The hint the summary prints must name a flag that actually exists.
	if !strings.Contains(stderr, "rows 2-3") || !strings.Contains(stderr, "next: --offset 26") {
		t.Errorf("pagination summary should reflect the offset window: %q", stderr)
	}
}

func TestAttachGetDownloads(t *testing.T) {
	hits := map[string]int{}
	srv := fakeInstance(t, hits)
	content := "log line 1\nlog line 2\n"

	// Explicit -o path.
	dest := filepath.Join(t.TempDir(), "out.log")
	stdout, stderr := runGlm(t, srv, "", "attach", "get", sysIDc, "-o", dest)
	if got, err := os.ReadFile(dest); err != nil || string(got) != content {
		t.Errorf("downloaded file = %q, %v", got, err)
	}
	if strings.TrimSpace(stdout) != dest {
		t.Errorf("stdout should carry the written path, got %q", stdout)
	}
	if !strings.Contains(stderr, "error.log - 22 bytes (text/plain)") {
		t.Errorf("size summary missing: %q", stderr)
	}

	// -o <existing dir> joins the attachment's own (sanitized) file name.
	dir := t.TempDir()
	runGlm(t, srv, "", "attach", "get", sysIDc, "-o", dir)
	if _, err := os.Stat(filepath.Join(dir, "error.log")); err != nil {
		t.Errorf("expected error.log inside -o directory: %v", err)
	}

	// Default name in the CWD; a second run must refuse to overwrite.
	cwd := t.TempDir()
	t.Chdir(cwd)
	runGlm(t, srv, "", "attach", "get", sysIDc)
	if _, _, err := runGlmErr(t, srv, "", "attach", "get", sysIDc); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Errorf("derived-name overwrite must be refused, got: %v", err)
	}
	// The staging temp must be cleaned up — only the final file remains.
	entries, _ := os.ReadDir(cwd)
	for _, e := range entries {
		if strings.Contains(e.Name(), ".glm") {
			t.Errorf("staging temp left behind: %s", e.Name())
		}
	}

	// -o - streams the bytes to stdout.
	stdout, _ = runGlm(t, srv, "", "attach", "get", sysIDc, "-o", "-")
	if stdout != content {
		t.Errorf("-o - stdout = %q, want file bytes", stdout)
	}

	// Junk keys never reach the network.
	if _, _, err := runGlmErr(t, srv, "", "attach", "get", "notasysid"); err == nil || !strings.Contains(err.Error(), "attach list") {
		t.Errorf("non-sys_id key should fail with remedy, got: %v", err)
	}
}

func TestAttachGetFailedDownloadPreservesTarget(t *testing.T) {
	hits := map[string]int{}
	srv := fakeInstance(t, hits)

	dir := t.TempDir()
	dest := filepath.Join(dir, "keep.log")
	if err := os.WriteFile(dest, []byte("precious"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := runGlmErr(t, srv, "", "attach", "get", sysIDd, "-o", dest); err == nil {
		t.Fatal("download of the broken attachment should fail")
	}
	got, err := os.ReadFile(dest)
	if err != nil || string(got) != "precious" {
		t.Errorf("failed download must not clobber the target, got %q, %v", got, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Errorf("temp file left behind after failure: %v", entries)
	}
}

func TestAPIGetRendersResultArray(t *testing.T) {
	hits := map[string]int{}
	srv := fakeInstance(t, hits)

	stdout, stderr := runGlm(t, srv, "", "api", "GET", "/api/now/table/incident", "-f", "sysparm_limit=2", "--format", "table")
	if !strings.Contains(stdout, "INC0000001") || !strings.Contains(stdout, "INC0000002") {
		t.Errorf("result rows missing:\n%s", stdout)
	}
	// Columns are the alphabetical union of keys.
	if !strings.Contains(stdout, "number") || !strings.Contains(stdout, "short_description") {
		t.Errorf("derived columns missing:\n%s", stdout)
	}
	if !strings.Contains(stderr, "2 rows") {
		t.Errorf("row summary missing: %q", stderr)
	}
	if hits["list"] != 1 {
		t.Errorf("want exactly one table hit, got %d", hits["list"])
	}
}

// writableProfile writes a write-enabled named profile pointing at srv into
// the test's isolated config dir; commands select it with -p <name>, which
// outranks the GLM_INSTANCE env profile runGlm sets.
func writableProfile(t *testing.T, srv *httptest.Server, name string) {
	t.Helper()
	isolateConfig(t)
	f := &config.File{Profiles: map[string]config.Profile{
		name: {Instance: srv.URL, Auth: "basic", Username: "svc.glm", Writable: true},
	}}
	if err := f.Save(); err != nil {
		t.Fatalf("save config: %v", err)
	}
}

func TestAPINonGetRequiresYes(t *testing.T) {
	hits := map[string]int{}
	srv := fakeInstance(t, hits)
	writableProfile(t, srv, "w")

	_, stderr, err := runGlmErr(t, srv, "", "api", "DELETE", "/api/now/table/incident/"+sysIDa, "-p", "w")
	if err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("non-GET without --yes must refuse, got: %v", err)
	}
	if !strings.Contains(stderr, "DELETE "+srv.URL+"/api/now/table/incident/"+sysIDa) {
		t.Errorf("the request must be printed before refusing: %q", stderr)
	}
	// W7: the preview names who the write would run as, and where.
	if !strings.Contains(stderr, "as svc.glm @ ") || !strings.Contains(stderr, "(profile w)") {
		t.Errorf("the preview must name the acting identity: %q", stderr)
	}
	if hits["get"] != 0 {
		t.Errorf("refused request must never reach the instance, got %d hits", hits["get"])
	}

	stdout, _ := runGlm(t, srv, "", "api", "DELETE", "/api/now/table/incident/"+sysIDa, "-p", "w", "--yes")
	if hits["get"] != 1 {
		t.Errorf("confirmed request should execute once, got %d hits", hits["get"])
	}
	if !strings.Contains(stdout, "INC0000001") {
		t.Errorf("response detail view missing:\n%s", stdout)
	}
}

func TestAPINestedResultRendersAsJSON(t *testing.T) {
	hits := map[string]int{}
	srv := fakeInstance(t, hits)

	// Stats shape {"result":{"stats":{"count":"42"}}} — RecordDetail would
	// blank the nested map, so it must fall back to complete JSON.
	stdout, _ := runGlm(t, srv, "", "api", "GET", "/api/now/stats/incident")
	if !strings.Contains(stdout, `"count":"42"`) {
		t.Errorf("nested result object must survive verbatim:\n%s", stdout)
	}

	// Grouped stats: a result array of nested objects — same fallback.
	stdout, _ = runGlm(t, srv, "", "api", "GET", "/api/now/stats/incident", "-f", "sysparm_group_by=state")
	if !strings.Contains(stdout, "groupby_fields") || !strings.Contains(stdout, `"count":"5"`) {
		t.Errorf("nested result array must survive verbatim:\n%s", stdout)
	}
}

func TestAPIMachineOutputIsFaithful(t *testing.T) {
	hits := map[string]int{}
	srv := fakeInstance(t, hits)
	long := strings.Repeat("x", 2500)

	// Machine formats are a raw passthrough: no truncation marker injected
	// into a JSON value, with or without --full.
	for _, args := range [][]string{
		{"api", "GET", "/api/now/table/long_story", "--json"},
		{"api", "GET", "/api/now/table/long_story", "--format", "json"},
		{"api", "GET", "/api/now/table/long_story", "--json", "--full"},
	} {
		stdout, _ := runGlm(t, srv, "", args...)
		if !strings.Contains(stdout, long) {
			t.Errorf("%v: machine output must be faithful (no truncation):\n%.120s", args, stdout)
		}
		if strings.Contains(stdout, "use --full") {
			t.Errorf("%v: machine output must not inject a truncation marker into JSON", args)
		}
	}
}

func TestAPITrailingBytesPassThroughVerbatim(t *testing.T) {
	hits := map[string]int{}
	srv := fakeInstance(t, hits)

	// A first JSON value followed by appended bytes is not a clean
	// document; the whole body must survive, tail included.
	stdout, _ := runGlm(t, srv, "", "api", "GET", "/api/now/table/trailing")
	if !strings.Contains(stdout, "then junk") {
		t.Errorf("trailing bytes were dropped instead of passed through:\n%s", stdout)
	}
}

func TestAPIJSONFidelity(t *testing.T) {
	hits := map[string]int{}
	srv := fakeInstance(t, hits)

	// H-03: a raw passthrough must not change types, round large integers,
	// drop nulls, or discard fields alongside a value/display_value pair.
	for _, format := range []string{"json", "jsonl"} {
		stdout, _ := runGlm(t, srv, "", "api", "GET", "/api/now/table/fidelity", "--format", format)
		var got map[string]any
		dec := json.NewDecoder(strings.NewReader(stdout))
		dec.UseNumber()
		if err := dec.Decode(&got); err != nil {
			t.Fatalf("%s: not valid JSON: %v\n%s", format, err, stdout)
		}
		if n, ok := got["big"].(json.Number); !ok || n.String() != "9007199254740993" {
			t.Errorf("%s: large integer lost precision: %v", format, got["big"])
		}
		if b, ok := got["flag"].(bool); !ok || b != true {
			t.Errorf("%s: boolean coerced to string: %v", format, got["flag"])
		}
		if v, present := got["empty"]; !present || v != nil {
			t.Errorf("%s: null field dropped or altered: present=%v v=%v", format, present, v)
		}
		// A reference-shaped object carrying extra keys must keep them.
		ref, ok := got["ref"].(map[string]any)
		if !ok || ref["extra"] == nil {
			t.Errorf("%s: nested field alongside value/display_value was dropped: %v", format, got["ref"])
		}
	}
}

func TestAPINestedArrayHonorsJSONL(t *testing.T) {
	hits := map[string]int{}
	srv := fakeInstance(t, hits)

	stdout, _ := runGlm(t, srv, "", "api", "GET", "/api/now/stats/incident", "-f", "sysparm_group_by=state", "--json")
	lines := strings.Split(strings.TrimRight(stdout, "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("jsonl must emit one object per element, got %d lines:\n%s", len(lines), stdout)
	}
	for _, line := range lines {
		var obj map[string]any
		if err := json.Unmarshal([]byte(line), &obj); err != nil {
			t.Errorf("line is not a JSON object: %v (%s)", err, line)
		}
	}
}

func TestAPIEmptyResultHonorsMachineFormats(t *testing.T) {
	hits := map[string]int{}
	srv := fakeInstance(t, hits)

	// sys_ui_action serves an empty result array.
	stdout, _ := runGlm(t, srv, "", "api", "GET", "/api/now/table/sys_ui_action", "--format", "ids")
	if stdout != "" {
		t.Errorf("ids on an empty result must be an empty stream, got %q", stdout)
	}
	stdout, _ = runGlm(t, srv, "", "api", "GET", "/api/now/table/sys_ui_action", "--json")
	if stdout != "" {
		t.Errorf("jsonl on an empty result must be an empty stream, got %q", stdout)
	}
	stdout, _ = runGlm(t, srv, "", "api", "GET", "/api/now/table/sys_ui_action", "--format", "json")
	if strings.TrimSpace(stdout) != "[]" {
		t.Errorf("json on an empty result must be [], got %q", stdout)
	}
	stdout, stderr := runGlm(t, srv, "", "api", "GET", "/api/now/table/sys_ui_action", "--format", "table")
	if stdout != "" || !strings.Contains(stderr, "0 rows") {
		t.Errorf("empty table output should be silent stdout + 0 rows on stderr, got %q / %q", stdout, stderr)
	}
}

func TestAPIStdinBodyCapped(t *testing.T) {
	hits := map[string]int{}
	srv := fakeInstance(t, hits)
	writableProfile(t, srv, "w") // the gate would otherwise refuse before the body is read

	// An oversized piped body must be rejected before any request is sent.
	big := strings.Repeat("x", (8<<20)+1)
	_, _, err := runGlmErr(t, srv, big, "api", "POST", "/api/x", "--body", "@-", "-p", "w", "--yes")
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("oversized stdin body should be rejected, got: %v", err)
	}
}

func TestAPIRejectsBadInput(t *testing.T) {
	hits := map[string]int{}
	srv := fakeInstance(t, hits)

	if _, _, err := runGlmErr(t, srv, "", "api", "BREW", "/api/now/table/incident"); err == nil || !strings.Contains(err.Error(), "unsupported method") {
		t.Errorf("unknown method should fail, got: %v", err)
	}
	writableProfile(t, srv, "w") // past the gate, so the body path is what fails
	if _, _, err := runGlmErr(t, srv, "", "api", "POST", "/x", "--body", "{not json", "-p", "w", "--yes"); err == nil || !strings.Contains(err.Error(), "valid JSON") {
		t.Errorf("invalid body should fail before any request, got: %v", err)
	}
	if _, _, err := runGlmErr(t, srv, "", "api", "GET", "/x", "-f", "noequals"); err == nil || !strings.Contains(err.Error(), "k=v") {
		t.Errorf("malformed -f should fail, got: %v", err)
	}
}

func TestWindowsBOMTolerance(t *testing.T) {
	hits := map[string]int{}
	srv := fakeInstance(t, hits)
	bom := string(rune(0xFEFF))

	// PowerShell pipes between native executables prepend a UTF-8 BOM to
	// the stream — the first stdin key must still resolve.
	stdout, _ := runGlm(t, srv, bom+sysIDa+"\n", "get", "incident", "-")
	if !strings.Contains(stdout, "INC0000001") {
		t.Errorf("BOM-prefixed stdin key must resolve:\n%s", stdout)
	}

	// Same stream shape for a piped --body payload: on a writable profile
	// (past the W1 gate) without --yes, a clean body is read, BOM-stripped,
	// and JSON-validated, then the confirm gate refuses — so a "--yes"
	// error proves BOM tolerance; a corrupted body would say "valid JSON".
	writableProfile(t, srv, "w")
	if _, _, err := runGlmErr(t, srv, bom+`{"short_description":"x"}`, "api", "POST", "/api/now/table/incident", "--body", "@-", "-p", "w"); err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Errorf("BOM-prefixed stdin body must survive validation and stop at the confirm gate, got: %v", err)
	}
}

func TestPrimeListsEveryCommand(t *testing.T) {
	hits := map[string]int{}
	srv := fakeInstance(t, hits)

	stdout, _ := runGlm(t, srv, "", "prime")
	for _, want := range []string{
		"glm query <table>",
		"glm grep <pattern>",
		"glm attach list <table>",
		"glm attach get <sys_id>",
		"glm api <METHOD> <path>",
		"glm profile add <name>",
		"encoded queries",
		"--format ids",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("prime output missing %q:\n%s", want, stdout)
		}
	}
	// The whole point is a small prompt: keep prime bounded (~700 tokens —
	// raised from ~650 when the write-gate profile commands joined the
	// surface; the economy block pays for itself many times over).
	if len(stdout) > 2900 {
		t.Errorf("prime output is %d chars — blowing the token budget", len(stdout))
	}
	if !strings.Contains(stdout, "Economy:") || !strings.Contains(stdout, "count/agg before listing") {
		t.Errorf("prime must teach the economy conventions:\n%s", stdout)
	}
	if hits["schema"]+hits["list"]+hits["stats"] != 0 {
		t.Errorf("prime must not touch the network: %v", hits)
	}
}
