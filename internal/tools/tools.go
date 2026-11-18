// Package tools holds the MCP tool definitions and handlers. Every handler
// gets the caller's router.Principal from ctx and answers with compact JSON in
// a single text content part (plus inline image parts when media=inline).
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"sync"
	"time"

	mcp "mcptransport"

	"mcp-discord-roleplay/internal/bus"
	"mcp-discord-roleplay/internal/config"
	"mcp-discord-roleplay/internal/discord"
	"mcp-discord-roleplay/internal/files"
	"mcp-discord-roleplay/internal/media"
	"mcp-discord-roleplay/internal/router"
)

// Deps is what a tool handler needs. It is built once in main and shared;
// every field is safe for concurrent use.
type Deps struct {
	Cfg        *config.Config
	Router     *router.Router
	Hub        *bus.Hub // one queue per agent; the inbound fan-out target
	DC         *discord.Client
	Files      *files.Store   // sandbox for send's file paths; nil-safe, reports files_disabled
	Media      *media.Fetcher // inbound attachments for poll; nil means poll reports metadata only
	MediaSweep *media.Report  // what the startup cache sweep did; nil when it did not run
	Stopping   chan struct{}  // closed as soon as shutdown begins
	StartedAt  time.Time
	Version    string
	Log        *slog.Logger // operator-facing bookkeeping; nil falls back to a sink

	toolNames []string // filled in by RegisterAll, reported by discord_status

	// last maps an agent id to the target it most recently spoke to, which is
	// what discord_send to:"last" resolves against. Per-agent on purpose: two
	// personas sharing one server must not inherit each other's conversation.
	// Deps is only ever used through a pointer, so this lock is never copied.
	last sync.Map

	// pollAt records when each agent last drained its queue, so discord_status
	// can show which persona stopped polling (and is therefore dropping events
	// at the queue cap).
	pollMu sync.Mutex
	pollAt map[string]time.Time

	// up answers "is the gateway listening?" for the poll reason. nil means ask
	// DC, which is what production does; a test injects its own answer, because a
	// client that was never started is indistinguishable from one that died.
	up func() bool
}

// gatewayUp is the only place that reads the connection state for an answer:
// a poll that found nothing on a deaf socket says "disconnected" rather than
// the "timeout" a quiet channel deserves.
func (d *Deps) gatewayUp() bool {
	if d.up != nil {
		return d.up()
	}
	return d.DC != nil && d.DC.Connected()
}

// RegisterAll registers the tools available in this build onto reg (which is
// expected to carry the "discord_" prefix).
func RegisterAll(reg *mcp.Registry, d *Deps) error {
	d.toolNames = nil
	plan := []struct {
		tool *mcp.Tool
		fn   mcp.Handler
	}{
		{statusTool(), d.handleStatus},
		{sendTool(), d.handleSend},
		{pollTool(), d.handlePoll},
		// M3: {historyTool(), d.handleHistory}, {channelsTool(), d.handleChannels}
	}
	for _, p := range plan {
		if err := reg.Register(p.tool, p.fn); err != nil {
			return fmt.Errorf("register %s: %w", p.tool.Name, err)
		}
		d.toolNames = append(d.toolNames, p.tool.Name)
	}
	sort.Strings(d.toolNames)
	return nil
}

// principal returns the caller identity, defaulting to the process agent so a
// handler is never surprised by a missing value.
func (d *Deps) principal(ctx context.Context) router.Principal {
	p := router.FromContext(ctx)
	if p.AgentID == "" {
		p.AgentID = d.Cfg.AgentID
	}
	return p
}

// debug returns the logger a handler writes its bookkeeping line to. Wire
// answers carry only what the model needs in order to act; the rest (snowflakes,
// timings, byte counts, which channel a DM resolved to) lands here, and the
// operator raises --log-level=debug to see it. Tests build a Deps without a
// logger, so nil degrades to a sink instead of a panic.
func (d *Deps) debug() *slog.Logger {
	if d.Log == nil {
		return devNull
	}
	return d.Log
}

var devNull = slog.New(slog.NewTextHandler(io.Discard, nil))

// ---------------------------------------------------------------------------
// result helpers
// ---------------------------------------------------------------------------

// jsonResult marshals v compactly into a single text part. Map keys are sorted
// by encoding/json, so output is deterministic.
func jsonResult(v any) *mcp.ToolResult {
	b, err := json.Marshal(v)
	if err != nil {
		return mcp.ResultError("internal: cannot encode result: " + err.Error())
	}
	return mcp.ResultText(string(b))
}

// errResult builds an isError result carrying a machine-readable code plus a
// hint the model can act on.
func errResult(code, hint string, extra map[string]any) *mcp.ToolResult {
	m := map[string]any{"ok": false, "error": code}
	if hint != "" {
		m["hint"] = hint
	}
	for k, v := range extra {
		m[k] = v
	}
	res := jsonResult(m)
	res.IsError = true
	return res
}

// decodeArguments unmarshals a tools/call argument blob. An empty blob is
// accepted as an empty object so zero-argument calls work.
func decodeArguments(call *mcp.Call, target any) error {
	if len(call.Arguments) == 0 {
		return nil
	}
	if err := json.Unmarshal(call.Arguments, target); err != nil {
		return fmt.Errorf("invalid arguments: %w", err)
	}
	return nil
}
