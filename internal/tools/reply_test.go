package tools

// What a quoted message is worth on the wire. poll, history and get all build
// their rows through buildMessage and ship them through wire, so these tests are
// the whole feature's contract with the model: the id and the name always, the
// words only as far as --reply-max-bytes and the byte budget allow.

import (
	"context"
	"strings"
	"testing"

	"mcp-discord-roleplay/internal/bus"
	"mcp-discord-roleplay/internal/discord"
)

func TestCutRunes(t *testing.T) {
	cases := []struct {
		in   string
		max  int
		want string
		why  string
	}{
		{"hello", 10, "hello", "shorter than the cap"},
		{"hello", 5, "hello", "exactly the cap"},
		{"hello", 2, "he", "ascii head"},
		{"日本語", 4, "日", "never half a rune, even when 3 bytes of 4 are left"},
		{"a日本", 4, "a日", "the boundary moves with what came before"},
		{"日本語", 80, "日本語", "a cap nobody is near"},
		{"hello", 0, "", "zero is a decision, not a mistake"},
		{"hello", -1, "hello", "-1 is uncapped"},
		{"", 3, "", "nothing to cut"},
	}
	for _, c := range cases {
		if got := cutRunes(c.in, c.max); got != c.want {
			t.Errorf("cutRunes(%q, %d) = %q, want %q (%s)", c.in, c.max, got, c.want, c.why)
		}
	}
}

func TestMessageCostCountsTheQuote(t *testing.T) {
	m := pollMessage{Text: "12345"}
	if got := messageCost(m); got != 5 {
		t.Errorf("text alone: %d", got)
	}
	m.Reply = &bus.Reply{ID: "x", Text: "678"}
	if got := messageCost(m); got != 8 {
		t.Errorf("a quote is words too and has to pay its way: %d", got)
	}
	m.Reply.Text = ""
	if got := messageCost(m); got != 5 {
		t.Errorf("an id and a name cost nothing here (they are counted as JSON overhead): %d", got)
	}
}

func TestFitMessageDropsTheQuoteBeforeTheText(t *testing.T) {
	m := pollMessage{Text: strings.Repeat("a", 60), Reply: &bus.Reply{ID: "m1", Author: "ami", Text: strings.Repeat("b", 60)}}
	st := &drainState{}
	fitMessage(&m, 100, st) // 60 + 60 does not fit
	if m.Reply.Text != "" {
		t.Error("the quote is the first thing to go: it summarises a message the caller can still fetch")
	}
	if len(m.Text) != 60 {
		t.Errorf("the row's own words survived untouched: %d bytes", len(m.Text))
	}
	if m.Reply.ID != "m1" || m.Reply.Author != "ami" {
		t.Error("only the words were dropped: who and which id stay on the row")
	}
	// Said out loud, because a caller that expected a quote and got none has to be
	// able to tell "we dropped it" from "there was never one".
	if len(st.notes) != 1 || !strings.Contains(st.notes[0], "--poll-max-bytes") {
		t.Errorf("dropping something the caller might have wanted needs a note: %v", st.notes)
	}
}

func TestFitMessageStillCutsTheTextWhenTheQuoteWasNotEnough(t *testing.T) {
	m := pollMessage{Text: strings.Repeat("a", 120), Reply: &bus.Reply{ID: "m1", Text: strings.Repeat("b", 60)}}
	fitMessage(&m, 100, &drainState{})
	if m.Reply.Text != "" {
		t.Error("the quote went first")
	}
	if len(m.Text) > 100 || !m.TextTruncated {
		t.Errorf("then the text had to give too: %d bytes, truncated=%v", len(m.Text), m.TextTruncated)
	}
}

func TestPollRowCarriesTheQuotedMessage(t *testing.T) {
	d := newDeps(t, routingCfg(t))
	e := evMsg("m2", "對啊")
	e.Reply = &bus.Reply{ID: "m1", AuthorID: "u1", Author: "ami", Text: strings.Repeat("x", 300)}
	push(t, d, "default", e)

	row := onlyMsg(t, pollOK(t, d, `{"max_wait":1}`))
	rt, ok := row["reply_to"].(map[string]any)
	if !ok {
		t.Fatalf("a reply must arrive as reply_to: %+v", row)
	}
	if rt["id"] != "m1" || rt["author"] != "ami" {
		t.Errorf("which message and who said it are the two free facts: %+v", rt)
	}
	if got, want := len(rt["text"].(string)), d.Cfg.ReplyMaxBytes; got != want {
		t.Errorf("the snippet is cut to --reply-max-bytes (%d), got %d", want, got)
	}
	if _, ok := rt["author_id"]; ok {
		t.Error("author_id is bookkeeping for us, not for the model: it has no json tag on purpose")
	}
}

// An event lives in the queue until every agent has read it. If the snippet cut
// wrote through to it, the next poll of the same message would see a shorter quote
// than the flag asked for — and the one after that shorter still.
func TestPollReplyCutDoesNotDamageTheEvent(t *testing.T) {
	d := newDeps(t, routingCfg(t, "-reply-max-bytes=10"))
	e := evMsg("m2", "對啊")
	e.Reply = &bus.Reply{ID: "m1", Author: "ami", Text: strings.Repeat("x", 300)}
	push(t, d, "default", e)

	out := pollOK(t, d, `{"max_wait":1}`)
	if got := len(firstReply(t, out)["text"].(string)); got != 10 {
		t.Fatalf("first call: %d bytes", got)
	}
	if len(e.Reply.Text) != 300 {
		t.Errorf("the queued event must still hold the whole quote: %d bytes", len(e.Reply.Text))
	}
}

func TestPollReplyCapZeroKeepsWhoAndID(t *testing.T) {
	d := newDeps(t, routingCfg(t, "-reply-max-bytes=0"))
	e := evMsg("m2", "對啊")
	e.Reply = &bus.Reply{ID: "m1", Author: "ami", Text: "不需要的話"}
	push(t, d, "default", e)

	rt := firstReply(t, pollOK(t, d, `{"max_wait":1}`))
	if rt["id"] != "m1" || rt["author"] != "ami" {
		t.Errorf("0 says we quote nobody's words, not that nobody was quoted: %+v", rt)
	}
	if _, ok := rt["text"]; ok {
		t.Errorf("with --reply-max-bytes=0 there is no text on the wire: %+v", rt)
	}
}

func TestPollReplyUncapped(t *testing.T) {
	long := strings.Repeat("y", 500)
	d := newDeps(t, routingCfg(t, "-reply-max-bytes=-1"))
	e := evMsg("m2", "對啊")
	e.Reply = &bus.Reply{ID: "m1", Text: long}
	push(t, d, "default", e)

	if got := firstReply(t, pollOK(t, d, `{"max_wait":1}`))["text"]; got != long {
		t.Errorf("-1 is the operator saying \"ship it whole\": %d bytes", len(got.(string)))
	}
}

// A row that answers nobody carries no reply_to at all — not an empty object the
// model has to learn to ignore.
func TestPollRowWithoutReplySaysNothingAboutReplies(t *testing.T) {
	d := newDeps(t, routingCfg(t))
	push(t, d, "default", evMsg("m1", "自己講自己的"))

	row := onlyMsg(t, pollOK(t, d, `{"max_wait":1}`))
	if _, ok := row["reply_to"]; ok {
		t.Errorf("reply_to must be absent, not empty: %+v", row)
	}
	if _, ok := row["forwarded"]; ok {
		t.Errorf("forwarded must be absent too: %+v", row)
	}
}

func TestPollRowMarksAForward(t *testing.T) {
	d := newDeps(t, routingCfg(t))
	e := evMsg("f1", "原本的話")
	e.Forwarded = true
	push(t, d, "default", e)

	row := onlyMsg(t, pollOK(t, d, `{"max_wait":1}`))
	if row["forwarded"] != true {
		t.Errorf("the words are not this person's, and the row has to say so: %+v", row)
	}
}

// The byte budget is a fuse against one oversized message. A quote is words in
// everything but name, so it belongs on the meter: without this the fuse would
// let a message through at its full text size plus a quote nobody priced.
func TestPollBudgetCountsTheQuote(t *testing.T) {
	d := newDeps(t, routingCfg(t, "-poll-max-bytes=100", "-reply-max-bytes=60"))
	e := evMsg("m2", strings.Repeat("a", 80))
	e.Reply = &bus.Reply{ID: "m1", Author: "ami", Text: strings.Repeat("b", 60)}
	push(t, d, "default", e)

	out := pollOK(t, d, `{"max_wait":1}`)
	row := onlyMsg(t, out)
	rt := row["reply_to"].(map[string]any)
	if _, ok := rt["text"]; ok {
		t.Errorf("80 bytes of text + 60 of quote does not fit a budget of 100: %+v", rt)
	}
	if len(row["text"].(string)) != 80 {
		t.Errorf("the quote went first, so the message's own words are intact: %d", len(row["text"].(string)))
	}
	if !hasNote(out, "quoted") {
		t.Errorf("the answer has to admit it trimmed something: %v", out["notes"])
	}
}

func TestSendRejectsAReplyToThatIsNotAnID(t *testing.T) {
	d := newDeps(t, routingCfg(t))
	res := call(t, d, context.Background(), `{"to":"boss","text":"hi","reply_to":"last"}`)
	if !res.IsError {
		t.Fatalf("a reply_to that is not an id must be refused before anything is spent: %s", res.Content[0].Text)
	}
	out := decode(t, res)
	if out["error"] != "bad_arguments" {
		t.Errorf("error code: %v", out["error"])
	}
	hint, _ := out["hint"].(string)
	if !strings.Contains(hint, "reply_to") {
		t.Errorf("the hint has to name the field the model got wrong: %q", hint)
	}
}

func TestSendAcceptsAQuoteOnAMultiPartMessage(t *testing.T) {
	d := newDeps(t, routingCfg(t))
	body := strings.Repeat("abcdefghij ", 220) // 2420 runes: two messages
	out := decode(t, call(t, d, context.Background(),
		`{"to":"boss","text":"`+body+`","reply_to":"1572567398089883688"}`))
	if out["dry_run"] != true {
		t.Fatalf("expected the dry-run rehearsal, got: %+v", out)
	}
	if out["parts_sent"] != float64(2) {
		t.Fatalf("two parts, so the first-part-only rule actually runs: %v", out["parts_sent"])
	}
}

// The bounds are the whole point of this test. 19 is int64's digit count: a
// snowflake IS an int64, so a 20-digit "id" is a fat-fingered paste rather than a
// message. 15 is a deliberately loose floor -- nothing Discord ever issued is
// shorter, and being generous there costs only a soft reference that lands
// nowhere. We never parse the number, so 19 digits past int64's maximum still
// counts as a plausible id: Discord is the authority on whether it exists.
func TestValidMessageIDShape(t *testing.T) {
	good := []string{"1572567398089883688", "100000000000000000", strings.Repeat("1", 19), strings.Repeat("1", 15), "9223372036854775808"}
	bad := []string{"", "last", "1", "me", "#rp-main", "157256739808988368a", "1572567 398089883688", strings.Repeat("1", 20), strings.Repeat("1", 14)}
	for _, s := range good {
		if !validMessageID(s) {
			t.Errorf("%q is a Discord id", s)
		}
	}
	for _, s := range bad {
		if validMessageID(s) {
			t.Errorf("%q is not a Discord id", s)
		}
	}
}

// onlyMsg reads the single message row out of a decoded poll answer. poll_test.go
// already has firstMsg, which takes whatever is at index 0; these tests promise
// the model exactly one row, so they want the stricter reader.
func onlyMsg(t *testing.T, out map[string]any) map[string]any {
	t.Helper()
	list := msgs(t, out)
	if len(list) != 1 {
		t.Fatalf("want exactly one row, got %d: %+v", len(list), list)
	}
	row, ok := list[0].(map[string]any)
	if !ok {
		t.Fatalf("a row is an object: %+v", list[0])
	}
	return row
}

// firstReply reads the reply_to object of that row, failing when the row has none.
func firstReply(t *testing.T, out map[string]any) map[string]any {
	t.Helper()
	rt, ok := onlyMsg(t, out)["reply_to"].(map[string]any)
	if !ok {
		t.Fatalf("no reply_to on the row: %+v", onlyMsg(t, out))
	}
	return rt
}

// A quote is not a poll-only courtesy. history and get answer "what did this
// channel say", and an answer that leaves out which message was being answered is
// a different sentence — the reason the feature exists at all.
func TestHistoryAndGetCarryTheQuote(t *testing.T) {
	h := newDeps(t, routingCfg(t))
	h.DC.SetReadHooks(discord.ReadHooks{
		History: func(_ context.Context, _ discord.HistoryQuery) ([]*bus.Event, error) {
			ev := readEvent("12", "對啊", false)
			ev.Reply = &bus.Reply{ID: "11", AuthorID: "u1", Author: "ami", Text: "今晚要下海嗎"}
			ev.Forwarded = true
			return []*bus.Event{ev}, nil
		},
	})
	row := onlyMsg(t, decode(t, callRead(t, h.handleHistory, context.Background(), `{"target":"boss"}`)))
	assertRowKeys(t, row)
	if row["forwarded"] != true {
		t.Errorf("a forwarded row read back over REST is still forwarded: %+v", row)
	}
	if rt, ok := row["reply_to"].(map[string]any); !ok || rt["id"] != "11" || rt["author"] != "ami" {
		t.Errorf("the history row lost the quote: %+v", row)
	}

	g := newDeps(t, routingCfg(t))
	g.DC.SetReadHooks(discord.ReadHooks{
		Get: func(_ context.Context, _, _ string) (*bus.Event, error) {
			ev := readEvent("12", "對啊", false)
			ev.Reply = &bus.Reply{ID: "11", Author: "ami", Text: "今晚要下海嗎"}
			return ev, nil
		},
	})
	out := decode(t, callRead(t, g.handleGet, context.Background(), `{"target":"boss","message":"12"}`))
	msg, _ := out["message"].(map[string]any)
	assertRowKeys(t, msg)
	rt, ok := msg["reply_to"].(map[string]any)
	if !ok {
		t.Fatalf("get is the tool you point at one message; it has to show the quote: %+v", msg)
	}
	if rt["text"] != "今晚要下海嗎" {
		t.Errorf("get has no byte budget of its own, so the quote arrives whole: %v", rt["text"])
	}
}
