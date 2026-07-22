package schema

import (
	"fmt"
	"sort"
	"strings"
)

// Validate checks field names against the table's dictionary and fails with
// a did-you-mean error on the first unknown — the ServiceNow API silently
// returns empty strings for nonexistent fields, so typos must die before the
// request. Dot-walks are checked on their first segment only (later hops
// live on other tables); sys_* names are always accepted (base bookkeeping
// fields exist everywhere and dictionaries are sometimes ACL-filtered).
func (m *TableMeta) Validate(names []string) error {
	for _, name := range names {
		first, _, _ := strings.Cut(name, ".")
		if first == "" || strings.HasPrefix(first, "sys_") {
			continue
		}
		if _, ok := m.Fields[first]; ok {
			continue
		}
		msg := fmt.Sprintf("unknown field %q on %s", first, m.Name)
		if suggestions := m.Suggest(first); len(suggestions) > 0 {
			msg += " — did you mean " + quoteJoin(suggestions) + "?"
		}
		return fmt.Errorf("%s (see: glm schema %s)", msg, m.Name)
	}
	return nil
}

// Suggest returns up to three close field names: edit distance ≤ 2 first,
// substring matches as a fallback.
func (m *TableMeta) Suggest(name string) []string {
	type candidate struct {
		name string
		dist int
	}
	var candidates []candidate
	for f := range m.Fields {
		if d := levenshtein(name, f); d <= 2 {
			candidates = append(candidates, candidate{f, d})
		}
	}
	if len(candidates) == 0 {
		for f := range m.Fields {
			if strings.Contains(f, name) || (len(name) > 3 && strings.Contains(name, f)) {
				candidates = append(candidates, candidate{f, 3})
			}
		}
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].dist != candidates[j].dist {
			return candidates[i].dist < candidates[j].dist
		}
		return candidates[i].name < candidates[j].name
	})
	if len(candidates) > 3 {
		candidates = candidates[:3]
	}
	names := make([]string, len(candidates))
	for i, c := range candidates {
		names[i] = c.name
	}
	return names
}

// ExtractQueryFields pulls the field names out of an encoded query that can
// be identified with confidence; anything ambiguous is skipped rather than
// guessed (validation is best-effort — a false error would block a valid
// query, a skip merely misses a typo).
func ExtractQueryFields(encoded string) []string {
	var names []string
	seen := map[string]bool{}
	rlDepth := 0
	for _, clause := range strings.Split(encoded, "^") {
		// Related-list scopes (RLQUERY...ENDRLQUERY) filter on a CHILD
		// table — their inner clauses must not be validated against the
		// outer table.
		if strings.HasPrefix(clause, "RLQUERY") {
			rlDepth++
			continue
		}
		if strings.HasPrefix(clause, "ENDRLQUERY") {
			if rlDepth > 0 {
				rlDepth--
			}
			continue
		}
		if rlDepth > 0 {
			continue
		}
		// Strip one leading uppercase marker: clause joiners (OR, NQ, EQ)
		// keep a field behind them; ORDERBY/GROUPBY are followed by exactly
		// a field name.
		for _, prefix := range []string{"ORDERBYDESC", "ORDERBY", "GROUPBY", "NQ", "EQ", "OR"} {
			if strings.HasPrefix(clause, prefix) {
				clause = clause[len(prefix):]
				break
			}
		}
		token := leadingFieldToken(clause)
		if token == "" || token == "javascript" || !strings.ContainsFunc(token, isLower) {
			continue
		}
		if !seen[token] {
			seen[token] = true
			names = append(names, token)
		}
	}
	return names
}

// leadingFieldToken reads the run of field-name runes (lowercase, digits,
// underscore, dot) that starts a clause; ServiceNow element names are
// lowercase, and operators (=, !=, LIKE, ISEMPTY, ...) terminate the run.
func leadingFieldToken(clause string) string {
	end := 0
	for end < len(clause) {
		c := clause[end]
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '_' || c == '.' {
			end++
			continue
		}
		break
	}
	token := clause[:end]
	if token == "" || !isLower(rune(token[0])) {
		return ""
	}
	return token
}

func isLower(r rune) bool { return r >= 'a' && r <= 'z' }

func quoteJoin(names []string) string {
	quoted := make([]string, len(names))
	for i, n := range names {
		quoted[i] = fmt.Sprintf("%q", n)
	}
	return strings.Join(quoted, ", ")
}

// levenshtein is the classic edit distance, small inputs only.
func levenshtein(a, b string) int {
	if a == b {
		return 0
	}
	ra, rb := []rune(a), []rune(b)
	prev := make([]int, len(rb)+1)
	cur := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		cur[0] = i
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, min(cur[j-1]+1, prev[j-1]+cost))
		}
		prev, cur = cur, prev
	}
	return prev[len(rb)]
}
