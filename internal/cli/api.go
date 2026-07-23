package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/tcurtsinger/GlideMind/internal/output"
)

var apiMethods = map[string]bool{
	http.MethodGet: true, http.MethodPost: true, http.MethodPut: true,
	http.MethodPatch: true, http.MethodDelete: true,
}

func newAPICmd() *cobra.Command {
	var params []string
	var body string
	var yes, full bool

	cmd := &cobra.Command{
		Use:   "api <METHOD> <path>",
		Short: "Raw REST passthrough (gh api style)",
		Long: "Calls any instance REST endpoint with profile auth and glm's usual\n" +
			"output formatting. Query parameters via -f k=v (repeatable). glm is\n" +
			"otherwise read-only, so non-GET methods print the request and refuse\n" +
			"to run without --yes.",
		Example: "  glm api GET /api/now/table/incident -f sysparm_limit=1\n" +
			"  glm api GET /api/sn_sc/servicecatalog/items -f sysparm_text=laptop\n" +
			"  glm api POST /api/x_acme_app/scaffold --body '{\"name\":\"demo\"}' --yes",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			method := strings.ToUpper(args[0])
			path := args[1]
			if !apiMethods[method] {
				return fmt.Errorf("unsupported method %q (use GET, POST, PUT, PATCH, or DELETE)", args[0])
			}
			if !strings.HasPrefix(path, "/") {
				path = "/" + path
			}

			q := url.Values{}
			for _, p := range params {
				k, v, ok := strings.Cut(p, "=")
				if !ok || k == "" {
					return fmt.Errorf("-f wants k=v, got %q", p)
				}
				q.Add(k, v)
			}

			payload, err := readBodyArg(cmd, body)
			if err != nil {
				return err
			}
			if len(payload) > 0 && !json.Valid(payload) {
				return fmt.Errorf("--body is not valid JSON")
			}

			client, _, err := clientFor(cmd, "")
			if err != nil {
				return err
			}

			if method != http.MethodGet {
				// Show exactly what will go on the wire — the same URL Raw
				// builds, so an approved --yes write matches the preview even
				// when the path carries its own query string.
				errOut := cmd.ErrOrStderr()
				target, err := client.PreviewURL(path, q)
				if err != nil {
					return err
				}
				fmt.Fprintf(errOut, "%s %s\n", method, target)
				if len(payload) > 0 {
					fmt.Fprintf(errOut, "%s\n", payload)
				}
				if !yes {
					return fmt.Errorf("non-GET requests need --yes (glm is read-only without it)")
				}
			}

			data, err := client.Raw(cmd.Context(), method, path, q, payload)
			if err != nil {
				return err
			}
			return renderAPIResponse(cmd, data, full)
		},
	}
	cmd.Flags().StringArrayVarP(&params, "param", "f", nil, "query parameter k=v (repeatable)")
	cmd.Flags().StringVar(&body, "body", "", "JSON request body (@file reads a file, @- reads stdin)")
	cmd.Flags().BoolVar(&yes, "yes", false, "confirm executing a non-GET request")
	cmd.Flags().BoolVar(&full, "full", false, "no truncation of long values")
	return cmd
}

// maxAPIBody caps a request body read from @- or @file so unbounded stdin or
// a huge file cannot exhaust memory before validation.
const maxAPIBody = 8 << 20

// readCapped reads at most maxAPIBody bytes, erroring if the source is larger.
func readCapped(r io.Reader) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, maxAPIBody+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxAPIBody {
		return nil, fmt.Errorf("request body exceeds %d MiB", maxAPIBody>>20)
	}
	return data, nil
}

// readBodyArg resolves --body: a literal JSON string, @file, or @- (stdin).
// A leading UTF-8 BOM is stripped — PowerShell pipes and Windows editors
// prepend one, and a BOM is invalid JSON.
func readBodyArg(cmd *cobra.Command, body string) ([]byte, error) {
	var data []byte
	var err error
	switch {
	case body == "":
		return nil, nil
	case body == "@-":
		data, err = readCapped(cmd.InOrStdin())
	case strings.HasPrefix(body, "@"):
		f, oerr := os.Open(body[1:])
		if oerr != nil {
			return nil, oerr
		}
		data, err = readCapped(f)
		f.Close()
	default:
		data = []byte(body)
	}
	if err != nil {
		return nil, err
	}
	return bytes.TrimPrefix(data, []byte("\ufeff")), nil
}

// renderAPIResponse formats an arbitrary REST response.
//
// Machine formats (json/jsonl) are a FAITHFUL passthrough: the response is
// decoded with UseNumber and re-emitted, so scalar types, large-integer
// precision, nulls, and nested structure all survive intact — glm api is
// the raw escape hatch and must not mutate the server's data.
//
// Human formats (table/tsv/csv/ids) may flatten and truncate for reading:
// a result array of flat objects renders like query, a flat result object
// like get's detail view, and anything else falls back to complete JSON so
// no value is silently dropped.
func renderAPIResponse(cmd *cobra.Command, data []byte, full bool) error {
	out := cmd.OutOrStdout()
	if len(bytes.TrimSpace(data)) == 0 {
		fmt.Fprintln(cmd.ErrOrStderr(), "(empty response)")
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	// UseNumber keeps 9007199254740993 exact instead of rounding through
	// float64 — the whole point of a raw passthrough.
	dec.UseNumber()
	var doc any
	if err := dec.Decode(&doc); err != nil {
		// Non-JSON responses pass through verbatim.
		_, werr := out.Write(data)
		return werr
	}
	// Decoder.Decode stops after the first value; json.Unmarshal used to
	// reject trailing bytes. Preserve that: a body that is a JSON value
	// plus appended data is not a clean document, so pass it through
	// verbatim rather than silently dropping the tail.
	if _, err := dec.Token(); err != io.EOF {
		_, werr := out.Write(data)
		return werr
	}
	// Unwrap the standard {result: ...} envelope when it is the whole story.
	if m, ok := doc.(map[string]any); ok && len(m) == 1 {
		if r, exists := m["result"]; exists {
			doc = r
		}
	}

	format, explicitFormat, err := resolveFormat(cmd)
	if err != nil {
		return err
	}

	// Faithful machine passthrough — never routed through the string-
	// coercing human renderers.
	if format == "json" || format == "jsonl" {
		return writeAPIJSON(out, doc, format)
	}

	switch v := doc.(type) {
	case []any:
		recs, ok := asRecords(v)
		if !ok || !allFlat(recs) {
			break
		}
		if len(recs) == 0 {
			fmt.Fprintln(cmd.ErrOrStderr(), "0 rows")
			return nil
		}
		if err := output.Records(out, apiFields(recs), recs, output.Options{Format: format, Full: full}); err != nil {
			return err
		}
		fmt.Fprintf(cmd.ErrOrStderr(), "%d rows\n", len(recs))
		return nil
	case map[string]any:
		if !flatRecord(v) {
			break
		}
		// Single flat objects default to the detail view, like get.
		if !explicitFormat {
			format = "table"
		}
		return output.RecordDetail(out, v, nil, output.Options{Format: format, Full: full})
	}

	// Nested/complex data in a human format: fall back to complete JSON so
	// nothing is silently dropped.
	return writeAPIJSON(out, doc, "json")
}

// writeAPIJSON emits doc losslessly. jsonl encodes one array element per
// line; every other shape is a single document. json.Number, bool, and nil
// round-trip exactly, so no scalar is coerced to a string.
func writeAPIJSON(w io.Writer, doc any, format string) error {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	if format == "jsonl" {
		if arr, ok := doc.([]any); ok {
			for _, el := range arr {
				if err := enc.Encode(el); err != nil {
					return err
				}
			}
			return nil
		}
	}
	return enc.Encode(doc)
}

// flatValue reports whether output.Value renders v faithfully in a human
// format: scalars and the Table API's {display_value, value} reference
// shape. Anything else (nested objects, arrays) would be blanked or mangled.
func flatValue(v any) bool {
	switch m := v.(type) {
	case nil, string, bool, float64, json.Number:
		return true
	case map[string]any:
		_, dv := m["display_value"]
		_, val := m["value"]
		return dv || val
	default:
		return false
	}
}

func flatRecord(m map[string]any) bool {
	for _, v := range m {
		if !flatValue(v) {
			return false
		}
	}
	return true
}

func allFlat(recs []map[string]any) bool {
	for _, r := range recs {
		if !flatRecord(r) {
			return false
		}
	}
	return true
}

// asRecords narrows []any to records when every element is an object.
func asRecords(v []any) ([]map[string]any, bool) {
	recs := make([]map[string]any, 0, len(v))
	for _, el := range v {
		m, ok := el.(map[string]any)
		if !ok {
			return nil, false
		}
		recs = append(recs, m)
	}
	return recs, true
}

// apiFields derives deterministic columns for arbitrary result objects: the
// union of keys, alphabetical.
func apiFields(recs []map[string]any) []string {
	seen := map[string]bool{}
	var fields []string
	for _, r := range recs {
		for k := range r {
			if !seen[k] {
				seen[k] = true
				fields = append(fields, k)
			}
		}
	}
	sort.Strings(fields)
	return fields
}
