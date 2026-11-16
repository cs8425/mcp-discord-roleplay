package mcptransport

import "encoding/json/jsontext"

// ContentPart is one item in a tool result's content array.
//
// It implements the encoding/json v2 Marshaler interface (MarshalJSONTo),
// which Go 1.27's default encoding/json calls directly — streaming the part
// into the enclosing encoder with no intermediate []byte, no reflection, no
// interface boxing.
//
// A custom encoder (rather than struct tags + omitempty) is required because
// omitempty would drop fields that are mandatory but legitimately empty: a
// text part MUST carry "text" (which may be ""), an image/audio part MUST
// carry "data" and "mimeType", a resource_link MUST carry "uri".
type ContentPart struct {
	Type        string `json:"type"`
	Text        string `json:"text,omitempty"`
	Data        string `json:"data,omitempty"`     // base64, for image/audio
	MimeType    string `json:"mimeType,omitempty"` // for image/audio/resource_link
	URI         string `json:"uri,omitempty"`      // for resource_link
	Name        string `json:"name,omitempty"`     // for resource_link
	Description string `json:"description,omitempty"`
}

// MarshalJSONTo implements encoding/json v2's Marshaler. Value receiver so
// that elements of []ContentPart (values) trigger it, not just *ContentPart.
// It emits the required fields for each type, keeping empty-but-required
// values (e.g. text:"" for a text part).
func (p ContentPart) MarshalJSONTo(enc *jsontext.Encoder) error {
	if err := enc.WriteToken(jsontext.BeginObject); err != nil {
		return err
	}
	if err := enc.WriteToken(jsontext.String("type")); err != nil {
		return err
	}
	if err := enc.WriteToken(jsontext.String(p.Type)); err != nil {
		return err
	}
	switch p.Type {
	case "text":
		if err := enc.WriteToken(jsontext.String("text")); err != nil {
			return err
		}
		if err := enc.WriteToken(jsontext.String(p.Text)); err != nil {
			return err
		}
	case "image", "audio":
		if err := enc.WriteToken(jsontext.String("mimeType")); err != nil {
			return err
		}
		if err := enc.WriteToken(jsontext.String(p.MimeType)); err != nil {
			return err
		}
		if err := enc.WriteToken(jsontext.String("data")); err != nil {
			return err
		}
		if err := enc.WriteToken(jsontext.String(p.Data)); err != nil {
			return err
		}
	case "resource_link":
		if err := enc.WriteToken(jsontext.String("uri")); err != nil {
			return err
		}
		if err := enc.WriteToken(jsontext.String(p.URI)); err != nil {
			return err
		}
		if p.Name != "" {
			if err := enc.WriteToken(jsontext.String("name")); err != nil {
				return err
			}
			if err := enc.WriteToken(jsontext.String(p.Name)); err != nil {
				return err
			}
		}
		if p.Description != "" {
			if err := enc.WriteToken(jsontext.String("description")); err != nil {
				return err
			}
			if err := enc.WriteToken(jsontext.String(p.Description)); err != nil {
				return err
			}
		}
		if p.MimeType != "" {
			if err := enc.WriteToken(jsontext.String("mimeType")); err != nil {
				return err
			}
			if err := enc.WriteToken(jsontext.String(p.MimeType)); err != nil {
				return err
			}
		}
	}
	return enc.WriteToken(jsontext.EndObject)
}

// Text returns a text content part. text may be empty (it is always emitted).
func Text(s string) ContentPart { return ContentPart{Type: "text", Text: s} }

// Image returns an image content part (data is base64).
func Image(data, mime string) ContentPart {
	return ContentPart{Type: "image", Data: data, MimeType: mime}
}

// Audio returns an audio content part (data is base64).
func Audio(data, mime string) ContentPart {
	return ContentPart{Type: "audio", Data: data, MimeType: mime}
}

// ResourceLink returns a resource_link content part.
func ResourceLink(uri, name, desc, mime string) ContentPart {
	return ContentPart{Type: "resource_link", URI: uri, Name: name, Description: desc, MimeType: mime}
}

// ToolResult is the result of a tools/call.
type ToolResult struct {
	Content []ContentPart `json:"content"`
	IsError bool          `json:"isError,omitempty"`
}

// ResultText builds a successful text result.
func ResultText(s string) *ToolResult {
	return &ToolResult{Content: []ContentPart{Text(s)}}
}

// ResultError builds a tool-execution error result (isError=true) that the
// model can read and self-correct from.
func ResultError(s string) *ToolResult {
	return &ToolResult{Content: []ContentPart{Text(s)}, IsError: true}
}

// ResultImage builds a successful image result (data is base64). Returned to
// the model as an inline image content block so a vision-capable LLM can look
// at the page directly.
func ResultImage(data, mime string) *ToolResult {
	return &ToolResult{Content: []ContentPart{Image(data, mime)}}
}
