// Package schema validates agent answers against the JSON Schema a turn asked for. Claude
// and Codex enforce schemas natively; runtimes without that (agy) prompt for JSON,
// extract it from the reply and validate it here (docs/architecture.md §7.5, D-017).
package schema

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// resource is the in-memory location the schema is compiled from.
const resource = "mem:///openchef/turn-schema.json"

// ErrNoJSON means the text contains no JSON value.
var ErrNoJSON = errors.New("schema: answer contains no JSON value")

// Schema is a compiled JSON Schema.
type Schema struct{ s *jsonschema.Schema }

// noLoader refuses every external reference: a turn schema must be self-contained, and
// compiling it must never read files or the network.
type noLoader struct{}

func (noLoader) Load(url string) (any, error) {
	return nil, fmt.Errorf("schema: external reference %q is not allowed", url)
}

// Compile parses and compiles raw.
func Compile(raw json.RawMessage) (*Schema, error) {
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("schema: parse: %w", err)
	}
	c := jsonschema.NewCompiler()
	c.UseLoader(noLoader{})
	if err := c.AddResource(resource, doc); err != nil {
		return nil, fmt.Errorf("schema: %w", err)
	}
	s, err := c.Compile(resource)
	if err != nil {
		return nil, fmt.Errorf("schema: compile: %w", err)
	}
	return &Schema{s: s}, nil
}

// Validate checks one JSON document.
func (s *Schema) Validate(doc json.RawMessage) error {
	v, err := jsonschema.UnmarshalJSON(bytes.NewReader(doc))
	if err != nil {
		return fmt.Errorf("schema: answer is not JSON: %w", err)
	}
	if err := s.s.Validate(v); err != nil {
		return fmt.Errorf("schema: %w", err)
	}
	return nil
}

// Extract returns the JSON value an agent answered with: the whole text, else the last
// ```json (or bare ```) fence, else the outermost {...} or [...] span. It does not
// validate against a schema.
func Extract(text string) (json.RawMessage, error) {
	t := strings.TrimSpace(text)
	if json.Valid([]byte(t)) && (strings.HasPrefix(t, "{") || strings.HasPrefix(t, "[")) {
		return json.RawMessage(t), nil
	}
	if f, ok := lastFence(t); ok && json.Valid([]byte(f)) {
		return json.RawMessage(f), nil
	}
	for _, pair := range [][2]string{{"{", "}"}, {"[", "]"}} {
		i, j := strings.Index(t, pair[0]), strings.LastIndex(t, pair[1])
		if i >= 0 && j > i && json.Valid([]byte(t[i:j+1])) {
			return json.RawMessage(t[i : j+1]), nil
		}
	}
	return nil, ErrNoJSON
}

func lastFence(t string) (string, bool) {
	end := strings.LastIndex(t, "```")
	if end < 0 {
		return "", false
	}
	start := strings.LastIndex(t[:end], "```")
	if start < 0 {
		return "", false
	}
	body := t[start+3 : end]
	if nl := strings.IndexByte(body, '\n'); nl >= 0 && !strings.ContainsAny(body[:nl], "{[") {
		body = body[nl+1:] // drop the language tag line
	}
	return strings.TrimSpace(body), true
}

// Instruction is appended to a prompt to ask for an answer that satisfies raw.
func Instruction(raw json.RawMessage) string {
	return "\n\nAnswer with a single JSON value that satisfies this JSON Schema, and nothing " +
		"else: no prose, no code fence.\nJSON Schema:\n" + string(raw)
}

// Repair is the follow-up prompt when an answer failed validation.
func Repair(problem error) string {
	return "Your previous answer was not valid for the required JSON Schema (" + problem.Error() +
		"). Reply again with only the corrected JSON value: no prose, no code fence."
}
