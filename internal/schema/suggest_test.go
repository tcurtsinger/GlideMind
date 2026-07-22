package schema

import (
	"strings"
	"testing"
)

func testMeta() *TableMeta {
	return &TableMeta{
		Name: "incident",
		Fields: map[string]Field{
			"state": {}, "priority": {}, "short_description": {},
			"assigned_to": {Type: "reference", Reference: "sys_user"},
			"number":      {},
		},
	}
}

func TestValidateAccepts(t *testing.T) {
	m := testMeta()
	names := []string{"state", "assigned_to.manager.name", "sys_created_on", ""}
	if err := m.Validate(names); err != nil {
		t.Fatalf("valid names rejected: %v", err)
	}
}

func TestValidateDidYouMean(t *testing.T) {
	m := testMeta()
	err := m.Validate([]string{"priorty"})
	if err == nil {
		t.Fatal("typo should fail")
	}
	msg := err.Error()
	if !strings.Contains(msg, `"priority"`) || !strings.Contains(msg, "glm schema incident") {
		t.Errorf("message should suggest and point at schema cmd: %q", msg)
	}
}

func TestSuggestSubstringFallback(t *testing.T) {
	m := testMeta()
	got := m.Suggest("descr")
	if len(got) == 0 || got[0] != "short_description" {
		t.Errorf("substring fallback failed: %v", got)
	}
}

func TestExtractQueryFields(t *testing.T) {
	cases := []struct {
		in   string
		want string // comma-joined
	}{
		{"active=true^priority=1", "active,priority"},
		{"stateIN1,2^ORDERBYDESCsys_created_on", "state,sys_created_on"},
		{"state=1^ORpriority=2", "state,priority"},
		{"short_descriptionISEMPTY", "short_description"},
		{"NQstate=3", "state"},
		{"state=1^state=2", "state"},
		{"123bogus=1", ""},
		{"GOTOnumber=INC0000001", ""},
		{"javascript:gs.now()", ""},
		{"sys_created_on>=javascript:gs.minutesAgoStart(15)", "sys_created_on"},
		// RLQUERY scopes filter a child table — inner clauses are skipped,
		// outer clauses on both sides still validate.
		{"RLQUERYtask_sla.task,>=1^has_breached=true^ENDRLQUERY", ""},
		{"active=true^RLQUERYtask_sla.task,>=1^has_breached=true^ENDRLQUERY^priority=1", "active,priority"},
		{"", ""},
	}
	for _, tc := range cases {
		got := strings.Join(ExtractQueryFields(tc.in), ",")
		if got != tc.want {
			t.Errorf("ExtractQueryFields(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestLevenshtein(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"", "abc", 3},
		{"kitten", "sitting", 3},
		{"stat", "state", 1},
		{"same", "same", 0},
	}
	for _, tc := range cases {
		if got := levenshtein(tc.a, tc.b); got != tc.want {
			t.Errorf("levenshtein(%q, %q) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}
