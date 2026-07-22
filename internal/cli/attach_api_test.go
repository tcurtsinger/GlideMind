package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
	t.Chdir(t.TempDir())
	runGlm(t, srv, "", "attach", "get", sysIDc)
	if _, _, err := runGlmErr(t, srv, "", "attach", "get", sysIDc); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Errorf("derived-name overwrite must be refused, got: %v", err)
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

func TestAPINonGetRequiresYes(t *testing.T) {
	hits := map[string]int{}
	srv := fakeInstance(t, hits)

	_, stderr, err := runGlmErr(t, srv, "", "api", "DELETE", "/api/now/table/incident/"+sysIDa)
	if err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("non-GET without --yes must refuse, got: %v", err)
	}
	if !strings.Contains(stderr, "DELETE "+srv.URL+"/api/now/table/incident/"+sysIDa) {
		t.Errorf("the request must be printed before refusing: %q", stderr)
	}
	if hits["get"] != 0 {
		t.Errorf("refused request must never reach the instance, got %d hits", hits["get"])
	}

	stdout, _ := runGlm(t, srv, "", "api", "DELETE", "/api/now/table/incident/"+sysIDa, "--yes")
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

func TestAPIFullLiftsTruncation(t *testing.T) {
	hits := map[string]int{}
	srv := fakeInstance(t, hits)
	long := strings.Repeat("x", 2500)

	stdout, _ := runGlm(t, srv, "", "api", "GET", "/api/now/table/long_story", "--json")
	if strings.Contains(stdout, long) || !strings.Contains(stdout, "use --full") {
		t.Errorf("default output should truncate with the --full remedy:\n%.200s", stdout)
	}
	stdout, _ = runGlm(t, srv, "", "api", "GET", "/api/now/table/long_story", "--json", "--full")
	if !strings.Contains(stdout, long) {
		t.Errorf("--full must lift truncation:\n%.200s", stdout)
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

func TestAPIRejectsBadInput(t *testing.T) {
	hits := map[string]int{}
	srv := fakeInstance(t, hits)

	if _, _, err := runGlmErr(t, srv, "", "api", "BREW", "/api/now/table/incident"); err == nil || !strings.Contains(err.Error(), "unsupported method") {
		t.Errorf("unknown method should fail, got: %v", err)
	}
	if _, _, err := runGlmErr(t, srv, "", "api", "POST", "/x", "--body", "{not json", "--yes"); err == nil || !strings.Contains(err.Error(), "valid JSON") {
		t.Errorf("invalid body should fail before any request, got: %v", err)
	}
	if _, _, err := runGlmErr(t, srv, "", "api", "GET", "/x", "-f", "noequals"); err == nil || !strings.Contains(err.Error(), "k=v") {
		t.Errorf("malformed -f should fail, got: %v", err)
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
	// The whole point is a small prompt: keep it near the 400-token budget.
	if len(stdout) > 2400 {
		t.Errorf("prime output is %d chars — blowing the ~400-token budget", len(stdout))
	}
	if hits["schema"]+hits["list"]+hits["stats"] != 0 {
		t.Errorf("prime must not touch the network: %v", hits)
	}
}
