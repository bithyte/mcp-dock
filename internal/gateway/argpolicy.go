package gateway

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
)

// ArgPolicy denies tool arguments at configured dotted paths.
type ArgPolicy struct {
	paths [][]string
}

// NewArgPolicy parses dotted paths. Blank paths and empty segments are ignored.
func NewArgPolicy(paths []string) ArgPolicy {
	var p ArgPolicy
	for _, s := range paths {
		segs := strings.Split(s, ".")
		valid := true
		for _, seg := range segs {
			if seg == "" {
				valid = false
			}
		}
		if valid {
			p.paths = append(p.paths, segs)
		}
	}
	return p
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
