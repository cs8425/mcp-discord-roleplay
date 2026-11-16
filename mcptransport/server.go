package mcptransport

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"
)

// maxBodyBytes caps a single MCP request body (4 MiB), matching the stdio
// line limit so neither transport is a bigger attack surface than the other.
const maxBodyBytes = 4 << 20

// Server is a transport-agnostic MCP server. The same instance can serve both
// Streamable HTTP (Handler) and stdio (NewStdioServer).
type Server struct {
	Name         string
	Version      string
	Instructions string

	// Origin is the exact CORS Origin allow-list (DNS-rebinding defence).
	// An empty map allows only non-browser clients (no Origin header); a
	// "*" entry allows all.
	Origin map[string]bool

	Reg *Registry

	// RequestTimeout bounds each MCP request end-to-end (0 disables). It caps the
	// whole tool call, on top of the per-CDP-call deadline the Core applies.
	RequestTimeout time.Duration
}

// Handler returns the Streamable HTTP handler (2026-07-28 + 2025-11-25).
func (s *Server) Handler() http.Handler {
	return http.HandlerFunc(s.handle)
}

func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	s.setCORS(w, r)

	if r.Method == http.MethodOptions {
		if origin := r.Header.Get("Origin"); origin != "" && !s.allowedOrigin(origin) {
			writePlain(w, http.StatusForbidden, "forbidden origin")
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}

	// 2026-07-28 Streamable HTTP is POST-only (GET/DELETE removed).
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	// Origin validation defends against DNS rebinding.
	if origin := r.Header.Get("Origin"); origin != "" && !s.allowedOrigin(origin) {
		writePlain(w, http.StatusForbidden, "forbidden origin")
		return
	}

	// Per the 2026-07-28 revision, Accept/Content-Type rejections happen before
	// JSON-RPC processing, so they are plain HTTP responses, not JSON-RPC bodies.
	if !acceptsStreamableHTTP(r.Header.Get("Accept")) {
		writePlain(w, http.StatusNotAcceptable, "Accept must include application/json and text/event-stream")
		return
	}

	if ct := strings.ToLower(r.Header.Get("Content-Type")); !strings.HasPrefix(ct, "application/json") {
		writePlain(w, http.StatusUnsupportedMediaType, "Content-Type must be application/json")
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
	if err != nil {
		writeRPCError(w, http.StatusBadRequest, nil, -32700, "failed to read request body", nil)
		return
	}

	var req Request
	if err := json.Unmarshal(body, &req); err != nil {
		writeRPCError(w, http.StatusBadRequest, nil, -32700, "invalid JSON", nil)
		return
	}

	if req.JSONRPC != JSONRPCVersion || req.Method == "" {
		writeRPCError(w, http.StatusBadRequest, req.ID, -32600, "invalid JSON-RPC request", nil)
		return
	}

	// Notification (no ID): accept, no body.
	if len(req.ID) == 0 {
		w.WriteHeader(http.StatusAccepted)
		return
	}

	// Modern requests carry the Mcp-Method header or a modern protocol version.
	if r.Header.Get("Mcp-Method") != "" || r.Header.Get("MCP-Protocol-Version") == VersionModern {
		s.handleModern(w, r, req)
		return
	}
	s.handleLegacy(w, r, req)
}

func (s *Server) handleModern(w http.ResponseWriter, r *http.Request, req Request) {
	version := r.Header.Get("MCP-Protocol-Version")
	if version != VersionModern {
		writeRPCError(w, http.StatusBadRequest, req.ID, -32022, "unsupported protocol version",
			map[string]any{"requested": version, "supported": []string{VersionModern, VersionLegacy}})
		return
	}

	var p CallParams
	if len(req.Params) == 0 {
		writeRPCError(w, http.StatusBadRequest, req.ID, -32602, "params must contain _meta", nil)
		return
	}
	if err := json.Unmarshal(req.Params, &p); err != nil {
		writeRPCError(w, http.StatusBadRequest, req.ID, -32602, "invalid params", nil)
		return
	}

	// Header/body version must match.
	if p.Meta.ProtocolVersion != version {
		writeRPCError(w, http.StatusBadRequest, req.ID, -32020,
			"MCP-Protocol-Version does not match _meta", nil)
		return
	}

	// Header/body method must match.
	if r.Header.Get("Mcp-Method") != req.Method {
		writeRPCError(w, http.StatusBadRequest, req.ID, -32020,
			"Mcp-Method does not match method", nil)
		return
	}

	// tools/call additionally mirrors params.name (base64-sentinel aware).
	if req.Method == "tools/call" && decodeHeaderValue(r.Header.Get("Mcp-Name")) != p.Name {
		writeRPCError(w, http.StatusBadRequest, req.ID, -32020,
			"Mcp-Name does not match params.name", nil)
		return
	}

	s.dispatch(w, r, req, VersionModern, p)
}

func (s *Server) handleLegacy(w http.ResponseWriter, r *http.Request, req Request) {
	switch req.Method {
	case "notifications/initialized":
		w.WriteHeader(http.StatusAccepted)
		return
	case "ping":
		writeResult(w, req.ID, map[string]any{})
		return
	case "initialize":
		var p InitializeParams
		if len(req.Params) > 0 {
			if err := json.Unmarshal(req.Params, &p); err != nil {
				writeRPCError(w, http.StatusBadRequest, req.ID, -32602, "invalid initialize params", nil)
				return
			}
		}
		writeResult(w, req.ID, map[string]any{
			"protocolVersion": VersionLegacy,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": s.Name, "version": s.Version},
			"instructions":    s.Instructions,
		})
		return
	}

	if version := r.Header.Get("MCP-Protocol-Version"); version != VersionLegacy {
		writeRPCError(w, http.StatusBadRequest, req.ID, -32022,
			"missing or invalid legacy MCP-Protocol-Version",
			map[string]any{"requested": version, "supported": []string{VersionLegacy}})
		return
	}

	var p CallParams
	if len(req.Params) > 0 {
		_ = json.Unmarshal(req.Params, &p)
	}
	s.dispatch(w, r, req, VersionLegacy, p)
}

func (s *Server) dispatch(w http.ResponseWriter, r *http.Request, req Request, version string, p CallParams) {
	ctx := r.Context()
	if s.RequestTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, s.RequestTimeout)
		defer cancel()
	}
	out := s.dispatchCore(ctx, req, version, p)
	if out.code != 0 {
		writeRPCError(w, out.status, req.ID, out.code, out.msg, out.data)
		return
	}
	writeResult(w, req.ID, out.result)
}

// outcome is the transport-agnostic result of one MCP method call. status is
// only meaningful for the HTTP transport.
type outcome struct {
	status int
	result any
	code   int    // JSON-RPC error code; 0 = success
	msg    string // JSON-RPC error message
	data   any    // JSON-RPC error data
}

// discoverResult is the server/discover result (2026-07-28).
type discoverResult struct {
	ResultType        string         `json:"resultType"`
	SupportedVersions []string       `json:"supportedVersions"`
	Capabilities      map[string]any `json:"capabilities"`
	Meta              map[string]any `json:"_meta"`
	Instructions      string         `json:"instructions,omitempty"`
	TTLMS             int64          `json:"ttlMs"`
	CacheScope        string         `json:"cacheScope"`
}

// listResult is the tools/list result. The cache fields are set only for the
// modern revision (omitted for legacy via omitempty).
type listResult struct {
	Tools      []*Tool `json:"tools"`
	ResultType string  `json:"resultType,omitempty"`
	TTLMS      int64   `json:"ttlMs,omitempty"`
	CacheScope string  `json:"cacheScope,omitempty"`
}

// callResult is the modern tools/call result (adds resultType).
type callResult struct {
	ResultType string        `json:"resultType"`
	Content    []ContentPart `json:"content"`
	IsError    bool          `json:"isError"`
}

// dispatchCore runs one MCP method without any transport dependency. Both
// Streamable HTTP and stdio dispatch through it.
func (s *Server) dispatchCore(ctx context.Context, req Request, version string, p CallParams) outcome {
	switch req.Method {

	case "server/discover":
		if version != VersionModern {
			return outcome{status: http.StatusNotFound, code: -32601, msg: "method not found"}
		}
		return outcome{status: http.StatusOK, result: discoverResult{
			ResultType:        "complete",
			SupportedVersions: []string{VersionModern, VersionLegacy},
			Capabilities:      map[string]any{"tools": map[string]any{}},
			Meta: map[string]any{
				"io.modelcontextprotocol/serverInfo": map[string]any{"name": s.Name, "version": s.Version},
			},
			Instructions: s.Instructions,
			TTLMS:        3600000,
			CacheScope:   "public",
		}}

	case "tools/list":
		lr := listResult{Tools: s.Reg.List()}
		if version == VersionModern {
			lr.ResultType = "complete"
			lr.TTLMS = 60000
			lr.CacheScope = "public"
		}
		return outcome{status: http.StatusOK, result: lr}

	case "tools/call":
		res, err := s.Reg.Dispatch(ctx, &Call{Name: p.Name, Arguments: p.Arguments})
		if err != nil {
			return outcome{status: http.StatusOK, code: -32602, msg: err.Error()}
		}
		if version == VersionModern {
			return outcome{status: http.StatusOK, result: callResult{
				ResultType: "complete",
				Content:    res.Content,
				IsError:    res.IsError,
			}}
		}
		return outcome{status: http.StatusOK, result: res}

	default:
		return outcome{status: http.StatusNotFound, code: -32601, msg: "method not found"}
	}
}

func (s *Server) setCORS(w http.ResponseWriter, r *http.Request) {
	origin := r.Header.Get("Origin")
	if origin == "" || !s.allowedOrigin(origin) {
		return
	}
	w.Header().Set("Access-Control-Allow-Origin", origin)
	w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers",
		"Content-Type, MCP-Protocol-Version, Mcp-Method, Mcp-Name, Authorization")
	w.Header().Set("Access-Control-Expose-Headers", "Mcp-Session-Id")
	w.Header().Add("Vary", "Origin")
}

func (s *Server) allowedOrigin(origin string) bool {
	return s.Origin[origin] || s.Origin["*"]
}

func acceptsStreamableHTTP(value string) bool {
	value = strings.ToLower(value)
	return strings.Contains(value, "application/json") &&
		strings.Contains(value, "text/event-stream")
}

// decodeHeaderValue decodes a base64-sentinel header value (=?base64?...?=)
// if present, per the MCP Value Encoding rules.
func decodeHeaderValue(v string) string {
	const pre, suf = "=?base64?", "?="
	if strings.HasPrefix(v, pre) && strings.HasSuffix(v, suf) && len(v) >= len(pre)+len(suf) {
		if dec, err := base64.StdEncoding.DecodeString(v[len(pre) : len(v)-len(suf)]); err == nil {
			return string(dec)
		}
	}
	return v
}

func writeResult(w http.ResponseWriter, id json.RawMessage, result any) {
	writeRPCStatus(w, http.StatusOK, Response{JSONRPC: JSONRPCVersion, ID: id, Result: result})
}

func writeRPCError(w http.ResponseWriter, status int, id json.RawMessage, code int, message string, data any) {
	writeRPCStatus(w, status, Response{
		JSONRPC: JSONRPCVersion,
		ID:      id,
		Error:   &Error{Code: code, Message: message, Data: data},
	})
}

func writeRPCStatus(w http.ResponseWriter, status int, resp Response) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	// Stream directly to the writer (no intermediate []byte). The encoder
	// calls ContentPart.MarshalJSONTo for content parts. A trailing newline
	// is emitted, which is valid JSON whitespace and required for stdio.
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		// Headers are already sent; resp is our own (always-marshalable) type,
		// so this is not expected in practice.
		_ = err
	}
}

func writePlain(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}
