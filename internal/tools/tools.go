// Package tools holds the MCP tool definitions and handlers. Every handler
// gets the caller's router.Principal from ctx and answers with compact JSON in
// a single text content part (plus inline image parts when media=inline).
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	mcp "mcptransport"

	"mcp-discord-roleplay/internal/bus"
	"mcp-discord-roleplay/internal/config"
	"mcp-discord-roleplay/internal/discord"
	"mcp-discord-roleplay/internal/router"
)

// Deps is what a tool handler needs. It is built once in main and shared;
// every field is safe for concurrent use.
type Deps struct {
	Cfg       *config.Config
	Router    *router.Router
	Bus       *bus.Bus
	DC        *discord.Client
	Stopping  chan struct{} // closed as soon as shutdown begins
	StartedAt time.Time
	Version   string

	toolNames []string // filled in by RegisterAll, reported by discord_status
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
		// M1: {sendTool(), d.handleSend}
		// M2: {pollTool(), d.handlePoll}
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
