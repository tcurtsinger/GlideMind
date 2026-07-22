package schema

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tcurtsinger/GlideMind/internal/exit"
	"github.com/tcurtsinger/GlideMind/internal/snow"
)

func fakeInstance(t *testing.T) *snow.Client {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/now/table/sys_db_object", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("sysparm_query")
		var rows []map[string]any
		switch {
		case strings.Contains(q, "name=incident"):
			rows = []map[string]any{{"name": "incident", "super_class.name": "task"}}
		case strings.Contains(q, "name=task"):
			rows = []map[string]any{{"name": "task", "super_class.name": ""}}
		case strings.Contains(q, "name=u_gadget"):
			rows = []map[string]any{{"name": "u_gadget", "super_class.name": ""}}
		}
		writeResult(w, rows)
	})
	mux.HandleFunc("/api/now/table/sys_dictionary", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("sysparm_query")
		var rows []map[string]any
		if strings.Contains(q, "incident") {
			rows = []map[string]any{
				{"name": "task", "element": "sys_id", "internal_type": "GUID", "display": "false", "reference.name": ""},
				{"name": "task", "element": "number", "internal_type": "string", "display": "true", "reference.name": ""},
				{"name": "task", "element": "short_description", "internal_type": "string", "display": "false", "reference.name": ""},
				{"name": "task", "element": "assigned_to", "internal_type": "reference", "display": "false", "reference.name": "sys_user"},
				{"name": "task", "element": "active", "internal_type": "boolean", "display": "false", "reference.name": ""},
				{"name": "task", "element": "sys_updated_on", "internal_type": "glide_date_time", "display": "false", "reference.name": ""},
				{"name": "incident", "element": "state", "internal_type": "integer", "display": "false", "reference.name": ""},
				{"name": "incident", "element": "incident_state", "internal_type": "integer", "display": "false", "reference.name": ""},
				{"name": "incident", "element": "priority", "internal_type": "integer", "display": "false", "reference.name": ""},
				{"name": "incident", "element": "description", "internal_type": "string", "display": "false", "reference.name": ""},
			}
		} else if strings.Contains(q, "u_gadget") {
			rows = []map[string]any{
				{"name": "u_gadget", "element": "sys_id", "internal_type": "GUID", "display": "false", "reference.name": ""},
				{"name": "u_gadget", "element": "name", "internal_type": "string", "display": "false", "reference.name": ""},
				{"name": "u_gadget", "element": "u_lifecycle_state", "internal_type": "integer", "display": "false", "reference.name": ""},
				{"name": "u_gadget", "element": "owner", "internal_type": "reference", "display": "false", "reference.name": "sys_user"},
				{"name": "u_gadget", "element": "active", "internal_type": "boolean", "display": "false", "reference.name": ""},
				{"name": "u_gadget", "element": "sys_updated_on", "internal_type": "glide_date_time", "display": "false", "reference.name": ""},
			}
		}
		writeResult(w, rows)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	c, err := snow.NewBasic(srv.URL, "u", "p", 5*time.Second)
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	return c
}

func writeResult(w http.ResponseWriter, rows []map[string]any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"result": rows}) //nolint:errcheck
}

func TestFetchDerivesInheritedMeta(t *testing.T) {
	c := fakeInstance(t)
	meta, err := Fetch(context.Background(), c, "incident")
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if len(meta.Chain) != 2 || meta.Chain[0] != "incident" || meta.Chain[1] != "task" {
		t.Errorf("chain = %v", meta.Chain)
	}
	if meta.DisplayField != "number" {
		t.Errorf("display = %q, want number (from task)", meta.DisplayField)
	}
	if f, ok := meta.Fields["assigned_to"]; !ok || f.Reference != "sys_user" {
		t.Errorf("assigned_to reference lost: %+v", meta.Fields["assigned_to"])
	}

	got := meta.DefaultFields()
	want := []string{"number", "short_description", "state", "priority", "assigned_to", "sys_updated_on", "active"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("default fields = %v, want %v", got, want)
	}
}

func TestFetchCustomTableHeuristics(t *testing.T) {
	c := fakeInstance(t)
	meta, err := Fetch(context.Background(), c, "u_gadget")
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if meta.DisplayField != "name" {
		t.Errorf("display = %q, want fallback to name", meta.DisplayField)
	}
	got := meta.DefaultFields()
	want := []string{"name", "u_lifecycle_state", "owner", "sys_updated_on", "active"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("default fields = %v, want %v (state-like detection, owner fallback, only present roles)", got, want)
	}
}

func TestFetchUnknownTable(t *testing.T) {
	c := fakeInstance(t)
	_, err := Fetch(context.Background(), c, "no_such_table")
	var nf *NotFoundError
	if !errors.As(err, &nf) {
		t.Fatalf("want NotFoundError, got %v", err)
	}
	if nf.ExitCode() != exit.NotFound {
		t.Errorf("exit = %d, want %d", nf.ExitCode(), exit.NotFound)
	}
}
