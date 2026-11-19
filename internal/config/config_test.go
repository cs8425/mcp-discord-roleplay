package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNormalizeToken(t *testing.T) {
	cases := map[string]string{
		"abc.def":         "Bot abc.def",
		"Bot abc.def":     "Bot abc.def",
		"  Bot abc.def  ": "Bot abc.def",
		"bearer xyz":      "bearer xyz",
		"":                "",
	}
	for in, want := range cases {
		if got := NormalizeToken(in); got != want {
			t.Errorf("NormalizeToken(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRedactedTokenNeverLeaksSecret(t *testing.T) {
	c := &Config{Token: NormalizeToken("MM1234567890abcdef.TKN")}
	got := c.RedactedToken()
	if strings.Contains(got, "1234567890abcdef") {
		t.Fatalf("redacted token leaks the secret: %q", got)
	}
	if !strings.HasPrefix(got, "Bot ") {
		t.Fatalf("redacted token lost the scheme: %q", got)
	}
}

func TestNormalizeTargetID(t *testing.T) {
	cases := []struct {
		in       string
		kind, id string
		ok       bool
	}{
		{"channel:1112223334445556", TargetChannel, "1112223334445556", true},
		{"dm:333444555666777888", TargetDM, "333444555666777888", true},
		{"DM:333444555666777888", TargetDM, "333444555666777888", true},
		{"dm:*", TargetDM, WildcardID, true},
		{"channel:*", TargetChannel, WildcardID, true},
		{"1112223334445556", TargetChannel, "1112223334445556", true},
		{"boss", "", "", false},
		{"channel:abc", "", "", false},
		{"dm:12", "", "", false},
		// M2c collapsed the entity model: "user:" is no longer a spelling of a
		// target, and accepting it silently would keep two vocabularies alive.
		{"user:333444555666777888", "", "", false},
		{"", "", "", false},
	}
	for _, c := range cases {
		kind, id, err := NormalizeTargetID(c.in)
		if (err == nil) != c.ok {
			t.Errorf("NormalizeTargetID(%q) err=%v want ok=%v", c.in, err, c.ok)
			continue
		}
		if c.ok && (kind != c.kind || id != c.id) {
			t.Errorf("NormalizeTargetID(%q) = %s/%s want %s/%s", c.in, kind, id, c.kind, c.id)
		}
	}
}

func TestLoadRequiresTokenUnlessSelfTest(t *testing.T) {
	os.Unsetenv("DISCORD_TOKEN")
	if _, err := Load([]string{"-no-file-root"}); err == nil {
		t.Fatal("expected an error when no token is set")
	}
	cfg, err := Load([]string{"-selftest", "-no-file-root"})
	if err != nil {
		t.Fatalf("selftest must not require a token: %v", err)
	}
	if cfg.Token != "" {
		t.Fatalf("unexpected token %q", cfg.Token)
	}
}

func TestLoadPrecedenceEnvAndDotEnv(t *testing.T) {
	dir := t.TempDir()
	envFile := filepath.Join(dir, ".env")
	body := "# comment\nexport DISCORD_TOKEN=fromdotenv\nMCPDR_BUS_CAPACITY=77\nQUOTED=\"a\\nb\"\n"
	if err := os.WriteFile(envFile, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	os.Unsetenv("DISCORD_TOKEN")
	os.Unsetenv("MCPDR_BUS_CAPACITY")

	cfg, err := Load([]string{"-selftest", "-no-file-root", "-env-file=" + envFile})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Token != "Bot fromdotenv" {
		t.Fatalf("token from .env = %q", cfg.Token)
	}
	if cfg.TokenSource != "env:DISCORD_TOKEN" {
		t.Fatalf("token source = %q", cfg.TokenSource)
	}
	if cfg.BusCapacity != 77 {
		t.Fatalf("MCPDR_BUS_CAPACITY should apply, got %d", cfg.BusCapacity)
	}
	if got := os.Getenv("QUOTED"); got != "a\nb" {
		t.Fatalf("escape handling: %q", got)
	}

	// A command line flag must beat the environment.
	os.Setenv("MCPDR_BUS_CAPACITY", "88")
	defer os.Unsetenv("MCPDR_BUS_CAPACITY")
	cfg2, err := Load([]string{"-selftest", "-no-file-root", "-env-file=" + envFile, "-bus-capacity=1234"})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg2.BusCapacity != 1234 {
		t.Fatalf("flag must win over env, got %d", cfg2.BusCapacity)
	}
}

func TestLoadRealEnvBeatsDotEnv(t *testing.T) {
	dir := t.TempDir()
	envFile := filepath.Join(dir, ".env")
	if err := os.WriteFile(envFile, []byte("DISCORD_TOKEN=fromdotenv\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DISCORD_TOKEN", "fromrealenv")

	cfg, err := Load([]string{"-selftest", "-no-file-root", "-env-file=" + envFile})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Token != "Bot fromrealenv" {
		t.Fatalf("real environment must win, got %q", cfg.Token)
	}
}

func TestLoadValidations(t *testing.T) {
	cases := [][]string{
		{"-selftest", "-no-file-root", "-poll-default-wait=9999"},
		{"-selftest", "-no-file-root", "-poll-max-wait=1"},
		{"-selftest", "-no-file-root", "-media-image=bogus"},
		{"-selftest", "-no-file-root", "-unknown-channel=queue"},
		{"-selftest", "-no-file-root", "-transport=http"},
		{"-selftest", "-no-file-root", "-bus-capacity=2"},
		{"-selftest", "-file-root=/nonexistent/dir/here"},
	}
	for _, args := range cases {
		if _, err := Load(args); err == nil {
			t.Errorf("expected error for %v", args)
		}
	}
}

func TestLoadRejectsUnknownRoutingKeys(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "routing.json")
	if err := os.WriteFile(bad, []byte(`{"targets":[{"alias":"a","id":"channel:111222333444","writ":true}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load([]string{"-selftest", "-no-file-root", "-config=" + bad}); err == nil {
		t.Fatal("unknown JSON keys must be rejected (typo could grant write access)")
	}
}

func TestMediaForAndSaveGuard(t *testing.T) {
	// The save mode has exactly one switch: --media-save-max-bytes=0. An empty
	// --media-out without it is a missing path, not another way to say "off".
	if _, err := Load([]string{"-selftest", "-no-file-root", "-media-out="}); err == nil {
		t.Fatal("empty --media-out with a live save cap must fail validation")
	}
	if _, err := Load([]string{"-selftest", "-no-file-root",
		"-media-out=", "-media-save-max-bytes=0"}); err != nil {
		t.Fatalf("empty --media-out plus cap 0 is the documented way off: %v", err)
	}
	cfg, err := Load([]string{"-selftest", "-no-file-root", "-media-audio=inline"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MediaFor(KindAudio) != MediaInline {
		t.Fatalf("audio policy = %v", cfg.MediaFor(KindAudio))
	}
	if cfg.MediaFor(KindVideo) != MediaSkip {
		t.Fatalf("video default = %v, want skip", cfg.MediaFor(KindVideo))
	}
	if cfg.MediaFor("nonsense") != MediaSkip {
		t.Fatal("unknown kind must default to skip")
	}
}

func TestFileRootDefaultsToCwd(t *testing.T) {
	cfg, err := Load([]string{"-selftest"})
	if err != nil {
		t.Fatal(err)
	}
	cwd, _ := os.Getwd()
	if len(cfg.FileRoots) != 1 || cfg.FileRoots[0] != filepath.Clean(cwd) {
		t.Fatalf("file roots = %v, want [%s]", cfg.FileRoots, cwd)
	}
}

// The paths this server *writes* must stay plain and cwd-local: no $HOME, no
// XDG cache dir, no /tmp, and no rewriting of what the operator typed.
func TestWrittenPathsStayRelativeToCwd(t *testing.T) {
	cfg, err := Load([]string{"-selftest"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MediaOut != DefaultMediaOut {
		t.Fatalf("defaults moved: media=%q", cfg.MediaOut)
	}
	for _, p := range []string{cfg.MediaOut} {
		if filepath.IsAbs(p) || filepath.Dir(p) != "." || strings.ContainsAny(p, "~$") {
			t.Fatalf("%q must be a plain name inside the cwd", p)
		}
	}

	// What is passed is stored verbatim: not Abs'd, not Clean'd, no ~ expansion.
	cfg, err = Load([]string{"-selftest", "-media-out=./media/"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MediaOut != "./media/" {
		t.Fatalf("media out was rewritten: %q", cfg.MediaOut)
	}

	// cap 0 turns the save mode off; an empty --media-out is legal alongside it
	// and means the same thing -- nothing gets written either way.
	cfg, err = Load([]string{"-selftest", "-media-save-max-bytes=0"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SaveEnabled() {
		t.Fatalf("--media-save-max-bytes=0 should disable save, got %q", cfg.MediaOut)
	}
	cfg, err = Load([]string{"-selftest", "-media-out=", "-media-save-max-bytes=0"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MediaOut != "" || cfg.SaveEnabled() {
		t.Fatalf("empty --media-out plus cap 0 should disable save, got %q", cfg.MediaOut)
	}
}

// The poll text budget (bytes of `text` only): default 16 KiB, -1 means uncapped, and 0 is refused --
// it used to be a second way to say "uncapped" while every other byte flag reads
// 0 as "this mode is off", which is exactly the kind of ambiguity an operator
// types wrong. How small or large a real budget is stays their business.
func TestPollMaxBytes(t *testing.T) {
	cfg, err := Load([]string{"-selftest"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PollMaxBytes != DefaultPollMaxBytes {
		t.Fatalf("default poll budget moved: %d", cfg.PollMaxBytes)
	}
	cfg, err = Load([]string{"-selftest", "-poll-max-bytes=40000"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PollMaxBytes != 40000 {
		t.Fatalf("budget not applied: %d", cfg.PollMaxBytes)
	}
	cfg, err = Load([]string{"-selftest", "-poll-max-bytes=-1"})
	if err != nil {
		t.Fatalf("-1 must mean uncapped and load, got %v", err)
	}
	if !Unlimited(cfg.PollMaxBytes) {
		t.Fatalf("-1 lost: %d", cfg.PollMaxBytes)
	}
	if _, err := Load([]string{"-selftest", "-poll-max-bytes=100"}); err != nil {
		t.Fatalf("a small budget is silly but legal, and must load: %v", err)
	}
	if _, err := Load([]string{"-selftest", "-poll-max-bytes=0"}); err == nil {
		t.Fatal("--poll-max-bytes=0 must be refused: use -1 for no cap")
	}
	if _, err := Load([]string{"-selftest", "-poll-max-bytes=one"}); err == nil {
		t.Fatal("a non-numeric budget must be rejected")
	}
	t.Setenv("MCPDR_POLL_MAX_BYTES", "20000")
	cfg, err = Load([]string{"-selftest", "-env-file=/nonexistent"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PollMaxBytes != 20000 {
		t.Fatalf("MCPDR_POLL_MAX_BYTES ignored: %d", cfg.PollMaxBytes)
	}
}

// Regression: the repeatable ACL flags are collected in flag.Value structs, and
// an earlier build never copied them into Config — so --allow-channels /
// --deny-channels / --allow-guilds silently did nothing. Both the command line
// and the MCPDR_* env path must land in the struct.
func TestACLListsReachTheStruct(t *testing.T) {
	cfg, err := Load([]string{"-selftest", "-no-file-root",
		"-allow-guilds=1112223334445556,2223334445556667",
		"-allow-channels=3334445556667778",
		"-allow-channels=4445556667778889",
		"-deny-channels=5556667778889990"})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(cfg.AllowGuilds, ","); got != "1112223334445556,2223334445556667" {
		t.Fatalf("comma split broken: %v", cfg.AllowGuilds)
	}
	if got := strings.Join(cfg.AllowChannels, ","); got != "3334445556667778,4445556667778889" {
		t.Fatalf("repeated flag broken: %v", cfg.AllowChannels)
	}
	if len(cfg.DenyChannels) != 1 || cfg.DenyChannels[0] != "5556667778889990" {
		t.Fatalf("deny list lost: %v", cfg.DenyChannels)
	}

	t.Setenv("MCPDR_DENY_CHANNELS", "6667778889990001")
	t.Setenv("MCPDR_ALLOW_GUILDS", "7778889990001112")
	defer os.Unsetenv("MCPDR_ALLOW_GUILDS")
	viaEnv, err := Load([]string{"-selftest", "-no-file-root", "-env-file=/nonexistent"})
	if err != nil {
		t.Fatal(err)
	}
	if len(viaEnv.DenyChannels) != 1 || viaEnv.DenyChannels[0] != "6667778889990001" {
		t.Fatalf("MCPDR_DENY_CHANNELS lost (copy must happen after applyEnv): %v", viaEnv.DenyChannels)
	}
	if len(viaEnv.AllowGuilds) != 1 || viaEnv.AllowGuilds[0] != "7778889990001112" {
		t.Fatalf("MCPDR_ALLOW_GUILDS lost: %v", viaEnv.AllowGuilds)
	}
}

func TestSnowflakeTime(t *testing.T) {
	// 175928847299117063 → 2023-10-22T04:42:25Z-ish; only assert monotonic sanity.
	tm, ok := SnowflakeTime("175928847299117063")
	if !ok {
		t.Fatal("snowflake should parse")
	}
	if tm.Year() < 2015 || tm.Year() > 2100 {
		t.Fatalf("implausible snowflake time %s", tm)
	}
	if _, ok := SnowflakeTime("nope"); ok {
		t.Fatal("non-numeric snowflake must not parse")
	}
}

// --tool-prefix is the only knob for how the tools are named, and its default is
// deliberately empty: bare poll/send/status, because the client is the one that
// namespaces them. A prefix is glued on, not separated, so it must not contain
// anything that would break the identifier the caller has to type back.
func TestLoadToolPrefix(t *testing.T) {
	cfg, err := Load([]string{"-selftest", "-no-file-root"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ToolPrefix != "" {
		t.Fatalf("the default prefix must be empty, got %q", cfg.ToolPrefix)
	}
	with, err := Load([]string{"-selftest", "-no-file-root", "-tool-prefix=dc_"})
	if err != nil || with.ToolPrefix != "dc_" {
		t.Fatalf("prefix lost: %q (%v)", with.ToolPrefix, err)
	}
	trimmed, err := Load([]string{"-selftest", "-no-file-root", "-tool-prefix= dc_ "})
	if err != nil || trimmed.ToolPrefix != "dc_" {
		t.Fatalf("the edges must be trimmed: %q (%v)", trimmed.ToolPrefix, err)
	}
	if _, err := Load([]string{"-selftest", "-no-file-root", "-tool-prefix=bad prefix"}); err == nil {
		t.Fatal("a prefix with a space registers a tool nobody can name back")
	}
	if _, err := Load([]string{"-selftest", "-no-file-root", "-tool-prefix=a/b"}); err == nil {
		t.Fatal("a slash is not part of a tool name")
	}
}

// --media-emoji is the one media knob that is not a MediaMode. Custom emoji either
// get their pictures inlined or keep their codes: "save" is not on the menu, because
// a saved emoji is a path to a filename nobody chose (see internal/discord/emoji.go
// for why the name is no help).
func TestMediaEmojiFlag(t *testing.T) {
	cfg, err := Load([]string{"-selftest", "-no-file-root"})
	if err != nil {
		t.Fatalf("defaults: %v", err)
	}
	if cfg.MediaEmoji != EmojiInline {
		t.Errorf("default MediaEmoji = %q, want %q", cfg.MediaEmoji, EmojiInline)
	}

	cfg, err = Load([]string{"-selftest", "-no-file-root", "-media-emoji=TEXT"})
	if err != nil {
		t.Fatalf("case-insensitive value: %v", err)
	}
	if cfg.MediaEmoji != EmojiText {
		t.Errorf("MediaEmoji = %q, want %q", cfg.MediaEmoji, EmojiText)
	}

	for _, bad := range []string{"save", "skip", "both"} {
		if _, err := Load([]string{"-selftest", "-no-file-root", "-media-emoji=" + bad}); err == nil {
			t.Errorf("media-emoji=%s was accepted, want an error", bad)
		}
	}
}

// --media-sticker is a real MediaMode, but it sits beside --media-image instead of
// inside it: what the operator set for photographs is not necessarily what a sticker
// should get, and a sticker is content, so the default is the visible one.
func TestMediaStickerFlag(t *testing.T) {
	cfg, err := Load([]string{"-selftest", "-no-file-root"})
	if err != nil {
		t.Fatalf("defaults: %v", err)
	}
	if cfg.MediaSticker != MediaInline {
		t.Errorf("default MediaSticker = %q, want %q (a sticker is content)", cfg.MediaSticker, MediaInline)
	}

	for _, want := range []MediaMode{MediaInline, MediaSave, MediaSkip} {
		got, err := Load([]string{"-selftest", "-no-file-root", "-media-sticker=" + strings.ToUpper(string(want))})
		if err != nil {
			t.Fatalf("media-sticker=%s: %v", want, err)
		}
		if got.MediaSticker != want {
			t.Errorf("media-sticker=%s: got %q", want, got.MediaSticker)
		}
	}

	for _, bad := range []string{"text", "both", ""} {
		if _, err := Load([]string{"-selftest", "-no-file-root", "-media-sticker=" + bad}); err == nil {
			t.Errorf("media-sticker=%q was accepted, want an error", bad)
		}
	}

	// AnySave is what makes the cache root open at all. The sticker switch lives
	// outside the per-kind map, so it is easy to forget -- and forgetting it turns
	// every "save" into a silent save_off, which reads like a broken directory.
	if cfg, err := Load([]string{"-selftest", "-no-file-root", "-media-sticker=save"}); err != nil {
		t.Fatalf("media-sticker=save: %v", err)
	} else if !cfg.AnySave() {
		t.Error("AnySave() is false when only the sticker asks for save: the media root would never open")
	}
}
