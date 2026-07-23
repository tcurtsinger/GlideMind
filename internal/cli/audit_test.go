package cli

import (
	"strings"
	"testing"
)

func TestNumericBoundsRejected(t *testing.T) {
	hits := map[string]int{}
	srv := fakeInstance(t, hits)

	cases := [][]string{
		{"query", "incident", "--limit", "0"},
		{"query", "incident", "--offset", "-1"},
		{"grep", "x", "--max-matches", "0"},
		{"grep", "x", "--limit", "0"},
		{"tables", "--limit", "0"},
		{"attach", "list", "incident", sysIDa, "--limit", "0"},
	}
	for _, args := range cases {
		if _, _, err := runGlmErr(t, srv, "", args...); err == nil {
			t.Errorf("%v: out-of-range numeric flag should be rejected", args)
		}
	}
	// A bad bound must fail before any network work.
	if hits["list"]+hits["grep"]+hits["schema"] != 0 {
		t.Errorf("bounds must be checked before the network: %v", hits)
	}
}

func TestSinceOverflowRejected(t *testing.T) {
	hits := map[string]int{}
	srv := fakeInstance(t, hits)

	if _, _, err := runGlmErr(t, srv, "", "query", "incident", "--since", "99999999999d"); err == nil {
		t.Error("an absurd --since day count should be rejected, not overflow")
	}
}

func TestQueryAppendsStableSort(t *testing.T) {
	hits := map[string]int{}
	srv := fakeInstance(t, hits)

	runGlm(t, srv, "", "query", "incident")
	if hits["stable-sort"] == 0 {
		t.Error("every query must append ORDERBYsys_id for deterministic pagination")
	}
}

func TestSchemaIDsFormatFails(t *testing.T) {
	hits := map[string]int{}
	srv := fakeInstance(t, hits)

	// schema rows are synthesized and carry no sys_id; --format ids must
	// fail loudly instead of printing blank lines.
	if _, _, err := runGlmErr(t, srv, "", "schema", "incident", "--format", "ids"); err == nil {
		t.Error("schema --format ids should error, not emit blank lines")
	}
}

func TestWhoamiPaginatesRoles(t *testing.T) {
	hits := map[string]int{}
	srv := fakeInstance(t, hits)

	stdout, _ := runGlm(t, srv, "", "whoami")
	// 250 grants exist across two pages; a single 200 window would undercount.
	if !strings.Contains(stdout, "(250)") {
		t.Errorf("whoami must paginate all roles, got:\n%s", stdout)
	}
	if hits["whoami-roles"] < 2 {
		t.Errorf("expected at least two role pages, got %d", hits["whoami-roles"])
	}
}
