// status.go exposes the one tool whose entire job is "what happened to my
// messages". Two views answer that, and they are deliberately not the same size.
//
// The WIRE view (statusWire, six keys) is what a model gets: a state table it can
// read and decide from — gateway, queue depth against its capacity, cumulative
// drops, why it is not connected, and the aliases it may speak to. No hints and
// no next_action: "1500/2000" already says poll soon, and "down" already says do
// not send. The judgement is left to the caller; the server only states facts.
// Static truths about this deployment (intent off, --dry-run) ride in the tool
// descriptions, which travel once per session instead of once per call.
//
// The PANEL view (fullPanel) keeps everything an operator might want: connection
// internals, send bookkeeping, every agent's queue, the configured limits and the
// routing file. It reaches nobody unless the debug log is on or --selftest prints
// it. Host paths (file roots, media save dir) survive only there, because a model
// cannot type them — its own file box is addressed by the mcpdr- handle.
//
// Both views describe ONE queue: the caller's. Since M2 each agent has its own
// queue and taking a message out is the acknowledgement, so there is no sequence
// number and no cursor to report — M1's baseline_cursor/head_cursor are gone,
// replaced by depth, drops and how long ago that agent last polled. Another
// agent's unread messages are deliberately not visible here.

package tools

import (
	"context"
	"encoding/json"
	"log/slog"
	"strconv"
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

// statusTool returns the status tool's registration; prefix is --tool-prefix, so
// the name it advertises is the name the caller has to type.
func statusTool(prefix string) *mcp.Tool {
	return &mcp.Tool{
		Name:  "status",
		Title: "Discord bridge health and your queue",
		Description: "Report connection health, this agent's message queue, " +
			"recent errors and the configured limits. Call it before anything else " +
			"when something seems wrong, or when you need a target alias. " +
			"gateway is ready|starting|down|stopping; queue is unread/capacity, so a " +
			"high ratio means poll before the oldest messages are dropped; dropped " +
			"counts messages overwritten while nobody polled, since startup; targets " +
			"are the aliases this caller may send to and read_only the ones it " +
			"receives but cannot answer.",
		InputSchema: mcp.Schema{
			Type:        mcp.TypeObject,
			Description: "Discord roleplay bridge status snapshot",
			// No parameters: the only input that matters is the caller
			// identity, and that arrives as _meta.agentId on the request.
			Properties: map[string]mcp.Schema{},
		},
	}
}

// statusWire is the whole answer discord_status gives a model. Six keys, all of
// them state rather than advice, because a caller that can read a ratio does not
// need a sentence telling it to poll. Everything else this process knows lives in
// fullPanel and only travels to the debug log or --selftest.
type statusWire struct {
	Gateway   string   `json:"gateway"`              // ready | starting | down | stopping
	Queue     string   `json:"queue"`                // "unread/capacity", always present
	Dropped   uint64   `json:"dropped,omitempty"`    // ring overwrites since startup, this agent only
	LastError string   `json:"last_error,omitempty"` // why the gateway is unusable, when it is
	Targets   []string `json:"targets,omitempty"`    // aliases send accepts from this caller
	ReadOnly  []string `json:"read_only,omitempty"`  // received, but refused on send
}

// gatewayState folds four booleans (started / connected / ready / stopping) into
// the one word that decides whether a caller should act at all. A client that was
// never started and a socket that died are the same thing to a model: sending
// would fail either way, and the panel keeps the difference.
func gatewayState(snap discord.Snapshot, stopping bool) string {
	switch {
	case stopping:
		return "stopping"
	case !snap.Started || !snap.Connected:
		return "down"
	case !snap.Ready:
		return "starting"
	default:
		return "ready"
	}
}

// statusWireOf reads live state, so it deliberately reads as little of it as it
// can: the answer is supposed to be cheap enough to call before every exchange.
func (d *Deps) statusWireOf(p router.Principal) statusWire {
	snap := d.gatewaySnapshot()
	qs := d.Hub.Stats()
	own, hasQueue := findQueueStat(qs, p.AgentID)
	if !hasQueue {
		// An agent with no queue of its own yet: report the numbers the process
		// default would see, so the ratio still means something.
		own, _ = findQueueStat(qs, d.Cfg.AgentID)
	}
	capacity := own.Capacity
	if capacity <= 0 {
		capacity = d.Cfg.BusCapacity
	}
	w := statusWire{
		Gateway:   gatewayState(snap, closed(d.Stopping)),
		Queue:     strconv.Itoa(own.Size) + "/" + strconv.Itoa(capacity),
		Dropped:   own.Dropped,
		LastError: snap.LastError,
	}
	w.Targets, w.ReadOnly = d.targetAliases(p)
	return w
}

// targetAliases answers, in the words the caller has to type, what it may send to
// and what it only hears. A wildcard rule ("dm:*") is a POLICY and not an
// address: send refuses to:"dms", so listing it as if it were one would only
// invite a failure. It stays visible in routing.json, the debug log and selftest.
// A rule this agent may neither read nor write is left out of both lists.
func (d *Deps) targetAliases(p router.Principal) (writable, readOnly []string) {
	for _, ru := range d.Router.Targets(p) {
		if ru.Wildcard {
			continue
		}
		switch {
		case ru.Write:
			writable = append(writable, ru.Alias)
		case ru.Read:
			readOnly = append(readOnly, ru.Alias)
		}
	}
	return writable, readOnly
}

// logStatus writes the whole panel as one debug line, and only when the operator
// actually asked for debug output — that guard is what keeps the expensive view
// from costing anything on the way to the cheap one.
func (d *Deps) logStatus(ctx context.Context, p router.Principal) {
	lg := d.debug()
	if !lg.Enabled(ctx, slog.LevelDebug) {
		return
	}
	b, err := json.Marshal(d.fullPanel(p))
	if err != nil {
		lg.Debug("status", "agent", p.AgentID, "error", err.Error())
		return
	}
	lg.Debug("status", "agent", p.AgentID, "panel", string(b))
}

// StatusPretty renders the status answer the way an operator wants it: the wire
// first, because that is literally what the model will see, then the full panel
// behind it. Only --selftest calls it, so its size costs no context.
func StatusPretty(d *Deps) string {
	p := d.principal(context.Background())
	w := d.statusWireOf(p)
	bw, err := json.Marshal(&w)
	if err != nil {
		bw = []byte("{wire unavailable: " + err.Error() + "}")
	}
	bp, err := json.MarshalIndent(d.fullPanel(p), "", "  ")
	if err != nil {
		bp = []byte("{panel unavailable: " + err.Error() + "}")
	}
	return "wire:  " + string(bw) + "\npanel:\n" + string(bp) + "\n"
}

// handleStatus answers the query with the wire shape and nothing else.
func (d *Deps) handleStatus(ctx context.Context, call *mcp.Call) (*mcp.ToolResult, error) {
	p := d.principal(ctx)
	w := d.statusWireOf(p)
	d.logStatus(ctx, p)
	return jsonResult(&w), nil
}

// fullPanel is everything this process knows, in the six blocks an operator reads
// when something is wrong. Built on demand: the poll/send/status handlers do not
// pay for it unless the debug log wants it.
func (d *Deps) fullPanel(p router.Principal) map[string]any {
	snap := d.gatewaySnapshot()
	qs := d.Hub.Stats()

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
			"transport":        d.Cfg.Transport,
			"dry_run":          d.Cfg.DryRun,
			"stopping":         closed(d.Stopping),
			"uptime_s":         int64(time.Since(d.uptimeBase()).Seconds()),
			"started_at":       tz.Format(d.uptimeBase()),
			"registered_tools": d.toolNames,
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
	return out
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
