// Package bus is the in-memory event queue between the Discord gateway
// handlers and the MCP tool callers.
//
// There is one Queue per agent (see Hub). The gateway handler only ever calls
// Hub.Fanout / Queue.Push, which never block and never perform I/O — that is a
// hard requirement, because a slow handler in the Discord websocket read loop
// stalls heartbeats.
//
// QUEUES ARE NOT A BACKLOG. A Queue is a fixed-size ring: when it fills, the
// oldest event is dropped and counted in Stats.Dropped. Dropped events are gone
// for good. This process deliberately never replays history on startup, so
// everything in here was produced while it was running: a restart empties every
// queue by design. Catching up on missed messages is a REST feature
// (discord_history), not something a queue can or should do.
//
// EVENTS ARE READ-ONLY ONCE PUSHED. Hub.Fanout hands the *same* pointer to
// several queues, so no code may mutate an Event (or its Attachment/Embed
// slices) after Push: a write done while rendering one agent's poll would be
// visible to every other agent and to later polls. Anything a caller computes
// about an event (for example how a media attachment was rendered) belongs in
// the caller's own output struct, never back on the event.
package bus

import (
	"strings"
	"sync"
	"time"
)

// Event kinds.
//
// KindSystem is what lifecycle notices ("gateway reconnected") used to be: they
// were pushed into every queue purely to wake a waiting poll, which meant they
// occupied a slot, counted as unread, and could evict a real message on a full
// ring. They now go through Hub.WakeAll and are not stored at all, so the kind
// survives only as a defensive check in poll — nothing puts one in a queue.
const (
	KindMessage = "message"
	KindSystem  = "system"
)

// Attachment kinds (Attachment.Kind). These literals mirror the media policy
// keys in internal/config, which are matched against the same strings.
const (
	KindImage = "image"
	KindAudio = "audio"
	KindVideo = "video"
	KindOther = "other"
)

// Where an attachment came from (Attachment.Origin). An upload from Discord leaves
// it empty. The two non-empty values mark an attachment we assembled ourselves:
// out of a message's sticker_items, or out of the custom emoji codes in its text
// (Discord sends no file for those; the client renders them from the id). Both
// point at a CDN file named by a snowflake id whose
// bytes Discord never replaces, which is exactly the property that makes them
// worth caching by URL (see internal/media).
const (
	OriginEmoji   = "emoji"
	OriginSticker = "sticker"
)

// Author is the normalized sender.
type Author struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Tag  string `json:"tag,omitempty"`
	Bot  bool   `json:"bot,omitempty"`
	Self bool   `json:"self,omitempty"`
}

// Attachment is a normalized file attached to a message. It carries metadata
// only: how an attachment is rendered (inlined, saved to disk, skipped) is a
// per-caller decision and lives in the caller's output struct, because this
// value is shared between agents and polls (see the package doc).
type Attachment struct {
	ID           string  `json:"id"`
	Filename     string  `json:"filename,omitempty"`
	ContentType  string  `json:"content_type,omitempty"`
	Kind         string  `json:"kind"`             // image|audio|video|other
	Origin       string  `json:"origin,omitempty"` // "" = an upload | emoji | sticker
	Size         int     `json:"size,omitempty"`
	Width        int     `json:"width,omitempty"`
	Height       int     `json:"height,omitempty"`
	DurationSecs float64 `json:"duration_secs,omitempty"`
	URL          string  `json:"url"`
	ProxyURL     string  `json:"proxy_url,omitempty"`
}

// Embed is a minimal view of a Discord embed (rich cards are flattened).
type Embed struct {
	Type        string `json:"type,omitempty"`
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
	URL         string `json:"url,omitempty"`
}

// Event is one stored item. It has no identity field on purpose: seq numbers
// are per-queue bookkeeping and are not exposed, so nothing can be built that
// depends on them surviving a restart.
type Event struct {
	Kind string    `json:"kind"`
	Ts   time.Time `json:"-"`
	Time string    `json:"time"`
	AgeS float64   `json:"age_s,omitempty"`

	Target      string `json:"target,omitempty"`
	GuildID     string `json:"guild_id,omitempty"`
	ChannelID   string `json:"channel_id,omitempty"`
	ChannelName string `json:"channel_name,omitempty"`
	DM          bool   `json:"dm,omitempty"`

	MessageID string `json:"message_id,omitempty"`
	Author    Author `json:"author"`
	Text      string `json:"text,omitempty"`

	// Truncated means Text is not the whole original message.
	Truncated bool `json:"truncated,omitempty"`

	MentionsSelf bool         `json:"mentions_self,omitempty"`
	Attachments  []Attachment `json:"attachments,omitempty"`
	Embeds       []Embed      `json:"embeds,omitempty"`

	// Extra holds kind-specific fields (e.g. peer) so future event types do
	// not need to change this struct.
	Extra map[string]string `json:"extra,omitempty"`
}

// Stats is a snapshot of one Queue.
type Stats struct {
	Size     int    `json:"size"`
	Capacity int    `json:"capacity"`
	Pushed   uint64 `json:"pushed"`
	Dropped  uint64 `json:"dropped"`
}

// Verdict is Queue.Drain's answer for one event.
type Verdict int

const (
	// Take delivers this event and removes it from the queue.
	Take Verdict = iota
	// Skip leaves the event in place, untouched, and keeps scanning.
	Skip
	// Stop ends the scan: this event and everything behind it stay in the
	// queue, in order. It is the "out of budget" answer — because Drain keeps
	// strict order, anything left behind is exactly what the next poll sees
	// first.
	Stop
)

// Queue is a fixed-capacity FIFO of events with a wake-up signal. The zero
// value is not usable; call NewQueue.
type Queue struct {
	mu      sync.Mutex
	cap     int
	ring    []*Event
	next    int // index of the next write
	size    int
	total   uint64
	dropped uint64

	notify chan struct{}
}

// NewQueue returns a queue holding at most capacity events. capacity <= 0 means
// "keep the newest default" (1) so a misconfiguration cannot create a queue
// that silently holds nothing.
func NewQueue(capacity int) *Queue {
	if capacity <= 0 {
		capacity = 1
	}
	return &Queue{
		cap:    capacity,
		ring:   make([]*Event, capacity),
		notify: make(chan struct{}),
	}
}

// Push appends an event, evicting the oldest one when full. It never blocks.
func (q *Queue) Push(e *Event) {
	if e == nil {
		return
	}
	q.mu.Lock()
	q.total++
	if q.ring[q.next] != nil {
		// overwriting the oldest live slot
		q.dropped++
	}
	q.ring[q.next] = e
	q.next = (q.next + 1) % q.cap
	if q.size < q.cap {
		q.size++
	}
	q.signalLocked()
	q.mu.Unlock()
}

// Notify returns a channel closed when an event arrives. Callers must read this
// channel BEFORE draining, then wait on it: a Push closes whatever channel
// existed at the time of the push, so a pre-read channel can never be missed.
func (q *Queue) Notify() <-chan struct{} {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.notify
}

// Wake signals waiting pollers without storing anything. A lifecycle notice is
// worth waking up for, but it is not information a caller reads, so it must not
// occupy a slot, count towards Size, or risk evicting an unread message. Nothing
// else changes: no Pushed, no Dropped, and a poll that finds an empty queue
// still answers a clean timeout.
func (q *Queue) Wake() {
	q.mu.Lock()
	q.signalLocked()
	q.mu.Unlock()
}

// Drain walks the queue oldest first, asks want() about each event, removes
// everything answered Take, and returns those events plus how many are still
// queued. Answers are strictly in queue order: Skip leaves an event in place,
// Stop stops the walk and leaves that event and the whole tail untouched.
//
// want may be nil, which means "take everything". It runs with the queue
// locked, so it must be pure computation: no I/O, no waiting, no calling back
// into the Hub (Push would deadlock on the same mutex).
func (q *Queue) Drain(want func(*Event) Verdict) (out []*Event, remaining int) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.size == 0 {
		return nil, 0
	}
	all := make([]*Event, 0, q.size)
	for i := 0; i < q.size; i++ {
		if e := q.atLocked(i); e != nil {
			all = append(all, e)
		}
	}

	var outEv, keep []*Event
	i := 0
	for ; i < len(all); i++ {
		v := Take
		if want != nil {
			v = want(all[i])
		}
		if v == Stop {
			break
		}
		if v == Skip {
			keep = append(keep, all[i])
			continue
		}
		outEv = append(outEv, all[i])
	}
	keep = append(keep, all[i:]...) // untouched tail (also covers Stop)

	q.rewriteLocked(keep)
	return outEv, len(keep)
}

// RequeueFront puts already-drained events back at the OLDEST end, preserving
// their order, so the next Drain sees them first — as if they were never taken.
// It exists for one case: a client cancelled the request after we drained, so
// the batch was never delivered. If the queue overflows, the newest events are
// dropped and counted.
func (q *Queue) RequeueFront(evs []*Event) {
	if len(evs) == 0 {
		return
	}
	q.mu.Lock()
	defer q.mu.Unlock()

	have := make([]*Event, 0, q.size)
	for i := 0; i < q.size; i++ {
		if e := q.atLocked(i); e != nil {
			have = append(have, e)
		}
	}
	all := append(append([]*Event{}, evs...), have...)
	if len(all) > q.cap {
		// keep the newest cap entries: drop from the oldest end of what we are
		// re-adding, which is the least useful part of a batch nobody read.
		drop := len(all) - q.cap
		q.dropped += uint64(drop)
		all = all[drop:]
	}
	q.rewriteLocked(all)
}

// Latest returns up to limit events passing f (nil f = all), newest first. It
// does not remove anything; it exists for discord_status previews.
func (q *Queue) Latest(f func(*Event) bool, limit int) []*Event {
	q.mu.Lock()
	defer q.mu.Unlock()
	if limit <= 0 || q.size == 0 {
		return nil
	}
	var out []*Event
	for i := q.size - 1; i >= 0; i-- {
		e := q.atLocked(i)
		if e == nil {
			continue
		}
		if f != nil && !f(e) {
			continue
		}
		out = append(out, e)
		if len(out) >= limit {
			break
		}
	}
	return out
}

// Oldest returns the event that has waited longest, or nil when the queue is
// empty. It is a peek: nothing is removed and no wake-up signal is consumed, so
// looking costs the caller no message. The ring's front is index 0 (atLocked
// numbers oldest-first), which is exactly the next thing a poll would deliver.
func (q *Queue) Oldest() *Event {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.size == 0 {
		return nil
	}
	return q.atLocked(0)
}

// Stats reports current queue state.
func (q *Queue) Stats() Stats {
	q.mu.Lock()
	defer q.mu.Unlock()
	return Stats{Size: q.size, Capacity: q.cap, Pushed: q.total, Dropped: q.dropped}
}

// --- internals (all called with q.mu held) ---

// atLocked returns the i-th oldest live entry.
func (q *Queue) atLocked(i int) *Event {
	start := (q.next - q.size + q.cap) % q.cap
	return q.ring[(start+i)%q.cap]
}

// rewriteLocked replaces the whole contents with events, oldest first.
func (q *Queue) rewriteLocked(events []*Event) {
	n := len(events)
	if n > q.cap {
		n = q.cap
	}
	// the caller may hand us more than cap only from RequeueFront, which trims
	// first; keep this defensive so slots are never left dangling
	copy(q.ring[:n], events[:n])
	for i := n; i < q.cap; i++ {
		q.ring[i] = nil
	}
	q.size = n
	q.next = n % q.cap
}

// signalLocked wakes every current waiter.
func (q *Queue) signalLocked() {
	if q.notify != nil {
		close(q.notify)
	}
	q.notify = make(chan struct{})
}

// ClassifyMedia maps a filename/content-type to image|audio|video|other.
func ClassifyMedia(filename, contentType string) string {
	ct := strings.ToLower(strings.TrimSpace(contentType))
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = strings.TrimSpace(ct[:i])
	}
	switch {
	case strings.HasPrefix(ct, "image/"):
		return KindImage
	case strings.HasPrefix(ct, "audio/"), ct == "application/ogg":
		return KindAudio
	case strings.HasPrefix(ct, "video/"):
		return KindVideo
	}
	// fall back on the extension (Discord voice messages arrive as .ogg/.opus)
	ext := strings.ToLower(filename)
	if i := strings.LastIndexByte(ext, '.'); i >= 0 {
		switch ext[i:] {
		case ".png", ".jpg", ".jpeg", ".gif", ".webp", ".bmp", ".tif", ".tiff", ".heic", ".avif":
			return KindImage
		case ".mp3", ".ogg", ".oga", ".opus", ".wav", ".m4a", ".aac", ".flac":
			return KindAudio
		case ".mp4", ".mov", ".webm", ".mkv", ".avi", ".m4v", ".3gp":
			return KindVideo
		}
	}
	return KindOther
}
