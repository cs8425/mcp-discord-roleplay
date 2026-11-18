// status.go exposes the one tool whose entire job is "what happened to my
// messages". The answer is split by concern — connection / send / receive /
// limits / routing — so an agent that is debugging a failed send is not made to
// wade through the inbound queues, and so each block can be extended without
// breaking the others.
//
// The receive block describes ONE queue: the caller's. Since M2 each agent has
// its own queue and taking a message out is the acknowledgement, so there is no
// sequence number and no cursor to report — M1's baseline_cursor/head_cursor are
// gone, replaced by depth, drops and how long ago that agent last polled.
// Another agent's unread messages are deliberately not visible here.

package tools

import (
	"context"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	mcp "mcptransport"

	"mcp-discord-roleplay/internal/bus"
	"mcp-discord-roleplay/internal/config"
	"mcp-discord-roleplay/internal/discord"
	"mcp-discord-roleplay/internal/router"
	"mcp-discord-roleplay/internal/tz"
)

// startedAt is the fallback process start time for a Deps built without one
// (unit tests); main always passes Deps.StartedAt.
var startedAt = time.Now()

// statusTool returns the discord_status registration.
func statusTool() *mcp.Tool {
	return &mcp.Tool{
		Name:  "status",
		Title: "Discord bridge health and your queue",
		Description: "Report connection health, this agent's message queue, " +
			"recent errors and the configured limits. Call it before anything else " +
			"when something seems wrong, or when you need a target alias.",
		InputSchema: mcp.Schema{
			Type:        mcp.TypeObject,
			Description: "Discord roleplay bridge status snapshot",
			// No parameters: the only input that matters is the caller
			// identity, and that arrives as _meta.agentId on the request.
			Properties: map[string]mcp.Schema{},
		},
	}
}

// handleStatus answers the status query. The caller's agent id (from
// _meta.agentId) scopes which routing rules and which queue are "theirs".
func (d *Deps) handleStatus(ctx context.Context, call *mcp.Call) (*mcp.ToolResult, error) {
	// A Deps without a client is legal (selftest, unit tests): answer honestly
	// with "gateway never started" instead of panicking on a nil pointer.
	var snap discord.Snapshot
	if d.DC != nil {
		snap = d.DC.Snapshot()
	}
	qs := d.Hub.Stats()

	p := d.principal(ctx)
	own, hasQueue := findQueueStat(qs, p.AgentID)
	if !hasQueue {
		// Report the numbers the process default would see rather than zeros,
		// but say so explicitly: a caller asking about an agent with no queue is
		// nearly always a misconfigured --agent.
		own, _ = findQueueStat(qs, d.Cfg.AgentID)
	}

	lim := discord.SendStats{}
	if d.DC != nil {
		lim = d.DC.Stats()
	}

	out := map[string]any{
		"ok": true,

		// --- connection ---------------------------------------------------
		"connection": map[string]any{
			"connected":        snap.Connected,
			"ready":            snap.Ready,
			"gateway_started":  snap.Started,
			"ready_at":         snap.ReadyAtS,
			"ready_count":      snap.ReadyCount,
			"resumes":          snap.Resumes,
			"disconnects":      snap.Disconnects,
			"reconnects":       snap.Reconnects,
			"session_id":       snap.SessionID,
			"gateway":          snap.Gateway,
			"heartbeat_age_s":  snap.HeartbeatAgoS,
			"self_id":          snap.SelfID,
			"self_name":        snap.SelfName,
			"self_tag":         snap.SelfTag,
			"last_error":       snap.LastError,
			"last_error_age_s": snap.LastErrorAgoS,
			"intents":          discord.IntentsList(d.Cfg.MessageContentEnabled()),
		},

		// --- send health ----------------------------------------------------
		"send": map[string]any{
			"budget_left":       lim.BudgetLeft,
			"sends_per_min":     lim.SendsPerMin,
			"gap_ms":            lim.GapMS,
			"breaker_open":      lim.BreakerOpen,
			"consecutive_429":   lim.Consecutive429,
			"breaker_hold_s":    lim.BreakerHoldS,
			"retry_after_ms":    lim.RetryAfterMS,
			"probing":           lim.Probing,
			"max_files_per_msg": d.Cfg.MaxFilesPerMsg,
			"max_upload_bytes":  d.Cfg.MaxUploadBytes,
			"file_roots":        d.Cfg.FileRoots,
			"mentions":          "none (roles, users and @everyone are always stripped)",
		},

		// --- receive: the caller's own queue --------------------------------
		"receive": map[string]any{
			"agent":              p.AgentID,
			"has_queue":          hasQueue,
			"unread":             own.Size,
			"dropped":            own.Dropped,
			"pushed":             own.Pushed,
			"capacity":           own.Capacity,
			"unread_all_agents":  queueSizeTotal(qs),
			"dropped_all_agents": queueDroppedTotal(qs),
			"agents":             d.Hub.Agents(),
			// How long ago a poll for this agent returned; nil when it has
			// never polled. Together with unread this separates "nothing
			// arrived" from "nobody is reading".
			"last_poll_age_s": d.lastPollAgeS(p.AgentID),
			// Events the bot saw but no agent may read. Non-zero with
			// routing.json is normal; with no routing file it means the
			// allow-lists are excluding everything.
			"skipped_no_audience": d.Hub.Skipped(),
			"last_event_age_s":    snap.LastEventAgoS,
		},

		// --- configuration --------------------------------------------------
		"limits": map[string]any{
			"text_max_runes":         d.Cfg.TextMaxRunes,
			"bus_capacity_per_agent": own.Capacity,
			"poll_max_bytes":         d.Cfg.PollMaxBytes,
			"poll_default_wait_secs": d.Cfg.PollDefaultWait,
			"poll_min_wait_secs":     d.Cfg.PollMinWait,
			"poll_max_wait_secs":     d.Cfg.PollMaxWait,
			"media":                  d.mediaSummary(),
			"file_roots":             d.Cfg.FileRoots,
			"max_upload_bytes":       d.Cfg.MaxUploadBytes,
			"max_files_per_msg":      d.Cfg.MaxFilesPerMsg,
			"max_sends_per_min":      d.Cfg.MaxSendsPerMin,
			"send_gap_ms":            d.Cfg.SendGapMS,
			"unknown_channel":        d.Cfg.UnknownChannel,
		},
		"routing": d.Router.Summary(p),

		// --- meta -----------------------------------------------------------
		"meta": map[string]any{
			"name":             "mcp-discord-roleplay",
			"version":          d.Version,
			"milestone":        BuildMilestone,
			"transport":        d.Cfg.Transport,
			"dry_run":          d.Cfg.DryRun,
			"stopping":         closed(d.Stopping),
			"uptime_s":         int64(time.Since(d.uptimeBase()).Seconds()),
			"started_at":       tz.Format(d.uptimeBase()),
			"registered_tools": d.toolNames,
			"next_action":      d.nextAction(snap, qs, p),
			"warnings":         d.warnings(snap, qs, p),
		},
	}

	// Per-agent send bookkeeping: where to:"last" points, plus the retry hint
	// when the breaker is open.
	if t := d.lastTarget(p.AgentID); t != "" {
		out["receive"].(map[string]any)["last_send_target"] = t
	}
	if lim.RetryAfterMS > 0 {
		out["send"].(map[string]any)["retry_after_ms"] = lim.RetryAfterMS
	}
	return jsonResult(out), nil
}

// uptimeBase is when this process came up: Deps.StartedAt when main set it,
// otherwise package load time.
func (d *Deps) uptimeBase() time.Time {
	if d.StartedAt.IsZero() {
		return startedAt
	}
	return d.StartedAt
}

// closed reports whether a shutdown channel has been closed (false when it is
// nil, which is how the unit tests build a Deps).
func closed(ch chan struct{}) bool {
	if ch == nil {
		return false
	}
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// mediaSummary states the inbound attachment policy the way an agent can act on
// it: one mode per kind, plus the caps that decide whether a given file can be
// inlined at all, plus where saved files live and under what name the agent is
// told about them. Without this a model has no way to know that the image it
// was promised arrived as a URL only.
func (d *Deps) mediaSummary() map[string]any {
	cfg := d.Cfg
	out := map[string]any{
		"image":        cfg.MediaFor(config.KindImage),
		"audio":        cfg.MediaFor(config.KindAudio),
		"video":        cfg.MediaFor(config.KindVideo),
		"other":        cfg.MediaFor(config.KindOther),
		"can_inline":   cfg.CanInline(),
		"save_dir":     cfg.MediaOut,
		"save_enabled": cfg.SaveEnabled(),
		// What a saved file is actually allowed to cost, and the prefix its
		// reported name carries: local_path is the handle the agent gets back,
		// so a status line naming it is the only way to read a poll's
		// "mcpdr-media/1a2b3c4d-photo.png" without guessing at the layout.
		"save_max_bytes":         cfg.MediaSaveMaxBytes,
		"local_path":             cfg.LocalPathPrefix(),
		"save_keep":              cfg.MediaKeep.String(),
		"save_max_mb":            cfg.MediaMaxMB,
		"inline_max_bytes_image": cfg.InlineMaxBytesImage,
		"inline_max_side":        cfg.InlineMaxSide,
		"inline_max_count":       cfg.InlineMaxCount,
		"inline_max_total_bytes": cfg.InlineMaxTotalBytes,
	}
	// The fetcher is the only one who knows whether the cache directory really
	// opened. save_enabled above is the intent; this is whether 'save' can be
	// honoured, and the difference is what a "why is everything skip:save_off"
	// question gets answered from.
	if d.Media != nil {
		out["save_ready"] = d.Media.Ready()
	}
	if s := d.MediaSweep; s != nil {
		out["cache_sweep"] = map[string]any{
			"removed": s.Removed,
			"bytes":   s.Bytes,
			"kept":    s.Kept,
			"skipped": s.Skipped,
		}
	}
	return out
}

// --- queue views ------------------------------------------------------------

// BuildMilestone is stamped into discord_status so a log or screenshot always
// says which build produced it. Bump it per milestone.
const BuildMilestone = "M2c"

func findQueueStat(qs []bus.QueueStat, agent string) (bus.QueueStat, bool) {
	for _, q := range qs {
		if q.Agent == agent {
			return q, true
		}
	}
	return bus.QueueStat{}, false
}

func queueSizeTotal(qs []bus.QueueStat) int {
	n := 0
	for _, q := range qs {
		n += q.Size
	}
	return n
}

func queueDroppedTotal(qs []bus.QueueStat) uint64 {
	var n uint64
	for _, q := range qs {
		n += q.Dropped
	}
	return n
}

// lastPollAgeS is how long ago this agent's last poll returned, or nil when it
// never polled.
func (d *Deps) lastPollAgeS(agent string) any {
	t, ok := d.lastPoll(agent)
	if !ok {
		return nil
	}
	return int64(time.Since(t).Seconds())
}

func joinAgents(agents []string) string { return strings.Join(agents, ", ") }

// nextAction is a single recommendation string. LLM callers act on prose far
// more reliably than on a table of booleans, so the judgement is done server
// side and shipped as one imperative line.
func (d *Deps) nextAction(snap discord.Snapshot, qs []bus.QueueStat, p router.Principal) string {
	switch {
	case !snap.Started:
		return "run the server without --selftest to connect"
	case !snap.Connected:
		return "wait for the gateway to reconnect before sending; check the token and that the bot is in the guild"
	case !snap.Ready:
		return "wait a moment for READY, then call discord_status again"
	case len(d.Router.Targets(p)) == 0:
		return "no visible targets for this agent: check routing.json or --allow-channel"
	case queueSizeTotal(qs) > 0:
		return "call discord_poll: there are unread messages waiting"
	default:
		return "connected and idle; send a message or call discord_poll to wait for one"
	}
}

// warnings collects the things that are silently wrong: no gateway, no queue
// for this caller, events nobody may read, dropped messages, an open breaker.
// An empty slice means "nothing to complain about"; do not fold it into a
// single ok flag, the caller needs the list to decide what to do.
func (d *Deps) warnings(snap discord.Snapshot, qs []bus.QueueStat, p router.Principal) []string {
	var w []string

	if !snap.Started {
		w = append(w, "gateway never started (--selftest mode): nothing will be received or sent")
	} else if !snap.Connected || !snap.Ready {
		w = append(w, "gateway is not ready: discord_send will fail until the websocket handshake completes")
	}
	if snap.SelfID == "" && snap.Started {
		w = append(w, "the bot identity is still unknown — READY has not arrived yet, so mentions_self cannot be computed")
	}
	if !d.Cfg.MessageContentEnabled() {
		w = append(w, "the Message Content intent is off (--no-message-content): incoming 'text' is always empty, "+
			"so a poll proves that something arrived, not what was said; mentions, attachments and embeds still work")
	}

	if d.Router.HasRouting() && d.Router.DeclaresAgent(p.AgentID) {
		// fine: the caller is named in routing.json
	} else if d.Router.HasRouting() {
		w = append(w, "caller "+p.AgentID+" is not listed in routing.json: it only sees unassigned targets")
	}

	q, ok := findQueueStat(qs, p.AgentID)
	switch {
	case len(qs) == 0:
		w = append(w, "no queues exist: no agent may read any incoming message")
	case !ok:
		w = append(w, "caller "+p.AgentID+" has no queue: discord_poll will return empty results for it")
	case q.Dropped > 0:
		w = append(w, strconv.FormatUint(q.Dropped, 10)+
			" event(s) were dropped from this agent's queue (cap "+strconv.Itoa(q.Capacity)+
			"); nobody polled it often enough")
	case q.Size*4 >= q.Capacity*3:
		w = append(w, "this agent's queue is "+strconv.Itoa(q.Size)+"/"+strconv.Itoa(q.Capacity)+
			" full: poll it before more messages arrive or the oldest will be dropped")
	}

	if total := queueDroppedTotal(qs); total > 0 && q.Dropped == 0 {
		w = append(w, strconv.FormatUint(total, 10)+
			" event(s) dropped across other agents' queues")
	}
	if n := d.Hub.Skipped(); n > 0 {
		w = append(w, strconv.FormatUint(n, 10)+
			" inbound event(s) had no audience and were discarded; check --allow-*/routing.json")
	}
	if !d.Cfg.SaveEnabled() && !d.Cfg.CanInline() {
		w = append(w, "inbound attachments are all skipped: no file content will reach the model, only url and metadata")
	}
	if d.Cfg.DryRun {
		w = append(w, "dry-run mode: discord_send is not delivering to Discord")
	}
	for _, root := range d.Cfg.FileRoots {
		if root == "." || root == string(filepath.Separator) {
			w = append(w, "file root is the whole filesystem or the working directory: reads are not confined")
			break
		}
	}
	if d.DC != nil {
		if lim := d.DC.Stats(); lim.BreakerOpen {
			w = append(w, "send circuit breaker is open: sends fail fast for "+
				strconv.FormatInt(lim.RetryAfterMS, 10)+" ms")
		}
	}
	return w
}
