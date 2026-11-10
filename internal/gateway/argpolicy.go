package gateway

import (
	"bytes"
	"encoding/json"
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
// A key counts as present even when its value is null. Non-object values
// along a path are not denied. err is returned only for invalid JSON.
func (p ArgPolicy) Check(args json.RawMessage) (denied string, err error) {
	if len(bytes.TrimSpace(args)) == 0 {
		return "", nil
	}
	var v any
	if err := json.Unmarshal(args, &v); err != nil {
		return "", err
	}
	for _, segs := range p.paths {
		cur := v
		found := true
		for _, seg := range segs {
			m, ok := cur.(map[string]any)
			if !ok {
				found = false
				break
			}
			next, has := m[seg]
			if !has {
				found = false
				break
			}
			cur = next
		}
		if found {
			return strings.Join(segs, "."), nil
		}
	}
	return "", nil
}
