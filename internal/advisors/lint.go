package advisors

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Categories of a lint, the ones of the Management API.
const (
	CategorySecurity    = "SECURITY"
	CategoryPerformance = "PERFORMANCE"
	CategoryHealth      = "HEALTH"
)

// Levels of a lint.
const (
	LevelError = "ERROR"
	LevelWarn  = "WARN"
	LevelInfo  = "INFO"
)

// DocsURL is the documentation root the remediation links of the lints computed outside SQL use. The
// SQL lints carry the one recorded in the header of lints.sql, which is the same.
const DocsURL = "https://supabase.com/docs"

// Lint is one finding. Raw is the object exactly as the SQL (or the code of this package) produced
// it, so a field the Management API spec does not list yet, such as metadata.bucket_id of
// public_bucket_allows_listing that Studio links on, reaches the client unchanged.
type Lint struct {
	Name       string
	Level      string
	Categories []string
	CacheKey   string
	// Metadata is the decoded metadata object, nil when the lint has none.
	Metadata map[string]any
	Raw      json.RawMessage
}

// MarshalJSON writes the lint as it was produced.
func (l Lint) MarshalJSON() ([]byte, error) {
	if len(l.Raw) == 0 {
		return []byte("null"), nil
	}
	return l.Raw, nil
}

// Has reports whether the lint is in category c.
func (l Lint) Has(c string) bool {
	for _, x := range l.Categories {
		if x == c {
			return true
		}
	}
	return false
}

// ParseRows decodes the rows the lint SQL returned: a JSON array of lint objects.
func ParseRows(b []byte) ([]Lint, error) {
	var rows []json.RawMessage
	if err := json.Unmarshal(b, &rows); err != nil {
		return nil, fmt.Errorf("advisors: lint rows are not a JSON array: %w", err)
	}
	out := make([]Lint, 0, len(rows))
	for _, r := range rows {
		l, err := parseLint(r)
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, nil
}

func parseLint(raw json.RawMessage) (Lint, error) {
	var v struct {
		Name       string         `json:"name"`
		Level      string         `json:"level"`
		Categories []string       `json:"categories"`
		CacheKey   string         `json:"cache_key"`
		Metadata   map[string]any `json:"metadata"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return Lint{}, fmt.Errorf("advisors: a lint row is not an object: %w", err)
	}
	if v.Name == "" || len(v.Categories) == 0 {
		return Lint{}, fmt.Errorf("advisors: a lint row has no name or categories: %.120s", raw)
	}
	cp := make(json.RawMessage, len(raw))
	copy(cp, raw)
	return Lint{Name: v.Name, Level: v.Level, Categories: v.Categories, CacheKey: v.CacheKey, Metadata: v.Metadata, Raw: cp}, nil
}

// fields is the shape of a lint this package builds in code. The keys are the ones of the
// Management API's lint object; the response specs in internal/api/gen are the authority and a test
// in internal/api checks every lint built here against them.
type fields struct {
	Name        string         `json:"name"`
	Title       string         `json:"title"`
	Level       string         `json:"level"`
	Facing      string         `json:"facing"`
	Categories  []string       `json:"categories"`
	Description string         `json:"description"`
	Detail      string         `json:"detail"`
	CacheKey    string         `json:"cache_key"`
	Remediation string         `json:"remediation"`
	Metadata    map[string]any `json:"metadata,omitempty"`
}

func build(f fields) Lint {
	f.Facing = "EXTERNAL"
	raw, err := json.Marshal(f)
	if err != nil { // a struct of strings and one map of strings: cannot fail
		panic(err)
	}
	return Lint{Name: f.Name, Level: f.Level, Categories: f.Categories, CacheKey: f.CacheKey, Metadata: f.Metadata, Raw: raw}
}

// Filter returns the lints of category c, in order.
func Filter(lints []Lint, c string) []Lint {
	out := make([]Lint, 0, len(lints))
	for _, l := range lints {
		if l.Has(c) {
			out = append(out, l)
		}
	}
	return out
}

// Names returns the names of lints, in order.
func Names(lints []Lint) string {
	n := make([]string, len(lints))
	for i, l := range lints {
		n[i] = l.Name
	}
	return strings.Join(n, ",")
}
