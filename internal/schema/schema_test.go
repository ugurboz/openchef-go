package schema

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

const planSchema = `{"type":"object","properties":{"steps":{"type":"array","items":{"type":"string"},"minItems":1}},"required":["steps"],"additionalProperties":false}`

func TestCompileAndValidate(t *testing.T) {
	t.Parallel()
	s, err := Compile(json.RawMessage(planSchema))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Validate(json.RawMessage(`{"steps":["a","b"]}`)); err != nil {
		t.Errorf("valid doc rejected: %v", err)
	}
	for _, bad := range []string{`{"steps":[]}`, `{"steps":["a"],"x":1}`, `{}`, `[1]`, `nope`} {
		if err := s.Validate(json.RawMessage(bad)); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
}

func TestCompileRejectsBadAndExternalSchemas(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{
		`not json`,
		`{"type":"nonsense"}`,
		`{"$ref":"https://example.com/s.json"}`,
		`{"$ref":"file:///etc/passwd"}`,
	} {
		if _, err := Compile(json.RawMessage(raw)); err == nil {
			t.Errorf("%s compiled", raw)
		}
	}
	if _, err := Compile(json.RawMessage(`{"$defs":{"s":{"type":"string"}},"$ref":"#/$defs/s"}`)); err != nil {
		t.Errorf("local $ref rejected: %v", err)
	}
}

func TestExtract(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		`{"steps":["a"]}`: `{"steps":["a"]}`,
		"  [1,2]\n":       `[1,2]`,
		"Here it is:\n```json\n{\"steps\":[\"a\"]}\n```\nDone.": `{"steps":["a"]}`,
		"```\n{\"a\":1}\n```": `{"a":1}`,
		"first ```json\n{\"a\":1}\n``` then ```json\n{\"a\":2}\n```": `{"a":2}`,
		"The plan is {\"steps\":[\"x\"]} as requested.":              `{"steps":["x"]}`,
	}
	for in, want := range cases {
		got, err := Extract(in)
		if err != nil || string(got) != want {
			t.Errorf("Extract(%q) = %s, %v; want %s", in, got, err, want)
		}
	}
	for _, in := range []string{"", "no json here", "{broken", `"just a string"`} {
		if _, err := Extract(in); !errors.Is(err, ErrNoJSON) {
			t.Errorf("Extract(%q) = %v, want ErrNoJSON", in, err)
		}
	}
}

func TestPrompts(t *testing.T) {
	t.Parallel()
	if s := Instruction(json.RawMessage(planSchema)); !strings.Contains(s, planSchema) || !strings.Contains(s, "JSON Schema") {
		t.Errorf("instruction: %s", s)
	}
	if s := Repair(errors.New("missing steps")); !strings.Contains(s, "missing steps") {
		t.Errorf("repair: %s", s)
	}
}
