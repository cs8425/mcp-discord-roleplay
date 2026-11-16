package tools

import (
	"context"
	"strconv"
	"time"

	mcp "mcptransport"

	"mcp-discord-roleplay/internal/bus"
	"mcp-discord-roleplay/internal/config"
	"mcp-discord-roleplay/internal/discord"
	"mcp-discord-roleplay/internal/router"
	"mcp-discord-roleplay/internal/tz"
)

// BuildMilestone marks how much of the tool surface exists in this binary.
const BuildMilestone = "M0"

func statusTool() *mcp.Tool {
	return &mcp.Tool{
		Name:  "status",
		Title: "Discord bridge status",
		Description: "Report the Discord bridge state: gateway connection, bot identity, " +
			"event buffer usage, and which targets this agent may read or write. " +
			"Call it once before sending or polling to learn the alias names you are allowed to use. " +
			"detail=full also returns the resolved configuration (token is always redacted).",
		InputSchema: mcp.Schema{
			Type:        mcp.TypeObject,
			Description: "No required arguments.",
			Properties: map[string]mcp.Schema{
				"detail": {
					Type:        mcp.TypeString,
					Enum:        []string{"brief", "full"},
					Description: "brief (default) = connection + routes only; full = also the whole resolved config and limits.",
				},
			},
		},
	}
}

type statusArgs struct {
	Detail string `json:"detail"`
}

func (d *Deps) handleStatus(ctx context.Context, call *mcp.Call) (*mcp.ToolResult, error) {
	var a statusArgs
	if err := decodeArguments(call, &a); err != nil {
		return errResult("bad_arguments", err.Error(), nil), nil
	}
	full := a.Detail == "full"
	p := d.principal(ctx)

	snap := d.DC.Snapshot()
	stats := d.Bus.Stats()

	out := map[string]any{
		"ok": true,
		"server": map[string]any{
			"name":      config.Module,
			"version":   d.Version,
			"milestone": BuildMilestone,
			"agent":     p.AgentID,
			"uptime_s":  int64(time.Since(d.StartedAt).Seconds()),
			"now":       tz.Format(tz.Now()),
		},
		"self": map[string]any{
			"id":   snap.SelfID,
			"name": snap.SelfName,
			"tag":  snap.SelfTag,
		},
		"gateway": map[string]any{
			"connected":          snap.Connected && snap.Ready,
			"started":            snap.Started,
			"ready_at":           snap.ReadyAtS,
			"session_id":         snap.SessionID,
			"endpoint":           snap.Gateway,
			"last_event_age_s":   snap.LastEventAgoS,
			"heartbeat_age_s":    snap.HeartbeatAgoS,
			"ready_count":        snap.ReadyCount,
			"resumes":            snap.Resumes,
			"disconnects":        snap.Disconnects,
			"reconnect_attempts": snap.Reconnects,
			"last_error":         snap.LastError,
			"last_error_age_s":   snap.LastErrorAgoS,
			"baseline_cursor":    bus.Cursor(snap.BaselineSeq),
		},
		"bus": map[string]any{
			"size":        stats.Size,
			"capacity":    stats.Capacity,
			"total":       stats.Total,
			"dropped":     stats.Dropped,
			"max_seq":     stats.MaxSeq,
			"head_cursor": bus.Cursor(stats.MaxSeq),
		},
		"routing": d.Router.Summary(p),
		"tools":   d.toolNames,
	}

	select {
	case <-d.Stopping:
		out["stopping"] = true
	default:
	}

	if full {
		out["config"] = d.Cfg.Audit()
		out["intents"] = discord.IntentsList()
	}

	if w := d.warnings(snap, stats, p); len(w) > 0 {
		out["warnings"] = w
	}
	return jsonResult(out), nil
}

// warnings turns the usual foot-guns into short actionable lines.
func (d *Deps) warnings(snap discord.Snapshot, stats bus.Stats, p router.Principal) []string {
	var w []string
	if !snap.Started {
		w = append(w, "gateway was never started (--selftest build or start failure); no messages will arrive")
	} else if !snap.Connected || !snap.Ready {
		w = append(w, "gateway is not connected: check the token in "+d.Cfg.TokenSource+
			", and that the bot is still in the guild/DM you target")
	}
	if snap.SelfID == "" && snap.Started {
		w = append(w, "READY has not arrived yet: wait a second and call discord_status again")
	}
	if !d.Cfg.AllowDM {
		w = append(w, "--allow-dm=false: inbound DMs are being dropped entirely")
	}
	if !d.Router.HasRouting() && len(d.Cfg.AllowChannels) == 0 && len(d.Cfg.AllowGuilds) == 0 {
		w = append(w, "no routing.json and no allow-lists: every guild channel this bot can see is admitted to the ring")
	}
	if stats.Dropped > 0 {
		w = append(w, "ring buffer overflow dropped "+strconv.FormatUint(stats.Dropped, 10)+
			" events: raise --bus-capacity or poll more often (cursors cannot recover dropped events)")
	}
	if !d.Cfg.CanInline() {
		w = append(w, "inline media is disabled by policy: images will only be listed by URL")
	} else if d.Cfg.MediaFor(config.KindImage) == config.MediaSkip {
		w = append(w, "media-image=skip: inbound images are not downloaded, only their URLs are listed")
	}
	if len(d.Cfg.FileRoots) == 0 {
		w = append(w, "--no-file-root is in effect: send cannot attach local files, only url targets work")
	}
	if d.Cfg.DryRun {
		w = append(w, "dry-run mode: sends are logged and not delivered")
	}
	if p.AgentID == "" {
		w = append(w, "request carries no agent id; falling back to the process default")
	} else if !d.Router.DeclaresAgent(p.AgentID) {
		w = append(w, "agent '"+p.AgentID+"' is not declared in routing.json: it can see no target (run with --agent <id> from the file's agents list)")
	}
	return w
}
