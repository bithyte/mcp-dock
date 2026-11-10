package gateway

import (
	"encoding/json"
	"reflect"
	"testing"
)

const jaypostSchema = `{"type":"object","properties":{"text":{"type":"string"},
  "attachment":{"type":"object","properties":{"path":{"type":"string"},"text":{"type":"string"}},"required":["path"]}},
  "required":["text"]}`

func parse(t *testing.T, s string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func attProps(m map[string]any) map[string]any {
	return m["properties"].(map[string]any)["attachment"].(map[string]any)
}

func TestStripSchemaRemovesPath(t *testing.T) {
	orig := parse(t, jaypostSchema)
	snapshot := parse(t, jaypostSchema)
	out := NewArgPolicy([]string{"attachment.path"}).StripSchema(orig).(map[string]any)
	att := attProps(out)
	props := att["properties"].(map[string]any)
	if _, ok := props["path"]; ok {
		t.Fatal("path still present")
	}
	if _, ok := props["text"]; !ok {
		t.Fatal("text missing")
	}
	if _, ok := att["required"]; ok {
		t.Fatal("required should be removed when empty")
	}
	if !reflect.DeepEqual(orig, snapshot) {
		t.Fatal("original mutated")
	}
}

func TestStripSchemaKeepsOtherRequired(t *testing.T) {
	s := parse(t, `{"type":"object","properties":{"a":{"type":"object","properties":{"p":{},"q":{}},"required":["p","q"]}}}`)
	out := NewArgPolicy([]string{"a.p"}).StripSchema(s).(map[string]any)
	req := out["properties"].(map[string]any)["a"].(map[string]any)["required"].([]any)
	if len(req) != 1 || req[0] != "q" {
		t.Fatalf("got %v", req)
	}
}

func TestStripSchemaInAnyOf(t *testing.T) {
	s := parse(t, `{"type":"object","properties":{"attachment":{"anyOf":[
	  {"type":"object","properties":{"path":{"type":"string"},"text":{"type":"string"}},"required":["path"]},
	  {"type":"null"}]}}}`)
	out := NewArgPolicy([]string{"attachment.path"}).StripSchema(s).(map[string]any)
	branch := attProps(out)["anyOf"].([]any)[0].(map[string]any)
	if _, ok := branch["properties"].(map[string]any)["path"]; ok {
		t.Fatal("path still present in branch")
	}
}

func TestStripSchemaNoProperties(t *testing.T) {
	p := NewArgPolicy([]string{"attachment.path"})
	if got := p.StripSchema(nil); got != nil {
		t.Fatalf("nil: got %v", got)
	}
	in := map[string]any{"type": "object"}
	if got := p.StripSchema(in); !reflect.DeepEqual(got, in) {
		t.Fatalf("got %v", got)
	}
	// odd shapes must not panic
	for _, odd := range []any{"str", 3.0, []any{1.0}, map[string]any{"properties": "x", "anyOf": 5.0}, map[string]any{"properties": map[string]any{"attachment": "x"}}} {
		p.StripSchema(odd)
	}
}

func TestEmptyPolicyStrip(t *testing.T) {
	s := parse(t, jaypostSchema)
	out := NewArgPolicy(nil).StripSchema(s)
	if !reflect.DeepEqual(out, parse(t, jaypostSchema)) {
		t.Fatal("changed")
	}
}

func TestCheck(t *testing.T) {
	p := NewArgPolicy([]string{"attachment.path"})
	cases := []struct {
		in     string
		denied string
		err    bool
	}{
		{`{"text":"hi","attachment":{"path":"/etc/passwd"}}`, "attachment.path", false},
		{`{"text":"hi","attachment":{"text":"x"}}`, "", false},
		{`{"text":"hi","attachment":"str"}`, "", false},
		{`null`, "", false},
		{``, "", false},
		{`{}`, "", false},
		{`{"attachment":{"path":null}}`, "attachment.path", false},
		{`{"attachment":null}`, "", false},
		{`{"attachment":[{"path":"x"}]}`, "", false},
		{`[1,2]`, "", false},
		{`"s"`, "", false},
		{`42`, "", false},
		{`{"path":"x"}`, "", false},
		{`{bad json`, "", true},
		{`{"a":1} x`, "", true},
		{`{"Attachment":{"Path":"/etc/passwd"}}`, "attachment.path", false},
		{`{"ATTACHMENT":{"text":"x","PATH":null}}`, "attachment.path", false},
		{`{"attachment":{"path":"x"},"attachment":{"text":"y"}}`, "attachment.path", false},
		{`{"attachment":{"text":"y"},"attachment":{"path":"x"}}`, "attachment.path", false},
		{`{"attachment":"s","Attachment":{"path":1}}`, "attachment.path", false},
		{`{"attachment":{"text":"y","path":"a","PATH":"b"}}`, "attachment.path", false},
		{`{"x":[{"attachment":{"path":"z"}}],"attachment":{"text":"y"}}`, "", false},
		{`{"attachment":{"text":"y"},"attachment":null}`, "", false},
	}
	for _, c := range cases {
		d, err := p.Check(json.RawMessage(c.in))
		if d != c.denied || (err != nil) != c.err {
			t.Errorf("%q: got (%q,%v)", c.in, d, err)
		}
	}
}

func TestCheckEmptyPolicy(t *testing.T) {
	d, err := NewArgPolicy(nil).Check(json.RawMessage(`{"a":1}`))
	if d != "" || err != nil {
		t.Fatal(d, err)
	}
	// empty/blank paths ignored
	d, err = NewArgPolicy([]string{"", "."}).Check(json.RawMessage(`{"a":1}`))
	if d != "" || err != nil {
		t.Fatal(d, err)
	}
}

func TestNewArgPolicyIgnoresMalformed(t *testing.T) {
	bad := []string{"", ".", "..", "a..b", "attachment.", ".path", " attachment.path",
		"attachment.path ", "attachment. path", "attach ment.path", "a.\tb", "a.b\x00", "a.\x7f"}
	p := NewArgPolicy(bad)
	if len(p.paths) != 0 {
		t.Fatalf("paths = %q, want none", p.paths)
	}
	if d, err := p.Check(json.RawMessage(`{"attachment":{"path":"x"}}`)); d != "" || err != nil {
		t.Fatal(d, err)
	}
	if got := NewArgPolicy([]string{"attachment.path", "a-b.c_d"}).paths; len(got) != 2 {
		t.Fatalf("valid paths dropped: %q", got)
	}
}

func TestUnmatched(t *testing.T) {
	js := parse(t, jaypostSchema)
	p := NewArgPolicy([]string{"attachment.path", "attachment.pth", "Text", "nope"})
	got := p.Unmatched([]any{js, nil, "junk"})
	want := []string{"attachment.pth", "nope"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Unmatched = %q, want %q", got, want)
	}
	if got := p.Unmatched(nil); len(got) != 4 {
		t.Fatalf("no schemas: %q", got)
	}
}
