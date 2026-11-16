package mcptransport

// DataType is a JSON Schema primitive type.
type DataType string

const (
	TypeObject  DataType = "object"
	TypeNumber  DataType = "number"
	TypeInteger DataType = "integer"
	TypeString  DataType = "string"
	TypeArray   DataType = "array"
	TypeNull    DataType = "null"
	TypeBoolean DataType = "boolean"
)

// Schema is a limited JSON Schema used to describe a tool's inputSchema.
// It covers the common cases (object with typed properties, enums, arrays).
// For advanced schemas, extend this struct or marshal a raw map separately.
type Schema struct {
	Type        DataType          `json:"type,omitempty"`
	Description string            `json:"description,omitempty"`
	Enum        []string          `json:"enum,omitempty"`
	Properties  map[string]Schema `json:"properties,omitempty"`
	Required    []string          `json:"required,omitempty"`
	Items       *Schema           `json:"items,omitempty"`
	Nullable    bool              `json:"nullable,omitempty"`
}
