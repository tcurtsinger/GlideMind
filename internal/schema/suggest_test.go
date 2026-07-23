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
			"sys_id":      {}, // completeness sentinel — validation requires it
		},
	}
}

func TestValidateSkipsIncompleteDictionary(t *testing.T) {
	// A dictionary without the sys_id row is ACL-filtered or partial;
	// validation must skip rather than reject fields it cannot see.
	m := &TableMeta{Name: "incident", Fields: map[string]Field{"state": {}}}
	if err := m.Validate([]string{"definitely_not_visible"}); err != nil {
		t.Fatalf("partial dictionary must not fail validation: %v", err)
	}
	empty := &TableMeta{Name: "incident", Fields: map[string]Field{}}
	if err := empty.Validate([]string{"anything"}); err != nil {
		t.Fatalf("empty dictionary must not fail validation: %v", err)
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

func TestValidateStrictChecksSysFields(t *testing.T) {
	// On the write path (DESIGN-WRITES.md W3) a sys_-prefixed typo is a
	// genuine unknown field, not a free pass: the lenient read bypass would
	// let it through and ServiceNow would silently drop it on the PATCH.
	m := &TableMeta{
		Name: "task",
		Fields: map[string]Field{
			"state":          {},
			"sys_id":         {}, // completeness sentinel
			"sys_updated_on": {},
			"sys_mod_count":  {},
		},
	}
	// A real system field still passes strict validation.
	if err := m.ValidateStrict([]string{"state", "sys_updated_on"}); err != nil {
		t.Fatalf("real sys_ field rejected on write path: %v", err)
	}
	// A sys_ typo is caught, with a did-you-mean pointing at the real field.
	err := m.ValidateStrict([]string{"sys_update_on"})
	if err == nil {
		t.Fatal("sys_ typo must fail strict validation")
	}
	if !strings.Contains(err.Error(), `"sys_updated_on"`) {
		t.Errorf("strict error should suggest the real field: %q", err)
	}
	// The lenient read path still accepts the same typo (unchanged behavior).
	if err := m.Validate([]string{"sys_update_on"}); err != nil {
		t.Fatalf("read path must keep the sys_ bypass: %v", err)
	}
	// The ACL-filtered guard is shared: a partial dictionary (no sys_id row)
	// cannot prove a field wrong, so strict validation still skips it.
	partial := &TableMeta{Name: "task", Fields: map[string]Field{"state": {}}}
	if err := partial.ValidateStrict([]string{"sys_update_on", "whatever"}); err != nil {
		t.Fatalf("strict must skip an incomplete dictionary: %v", err)
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
