package output

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

var recs = []map[string]any{
	{"number": "INC0000001", "short_description": "Printer on fire", "sys_id": "abc123"},
	{"number": "INC0000002", "short_description": map[string]any{"display_value": "Café down", "link": "x"}, "sys_id": "def456"},
}

func TestHumanFormatsSanitizeTerminalControls(t *testing.T) {
	// A value carrying an ESC/OSC sequence must not reach the terminal
	// verbatim in any human format.
	evil := []map[string]any{{"number": "INC1", "short_description": "safe\x1b]0;pwned\x07evil\x1b[2J", "sys_id": "s1"}}
	for _, format := range []string{"table", "tsv", "csv"} {
		out := render(t, format, []string{"number", "short_description"}, evil, false)
		if strings.ContainsRune(out, 0x1b) || strings.ContainsRune(out, 0x07) {
			t.Errorf("%s: control characters survived into output: %q", format, out)
		}
		if !strings.Contains(out, "safe") || !strings.Contains(out, "evil") {
			t.Errorf("%s: legitimate text was lost: %q", format, out)
		}
	}
}

func TestSanitizeLineFlattensAndStrips(t *testing.T) {
	// Single-line status fields (grep names/lines, attachment summaries) must
	// neutralize newlines and CR too, not just other control chars — an
	// embedded \n injects a line, \r returns to column 0.
	got := SanitizeLine("name\r\nFAKE: injected\x1b[2Kline")
	if strings.ContainsAny(got, "\r\n\t") {
		t.Errorf("newline/CR/tab must be folded to spaces: %q", got)
	}
	if strings.ContainsRune(got, 0x1b) {
		t.Errorf("control chars must be stripped: %q", got)
	}
	if !strings.Contains(got, "name") || !strings.Contains(got, "injected") {
		t.Errorf("legitimate text must survive: %q", got)
	}
}

func TestMachineFormatsStayLossless(t *testing.T) {
	// json/jsonl must not sanitize: control bytes are encoded (not replaced
	// with U+FFFD), so they round-trip byte-for-byte.
	raw := "tab\tand\x1bescape"
	evil := []map[string]any{{"number": "INC1", "note": raw, "sys_id": "s1"}}
	out := render(t, "jsonl", []string{"number", "note"}, evil, true)
	var got map[string]string
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &got); err != nil {
		t.Fatalf("jsonl not valid JSON: %v (%q)", err, out)
	}
	if got["note"] != raw {
		t.Errorf("control byte did not round-trip through jsonl: %q", got["note"])
	}
	if strings.ContainsRune(out, '�') {
		t.Errorf("machine format must not sanitize (no U+FFFD): %q", out)
	}
}

func render(t *testing.T, format string, fields []string, in []map[string]any, full bool) string {
	t.Helper()
	var buf bytes.Buffer
	if err := Records(&buf, fields, in, Options{Format: format, Full: full}); err != nil {
		t.Fatalf("render %s: %v", format, err)
	}
	return buf.String()
}

func TestTableAlignsAndHeaders(t *testing.T) {
	got := render(t, "table", []string{"number", "short_description"}, recs, false)
	lines := strings.Split(strings.TrimRight(got, "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("want header + 2 rows, got %d lines:\n%s", len(lines), got)
	}
	if !strings.HasPrefix(lines[0], "number      short_description") {
		t.Errorf("header misaligned: %q", lines[0])
	}
	if !strings.Contains(got, "Café down") {
		t.Errorf("reference object display_value not extracted:\n%s", got)
	}
}

func TestTSVIsTabSeparatedWithoutPadding(t *testing.T) {
	got := render(t, "tsv", []string{"number", "short_description"}, recs, false)
	if !strings.Contains(got, "INC0000001\tPrinter on fire") {
		t.Errorf("tsv row wrong:\n%s", got)
	}
}

func TestIDsFormat(t *testing.T) {
	got := render(t, "ids", []string{"number"}, recs, false)
	if got != "abc123\ndef456\n" {
		t.Errorf("ids output = %q", got)
	}
}

func TestJSONLCarriesSysID(t *testing.T) {
	got := render(t, "jsonl", []string{"number"}, recs, false)
	lines := strings.Split(strings.TrimRight(got, "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("want 2 jsonl lines, got %d", len(lines))
	}
	if !strings.Contains(lines[0], `"sys_id":"abc123"`) {
		t.Errorf("jsonl must include sys_id for chaining: %s", lines[0])
	}
}

func TestCellTruncationMarksAndFullLifts(t *testing.T) {
	long := strings.Repeat("x", CellMax+50)
	in := []map[string]any{{"description": long, "sys_id": "a"}}

	got := render(t, "tsv", []string{"description"}, in, false)
	if !strings.Contains(got, "…") {
		t.Errorf("expected truncation marker:\n%s", got)
	}
	if strings.Contains(got, long) {
		t.Errorf("value should have been truncated")
	}

	got = render(t, "tsv", []string{"description"}, in, true)
	if !strings.Contains(got, long) {
		t.Errorf("--full should lift truncation")
	}
}

func TestFieldTruncationMarkerNamesRemedy(t *testing.T) {
	long := strings.Repeat("y", FieldMax+7)
	got := TruncateField(long, false)
	if !strings.Contains(got, "+7 chars") || !strings.Contains(got, "--full") {
		t.Errorf("marker should count the remainder and name the remedy: %q", got[len(got)-60:])
	}
	if TruncateField(long, true) != long {
		t.Errorf("full should lift the cap")
	}
}

func TestTabularCellsStayOneLine(t *testing.T) {
	in := []map[string]any{{"description": "line1\nline2\ttabbed", "sys_id": "a"}}
	got := render(t, "tsv", []string{"description"}, in, false)
	if strings.Contains(got, "line1\nline2") || strings.Contains(got, "\ttabbed") {
		t.Errorf("newlines/tabs must be flattened in tabular cells: %q", got)
	}
}

func TestRecordDetailOmitsEmptyAndGroupsSysFields(t *testing.T) {
	rec := map[string]any{
		"number":        "INC0000001",
		"description":   "broken",
		"empty_one":     "",
		"sys_id":        "abc",
		"sys_mod_count": "4",
	}
	var buf bytes.Buffer
	if err := RecordDetail(&buf, rec, nil, Options{Format: "table"}); err != nil {
		t.Fatalf("detail: %v", err)
	}
	got := buf.String()
	if strings.Contains(got, "empty_one") {
		t.Errorf("empty fields must be omitted:\n%s", got)
	}
	numberAt := strings.Index(got, "number")
	sysAt := strings.Index(got, "sys_id")
	if numberAt == -1 || sysAt == -1 || numberAt > sysAt {
		t.Errorf("regular fields should precede sys_* fields:\n%s", got)
	}
}

func TestRecordDetailDelimitedFormats(t *testing.T) {
	rec := map[string]any{
		"number":    "INC0000001",
		"state":     "In Progress",
		"empty_one": "",
		"sys_id":    "abc",
	}
	var buf bytes.Buffer
	if err := RecordDetail(&buf, rec, nil, Options{Format: "tsv"}); err != nil {
		t.Fatalf("detail tsv: %v", err)
	}
	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("tsv detail should be header + one row, got:\n%s", buf.String())
	}
	if lines[0] != "number\tstate\tsys_id" {
		t.Errorf("tsv header = %q (non-empty only, regular before sys_*)", lines[0])
	}
	if lines[1] != "INC0000001\tIn Progress\tabc" {
		t.Errorf("tsv row = %q", lines[1])
	}

	buf.Reset()
	if err := RecordDetail(&buf, rec, nil, Options{Format: "csv"}); err != nil {
		t.Fatalf("detail csv: %v", err)
	}
	if !strings.HasPrefix(buf.String(), "number,state,sys_id\n") {
		t.Errorf("csv detail header wrong:\n%s", buf.String())
	}
}

func TestRecordDetailExplicitFieldsExactSchema(t *testing.T) {
	rec := map[string]any{
		"number": "INC0000001", "state": "In Progress", "note": "", "sys_id": "abc",
	}
	var buf bytes.Buffer
	if err := RecordDetail(&buf, rec, []string{"number", "note"}, Options{Format: "tsv"}); err != nil {
		t.Fatalf("detail: %v", err)
	}
	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if lines[0] != "number\tnote" {
		t.Errorf("explicit fields must define the schema exactly (no sys_id, requested order): %q", lines[0])
	}
	if lines[1] != "INC0000001\t" {
		t.Errorf("explicitly requested empty fields stay as empty cells: %q", lines[1])
	}

	// json keeps sys_id for chaining even with explicit fields.
	buf.Reset()
	if err := RecordDetail(&buf, rec, []string{"number"}, Options{Format: "jsonl"}); err != nil {
		t.Fatalf("detail jsonl: %v", err)
	}
	if !strings.Contains(buf.String(), `"sys_id":"abc"`) {
		t.Errorf("machine formats must keep sys_id: %s", buf.String())
	}
}

func TestUnknownFormatErrors(t *testing.T) {
	if err := Records(&bytes.Buffer{}, []string{"a"}, nil, Options{Format: "yaml"}); err == nil {
		t.Fatal("unknown format should error")
	}
}
