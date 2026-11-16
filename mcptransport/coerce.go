package mcptransport

import (
	"bytes"
	"encoding/json"
	"strings"
)

// coerceBooleans walks raw JSON arguments against a tool's input schema and
// normalizes any value sitting at a schema-declared boolean position into a real
// JSON boolean. Clients routinely send booleans as strings ("true"/"True"/"T")
// or numbers (1/0) even though the schema says boolean; without this the typed
// handler struct (bool / *bool) fails with
// "json: cannot unmarshal string into Go value of type bool".
//
// It is intentionally best-effort and silent: any parse problem returns the
// original bytes unchanged, so a valid call is never damaged. Only positions the
// schema declares as boolean are touched (recursing through objects by property
// and arrays by items), so string/number fields are left exactly as sent.
func coerceBooleans(raw json.RawMessage, s *Schema) json.RawMessage {
	if s == nil || len(raw) == 0 {
		return raw
	}
	switch s.Type {
	case TypeObject:
		if len(s.Properties) == 0 {
			return raw
		}
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(raw, &obj); err != nil {
			return raw
		}
		changed := false
		for k, v := range obj {
			ps, ok := s.Properties[k]
			if !ok {
				continue
			}
			if nv := coerceBooleans(v, &ps); !bytes.Equal(nv, v) {
				obj[k] = nv
				changed = true
			}
		}
		if !changed {
			return raw
		}
		out, err := json.Marshal(obj)
		if err != nil {
			return raw
		}
		return out
	case TypeArray:
		if s.Items == nil {
			return raw
		}
		var arr []json.RawMessage
		if err := json.Unmarshal(raw, &arr); err != nil {
			return raw
		}
		changed := false
		for i, v := range arr {
			if nv := coerceBooleans(v, s.Items); !bytes.Equal(nv, v) {
				arr[i] = nv
				changed = true
			}
		}
		if !changed {
			return raw
		}
		out, err := json.Marshal(arr)
		if err != nil {
			return raw
		}
		return out
	case TypeBoolean:
		return boolBytes(raw)
	}
	return raw
}

// boolBytes normalizes a single JSON value found in a boolean slot. Real
// booleans pass through untouched; the number 1/0 and the common string
// spellings ("true"/"True"/"TRUE"/"T"/"yes"/"1"/... and their false-side
// counterparts) become true/false. Anything unrecognized is returned unchanged
// so encoding/json can surface the real error to the caller.
func boolBytes(v json.RawMessage) json.RawMessage {
	t := strings.TrimSpace(string(v))
	switch t {
	case "true", "false":
		return []byte(t)
	case "1":
		return []byte("true")
	case "0":
		return []byte("false")
	}
	if len(t) >= 2 && t[0] == '"' && t[len(t)-1] == '"' {
		switch strings.ToLower(t[1 : len(t)-1]) {
		case "true", "t", "1", "yes", "y", "on":
			return []byte("true")
		case "false", "f", "0", "no", "n", "off":
			return []byte("false")
		}
	}
	return v
}
