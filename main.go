// Command mcp-discord-roleplay is an MCP server that lets an LLM sit inside a
// Discord conversation: it blocks on discord_poll until something new arrives
// and answers with discord_send.
//
// Design notes live in docs/DESIGN.md. The stdout stream is reserved for MCP
// JSON-RPC; every log line goes to stderr.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	mcp "mcptransport"

	"mcp-discord-roleplay/internal/bus"
	"mcp-discord-roleplay/internal/config"
	"mcp-discord-roleplay/internal/discord"
	"mcp-discord-roleplay/internal/files"
	"mcp-discord-roleplay/internal/media"
	"mcp-discord-roleplay/internal/router"
	"mcp-discord-roleplay/internal/tools"
	"mcp-discord-roleplay/internal/tz"
)

// version is this build's one identity: it answers -version, it rides the
// startup log line, and main hands it to the MCP server as serverInfo.version
// (initialize/discover). That is a *server* version and has nothing to do with
// the protocol revisions negotiated in mcptransport/rpc.go, which must not move.
// A var so a later CI build can stamp it with -ldflags -X main.version=…
// without touching the code; until then it says "dev", which is what it is.
var version = "0.1.0-dev"

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	cfg, err := config.Load(args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		fmt.Fprintf(os.Stderr, "%s: %v\n", config.Module, err)
		return 2
	}
	if cfg.PrintVersion {
		fmt.Printf("%s %s\n", config.Module, version)
		return 0
	}

	logger, err := newLogger(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: logger: %v\n", config.Module, err)
		return 2
	}
	slog.SetDefault(logger)
	// outbound_proxy is the only place an operator can see, without waiting for a
	// failure that happens to mention a proxy, whether the standard *_PROXY
	// variables are in this process at all — there is no flag for them, and
	// internal/config/outbound.go says why the summary never prints a credential.
	logger.Info("starting", "version", version, "agent", cfg.AgentID, "transport", cfg.Transport,
		"outbound_proxy", config.OutboundProxySummary())
	// Config never fails on a soft finding (an entry in both allow and deny), but
	// it must not stay invisible either.
	for _, w := range cfg.Warnings {
		logger.Warn("config warning", "detail", w)
	}

	rt, err := router.New(cfg)
	if err != nil {
		logger.Error("routing config is invalid", "err", err)
		return 2
	}

	if cfg.SelfTest {
		return selftest(cfg, rt, logger)
	}

	ctx, stopSignal := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignal()

	// One inbound queue per agent (docs/DESIGN.md §2): an event is fanned out to
	// the agents routing says may read it, and taking it out is the ack.
	hub := bus.NewHub(cfg.BusCapacity, rt.AgentIDs(), logger)
	cl := discord.New(cfg, logger, hub, rt)
	store := files.New(cfg, logger)

	// Inbound attachments: one fetcher for the whole process (it owns the HTTP
	// client and the open handle on the cache directory), and one sweep of that
	// cache at startup instead of a background goroutine nobody can see.
	fetcher := media.New(cfg, media.Deps{Log: logger})
	defer fetcher.Close()
	sweep, err := media.Cleanup(cfg, logger)
	if err != nil {
		logger.Warn("media cache sweep failed; the cache is simply still full", "err", err)
	} else if sweep.Removed > 0 || sweep.Skipped > 0 {
		logger.Info("media cache sweep", "result", sweep.String())
	}

	if err := cl.Start(ctx); err != nil {
		logger.Error("discord gateway start failed", "err", err,
			"hint", "check the token, the privileged Message Content intent, and network reachability")
		return 3
	}

	shutting := make(chan struct{})
	var closeOnce sync.Once
	shutdown := func() { closeOnce.Do(func() { close(shutting) }) }

	reg := mcp.NewRegistry(cfg.ToolPrefix)
	deps := &tools.Deps{
		Cfg:        cfg,
		Router:     rt,
		Hub:        hub,
		DC:         cl,
		Files:      store,
		Media:      fetcher,
		MediaSweep: &sweep,
		Stopping:   shutting,
		StartedAt:  time.Now(),
		Log:        logger,
	}
	if err := tools.RegisterAll(reg, deps); err != nil {
		logger.Error("tool registration failed", "err", err)
		return 2
	}
	logger.Info("tools registered", "count", len(reg.List()),
		"names", toolNames(reg), "agent", cfg.AgentID)

	srv := &mcp.Server{
		Name:         config.Module,
		Version:      version,
		Instructions: instructions(cfg, toolNames(reg)),
		Reg:          reg,
		Origin:       originSet(cfg),
		// RequestTimeout must stay 0: it is applied to the whole tools/call, and
		// discord_poll is expected to block for up to --poll-max-wait seconds.
		RequestTimeout: 0,
	}

	serveDone := make(chan error, 1)
	ss := mcp.NewStdioServer(srv)
	go func() { serveDone <- ss.Serve() }()

	var exitCode int
	select {
	case <-ctx.Done():
		logger.Info("signal received, draining")
	case err := <-serveDone:
		switch {
		case err == nil:
			logger.Info("stdin closed by client, shutting down")
		case errors.Is(err, bufio.ErrTooLong):
			// mcptransport caps one inbound line at 4 MiB; exceeding it ends the
			// read loop, i.e. the server can no longer answer anything. Say so loudly
			// instead of dying silently.
			logger.Error("inbound MCP line exceeded the 4 MiB limit; the stdio read loop has stopped",
				"err", err,
				"hint", "arguments must stay small — reference files by path relative to --file-root, never inline binary data")
			exitCode = 4
		default:
			logger.Error("stdio server stopped", "err", err)
			exitCode = 4
		}
	}

	shutdown()
	// Give in-flight requests a beat to return (poll answers reason:"shutdown"
	// from M2 onwards) before we pull the gateway out from under them.
	time.Sleep(250 * time.Millisecond)
	if err := cl.Close(); err != nil {
		logger.Warn("discord close", "err", err)
	}
	logger.Info("bye")
	return exitCode
}

// ---------------------------------------------------------------------------
// selftest
// ---------------------------------------------------------------------------

func selftest(cfg *config.Config, rt *router.Router, logger *slog.Logger) int {
	out := os.Stderr
	report := func(ok bool, format string, args ...any) {
		tag := "OK  "
		if !ok {
			tag = "WARN"
		}
		fmt.Fprintf(out, "%s %s\n", tag, fmt.Sprintf(format, args...))
	}
	failures := 0
	fail := func(format string, args ...any) {
		failures++
		fmt.Fprintf(out, "FAIL %s\n", fmt.Sprintf(format, args...))
	}

	fmt.Fprintf(out, "%s %s selftest\n", config.Module, version)
	fmt.Fprintf(out, "time now: %s\n\n", tz.Format(tz.Now()))

	if cfg.Token == "" {
		// The question a selftest answers is "would this start?", and without a
		// token it would not - so this flips the exit code instead of being a WARN
		// that contradicts the PASS line at the bottom.
		fail("token: (unset) — nothing in %s; the server would refuse to start", cfg.TokenEnv)
	} else {
		report(true, "token: %s", cfg.RedactedToken())
	}
	switch {
	case cfg.EnvLoaded:
		report(true, "env file: %s (%d keys applied, real environment wins)", cfg.EnvFile, len(cfg.LoadedEnvKeys))
	case cfg.EnvFile != "":
		report(false, "no .env file at %q (skipped; the token must then come from the real environment)", cfg.EnvFile)
	default:
		report(false, "env file disabled (--env-file='')")
	}

	report(true, "transport=%s agent=%s tz=%s (%s)", cfg.Transport, cfg.AgentID, cfg.TZ, tz.Now().Format("-07:00"))
	// Everything we write defaults to a plain name in the cwd; say which cwd that is.
	if wd, err := os.Getwd(); err == nil {
		report(true, "working directory: %s (relative --media-out / --log-file land here)", wd)
	}
	if cfg.MessageContentEnabled() {
		report(true, "intents: %s (MessageContent is privileged — enable it in the Developer Portal)",
			strings.Join(discord.IntentsList(true), "|"))
	} else {
		report(true, "intents: %s (Message Content intent OFF by --no-message-content: incoming text is usually empty; @-mentions, DMs and our own sends still carry it)",
			strings.Join(discord.IntentsList(false), "|"))
	}
	// A REST message object carries no `member` and we do not subscribe to
	// GUILD_MEMBERS, so --nick-lookup is what keeps history/get from naming a
	// person differently than poll already does.
	if cfg.NickLookupEnabled() {
		report(true, "author names: history/get read the guild nickname from the state cache, and fetch one guild member only for an author that cache has never seen (--nick-lookup=cache|off)")
	} else {
		report(true, "author names: --nick-lookup=off, so history/get show an author's global name while poll shows the guild nickname")
	}

	routingLabel := cfg.RoutingFile
	switch {
	case routingLabel == "":
		routingLabel = "disabled (--config='')"
	case !cfg.RoutingLoaded:
		routingLabel += " (absent)"
	}
	fmt.Fprintf(out, "\nrouting (%s):\n", routingLabel)
	targets := rt.Targets(router.Principal{AgentID: cfg.AgentID})
	if len(targets) == 0 {
		// These lists used to be silently dropped by the loader; print all three so
		// an operator can see which safety switches are actually live.
		report(false, "no routing rules: falling back to allow-lists (%d channels / %d guilds listed, %d denied)",
			len(cfg.AllowChannels), len(cfg.AllowGuilds), len(cfg.DenyChannels))
	}
	for _, t := range targets {
		mode := "rw"
		switch {
		case t.Read && !t.Write:
			mode = "r-"
		case !t.Read && t.Write:
			mode = "-w"
		case !t.Read && !t.Write:
			mode = "--"
		}
		name := t.Name
		if name == "" {
			name = "-"
		}
		agent := t.Agent
		if agent == "" {
			agent = "(shared)"
		}
		fmt.Fprintf(out, "  %s %s %-10s %s agent=%s\n", mode, t.Alias, t.Target, name, agent)
	}
	if !rt.DeclaresAgent(cfg.AgentID) {
		// No early return: the rest of the report is still worth reading.
		fail("agent %q is not declared in routing.json: it would see zero targets and every send would be refused as foreign_agent", cfg.AgentID)
	}
	report(true, "unrouted channels: unknown_channel=%s (ignore drops them, ring admits them to the default agent)",
		cfg.UnknownChannel)
	report(true, "gates: allow_channels=%v deny_channels=%v (an entry is an alias or a target id; dm:* and channel:* are the wildcards, deny wins)",
		cfg.AllowChannels, cfg.DenyChannels)
	for _, w := range cfg.Warnings {
		report(false, "config warning: %s", w)
	}

	// Built here rather than at the bottom so the queue list can be reported next
	// to the poll settings an operator is tuning. nil logger: the report itself
	// prints the same information.
	hub := bus.NewHub(cfg.BusCapacity, rt.AgentIDs(), nil)

	fmt.Fprintf(out, "\nfiles & media:\n")
	if len(cfg.FileRoots) == 0 {
		report(false, "no --file-root: send cannot read local files")
	} else {
		report(true, "file roots: %s", strings.Join(cfg.FileRoots, ", "))
		report(true, "attachment paths are named inside the first root that holds them (a leading '/' just means that root, "+
			"and the name is cleaned); opened through os.Root, so a '..' that leaves the root, '~' and links pointing out are refused")
		for _, r := range cfg.FileRoots {
			if filepath.Clean(r) == string(filepath.Separator) {
				report(false, "--file-root is the filesystem root: os.Root cannot stop us at the boundary, "+
					"so send can read any regular file this user can (including /proc files). Use the project directory")
			}
		}
	}
	report(true, "url attachments are pasted into the message, never fetched by this server")
	report(true, "upload caps: %d bytes/file, %d files/message", cfg.MaxUploadBytes, cfg.MaxFilesPerMsg)
	report(true, "inbound media: image=%s audio=%s video=%s other=%s emoji=%s sticker=%s",
		cfg.MediaFor(config.KindImage), cfg.MediaFor(config.KindAudio),
		cfg.MediaFor(config.KindVideo), cfg.MediaFor(config.KindOther),
		cfg.MediaEmoji, cfg.MediaSticker)
	if !cfg.SaveEnabled() {
		// --media-save-max-bytes=0 is the one switch that turns saving off (and it
		// is the only thing that licenses an empty --media-out), so say whether a
		// directory is merely idling.
		if cfg.MediaOut == "" {
			report(true, "save mode off (--media-save-max-bytes=0): no cache directory configured")
		} else {
			report(true, "save mode off (--media-save-max-bytes=0): %s is configured but nothing will be written", cfg.MediaOut)
		}
	} else {
		report(true, "media cache: %s (keep %s, max %d MiB, %d bytes/attachment; relative = under the cwd)",
			cfg.MediaOut, cfg.MediaKeep, cfg.MediaMaxMB, cfg.MediaSaveMaxBytes)
		if p := cfg.LocalPathPrefix(); p != "" {
			report(true, "local_path: saved files are reported to the agent as %s/<name>, "+
				"which is what it hands back to %s", p, tools.ToolName(cfg.ToolPrefix, "send"))
			if len(cfg.FileRoots) == 0 {
				report(false, "save mode is on but there is no --file-root: the agent can be told about a saved file and still be refused when it tries to send it back")
			}
		} else {
			report(false, "neither --local-path nor --media-out yields a prefix: saved files are reported by bare name, so they can only be sent back if the cache sits directly under a --file-root")
		}
	}
	report(true, "inline caps: %d B/image, side<=%d, %d per result, %d B total per result",
		cfg.InlineMaxBytesImage, cfg.InlineMaxSide, cfg.InlineMaxCount, cfg.InlineMaxTotalBytes)

	fmt.Fprintf(out, "\npoll & limits:\n")
	report(true, "poll window: %d..%d s (default %d, timeout jitter +%d%%)",
		cfg.PollMinWait, cfg.PollMaxWait, cfg.PollDefaultWait, cfg.PollJitterPct)
	if len(hub.Agents()) == 0 {
		fail("no message queues were built: discord_poll has nothing to read (check routing.json's agents)")
	} else {
		report(true, "queues: %d agent(s) x %d events each, text truncated at %d runes",
			len(hub.Agents()), cfg.BusCapacity, cfg.TextMaxRunes)
		for _, a := range hub.Agents() {
			fmt.Fprintf(out, "  queue %s\n", a)
		}
	}
	if config.Unlimited(cfg.PollMaxBytes) {
		report(false, "poll text budget is uncapped: one poll can hand back the whole queue in one answer")
	} else {
		report(true, "poll text budget: %d bytes of message text per discord_poll answer (JSON overhead is not counted); the rest stays queued",
			cfg.PollMaxBytes)
	}
	report(true, "send budget: %d/min, %d ms gap per channel, breaker after %d rate-limit errors",
		cfg.MaxSendsPerMin, cfg.SendGapMS, cfg.BreakerErrors)
	report(true, "429 handling: discordgo's own retry is OFF; we absorb retry_after<=%dms once, then answer "+
		"rate_limited, and after %d in a row the breaker refuses everything for 60s",
		cfg.SendRetryWaitMS, cfg.BreakerErrors)
	report(true, "long messages: split at %d characters, sent in order, attachments on the last part",
		discord.MaxContentRunes)
	if cfg.DryRun {
		report(false, "dry-run is ON: sends will be logged, not delivered")
	}

	fmt.Fprintf(out, "\ntools:\n")
	reg := mcp.NewRegistry(cfg.ToolPrefix)
	deps := &tools.Deps{
		Cfg: cfg, Router: rt, Hub: hub,
		DC:        discord.New(cfg, logger, hub, rt), // never started in selftest
		Files:     files.New(cfg, logger),
		Stopping:  make(chan struct{}),
		StartedAt: time.Now(),
		Log:       logger,
	}
	if err := tools.RegisterAll(reg, deps); err != nil {
		fail("tool registration: %v", err)
		fmt.Fprintf(out, "\nselftest FAILED (%d problem(s); the tool list below is not trustworthy)\n", failures)
		return 1
	}
	for _, t := range reg.List() {
		fmt.Fprintf(out, "  %s (%d args) — %s\n", t.Name, len(t.InputSchema.Properties), firstLine(t.Description))
	}
	if len(reg.List()) == 0 {
		fail("no tools registered")
	}

	// The status answer in both views: the handful of keys a model actually gets,
	// then the panel an operator debugs from. selftest output never reaches a
	// model, so this is where the whole thing can be printed without anyone
	// paying for it. The gateway reads "down" here because selftest never starts
	// the client -- which is also the state machine's smoke test.
	fmt.Fprintf(out, "\n--- %s ---\n", tools.ToolName(cfg.ToolPrefix, "status"))
	fmt.Print(tools.StatusPretty(deps))

	if failures > 0 {
		plural := "s"
		if failures == 1 {
			plural = ""
		}
		fmt.Fprintf(out, "\nselftest FAILED (%d problem%s; no gateway connection was attempted)\n", failures, plural)
		return 1
	}
	fmt.Fprintf(out, "\nselftest PASS (configuration is valid; no gateway connection was attempted)\n")
	return 0
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 90 {
		s = s[:90] + "…"
	}
	return s
}

// ---------------------------------------------------------------------------
// plumbing
// ---------------------------------------------------------------------------

func toolNames(reg *mcp.Registry) []string {
	list := reg.List()
	out := make([]string, 0, len(list))
	for _, t := range list {
		out = append(out, t.Name)
	}
	return out
}

func instructions(cfg *config.Config, names []string) string {
	// The names in this text are the ones the caller can actually invoke: a
	// sentence telling the model to "call discord_status" is a dead end when
	// --tool-prefix says otherwise.
	status := tools.ToolName(cfg.ToolPrefix, "status")
	poll := tools.ToolName(cfg.ToolPrefix, "poll")
	return "Discord bridge for roleplay. Available tools: " + strings.Join(names, ", ") + ". " +
		"Call " + status + " first to learn which target aliases you may use. " +
		"Targets are given as an alias, channel:<id>, dm:<id> (a DM is addressed by the PEER's user id), #name, me or last. " +
		"You never send binary data: reference local files by a path under the server's --file-root (a leading '/' means that root), " +
		"and put links in files[].url - they are pasted, not fetched. " +
		"Text over 2000 characters is split for you. " +
		poll + " holds the call open until something arrives or max_wait passes; an empty answer (reason=timeout) is not a failure — isError stays false. " +
		"Messages you have taken are gone from your queue - there is nothing to acknowledge and nothing to remember. " +
		"If an answer says remaining:N, that many messages are still queued - call " + poll + " again right away. " +
		"When such an answer comes back empty, waiting names the filters that turned those messages away and oldest_s says how long the oldest has waited. " +
		"Messages this bot sent are never queued, so " + poll + " only hands back what someone else said. " +
		"If dropped is present, messages were overwritten while nobody polled; poll more often. " +
		"Set your client's request timeout to at least max_wait, or the call is cancelled while waiting and the answer is lost. " +
		"This process serves agent '" + cfg.AgentID + "'."
}

func originSet(cfg *config.Config) map[string]bool {
	m := make(map[string]bool, len(cfg.Origins))
	for _, o := range cfg.Origins {
		o = strings.TrimSpace(o)
		if o != "" {
			m[o] = true
		}
	}
	return m
}

func newLogger(cfg *config.Config) (*slog.Logger, error) {
	var lvl slog.Level
	switch strings.ToLower(cfg.LogLevel) {
	case "debug":
		lvl = slog.LevelDebug
	case "warn", "warning":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}

	w := os.Stderr
	switch strings.ToLower(cfg.LogFile) {
	case "", "stderr":
		w = os.Stderr
	case "stdout":
		fmt.Fprintln(os.Stderr, "warning: stdout carries MCP traffic; logging to stderr instead")
		w = os.Stderr
	default:
		f, err := os.OpenFile(cfg.LogFile, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			return nil, err
		}
		w = f
	}
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: lvl})), nil
}
