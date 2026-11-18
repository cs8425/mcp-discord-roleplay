package bus

import (
	"encoding/json"
	"testing"
	"time"
)

func ev(target, text string) *Event {
	return &Event{Kind: "message", Target: target, Text: text}
}

func texts(evs []*Event) []string {
	out := make([]string, 0, len(evs))
	for _, e := range evs {
		out = append(out, e.Text)
	}
	return out
}

func eq(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func fill(q *Queue, names ...string) {
	for _, n := range names {
		q.Push(ev("boss", n))
	}
}

func TestPushCounts(t *testing.T) {
	q := NewQueue(10)
	fill(q, "a", "b", "c")
	st := q.Stats()
	if st.Size != 3 || st.Pushed != 3 || st.Dropped != 0 || st.Capacity != 10 {
		t.Fatalf("stats = %+v", st)
	}
	got, rem := q.Drain(nil)
	if !eq(texts(got), []string{"a", "b", "c"}) {
		t.Fatalf("drain order = %v, want oldest first", texts(got))
	}
	if rem != 0 {
		t.Fatalf("remaining = %d, want 0", rem)
	}
	if again, _ := q.Drain(nil); len(again) != 0 {
		t.Fatal("drained events must not reappear")
	}
}

func TestDrainSkipLeavesInPlace(t *testing.T) {
	q := NewQueue(10)
	fill(q, "a1", "b1", "a2", "b2")

	got, rem := q.Drain(func(e *Event) Verdict {
		if e.Text[0] == 'a' {
			return Take
		}
		return Skip
	})
	if !eq(texts(got), []string{"a1", "a2"}) {
		t.Fatalf("take = %v", texts(got))
	}
	if rem != 2 {
		t.Fatalf("remaining = %d, want 2", rem)
	}
	// the skipped ones are untouched, in order, and a different filter gets them
	rest, rem2 := q.Drain(func(e *Event) Verdict {
		if e.Text[0] == 'b' {
			return Take
		}
		return Skip
	})
	if !eq(texts(rest), []string{"b1", "b2"}) || rem2 != 0 {
		t.Fatalf("second drain = %v rem %d", texts(rest), rem2)
	}
}

func TestDrainStopKeepsOrderAndTail(t *testing.T) {
	q := NewQueue(10)
	fill(q, "a", "b", "c", "d", "e")

	n := 0
	got, rem := q.Drain(func(e *Event) Verdict {
		if n >= 2 {
			return Stop
		}
		n++
		return Take
	})
	if !eq(texts(got), []string{"a", "b"}) {
		t.Fatalf("take = %v, want exactly the two oldest", texts(got))
	}
	if rem != 3 {
		t.Fatalf("remaining = %d, want 3", rem)
	}
	// nothing was skipped or reordered: the next drain returns c,d,e in order
	again, _ := q.Drain(nil)
	if !eq(texts(again), []string{"c", "d", "e"}) {
		t.Fatalf("tail = %v, want c,d,e untouched and in order", texts(again))
	}
}

func TestDrainStopDoesNotPeekAhead(t *testing.T) {
	// A Stop must not evaluate want() for the rest of the queue: the caller's
	// budget counter would be consulted for events it has no room for.
	q := NewQueue(10)
	fill(q, "a", "b", "c", "d")
	seen := 0
	q.Drain(func(e *Event) Verdict {
		seen++
		return Stop
	})
	if seen != 1 {
		t.Fatalf("want() called %d times after Stop, want 1", seen)
	}
	if st := q.Stats(); st.Size != 4 {
		t.Fatalf("Stop must not remove anything, size = %d", st.Size)
	}
}

func TestDrainOnEmpty(t *testing.T) {
	q := NewQueue(4)
	got, rem := q.Drain(nil)
	if len(got) != 0 || rem != 0 {
		t.Fatalf("empty drain = %v/%d", got, rem)
	}
}

func TestRingDropsNewestSideAndWraps(t *testing.T) {
	q := NewQueue(3)
	fill(q, "1", "2", "3", "4", "5")
	st := q.Stats()
	if st.Size != 3 || st.Dropped != 2 || st.Pushed != 5 {
		t.Fatalf("stats = %+v", st)
	}
	// wrap-around: the surviving entries are the newest three, oldest first
	got, _ := q.Drain(nil)
	if !eq(texts(got), []string{"3", "4", "5"}) {
		t.Fatalf("after wrap = %v, want 3,4,5 (no duplicates, no holes)", texts(got))
	}
	// push again past the wrap boundary and drain once more
	fill(q, "6", "7", "8", "9")
	got, _ = q.Drain(nil)
	if !eq(texts(got), []string{"7", "8", "9"}) {
		t.Fatalf("second wrap = %v, want 7,8,9", texts(got))
	}
}

func TestRequeueFrontRestoresOrder(t *testing.T) {
	q := NewQueue(10)
	fill(q, "a", "b", "c")
	got, _ := q.Drain(func(e *Event) Verdict {
		if e.Text == "b" {
			return Skip
		}
		return Take
	})
	if !eq(texts(got), []string{"a", "c"}) {
		t.Fatalf("take = %v", texts(got))
	}
	q.RequeueFront(got)
	// restored batch is older than what stayed, and keeps its own order
	again, rem := q.Drain(nil)
	if !eq(texts(again), []string{"a", "c", "b"}) {
		t.Fatalf("after requeue = %v, want a,c,b", texts(again))
	}
	if rem != 0 {
		t.Fatalf("remaining = %d", rem)
	}
}

func TestRequeueFrontOverflowDropsAndCounts(t *testing.T) {
	q := NewQueue(3)
	batch := []*Event{ev("boss", "1"), ev("boss", "2"), ev("boss", "3"), ev("boss", "4")}
	q.RequeueFront(batch)
	st := q.Stats()
	if st.Size != 3 || st.Dropped != 1 {
		t.Fatalf("stats = %+v, want the newest 3 kept and 1 dropped", st)
	}
	got, _ := q.Drain(nil)
	if !eq(texts(got), []string{"2", "3", "4"}) {
		t.Fatalf("kept = %v", texts(got))
	}
}

func TestLatestDoesNotConsume(t *testing.T) {
	q := NewQueue(10)
	for i := 1; i <= 10; i++ {
		q.Push(ev("boss", string(rune('a'+i))))
	}
	got := q.Latest(nil, 3)
	if len(got) != 3 {
		t.Fatalf("Latest(3) = %d", len(got))
	}
	if got[0].Text != "k" || got[2].Text != "i" {
		t.Fatalf("Latest must return newest first, got %v", texts(got))
	}
	if st := q.Stats(); st.Size != 10 {
		t.Fatalf("Latest consumed events: size = %d", st.Size)
	}
	if got := q.Latest(func(e *Event) bool { return false }, 5); len(got) != 0 {
		t.Fatal("filter must be honoured")
	}
	if got := q.Latest(nil, 0); len(got) != 0 {
		t.Fatal("limit 0 means nothing")
	}
}

func TestNotifyWakesWaiter(t *testing.T) {
	q := NewQueue(10)
	ch := q.Notify()
	done := make(chan struct{})
	go func() {
		select {
		case <-ch:
			close(done)
		case <-time.After(2 * time.Second):
			panic("notify never fired")
		}
	}()
	q.Push(ev("boss", "wake"))
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("waiter not woken")
	}
	// a fresh Notify hands out a live channel, never the closed one
	select {
	case <-q.Notify():
		t.Fatal("fresh Notify must not be closed already")
	default:
	}
}

// The pre-read order that poll relies on: read Notify, then drain. A push that
// lands between the two still wakes the waiter.
func TestNotifyNoMissedWakeup(t *testing.T) {
	q := NewQueue(10)
	for i := 0; i < 50; i++ {
		ch := q.Notify()
		if got, _ := q.Drain(nil); len(got) > 0 {
			continue // drained the race winner
		}
		go q.Push(ev("boss", "x"))
		select {
		case <-ch:
		case <-time.After(2 * time.Second):
			t.Fatal("missed a wakeup")
		}
		if got, _ := q.Drain(nil); len(got) == 0 {
			t.Fatal("woken but queue empty")
		}
	}
}

func TestPushNeverBlocks(t *testing.T) {
	q := NewQueue(1)
	done := make(chan struct{})
	go func() {
		for i := 0; i < 100; i++ {
			q.Push(ev("boss", "x"))
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Push blocked")
	}
	if st := q.Stats(); st.Size != 1 || st.Dropped != 99 {
		t.Fatalf("stats = %+v", st)
	}
}

func TestZeroCapacityQueueIsUsable(t *testing.T) {
	q := NewQueue(0)
	q.Push(ev("boss", "a"))
	if got, _ := q.Drain(nil); len(got) != 1 {
		t.Fatalf("NewQueue(0) should behave as capacity 1, got %v", texts(got))
	}
}

func TestEventJSONShape(t *testing.T) {
	// The keys the poll renderer and the LLM prompt depend on must stay stable.
	q := NewQueue(4)
	q.Push(&Event{
		Kind: "message", Ts: time.Now(), Time: "2026-01-01T00:00:00+00:00",
		ChannelID: "42", MessageID: "99", Author: Author{ID: "7", Name: "amy"},
		Text: "hi", Attachments: []Attachment{{ID: "1", Kind: KindImage, URL: "https://x/y.png"}},
	})
	got, _ := q.Drain(nil)
	b, err := json.Marshal(got[0])
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"kind", "time", "channel_id", "message_id", "author", "attachments"} {
		if _, ok := m[k]; !ok {
			t.Errorf("event JSON lost key %q: %s", k, b)
		}
	}
	// ts is internal only
	if _, ok := m["ts"]; ok {
		t.Error("Ts must stay out of the wire format")
	}
	// rendering decisions are NOT part of the shared event (they belong to the
	// caller's output struct; a shared pointer must never carry them)
	if _, ok := m["media"]; ok {
		t.Error("bus.Attachment must not carry a media rendering verdict")
	}
	if _, ok := m["local_path"]; ok {
		t.Error("bus.Attachment must not carry a local path")
	}
}

func TestClassifyMedia(t *testing.T) {
	cases := []struct {
		name, mime string
		want       string
	}{
		{"shot.PNG", "", KindImage},
		{"clip.gifv", "", "other"},
		{"voice.ogg", "", KindAudio},
		{"movie.mp4", "", KindVideo},
		{"data.bin", "image/webp", KindImage},
		{"x.txt", "audio/mpeg", KindAudio},
		{"note", "application/ogg", KindAudio},
		{"", "", KindOther},
	}
	for _, c := range cases {
		if got := ClassifyMedia(c.name, c.mime); got != c.want {
			t.Errorf("ClassifyMedia(%q,%q) = %s want %s", c.name, c.mime, got, c.want)
		}
	}
}

// Oldest is the peek that lets a send report how long the front of the queue has
// waited. It must never consume: a peek that cost a message would quietly eat
// the caller's inbox, and the number would then disagree with the next poll.
func TestOldestIsAPeek(t *testing.T) {
	q := NewQueue(4)
	if got := q.Oldest(); got != nil {
		t.Fatalf("an empty queue has no oldest, got %+v", got)
	}

	first := &Event{Kind: "message", Target: "boss", Text: "a", Ts: time.Now().Add(-30 * time.Second)}
	q.Push(first)
	q.Push(&Event{Kind: "message", Target: "boss", Text: "b", Ts: time.Now()})

	if got := q.Oldest(); got != first {
		t.Fatalf("oldest = %+v, want the first push", got)
	}
	if s := q.Stats(); s.Size != 2 {
		t.Errorf("a peek must not change the size: %+v", s)
	}

	// Take the front and the next one is oldest: this is exactly what a poll
	// would deliver next, so the two views can never disagree.
	q.Drain(func(e *Event) Verdict {
		if e.Text == "a" {
			return Take
		}
		return Stop
	})
	if got := q.Oldest(); got == nil || got.Text != "b" {
		t.Errorf("oldest after taking a = %+v, want b", got)
	}
}
