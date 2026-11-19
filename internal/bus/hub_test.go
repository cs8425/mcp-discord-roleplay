package bus

import (
	"io"
	"log/slog"
	"reflect"
	"testing"
)

func testHub(t *testing.T, cap int, agents ...string) *Hub {
	t.Helper()
	return NewHub(cap, agents, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func TestHubQueuesAreIndependent(t *testing.T) {
	h := testHub(t, 10, "alice", "bob")

	qa, _ := h.Queue("alice")
	qb, ok := h.Queue("bob")
	if !ok {
		t.Fatal("bob has no queue")
	}
	qa.Push(ev("x", "for alice"))

	if got, _ := qb.Drain(nil); len(got) != 0 {
		t.Fatalf("bob saw alice's event: %v", texts(got))
	}
	got, _ := qa.Drain(nil)
	if len(got) != 1 || got[0].Text != "for alice" {
		t.Fatalf("alice drain = %v", texts(got))
	}
}

func TestHubUnknownAgent(t *testing.T) {
	h := testHub(t, 10, "alice")
	if _, ok := h.Queue("mallory"); ok {
		t.Fatal("unknown agent must not get a queue")
	}
	// Fanout to an unknown agent is a no-op, not a crash
	n := h.Fanout(ev("x", "hi"), []string{"mallory"})
	if n != 0 {
		t.Fatalf("Fanout to unknown agent stored %d", n)
	}
	if _, ok := h.Queue("mallory"); ok {
		t.Fatal("Fanout must never create a queue lazily")
	}
}

func TestFanoutSharesOnePointer(t *testing.T) {
	h := testHub(t, 10, "alice", "bob", "carol")
	e := ev("x", "shared")

	if n := h.Fanout(e, []string{"alice", "bob", "nope"}); n != 2 {
		t.Fatalf("Fanout reported %d deliveries, want 2", n)
	}
	qa, _ := h.Queue("alice")
	qb, _ := h.Queue("bob")
	qaOnly, _ := qa.Drain(nil)
	qbOnly, _ := qb.Drain(nil)
	if len(qaOnly) != 1 || len(qbOnly) != 1 {
		t.Fatalf("both queues should hold the event: %v / %v", texts(qaOnly), texts(qbOnly))
	}
	if qaOnly[0] != qbOnly[0] {
		t.Fatal("fanout must share one *Event, not copy it per agent")
	}
	// draining alice must not touch bob
	qb2, _ := qb.Drain(nil)
	if len(qb2) != 0 {
		t.Fatal("bob's queue was affected by alice's drain")
	}
}

// A lifecycle notice must wake every waiter WITHOUT filling any queue: the ring is
// for things that happened, and a notice is not a thing anyone said. If it ever
// starts storing again, every restart costs each agent a slot and a phantom
// `queue` count -- which is exactly what it used to do.
func TestWakeAllNudgesWithoutQueueing(t *testing.T) {
	h := testHub(t, 5, "a", "b", "c")
	wakes := make([]<-chan struct{}, 0, 3)
	for _, name := range h.Agents() {
		q, _ := h.Queue(name)
		wakes = append(wakes, q.Notify())
	}
	if n := h.WakeAll(); n != 3 {
		t.Fatalf("WakeAll = %d, want 3", n)
	}
	for i, name := range h.Agents() {
		select {
		case <-wakes[i]:
		default:
			t.Errorf("queue %s was not woken", name)
		}
		q, _ := h.Queue(name)
		if st := q.Stats(); st.Size != 0 || st.Dropped != 0 {
			t.Errorf("queue %s after a wake: size=%d dropped=%d, want 0 and 0", name, st.Size, st.Dropped)
		}
	}
}

// The two doors must not share a counter: skipped_no_audience is "nobody was
// allowed to read this", self_echoes is "somebody was, and does not want to".
// Merging them would make a chatty agent look like a routing mistake.
func TestSelfEchoesAreCountedApartFromSkipped(t *testing.T) {
	h := testHub(t, 5, "a")
	h.NoteSkipped()
	h.NoteSelfEcho()
	h.NoteSelfEcho()
	if got := h.Skipped(); got != 1 {
		t.Errorf("Skipped = %d, want 1", got)
	}
	if got := h.SelfEchoes(); got != 2 {
		t.Errorf("SelfEchoes = %d, want 2", got)
	}
}

func TestHubStatsAndAgents(t *testing.T) {
	h := testHub(t, 4, "zed", "amy")
	q, _ := h.Queue("amy")
	q.Push(ev("x", "1"))
	q.Push(ev("x", "2"))

	stats := h.Stats()
	if len(stats) != 2 {
		t.Fatalf("stats = %+v", stats)
	}
	if stats[0].Agent != "amy" || stats[0].Size != 2 || stats[0].Capacity != 4 {
		t.Fatalf("amy stats = %+v", stats[0])
	}
	if stats[1].Agent != "zed" || stats[1].Size != 0 {
		t.Fatalf("zed stats = %+v", stats[1])
	}
	if !reflect.DeepEqual(h.Agents(), []string{"amy", "zed"}) {
		t.Fatalf("Agents() = %v, want sorted", h.Agents())
	}
}

func TestHubDedupesAgents(t *testing.T) {
	h := testHub(t, 4, "amy", "amy", "", "bob")
	if got := h.Agents(); len(got) != 2 {
		t.Fatalf("Agents() = %v, want 2 (empty and duplicate ids collapse)", got)
	}
}

func TestSkippedCounter(t *testing.T) {
	h := testHub(t, 4, "amy")
	h.NoteSkipped()
	h.NoteSkipped()
	if got := h.Skipped(); got != 2 {
		t.Fatalf("Skipped = %d, want 2", got)
	}
}

func TestForEach(t *testing.T) {
	h := testHub(t, 4, "a", "b")
	seen := map[string]bool{}
	h.ForEach(func(agent string, q *Queue) { seen[agent] = q != nil })
	if len(seen) != 2 || !seen["a"] || !seen["b"] {
		t.Fatalf("ForEach saw %v", seen)
	}
}
