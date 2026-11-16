package mcptransport

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestBoolBytesVariants(t *testing.T) {
	truthy := []string{`true`, `"true"`, `"True"`, `"TRUE"`, `"T"`, `1`, `"1"`, `"yes"`, `"Y"`, `"on"`}
	for _, v := range truthy {
		if got := string(boolBytes(json.RawMessage(v))); got != "true" {
			t.Errorf("boolBytes(%s) = %s, want true", v, got)
		}
	}
	falsy := []string{`false`, `"false"`, `"False"`, `"F"`, `"f"`, `0`, `"0"`, `"no"`, `"off"`}
	for _, v := range falsy {
		if got := string(boolBytes(json.RawMessage(v))); got != "false" {
			t.Errorf("boolBytes(%s) = %s, want false", v, got)
		}
	}
	// Unrecognized values are returned unchanged so the real unmarshal error
	// surfaces instead of being silently forced to some boolean.
	for _, v := range []string{`"maybe"`, `2`, `{}`, `null`} {
		if got := string(boolBytes(json.RawMessage(v))); got != strings.TrimSpace(v) {
			t.Errorf("boolBytes(%s) = %s, want unchanged", v, got)
		}
	}
}

func TestCoerceBooleansSchema(t *testing.T) {
	schema := &Schema{
		Type: TypeObject,
		Properties: map[string]Schema{
			"mobile": {Type: TypeBoolean},
			"count":  {Type: TypeInteger},
			"steps": {
				Type:  TypeArray,
				Items: &Schema{Type: TypeObject, Properties: map[string]Schema{"double": {Type: TypeBoolean}}},
			},
		},
	}
	in := `{"mobile":"TRUE","count":5,"steps":[{"double":1},{"double":false}]}`
	out := string(coerceBooleans(json.RawMessage(in), schema))
	var got struct {
		Mobile bool `json:"mobile"`
		Count  int  `json:"count"`
		Steps  []struct {
			Double bool `json:"double"`
		} `json:"steps"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("output not decodable: %v: %s", err, out)
	}
	if !got.Mobile || got.Count != 5 || len(got.Steps) != 2 || !got.Steps[0].Double || got.Steps[1].Double {
		t.Errorf("coercion wrong: %s", out)
	}
}

// TestDispatchCoercesStringBooleans is the end-to-end guard: a schema-declared
// boolean arriving as "T" / 1 must reach a typed bool/*bool handler cleanly.
func TestDispatchCoercesStringBooleans(t *testing.T) {
	reg := NewRegistry("")
	var saw struct {
		Mobile bool `json:"mobile"`
		Reset  bool `json:"reset"`
	}
	tool := &Tool{Name: "x", InputSchema: Schema{
		Type: TypeObject,
		Properties: map[string]Schema{
			"mobile": {Type: TypeBoolean},
			"reset":  {Type: TypeBoolean},
		},
	}}
	if err := reg.Register(tool, func(ctx context.Context, c *Call) (*ToolResult, error) {
		if err := json.Unmarshal(c.Arguments, &saw); err != nil {
			return nil, err
		}
		return ResultText("ok"), nil
	}); err != nil {
		t.Fatal(err)
	}
	res, err := reg.Dispatch(context.Background(), &Call{Name: "x", Arguments: json.RawMessage(`{"mobile":"T","reset":1}`)})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if res != nil && res.IsError {
		t.Fatalf("unexpected tool error: %s", res.Content[0].Text)
	}
	if !saw.Mobile || !saw.Reset {
		t.Errorf("coerced args wrong: %+v", saw)
	}
}
