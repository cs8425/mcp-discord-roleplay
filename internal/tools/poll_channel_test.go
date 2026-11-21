// poll_channel_test.go pins the one-conversation-per-answer rule: a queue holding
// two channels is answered one channel at a time, the oldest first, and the
// envelope -- not the rows -- says which conversation this batch was.
//
// The reason is the failure a mixed batch causes: a model reading
// "A1 B1 A2" answers the wrong half of it. The cost is that a busy multi-channel
// queue needs more polls, so these tests also pin that nothing is LOST or
// reordered on the way, and that the leftovers still explain themselves.

package tools

import (
	"strings"
	"testing"

	"mcp-discord-roleplay/internal/bus"
)

// chanEv is evMsg on an arbitrary channel target, so a test can build a queue
// that spans more than one conversation.
func chanEv(target, id, text string) *bus.Event {
	e := evMsg(id, text)
	e.Target = target
	e.ChannelID = strings.TrimPrefix(target, "channel:")
	e.ChannelName = "ch-" + e.ChannelID
	return e
}

// dmEv is the same for a DM, which the queue labels canonically as dm:<peer>.
func dmEv(peer, id, text string) *bus.Event {
	e := evMsg(id, text)
	e.Target = "dm:" + peer
	e.DM = true
	e.ChannelName = "Ami"
	e.Extra = map[string]string{"peer": peer}
	return e
}

// noRowChannel checks the row-level half of the rule: neither target nor dm is
// paid for on every row any more.
func noRowChannel(t *testing.T, out map[string]any) {
	t.Helper()
	for i, m := range msgs(t, out) {
		mm, _ := m.(map[string]any)
		for _, banned := range []string{"target", "dm"} {
			if _, bad := mm[banned]; bad {
				t.Errorf("messages[%d] carries %q: the envelope says it once", i, banned)
			}
		}
	}
}

func TestPollAnswersOneChannelAtATime(t *testing.T) {
	d := newDeps(t, routingCfg(t, "-poll-jitter-pct=0"))
	connected(d)

	// Interleaved on purpose, oldest first: boss, log, boss, log, boss.
	push(t, d, "default",
		chanEv("channel:111222333444555666", "b1", "b1"),
		chanEv("channel:333444555666777888", "l1", "l1"),
		chanEv("channel:111222333444555666", "b2", "b2"),
		chanEv("channel:333444555666777888", "l2", "l2"),
		chanEv("channel:111222333444555666", "b3", "b3"),
	)

	out := pollOK(t, d, `{"max_wait":1}`)
	wantStr(t, out, "reason", "messages")
	wantStr(t, out, "target", "boss") // the caller's own alias, not the snowflake
	if list := msgs(t, out); len(list) != 3 {
		t.Fatalf("the whole channel must come in one answer, got %d message(s): %v", len(list), list)
	}
	for i := 0; i < 3; i++ {
		if !strings.HasPrefix(wireText(t, out, i), "b") {
			t.Errorf("messages[%d] = %q, want a boss-channel message", i, wireText(t, out, i))
		}
	}
	noRowChannel(t, out)
	wantNum(t, out, "remaining", 2)
	wantStr(t, out, "waiting", "2 other_channel")

	// The other channel is next, in its own order, and nothing was reordered to get
	// there: skipping the front of the queue only ever moves a message backwards.
	out = pollOK(t, d, `{"max_wait":1}`)
	wantStr(t, out, "target", "log")
	if got := wireText(t, out, 0) + "," + wireText(t, out, 1); got != "l1,l2" {
		t.Errorf("log batch = %q, want the two of them in queue order", got)
	}
	noRowChannel(t, out)

	// Caught up: an answer with no messages has no conversation to name.
	out = pollOK(t, d, `{"max_wait":1}`)
	wantStr(t, out, "reason", "timeout")
	if _, bad := out["target"]; bad {
		t.Errorf("a timeout must not name a channel it delivered nothing from: %v", out["target"])
	}
}

// A batch cut short by `limit` in the middle of a channel continues that channel;
// it does not jump to the one waiting behind it.
func TestPollLimitCutContinuesSameChannel(t *testing.T) {
	d := newDeps(t, routingCfg(t, "-poll-jitter-pct=0"))
	connected(d)
	push(t, d, "default",
		chanEv("channel:111222333444555666", "b1", "b1"),
		chanEv("channel:111222333444555666", "b2", "b2"),
		chanEv("channel:111222333444555666", "b3", "b3"),
		chanEv("channel:333444555666777888", "l1", "l1"),
		chanEv("channel:333444555666777888", "l2", "l2"),
	)

	var seen []string
	for i := 0; i < 3; i++ {
		out := pollOK(t, d, `{"max_wait":1,"limit":2}`)
		s, _ := out["target"].(string)
		seen = append(seen, s)
	}
	if got := strings.Join(seen, ","); got != "boss,boss,log" {
		t.Errorf("targets over three polls = %q, want boss,boss,log", got)
	}
}

// The same promise on the other cut: --poll-max-bytes stops the pass in the
// middle of a channel, and the next answer is the rest of that channel.
func TestPollBudgetCutContinuesSameChannel(t *testing.T) {
	d := newDeps(t, routingCfg(t, "-poll-jitter-pct=0", "-poll-max-bytes=150"))
	connected(d)
	long := strings.Repeat("x", 60)
	push(t, d, "default",
		chanEv("channel:111222333444555666", "b1", "b1 "+long),
		chanEv("channel:111222333444555666", "b2", "b2 "+long),
		chanEv("channel:111222333444555666", "b3", "b3 "+long),
		chanEv("channel:333444555666777888", "l1", "l1"),
	)

	out := pollOK(t, d, `{"max_wait":1}`)
	wantStr(t, out, "target", "boss")
	if len(msgs(t, out)) != 2 {
		t.Fatalf("two 63-byte messages fit in 150, the third does not: %v", msgs(t, out))
	}
	out = pollOK(t, d, `{"max_wait":1}`)
	wantStr(t, out, "target", "boss")
	if got := wireText(t, out, 0); !strings.HasPrefix(got, "b3") {
		t.Errorf("second answer = %q, want the boss message the budget left behind", got)
	}
	out = pollOK(t, d, `{"max_wait":1}`)
	wantStr(t, out, "target", "log")
}

// The caller's own filters are answered first: a message that is out of scope is
// reported as such, not as "another channel", and it never pins the batch.
func TestPollFilterRunsBeforeTheChannelLatch(t *testing.T) {
	d := newDeps(t, routingCfg(t, "-poll-jitter-pct=0"))
	connected(d)
	push(t, d, "default",
		chanEv("channel:333444555666777888", "l1", "l1"),
		chanEv("channel:333444555666777888", "l2", "l2"),
		chanEv("channel:111222333444555666", "b1", "b1"),
	)

	out := pollOK(t, d, `{"targets":["boss"],"max_wait":1}`)
	wantStr(t, out, "target", "boss")
	if len(msgs(t, out)) != 1 {
		t.Fatalf("only the in-scope message may be delivered: %v", msgs(t, out))
	}
	wantStr(t, out, "waiting", "2 target")
}

// A DM batch names itself the same way -- with dm:true moved up from the rows.
func TestPollNamesADMBatchOnTheEnvelope(t *testing.T) {
	d := newDeps(t, routingCfg(t, "-poll-jitter-pct=0"))
	connected(d)
	push(t, d, "default",
		dmEv("222333444555666777", "d1", "d1"),
		chanEv("channel:111222333444555666", "b1", "b1"),
		dmEv("222333444555666777", "d2", "d2"),
	)

	out := pollOK(t, d, `{"max_wait":1}`)
	wantStr(t, out, "target", "ami")
	if isDM, _ := out["dm"].(bool); !isDM {
		t.Errorf("envelope dm = %v, want true: it is the flag that tells a caller to answer with dm:<id>", out["dm"])
	}
	if len(msgs(t, out)) != 2 {
		t.Fatalf("both DM messages belong to this answer: %v", msgs(t, out))
	}
	noRowChannel(t, out)

	out = pollOK(t, d, `{"max_wait":1}`)
	wantStr(t, out, "target", "boss")
	if _, bad := out["dm"]; bad {
		t.Errorf("a guild batch must not claim to be a DM: %v", out)
	}
}

// pollChannelKey is what the batching compares, so its edges are worth naming:
// case, the label-vs-id question, and the fallbacks when an event is thin.
func TestPollChannelKeyIsCanonical(t *testing.T) {
	cases := []struct {
		name string
		ev   *bus.Event
		want string
	}{
		{"canonical", &bus.Event{Target: "channel:111222333444555666"}, "channel:111222333444555666"},
		{"case folded", &bus.Event{Target: "Channel:111222333444555666"}, "channel:111222333444555666"},
		{"dm peer", &bus.Event{DM: true, Extra: map[string]string{"peer": "AA"}}, "dm:aa"},
		{"id fallback", &bus.Event{ChannelID: "BB"}, "channel:bb"},
		{"nothing to go on", &bus.Event{}, ""},
	}
	for _, c := range cases {
		if got := pollChannelKey(c.ev); got != c.want {
			t.Errorf("%s: pollChannelKey = %q, want %q", c.name, got, c.want)
		}
	}
}

// The latch is per Drain attempt, and an attempt is per poll call -- so a queue
// that keeps arriving on one channel is never starved by another. This is the
// shape of a live queue: the tail is always newer than what was skipped.
func TestPollLaterArrivalDoesNotStarveSkippedChannel(t *testing.T) {
	d := newDeps(t, routingCfg(t, "-poll-jitter-pct=0"))
	connected(d)
	push(t, d, "default",
		chanEv("channel:111222333444555666", "b1", "b1"),
		chanEv("channel:333444555666777888", "l1", "l1"),
	)
	// log keeps talking while boss is being answered.
	out := pollOK(t, d, `{"max_wait":1}`)
	wantStr(t, out, "target", "boss")
	push(t, d, "default", chanEv("channel:333444555666777888", "l2", "l2"))

	out = pollOK(t, d, `{"max_wait":1}`)
	wantStr(t, out, "target", "log")
	if got := wireText(t, out, 0) + "," + wireText(t, out, 1); got != "l1,l2" {
		t.Errorf("log batch = %q, want the skipped message first and the new one after", got)
	}
}
