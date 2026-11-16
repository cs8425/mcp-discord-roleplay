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
		{"channel:1112223334445556", "channel", "1112223334445556", true},
		{"user:333444555666777888", "user", "333444555666777888", true},
		{"dm:333444555666777888", "user", "333444555666777888", true},
		{"1112223334445556", "channel", "1112223334445556", true},
		{"boss", "", "", false},
		{"channel:abc", "", "", false},
		{"user:12", "", "", false},
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
		{"-selftest", "-no-file-root", "-unknown-dm=queue"},
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
	if _, err := Load([]string{"-selftest", "-no-file-root", "-media-image=save", "-no-media-out", "-media-out="}); err == nil {
		t.Fatal("media-image=save with no --media-out must fail validation")
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
