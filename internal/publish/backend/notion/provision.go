package notion

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"github.com/sigma/okf-tools/internal/schema"
)

// Provision reconciles the data source's columns against schema.json before a
// publish, so a fresh data source Just Works instead of requiring the columns to
// be hand-created. It reads the data source's current properties, then adds any
// schema-declared column that is missing, typed from the column's kind (text →
// rich_text, select → select+options, list → multi_select+options, date → date,
// number → number, checkbox → checkbox).
//
// It is idempotent and additive: an already-present column is left exactly as it
// is (never recreated or retyped), and a fully-provisioned data source issues no
// PATCH at all — the near-noop property the scheduled path relies on. The title
// column is never provisioned: a Notion data source is created with exactly one
// title property, and a second cannot be added.
//
// With no schema configured Provision is a no-op — property values then fall back
// to their legacy title/rich_text split, and column shape is whatever the target
// already has.
func (b *Backend) Provision(ctx context.Context) error {
	if b.schema == nil {
		return nil
	}

	var ds dataSource
	path := "/data_sources/" + url.PathEscape(b.dataSourceID)
	if err := b.do(ctx, http.MethodGet, path, nil, &ds); err != nil {
		return fmt.Errorf("notion: provision: read data source: %w", err)
	}

	existing := make(map[string]bool, len(ds.Properties))
	for name := range ds.Properties {
		existing[strings.ToLower(name)] = true
	}

	// Sorted for a deterministic build; the added set is what matters, and JSON
	// marshaling sorts map keys on the wire regardless.
	names := make([]string, 0, len(b.schema.Columns))
	for name := range b.schema.Columns {
		names = append(names, name)
	}
	sort.Strings(names)

	add := map[string]any{}
	for _, name := range names {
		if existing[strings.ToLower(name)] {
			continue // idempotent: leave an existing column untouched
		}
		def, ok := columnPropertyDef(b.schema.Columns[name])
		if !ok {
			continue // title (always exists) or an unknown kind: nothing to add
		}
		add[name] = def
	}
	if len(add) == 0 {
		return nil // already provisioned: no write, preserving the near-noop property
	}

	req := dataSourceUpdate{Properties: add}
	if err := b.do(ctx, http.MethodPatch, path, req, nil); err != nil {
		return fmt.Errorf("notion: provision: add columns: %w", err)
	}
	return nil
}

// columnPropertyDef maps a schema column to the Notion property definition that
// creates it on a data source. The bool is false for a column that must not be
// provisioned: the title (a data source already has exactly one) or an unknown
// kind.
func columnPropertyDef(c schema.Column) (map[string]any, bool) {
	switch c.Kind {
	case "text":
		return map[string]any{"rich_text": map[string]any{}}, true
	case "select":
		return map[string]any{"select": map[string]any{"options": selectOptions(c.Options)}}, true
	case "list":
		return map[string]any{"multi_select": map[string]any{"options": selectOptions(c.Options)}}, true
	case "date":
		return map[string]any{"date": map[string]any{}}, true
	case "number":
		return map[string]any{"number": map[string]any{}}, true
	case "checkbox":
		return map[string]any{"checkbox": map[string]any{}}, true
	default: // "title" and any unknown kind
		return nil, false
	}
}

// selectOptions turns a column's closed value vocabulary into Notion select /
// multi_select option definitions. A nil/empty vocabulary yields an empty option
// list — Notion then accepts any value of the kind and grows the options as
// values arrive.
func selectOptions(opts []string) []map[string]any {
	out := make([]map[string]any, 0, len(opts))
	for _, o := range opts {
		out = append(out, map[string]any{"name": o})
	}
	return out
}

// dataSource is the sliver of a GET /data_sources/{id} response provisioning
// reads: the existing column set keyed by name, each carrying its Notion type.
// Only the presence of a name matters to reconcile — an existing column is never
// retyped — so the value need only be decodable.
type dataSource struct {
	Properties map[string]dataSourceProp `json:"properties"`
}

// dataSourceProp is one existing column in a data source. Its type is decoded for
// diagnosability; reconcile keys only off the property's presence.
type dataSourceProp struct {
	Type string `json:"type"`
}

// dataSourceUpdate is the PATCH /data_sources/{id} body that adds columns: a
// properties map of column name → Notion property definition. Notion merges these
// into the existing schema, so naming only the columns to add leaves the rest
// untouched.
type dataSourceUpdate struct {
	Properties map[string]any `json:"properties"`
}
