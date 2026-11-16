package mcptransport

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"sync"
	"time"
)

// StdioServer serves the MCP protocol over stdin/stdout.
//
// Framing (2026-07-28 stdio): one JSON-RPC message per line, no embedded
// newlines; requests/notifications on stdin, responses on stdout; stdout
// carries nothing but MCP messages (logs go to stderr); the server exits
// when stdin reaches EOF.
//
// stdio has no header layer: modern requests carry their protocol version
// inline in _meta on every request; legacy clients do the initialize
// handshake once per process.
type StdioServer struct {
	server *Server

	in  io.Reader
	out io.Writer

	outMu sync.Mutex
	outBw *bufio.Writer

	mu      sync.Mutex
	version string // negotiated legacy version, "" until initialize

	inflightMu sync.Mutex
	inflight   map[string]context.CancelFunc

	wg sync.WaitGroup
}

// NewStdioServer wraps an MCP server with the stdio transport.
func NewStdioServer(s *Server) *StdioServer {
	return &StdioServer{
		server:   s,
		in:       os.Stdin,
		out:      os.Stdout,
		inflight: make(map[string]context.CancelFunc),
	}
}

// SetIO overrides stdin/stdout (for tests).
func (s *StdioServer) SetIO(in io.Reader, out io.Writer) { s.in, s.out = in, out }

// Serve reads newline-delimited JSON-RPC from stdin until EOF and writes
// responses to stdout. It returns when stdin closes (the primary graceful
// shutdown signal for stdio).
func (s *StdioServer) Serve() error {
	s.outMu.Lock()
	if s.outBw == nil {
		s.outBw = bufio.NewWriter(s.out)
	}
	bw := s.outBw
	s.outMu.Unlock()

	scanner := bufio.NewScanner(s.in)
	scanner.Buffer(make([]byte, 64*1024), 4<<20) // same 4 MiB cap as HTTP

	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		s.handleLine(line, bw)
	}

	// Bounded grace so in-flight responses flush before exit.
	finished := make(chan struct{})
	go func() { s.wg.Wait(); close(finished) }()
	select {
	case <-finished:
	case <-time.After(10 * time.Second):
	}

	s.outMu.Lock()
	_ = bw.Flush()
	s.outMu.Unlock()
	return scanner.Err()
}

func (s *StdioServer) handleLine(line []byte, bw *bufio.Writer) {
	var msg Request
	if err := json.Unmarshal(line, &msg); err != nil {
		s.writeMessage(bw, Response{JSONRPC: JSONRPCVersion,
			Error: &Error{Code: -32700, Message: "parse error: " + err.Error()}})
		return
	}
	if msg.JSONRPC != JSONRPCVersion {
		s.writeMessage(bw, Response{JSONRPC: JSONRPCVersion, ID: msg.ID,
			Error: &Error{Code: -32600, Message: "invalid request: jsonrpc must be 2.0"}})
		return
	}

	// Notification (no ID): no response.
	if len(msg.ID) == 0 {
		s.handleNotification(msg)
		return
	}

	// Requests run in their own goroutine (one slow tool can't block the read
	// loop) and can be cancelled by ID.
	var ctx context.Context
	var cancel context.CancelFunc
	if s.server.RequestTimeout > 0 {
		ctx, cancel = context.WithTimeout(context.Background(), s.server.RequestTimeout)
	} else {
		ctx, cancel = context.WithCancel(context.Background())
	}
	s.track(msg.ID, cancel)
	s.wg.Add(1)
	go func() {
		defer cancel()
		defer s.untrack(msg.ID)
		defer s.wg.Done()
		resp := s.dispatchRequest(ctx, msg)
		// Stay silent ONLY when the client explicitly cancelled this request
		// (notifications/cancelled -> context.Canceled); MCP allows dropping the
		// reply there. When OUR RequestTimeout fires it is context.DeadlineExceeded
		// and the client is still waiting for a result — discarding that response
		// was the bug that turned a timeout into a permanent hang (no goroutine
		// left behind, nothing ever written to stdout). So always reply unless the
		// client told us to stop.
		if ctx.Err() == context.Canceled {
			return
		}
		s.writeMessage(bw, resp)
	}()
}

func (s *StdioServer) dispatchRequest(ctx context.Context, msg Request) Response {
	switch msg.Method {

	case "server/discover":
		var p CallParams
		if len(msg.Params) > 0 {
			_ = json.Unmarshal(msg.Params, &p)
		}
		if v := p.Meta.ProtocolVersion; v != "" && v != VersionModern {
			return errorResponse(msg.ID, -32022, "unsupported protocol version",
				map[string]any{"requested": v, "supported": []string{VersionModern, VersionLegacy}})
		}
		return s.run(ctx, msg, VersionModern, p)

	case "initialize":
		return s.handleInitialize(msg)

	case "ping":
		return Response{JSONRPC: JSONRPCVersion, ID: msg.ID, Result: map[string]any{}}

	case "tools/list", "tools/call":
		var p CallParams
		if len(msg.Params) > 0 {
			if err := json.Unmarshal(msg.Params, &p); err != nil {
				return errorResponse(msg.ID, -32602, "invalid params", nil)
			}
		}
		// Self-describing modern request?
		if v := p.Meta.ProtocolVersion; v != "" {
			if v != VersionModern {
				return errorResponse(msg.ID, -32022, "unsupported protocol version",
					map[string]any{"requested": v, "supported": []string{VersionModern, VersionLegacy}})
			}
			return s.run(ctx, msg, VersionModern, p)
		}
		// Legacy: requires the initialize handshake first.
		s.mu.Lock()
		version := s.version
		s.mu.Unlock()
		if version == "" {
			return errorResponse(msg.ID, -32600, "not initialized: send initialize first", nil)
		}
		return s.run(ctx, msg, version, p)

	default:
		return errorResponse(msg.ID, -32601, "method not found", nil)
	}
}

func (s *StdioServer) run(ctx context.Context, msg Request, version string, p CallParams) Response {
	out := s.server.dispatchCore(ctx, msg, version, p)
	if out.code != 0 {
		return errorResponse(msg.ID, out.code, out.msg, out.data)
	}
	return Response{JSONRPC: JSONRPCVersion, ID: msg.ID, Result: out.result}
}

func (s *StdioServer) handleInitialize(msg Request) Response {
	var p InitializeParams
	if len(msg.Params) > 0 {
		if err := json.Unmarshal(msg.Params, &p); err != nil {
			return errorResponse(msg.ID, -32602, "invalid initialize params", nil)
		}
	}
	// Negotiate the session version. Honour an explicit modern handshake;
	// otherwise fall back to the legacy handshake (older clients). The
	// negotiated version is the default for later requests that carry no
	// inline _meta.
	negotiated := VersionLegacy
	if p.ProtocolVersion == VersionModern {
		negotiated = VersionModern
	}
	s.mu.Lock()
	s.version = negotiated
	s.mu.Unlock()
	return Response{
		JSONRPC: JSONRPCVersion,
		ID:      msg.ID,
		Result: map[string]any{
			"protocolVersion": negotiated,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": s.server.Name, "version": s.server.Version},
			"instructions":    s.server.Instructions,
		},
	}
}

func (s *StdioServer) handleNotification(msg Request) {
	// The only core client notification is notifications/cancelled (stdio).
	if msg.Method == "notifications/cancelled" {
		var c struct {
			RequestID json.RawMessage `json:"requestId"`
		}
		if len(msg.Params) > 0 {
			_ = json.Unmarshal(msg.Params, &c)
		}
		if len(c.RequestID) > 0 {
			s.cancel(string(c.RequestID))
		}
	}
}

func (s *StdioServer) track(id json.RawMessage, cancel context.CancelFunc) {
	s.inflightMu.Lock()
	s.inflight[string(id)] = cancel
	s.inflightMu.Unlock()
}

func (s *StdioServer) untrack(id json.RawMessage) {
	s.inflightMu.Lock()
	delete(s.inflight, string(id))
	s.inflightMu.Unlock()
}

func (s *StdioServer) cancel(id string) {
	s.inflightMu.Lock()
	c, ok := s.inflight[id]
	s.inflightMu.Unlock()
	if ok {
		c()
	}
}

func (s *StdioServer) writeMessage(bw *bufio.Writer, resp Response) {
	s.outMu.Lock()
	defer s.outMu.Unlock()
	// Stream directly to the buffered writer (no intermediate []byte).
	// Encode appends the trailing newline required for stdio framing.
	if err := json.NewEncoder(bw).Encode(resp); err != nil {
		return
	}
	_ = bw.Flush()
}
