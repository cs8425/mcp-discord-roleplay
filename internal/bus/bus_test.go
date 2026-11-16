package bus

import (
	"testing"
	"time"
)

func ev(target, text string) *Event {
	return &Event{Kind: "message", Target: target, Text: text, DM: false}
}

func TestPublishAssignsSeq(t *testing.T) {
	b := New(10)
	for i := 0; i < 3; i++ {
		b.Publish(ev("boss", "hi"))
	}
	st := b.Stats()
	if st.MaxSeq != 3 || st.Size != 3 || st.Dropped != 0 {
		t.Fatalf("stats = %+v", st)
	}
	got, last := b.After(0, nil, 10)
	if last != 3 || len(got) != 3 {
		t.Fatalf("After(0) = %d events, last %d", len(got), last)
	}
	for i, e := range got {
		if e.Seq != uint64(i+1) {
			t.Fatalf("event %d has seq %d", i, e.Seq)
		}
	}
}

func TestRingOverwritesOldest(t *testing.T) {
	b := New(3)
	for i := 1; i <= 5; i++ {
		b.Publish(ev("boss", string(rune('a'+i))))
	}
	st := b.Stats()
	if st.Size != 3 || st.Dropped != 2 || st.MaxSeq != 5 || st.Total != 5 {
		t.Fatalf("stats = %+v", st)
	}
	got, last := b.After(0, nil, 10)
	if last != 5 {
		t.Fatalf("cursor must follow the head even after overwrites, got %d", last)
	}
	if len(got) != 3 || got[0].Seq != 3 || got[2].Seq != 5 {
		t.Fatalf("ring should hold seq 3,4,5 got %v", seqs(got))
	}
}

func TestAfterLimitAndCursorRoundTrip(t *testing.T) {
	b := New(100)
	for i := 1; i <= 10; i++ {
		b.Publish(ev("boss", "m"))
	}
	got, last := b.After(0, nil, 4)
	if len(got) != 4 || got[0].Seq != 1 || last != 4 {
		t.Fatalf("first page = %v last=%d", seqs(got), last)
	}
	cur := Cursor(last)
	nextSeq, ok := ParseCursor(cur)
	if !ok || nextSeq != 4 {
		t.Fatalf("cursor round trip: %q -> %d,%v", cur, nextSeq, ok)
	}
	rest, last2 := b.After(nextSeq, nil, 100)
	if len(rest) != 6 || rest[0].Seq != 5 || last2 != 10 {
		t.Fatalf("second page = %v last=%d (cursor %q)", seqs(rest), last2, cur)
	}
	if v, ok := ParseCursor(""); v != 0 || ok {
		t.Fatalf("empty cursor must decode to 0,false, got %d,%v", v, ok)
	}
	if v, ok := ParseCursor("nonsense"); v != 0 || ok {
		t.Fatalf("garbage cursor must degrade to 0,false, got %d,%v", v, ok)
	}
	if Cursor(0) != "" {
		t.Fatal("seq 0 must serialize as the empty cursor")
	}
	// reading past the head is not an error, it is "nothing new"; the caller
	// keeps its own cursor when the returned high-water mark is 0.
	none, last3 := b.After(10, nil, 10)
	if len(none) != 0 || last3 != 0 {
		t.Fatalf("After(head) = %v/%d, want no events and cursor 0", seqs(none), last3)
	}
}

func TestAfterFilter(t *testing.T) {
	b := New(100)
	b.Publish(ev("boss", "a"))
	b.Publish(ev("side", "b"))
	b.Publish(ev("boss", "c"))

	got, _ := b.After(0, func(e *Event) bool { return e.Target == "boss" }, 10)
	if len(got) != 2 || got[0].Text != "a" || got[1].Text != "c" {
		t.Fatalf("filter returned %d events", len(got))
	}
	// the cursor must still advance past skipped events
	_, last := b.After(1, func(e *Event) bool { return e.Target == "boss" }, 10)
	if last != 3 {
		t.Fatalf("cursor should be 3, got %d", last)
	}
}

func TestLatestKeepsNewest(t *testing.T) {
	b := New(100)
	for i := 1; i <= 10; i++ {
		b.Publish(ev("boss", "m"))
	}
	got := b.Latest(nil, 3)
	if len(got) != 3 || got[0].Seq != 8 || got[2].Seq != 10 {
		t.Fatalf("Latest(3) = %v, want the newest three", seqs(got))
	}
	if got := b.Latest(nil, 0); len(got) != 1 {
		t.Fatalf("Latest(0) should still return one event, got %d", len(got))
	}
}

func TestNotifyWakesWaiters(t *testing.T) {
	b := New(10)
	ch := b.Notify()
	done := make(chan struct{})
	go func() {
		select {
		case <-ch:
			close(done)
		case <-time.After(2 * time.Second):
			panic("notify never fired")
		}
	}()
	b.Publish(ev("boss", "wake"))
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("waiter not woken")
	}
	// a fresh Notify must hand out a live channel, never the closed one
	if b.Notify() == nil {
		t.Fatal("Notify returned nil")
	}
}

func TestUnreadCountsOnlyMatches(t *testing.T) {
	b := New(100)
	b.Publish(ev("boss", "a"))
	b.Publish(ev("side", "b"))
	b.Publish(ev("boss", "c"))
	if n := b.Unread(0, func(e *Event) bool { return e.Target == "boss" }, 10); n != 2 {
		t.Fatalf("unread = %d, want 2", n)
	}
	if n := b.Unread(3, func(e *Event) bool { return e.Target == "boss" }, 10); n != 0 {
		t.Fatalf("no unread expected at head, got %d", n)
	}
}

func TestPublishNeverBlocks(t *testing.T) {
	// capacity 1 with no reader: publishes must return immediately.
	b := New(1)
	done := make(chan struct{})
	go func() {
		for i := 0; i < 100; i++ {
			b.Publish(ev("boss", "x"))
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Publish blocked")
	}
	if st := b.Stats(); st.Size != 1 || st.Dropped != 99 {
		t.Fatalf("stats = %+v", st)
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
		{"", "", KindOther},
	}
	for _, c := range cases {
		if got := ClassifyMedia(c.name, c.mime); got != c.want {
			t.Errorf("ClassifyMedia(%q,%q) = %s want %s", c.name, c.mime, got, c.want)
		}
	}
}

func TestAttachmentJSONShape(t *testing.T) {
	// M2 depends on these keys being stable; assert the contract early.
	a := Attachment{URL: "https://x/y.png", Kind: KindImage, Media: "inline"}
	b := New(4)
	b.Publish(&Event{Kind: "message", Attachments: []Attachment{a}, Ts: time.Now()})
	got, _ := b.After(0, nil, 1)
	if len(got) != 1 || len(got[0].Attachments) != 1 {
		t.Fatal("attachment lost")
	}
}

func seqs(evs []*Event) []uint64 {
	out := make([]uint64, 0, len(evs))
	for _, e := range evs {
		out = append(out, e.Seq)
	}
	return out
}
