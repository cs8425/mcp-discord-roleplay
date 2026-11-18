package discord

import (
	"bytes"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/bwmarrin/discordgo"
)

// discordgo logs its own internals through the package-level Logger hook, which
// gatewayLog bridges into slog. The one thing worth guarding is the "First
// Packet" line: discordgo %#v-dumps the whole first event including RawData (the
// READY payload, kilobytes), so the bridge shortens it at info and prints it in
// full only when debug logging is on.

func logClient(lvl slog.Level) (*Client, *bytes.Buffer) {
	buf := &bytes.Buffer{}
	return &Client{log: slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: lvl}))}, buf
}

// firstPacketDump is what websocket.go's s.log(LogInformational,
// "First Packet:\n%#v\n", e) produces for a bot in many guilds.
func firstPacketDump(n int) string {
	return "First Packet:\n&discordgo.Event{Operation:0, Sequence:1, Type:\"READY\", RawData:jsontext.Value{" +
		strings.Repeat("0x7b, ", n) + "}}\n"
}

func logLines(t *testing.T, buf *bytes.Buffer) []string {
	t.Helper()
	out := strings.TrimSuffix(buf.String(), "\n")
	if out == "" {
		return nil
	}
	return strings.Split(out, "\n")
}

func TestGatewayLogFirstPacketIsHeadAtInfo(t *testing.T) {
	c, buf := logClient(slog.LevelInfo)
	msg := firstPacketDump(1200) // ~7 KB, the complaint this guards
	if len(msg) < 4096 {
		t.Fatalf("fixture too small to prove anything: %d", len(msg))
	}

	c.gatewayLog(discordgo.LogInformational, 2, "%s", msg)

	lines := logLines(t, buf)
	if len(lines) != 1 {
		t.Fatalf("want 1 log line, got %d: %q", len(lines), buf.String())
	}
	line := lines[0]
	if !strings.Contains(line, "level=INFO") {
		t.Errorf("want the head at info level, got %q", line)
	}
	if !strings.Contains(line, "First Packet:") {
		t.Errorf("head lost the message identity: %q", line)
	}
	if want := fmt.Sprintf("(+%d bytes)", len(msg)-dgoFirstPacketHead); !strings.Contains(line, want) {
		t.Errorf("want truncation marker %q in %q", want, line)
	}
	if strings.Contains(line, "}}") {
		t.Errorf("payload tail survived, nothing was cut: %q", line)
	}
	if len(line) > 400 {
		t.Errorf("info line is %d bytes, want a short head: %q", len(line), line)
	}
}

func TestGatewayLogFirstPacketIsFullAtDebug(t *testing.T) {
	c, buf := logClient(slog.LevelDebug)
	msg := firstPacketDump(1200)

	c.gatewayLog(discordgo.LogInformational, 2, "%s", msg)

	lines := logLines(t, buf)
	if len(lines) != 1 {
		t.Fatalf("want 1 log line, got %d: %q", len(lines), buf.String())
	}
	if !strings.Contains(lines[0], "level=DEBUG") {
		t.Errorf("the full dump belongs to debug, got %q", lines[0])
	}
	if !strings.Contains(lines[0], "}}") {
		t.Errorf("debug must print the whole RawData dump, got %q", lines[0])
	}
	if strings.Contains(lines[0], "(+") {
		t.Errorf("debug output was truncated: %q", lines[0])
	}
}

func TestGatewayLogOtherInfoKeepsFullText(t *testing.T) {
	c, buf := logClient(slog.LevelInfo)
	msg := "We are now connected to Discord, emitting connect event " + strings.Repeat("y", 500)

	c.gatewayLog(discordgo.LogInformational, 2, "%s", msg)

	lines := logLines(t, buf)
	if len(lines) != 1 {
		t.Fatalf("want 1 log line, got %d", len(lines))
	}
	if !strings.Contains(lines[0], strings.Repeat("y", 500)) {
		t.Errorf("only First Packet is shortened; this one lost text: %q", lines[0])
	}
	if strings.Contains(lines[0], "(+") {
		t.Errorf("unexpected truncation: %q", lines[0])
	}
}

func TestGatewayLogReconnectStillCounts(t *testing.T) {
	c, _ := logClient(slog.LevelInfo)

	c.gatewayLog(discordgo.LogInformational, 2, "trying to reconnect to gateway")

	if got := c.Snapshot().Reconnects; got != 1 {
		t.Errorf("Reconnects = %d, want 1", got)
	}
}

// Discord shows a member's guild nickname above global_name/username, and the
// message payload carries the nickname only in Message.Member. Author.Name must
// follow Discord, otherwise an operator renames someone in the server and the
// agent keeps calling them by the old name forever.
func TestNormalizePrefersGuildNickname(t *testing.T) {
	c, _ := logClient(slog.LevelInfo)
	c.cfg = testCfg(t)

	msg := &discordgo.Message{
		ID: "1", ChannelID: "2", GuildID: "g1", Content: "hi",
		Author: &discordgo.User{ID: "u1", Username: "oldname", GlobalName: "olddisplay"},
		Member: &discordgo.Member{Nick: "群內暱稱"},
	}
	ev := c.normalize(nil, msg)
	if ev.Author.Name != "群內暱稱" {
		t.Errorf("Name = %q, want the guild nickname", ev.Author.Name)
	}
	if ev.Author.ID != "u1" {
		t.Errorf("ID = %q, want u1", ev.Author.ID)
	}
	if !strings.HasPrefix(ev.Author.Tag, "oldname") {
		t.Errorf("Tag = %q, want it to keep the account name for the log", ev.Author.Tag)
	}

	// Empty nick in a partial member object means "no nickname": fall back.
	msg.Member = &discordgo.Member{}
	if got := c.normalize(nil, msg).Author.Name; got != "olddisplay" {
		t.Errorf("empty nick: Name = %q, want global_name", got)
	}
	// DMs carry no member at all.
	msg.Member = nil
	if got := c.normalize(nil, msg).Author.Name; got != "olddisplay" {
		t.Errorf("no member: Name = %q, want global_name", got)
	}
}

func TestHeadLogKeepsUTF8AndShortStrings(t *testing.T) {
	if got := headLog("abc", 10); got != "abc" {
		t.Errorf("short string changed: %q", got)
	}

	s := "ab" + strings.Repeat("界", 100) // 302 bytes, 3 per rune
	// n lands on a rune boundary: keep "ab" plus the whole first rune.
	if got, want := headLog(s, 5), "ab界…(+297 bytes)"; got != want {
		t.Errorf("headLog(s, 5) = %q, want %q", got, want)
	}
	// n cuts inside the second rune: back off to the boundary before it.
	if got, want := headLog(s, 4), "ab…(+300 bytes)"; got != want {
		t.Errorf("headLog(s, 4) = %q, want %q", got, want)
	}
	for _, got := range []string{headLog(s, 5), headLog(s, 4)} {
		if strings.Contains(got, "\uFFFD") {
			t.Errorf("cut inside a UTF-8 sequence: %q", got)
		}
	}
}
