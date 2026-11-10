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
	for _, k := range []string{"anyOf", "oneOf", "allOf"} {
		if arr, ok := m[k].([]any); ok {
			for _, branch := range arr {
				stripNode(branch, segs)
			}
		}
	}
}

// Check returns the first denied path present in args, or "" if allowed.
// A key counts as present even when its value is null. Keys match
// case-insensitively and every occurrence of a duplicate key is inspected,
// because upstream parsers may differ on case and on first/last-wins.
// Non-object values along a path are not denied. err is returned only for
// invalid JSON.
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
// remaining path segs resolves to a present key anywhere inside it.
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
		for dec.More() {
			if _, err := walk(dec, nil); err != nil {
				return false, err
			}
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
// through properties and anyOf/oneOf/allOf branches.
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
	for _, k := range []string{"anyOf", "oneOf", "allOf"} {
		if arr, ok := m[k].([]any); ok {
			for _, branch := range arr {
				if schemaHas(branch, segs) {
					return true
				}
			}
		}
	}
	return false
}
