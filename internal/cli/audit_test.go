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

func TestAPIPreviewMatchesSentQuery(t *testing.T) {
	hits := map[string]int{}
	srv := fakeInstance(t, hits)
	writableProfile(t, srv, "w")

	// The path carries its own query and -f adds another param. The preview
	// must show the merged URL (one "?"), matching what Raw would send.
	_, stderr, err := runGlmErr(t, srv, "", "api", "POST", "/api/x?a=1", "-f", "b=2", "-p", "w")
	if err == nil {
		t.Fatal("non-GET without --yes must refuse")
	}
	if !strings.Contains(stderr, "a=1") || !strings.Contains(stderr, "b=2") {
		t.Errorf("preview must include the merged query: %q", stderr)
	}
	if strings.Count(stderr, "?") != 1 {
		t.Errorf("preview must have exactly one query separator, got: %q", stderr)
	}
}

func TestValidationSelfHealsStaleCache(t *testing.T) {
	hits := map[string]int{}
	srv := fakeInstance(t, hits)

	// Warm the cache — this first schema fetch has no "tier".
	runGlm(t, srv, "", "query", "evolving")
	// A query on the just-created field must not be falsely blocked: glm
	// refetches the schema once, sees tier, and proceeds.
	if _, _, err := runGlmErr(t, srv, "", "query", "evolving", "-q", "tierISNOTEMPTY"); err != nil {
		t.Errorf("validation must self-heal for a field added after caching, got: %v", err)
	}
	// A genuine unknown field still fails after the refetch confirms it.
	if _, _, err := runGlmErr(t, srv, "", "query", "evolving", "-q", "nosuchfield=1"); err == nil {
		t.Error("a real unknown field should still be rejected after refetch")
	}
}

func TestQueryColumnHint(t *testing.T) {
	hits := map[string]int{}
	srv := fakeInstance(t, hits)

	// Derived columns (no --fields): stderr says how many of the table's
	// fields are shown so an omitted column is never a silent surprise.
	_, stderr := runGlm(t, srv, "", "query", "incident")
	if !strings.Contains(stderr, "columns:") || !strings.Contains(stderr, "--fields") {
		t.Errorf("expected a column-count hint on stderr, got: %q", stderr)
	}
	// Explicit --fields: the caller chose, so no hint.
	_, stderr = runGlm(t, srv, "", "query", "incident", "--fields", "number")
	if strings.Contains(stderr, "columns:") {
		t.Errorf("no column hint when --fields is explicit, got: %q", stderr)
	}
}

func TestSchemaStampsCacheAge(t *testing.T) {
	hits := map[string]int{}
	srv := fakeInstance(t, hits)

	_, stderr := runGlm(t, srv, "", "schema", "incident")
	if !strings.Contains(stderr, "fetched live") {
		t.Errorf("first schema read should report a live fetch, got: %q", stderr)
	}
	_, stderr = runGlm(t, srv, "", "schema", "incident")
	if !strings.Contains(stderr, "cached") || !strings.Contains(stderr, "--refresh") {
		t.Errorf("second schema read should report cache age + --refresh, got: %q", stderr)
	}
}

func TestWhoamiPaginatesRoles(t *testing.T) {
	hits := map[string]int{}
	srv := fakeInstance(t, hits)

	// 250 grants exist across two pages; a single 200 window would undercount.
	// Default output previews a few and names the true total.
	stdout, _ := runGlm(t, srv, "", "whoami")
	if !strings.Contains(stdout, "250 total") || !strings.Contains(stdout, "--full for all") {
		t.Errorf("whoami should preview roles and name the paginated total, got:\n%s", stdout)
	}
	if hits["whoami-roles"] < 2 {
		t.Errorf("expected at least two role pages, got %d", hits["whoami-roles"])
	}
	// --full lists every role (exact count, no preview marker).
	stdout, _ = runGlm(t, srv, "", "whoami", "--full")
	if !strings.Contains(stdout, "(250)") || strings.Contains(stdout, "more (") {
		t.Errorf("whoami --full must list all roles, got:\n%s", stdout)
	}
	if !strings.Contains(stdout, "role_249") {
		t.Errorf("whoami --full must include roles past the preview window")
	}
}
