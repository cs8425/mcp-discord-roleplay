// Package mcptransport is a reusable MCP (Model Context Protocol) server
// transport: Streamable HTTP + stdio, sharing one transport-agnostic server
// core and tool registry. It supports the modern per-request-metadata
// revision (2026-07-28) and the legacy initialize-handshake revision
// (2025-11-25). Pure Go, no CGO.
package mcptransport

import "encoding/json"

const (
	// JSONRPCVersion is the JSON-RPC version used by MCP.
	JSONRPCVersion = "2.0"

	// VersionModern is the per-request-metadata revision (no handshake;
	// server/discover; MCP-Protocol-Version/Mcp-Method/Mcp-Name headers).
	VersionModern = "2026-07-28"

	// VersionLegacy is the initialize-handshake revision.
	VersionLegacy = "2025-11-25"
)

// Request is a JSON-RPC request or notification (no ID = notification).
type Request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// Error is a JSON-RPC error object.
type Error struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

// Response is a JSON-RPC response.
type Response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *Error          `json:"error,omitempty"`
}

// Meta is the per-request _meta carried by modern (2026-07-28) requests.
type Meta struct {
	ProtocolVersion    string         `json:"io.modelcontextprotocol/protocolVersion"`
	ClientInfo         map[string]any `json:"io.modelcontextprotocol/clientInfo,omitempty"`
	ClientCapabilities map[string]any `json:"io.modelcontextprotocol/clientCapabilities"`
}

// InitializeParams is the params of a legacy initialize request.
type InitializeParams struct {
	ProtocolVersion string         `json:"protocolVersion"`
	Capabilities    map[string]any `json:"capabilities"`
	ClientInfo      map[string]any `json:"clientInfo"`
}

// CallParams is the params of a tools/call request (also used for tools/list,
// where only Meta is meaningful).
type CallParams struct {
	Meta      Meta            `json:"_meta"`
	Name      string          `json:"name,omitempty"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

func errorResponse(id json.RawMessage, code int, msg string, data any) Response {
	return Response{JSONRPC: JSONRPCVersion, ID: id, Error: &Error{Code: code, Message: msg, Data: data}}
}
