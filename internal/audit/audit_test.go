package audit

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestPathEnvOverride(t *testing.T) {
	t.Setenv(EnvLogPath, `C:\elsewhere\audit.jsonl`)
	p, err := Path()
	if err != nil || p != `C:\elsewhere\audit.jsonl` {
		t.Fatalf("env override not honored: %q, %v", p, err)
	}
}

func TestAppendRoundTrip(t *testing.T) {
	p := filepath.Join(t.TempDir(), "nested", "audit.jsonl")
	t.Setenv(EnvLogPath, p)

	entries := []Entry{
		{Time: time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC), Instance: "https://a", Profile: "dev", User: "u", Command: "api", Method: "DELETE", Target: "/api/x", Result: "ok"},
		{Time: time.Date(2026, 7, 23, 12, 1, 0, 0, time.UTC), Instance: "https://a", Profile: "dev", User: "u", Command: "api", Method: "PATCH", Target: "/api/y", Fields: []string{"a", "b"}, Result: "error"},
	}
	for _, e := range entries {
		if err := Append(e); err != nil {
			t.Fatalf("append: %v", err)
		}
	}

	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 {
		t.Fatalf("want 2 lines, got %d", len(lines))
	}
	var got Entry
	if err := json.Unmarshal([]byte(lines[1]), &got); err != nil {
		t.Fatalf("line 2 not valid JSON: %v", err)
	}
	if !reflect.DeepEqual(got, entries[1]) {
		t.Errorf("round trip mismatch:\n got %+v\nwant %+v", got, entries[1])
	}
}

func TestBodyFieldNames(t *testing.T) {
	cases := []struct {
		body string
		want []string
	}{
		{`{"state":"6","close_notes":"x","assigned_to":"y"}`, []string{"assigned_to", "close_notes", "state"}},
		{`{}`, []string{}},
		{`[1,2]`, nil},    // non-object: method+target still identify the write
		{`"scalar"`, nil}, //
		{`{broken`, nil},  // invalid JSON never reaches the wire anyway
		{``, nil},
	}
	for _, c := range cases {
		if got := BodyFieldNames([]byte(c.body)); !reflect.DeepEqual(got, c.want) {
			t.Errorf("BodyFieldNames(%q) = %v, want %v", c.body, got, c.want)
		}
	}
}
