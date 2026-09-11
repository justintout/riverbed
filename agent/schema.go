package agent

import "encoding/json"

// schemaParts pulls the properties and required list out of a tool's JSON
// Schema. The MCP SDK hands back a typed schema value, so it is round-tripped
// through JSON rather than type-asserted: that works whatever concrete type the
// server or SDK version produced.
func schemaParts(schema any) (properties map[string]any, required []string, ok bool) {
	if schema == nil {
		return nil, nil, false
	}
	encoded, err := json.Marshal(schema)
	if err != nil {
		return nil, nil, false
	}
	var decoded struct {
		Properties map[string]any `json:"properties"`
		Required   []string       `json:"required"`
	}
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		return nil, nil, false
	}
	if decoded.Properties == nil {
		return nil, decoded.Required, len(decoded.Required) > 0
	}
	return decoded.Properties, decoded.Required, true
}

// schemaObject returns a tool's schema as a plain JSON object, defaulting to an
// empty object schema when the server supplied none.
func schemaObject(schema any) map[string]any {
	out := map[string]any{"type": "object", "properties": map[string]any{}}
	if schema == nil {
		return out
	}
	encoded, err := json.Marshal(schema)
	if err != nil {
		return out
	}
	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil || decoded == nil {
		return out
	}
	if _, hasType := decoded["type"]; !hasType {
		decoded["type"] = "object"
	}
	if _, hasProps := decoded["properties"]; !hasProps {
		decoded["properties"] = map[string]any{}
	}
	return decoded
}
