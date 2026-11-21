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
	"path"
	"path/filepath"
	"reflect"
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
	DefaultMediaOut = "mcpdr-media"

	// DefaultPollMaxBytes caps how much text discord_poll ships in one call: the
	// sum of the `text` byte lengths, not the JSON reply. The queue can hold far
	// more than a model context window likes, so a poll stops taking events once
	// the text budget is used and the rest wait for the next poll (in order).
	DefaultPollMaxBytes = 16384

	// DefaultReplyMaxBytes caps the quoted line a reply row shows: the head of the
	// message being answered, in bytes. Deliberately small — the whole quoted
	// message is one dc_get away, same channel, same id — because in a channel
	// where everyone answers everyone, a quote made of nothing but other people's
	// words would eat --poll-max-bytes before our own messages got a turn.
	DefaultReplyMaxBytes = 80

	// DefaultMediaSaveMaxBytes caps a single saved attachment. Discord lets a
	// file reach ~200 MB and people throw 30-50 MB videos around without
	// thinking; writing those to disk for an agent that may never read them is
	// the expensive mistake, so save gets its own ceiling beside inline's.
	DefaultMediaSaveMaxBytes = 4 << 20
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

// What custom emoji in an inbound message deliver (Config.MediaEmoji). They get
// their own switch instead of following --media-image because the usual answers do
// not work for them: the text carries only a code, and the name inside that code is
// frequently no help at all (people upload under "emoji_23", or under nothing but
// the id they pulled it from). So the picture is the message, and a URL or a file
// path tells the model nothing about what was said -- unless the caller turns the
// whole feature off, which is what text is for.
const (
	EmojiInline = "inline" // fetch each distinct emoji of a message as its own image attachment
	EmojiText   = "text"   // leave the raw <:name:id> codes, create no attachment
)

// What the read path may do about an author's guild nickname (Config.NickLookup).
// The reason this needs a switch at all is that Discord's REST message object
// carries no `member` field: it appears in MESSAGE_CREATE / MESSAGE_UPDATE /
// INTERACTIONS_CREATE only. So the very same person reads as "tapris" on the
// gateway path and as their global name on history/get, and the only ways to fix
// it cost something different -- the state cache is free, a member fetch is one
// request per author. Off buys back the old behaviour, nothing else.
const (
	NickCache = "cache" // state cache first; on a miss, one member fetch, and the answer is written back
	NickOff   = "off"   // never look: read rows keep the author's global name
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
	ToolPrefix  string // stamped in front of every tool name at registration ("" = bare poll/send/status)
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
	PollMaxBytes int // text-byte budget for one discord_poll call; -1 = unlimited (0 is a rejected typo)
	TextMaxRunes int

	// ReplyMaxBytes is how much of the quoted message a reply row carries, in
	// bytes: 0 keeps who-and-which-id and drops the words, -1 ships the whole
	// quoted text. See DefaultReplyMaxBytes.
	ReplyMaxBytes int

	// ReadTimeout bounds ONE REST read (history / get / channels), per call. It is
	// ours, not discordgo's: its own default is 20s (discord.go, New()), which no
	// tool call should wait out. ReadTimeout also implies that we disable
	// discordgo's sleep-and-retry on 429 -- see internal/discord/read.go.
	// HistoryMaxBytes / ChannelsMaxBytes are the same kind of budget as
	// PollMaxBytes: a whole answer, -1 removes the cap, and a call that hits it
	// says so instead of silently returning less.
	ReadTimeout      time.Duration
	HistoryMaxBytes  int
	ChannelsMaxBytes int

	// NickLookup is NickCache|NickOff: whether history/get resolve the guild
	// nickname a REST message object does not carry. See the constants above.
	NickLookup string

	// --- inbound media policy -------------------------------------------------
	Media        map[string]MediaMode // key: image|audio|video|other
	MediaEmoji   string               // inline | text, see EmojiInline/EmojiText
	MediaSticker MediaMode            // inline | save | skip; overrides Media["image"] for stickers
	MediaOut     string               // "" = save disabled; relative = under the cwd
	LocalPath    string               // declared prefix for the agent-facing local_path; "" = derive from MediaOut
	MediaKeep    time.Duration
	MediaMaxMB   int
	// The inline caps and the save cap share one rule (decided 2026-11-17):
	// -1 = no limit, 0 = that mode is switched off, anything else = the limit.
	InlineMaxBytesImage int
	InlineMaxSide       int
	InlineMaxCount      int
	InlineMaxTotalBytes int
	MediaSaveMaxBytes   int

	// --- outbound files -------------------------------------------------------
	FileRoots      []string // absolute, one per --file-root
	MaxUploadBytes int
	MaxFilesPerMsg int

	// --- outbound rate limits -------------------------------------------------
	MaxSendsPerMin  int
	SendGapMS       int
	BreakerErrors   int
	SendRetryWaitMS int // largest retry_after we absorb locally instead of reporting

	// --- access control -------------------------------------------------------
	// M2c: a DM is a channel, so there is one policy per kind. AllowChannels and
	// DenyChannels hold aliases or target ids ("channel:123", "dm:456", "dm:*");
	// deny wins over allow, and an entry in both is reported as a warning.
	AllowGuilds    []string
	AllowChannels  []string
	DenyChannels   []string
	UnknownChannel string // ignore | ring: a channel (of either kind) with no rule

	// Warnings are non-fatal findings collected during finalize (an entry in both
	// allow and deny). main logs them; nothing else acts on them.
	Warnings []string

	// --- files that feed the config -------------------------------------------
	// Both paths are kept exactly as typed, even when the file turns out to be
	// absent, so the operator can see which path was searched; the *Loaded flags
	// say what actually happened.
	EnvFile       string // "" = env file disabled
	EnvLoaded     bool
	RoutingFile   string // "" = routing disabled
	RoutingLoaded bool
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
	mSticker                       string
	rawRoots                       []string

	// toggles
	NoFileRoot       bool
	NoMessageContent bool // drop the privileged Message Content intent
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
	fs.StringVar(&c.AgentID, "agent", "default", "agent id this process serves (queue + ACL namespace)")
	fs.StringVar(&c.ToolPrefix, "tool-prefix", "", "string put in front of every tool name as it is registered; empty keeps the bare names poll/send/status (a client that namespaces tools itself usually wants this empty)")
	fs.StringVar(&c.Transport, "transport", "stdio", "transport: stdio (http reserved for a later milestone)")
	fs.StringVar(&c.HTTPAddr, "http-addr", "127.0.0.1:8787", "reserved: listen address for the future http transport")
	fs.Var(&origins, "origin", "allowed CORS Origin for http transport (repeatable)")

	// Message Content is the only PRIVILEGED intent this server asks for, so it is
	// the one that can be dropped -- and dropping it is what keeps us connectable
	// while the Portal toggle moves. But what we DECLARE is not what we GET: live on
	// 2026-11-19 the flag was on (bit undeclared) and a guild message naming nobody
	// still arrived WITH text while the Portal toggle was on; with the toggle off,
	// only @-mentions and DMs carried it. Mentions, attachments and embeds are not
	// gated by this intent either way.
	fs.BoolVar(&c.NoMessageContent, "no-message-content", false, "do not request the privileged Message Content intent at IDENTIFY; whether Discord sends 'text' at all is decided by the app's Message Content setting in the Developer Portal (observed both ways -- see docs/DESIGN.md §7)")

	fs.IntVar(&c.PollMinWait, "poll-min-wait", 5, "smallest max_wait (seconds) a poll may request")
	fs.IntVar(&c.PollMaxWait, "poll-max-wait", 1200, "largest max_wait (seconds) a poll may request")
	fs.IntVar(&c.PollDefaultWait, "poll-default-wait", 45, "max_wait used when the caller omits it")
	fs.IntVar(&c.PollJitterPct, "poll-jitter-pct", 15, "percent jitter added to the poll deadline only")

	fs.IntVar(&c.BusCapacity, "bus-capacity", 2000, "per-agent in-memory queue size (messages); the oldest is dropped when full")
	fs.IntVar(&c.PollMaxBytes, "poll-max-bytes", DefaultPollMaxBytes, "budget for one poll, counted in the bytes of message text only (not JSON); -1 removes the cap")
	fs.IntVar(&c.TextMaxRunes, "text-max-runes", 4000, "truncate stored message text beyond this many runes")
	fs.IntVar(&c.ReplyMaxBytes, "reply-max-bytes", DefaultReplyMaxBytes, "bytes of the quoted message shown on a reply row; 0 keeps only the author and the id, -1 is uncapped")

	fs.DurationVar(&c.ReadTimeout, "read-timeout", 10*time.Second, "deadline for one REST read (history/get/channels); discordgo's own default is 20s and its 429 retry sleep is disabled on this path")
	fs.IntVar(&c.HistoryMaxBytes, "history-max-bytes", 4000, "budget for one history answer (-1 = no cap); messages are dropped from the OLDEST end and the answer says it did")
	fs.IntVar(&c.ChannelsMaxBytes, "channels-max-bytes", 4000, "budget for one channels answer (-1 = no cap); channel lists are trimmed from the tail and the answer says it did")
	fs.StringVar(&c.NickLookup, "nick-lookup", NickCache,
		"how history/get name an author: cache (state cache first, one member fetch per author it has never seen, answer written back for next time) | off (never look; a REST row shows the global name while poll shows the guild nickname)")

	fs.StringVar(&c.mImage, "media-image", "inline", "policy for inbound images: inline|save|skip")
	fs.StringVar(&c.mAudio, "media-audio", "skip", "policy for inbound audio (skip = never burn tokens on voice)")
	fs.StringVar(&c.mVideo, "media-video", "skip", "policy for inbound video: inline|save|skip")
	fs.StringVar(&c.mOther, "media-other", "skip", "policy for any other attachment: inline|save|skip")
	fs.StringVar(&c.MediaEmoji, "media-emoji", EmojiInline,
		"what custom emoji in an inbound message deliver: inline (fetch each distinct emoji's picture) | text (keep the <:name:id> codes, no picture)")
	fs.StringVar(&c.mSticker, "media-sticker", string(MediaInline), "policy for inbound stickers, overriding --media-image: inline|save|skip")
	fs.StringVar(&c.MediaOut, "media-out", DefaultMediaOut, "directory 'save' writes attachments into, relative to the working directory; empty is only allowed together with --media-save-max-bytes=0")
	fs.StringVar(&c.LocalPath, "local-path", "", "prefix we put in front of a saved file's name when we tell the agent about it, e.g. /dc-cache; it declares how the cache looks from inside --file-root (the real --media-out may live anywhere, even behind a bind mount), empty = derive from --media-out")
	fs.DurationVar(&c.MediaKeep, "media-keep", 168*time.Hour, "delete saved attachments older than this at startup (0 wipes the cache, a negative value keeps everything)")
	fs.IntVar(&c.MediaMaxMB, "media-max-mb", 512, "cap the media cache at this many MiB (LRU by mtime)")

	fs.IntVar(&c.InlineMaxBytesImage, "inline-max-bytes-image", 4<<20, "max raw bytes per inlined image (-1 = no limit, 0 = inline off)")
	fs.IntVar(&c.InlineMaxSide, "inline-max-side", 1280, "longest side an image may have to still be inlined; we never resize, bigger just skips (-1 = no check, 0 = inline off)")
	fs.IntVar(&c.InlineMaxCount, "inline-max-count", 4, "max inlined attachments per tool result (-1 = no limit, 0 = inline off)")
	fs.IntVar(&c.InlineMaxTotalBytes, "inline-max-total-bytes", 12<<20, "max raw bytes inlined per tool result (-1 = no limit, 0 = inline off)")
	fs.IntVar(&c.MediaSaveMaxBytes, "media-save-max-bytes", DefaultMediaSaveMaxBytes, "max raw bytes per saved attachment; Discord allows ~200 MB and people attach them casually (-1 = no limit, 0 = save off)")

	fs.Var(&roots, "file-root", "directory send may read files from, absolute or relative to the cwd; attachment paths are named inside it (a leading '/' just means that root; the name is cleaned, so a '..' inside resolves and only one that would leave is refused, as is '~'), opened through os.Root (repeatable; do not use '/')")
	fs.BoolVar(&c.NoFileRoot, "no-file-root", false, "forbid reading any local file in send")
	fs.IntVar(&c.MaxUploadBytes, "max-upload-bytes", 8<<20, "max bytes per outbound file")
	fs.IntVar(&c.MaxFilesPerMsg, "max-files-per-msg", 10, "max attachments per outbound message")

	fs.IntVar(&c.MaxSendsPerMin, "max-sends-per-min", 20, "outbound message budget per minute")
	fs.IntVar(&c.SendGapMS, "send-gap-ms", 250, "minimum gap between two sends into the same channel")
	fs.IntVar(&c.BreakerErrors, "breaker-errors", 3, "consecutive rate-limit errors before a 60s circuit break")
	fs.IntVar(&c.SendRetryWaitMS, "send-retry-wait-ms", 1500, "largest Discord retry_after we absorb by waiting; anything longer is reported back to the caller")

	fs.Var(&allowGuilds, "allow-guilds", "guild IDs to admit when routing.json is absent (repeatable; empty = unrestricted)")
	fs.Var(&allowChannels, "allow-channels", "channel aliases or target ids to admit (repeatable; channel:123, dm:456, dm:*)")
	fs.Var(&denyChannels, "deny-channels", "channel aliases or target ids that are always excluded, reading included (repeatable; wins over allow)")
	fs.StringVar(&c.UnknownChannel, "unknown-channel", "ignore", "channels that match no routing rule, guild or DM: ignore|ring")

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
		return nil // absent: silently skipped, EnvLoaded stays false
	}
	keys, err := LoadDotEnv(c.EnvFile)
	if err != nil {
		return fmt.Errorf("env file: %w", err)
	}
	c.EnvLoaded = true
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

// NoFileRoot lives in the Config struct above.

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
		switch {
		case c.EnvFile == "":
		case c.EnvLoaded:
			src = " (nor from the loaded " + c.EnvFile + ")"
		default:
			src = " (or write it into " + c.EnvFile + ")"
		}
		return fmt.Errorf("no bot token: set %s%s, or name another variable with --token-env", c.TokenEnv, src)
	}

	// 2. timezone
	if err := tz.Set(c.TZ); err != nil {
		return err
	}

	// 3. media modes
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

	// 3b. emoji and sticker rendering. Normalized here so the rest of the program can
	// compare against the constants without thinking about how the flag was typed.
	c.MediaEmoji = strings.ToLower(strings.TrimSpace(c.MediaEmoji))
	switch c.MediaEmoji {
	case EmojiInline, EmojiText:
	default:
		return fmt.Errorf("media-emoji: invalid value %q (want inline|text)", c.MediaEmoji)
	}
	stickerMode, perr := ParseMediaMode(c.mSticker)
	if perr != nil {
		return fmt.Errorf("media-sticker: %w", perr)
	}
	c.MediaSticker = stickerMode

	// 3c. the read path's nickname lookup. Normalized like the two above so the
	// read code compares against the constants.
	c.NickLookup = strings.ToLower(strings.TrimSpace(c.NickLookup))
	switch c.NickLookup {
	case NickCache, NickOff:
	default:
		return fmt.Errorf("nick-lookup: invalid value %q (want cache|off)", c.NickLookup)
	}

	// 4. write-side paths
	var err error
	// --media-out stays exactly as typed: relative means "inside the cwd", which
	// is the documented default. No ~ expansion, no Abs, no Clean — this is a
	// path the server *writes*, so it must be predictable. The read-side paths
	// (--config, --env-file) are likewise used as typed; only --file-root goes
	// through filepath.Abs, for the chroot containment checks.
	c.MediaOut = strings.TrimSpace(c.MediaOut)
	c.FileRoots, err = resolveRoots(c.rawRoots, c.NoFileRoot)
	if err != nil {
		return err
	}

	// 5. routing (path used as typed: absolute, or relative to the cwd)
	if c.RoutingFile != "" {
		if st, e := os.Stat(c.RoutingFile); e == nil && !st.IsDir() {
			r, e := LoadRouting(c.RoutingFile)
			if e != nil {
				return e
			}
			c.Routing = r
			c.RoutingLoaded = true
			if r.UnknownChannel != "" {
				c.UnknownChannel = strings.ToLower(r.UnknownChannel)
			}
			// The two sources of the global gates merge (union), they do not replace
			// each other: dropping a flag-listed channel because a routing file
			// appeared would silently widen or narrow the blast radius.
			c.AllowChannels = mergeTargets(c.AllowChannels, r.AllowChannels)
			c.DenyChannels = mergeTargets(c.DenyChannels, r.DenyChannels)
		} else {
			c.Routing = &Routing{}
		}
	} else {
		c.Routing = &Routing{}
	}

	// 6. validate the rest
	switch strings.ToLower(c.UnknownChannel) {
	case "ignore", "ring":
		c.UnknownChannel = strings.ToLower(c.UnknownChannel)
	default:
		return fmt.Errorf("--unknown-channel must be ignore|ring, got %q", c.UnknownChannel)
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
	if c.PollMaxBytes == 0 {
		// There used to be two sentinels here (0 and -1 both meant "no cap"),
		// while every other byte flag treats 0 as "this mode is off". A poll
		// budget has no off state, so 0 was only ever a typo -- refuse it and
		// leave -1 as the one way to say "uncapped".
		return fmt.Errorf("--poll-max-bytes cannot be 0: use -1 for no cap, or a number of bytes")
	}
	if c.TextMaxRunes < 200 {
		return fmt.Errorf("--text-max-runes must be >= 200, got %d", c.TextMaxRunes)
	}
	if c.ReplyMaxBytes < -1 {
		return fmt.Errorf("--reply-max-bytes must be >= 0 (0 quotes nobody's words) or -1 for uncapped, got %d", c.ReplyMaxBytes)
	}
	if c.ReplyMaxBytes > 1000 {
		// A longer quote is a transcript, and a transcript is what history and get
		// are for; a row is not the place to renegotiate that.
		return fmt.Errorf("--reply-max-bytes must be <= 1000, got %d", c.ReplyMaxBytes)
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
	if c.BreakerErrors < 1 {
		return fmt.Errorf("--breaker-errors must be >= 1, got %d", c.BreakerErrors)
	}
	if c.SendRetryWaitMS < 0 {
		return fmt.Errorf("--send-retry-wait-ms must be >= 0, got %d", c.SendRetryWaitMS)
	}
	if err := checkGate("--inline-max-bytes-image", c.InlineMaxBytesImage); err != nil {
		return err
	}
	if err := checkGate("--inline-max-side", c.InlineMaxSide); err != nil {
		return err
	}
	if err := checkGate("--inline-max-count", c.InlineMaxCount); err != nil {
		return err
	}
	if err := checkGate("--inline-max-total-bytes", c.InlineMaxTotalBytes); err != nil {
		return err
	}
	if err := checkGate("--media-save-max-bytes", c.MediaSaveMaxBytes); err != nil {
		return err
	}
	if c.MediaOut == "" && !Disabled(c.MediaSaveMaxBytes) {
		// --media-save-max-bytes=0 is the one switch that turns the save mode off;
		// an empty --media-out without it means the operator forgot a path.
		return fmt.Errorf("--media-out is empty but --media-save-max-bytes is %d: give the save mode a directory, or set --media-save-max-bytes=0 to turn it off", c.MediaSaveMaxBytes)
	}
	if err := checkLocalPath(c.LocalPath); err != nil {
		return err
	}
	switch strings.ToLower(c.LogLevel) {
	case "debug", "info", "warn", "warning", "error":
	default:
		return fmt.Errorf("--log-level must be debug|info|warn|error, got %q", c.LogLevel)
	}
	// A tool name is an identifier the caller has to type back, so a prefix with
	// whitespace or a path separator in it would register something nobody can
	// call. Trim the edges, then refuse anything sneaking through.
	c.ToolPrefix = strings.TrimSpace(c.ToolPrefix)
	if strings.ContainsAny(c.ToolPrefix, " \t\n/") {
		return fmt.Errorf("--tool-prefix must not contain whitespace or '/', got %q", c.ToolPrefix)
	}
	c.checkACLOverlap()
	return nil
}

// mergeTargets concatenates two alias-or-id lists, dropping blank entries and
// duplicates while keeping order (flags first, then the routing file).
func mergeTargets(flagVals, routing []string) []string {
	out := make([]string, 0, len(flagVals)+len(routing))
	seen := map[string]bool{}
	for _, list := range [][]string{flagVals, routing} {
		for _, s := range list {
			s = strings.TrimSpace(s)
			if s == "" || seen[s] {
				continue
			}
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// checkACLOverlap reports entries named by BOTH allow_channels and
// deny_channels. Deny always wins, so this is a warning rather than an error --
// but the two lists mean different things (allow = the rule exists, deny = it is
// shut), so an entry in both almost certainly means the alias was written wrong
// in one of them.
func (c *Config) checkACLOverlap() {
	if len(c.AllowChannels) == 0 || len(c.DenyChannels) == 0 {
		return
	}
	// Aliases and ids describe the same target, so compare canonical forms.
	byAlias := map[string]string{}
	for i := range c.Routing.Targets {
		t := &c.Routing.Targets[i]
		if kind, id, err := NormalizeTargetID(t.ID); err == nil {
			byAlias[strings.TrimSpace(t.Alias)] = CanonicalTarget(kind, id)
		}
	}
	canon := func(s string) string {
		s = strings.TrimSpace(s)
		if v, ok := byAlias[s]; ok {
			return v
		}
		if kind, id, err := NormalizeTargetID(s); err == nil {
			return CanonicalTarget(kind, id)
		}
		return s
	}
	deny := map[string]bool{}
	for _, s := range c.DenyChannels {
		deny[canon(s)] = true
	}
	for _, s := range c.AllowChannels {
		if k := canon(s); deny[k] {
			c.Warnings = append(c.Warnings, fmt.Sprintf(
				"%s is in both allow_channels and deny_channels; deny wins (it cannot be read either)", k))
		}
	}
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

// MessageContentEnabled reports whether we ask Discord for the privileged
// Message Content intent. Off means MESSAGE_CREATE arrives with an empty
// content: the queue still fills up, but 'text' says nothing.
func (c *Config) MessageContentEnabled() bool { return !c.NoMessageContent }

// NickLookupEnabled is whether the read path may go looking for the guild
// nickname a REST message object does not carry (see NickCache / NickOff).
func (c *Config) NickLookupEnabled() bool { return c.NickLookup != NickOff }

// SaveEnabled reports whether the save mode is available. --media-save-max-bytes=0
// is the switch that turns it off; an empty --media-out can only mean the same
// thing, because validate refuses a non-zero cap without a directory.
func (c *Config) SaveEnabled() bool {
	return c.MediaOut != "" && !Disabled(c.MediaSaveMaxBytes)
}

// AnySave reports whether at least one kind asks for the save mode. It decides
// whether the cache directory has to exist at all: with nothing set to save,
// the server should not leave an empty folder behind on first run. The sticker
// switch counts too even though it lives outside the per-kind map -- a root that
// never opens turns every "save" into a silent save_off.
func (c *Config) AnySave() bool {
	if c.MediaSticker == MediaSave {
		return true
	}
	for _, k := range []string{KindImage, KindAudio, KindVideo, KindOther} {
		if c.MediaFor(k) == MediaSave {
			return true
		}
	}
	return false
}

// LocalPathPrefix returns the prefix we put in front of a saved file's name
// when we tell the agent about it. It is declared, not derived from the
// filesystem: --media-out may live anywhere the operator chose, including
// behind a bind mount, and only they know what that directory is called from
// inside --file-root. Left empty it follows --media-out — a relative one is
// already a name under the working directory, an absolute one lends its last
// element, which is the best guess available.
func (c *Config) LocalPathPrefix() string {
	p := strings.TrimSpace(filepath.ToSlash(c.LocalPath))
	if p == "" {
		p = strings.TrimSpace(filepath.ToSlash(c.MediaOut))
		if p == "" || p == "." || p == "/" {
			return ""
		}
		if filepath.IsAbs(filepath.FromSlash(p)) {
			p = "/" + path.Base(p)
		}
	}
	return cleanPrefix(p)
}

// cleanPrefix tidies a declared prefix: no doubled or trailing slash, and a
// bare "/" means no prefix at all. A leading slash is kept on purpose — it is
// the notation discord_send reads as "inside the file root".
func cleanPrefix(p string) string {
	p = strings.TrimSuffix(path.Clean(p), "/")
	if p == "" || p == "." {
		return ""
	}
	return p
}

// checkLocalPath refuses the two things that stop meaning the same thing once
// the agent repeats the name back to us: '~', and a '..' that climbs out.
func checkLocalPath(p string) error {
	p = strings.TrimSpace(filepath.ToSlash(p))
	if p == "" {
		return nil
	}
	if strings.HasPrefix(p, "~") {
		return fmt.Errorf("--local-path must not start with '~', got %q", p)
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return fmt.Errorf("--local-path must not contain '..', got %q", p)
		}
	}
	return nil
}

// CanInline reports whether inlining is switched on at all. Under the "-1 = no
// limit, 0 = off" rule every gate counts: a total of 0 bytes, 0 images or a
// longest side of 0 leaves nothing that could be inlined, so the mode is off.
// A gate at -1 only stops checking, it does not disable anything.
func (c *Config) CanInline() bool {
	return !Disabled(c.InlineMaxCount) &&
		!Disabled(c.InlineMaxTotalBytes) &&
		!Disabled(c.InlineMaxBytesImage) &&
		!Disabled(c.InlineMaxSide)
}

// Unlimited reports whether one of the cap flags means "no limit".
func Unlimited(v int) bool { return v < 0 }

// Disabled reports whether one of the cap flags means "this mode is off".
func Disabled(v int) bool { return v == 0 }

// checkGate validates one of those caps: -1 (no limit), 0 (off) or a positive
// number. Anything below -1 is a typo — nobody means "negative four bytes".
func checkGate(flag string, v int) error {
	if v < -1 {
		return fmt.Errorf("%s must be -1 (no limit), 0 (off) or positive, got %d", flag, v)
	}
	return nil
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
		"agent":           c.AgentID,
		"transport":       c.Transport,
		"message_content": c.MessageContentEnabled(),
		"nick_lookup":     c.NickLookup,
		"token_source":    c.TokenSource,
		"token":           c.RedactedToken(),
		"poll": map[string]any{
			"min_wait_s": c.PollMinWait, "max_wait_s": c.PollMaxWait,
			"default_wait_s": c.PollDefaultWait, "jitter_pct": c.PollJitterPct,
		},
		"bus_capacity":    c.BusCapacity,
		"poll_max_bytes":  c.PollMaxBytes,
		"text_max_runes":  c.TextMaxRunes,
		"reply_max_bytes": c.ReplyMaxBytes,
		"media": map[string]any{
			"image": c.MediaFor(KindImage), "audio": c.MediaFor(KindAudio),
			"video": c.MediaFor(KindVideo), "other": c.MediaFor(KindOther),
			"emoji": c.MediaEmoji, "sticker": c.MediaSticker,
			"out": c.MediaOut, "keep": c.MediaKeep.String(), "max_mb": c.MediaMaxMB,
			"local_path":     c.LocalPathPrefix(),
			"save_max_bytes": c.MediaSaveMaxBytes,
		},
		"inline": map[string]any{
			"max_bytes_image": c.InlineMaxBytesImage, "max_side": c.InlineMaxSide,
			"max_count": c.InlineMaxCount, "max_total_bytes": c.InlineMaxTotalBytes,
		},
		"file_roots":        c.FileRoots,
		"max_upload_bytes":  c.MaxUploadBytes,
		"max_files_per_msg": c.MaxFilesPerMsg,
		"rate_limit": map[string]any{
			"sends_per_min": c.MaxSendsPerMin, "gap_ms": c.SendGapMS,
			"breaker_errors": c.BreakerErrors, "retry_wait_ms": c.SendRetryWaitMS,
		},
		"acl": map[string]any{
			"allow_guilds": c.AllowGuilds, "allow_channels": c.AllowChannels,
			"deny_channels": c.DenyChannels, "unknown_channel": c.UnknownChannel,
		},
		"routing_file":   c.RoutingFile,
		"routing_loaded": c.RoutingLoaded,
		"env_file":       c.EnvFile,
		"env_loaded":     c.EnvLoaded,
		"loaded_env":     c.LoadedEnvKeys,
		"dry_run":        c.DryRun,
		"log_level":      c.LogLevel,
		"log_file":       c.LogFile,
		"tz":             c.TZ,
		"tz_offset":      tz.Now().Format("-07:00"),
	}
}

// MarshalJSON refuses to marshal a Config. It carries the bot token, and this
// project encodes maps into tool results, audit lines and logs all over the
// place; a Config that accidentally landed in one of those would leak the token
// into a model context or a file. The secret-free view is Audit().
func (c *Config) MarshalJSON() ([]byte, error) {
	return nil, fmt.Errorf("%s: %s is not marshalable because it holds the bot token; use Audit()",
		Module, reflect.TypeOf(*c).String())
}

// resolveRoots turns --file-root values into existing directories. The value is
// used as typed - an absolute path, or one relative to the cwd, which is exactly
// how the OS resolves it when we open a file under the root. We do not expand ~:
// that is the shell's job, and a second private path language here would only
// surprise people. The only transformation is filepath.Abs, and only so the list
// can be de-duplicated and printed in logs - containment itself is enforced by
// os.Root at open time, not by comparing directory strings here.
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
// every path argument (--media-out, --log-file, --env-file,
// --config, --file-root) is taken as typed — absolute, or relative to the
// process cwd. We never expand ~ (the operator's shell does that, and a second
// private path language would only surprise people), we never consult $HOME,
// $XDG_CACHE_HOME or $TMPDIR, and nothing rewrites a value into a different
// location. filepath.Abs is applied to --file-root for de-duplication and
// readable logs only; the sandbox itself is enforced by os.Root when a file is
// opened (internal/files), so no path string comparison decides access here.
