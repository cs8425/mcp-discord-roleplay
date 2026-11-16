package mcptransport

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func newTestServer(t *testing.T) *Server {
	t.Helper()
	reg := NewRegistry("test_")
	if err := reg.Register(&Tool{
		Name:        "echo",
		Description: "echo the input",
		InputSchema: Schema{
			Type:       TypeObject,
			Properties: map[string]Schema{"msg": {Type: TypeString}},
			Required:   []string{"msg"},
		},
	}, func(ctx context.Context, c *Call) (*ToolResult, error) {
		var a struct{ Msg string }
		_ = json.Unmarshal(c.Arguments, &a)
		return ResultText("echo: " + a.Msg), nil
	}); err != nil {
		t.Fatal(err)
	}
	return &Server{Name: "test", Version: "0.0.1", Reg: reg, Origin: map[string]bool{"*": true}}
}

// ---------- stdio ----------

func stdioRoundTrip(t *testing.T, srv *Server, lines []string) map[string]Response {
	t.Helper()
	pr, pw := io.Pipe()
	qr, qw := io.Pipe()
	ss := NewStdioServer(srv)
	ss.SetIO(pr, qw)

	scan := bufio.NewScanner(qr)
	scan.Buffer(make([]byte, 0, 64*1024), 1<<20)

	done := make(chan error, 1)
	go func() { done <- ss.Serve() }()

	// Send each line; after a request (a line with an id) read exactly one
	// response. This serialises the legacy handshake (initialize must set the
	// negotiated version before tools/list/call read it) and lets us match
	// responses by id (JSON-RPC permits out-of-order responses).
	resps := map[string]Response{}
	for _, l := range lines {
		if _, err := pw.Write([]byte(l + "\n")); err != nil {
			t.Fatalf("write: %v", err)
		}
		if isRequest(l) {
			if !scan.Scan() {
				t.Fatalf("no response for request: %s", l)
			}
			var r Response
			if err := json.Unmarshal(scan.Bytes(), &r); err != nil {
				t.Fatalf("unmarshal response: %v: %s", err, scan.Bytes())
			}
			resps[string(r.ID)] = r
		}
	}
	pw.Close() // EOF -> Serve returns
	if err := <-done; err != nil {
		t.Fatalf("serve: %v", err)
	}
	return resps
}

// isRequest reports whether a JSON-RPC line carries an id (expects a
// response), as opposed to a notification.
func isRequest(line string) bool {
	var m struct {
		ID json.RawMessage `json:"id"`
	}
	_ = json.Unmarshal([]byte(line), &m)
	return len(m.ID) > 0
}

// TestStdioTimeoutNotSwallowed is the regression for the permanent hang: when
// the server's RequestTimeout fires, the (late) response MUST still be written
// to the client. The old code dropped it under `if ctx.Err() != nil`, leaving
// the client waiting forever with no goroutine left in the server. Here the
// handler sleeps past the cap and returns normally, so at reply time
// ctx.Err()==DeadlineExceeded and the response must still be delivered.
func TestStdioTimeoutNotSwallowed(t *testing.T) {
	reg := NewRegistry("test_")
	if err := reg.Register(&Tool{
		Name:        "slow",
		Description: "sleeps past the request timeout then returns",
		InputSchema: Schema{Type: TypeObject},
	}, func(ctx context.Context, c *Call) (*ToolResult, error) {
		time.Sleep(150 * time.Millisecond) // ignores ctx: finishes after the cap
		return ResultText("late result"), nil
	}); err != nil {
		t.Fatal(err)
	}
	srv := &Server{Name: "test", Version: "0.0.1", Reg: reg,
		Origin: map[string]bool{"*": true}, RequestTimeout: 50 * time.Millisecond}

	pr, pw := io.Pipe()
	qr, qw := io.Pipe()
	ss := NewStdioServer(srv)
	ss.SetIO(pr, qw)
	go ss.Serve()

	req := `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"test_slow","arguments":{},"_meta":{"io.modelcontextprotocol/protocolVersion":"` + VersionModern + `"}}}`
	if _, err := pw.Write([]byte(req + "\n")); err != nil {
		t.Fatalf("write: %v", err)
	}

	got := make(chan Response, 1)
	go func() {
		scan := bufio.NewScanner(qr)
		if scan.Scan() {
			var r Response
			_ = json.Unmarshal(scan.Bytes(), &r)
			got <- r
		}
	}()
	select {
	case r := <-got:
		if string(r.ID) != "7" {
			t.Errorf("response id = %s, want 7", r.ID)
		}
		if r.Error != nil || r.Result == nil {
			t.Errorf("want a delivered result, got err=%v result=%v", r.Error, r.Result)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout response was swallowed: the client would hang forever")
	}
	pw.Close()
}

func TestStdioLegacy(t *testing.T) {
	srv := newTestServer(t)
	lines := []string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"t","version":"0"}}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"test_echo","arguments":{"msg":"hi"}}}`,
	}
	resps := stdioRoundTrip(t, srv, lines)
	if len(resps) != 3 { // initialized is a notification (no response)
		t.Fatalf("want 3 responses, got %d", len(resps))
	}
	// initialize (id=1)
	if r, _ := resps["1"].Result.(map[string]any); r["protocolVersion"] != VersionLegacy {
		t.Errorf("initialize protocolVersion = %v", r["protocolVersion"])
	}
	// tools/list (id=2; after JSON round-trip, tools is []any)
	rl, _ := resps["2"].Result.(map[string]any)
	tools, _ := rl["tools"].([]any)
	if len(tools) != 1 {
		t.Errorf("tools/list = %v", rl["tools"])
	} else if tool, _ := tools[0].(map[string]any); tool["name"] != "test_echo" {
		t.Errorf("tool name = %v", tool["name"])
	}
	// tools/call (id=3; result is a JSON round-tripped map[string]any)
	if got := resultText(t, resps["3"].Result); got != "echo: hi" {
		t.Errorf("tools/call text = %q", got)
	}
}

func TestStdioModern(t *testing.T) {
	srv := newTestServer(t)
	lines := []string{
		`{"jsonrpc":"2.0","id":1,"method":"server/discover","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28"}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28"}}}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"test_echo","arguments":{"msg":"yo"},"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28"}}}`,
	}
	resps := stdioRoundTrip(t, srv, lines)
	if len(resps) != 3 {
		t.Fatalf("want 3 responses, got %d", len(resps))
	}
	// discover (id=1)
	d, _ := resps["1"].Result.(map[string]any)
	if d["resultType"] != "complete" {
		t.Errorf("discover resultType = %v", d["resultType"])
	}
	// tools/list (id=2) modern has resultType
	rl, _ := resps["2"].Result.(map[string]any)
	if rl["resultType"] != "complete" {
		t.Errorf("tools/list resultType = %v", rl["resultType"])
	}
	// tools/call (id=3) modern has resultType + content
	rc, _ := resps["3"].Result.(map[string]any)
	if rc["resultType"] != "complete" {
		t.Errorf("tools/call resultType = %v", rc["resultType"])
	}
	content, _ := rc["content"].([]any)
	if len(content) != 1 {
		t.Errorf("tools/call content = %v", rc["content"])
	}
}

func TestStdioLegacyNotInitialized(t *testing.T) {
	srv := newTestServer(t)
	lines := []string{
		`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`,
	}
	resps := stdioRoundTrip(t, srv, lines)
	if len(resps) != 1 {
		t.Fatalf("want 1 response, got %d", len(resps))
	}
	if resps["1"].Error == nil || resps["1"].Error.Code != -32600 {
		t.Fatalf("want not-initialized error, got %v", resps["1"])
	}
}

// ---------- Streamable HTTP ----------

// resultText extracts the first text part from a JSON round-tripped tool result.
func resultText(t *testing.T, result any) string {
	t.Helper()
	m, ok := result.(map[string]any)
	if !ok {
		t.Fatalf("result is %T, want map[string]any", result)
	}
	content, _ := m["content"].([]any)
	if len(content) == 0 {
		t.Fatalf("no content in result: %v", m)
	}
	part, _ := content[0].(map[string]any)
	text, _ := part["text"].(string)
	return text
}

func httpPost(t *testing.T, url string, headers map[string]string, body any) (int, Response) {
	t.Helper()
	b, _ := json.Marshal(body)
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	var r Response
	_ = json.Unmarshal(rb, &r)
	return resp.StatusCode, r
}

func TestHTTPLegacy(t *testing.T) {
	srv := newTestServer(t)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// initialize
	st, r := httpPost(t, ts.URL, map[string]string{}, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "initialize",
		"params": map[string]any{"protocolVersion": VersionLegacy, "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "t"}},
	})
	if st != 200 || r.Result == nil {
		t.Fatalf("initialize: status=%d resp=%v", st, r)
	}
	// tools/list (legacy header)
	st, r = httpPost(t, ts.URL, map[string]string{"MCP-Protocol-Version": VersionLegacy},
		map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/list"})
	if st != 200 {
		t.Fatalf("tools/list status=%d", st)
	}
	// tools/call
	st, r = httpPost(t, ts.URL, map[string]string{"MCP-Protocol-Version": VersionLegacy},
		map[string]any{"jsonrpc": "2.0", "id": 3, "method": "tools/call",
			"params": map[string]any{"name": "test_echo", "arguments": map[string]any{"msg": "http"}}})
	if st != 200 {
		t.Fatalf("tools/call status=%d", st)
	}
	// tools/call (JSON round-trip -> map[string]any)
	if got := resultText(t, r.Result); got != "echo: http" {
		t.Errorf("tools/call text = %q", got)
	}
}

func TestHTTPModern(t *testing.T) {
	srv := newTestServer(t)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	meta := map[string]any{"io.modelcontextprotocol/protocolVersion": VersionModern}

	// server/discover
	st, r := httpPost(t, ts.URL,
		map[string]string{"MCP-Protocol-Version": VersionModern, "Mcp-Method": "server/discover"},
		map[string]any{"jsonrpc": "2.0", "id": 1, "method": "server/discover", "params": map[string]any{"_meta": meta}})
	if st != 200 || r.Error != nil {
		t.Fatalf("discover: status=%d err=%v", st, r.Error)
	}
	d, _ := r.Result.(map[string]any)
	if d["resultType"] != "complete" {
		t.Errorf("discover resultType = %v", d["resultType"])
	}

	// tools/call with Mcp-Name
	st, r = httpPost(t, ts.URL,
		map[string]string{"MCP-Protocol-Version": VersionModern, "Mcp-Method": "tools/call", "Mcp-Name": "test_echo"},
		map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/call",
			"params": map[string]any{"name": "test_echo", "arguments": map[string]any{"msg": "m"}, "_meta": meta}})
	if st != 200 || r.Error != nil {
		t.Fatalf("tools/call: status=%d err=%v", st, r.Error)
	}
	rc, _ := r.Result.(map[string]any)
	if rc["resultType"] != "complete" {
		t.Errorf("tools/call resultType = %v", rc["resultType"])
	}
}

func TestHTTPModernHeaderMismatch(t *testing.T) {
	srv := newTestServer(t)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	meta := map[string]any{"io.modelcontextprotocol/protocolVersion": VersionModern}
	// Mcp-Name does not match params.name -> -32020
	st, r := httpPost(t, ts.URL,
		map[string]string{"MCP-Protocol-Version": VersionModern, "Mcp-Method": "tools/call", "Mcp-Name": "wrong_name"},
		map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call",
			"params": map[string]any{"name": "test_echo", "arguments": map[string]any{}, "_meta": meta}})
	if st != 400 || r.Error == nil || r.Error.Code != -32020 {
		t.Fatalf("want 400/-32020, got status=%d err=%v", st, r.Error)
	}
}

func TestHTTPRejectsGet(t *testing.T) {
	srv := newTestServer(t)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	resp, err := http.Get(ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET status = %d, want 405", resp.StatusCode)
	}
}

func TestHTTPNotification202(t *testing.T) {
	srv := newTestServer(t)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	st, _ := httpPost(t, ts.URL, map[string]string{},
		map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"})
	if st != http.StatusAccepted {
		t.Errorf("notification status = %d, want 202", st)
	}
}
