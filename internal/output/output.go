// Package output renders records in glm's formats (DESIGN.md §4, §7).
// Only data is ever written here — summaries and pagination hints belong on
// stderr, which is the caller's job.
package output

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"unicode/utf8"
)

const (
	// CellMax caps table/tsv/csv cells; FieldMax soft-caps json/jsonl and
	// record-detail values. --full lifts both.
	CellMax  = 160
	FieldMax = 2000
)

// Formats is the accepted --format set.
var Formats = []string{"table", "tsv", "csv", "json", "jsonl", "ids"}

// Options control rendering.
type Options struct {
	Format string
	Full   bool
}

// Value extracts a field as a string, tolerating the Table API's
// {display_value, value, link} reference objects.
func Value(rec map[string]any, field string) string {
	switch v := rec[field].(type) {
	case nil:
		return ""
	case string:
		return v
	case map[string]any:
		if s, ok := v["display_value"].(string); ok {
			return s
		}
		if s, ok := v["value"].(string); ok {
			return s
		}
		return ""
	default:
		return fmt.Sprint(v)
	}
}

// TruncateField applies the FieldMax soft cap with an explicit marker so a
// consumer always knows content was cut and how to get the rest.
func TruncateField(s string, full bool) string {
	if full || utf8.RuneCountInString(s) <= FieldMax {
		return s
	}
	runes := []rune(s)
	return string(runes[:FieldMax]) + fmt.Sprintf(" …[+%d chars — use --full]", len(runes)-FieldMax)
}

func truncateCell(s string, full bool) string {
	if full || utf8.RuneCountInString(s) <= CellMax {
		return s
	}
	runes := []rune(s)
	return string(runes[:CellMax-1]) + "…"
}

// oneLine keeps tabular cells on a single row.
func oneLine(s string) string {
	if !strings.ContainsAny(s, "\r\n\t") {
		return s
	}
	s = strings.ReplaceAll(s, "\r\n", " ")
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '\t' {
			return ' '
		}
		return r
	}, s)
}

// Records renders a list result set to w.
func Records(w io.Writer, fields []string, recs []map[string]any, opts Options) error {
	switch opts.Format {
	case "ids":
		for _, r := range recs {
			fmt.Fprintln(w, Value(r, "sys_id"))
		}
		return nil

	case "json", "jsonl":
		return writeJSON(w, fields, recs, opts)

	case "csv":
		cw := csv.NewWriter(w)
		if err := cw.Write(fields); err != nil {
			return err
		}
		for _, r := range recs {
			row := make([]string, len(fields))
			for i, f := range fields {
				row[i] = truncateCell(Value(r, f), opts.Full)
			}
			if err := cw.Write(row); err != nil {
				return err
			}
		}
		cw.Flush()
		return cw.Error()

	case "tsv":
		for _, row := range tabularRows(fields, recs, opts) {
			fmt.Fprintln(w, strings.Join(row, "\t"))
		}
		return nil

	case "table":
		rows := tabularRows(fields, recs, opts)
		widths := make([]int, len(fields))
		for _, row := range rows {
			for i, cell := range row {
				if n := utf8.RuneCountInString(cell); n > widths[i] {
					widths[i] = n
				}
			}
		}
		for _, row := range rows {
			var b strings.Builder
			for i, cell := range row {
				b.WriteString(cell)
				if i < len(row)-1 {
					b.WriteString(strings.Repeat(" ", widths[i]-utf8.RuneCountInString(cell)+2))
				}
			}
			fmt.Fprintln(w, b.String())
		}
		return nil

	default:
		return fmt.Errorf("unknown format %q (formats: %s)", opts.Format, strings.Join(Formats, "|"))
	}
}

func tabularRows(fields []string, recs []map[string]any, opts Options) [][]string {
	rows := make([][]string, 0, len(recs)+1)
	rows = append(rows, fields)
	for _, r := range recs {
		row := make([]string, len(fields))
		for i, f := range fields {
			row[i] = oneLine(truncateCell(Value(r, f), opts.Full))
		}
		rows = append(rows, row)
	}
	return rows
}

func writeJSON(w io.Writer, fields []string, recs []map[string]any, opts Options) error {
	objs := make([]map[string]string, len(recs))
	for i, r := range recs {
		obj := make(map[string]string, len(fields)+1)
		for _, f := range fields {
			obj[f] = TruncateField(Value(r, f), opts.Full)
		}
		// sys_id is always present in machine formats for chaining.
		if id := Value(r, "sys_id"); id != "" {
			obj["sys_id"] = id
		}
		objs[i] = obj
	}

	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	if opts.Format == "json" {
		return enc.Encode(objs)
	}
	for _, obj := range objs {
		if err := enc.Encode(obj); err != nil {
			return err
		}
	}
	return nil
}

// RecordDetail renders a single record. With nil fields, columns are derived
// from the record: non-empty only (empty-field omission is a large share of
// the token win on wide tables), regular fields before sys_* bookkeeping.
// Explicitly requested fields render exactly as asked — same names, same
// order, empties included — the stable schema scripts depend on. csv/tsv
// produce a parseable header + one row instead of the key/value view.
func RecordDetail(w io.Writer, rec map[string]any, fields []string, opts Options) error {
	explicit := len(fields) > 0
	if !explicit {
		fields = detailFields(rec)
	}

	switch opts.Format {
	case "ids":
		fmt.Fprintln(w, Value(rec, "sys_id"))
		return nil
	case "json", "jsonl":
		obj := map[string]string{}
		if explicit {
			for _, k := range fields {
				obj[k] = TruncateField(Value(rec, k), opts.Full)
			}
			// Machine formats always carry sys_id for chaining.
			if id := Value(rec, "sys_id"); id != "" {
				obj["sys_id"] = id
			}
		} else {
			for k := range rec {
				if v := Value(rec, k); v != "" {
					obj[k] = TruncateField(v, opts.Full)
				}
			}
		}
		enc := json.NewEncoder(w)
		enc.SetEscapeHTML(false)
		return enc.Encode(obj)
	case "csv", "tsv":
		return Records(w, fields, []map[string]any{rec}, opts)
	}

	width := 0
	for _, k := range fields {
		if n := utf8.RuneCountInString(k); n > width {
			width = n
		}
	}
	for _, k := range fields {
		fmt.Fprintf(w, "%-*s  %s\n", width, k, TruncateField(Value(rec, k), opts.Full))
	}
	return nil
}

// detailFields orders a record's non-empty fields: regular first, then
// sys_* bookkeeping, alphabetical within each group.
func detailFields(rec map[string]any) []string {
	var regular, system []string
	for k := range rec {
		if Value(rec, k) == "" {
			continue
		}
		if strings.HasPrefix(k, "sys_") {
			system = append(system, k)
		} else {
			regular = append(regular, k)
		}
	}
	sort.Strings(regular)
	sort.Strings(system)
	return append(regular, system...)
}
