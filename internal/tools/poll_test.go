// poll_test.go drives discord_poll the way the transport does. No gateway is
// involved: events are pushed straight into an agent's queue, and what is under
// test is the drain contract — order, budget, filters, and the promise that an
// event is handed to exactly one caller.

package tools

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	mcp "mcptransport"

	"mcp-discord-roleplay/internal/bus"
	"mcp-discord-roleplay/internal/config"
	"mcp-discord-roleplay/internal/media"
	"mcp-discord-roleplay/internal/router"
)

// pollRoutingCfg builds a config with an arbitrary routing.json body.
func pollRoutingCfg(t *testing.T, body string, extra ...string) *config.Config {
	t.Helper()
	path := filepath.Join(t.TempDir(), "routing.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return toolCfg(t, append([]string{"-config=" + path, "-poll-min-wait=1"}, extra...)...)
}

func pollCall(t *testing.T, d *Deps, ctx context.Context, args string) *mcp.ToolResult {
	t.Helper()
	res, err := d.handlePoll(ctx, &mcp.Call{Name: "poll", Arguments: json.RawMessage(args)})
	if err != nil {
		t.Fatalf("handlePoll must answer through the result, not the protocol: %v", err)
	}
	if len(res.Content) == 0 || res.Content[0].Type != "text" {
		t.Fatalf("answer has no text part: %+v", res)
	}
	return res
}

func pollOK(t *testing.T, d *Deps, args string) map[string]any {
	t.Helper()
	res := pollCall(t, d, context.Background(), args)
	if res.IsError {
		t.Fatalf("expected success, got error answer: %s", res.Content[0].Text)
	}
	return decode(t, res)
}

func pollErr(t *testing.T, d *Deps, args string) map[string]any {
	t.Helper()
	res := pollCall(t, d, context.Background(), args)
	if !res.IsError {
		t.Fatalf("expected an isError answer, got: %s", res.Content[0].Text)
	}
	out := decode(t, res)
	if s, _ := out["error"].(string); s == "" {
		t.Errorf("a failure must carry an error code: %v", out)
	}
	if s, _ := out["hint"].(string); s == "" {
		t.Errorf("a failure must carry a hint the model can act on: %v", out)
	}
	return out
}

// pollTry is pollOK/pollErr without the fatal shortcuts: go vet forbids
// t.Fatalf from a goroutine it cannot end the test with, so concurrent tests
// collect the error and report it from the test goroutine.
func pollTry(d *Deps, ctx context.Context, args string) (map[string]any, error) {
	res, err := d.handlePoll(ctx, &mcp.Call{Name: "poll", Arguments: json.RawMessage(args)})
	if err != nil {
		return nil, err
	}
	if len(res.Content) == 0 {
		return nil, fmt.Errorf("answer has no content part")
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(res.Content[0].Text), &out); err != nil {
		return nil, fmt.Errorf("answer is not JSON (%q): %v", res.Content[0].Text, err)
	}
	if res.IsError {
		return out, fmt.Errorf("error answer: %s", res.Content[0].Text)
	}
	return out, nil
}

// evMsg builds one human message event on the "boss" channel of routingJSON.
func evMsg(id, text string) *bus.Event {
	return &bus.Event{
		Kind:        bus.KindMessage,
		Ts:          time.Now(),
		Time:        "2026-01-01T00:00:00Z",
		Target:      "channel:111222333444555666",
		ChannelID:   "111222333444555666",
		ChannelName: "rp-main",
		MessageID:   id,
		Author:      bus.Author{ID: "u1", Name: "ami", Tag: "ami#0001"},
		Text:        text,
	}
}

// push puts events into an agent's queue and fails if that agent has none.
func push(t *testing.T, d *Deps, agent string, evs ...*bus.Event) {
	t.Helper()
	q, ok := d.Hub.Queue(agent)
	if !ok {
		t.Fatalf("agent %q has no queue", agent)
	}
	for _, e := range evs {
		q.Push(e)
	}
}

func msgs(t *testing.T, out map[string]any) []any {
	t.Helper()
	list, ok := out["messages"].([]any)
	if !ok {
		t.Fatalf("no messages array in %v", out)
	}
	return list
}

func firstMsg(t *testing.T, out map[string]any) map[string]any {
	t.Helper()
	list := msgs(t, out)
	m, ok := list[0].(map[string]any)
	if !ok {
		t.Fatalf("messages[0] is not an object: %T", list[0])
	}
	return m
}

// wireText reads the text of one answer message. Tests tell messages apart by
// what they said, because the internal id is no longer on the wire.
func wireText(t *testing.T, out map[string]any, i int) string {
	t.Helper()
	list := msgs(t, out)
	if i >= len(list) {
		t.Fatalf("only %d message(s) in the answer", len(list))
	}
	m, ok := list[i].(map[string]any)
	if !ok {
		t.Fatalf("messages[%d] is not an object: %T", i, list[i])
	}
	s, _ := m["text"].(string)
	return s
}

func queueLen(t *testing.T, d *Deps, agent string) int {
	t.Helper()
	q, ok := d.Hub.Queue(agent)
	if !ok {
		t.Fatalf("agent %q has no queue", agent)
	}
	return q.Stats().Size
}

// connected tells the handler the gateway is up. The fixture's client is never
// started, so without this every quiet poll would answer "disconnected" — which
// is correct behaviour and exactly what TestPollDisconnectedIsNotTimeout checks.
func connected(d *Deps) {
	d.up = func() bool { return true }
}

// withWait adds a short max_wait to an argument object without breaking the
// empty one.
func withWait(args string) string {
	if args == "{}" {
		return `{"max_wait":1}`
	}
	return strings.TrimSuffix(args, "}") + `,"max_wait":1}`
}

// ---------------------------------------------------------------------------
// the drain contract
// ---------------------------------------------------------------------------

// A queued message must come back on the first poll, without waiting a tick.
func TestPollReturnsImmediately(t *testing.T) {
	d := newDeps(t, routingCfg(t))
	push(t, d, "default", evMsg("m1", "hi"))

	start := time.Now()
	out := pollOK(t, d, `{"max_wait":5}`)
	if el := time.Since(start); el > time.Second {
		t.Errorf("took %v for a message that was already queued", el)
	}
	wantStr(t, out, "reason", "messages")
	if list := msgs(t, out); len(list) != 1 {
		t.Fatalf("one queued message, one answer: %d", len(list))
	}
	wantStr(t, firstMsg(t, out), "text", "hi")
	if queueLen(t, d, "default") != 0 {
		t.Error("taking a message out IS the ack; it must not stay queued")
	}
}

// An empty answer is the common case on a quiet server, so it stays tiny and is
// NOT an error: the agent should poll again, not give up.
func TestPollTimeoutIsSuccessAndTiny(t *testing.T) {
	d := newDeps(t, routingCfg(t, "-poll-min-wait=1", "-poll-default-wait=1", "-poll-jitter-pct=0"))
	connected(d)
	start := time.Now()
	out := pollOK(t, d, `{}`)
	if el := time.Since(start); el < 900*time.Millisecond {
		t.Errorf("returned after %v, should have waited the full second", el)
	}
	wantStr(t, out, "reason", "timeout")
	if len(out) != 1 {
		t.Errorf("an empty answer must stay small, got %d keys: %v", len(out), out)
	}
	if _, dead := out["ok"]; dead {
		t.Error("poll must not repeat the tool result's isError as ok")
	}
	if _, bad := out["messages"]; bad {
		t.Error("no messages, no messages key")
	}
	if _, bad := out["remaining"]; bad {
		t.Error("nothing is left in the queue, so remaining must be absent")
	}
}

// The same empty moment on a deaf socket is a different answer: an agent that
// keeps polling a dead gateway burns its context on "timeout" and never learns
// the bot went away.
func TestPollDisconnectedIsNotTimeout(t *testing.T) {
	d := newDeps(t, routingCfg(t, "-poll-min-wait=1", "-poll-default-wait=1", "-poll-jitter-pct=0"))
	out := pollOK(t, d, `{}`) // the fixture's client was never started
	wantStr(t, out, "reason", "disconnected")
	if len(out) != 1 {
		t.Errorf("still one key, just a different word: %v", out)
	}
}

// A message arriving while a poll is blocked must wake it.
func TestPollWakesOnPush(t *testing.T) {
	d := newDeps(t, routingCfg(t))
	done := make(chan map[string]any, 1)
	failed := make(chan error, 1)
	go func() {
		out, err := pollTry(d, context.Background(), `{"max_wait":10}`)
		if err != nil {
			failed <- err
			return
		}
		done <- out
	}()
	time.Sleep(80 * time.Millisecond)
	push(t, d, "default", evMsg("m9", "late"))

	select {
	case out := <-done:
		wantStr(t, out, "reason", "messages")
		wantStr(t, firstMsg(t, out), "text", "late")
	case err := <-failed:
		t.Fatalf("the blocked poll failed: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("a push did not wake the blocked poll")
	}
}

// Order is queue order, and the leftovers stay for the next call.
func TestPollKeepsOrderAndStopsAtLimit(t *testing.T) {
	d := newDeps(t, routingCfg(t))
	push(t, d, "default", evMsg("m1", "a"), evMsg("m2", "b"), evMsg("m3", "c"))

	out := pollOK(t, d, `{"max_wait":1,"limit":2}`)
	list := msgs(t, out)
	if len(list) != 2 {
		t.Fatalf("want 2 messages, got %d", len(list))
	}
	if list[0].(map[string]any)["text"] != "a" || list[1].(map[string]any)["text"] != "b" {
		t.Errorf("order must be oldest first, got %v", list)
	}
	// remaining is the "poll me again" signal: a caller that can count what is
	// left does not also need a partial flag.
	wantNum(t, out, "remaining", 1)
	if notes, _ := out["notes"].([]any); len(notes) == 0 {
		t.Error("the cut must be explained in notes")
	}

	out = pollOK(t, d, `{"max_wait":1}`)
	if list := msgs(t, out); len(list) != 1 || list[0].(map[string]any)["text"] != "c" {
		t.Errorf("second poll should get exactly m3, got %v", list)
	}
}

// One event may be delivered to exactly one caller, even when two polls race.
func TestPollConcurrentNoDuplicates(t *testing.T) {
	d := newDeps(t, routingCfg(t))
	const n = 40
	evs := make([]*bus.Event, 0, n)
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("id%d", i)
		evs = append(evs, evMsg(id, id))
	}
	push(t, d, "default", evs...)

	var mu sync.Mutex
	seen := map[string]bool{}
	dup := 0
	var probs []string
	var wg sync.WaitGroup
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for r := 0; r < 3; r++ {
				out, err := pollTry(d, context.Background(), `{"max_wait":1,"limit":5}`)
				if err != nil {
					mu.Lock()
					probs = append(probs, err.Error())
					mu.Unlock()
					return
				}
				list, _ := out["messages"].([]any)
				for _, raw := range list {
					id, _ := raw.(map[string]any)["text"].(string)
					mu.Lock()
					if seen[id] {
						dup++
					}
					seen[id] = true
					mu.Unlock()
				}
			}
		}()
	}
	wg.Wait()
	for _, p := range probs {
		t.Error(p)
	}
	if dup != 0 {
		t.Fatalf("%d message(s) were delivered twice", dup)
	}
	if got := len(seen); got != n {
		t.Errorf("delivered %d of %d queued messages", got, n)
	}
}

// ---------------------------------------------------------------------------
// byte budget
// ---------------------------------------------------------------------------

func TestPollStopsAtByteBudget(t *testing.T) {
	d := newDeps(t, routingCfg(t, "-poll-max-bytes=16384"))
	big := strings.Repeat("x", 5000)
	push(t, d, "default",
		evMsg("m1", "1"+big), evMsg("m2", "2"+big), evMsg("m3", "3"+big),
		evMsg("m4", "4"+big), evMsg("m5", "5"+big))

	res := pollCall(t, d, context.Background(), `{"max_wait":1,"limit":20}`)
	out := decode(t, res)
	if n := len(msgs(t, out)); n != 3 {
		t.Fatalf("16384/5000 should fit 3 messages, got %d", n)
	}
	wantNum(t, out, "remaining", 2)
	// The budget counts text only, so the JSON answer drifts past it by the fixed
	// per-message overhead (keys, author, timestamps). That drift is the point:
	// nobody has to guess what a message's envelope costs to encode.
	if b := len(res.Content[0].Text); b > 16384+2048 {
		t.Errorf("answer is %d bytes, far past the 16384 text budget", b)
	}
	// The leftovers come back in order on the next poll.
	out = pollOK(t, d, `{"max_wait":1}`)
	list := msgs(t, out)
	if len(list) != 2 || !strings.HasPrefix(wireText(t, out, 0), "4") {
		t.Errorf("leftovers must keep their order, got %d message(s)", len(list))
	}
}

// A message whose text is bigger than the whole budget must not wedge the queue
// head: the text is cut to the budget on a rune boundary, and the model is told
// how big it really was.
func TestPollTrimsOversizedMessage(t *testing.T) {
	d := newDeps(t, routingCfg(t, "-poll-max-bytes=8192"))
	text := strings.Repeat("漢", 4000) // 12000 bytes of 3-byte runes
	push(t, d, "default", evMsg("m1", text))

	res := pollCall(t, d, context.Background(), `{"max_wait":1}`)
	out := decode(t, res)
	m := firstMsg(t, out)
	got, _ := m["text"].(string)
	if !utf8.ValidString(got) {
		t.Error("truncation split a UTF-8 rune")
	}
	if len(got) > 8192 {
		t.Fatalf("text was not cut into the budget: %d bytes", len(got))
	}
	if len(got) != 8190 {
		t.Errorf("the cut should land as close to the budget as a rune boundary allows: %d bytes", len(got))
	}
	if m["text_truncated"] != true {
		t.Error("a trimmed text must say so")
	}
	// How big it really was is a note now: the model needs to know the line was
	// cut, not the exact byte count of a message it has already read.
	if !hasNote(out, fmt.Sprintf("原本 %d bytes", len(text))) {
		t.Errorf("the note must report the real size: %v", out["notes"])
	}
	if b := len(res.Content[0].Text); b > 8192+2048 {
		t.Errorf("answer is %d bytes, far past the 8192 budget", b)
	}
	if queueLen(t, d, "default") != 0 {
		t.Error("a trimmed message is still consumed, or it would be re-trimmed forever")
	}
}

// Uncapped means uncapped: a -1 budget never cuts.
func TestPollBudgetMinusOneIsUncapped(t *testing.T) {
	d := newDeps(t, routingCfg(t, "-poll-max-bytes=-1"))
	push(t, d, "default", evMsg("m1", strings.Repeat("y", 30000)))
	out := pollOK(t, d, `{"max_wait":1}`)
	if _, bad := out["remaining"]; bad {
		t.Error("no budget means nothing was left behind, so remaining must be absent")
	}
	if m := firstMsg(t, out); m["text_truncated"] == true {
		t.Error("no budget, no trimming")
	}
}

// The byte budget only negotiates text: an attachment is never dropped to make a
// message fit, not even when the text beside it has to be cut. That is the old
// empty-shell bug nailed shut — the previous order of operations trimmed the text
// to zero, threw the attachments out with the room it had just freed, and shipped
// a message with neither.
func TestPollBudgetNeverDropsAttachments(t *testing.T) {
	d := newDeps(t, routingCfg(t, "-poll-max-bytes=200"))
	e := evMsg("m1", strings.Repeat("x", 400))
	for i := 0; i < 20; i++ {
		e.Attachments = append(e.Attachments, bus.Attachment{
			ID: "a", Filename: "shot.png", Kind: bus.KindImage,
			ContentType: "image/png", Size: 1234,
			URL: "https://cdn.discordapp.com/attachments/1/2/shot.png",
		})
	}
	push(t, d, "default", e)

	out := pollOK(t, d, `{"max_wait":1}`)
	m := firstMsg(t, out)
	list, _ := m["attachments"].([]any)
	if len(list) != 20 {
		t.Fatalf("a text budget must never drop attachments, got %d of 20 listed", len(list))
	}
	if txt, _ := m["text"].(string); len(txt) != 200 {
		t.Errorf("the text should be cut to exactly the budget, got %d bytes", len(txt))
	}
	if m["text_truncated"] != true {
		t.Error("a cut text must say so")
	}
	if _, bad := m["attachments_omitted"]; bad {
		t.Error("attachments_omitted is gone from the wire: nothing omits attachments any more")
	}
	first := list[0].(map[string]any)
	if s, _ := first["url"].(string); !strings.HasPrefix(s, "https://") {
		t.Errorf("a kept attachment must still carry its url, got %v", first["url"])
	}
	if s, _ := first["media"].(string); s != "url" {
		t.Errorf("with no fetcher wired up, every attachment is metadata only (media:url), got %q", s)
	}
	if _, ok := first["policy"]; ok {
		t.Errorf("policy is not on the wire: the agent is told what it GOT, not what was configured: %v", first["policy"])
	}
}

// The answer is a wire struct, not the working struct: a key that creeps back
// onto the wire is context the model pays for twice, so this is the contract
// test -- the key set is spelled out, and any change must be argued here first.
func TestPollWireKeysAreExactlyThese(t *testing.T) {
	d := newDeps(t, routingCfg(t))
	connected(d)
	e := evMsg("m1", "hello")
	e.Ts = time.Now().Add(-90 * time.Second) // a real age, so age_s is on the wire
	e.Embeds = []bus.Embed{{Type: "rich", Title: "t"}}
	e.Attachments = []bus.Attachment{{ID: "a1", Filename: "shot.png", Kind: bus.KindImage,
		ContentType: "image/png", Size: 10, URL: "https://cdn.discordapp.com/attachments/1/2/shot.png"}}
	push(t, d, "default", e)

	out := pollOK(t, d, `{"max_wait":1}`)
	if got := keySet(out); got != "messages,reason" {
		t.Errorf("envelope keys = %s, want exactly the two that change what the agent does", got)
	}

	m := firstMsg(t, out)
	want := "age_s,attachments,author,author_id,embeds,target,text,ts"
	if got := keySet(m); got != want {
		t.Errorf("message keys =\n  %s\nwant\n  %s", got, want)
	}

	att := firstAttachment(t, out)
	wantAtt := "kind,media,name,url"
	if got := keySet(att); got != wantAtt {
		t.Errorf("attachment keys =\n  %s\nwant\n  %s", got, wantAtt)
	}

	emb := m["embeds"].([]any)[0].(map[string]any)
	if got := keySet(emb); got != "title" {
		t.Errorf("embed keys = %s (only the ones present may appear)", got)
	}
	// The one id that earns its bytes: to:"dm:<id>" is built from it.
	wantStr(t, m, "author_id", "u1")
}

// A placeholder author id must not reach the wire: quoting it back as
// to:"dm:?" would send the reply nowhere useful.
func TestPollOmitsPlaceholderAuthorID(t *testing.T) {
	d := newDeps(t, routingCfg(t))
	connected(d)
	e := evMsg("m1", "hi")
	e.Author = bus.Author{ID: "?", Name: "?"}
	push(t, d, "default", e)

	out := pollOK(t, d, `{"max_wait":1}`)
	m := firstMsg(t, out)
	if _, ok := m["author_id"]; ok {
		t.Errorf(`author_id should be omitted for the "?" placeholder, got %v`, m["author_id"])
	}
	wantStr(t, m, "author", "?")
}

// keySet sorts an answer object's keys into one comparable string.
func keySet(m map[string]any) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return strings.Join(keys, ",")
}

// ---------------------------------------------------------------------------
// filters
// ---------------------------------------------------------------------------

func TestPollFilters(t *testing.T) {
	dm := &bus.Event{
		Kind: bus.KindMessage, Ts: time.Now(), Time: "2026-01-01T00:00:00Z",
		Target: "dm:222333444555666777", ChannelID: "999000111222333444", DM: true,
		MessageID: "d1", Author: bus.Author{ID: "u2", Name: "boss", Tag: "boss#0002"},
		Text: "in dm",
	}
	mention := evMsg("m2", "hey <@777>")
	mention.MentionsSelf = true
	bot := evMsg("m3", "bot noise")
	bot.Author = bus.Author{ID: "u3", Name: "webhook", Bot: true}
	// m4 is what our own message looks like if something pushes one into a queue by
	// hand. Production no longer does it (the gateway client drops a self echo
	// before it fans out), so this event's only job is to prove poll has no self
	// gate of its own any more: a queued self event is just a bot, and that is the
	// whole of it.
	own := evMsg("m4", "I talk to myself")
	own.Author = bus.Author{ID: "u9", Name: "selfbot", Bot: true, Self: true}

	// The answer carries no message ids any more, so the table below keeps naming
	// them for readability while the assertion resolves an id to the text that
	// message said.
	textOf := map[string]string{
		"m1": "hello", "m2": "hey <@777>", "m3": "bot noise",
		"m4": "I talk to myself", "d1": "in dm",
	}

	cases := []struct {
		why  string
		args string
		want []string // message ids, in order
	}{
		{"no filter", `{}`, []string{"m1", "m2", "d1"}},
		{"by target", `{"targets":["ami"]}`, []string{"d1"}},
		{"by canonical target", `{"targets":["channel:111222333444555666"]}`, []string{"m1", "m2"}},
		{"two targets", `{"targets":["boss","ami"]}`, []string{"m1", "m2", "d1"}},
		{"by channel name", `{"channels":["#rp-main"]}`, []string{"m1", "m2"}},
		{"by channel id", `{"channels":["999000111222333444"]}`, []string{"d1"}},
		{"by author name", `{"from":["boss"]}`, []string{"d1"}},
		{"by author id", `{"from":["u1"]}`, []string{"m1", "m2"}},
		{"mentions only", `{"mentions_only":true}`, []string{"m2"}},
		{"bots skipped by default", `{"limit":10}`, []string{"m1", "m2", "d1"}},
		{"bots allowed", `{"ignore_bots":false,"limit":10}`, []string{"m1", "m2", "m3", "m4", "d1"}},
		{"nothing matches", `{"channels":["nope"]}`, nil},
	}

	for _, c := range cases {
		t.Run(c.why, func(t *testing.T) {
			d := newDeps(t, routingCfg(t))
			connected(d)
			push(t, d, "default", evMsg("m1", "hello"), mention, bot, own, dm)
			out := pollOK(t, d, withWait(c.args))
			if len(c.want) == 0 {
				if out["reason"] != "timeout" {
					t.Fatalf("want timeout, got %v", out["reason"])
				}
				if queueLen(t, d, "default") != 5 {
					t.Error("a filtered-out message must stay queued")
				}
				// An empty answer also has to say why it is empty, and the tally is
				// per gate: m3/m4 are bots (checked first), the other three are the
				// wrong channel. Busiest gate first, ties by name.
				if out["waiting"] != "3 channel, 2 bot" {
					t.Errorf(`empty answer waiting = %v, want "3 channel, 2 bot"`, out["waiting"])
				}
				return
			}
			list := msgs(t, out)
			if len(list) != len(c.want) {
				t.Fatalf("got %d messages, want %d: %v", len(list), len(c.want), list)
			}
			for i, id := range c.want {
				if got := wireText(t, out, i); got != textOf[id] {
					t.Errorf("messages[%d] = %q, want the one called %s", i, got, id)
				}
			}
		})
	}
}

// A target the operator marked read:false may not be polled, and that is a hard
// refusal rather than an empty answer (which would look like "nothing arrived").
func TestPollRefusesReadOnlyTarget(t *testing.T) {
	cfg := pollRoutingCfg(t, `{
	  "agents": [{"id": "default", "targets": ["feed"]}],
	  "targets": [{"alias": "feed", "id": "channel:111222333444555666", "read": false}]
	}`)
	d := newDeps(t, cfg)
	out := pollErr(t, d, `{"targets":["feed"],"max_wait":1}`)
	wantCode(t, out, "bad_target")
}

// ---------------------------------------------------------------------------
// identity, lifecycle
// ---------------------------------------------------------------------------

func TestPollUnknownAgent(t *testing.T) {
	d := newDeps(t, routingCfg(t))
	push(t, d, "default", evMsg("m1", "hi"))
	ctx := router.WithPrincipal(context.Background(), router.Principal{AgentID: "ghost"})
	out := decode(t, pollCall(t, d, ctx, `{"max_wait":1}`))
	wantCode(t, out, "unknown_agent")
	list, ok := out["agents"].([]any)
	if !ok || len(list) == 0 {
		t.Errorf("the answer must name the agents that do have queues, got %v", out["agents"])
	}
	// and the queued message is still there for its real owner
	if queueLen(t, d, "default") != 1 {
		t.Error("a poll by an agent with no queue must not consume anyone's messages")
	}
}

// Two personas on one server must not read each other's messages.
func TestPollQueuesAreIsolated(t *testing.T) {
	d := newDeps(t, routingCfg(t))
	push(t, d, "default", evMsg("m1", "for default"))
	push(t, d, "solo", evMsg("m2", "for solo"))

	ctx := router.WithPrincipal(context.Background(), router.Principal{AgentID: "solo"})
	out := decode(t, pollCall(t, d, ctx, `{"max_wait":1}`))
	if list := msgs(t, out); len(list) != 1 || list[0].(map[string]any)["text"] != "for solo" {
		t.Errorf("solo got %v", list)
	}
	if queueLen(t, d, "default") != 1 {
		t.Error("default's queue must be untouched by solo's poll")
	}
}

// A polled message must carry the alias its caller answers with, not a raw
// snowflake, and an alias belonging to another agent must never leak: the same
// event pointer sits in both queues, which is exactly why the label is computed
// per call into pollMessage and never written back onto bus.Event.
func TestPollLabelsTargetWithCallerAlias(t *testing.T) {
	cfg := pollRoutingCfg(t, `{
	  "agents": [
	    {"id": "default", "targets": ["boss"]},
	    {"id": "solo",    "targets": ["side"]}
	  ],
	  "targets": [
	    {"alias": "boss", "id": "channel:111222333444555666", "name": "rp-main"},
	    {"alias": "side", "id": "channel:999888777666555444", "name": "rp-side"}
	  ]
	}`)
	d := newDeps(t, cfg)

	mine := evMsg("m1", "in my channel")
	theirs := evMsg("m2", "in the other one")
	theirs.ChannelID = "999888777666555444"
	theirs.ChannelName = "rp-side"
	theirs.Target = "channel:999888777666555444"

	// Both queues get the very same pointers, the way Hub.Fanout does it.
	push(t, d, "default", mine, theirs)
	push(t, d, "solo", mine, theirs)

	targetsOf := func(out map[string]any) string {
		list := msgs(t, out)
		got := make([]string, 0, len(list))
		for _, m := range list {
			mm, _ := m.(map[string]any)
			s, _ := mm["target"].(string)
			got = append(got, s)
		}
		return strings.Join(got, ",")
	}

	if got := targetsOf(pollOK(t, d, `{"max_wait":1}`)); got != "boss,channel:999888777666555444" {
		t.Errorf("default sees its own alias and a bare id for the foreign channel: %q", got)
	}

	solo := router.WithPrincipal(context.Background(), router.Principal{AgentID: "solo"})
	out, err := pollTry(d, solo, `{"max_wait":1}`)
	if err != nil {
		t.Fatalf("solo poll: %v", err)
	}
	if got := targetsOf(out); got != "channel:111222333444555666,side" {
		t.Errorf("solo sees the mirror image, must not learn 'boss': %q", got)
	}
}

func TestPollBadMaxWait(t *testing.T) {
	d := newDeps(t, routingCfg(t, "-poll-min-wait=5", "-poll-max-wait=60"))
	for _, v := range []string{"0", "1", "4.9", "61", "3600", "-5"} {
		out := pollErr(t, d, `{"max_wait":`+v+`}`)
		wantCode(t, out, "bad_max_wait")
		wantNum(t, out, "min", 5)
		wantNum(t, out, "max", 60)
	}
}

// A client that goes away mid-render must not swallow the batch: the events go
// back to the front, in order, for the next poll.
func TestPollRequeuesAfterCancel(t *testing.T) {
	d := newDeps(t, routingCfg(t))
	push(t, d, "default", evMsg("m1", "a"), evMsg("m2", "b"))

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the caller is already gone when the drain finishes
	out := decode(t, pollCall(t, d, ctx, `{"max_wait":1}`))
	wantStr(t, out, "reason", "cancelled")
	if _, bad := out["messages"]; bad {
		t.Error("a cancelled answer delivers no messages")
	}
	if queueLen(t, d, "default") != 2 {
		t.Fatalf("the batch must be back in the queue, size = %d", queueLen(t, d, "default"))
	}

	out = pollOK(t, d, `{"max_wait":1}`)
	list := msgs(t, out)
	if len(list) != 2 || list[0].(map[string]any)["text"] != "a" || list[1].(map[string]any)["text"] != "b" {
		t.Errorf("order after requeue must be preserved, got %v", list)
	}
}

func TestPollShutdown(t *testing.T) {
	d := newDeps(t, routingCfg(t))
	ch := make(chan struct{})
	d.Stopping = ch
	close(ch)
	out := pollOK(t, d, `{"max_wait":10}`)
	wantStr(t, out, "reason", "shutdown")
}

// A lifecycle notice is not a message. They no longer enter a queue at all
// (Hub.WakeAll only wakes the waiters), so what this pins is the defensive branch
// in the drain: if one ever does get stored, it is taken and discarded, never
// handed to a caller, and it leaves no phantom `remaining` behind.
func TestPollConsumesSystemEvents(t *testing.T) {
	d := newDeps(t, routingCfg(t, "-poll-jitter-pct=0"))
	connected(d)
	push(t, d, "default", &bus.Event{Kind: bus.KindSystem, Time: "2026-01-01T00:00:00Z", Text: "gateway reconnected"})
	out := pollOK(t, d, `{"max_wait":1}`)
	wantStr(t, out, "reason", "timeout")
	if queueLen(t, d, "default") != 0 {
		t.Error("the notice should have been consumed and discarded")
	}
}

// Events overwritten while nobody polled must be visible in the answer.
func TestPollReportsDrops(t *testing.T) {
	// --bus-capacity has a floor of 10 (enforced in internal/config and asserted
	// there), so the overflow is demonstrated with 12 events in a queue of 10.
	d := newDeps(t, routingCfg(t, "-bus-capacity=10", "-poll-jitter-pct=0"))
	evs := make([]*bus.Event, 0, 12)
	for i := 1; i <= 12; i++ {
		id := fmt.Sprintf("m%d", i)
		evs = append(evs, evMsg(id, id))
	}
	push(t, d, "default", evs...)
	out := pollOK(t, d, `{"max_wait":1,"limit":20}`)
	wantNum(t, out, "dropped", 2)
	if list := msgs(t, out); len(list) != 10 || list[0].(map[string]any)["text"] != "m3" {
		t.Errorf("the oldest events are the ones overwritten, got %v", list)
	}

	// The counter is cumulative, so a healthy agent must be told nothing: only a
	// drop that actually happened is reported.
	fresh := newDeps(t, routingCfg(t, "-poll-jitter-pct=0"))
	push(t, fresh, "default", evMsg("m4", "d"))
	out = pollOK(t, fresh, `{"max_wait":1}`)
	if _, bad := out["dropped"]; bad {
		t.Error("dropped must be omitted when nothing was dropped")
	}
}

// An empty answer used to be one fact. It is now two, and they call for
// different actions -- nothing happened, versus something did and this call's own
// filter refused it -- so the difference has to be legible without a second poll.
func TestPollEmptyAnswerNamesWhatIsWaiting(t *testing.T) {
	d := newDeps(t, routingCfg(t, "-poll-jitter-pct=0"))
	connected(d)
	e := evMsg("m1", "hello")
	e.Ts = time.Now().Add(-3 * time.Second) // it has been waiting a while
	push(t, d, "default", e)

	// This call asked for the DM conversation; the message is from a guild channel.
	out := pollOK(t, d, `{"targets":["ami"],"max_wait":1}`)
	wantStr(t, out, "reason", "timeout")
	if got := keySet(out); got != "oldest_s,reason,remaining,waiting" {
		t.Errorf("keys = %s, want the reason plus what is still queued", got)
	}
	wantNum(t, out, "remaining", 1)
	wantStr(t, out, "waiting", "1 target")
	if v, ok := out["oldest_s"].(float64); !ok || v < 3 {
		t.Errorf("oldest_s = %v, want the ~3 seconds this event has waited", out["oldest_s"])
	}

	// Ask without the filter and the very same message is deliverable: the queue
	// drains, and the leftovers vocabulary disappears again.
	out = pollOK(t, d, `{"max_wait":1}`)
	wantStr(t, out, "reason", "messages")
	if _, bad := out["waiting"]; bad {
		t.Error("waiting describes what a poll could not deliver, not a full batch")
	}
	if _, bad := out["remaining"]; bad {
		t.Error("the queue is empty now, so remaining must be absent")
	}
}

// ---------------------------------------------------------------------------
// small helpers
// ---------------------------------------------------------------------------

// Jitter may only ever make the wait later (never shorter than asked), and must
// stay inside its declared percentage.
func TestJitterOnlyExtends(t *testing.T) {
	base := time.Second
	span := time.Duration(float64(base) * 0.15)
	for i := 0; i < 200; i++ {
		j := jitter(base, 15)
		if j < 0 || j > span {
			t.Fatalf("jitter %v outside [0,%v]", j, span)
		}
	}
	if jitter(base, 0) != 0 || jitter(0, 15) != 0 {
		t.Error("jitter must be 0 when disabled or when there is nothing to spread")
	}
}

// The schema has to describe the bounds the handler actually enforces, so a
// model cannot guess itself out of bad_max_wait.
func TestPollToolSchema(t *testing.T) {
	tool := pollTool("discord_")
	if tool.Name != "poll" {
		t.Fatalf("name = %q", tool.Name)
	}
	if tool.InputSchema.Type != mcp.TypeObject {
		t.Fatal("input schema must be an object")
	}
	for _, key := range []string{"max_wait", "limit", "targets", "channels", "from",
		"mentions_only", "ignore_bots"} {
		if _, ok := tool.InputSchema.Properties[key]; !ok {
			t.Errorf("missing property %q", key)
		}
	}
	// Our own messages are dropped before they are queued, so there is nothing for
	// a poll argument to ask for: bringing include_self back would mean deciding
	// again that the model should be shown its own words.
	if _, dead := tool.InputSchema.Properties["include_self"]; dead {
		t.Error("include_self is gone from the schema")
	}
	if len(tool.InputSchema.Required) != 0 {
		t.Errorf("every poll argument is optional, got %v", tool.InputSchema.Required)
	}
	if got := tool.InputSchema.Properties["targets"].Items; got == nil || got.Type != mcp.TypeString {
		t.Error("targets must be an array of strings")
	}
	if d := tool.InputSchema.Properties["max_wait"].Description; !strings.Contains(d, "poll-min-wait") {
		t.Error("max_wait must document where its bounds come from")
	}
	// The declared name stays bare (the registry adds the prefix), but prose that
	// points at ANOTHER tool must already carry it, or the model is sent looking
	// for a tool that does not exist under that name.
	if d := tool.InputSchema.Properties["targets"].Description; !strings.Contains(d, "discord_status") {
		t.Errorf("targets must point at the status tool by its registered name, got %q", d)
	}
}

// --- media: the second phase (M2b) -----------------------------------------
//
// These are the tests for the wiring, not for the fetcher: the fetcher decides
// per attachment, the poll handler decides where the bytes go in the answer.
// The contract being pinned here is that a model reading only the JSON can
// find the picture it was promised, and that the promise survives the
// budget-cutter.

// startCDN is the smallest possible Discord CDN: one payload at every path and
// a counter, so a test can tell "we looked and decided" from "we fetched".
func startCDN(t *testing.T, body []byte) (string, *atomic.Int64) {
	t.Helper()
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv.URL, &hits
}

// firstAttachment digs out the first attachment of the first message.
func firstAttachment(t *testing.T, out map[string]any) map[string]any {
	t.Helper()
	msgs, _ := out["messages"].([]any)
	if len(msgs) == 0 {
		t.Fatal("no messages to look at")
	}
	list, _ := msgs[0].(map[string]any)["attachments"].([]any)
	if len(list) == 0 {
		t.Fatal("no attachments to look at")
	}
	return list[0].(map[string]any)
}

// hasNote reports whether any note contains the text. Notes are prose, so a
// test wants a fragment, not a whole sentence it would then never update.
func hasNote(out map[string]any, want string) bool {
	notes, _ := out["notes"].([]any)
	for _, n := range notes {
		if s, ok := n.(string); ok && strings.Contains(s, want) {
			return true
		}
	}
	return false
}

// An inline image is not in the JSON: the answer text is content[0] and the
// picture is content[1], which is what content_index points at.
func TestPollInlinesImageIntoContentBlocks(t *testing.T) {
	body := []byte{0x89, 'P', 'N', 'G', 1, 2}
	url, hits := startCDN(t, body)
	cfg := routingCfg(t, "-media-image=inline")
	d := newDeps(t, cfg)
	d.Media = media.New(cfg, media.Deps{})
	t.Cleanup(func() { _ = d.Media.Close() })

	e := evMsg("m1", "看這張")
	e.Attachments = []bus.Attachment{{ID: "a1", Filename: "shot.png", Kind: bus.KindImage,
		ContentType: "image/png", Size: len(body), URL: url + "/shot.png"}}
	push(t, d, "default", e)

	res := pollCall(t, d, context.Background(), `{"max_wait":1}`)
	out := decode(t, res)
	att := firstAttachment(t, out)

	if att["media"] != "base64" {
		t.Fatalf("an inlined image must say base64, got %v (reason %v)", att["media"], att["reason"])
	}
	if _, ok := att["reason"]; ok {
		t.Errorf("base64 carries no reason: %v", att["reason"])
	}
	if _, ok := att["policy"]; ok {
		t.Errorf("policy is not on the wire: the answer is what it GOT: %v", att["policy"])
	}
	if ci, _ := att["content_index"].(float64); ci != 1 {
		t.Errorf("content[0] is the JSON answer itself, so the first block is index 1, got %v", att["content_index"])
	}
	if len(res.Content) != 2 {
		t.Fatalf("want [text, image], got %d parts", len(res.Content))
	}
	if res.Content[0].Text == "" || res.Content[0].Type != "text" {
		t.Fatalf("content[0] must stay the JSON answer: %+v", res.Content[0])
	}
	if res.Content[1].Type != "image" {
		t.Errorf("a KindImage must arrive as an image block, got %q", res.Content[1].Type)
	}
	if res.Content[1].Data != base64.StdEncoding.EncodeToString(body) {
		t.Errorf("the block must carry the bytes, got %d chars of base64", len(res.Content[1].Data))
	}
	if _, bad := att["bytes"]; bad {
		t.Errorf("the block carries its own size; a byte count on the wire is a duplicate: %v", att)
	}
	if hits.Load() != 1 {
		t.Errorf("one attachment, one fetch: %d", hits.Load())
	}
	// The base64 must not be in the JSON either, or result_bytes would be a
	// lie and the byte budget would be paying for it twice.
	if strings.Contains(res.Content[0].Text, base64.StdEncoding.EncodeToString(body)) {
		t.Error("base64 belongs in the content block, not in the JSON answer")
	}
}

// A saved attachment costs the answer nothing but a path, that path must be a
// file that exists, and the same attachment must not be fetched twice.
func TestPollSavesAttachmentAndReportsLocalPath(t *testing.T) {
	dir := t.TempDir()
	body := []byte("saved, not inlined")
	url, hits := startCDN(t, body)
	cfg := routingCfg(t, "-media-image=save", "-media-out="+dir, "-local-path=/dc-cache")
	d := newDeps(t, cfg)
	d.Media = media.New(cfg, media.Deps{})
	t.Cleanup(func() { _ = d.Media.Close() })

	e := evMsg("m1", "")
	e.Attachments = []bus.Attachment{{ID: "a1", Filename: "shot.png", Kind: bus.KindImage,
		ContentType: "image/png", Size: len(body), URL: url + "/shot.png"}}
	push(t, d, "default", e)

	out := pollOK(t, d, `{"max_wait":1}`)
	att := firstAttachment(t, out)
	if att["media"] != "file" {
		t.Fatalf("want media=file, got %v/%v", att["media"], att["reason"])
	}
	lp, _ := att["local_path"].(string)
	if !strings.HasPrefix(lp, "/dc-cache/") {
		t.Fatalf("local_path must carry the declared prefix: %q", lp)
	}
	if _, ok := att["content_index"]; ok {
		t.Errorf("a saved file has no content block, so no content_index: %v", att["content_index"])
	}
	raw, err := os.ReadFile(filepath.Join(dir, filepath.Base(lp)))
	if err != nil {
		t.Fatalf("local_path must name a file that exists: %v", err)
	}
	if string(raw) != string(body) {
		t.Errorf("wrong bytes on disk: %q", raw)
	}
	if !hasNote(out, "local_path") {
		t.Errorf("a save must be explained, because the agent has to know the path is readable: %v", out["notes"])
	}

	// Same attachment again — a second agent, or a requeue: the file is reused,
	// and the bytes are never fetched a second time in this process.
	push(t, d, "default", e)
	out2 := pollOK(t, d, `{"max_wait":1}`)
	if firstAttachment(t, out2)["media"] != "file" {
		t.Errorf("the second look must still be a file: %v", firstAttachment(t, out2))
	}
	if hits.Load() != 1 {
		t.Errorf("a saved attachment must not be downloaded twice in one process: %d", hits.Load())
	}
}
