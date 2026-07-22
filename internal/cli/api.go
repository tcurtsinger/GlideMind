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
	var yes bool

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
				// Write paths always show exactly what would be sent.
				errOut := cmd.ErrOrStderr()
				target := path
				if len(q) > 0 {
					target += "?" + q.Encode()
				}
				fmt.Fprintf(errOut, "%s %s%s\n", method, client.BaseURL(), target)
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
			return renderAPIResponse(cmd, data)
		},
	}
	cmd.Flags().StringArrayVarP(&params, "param", "f", nil, "query parameter k=v (repeatable)")
	cmd.Flags().StringVar(&body, "body", "", "JSON request body (@file reads a file, @- reads stdin)")
	cmd.Flags().BoolVar(&yes, "yes", false, "confirm executing a non-GET request")
	return cmd
}

// readBodyArg resolves --body: a literal JSON string, @file, or @- (stdin).
func readBodyArg(cmd *cobra.Command, body string) ([]byte, error) {
	switch {
	case body == "":
		return nil, nil
	case body == "@-":
		return io.ReadAll(cmd.InOrStdin())
	case strings.HasPrefix(body, "@"):
		return os.ReadFile(body[1:])
	default:
		return []byte(body), nil
	}
}

// renderAPIResponse formats an arbitrary REST response with the shared
// output machinery: a result array of objects renders like query, a result
// object renders like get's detail view, anything else prints as JSON.
func renderAPIResponse(cmd *cobra.Command, data []byte) error {
	out := cmd.OutOrStdout()
	if len(bytes.TrimSpace(data)) == 0 {
		fmt.Fprintln(cmd.ErrOrStderr(), "(empty response)")
		return nil
	}
	var doc any
	if err := json.Unmarshal(data, &doc); err != nil {
		// Non-JSON responses pass through verbatim.
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

	switch v := doc.(type) {
	case []any:
		if recs, ok := asRecords(v); ok && len(recs) > 0 {
			if err := output.Records(out, apiFields(recs), recs, output.Options{Format: format}); err != nil {
				return err
			}
			fmt.Fprintf(cmd.ErrOrStderr(), "%d rows\n", len(recs))
			return nil
		}
	case map[string]any:
		// Single objects default to the detail view, like get.
		if !explicitFormat {
			format = "table"
		}
		return output.RecordDetail(out, v, nil, output.Options{Format: format})
	}

	// Scalars, empty or mixed arrays: the JSON itself is the clearest form.
	enc := json.NewEncoder(out)
	enc.SetEscapeHTML(false)
	return enc.Encode(doc)
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
