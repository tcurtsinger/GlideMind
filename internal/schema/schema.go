// Package schema derives table metadata from the instance dictionary: the
// inheritance chain, the display field, and the zero-config default field set
// (DESIGN.md §5). Lookups are live per invocation today; the on-disk cache
// and did-you-mean field validation build on this package next.
package schema

import (
	"context"
	"fmt"
	"net/url"
	"sort"
	"strings"

	"github.com/tcurtsinger/GlideMind/internal/exit"
	"github.com/tcurtsinger/GlideMind/internal/snow"
)

const (
	maxHierarchyDepth = 10
	maxDefaultFields  = 7
)

// NotFoundError reports a table that does not exist on the instance.
type NotFoundError struct{ Table string }

func (e *NotFoundError) Error() string {
	return fmt.Sprintf("table %q not found on the instance (find it with: glm tables <pattern>)", e.Table)
}
func (e *NotFoundError) ExitCode() int { return exit.NotFound }

// Field is one dictionary entry.
type Field struct {
	Type      string
	Reference string // referenced table, when Type is "reference"
}

// TableMeta is the derived metadata for one table, fields inherited included.
type TableMeta struct {
	Name         string
	Chain        []string // the table itself, then its ancestors
	DisplayField string
	Fields       map[string]Field
}

// Fetch walks the inheritance chain via sys_db_object, then loads
// sys_dictionary for every table in the chain.
func Fetch(ctx context.Context, c *snow.Client, table string) (*TableMeta, error) {
	var chain []string
	cur := table
	for depth := 0; cur != "" && depth < maxHierarchyDepth; depth++ {
		q := url.Values{}
		q.Set("sysparm_query", "name="+cur)
		q.Set("sysparm_fields", "name,super_class.name")
		q.Set("sysparm_limit", "1")
		q.Set("sysparm_display_value", "false")
		q.Set("sysparm_exclude_reference_link", "true")
		rows, err := c.Table(ctx, "sys_db_object", q)
		if err != nil {
			return nil, fmt.Errorf("resolve table %q: %w", cur, err)
		}
		if len(rows) == 0 {
			if depth == 0 {
				return nil, &NotFoundError{Table: table}
			}
			break
		}
		chain = append(chain, cur)
		cur = value(rows[0], "super_class.name")
	}

	meta := &TableMeta{Name: table, Chain: chain, Fields: map[string]Field{}}

	q := url.Values{}
	q.Set("sysparm_query", "nameIN"+strings.Join(chain, ",")+"^elementISNOTEMPTY")
	q.Set("sysparm_fields", "name,element,internal_type,display,reference.name")
	q.Set("sysparm_limit", "2000")
	q.Set("sysparm_display_value", "false")
	q.Set("sysparm_exclude_reference_link", "true")
	rows, err := c.Table(ctx, "sys_dictionary", q)
	if err != nil {
		return nil, fmt.Errorf("load dictionary for %q: %w", table, err)
	}

	displayByTable := map[string]string{}
	for _, r := range rows {
		element := value(r, "element")
		if element == "" {
			continue
		}
		// The most-derived definition of an element wins; dictionary rows for
		// the table itself shadow inherited ones only for display selection,
		// so first-seen is fine for the field map.
		if _, seen := meta.Fields[element]; !seen {
			meta.Fields[element] = Field{
				Type:      value(r, "internal_type"),
				Reference: value(r, "reference.name"),
			}
		}
		if value(r, "display") == "true" {
			t := value(r, "name")
			if _, seen := displayByTable[t]; !seen {
				displayByTable[t] = element
			}
		}
	}

	// Chain is ordered most-derived first, so the first table with an
	// explicit display field wins.
	for _, t := range chain {
		if d, ok := displayByTable[t]; ok {
			meta.DisplayField = d
			break
		}
	}
	if meta.DisplayField == "" {
		for _, candidate := range []string{"number", "name", "short_description"} {
			if _, ok := meta.Fields[candidate]; ok {
				meta.DisplayField = candidate
				break
			}
		}
	}
	if meta.DisplayField == "" {
		meta.DisplayField = "sys_id"
	}
	return meta, nil
}

// DefaultFields is the zero-config column set: the display field plus
// semantic roles present on the table, capped for readability. Identical
// logic for OOB and custom tables.
func (m *TableMeta) DefaultFields() []string {
	fields := []string{m.DisplayField}
	add := func(name string) {
		if len(fields) >= maxDefaultFields {
			return
		}
		if _, ok := m.Fields[name]; !ok {
			return
		}
		for _, existing := range fields {
			if existing == name {
				return
			}
		}
		fields = append(fields, name)
	}

	add("number")
	add("short_description")
	if _, ok := m.Fields["state"]; ok {
		add("state")
	} else {
		add(m.firstStateLike())
	}
	add("priority")
	// One owner-ish column: assigned_to when the table has it, else owner.
	if _, ok := m.Fields["assigned_to"]; ok {
		add("assigned_to")
	} else {
		add("owner")
	}
	add("sys_updated_on")
	add("active")
	return fields
}

// firstStateLike finds a *_state element for tables without a plain "state"
// (e.g. custom lifecycle fields); empty when none exist.
func (m *TableMeta) firstStateLike() string {
	var candidates []string
	for name := range m.Fields {
		if strings.HasSuffix(name, "_state") && !strings.HasPrefix(name, "sys_") {
			candidates = append(candidates, name)
		}
	}
	if len(candidates) == 0 {
		return ""
	}
	sort.Strings(candidates)
	return candidates[0]
}

// value reads a raw string field from a record, tolerating reference objects.
func value(r snow.Record, name string) string {
	switch v := r[name].(type) {
	case string:
		return v
	case map[string]any:
		if s, ok := v["value"].(string); ok {
			return s
		}
		if s, ok := v["display_value"].(string); ok {
			return s
		}
	}
	return ""
}
