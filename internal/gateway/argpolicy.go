package gateway

import (
	"bytes"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// ArgPolicy denies tool arguments at configured dotted paths.
type ArgPolicy struct {
	paths [][]string
}

// NewArgPolicy parses dotted paths. A path with an empty segment, or a
// segment containing whitespace or control characters, is ignored; config
// validation rejects those before they get here.
func NewArgPolicy(paths []string) ArgPolicy {
	var p ArgPolicy
	for _, s := range paths {
		segs := strings.Split(s, ".")
		if !slices.ContainsFunc(segs, badSegment) {
			p.paths = append(p.paths, segs)
		}
	}
	return p
}

func badSegment(seg string) bool {
	if seg == "" {
		return true
	}
	for _, r := range seg {
		if unicode.IsSpace(r) || unicode.IsControl(r) || r == utf8.RuneError {
			return true
		}
	}
	return false
}

// StripSchema returns a deep copy of schema with denied paths removed.
// The input is never modified.
func (p ArgPolicy) StripSchema(schema any) any {
	if schema == nil {
		return nil
	}
	b, err := json.Marshal(schema)
	if err != nil {
		return schema
	}
	var cp any
	if err := json.Unmarshal(b, &cp); err != nil {
		return schema
	}
	for _, segs := range p.paths {
		stripNode(cp, segs)
	}
	return cp
}

func stripNode(node any, segs []string) {
	m, ok := node.(map[string]any)
	if !ok || len(segs) == 0 {
		return
	}
	if props, ok := m["properties"].(map[string]any); ok {
		if len(segs) == 1 {
			if _, has := props[segs[0]]; has {
				delete(props, segs[0])
				if req, ok := m["required"].([]any); ok {
					kept := make([]any, 0, len(req))
					for _, r := range req {
						if s, ok := r.(string); !ok || s != segs[0] {
							kept = append(kept, r)
						}
					}
					if len(kept) == 0 {
						delete(m, "required")
					} else {
						m["required"] = kept
					}
				}
			}
		} else if child, has := props[segs[0]]; has {
			stripNode(child, segs[1:])
		}
	}
	for _, sub := range sameLevel(m) {
		stripNode(sub, segs)
	}
}

// sameLevel returns the subschemas of m that a path continues into without
// consuming a segment: anyOf/oneOf/allOf branches, and array element
// schemas (items, in object or array form, and prefixItems), since Check
// looks inside every array element.
func sameLevel(m map[string]any) []any {
	var out []any
	for _, k := range []string{"anyOf", "oneOf", "allOf", "prefixItems"} {
		if arr, ok := m[k].([]any); ok {
			out = append(out, arr...)
		}
	}
	switch it := m["items"].(type) {
	case map[string]any:
		out = append(out, it)
	case []any:
		out = append(out, it...)
	}
	return out
}

// Check returns the first denied path present in args, or "" if allowed.
// A key counts as present even when its value is null. Keys match
// case-insensitively and every occurrence of a duplicate key is inspected,
// because upstream parsers may differ on case and on first/last-wins.
// Arrays along a path are looked through: each element is checked against
// the rest of the path. Scalars along a path are not denied. err is
// returned only for invalid JSON.
func (p ArgPolicy) Check(args json.RawMessage) (denied string, err error) {
	if len(bytes.TrimSpace(args)) == 0 {
		return "", nil
	}
	if !json.Valid(args) {
		return "", errors.New("invalid JSON arguments")
	}
	for _, segs := range p.paths {
		dec := json.NewDecoder(bytes.NewReader(args))
		found, err := walk(dec, segs)
		if err != nil {
			return "", err
		}
		if found {
			return strings.Join(segs, "."), nil
		}
	}
	return "", nil
}

// walk consumes exactly one JSON value from dec and reports whether the
// remaining path segs resolves to a present key inside it, looking through
// arrays at any level. Recursion depth is bounded by the JSON nesting depth,
// which json.Valid in Check has already limited.
// With empty segs it just consumes the value.
func walk(dec *json.Decoder, segs []string) (bool, error) {
	tok, err := dec.Token()
	if err != nil {
		return false, err
	}
	d, ok := tok.(json.Delim)
	if !ok {
		return false, nil
	}
	found := false
	switch d {
	case '{':
		for dec.More() {
			kt, err := dec.Token()
			if err != nil {
				return false, err
			}
			key, _ := kt.(string)
			if len(segs) > 0 && strings.EqualFold(key, segs[0]) {
				if len(segs) == 1 {
					found = true
					if _, err := walk(dec, nil); err != nil {
						return false, err
					}
				} else {
					f, err := walk(dec, segs[1:])
					if err != nil {
						return false, err
					}
					found = found || f
				}
			} else if _, err := walk(dec, nil); err != nil {
				return false, err
			}
		}
	case '[':
		// Every element is checked with the same remaining path, so
		// {"attachment":[{"path":...}]} is denied. This can over-block a
		// tool whose schema has no array there, which is the safe side.
		for dec.More() {
			f, err := walk(dec, segs)
			if err != nil {
				return false, err
			}
			found = found || f
		}
	}
	if _, err := dec.Token(); err != nil { // closing delimiter
		return false, err
	}
	return found, nil
}

// Unmatched returns the denied paths, in policy order, that name no
// property in any of schemas. Segments match case-insensitively, as in
// Check. Such a path is probably a typo in deny_args.
func (p ArgPolicy) Unmatched(schemas []any) []string {
	norm := make([]any, 0, len(schemas))
	for _, s := range schemas {
		if b, err := json.Marshal(s); err == nil {
			var cp any
			if json.Unmarshal(b, &cp) == nil {
				norm = append(norm, cp)
			}
		}
	}
	var out []string
	for _, segs := range p.paths {
		if !slices.ContainsFunc(norm, func(s any) bool { return schemaHas(s, segs) }) {
			out = append(out, strings.Join(segs, "."))
		}
	}
	return out
}

// schemaHas reports whether segs names a property in schema, looking
// through properties and the subschemas sameLevel returns.
func schemaHas(node any, segs []string) bool {
	m, ok := node.(map[string]any)
	if !ok || len(segs) == 0 {
		return false
	}
	if props, ok := m["properties"].(map[string]any); ok {
		for k, child := range props {
			if strings.EqualFold(k, segs[0]) && (len(segs) == 1 || schemaHas(child, segs[1:])) {
				return true
			}
		}
	}
	for _, sub := range sameLevel(m) {
		if schemaHas(sub, segs) {
			return true
		}
	}
	return false
}
