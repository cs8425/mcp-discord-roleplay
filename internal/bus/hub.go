// hub.go gives every agent its own Queue.
//
// Why per-agent and not one shared ring: with a shared ring, agent B's message
// wakes agent A's waiting poll, A's filter drops it, and it is then either
// consumed by nobody or requeued in a way that can reorder events. A queue per
// agent removes both problems — an event only ever reaches the agents allowed
// to read it, and each queue drains strictly oldest first.
//
// Fanout stores the SAME *Event in each selected queue (an extra agent costs a
// pointer per event, ~16 KB at the default capacity of 2000). That is only safe
// because events are read-only once pushed; see the package doc.
package bus

import (
	"log/slog"
	"sort"
	"sync"
	"sync/atomic"
)

// Hub owns the per-agent queues. The set of queues is fixed at construction:
// a queue is never created lazily, so a bogus agent id in a request cannot grow
// memory. Agents with no queue get an explicit unknown_agent error from
// discord_poll.
type Hub struct {
	mu     sync.RWMutex
	queues map[string]*Queue
	order  []string // declaration order, for stable status output
	cap    int

	skipped    atomic.Uint64 // inbound events nobody was allowed to see
	selfEchoes atomic.Uint64 // our own messages, turned away at the door
}

// NewHub builds one queue per agent id. Duplicate ids collapse; the default
// agent (cfg.AgentID) should be included by the caller so a poll issued without
// an _meta.agentId still has somewhere to read from.
func NewHub(capacity int, agents []string, log *slog.Logger) *Hub {
	h := &Hub{queues: map[string]*Queue{}, cap: capacity}
	seen := map[string]bool{}
	for _, a := range agents {
		if a == "" || seen[a] {
			continue
		}
		seen[a] = true
		h.queues[a] = NewQueue(capacity)
		h.order = append(h.order, a)
	}
	sort.Strings(h.order)
	if log != nil {
		log.Info("event queues built", "queues", len(h.order), "agents", h.order, "capacity", capacity)
	}
	return h
}

// Queue returns an agent's queue, or false when that agent has none. A nil Hub
// is legal and reports "no queue" — poll answers unknown_agent instead of
// panicking when a build wires the dependency wrong.
func (h *Hub) Queue(agentID string) (*Queue, bool) {
	if h == nil {
		return nil, false
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	q, ok := h.queues[agentID]
	return q, ok
}

// Agents lists the agent ids that have a queue, sorted.
func (h *Hub) Agents() []string {
	if h == nil {
		return nil
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	return append([]string{}, h.order...)
}

// Fanout pushes e to every listed agent's queue and reports how many got it.
// Unknown agent ids are ignored (they cannot have messages). The event is
// stored by reference, so callers must not touch it afterwards.
func (h *Hub) Fanout(e *Event, agents []string) int {
	if h == nil || e == nil || len(agents) == 0 {
		return 0
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	n := 0
	for _, a := range agents {
		if q, ok := h.queues[a]; ok && q != nil {
			q.Push(e)
			n++
		}
	}
	return n
}

// WakeAll nudges every queue without storing anything (lifecycle notices:
// "gateway ready", "session resumed", "disconnected; reconnecting"). A change
// of gateway state is worth waking a blocked poll for — it can then answer
// disconnected instead of timing out on a dead line — but the notice itself is
// not information a caller reads, so it never occupies a slot, never counts as
// unread, and cannot evict a message on a full ring.
func (h *Hub) WakeAll() int {
	if h == nil {
		return 0
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	n := 0
	for _, q := range h.queues {
		q.Wake()
		n++
	}
	return n
}

// NoteSkipped records an inbound event that no agent was allowed to read.
func (h *Hub) NoteSkipped() {
	if h == nil {
		return
	}
	h.skipped.Add(1)
}

// Skipped reports how many inbound events were dropped before any queue.
func (h *Hub) Skipped() uint64 {
	if h == nil {
		return 0
	}
	return h.skipped.Load()
}

// NoteSelfEcho records a message this bot wrote, which is never queued: the
// sender already has it (discord_send handed it over, with the clock and the
// queue state attached), so storing the echo would be a second copy of a fact
// the caller does not need to learn. Counted separately from NoteSkipped
// because the reason differs: that one is "nobody was allowed to read this",
// this one is "somebody was allowed to, and does not want to".
func (h *Hub) NoteSelfEcho() {
	if h == nil {
		return
	}
	h.selfEchoes.Add(1)
}

// SelfEchoes reports how many of this bot's own messages were turned away.
func (h *Hub) SelfEchoes() uint64 {
	if h == nil {
		return 0
	}
	return h.selfEchoes.Load()
}

// QueueStat is one agent's queue counters, for discord_status.
type QueueStat struct {
	Agent    string `json:"agent"`
	Size     int    `json:"size"`
	Capacity int    `json:"capacity"`
	Pushed   uint64 `json:"pushed"`
	Dropped  uint64 `json:"dropped"`
}

// Stats reports every queue in declaration order.
func (h *Hub) Stats() []QueueStat {
	if h == nil {
		return nil
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make([]QueueStat, 0, len(h.order))
	for _, a := range h.order {
		s := h.queues[a].Stats()
		out = append(out, QueueStat{Agent: a, Size: s.Size, Capacity: s.Capacity, Pushed: s.Pushed, Dropped: s.Dropped})
	}
	return out
}

// ForEach runs f over every queue (fan-out point for future features such as
// per-agent typing indicators). f must not block.
func (h *Hub) ForEach(f func(agentID string, q *Queue)) {
	if h == nil || f == nil {
		return
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	for _, a := range h.order {
		f(a, h.queues[a])
	}
}
