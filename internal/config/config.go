// Package config owns every knob of the server: command line flags, the
// process environment, an optional .env file and the routing file. Precedence
// is strictly flag > env > .env > built-in default.
//
// The bot token is only ever read from the environment (or a .env file). It is
// never accepted as a command line value (that would leak through `ps`) and it
// is never written to a log line, a tool result or any file.
package config

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"mcp-discord-roleplay/internal/tz"
)

// Module is the Go module name / MCP serverInfo name.
const Module = "mcp-discord-roleplay"

// Where this server writes when the operator did not say otherwise: plain
// relative paths, i.e. directly inside the process working directory. Nothing
// here consults $HOME, $XDG_CACHE_HOME or $TMPDIR, and the values are never
// rewritten (no ~ expansion, no Abs, no Clean) — what you pass is what lands.
const (
	DefaultStateFile = "mcpdr-state.json"
	DefaultMediaOut  = "mcpdr-media"
)

// MediaMode decides what happens to an inbound attachment.
type MediaMode string

const (
	MediaInline MediaMode = "inline" // download + base64 into the tool result
	MediaSave   MediaMode = "save"   // download to --media-out, return local_path
	MediaSkip   MediaMode = "skip"   // no download, only url + metadata
)

// Media kinds.
const (
	KindImage = "image"
	KindAudio = "audio"
	KindVideo = "video"
	KindOther = "other"
)

// ParseMediaMode validates a user supplied media mode.
func ParseMediaMode(s string) (MediaMode, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "inline":
		return MediaInline, nil
	case "save":
		return MediaSave, nil
	case "skip":
		return MediaSkip, nil
	default:
		return "", fmt.Errorf("invalid media mode %q (want inline|save|skip)", s)
	}
}

// Config is the fully resolved configuration.
type Config struct {
	// --- identity / transport -------------------------------------------------
	Token       string // normalized (always "Bot <token>"); NEVER log or marshal
	TokenEnv    string // environment variable name the token came from
	TokenSource string // human-readable, safe to log, e.g. "env:DISCORD_TOKEN"
	AgentID     string // principal for stdio (one process = one agent)
	Transport   string // stdio | http
	HTTPAddr    string // reserved: http transport lands after M4
	Origins     []string

	// --- poll timing ----------------------------------------------------------
	PollMinWait     int // seconds
	PollMaxWait     int // seconds
	PollDefaultWait int // seconds
	PollJitterPct   int // added to the timeout only

	// --- bus / state ----------------------------------------------------------
	BusCapacity  int
	StateFile    string // "" = disabled; otherwise used exactly as typed (relative = cwd)
	TextMaxRunes int

	// --- inbound media policy -------------------------------------------------
	Media               map[string]MediaMode // key: image|audio|video|other
	MediaOut            string               // "" = save disabled; relative = under the cwd
	MediaKeep           time.Duration
	MediaMaxMB          int
	InlineMaxBytesImage int
	InlineMaxSide       int
	InlineMaxCount      int
	InlineMaxTotalBytes int

	// --- outbound files -------------------------------------------------------
	FileRoots      []string // absolute, one per --file-root
	MaxUploadBytes int
	MaxFilesPerMsg int

	// --- outbound rate limits -------------------------------------------------
	MaxSendsPerMin int
	SendGapMS      int
	BreakerErrors  int

	// --- access control -------------------------------------------------------
	AllowGuilds   []string
	AllowChannels []string
	DenyChannels  []string
	AllowDM       bool
	UnknownDM     string // ignore | ring

	// --- files that feed the config -------------------------------------------
	EnvFile       string // used exactly as typed; "" = nothing loaded
	RoutingFile   string // used exactly as typed; "" = none found/loaded
	LoadedEnvKeys []string

	// --- runtime --------------------------------------------------------------
	DryRun       bool
	LogLevel     string
	LogFile      string
	TZ           string
	SelfTest     bool
	PrintVersion bool

	Routing *Routing // parsed (never nil after Load)

	// raw flag values kept private so the public surface stays tidy
	mImage, mAudio, mVideo, mOther string
	rawRoots                       []string

	// toggles
	NoMediaOut bool
	NoFileRoot bool
}

// stringList is a repeatable, comma-splitting flag.Value.
type stringList struct{ v []string }

func (l *stringList) String() string { return strings.Join(l.v, ",") }
func (l *stringList) Set(s string) error {
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part != "" {
			l.v = append(l.v, part)
		}
	}
	return nil
}
func (l *stringList) Get() any { return l.v }

// Load parses args (without the program name), applies env and .env, then
// validates everything. flag.ErrHelp is returned verbatim for -h.
func Load(args []string) (*Config, error) {
	c := &Config{}
	var roots, allowGuilds, allowChannels, denyChannels, origins stringList

	fs := flag.NewFlagSet(Module, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)

	fs.StringVar(&c.TokenEnv, "token-env", "DISCORD_TOKEN", "environment variable holding the bot token (never a literal value)")
	fs.StringVar(&c.AgentID, "agent", "default", "agent id this process serves (cursor + ACL namespace)")
	fs.StringVar(&c.Transport, "transport", "stdio", "transport: stdio (http reserved for a later milestone)")
	fs.StringVar(&c.HTTPAddr, "http-addr", "127.0.0.1:8787", "reserved: listen address for the future http transport")
	fs.Var(&origins, "origin", "allowed CORS Origin for http transport (repeatable)")

	fs.IntVar(&c.PollMinWait, "poll-min-wait", 5, "smallest max_wait (seconds) a poll may request")
	fs.IntVar(&c.PollMaxWait, "poll-max-wait", 600, "largest max_wait (seconds) a poll may request")
	fs.IntVar(&c.PollDefaultWait, "poll-default-wait", 45, "max_wait used when the caller omits it")
	fs.IntVar(&c.PollJitterPct, "poll-jitter-pct", 15, "percent jitter added to the poll deadline only")

	fs.IntVar(&c.BusCapacity, "bus-capacity", 2000, "in-memory ring buffer size (messages)")
	fs.StringVar(&c.StateFile, "state-file", DefaultStateFile, "cursor persistence file, relative to the working directory ('' disables)")
	fs.IntVar(&c.TextMaxRunes, "text-max-runes", 4000, "truncate stored message text beyond this many runes")

	fs.StringVar(&c.mImage, "media-image", "inline", "policy for inbound images: inline|save|skip")
	fs.StringVar(&c.mAudio, "media-audio", "skip", "policy for inbound audio (skip = never burn tokens on voice)")
	fs.StringVar(&c.mVideo, "media-video", "skip", "policy for inbound video: inline|save|skip")
	fs.StringVar(&c.mOther, "media-other", "skip", "policy for any other attachment: inline|save|skip")
	fs.StringVar(&c.MediaOut, "media-out", DefaultMediaOut, "directory 'save' writes attachments into, relative to the working directory")
	fs.BoolVar(&c.NoMediaOut, "no-media-out", false, "disable the save mode entirely")
	fs.DurationVar(&c.MediaKeep, "media-keep", 168*time.Hour, "delete saved attachments older than this")
	fs.IntVar(&c.MediaMaxMB, "media-max-mb", 512, "cap the media cache at this many MiB (LRU by mtime)")

	fs.IntVar(&c.InlineMaxBytesImage, "inline-max-bytes-image", 4<<20, "max raw bytes per inlined image")
	fs.IntVar(&c.InlineMaxSide, "inline-max-side", 1280, "downscale images so the longest side fits (0 = never)")
	fs.IntVar(&c.InlineMaxCount, "inline-max-count", 4, "max inlined attachments per tool result")
	fs.IntVar(&c.InlineMaxTotalBytes, "inline-max-total-bytes", 12<<20, "max raw bytes inlined per tool result")

	fs.Var(&roots, "file-root", "directory send may read files from; absolute or relative to the cwd. Attachment paths stay relative to it (repeatable)")
	fs.BoolVar(&c.NoFileRoot, "no-file-root", false, "forbid reading any local file in send")
	fs.IntVar(&c.MaxUploadBytes, "max-upload-bytes", 8<<20, "max bytes per outbound file")
	fs.IntVar(&c.MaxFilesPerMsg, "max-files-per-msg", 10, "max attachments per outbound message")

	fs.IntVar(&c.MaxSendsPerMin, "max-sends-per-min", 20, "outbound message budget per minute")
	fs.IntVar(&c.SendGapMS, "send-gap-ms", 250, "minimum gap between two sends into the same channel")
	fs.IntVar(&c.BreakerErrors, "breaker-errors", 3, "consecutive rate-limit errors before a 60s circuit break")

	fs.Var(&allowGuilds, "allow-guilds", "guild IDs to admit when routing.json is absent (repeatable; empty = unrestricted)")
	fs.Var(&allowChannels, "allow-channels", "channel IDs to admit when routing.json is absent (repeatable)")
	fs.Var(&denyChannels, "deny-channels", "channel IDs that are always excluded (repeatable; wins over allow)")
	fs.BoolVar(&c.AllowDM, "allow-dm", true, "admit inbound DMs at all")
	fs.StringVar(&c.UnknownDM, "unknown-dm", "ignore", "DMs that match no routing rule: ignore|ring")

	fs.StringVar(&c.EnvFile, "env-file", ".env", "dotenv file to load (silently skipped when absent)")
	fs.StringVar(&c.RoutingFile, "config", "routing.json", "routing file (silently skipped when absent)")

	fs.BoolVar(&c.DryRun, "dry-run", false, "resolve everything and log sends instead of performing them")
	fs.StringVar(&c.LogLevel, "log-level", "info", "debug|info|warn|error")
	fs.StringVar(&c.LogFile, "log-file", "stderr", "stderr|stdout|<path> (stdout is reserved for MCP traffic)")
	fs.StringVar(&c.TZ, "tz", "Asia/Taipei", "timezone for emitted timestamps")

	fs.BoolVar(&c.SelfTest, "selftest", false, "validate configuration and routing, print a report, exit")
	var showVersion bool
	fs.BoolVar(&showVersion, "version", false, "print version and exit")

	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	c.PrintVersion = showVersion

	// Order matters for precedence: the dotenv file may add MCPDR_* values, but
	// it must never override a variable the operator really exported.
	if err := c.loadEnvFile(fs); err != nil {
		return nil, err
	}
	if err := c.applyEnv(fs); err != nil {
		return nil, err
	}

	// The repeatable flags (--file-root, --allow-*, --deny-channels, --origin) live
	// in flag.Value collectors, not in the struct, and applyEnv writes through the
	// same collector - so copy only now, or MCPDR_DENY_CHANNELS & friends parsed
	// from the environment would be thrown away. Missing this copy made every
	// allow/deny list silently inert.
	c.rawRoots = roots.v
	c.AllowGuilds = allowGuilds.v
	c.AllowChannels = allowChannels.v
	c.DenyChannels = denyChannels.v
	c.Origins = origins.v

	if err := c.finalize(); err != nil {
		return nil, err
	}
	return c, nil
}

// loadEnvFile resolves the --env-file path (flag > MCPDR_ENV_FILE > .env) and
// applies it to the process environment without overriding existing variables.
// A missing file is silently ignored.
func (c *Config) loadEnvFile(fs *flag.FlagSet) error {
	if !flagSet(fs, "env-file") {
		if v := os.Getenv("MCPDR_ENV_FILE"); v != "" {
			c.EnvFile = v
		}
	}
	if c.EnvFile == "" {
		return nil
	}
	// Used as typed: absolute, or relative to the cwd. No ~ expansion, no Abs.
	st, err := os.Stat(c.EnvFile)
	if err != nil || st.IsDir() {
		c.EnvFile = "" // silently ignored when absent
		return nil
	}
	keys, err := LoadDotEnv(c.EnvFile)
	if err != nil {
		return fmt.Errorf("env file: %w", err)
	}
	c.LoadedEnvKeys = keys
	return nil
}

func flagSet(fs *flag.FlagSet, name string) bool {
	found := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			found = true
		}
	})
	return found
}

// NoMediaOut / NoFileRoot live in the Config struct above.

// applyEnv lets every flag be set through MCPDR_<FLAG_NAME_IN_CAPS>. Values
// already given on the command line are never overwritten.
func (c *Config) applyEnv(fs *flag.FlagSet) error {
	already := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { already[f.Name] = true })

	var errs []string
	fs.VisitAll(func(f *flag.Flag) {
		if already[f.Name] {
			return
		}
		name := "MCPDR_" + strings.ToUpper(strings.ReplaceAll(f.Name, "-", "_"))
		v, ok := os.LookupEnv(name)
		if !ok || v == "" {
			return
		}
		if err := f.Value.Set(v); err != nil {
			errs = append(errs, fmt.Sprintf("%s=%q: %v", name, v, err))
		}
	})
	if len(errs) > 0 {
		return errors.New("bad environment: " + strings.Join(errs, "; "))
	}
	return nil
}

func (c *Config) finalize() error {
	// 1. token: environment only (already merged with .env by loadEnvFile)
	tok := strings.TrimSpace(os.Getenv(c.TokenEnv))
	if tok != "" {
		c.TokenSource = "env:" + c.TokenEnv
		c.Token = NormalizeToken(tok)
	} else if !c.SelfTest && !c.PrintVersion {
		src := ""
		if c.EnvFile != "" {
			src = " (nor from " + c.EnvFile + ")"
		}
		return fmt.Errorf("no bot token: set %s%s, or name another variable with --token-env", c.TokenEnv, src)
	}

	// 3. timezone
	if err := tz.Set(c.TZ); err != nil {
		return err
	}

	// 4. media modes
	c.Media = map[string]MediaMode{}
	for kind, raw := range map[string]string{
		KindImage: c.mImage, KindAudio: c.mAudio, KindVideo: c.mVideo, KindOther: c.mOther,
	} {
		m, err := ParseMediaMode(raw)
		if err != nil {
			return fmt.Errorf("media-%s: %w", kind, err)
		}
		c.Media[kind] = m
	}

	// 5. paths
	var err error
	// --state-file / --media-out stay exactly as typed: relative means "inside the
	// cwd", which is the documented default. No ~ expansion, no Abs, no Clean —
	// these are the paths the server *writes*, so they must be predictable. The
	// read-side paths (--config, --env-file) are likewise used as typed; only
	// --file-root goes through filepath.Abs, for the chroot containment checks.
	c.StateFile = strings.TrimSpace(c.StateFile)
	c.MediaOut = strings.TrimSpace(c.MediaOut)
	if c.NoMediaOut {
		c.MediaOut = ""
	}
	c.FileRoots, err = resolveRoots(c.rawRoots, c.NoFileRoot)
	if err != nil {
		return err
	}

	// 6. routing (path used as typed: absolute, or relative to the cwd)
	if c.RoutingFile != "" {
		if st, e := os.Stat(c.RoutingFile); e == nil && !st.IsDir() {
			r, e := LoadRouting(c.RoutingFile)
			if e != nil {
				return e
			}
			c.Routing = r
			if r.UnknownDM != "" {
				c.UnknownDM = strings.ToLower(r.UnknownDM)
			}
		} else {
			c.Routing = &Routing{}
			c.RoutingFile = ""
		}
	} else {
		c.Routing = &Routing{}
	}

	// 7. validate the rest
	switch strings.ToLower(c.UnknownDM) {
	case "ignore", "ring":
		c.UnknownDM = strings.ToLower(c.UnknownDM)
	default:
		return fmt.Errorf("--unknown-dm must be ignore|ring, got %q", c.UnknownDM)
	}
	switch strings.ToLower(c.Transport) {
	case "stdio":
		c.Transport = "stdio"
	case "http":
		return fmt.Errorf("--transport http is not built yet (it lands after the single-agent milestones); use stdio")
	default:
		return fmt.Errorf("--transport must be stdio (http reserved), got %q", c.Transport)
	}
	if c.PollMinWait < 1 {
		return fmt.Errorf("--poll-min-wait must be >= 1, got %d", c.PollMinWait)
	}
	if c.PollMaxWait < c.PollMinWait {
		return fmt.Errorf("--poll-max-wait (%d) must be >= --poll-min-wait (%d)", c.PollMaxWait, c.PollMinWait)
	}
	if c.PollDefaultWait < c.PollMinWait || c.PollDefaultWait > c.PollMaxWait {
		return fmt.Errorf("--poll-default-wait must be within [%d,%d], got %d", c.PollMinWait, c.PollMaxWait, c.PollDefaultWait)
	}
	if c.PollJitterPct < 0 || c.PollJitterPct > 50 {
		return fmt.Errorf("--poll-jitter-pct must be within [0,50], got %d", c.PollJitterPct)
	}
	if c.BusCapacity < 10 {
		return fmt.Errorf("--bus-capacity must be >= 10, got %d", c.BusCapacity)
	}
	if c.TextMaxRunes < 200 {
		return fmt.Errorf("--text-max-runes must be >= 200, got %d", c.TextMaxRunes)
	}
	if c.MaxSendsPerMin < 1 {
		return fmt.Errorf("--max-sends-per-min must be >= 1, got %d", c.MaxSendsPerMin)
	}
	if c.SendGapMS < 0 {
		return fmt.Errorf("--send-gap-ms must be >= 0, got %d", c.SendGapMS)
	}
	if c.MaxFilesPerMsg < 1 || c.MaxFilesPerMsg > 10 {
		return fmt.Errorf("--max-files-per-msg must be within [1,10] (Discord hard limit), got %d", c.MaxFilesPerMsg)
	}
	if c.MaxUploadBytes < 1 {
		return fmt.Errorf("--max-upload-bytes must be >= 1, got %d", c.MaxUploadBytes)
	}
	if c.InlineMaxCount < 0 {
		return fmt.Errorf("--inline-max-count must be >= 0, got %d", c.InlineMaxCount)
	}
	if c.InlineMaxTotalBytes < 0 {
		return fmt.Errorf("--inline-max-total-bytes must be >= 0, got %d", c.InlineMaxTotalBytes)
	}
	if c.MediaFor(KindImage) == MediaSave && c.MediaOut == "" {
		return fmt.Errorf("media-image=save needs --media-out; pass one or use --no-media-out with media-image=inline")
	}
	switch strings.ToLower(c.LogLevel) {
	case "debug", "info", "warn", "warning", "error":
	default:
		return fmt.Errorf("--log-level must be debug|info|warn|error, got %q", c.LogLevel)
	}
	return nil
}

// raw flag strings are read through c.rawRoots (set by Load).

// MediaFor returns the policy for a media kind.
func (c *Config) MediaFor(kind string) MediaMode {
	if c.Media == nil {
		return MediaSkip
	}
	if m, ok := c.Media[kind]; ok {
		return m
	}
	return MediaSkip
}

// SaveEnabled reports whether the save mode is available.
func (c *Config) SaveEnabled() bool { return c.MediaOut != "" }

// CanInline reports whether any inline budget exists at all.
func (c *Config) CanInline() bool {
	return c.InlineMaxCount > 0 && c.InlineMaxTotalBytes > 0 && c.InlineMaxBytesImage > 0
}

// NormalizeToken makes sure a token carries its authorization scheme.
func NormalizeToken(tok string) string {
	t := strings.TrimSpace(tok)
	if t == "" {
		return ""
	}
	lower := strings.ToLower(t)
	if strings.HasPrefix(lower, "bot ") || strings.HasPrefix(lower, "bearer ") {
		return t
	}
	return "Bot " + t
}

// RedactedToken is safe for logs and tool results.
func (c *Config) RedactedToken() string {
	t := c.Token
	if t == "" {
		return "(unset)"
	}
	scheme := "Bot"
	if strings.HasPrefix(strings.ToLower(t), "bearer ") {
		scheme = "Bearer"
	}
	body := t
	if i := strings.IndexByte(t, ' '); i >= 0 {
		body = t[i+1:]
	}
	if len(body) <= 6 {
		return scheme + " ****"
	}
	return fmt.Sprintf("%s ****%s (%d chars)", scheme, body[len(body)-4:], len(body))
}

// Audit is the token-free view handed to the discord_status tool.
func (c *Config) Audit() map[string]any {
	return map[string]any{
		"agent":        c.AgentID,
		"transport":    c.Transport,
		"token_source": c.TokenSource,
		"token":        c.RedactedToken(),
		"poll": map[string]any{
			"min_wait_s": c.PollMinWait, "max_wait_s": c.PollMaxWait,
			"default_wait_s": c.PollDefaultWait, "jitter_pct": c.PollJitterPct,
		},
		"bus_capacity":   c.BusCapacity,
		"state_file":     c.StateFile,
		"text_max_runes": c.TextMaxRunes,
		"media": map[string]any{
			"image": c.MediaFor(KindImage), "audio": c.MediaFor(KindAudio),
			"video": c.MediaFor(KindVideo), "other": c.MediaFor(KindOther),
			"out": c.MediaOut, "keep": c.MediaKeep.String(), "max_mb": c.MediaMaxMB,
		},
		"inline": map[string]any{
			"max_bytes_image": c.InlineMaxBytesImage, "max_side": c.InlineMaxSide,
			"max_count": c.InlineMaxCount, "max_total_bytes": c.InlineMaxTotalBytes,
		},
		"file_roots":        c.FileRoots,
		"max_upload_bytes":  c.MaxUploadBytes,
		"max_files_per_msg": c.MaxFilesPerMsg,
		"rate_limit": map[string]any{
			"sends_per_min": c.MaxSendsPerMin, "gap_ms": c.SendGapMS, "breaker_errors": c.BreakerErrors,
		},
		"acl": map[string]any{
			"allow_guilds": c.AllowGuilds, "allow_channels": c.AllowChannels,
			"deny_channels": c.DenyChannels, "allow_dm": c.AllowDM, "unknown_dm": c.UnknownDM,
		},
		"routing_file": c.RoutingFile,
		"env_file":     c.EnvFile,
		"loaded_env":   c.LoadedEnvKeys,
		"dry_run":      c.DryRun,
		"log_level":    c.LogLevel,
		"log_file":     c.LogFile,
		"tz":           c.TZ,
		"tz_offset":    tz.Now().Format("-07:00"),
	}
}

// resolveRoots turns --file-root values into existing directories. The value is
// used as typed - an absolute path, or one relative to the cwd, which is exactly
// how the OS resolves it when we open a file under the root. We do not expand ~:
// that is the shell's job, and a second private path language here would only
// surprise people. The only transformation is filepath.Abs, because the chroot
// containment checks in M1 compare against a canonical directory string.
// With no flag at all we default to the current working directory (the agent
// writes a file, then hands the server its relative path); --no-file-root
// turns local reads off completely.
func resolveRoots(raw []string, none bool) ([]string, error) {
	if none {
		if len(raw) > 0 {
			return nil, fmt.Errorf("--no-file-root cannot be combined with --file-root")
		}
		return nil, nil
	}
	if len(raw) == 0 {
		cwd, err := os.Getwd()
		if err != nil {
			return nil, fmt.Errorf("default --file-root (cwd): %w", err)
		}
		raw = []string{cwd}
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(raw))
	for _, r := range raw {
		root, err := filepath.Abs(strings.TrimSpace(r))
		if err != nil {
			return nil, fmt.Errorf("--file-root %q: %w", r, err)
		}
		st, err := os.Stat(root)
		if err != nil {
			return nil, fmt.Errorf("--file-root %q: %w", root, err)
		}
		if !st.IsDir() {
			return nil, fmt.Errorf("--file-root %q is not a directory", root)
		}
		if seen[root] {
			continue
		}
		seen[root] = true
		out = append(out, root)
	}
	return out, nil
}

// Path policy, and the reason there is no expandPath / cacheDir helper any more:
// every path argument (--state-file, --media-out, --log-file, --env-file,
// --config, --file-root) is taken as typed — absolute, or relative to the
// process cwd. We never expand ~ (the operator's shell does that, and a second
// private path language would only surprise people), we never consult $HOME,
// $XDG_CACHE_HOME or $TMPDIR, and nothing rewrites a value into a different
// location. The single exception is --file-root, where filepath.Abs is needed
// for the chroot containment checks; Abs is also what makes "relative = under the
// cwd" true, so it is the documented semantics rather than a surprise.
