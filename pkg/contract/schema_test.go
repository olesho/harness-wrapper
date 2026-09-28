package contract

import (
	"bytes"
	"encoding/json"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

// The JSON Schema is generated from the Go types — never kept by hand beside
// them. Regenerate after a change to a type with:
//
//	UPDATE_SCHEMA=1 go test ./pkg/contract -run TestSchema
func TestSchema(t *testing.T) {
	got := generateSchema()
	if os.Getenv("UPDATE_SCHEMA") != "" {
		if err := os.WriteFile("schema.json", got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	if !bytes.Equal(got, Schema) {
		t.Fatal("schema.json is not what the Go types generate: UPDATE_SCHEMA=1 go test ./pkg/contract -run TestSchema")
	}
	var doc map[string]any
	if err := json.Unmarshal(Schema, &doc); err != nil {
		t.Fatalf("schema.json is not JSON: %v", err)
	}
}

// messages are the schema's top-level definitions: every type that crosses a
// process boundary or is journaled, and every observation payload.
var messages = []any{
	Descriptor{},
	ProvisionRequest{},
	ProvisionResult{},
	OpenRequest{},
	OpenResult{},
	Input{},
	SendResult{},
	InterruptRequest{},
	Choice{},
	State{},
	Batch{},
	CloseResult{},
	RecordRequest{},
	Recovered{},
	Error{},
	TurnEndedData{},
	TextData{},
	AssistantTextData{},
	ToolUseData{},
	ToolResultData{},
	ToolFinishedData{},
	APIErrorData{},
	TextDeltaData{},
	SubagentData{},
	PromptInfo{},
	PromptResolvedData{},
	RateLimitData{},
	RetryingData{},
	Block{},
	SessionExitedData{},
}

// requests are refused with unknown fields; results and observations take
// them (a minor version adds optional fields).
var requests = map[reflect.Type]bool{
	reflect.TypeOf(ProvisionRequest{}): true, reflect.TypeOf(AgentSpec{}): true,
	reflect.TypeOf(Layout{}): true, reflect.TypeOf(Instructions{}): true,
	reflect.TypeOf(Skill{}): true, reflect.TypeOf(SkillFile{}): true,
	reflect.TypeOf(Connector{}): true, reflect.TypeOf(StdioConnector{}): true,
	reflect.TypeOf(HTTPConnector{}): true, reflect.TypeOf(Memory{}): true,
	reflect.TypeOf(MemoryFile{}): true, reflect.TypeOf(CredentialRef{}): true,
	reflect.TypeOf(OpenRequest{}): true, reflect.TypeOf(CredentialFile{}): true,
	reflect.TypeOf(Input{}): true, reflect.TypeOf(ContentPart{}): true,
	reflect.TypeOf(InterruptRequest{}): true, reflect.TypeOf(Choice{}): true,
	reflect.TypeOf(RecordRequest{}): true,
}

type enum interface{ Values() []string }

var (
	enumType = reflect.TypeOf((*enum)(nil)).Elem()
	timeType = reflect.TypeOf(time.Time{})
	rawType  = reflect.TypeOf(json.RawMessage(nil))
	bytesTyp = reflect.TypeOf([]byte(nil))
	models   = reflect.TypeOf(Models{})
)

func generateSchema() []byte {
	defs := map[string]any{}
	for _, m := range messages {
		ref(reflect.TypeOf(m), defs)
	}
	doc := map[string]any{
		"$schema": "https://json-schema.org/draft/2020-12/schema",
		"$id":     "https://github.com/olesho/harness-wrapper/pkg/contract/schema.json",
		"title":   "Harness Adapter Interface " + Version,
		"$defs":   defs,
	}
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(doc); err != nil {
		panic(err)
	}
	return b.Bytes()
}

// ref is the schema of t: a $ref to its definition for a named struct or
// enum, inline for everything else.
func ref(t reflect.Type, defs map[string]any) map[string]any {
	switch {
	case t == timeType:
		return map[string]any{"type": "string", "format": "date-time"}
	case t == rawType:
		return map[string]any{}
	case t == bytesTyp:
		return map[string]any{"type": "string", "contentEncoding": "base64"}
	case t == models:
		return map[string]any{"oneOf": []any{
			map[string]any{"const": "any"},
			map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
		}}
	case t.Implements(enumType) && t.Kind() == reflect.String:
		name := t.Name()
		if _, done := defs[name]; !done {
			values := reflect.Zero(t).Interface().(enum).Values()
			defs[name] = map[string]any{"type": "string", "enum": values}
		}
		return map[string]any{"$ref": "#/$defs/" + name}
	}
	switch t.Kind() {
	case reflect.Pointer:
		return ref(t.Elem(), defs)
	case reflect.String:
		return map[string]any{"type": "string"}
	case reflect.Bool:
		return map[string]any{"type": "boolean"}
	case reflect.Int, reflect.Int64, reflect.Int32:
		return map[string]any{"type": "integer"}
	case reflect.Float64:
		return map[string]any{"type": "number"}
	case reflect.Slice:
		return map[string]any{"type": "array", "items": ref(t.Elem(), defs)}
	case reflect.Map:
		return map[string]any{"type": "object", "additionalProperties": ref(t.Elem(), defs)}
	case reflect.Struct:
		name := t.Name()
		if _, done := defs[name]; !done {
			defs[name] = nil // reserve against recursion
			defs[name] = object(t, defs)
		}
		return map[string]any{"$ref": "#/$defs/" + name}
	}
	panic("schema: unhandled type " + t.String())
}

func object(t reflect.Type, defs map[string]any) map[string]any {
	props := map[string]any{}
	var required []string
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		tag := f.Tag.Get("json")
		name, opts, _ := strings.Cut(tag, ",")
		if name == "-" {
			continue
		}
		if name == "" {
			name = f.Name
		}
		props[name] = ref(f.Type, defs)
		if !strings.Contains(opts, "omitempty") {
			required = append(required, name)
		}
	}
	sort.Strings(required)
	o := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		o["required"] = required
	}
	if requests[t] {
		o["additionalProperties"] = false
	}
	return o
}
