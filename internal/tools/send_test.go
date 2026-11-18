package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	mcp "mcptransport"

	"mcp-discord-roleplay/internal/bus"
	"mcp-discord-roleplay/internal/config"
	"mcp-discord-roleplay/internal/discord"
	"mcp-discord-roleplay/internal/files"
	"mcp-discord-roleplay/internal/router"
)

// These tests drive the handler the way the transport does: an argument blob in,
// a JSON text part out. Everything runs with -dry-run, so no gateway, no socket,
// no token — the decisions (routing, sandbox, splitting, envelopes) are what is
// under test. The REST behaviour behind Send is covered in internal/discord.

const routingJSON = `{
  "agents": [
    {"id": "default", "targets": ["boss", "ami", "log"]},
    {"id": "solo", "targets": ["boss"]}
  ],
  "targets": [
    {"alias": "boss", "id": "channel:111222333444555666", "name": "#rp-main", "read": true, "write": true},
    {"alias": "ami", "id": "dm:222333444555666777", "name": "Ami"},
    {"alias": "log", "id": "channel:333444555666777888", "read": true, "write": false}
  ],
  "unknown_channel": "ignore"
}`

func toolCfg(t *testing.T, extra ...string) *config.Config {
	t.Helper()
	args := append([]string{"-selftest", "-dry-run", "-config=" + filepath.Join(t.TempDir(), "absent.json")}, extra...)
	cfg, err := config.Load(args)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	return cfg
}

func routingCfg(t *testing.T, extra ...string) *config.Config {
	t.Helper()
	path := filepath.Join(t.TempDir(), "routing.json")
	if err := os.WriteFile(path, []byte(routingJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	// The poll tests in poll_test.go block on max_wait, so the fixture lowers the
	// operator floor to 1 s; the real default (5) is still what TestPollBadMaxWait
	// asserts, because that test re-states both bounds explicitly and a later flag
	// wins. Nothing else in this package waits on a poll.
	return toolCfg(t, append([]string{"-config=" + path, "-poll-min-wait=1"}, extra...)...)
}

func newDeps(t *testing.T, cfg *config.Config) *Deps {
	t.Helper()
	rt, err := router.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	hub := bus.NewHub(cfg.BusCapacity, rt.AgentIDs(), nil)
	return &Deps{
		Cfg:       cfg,
		Router:    rt,
		Hub:       hub,
		DC:        discord.New(cfg, nil, hub, rt),
		Files:     files.New(cfg, nil),
		StartedAt: time.Now(),
	}
}

func call(t *testing.T, d *Deps, ctx context.Context, args string) *mcp.ToolResult {
	t.Helper()
	res, err := d.handleSend(ctx, &mcp.Call{Name: "send", Arguments: json.RawMessage(args)})
	if err != nil {
		t.Fatalf("handleSend must answer through the result, not the protocol: %v", err)
	}
	if len(res.Content) == 0 || res.Content[0].Type != "text" {
		t.Fatalf("answer has no text part: %+v", res)
	}
	return res
}

func decode(t *testing.T, res *mcp.ToolResult) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal([]byte(res.Content[0].Text), &out); err != nil {
		t.Fatalf("answer is not JSON (%q): %v", res.Content[0].Text, err)
	}
	return out
}

// withDebugLog installs a debug logger on d and hands back what it caught. The
// wire answer now says only what the model needs to act on; everything else a
// send decided (which alias, which channel, which ids, when) is written here,
// so a test that cares about the resolution reads the log the way an operator
// would -- and the log itself stops being code nobody ever exercises.
func withDebugLog(t *testing.T, d *Deps) *bytes.Buffer {
	t.Helper()
	buf := &bytes.Buffer{}
	d.Log = slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return buf
}

// sentTo asserts that the debug line for the send(s) since the buffer was
// installed carries each of these facts.
func sentTo(t *testing.T, buf *bytes.Buffer, want ...string) {
	t.Helper()
	line := buf.String()
	for _, w := range want {
		if !strings.Contains(line, w) {
			t.Errorf("the send log must say %q, got: %s", w, line)
		}
	}
}

func sendOK(t *testing.T, d *Deps, args string) map[string]any {
	t.Helper()
	res := call(t, d, context.Background(), args)
	if res.IsError {
		t.Fatalf("expected success, got error answer: %s", res.Content[0].Text)
	}
	out := decode(t, res)
	if out["ok"] != true {
		t.Fatalf("ok != true: %v", out)
	}
	return out
}

// sendErr is the failure path: every rejection must be an isError result whose
// JSON names a code, so the agent can branch instead of guessing.
func sendErr(t *testing.T, d *Deps, args string) map[string]any {
	t.Helper()
	res := call(t, d, context.Background(), args)
	if !res.IsError {
		t.Fatalf("expected an isError answer, got success: %s", res.Content[0].Text)
	}
	out := decode(t, res)
	if out["ok"] != false {
		t.Errorf("a failure must say ok:false, got %v", out["ok"])
	}
	if s, _ := out["error"].(string); s == "" {
		t.Errorf("a failure must carry an error code: %v", out)
	}
	if s, _ := out["hint"].(string); s == "" {
		t.Errorf("a failure must carry a hint the model can act on: %v", out)
	}
	return out
}

func wantStr(t *testing.T, out map[string]any, key, want string) {
	t.Helper()
	if got, _ := out[key].(string); got != want {
		t.Errorf("%s = %v, want %q", key, out[key], want)
	}
}

func wantNum(t *testing.T, out map[string]any, key string, want float64) {
	t.Helper()
	got, ok := out[key].(float64)
	if !ok {
		t.Fatalf("%s is not a number: %v (%T)", key, out[key], out[key])
	}
	if got != want {
		t.Errorf("%s = %v, want %v", key, got, want)
	}
}

func wantCode(t *testing.T, out map[string]any, want string) {
	t.Helper()
	wantStr(t, out, "error", want)
}

// ---------------------------------------------------------------------------
// happy path
// ---------------------------------------------------------------------------

func TestSendDryRunEnvelope(t *testing.T) {
	d := newDeps(t, routingCfg(t))
	buf := withDebugLog(t, d)
	out := sendOK(t, d, `{"to":"boss","text":"hello world"}`)
	if out["dry_run"] != true {
		t.Error("dry-run must say so, or the model will believe it posted")
	}
	// One part, no attachments: the whole answer is that it worked, that this was
	// a rehearsal, and the warning that says what a rehearsal means. A fourth key
	// has to be argued for here.
	if got := keySet(out); got != "dry_run,ok,warnings" {
		t.Errorf("a plain dry-run send costs the model three keys, got %v", out)
	}
	// Everything else is still decided and still recorded, only not on the wire:
	// which alias resolved to which channel, and what Discord called the message.
	sentTo(t, buf, "target=boss", "kind=channel", "channel_id=111222333444555666",
		"message_ids=[dry-run]", "delivered=111222333444555666")
}

func TestSendByEveryTargetForm(t *testing.T) {
	d := newDeps(t, routingCfg(t))
	cases := []struct {
		to    string
		alias string
		kind  string
		ch    string
	}{
		{"boss", "boss", "channel", "111222333444555666"},
		{"BOSS", "boss", "channel", "111222333444555666"}, // aliases are case-insensitive
		{"channel:111222333444555666", "boss", "channel", "111222333444555666"},
		{"#rp-main", "boss", "channel", "111222333444555666"},
		{"dm:222333444555666777", "ami", "dm", "dry-run-dm:222333444555666777"},
		{"ami", "ami", "dm", "dry-run-dm:222333444555666777"},
		{"@ami", "ami", "dm", "dry-run-dm:222333444555666777"},
	}
	for _, tc := range cases {
		t.Run(tc.to, func(t *testing.T) {
			buf := withDebugLog(t, d)
			out := sendOK(t, d, `{"to":`+jsonString(tc.to)+`,"text":"hi"}`)
			if out["ok"] != true {
				t.Fatalf("out = %v", out)
			}
			sentTo(t, buf, "target="+tc.alias, "kind="+tc.kind,
				"channel_id="+tc.ch, "delivered="+tc.ch)
		})
	}

	// M2c: "user:" is no longer a spelling of a target. Rejecting it beats
	// accepting it silently, which would keep two vocabularies alive.
	if out := sendErr(t, d, `{"to":"user:222333444555666777","text":"hi"}`); out["error"] != "unknown_target" {
		t.Fatalf("user: must be gone, got %v", out)
	}
}

// An unlisted channel id is allowed when there is no routing file and no
// --allow-channels: that is the "run it against one server, name nothing" setup.
func TestAdHocBareSnowflake(t *testing.T) {
	d := newDeps(t, toolCfg(t))
	buf := withDebugLog(t, d)
	out := sendOK(t, d, `{"to":"444555666777888999","text":"hi"}`)
	if out["ok"] != true {
		t.Fatalf("out = %v", out)
	}
	sentTo(t, buf, "channel_id=444555666777888999", "kind=channel")
}

// Closing DMs is the deny list talking about a kind, not a flag of its own --
// a DM is a channel, so it gets the same fence every channel gets.
func TestAdHocDenyDMWildcard(t *testing.T) {
	d := newDeps(t, toolCfg(t, "-deny-channels=dm:*"))
	out := sendErr(t, d, `{"to":"dm:222333444555666777","text":"hi"}`)
	wantCode(t, out, "not_allowlisted")
	if _, bad := out["targets"]; bad {
		t.Error("an allow-list refusal is the operator's call, listing aliases would invite probing")
	}
	buf := withDebugLog(t, d)
	if o := sendOK(t, d, `{"to":"444555666777888999","text":"hi"}`); o["ok"] != true {
		t.Fatalf("denying dm:* must leave guild channels alone: %v", o)
	}
	sentTo(t, buf, "kind=channel", "channel_id=444555666777888999")
}

func TestRoutingFileRejectsUnlistedTarget(t *testing.T) {
	d := newDeps(t, routingCfg(t))
	out := sendErr(t, d, `{"to":"444555666777888999","text":"hi"}`)
	wantCode(t, out, "unknown_target")
	list, ok := out["targets"].([]any)
	if !ok || len(list) == 0 {
		t.Fatalf("an unknown target must come back with the names that do work, got %v", out["targets"])
	}
	if list[0] != "boss" {
		t.Errorf("targets should be in declaration order and only the writable ones: %v", list)
	}
	for _, v := range list {
		if v == "log" {
			t.Error("read-only aliases must not be offered as send targets")
		}
	}
}

func TestReadOnlyTargetIsRefusedForSending(t *testing.T) {
	d := newDeps(t, routingCfg(t))
	out := sendErr(t, d, `{"to":"log","text":"hi"}`)
	wantCode(t, out, "write_denied")
}

func TestForeignAgentTargetIsRefused(t *testing.T) {
	d := newDeps(t, routingCfg(t))
	ctx := router.WithPrincipal(context.Background(), router.Principal{AgentID: "nobody"})
	res := call(t, d, ctx, `{"to":"boss","text":"hi"}`)
	if !res.IsError {
		t.Fatal("an agent that routing.json does not declare must not reach anyone")
	}
	out := decode(t, res)
	wantCode(t, out, "foreign_agent")
}

func TestEmptyTargetIsRejected(t *testing.T) {
	d := newDeps(t, routingCfg(t))
	out := sendErr(t, d, `{"to":"   ","text":"hi"}`)
	wantCode(t, out, "empty_target")
	if _, ok := out["targets"].([]any); !ok {
		t.Error("an empty to: should list what could be named")
	}
}

func TestNothingToSend(t *testing.T) {
	d := newDeps(t, routingCfg(t))
	for _, args := range []string{`{"to":"boss"}`, `{"to":"boss","text":""}`, `{"to":"boss","text":"  \n\t "}`} {
		out := sendErr(t, d, args)
		wantCode(t, out, "empty_message")
	}
}

func TestBadArgumentsIsRejected(t *testing.T) {
	d := newDeps(t, routingCfg(t))
	out := sendErr(t, d, `{"to":`)
	wantCode(t, out, "bad_arguments")
	out = sendErr(t, d, `{"to":"boss","files":"not-an-array"}`)
	wantCode(t, out, "bad_arguments")
	// A call with no arguments at all is accepted as an empty object, and then
	// fails for the reason it should: no target named.
	out = sendErr(t, d, ``)
	wantCode(t, out, "empty_target")
}

func TestNoClientMeansNotConnected(t *testing.T) {
	cfg := toolCfg(t)
	rt, err := router.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	d := &Deps{Cfg: cfg, Router: rt, Hub: bus.NewHub(cfg.BusCapacity, rt.AgentIDs(), nil), Files: files.New(cfg, nil)}
	out := sendErr(t, d, `{"to":"channel:111222333444555666","text":"hi"}`)
	wantCode(t, out, discord.CodeNotConnected)
}

// "me" is a DM to the bot itself: it needs the READY payload for the bot's own
// id, and must say so politely before that arrives.
func TestMeNeedsOwnID(t *testing.T) {
	d := newDeps(t, toolCfg(t))
	out := sendErr(t, d, `{"to":"me","text":"note to self"}`)
	wantCode(t, out, discord.CodeNotConnected)
	wantStr(t, out, "target", "me")
}

// ---------------------------------------------------------------------------
// last target
// ---------------------------------------------------------------------------

func TestLastTargetRoundTripAndIsolation(t *testing.T) {
	d := newDeps(t, routingCfg(t))

	out := sendErr(t, d, `{"to":"last","text":"hi"}`)
	wantCode(t, out, "no_last_target")
	if _, ok := out["targets"].([]any); !ok {
		t.Error("no_last_target should offer the aliases it could use instead")
	}

	if o := sendOK(t, d, `{"to":"ami","text":"first"}`); o["ok"] != true {
		t.Fatalf("setup send failed: %v", o)
	}
	buf := withDebugLog(t, d)
	sendOK(t, d, `{"to":"LAST","text":"follow-up"}`)
	sentTo(t, buf, "target=ami", "channel_id=dry-run-dm:222333444555666777")

	// A different agent on the same process must not inherit it.
	ctx := router.WithPrincipal(context.Background(), router.Principal{AgentID: "solo"})
	res := call(t, d, ctx, `{"to":"last","text":"hi"}`)
	if !res.IsError {
		t.Fatal("'last' leaked across agents")
	}
	wantCode(t, decode(t, res), "no_last_target")

	// ... and its own send creates its own pointer.
	soloCtx := router.WithPrincipal(context.Background(), router.Principal{AgentID: "solo"})
	if o := decode(t, call(t, d, soloCtx, `{"to":"boss","text":"hi"}`)); o["ok"] != true {
		t.Fatalf("solo send failed: %v", o)
	}
	buf = withDebugLog(t, d)
	if o := decode(t, call(t, d, soloCtx, `{"to":"last","text":"again"}`)); o["ok"] != true {
		t.Fatalf("solo 'last' send failed: %v", o)
	}
	sentTo(t, buf, "target=boss")
	// The default agent still points at its own conversation.
	buf = withDebugLog(t, d)
	sendOK(t, d, `{"to":"last","text":"mine"}`)
	sentTo(t, buf, "target=ami")
}

func TestLastSurvivesTargetRemoval(t *testing.T) {
	// A remembered pointer is re-checked against the ACL, so revoking write
	// access takes effect immediately even for to:"last".
	dir := t.TempDir()
	path := filepath.Join(dir, "routing.json")
	on := strings.Replace(routingJSON, `"alias": "log", "id": "channel:333444555666777888", "read": true, "write": false`,
		`"alias": "log", "id": "channel:333444555666777888", "read": true, "write": true`, 1)
	if on == routingJSON {
		t.Fatal("the routing fixture changed shape; this test no longer tests anything")
	}
	if err := os.WriteFile(path, []byte(on), 0o600); err != nil {
		t.Fatal(err)
	}
	d := newDeps(t, toolCfg(t, "-config="+path))
	if o := sendOK(t, d, `{"to":"log","text":"while allowed"}`); o["ok"] != true {
		t.Fatalf("setup: %v", o)
	}

	// Same alias, now read-only, and a router rebuilt from it.
	off := routingJSON
	if err := os.WriteFile(path, []byte(off), 0o600); err != nil {
		t.Fatal(err)
	}
	rt, err := router.New(toolCfg(t, "-config="+path))
	if err != nil {
		t.Fatal(err)
	}
	d.Router = rt
	out := sendErr(t, d, `{"to":"last","text":"should be refused now"}`)
	wantCode(t, out, "write_denied")
}

// ---------------------------------------------------------------------------
// splitting
// ---------------------------------------------------------------------------

func TestSplitKeepsEveryCharacter(t *testing.T) {
	body := "first line here\n" +
		strings.Repeat("word ", 700) + // 3500 chars, breaks at spaces
		"\n句子結束了。" + strings.Repeat("中文字", 700) + // CJK: runes, not bytes
		"\n" + strings.Repeat("x", 4321) // nothing to break at: hard cut
	parts, hard := splitMessage(body, discord.MaxContentRunes)
	if !hard {
		t.Error("a 4321-character unbroken run must report a hard cut")
	}
	if len(parts) < 4 {
		t.Fatalf("expected several parts, got %d", len(parts))
	}
	var joined strings.Builder
	for i, p := range parts {
		if n := len([]rune(p)); n > discord.MaxContentRunes {
			t.Errorf("part %d is %d runes, over the limit", i, n)
		}
		if strings.TrimSpace(p) == "" {
			t.Errorf("part %d is blank: the model would see an empty message on Discord", i)
		}
		joined.WriteString(p)
	}
	// The only characters allowed to disappear are the whitespace we broke on.
	squeeze := func(s string) string {
		return strings.Map(func(r rune) rune {
			if r == ' ' || r == '\n' || r == '\t' || r == '\r' {
				return -1
			}
			return r
		}, s)
	}
	if got, want := squeeze(joined.String()), squeeze(body); got != want {
		t.Errorf("split lost or invented non-whitespace content (got %d runes, want %d)", len([]rune(got)), len([]rune(want)))
	}
}

func TestSplitBoundaries(t *testing.T) {
	cases := []struct {
		name  string
		in    string
		limit int
		want  []string
		hard  bool
	}{
		{"empty", "", 20, nil, false},
		{"only whitespace", "  \n\t ", 20, nil, false},
		{"exact fit", strings.Repeat("a", 20), 20, []string{strings.Repeat("a", 20)}, false},
		{"one over, breaks at the space", strings.Repeat("a", 19) + " b", 20, []string{strings.Repeat("a", 19), "b"}, false},
		{"newline wins over space", "wwwwwwwwwwww\n" + strings.Repeat("v", 20), 20, []string{strings.Repeat("w", 12), strings.Repeat("v", 20)}, false},
		{"trailing whitespace trimmed", "hello \n\n", 20, []string{"hello"}, false},
		{"tiny limit is clamped to 20", strings.Repeat("1234567890", 2), 10, []string{strings.Repeat("1234567890", 2)}, false},
		{"unbreakable hard cut", strings.Repeat("x", 25), 20, []string{strings.Repeat("x", 20), "xxxxx"}, true},
		{"sentence end", strings.Repeat("字", 12) + "。" + strings.Repeat("词", 20), 20, []string{strings.Repeat("字", 12) + "。", strings.Repeat("词", 20)}, false},
		{"closer absorbed", strings.Repeat("a", 12) + "！」" + strings.Repeat("b", 20), 20, []string{strings.Repeat("a", 12) + "！」", strings.Repeat("b", 20)}, false},
		{"break before min is ignored", "hello\n" + strings.Repeat("w", 18), 20, []string{"hello\n" + strings.Repeat("w", 14), "wwww"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, hard := splitMessage(tc.in, tc.limit)
			if len(got) != len(tc.want) {
				t.Fatalf("parts = %q, want %q", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("part %d = %q, want %q", i, got[i], tc.want[i])
				}
			}
			if hard != tc.hard {
				t.Errorf("hardCut = %v, want %v", hard, tc.hard)
			}
		})
	}
}

func TestSplitCountsRunesNotBytes(t *testing.T) {
	body := strings.Repeat("漢", 4001) // 12003 bytes
	parts, hard := splitMessage(body, discord.MaxContentRunes)
	if !hard {
		t.Error("no break points, so the cut is hard by necessity")
	}
	if len(parts) != 3 {
		t.Fatalf("parts = %d, want 3 (a byte-based split would give many more)", len(parts))
	}
	for i, p := range parts {
		if n := len([]rune(p)); n > discord.MaxContentRunes {
			t.Errorf("part %d = %d runes", i, n)
		}
	}
}

func TestTooManyPartsIsRefused(t *testing.T) {
	d := newDeps(t, routingCfg(t))
	body := strings.Repeat("x", 2000*maxSendParts+1)
	out := sendErr(t, d, `{"to":"boss","text":`+jsonString(body)+`}`)
	wantCode(t, out, "text_too_long")
	wantNum(t, out, "runes", float64(len([]rune(body))))
	wantNum(t, out, "max_parts", maxSendParts)
}

func TestURLsJoinTheBodyBeforeSplitting(t *testing.T) {
	d := newDeps(t, routingCfg(t))
	// 1995 runes of text plus one link cannot fit in one message: the proof that
	// the link is pasted into the body rather than sent as an attachment.
	text := strings.Repeat("y", 1995)
	out := sendOK(t, d, `{"to":"boss","text":`+jsonString(text)+`,"files":[{"url":"https://example.com/a.png"}]}`)
	wantNum(t, out, "parts_sent", 2)
	wantNum(t, out, "url_count", 1)
	if _, bad := out["attachments"]; bad {
		t.Error("a url is not an attachment")
	}
	// Link with no text at all: the link is the message.
	out = sendOK(t, d, `{"to":"boss","files":[{"url":"https://example.com/a.png"},{"url":"https://example.com/b.png"}]}`)
	if _, bad := out["parts_sent"]; bad {
		t.Errorf("one part needs no counter: %v", out)
	}
	wantNum(t, out, "url_count", 2)
}

func TestURLsAreNeverFetched(t *testing.T) {
	// Anything that is not one clean absolute http(s) URL is refused outright:
	// file/data/javascript schemes, host-less links, and control characters that
	// would smuggle extra lines into the message.
	d := newDeps(t, routingCfg(t))
	bad := []string{
		"file:///etc/passwd",
		"data:text/plain,hello",
		"javascript:alert(1)",
		"ftp://example.com/x",
		"//example.com/x",
		"https://",
		"http:///nohost",
		"https://example.com/a\nb",
		"https://example.com/a\tb",
	}
	for _, u := range bad {
		t.Run(strings.ReplaceAll(strings.ReplaceAll(u, "\n", "\\n"), "\t", "\\t"), func(t *testing.T) {
			out := sendErr(t, d, `{"to":"boss","files":[{"url":`+jsonString(u)+`}]}`)
			wantCode(t, out, files.CodeBadURL)
			wantNum(t, out, "file_index", 0)
		})
	}
	out := sendErr(t, d, `{"to":"boss","files":[{"url":"https://example.com/"},{"url":"nope"}]}`)
	wantCode(t, out, files.CodeBadURL)
	wantNum(t, out, "file_index", 1)
}

func TestCancelledCallerDoesNotFinishTheMessage(t *testing.T) {
	// Each part is a separate request. If the caller hangs up between them, the
	// answer says exactly how much landed, so the resume is not guesswork.
	d := newDeps(t, routingCfg(t))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res := call(t, d, ctx, `{"to":"boss","text":`+jsonString(strings.Repeat("z", 2001))+`}`)
	if !res.IsError {
		t.Fatal("a cancelled caller must not get a success answer")
	}
	out := decode(t, res)
	wantCode(t, out, discord.CodeCancelled)
	wantNum(t, out, "parts_sent", 1)
	if ids, ok := out["message_ids"].([]any); !ok || len(ids) != 1 {
		t.Errorf("message_ids should list the one part that went out, got %v", out["message_ids"])
	}
	if !strings.Contains(out["hint"].(string), "cancel") && !strings.Contains(out["error"].(string), "cancel") {
		t.Errorf("hint should explain the cancellation: %v", out["hint"])
	}
}

// ---------------------------------------------------------------------------
// attachments
// ---------------------------------------------------------------------------

func TestAttachmentFromTheSandbox(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "pose.txt"), []byte("hello from disk"), 0o600); err != nil {
		t.Fatal(err)
	}
	d := newDeps(t, routingCfg(t, "-file-root", dir))

	out := sendOK(t, d, `{"to":"boss","files":[{"path":"pose.txt"}]}`)
	att, ok := out["attachments"].([]any)
	if !ok || len(att) != 1 {
		t.Fatalf("attachments = %v", out["attachments"])
	}
	first := att[0].(map[string]any)
	if first["name"] != "pose.txt" {
		t.Errorf("name = %v", first["name"])
	}
	if first["size"].(float64) != 15 {
		t.Errorf("size = %v, want 15 bytes", first["size"])
	}
	if _, bad := first["mime"]; bad {
		t.Errorf("the agent picked the file, it does not need its mime echoed back: %v", first)
	}
	if w, _ := out["warnings"].([]any); len(w) == 0 || !strings.Contains(w[0].(string), "blank") {
		t.Errorf("a file-only message should warn that text was blank: %v", out["warnings"])
	}
}

func TestAttachmentNameOverride(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "0001.png"), []byte("\x89PNG\r\n\x1a\n1234"), 0o600); err != nil {
		t.Fatal(err)
	}
	d := newDeps(t, routingCfg(t, "-file-root", dir))
	out := sendOK(t, d, `{"to":"boss","text":"look","files":[{"path":"0001.png","name":"pose one.png"}]}`)
	att := out["attachments"].([]any)
	if got := att[0].(map[string]any)["name"]; got != "pose one.png" {
		t.Errorf("name = %v", got)
	}
}

func TestMissingFileSaysWhereItLooked(t *testing.T) {
	dir := t.TempDir()
	d := newDeps(t, routingCfg(t, "-file-root", dir))
	out := sendErr(t, d, `{"to":"boss","files":[{"path":"nope.png"}]}`)
	wantCode(t, out, files.CodeNotFound)
	// The roots belong in the sentence the model reads, not in a second field that
	// repeats them: one host path per refusal is enough, and that one has to be
	// there or a wrong path is a mystery.
	if h, _ := out["hint"].(string); !strings.Contains(h, dir) {
		t.Errorf("hint = %q, want it to name the file root it looked under", h)
	}
	if _, present := out["file_roots"]; present {
		t.Errorf("file_roots must not be a second copy of the same path: %v", out)
	}
}

func TestPathEscapeIsRefused(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "..secret"), []byte("top secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ok.txt"), []byte("fine"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sub", "deep.txt"), []byte("deep"), 0o600); err != nil {
		t.Fatal(err)
	}
	d := newDeps(t, routingCfg(t, "-file-root", dir))

	// The name is cleaned first, so only a ".." that survives cleaning — one that
	// really leaves the root — is refused. "~" never gets expanded.
	for _, p := range []string{
		"../..secret", "..", "../sub/../..secret", "sub/../../..secret",
		"~", "~/.ssh/id_ed25519",
	} {
		out := sendErr(t, d, `{"to":"boss","files":[{"path":`+jsonString(p)+`}]}`)
		wantCode(t, out, files.CodeEscapes)
	}

	// '/' names the root, and ".." above the root of a namespace has nowhere to
	// go: it collapses, exactly as it does for POSIX absolute paths. Every spelling
	// below lands inside the root or not at all — never outside it.
	for _, p := range []string{
		"ok.txt", "/ok.txt", "./ok.txt", "//ok.txt", "ok.txt/",
		"/../ok.txt", "sub/../ok.txt", "/./sub/../ok.txt", "sub/deep.txt", "/sub/../sub/deep.txt",
	} {
		if out := sendOK(t, d, `{"to":"boss","files":[{"path":`+jsonString(p)+`}]}`); out["ok"] != true {
			t.Errorf("Open(%q) failed: %v", p, out)
		}
	}

	// A leading '/' is only cosmetic, so an absolute host path is a lookup that
	// misses — inside the sandbox, and the answer names the roots it looked under
	// (in the sentence, not in a second field) so a wrong path stays diagnosable.
	// Never a bypass, never a false alarm.
	misses := []string{"/etc/passwd", "/nope.txt"}
	if runtime.GOOS != "windows" {
		misses = append(misses, filepath.Join(dir, "..secret"))
	}
	for _, p := range misses {
		out := sendErr(t, d, `{"to":"boss","files":[{"path":`+jsonString(p)+`}]}`)
		wantCode(t, out, files.CodeNotFound)
		if h, _ := out["hint"].(string); !strings.Contains(h, dir) {
			t.Errorf("%q should come back naming the root it was looked up in (%s): %v", p, dir, out)
		}
	}
}

func TestFilesDisabledAnswer(t *testing.T) {
	d := newDeps(t, routingCfg(t, "-no-file-root"))
	out := sendErr(t, d, `{"to":"boss","files":[{"path":"pose.txt"}]}`)
	wantCode(t, out, files.CodeDisabled)
}

func TestNoPathNoURL(t *testing.T) {
	d := newDeps(t, routingCfg(t))
	out := sendErr(t, d, `{"to":"boss","files":[{}]}`)
	wantCode(t, out, files.CodeEmpty)
	out = sendErr(t, d, `{"to":"boss","files":[{"name":"x.png"}]}`)
	wantCode(t, out, files.CodeEmpty)
}

func TestTooManyFilesRefusedBeforeReadingThem(t *testing.T) {
	d := newDeps(t, routingCfg(t, "-max-files-per-msg=2"))
	out := sendErr(t, d, `{"to":"boss","text":"hi","files":[{"url":"https://a/x"},{"url":"https://b/y"},{"url":"https://c/z"}]}`)
	wantCode(t, out, files.CodeTooMany)
	wantNum(t, out, "max_files_per_msg", 2)
}

func TestPathAndURLTogetherWarns(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	d := newDeps(t, routingCfg(t, "-file-root", dir))
	out := sendOK(t, d, `{"to":"boss","text":"hi","files":[{"path":"a.txt","url":"https://example.com/ignored.png"}]}`)
	warn, ok := out["warnings"].([]any)
	if !ok {
		t.Fatalf("warnings = %v", out["warnings"])
	}
	found := false
	for _, w := range warn {
		if strings.Contains(w.(string), "both path and url") {
			found = true
		}
	}
	if !found {
		t.Errorf("the ignored link must be reported, not silently dropped: %v", warn)
	}
	if n, ok := out["url_count"]; ok && n != float64(0) {
		t.Errorf("the ignored link must not be reported as pasted: %v", n)
	}
	if _, bad := out["attachments"]; !bad {
		t.Error("the path should still have been uploaded")
	}
}

// ---------------------------------------------------------------------------
// failure envelopes
// ---------------------------------------------------------------------------

func TestSendFailureEnvelopeCarriesTheNumbers(t *testing.T) {
	d := newDeps(t, routingCfg(t))
	se := &discord.SendError{
		Code:         discord.CodeRateLimited,
		Msg:          "discord told us to slow down",
		Status:       429,
		DiscordCode:  "",
		RetryAfterMS: 4321,
		BreakerOpen:  true,
	}
	out := decode(t, d.sendFailure(se, map[string]any{"target": "boss", "parts_sent": 1}))
	wantCode(t, out, discord.CodeRateLimited)
	wantNum(t, out, "retry_after_ms", 4321)
	wantNum(t, out, "http_status", 429)
	if out["breaker_open"] != true {
		t.Error("breaker_open must be reported, otherwise the model retries into a closed door")
	}
	if out["dry_run"] != true {
		t.Error("the failure happened during a dry run and the answer should say so")
	}
	if h, _ := out["hint"].(string); !strings.Contains(h, "circuit") {
		t.Errorf("hint should match the breaker case: %q", h)
	}
}

func TestPartialDeliveryIsSpelledOut(t *testing.T) {
	se := &discord.SendError{Code: discord.CodeForbidden, Msg: "Missing Permissions"}
	notePartial(se, 3, 5)
	if !strings.Contains(se.Msg, "3 of 5 part(s) landed") || !strings.Contains(se.Msg, "continue from part 4") {
		t.Errorf("the model needs to know exactly where to resume: %q", se.Msg)
	}
	if strings.Contains(se.Msg, "resend the whole message") && !strings.Contains(se.Msg, "Do not resend") {
		t.Error("must warn against resending the whole thing")
	}
	// Nothing landed: no resume advice.
	se2 := &discord.SendError{Code: discord.CodeForbidden, Msg: "nope"}
	notePartial(se2, 0, 5)
	if se2.Msg != "nope" {
		t.Errorf("a first-part failure must not claim anything landed: %q", se2.Msg)
	}
}

func TestAdviceExistsForEveryCode(t *testing.T) {
	d := newDeps(t, toolCfg(t))
	codes := []string{
		discord.CodeRateLimited, discord.CodeCircuitOpen, discord.CodeBudgetExhausted,
		discord.CodeNotConnected, discord.CodeUnauthorized, discord.CodeForbidden,
		discord.CodeUnknownChannel, discord.CodeCannotSend, discord.CodeBadRequest,
		discord.CodeServerError, discord.CodeNetwork, discord.CodeCancelled,
		discord.CodeDMFailed, discord.CodeFileFailed, "something_unexpected",
	}
	for _, c := range codes {
		if h := d.sendAdvice(&discord.SendError{Code: c}); h == "" {
			t.Errorf("no advice for %s", c)
		}
	}
}

// Every sentence we show the model has to name the tools the way --tool-prefix
// registered them: prose that says "discord_status" while the tool answers to
// "status" sends the model after a tool it does not have.
func TestProseNamesFollowToolPrefix(t *testing.T) {
	for _, prefix := range []string{"", "discord_", "dc_"} {
		d := newDeps(t, routingCfg(t, "-tool-prefix="+prefix))
		status, poll, send := d.toolStatus(), d.toolPoll(), d.toolSend()
		if status != prefix+"status" || poll != prefix+"poll" || send != prefix+"send" {
			t.Fatalf("prefix %q produced %q/%q/%q", prefix, status, poll, send)
		}
		cases := map[string]string{
			"advice(circuit)": d.sendAdvice(&discord.SendError{Code: discord.CodeCircuitOpen}),
			"advice(no-conn)": d.sendAdvice(&discord.SendError{Code: discord.CodeNotConnected}),
			"advice(unknown)": d.sendAdvice(&discord.SendError{Code: discord.CodeUnknownChannel}),
			"advice(other)":   d.sendAdvice(&discord.SendError{Code: "something_unexpected"}),
			"hint(empty)":     d.targetHint("empty_target"),
			"send tool":       sendTool(prefix).Description + " " + sendTool(prefix).InputSchema.Properties["to"].Description,
			"poll tool":       pollTool(prefix).InputSchema.Properties["targets"].Description,
		}
		for name, text := range cases {
			if !strings.Contains(text, status) && !strings.Contains(text, poll) && !strings.Contains(text, send) {
				t.Errorf("%s (%q) names no tool of this server: %q", name, prefix, text)
			}
			if prefix == "" && (strings.Contains(text, "discord_status") ||
				strings.Contains(text, "discord_poll") || strings.Contains(text, "discord_send")) {
				t.Errorf("%s still hard-codes a discord_* name: %q", name, text)
			}
		}
	}
}

func TestEveryRejectionReasonHasAHint(t *testing.T) {
	d := newDeps(t, routingCfg(t))
	for _, r := range []string{"empty_target", "no_last_target", "unknown_target", "not_allowlisted", "foreign_agent", "write_denied"} {
		if h := d.targetHint(r); h == "" || strings.Contains(h, "cannot resolve that target") {
			t.Errorf("hint for %s should be specific, got %q", r, h)
		}
	}
	if h := d.targetHint("made_up"); !strings.Contains(h, "cannot resolve that target") {
		t.Errorf("the fallback hint is missing: %q", h)
	}
}

func TestAsSendErrorWrapsForeignErrors(t *testing.T) {
	se := asSendError(errPlain)
	if se.Code != discord.CodeAPIError || se.Msg == "" {
		t.Errorf("an unknown error must still become a coded answer: %+v", se)
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

var errPlain = errorString("gateway exploded")

type errorString string

func (e errorString) Error() string { return string(e) }

func jsonString(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(b)
}
