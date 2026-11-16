// Package bus is the in-memory event store the poll tool reads from.
//
// It is a fixed-size ring of normalized Discord events plus a broadcast
// channel. The gateway handler only ever calls Publish, which never blocks and
// never performs I/O — that is a hard requirement, because a slow handler in
// the Discord websocket read loop stalls heartbeats.
package bus

import (
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Event kinds.
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

// Author is the normalized sender.
type Author struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Tag  string `json:"tag,omitempty"`
	Bot  bool   `json:"bot,omitempty"`
	Self bool   `json:"self,omitempty"`
}

// Attachment is a normalized file attached to a message. The Media field
// records what the poll tool decided to do with it.
type Attachment struct {
	ID           string  `json:"id"`
	Filename     string  `json:"filename,omitempty"`
	ContentType  string  `json:"content_type,omitempty"`
	Kind         string  `json:"kind"` // image|audio|video|other
	Size         int     `json:"size,omitempty"`
	Width        int     `json:"width,omitempty"`
	Height       int     `json:"height,omitempty"`
	DurationSecs float64 `json:"duration_secs,omitempty"`
	URL          string  `json:"url"`
	ProxyURL     string  `json:"proxy_url,omitempty"`
	Media        string  `json:"media"` // inline|save|skip|skipped:<reason>
	LocalPath    string  `json:"local_path,omitempty"`
	ContentIndex int     `json:"content_index,omitempty"` // index into result content[]
}

// Embed is a minimal view of a Discord embed (rich cards are flattened).
type Embed struct {
	Type        string `json:"type,omitempty"`
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
	URL         string `json:"url,omitempty"`
}

// Event is one stored item. Seq is monotonically increasing and is what
// cursors refer to.
type Event struct {
	Seq  uint64    `json:"seq"`
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

	Text         string `json:"text"`
	Truncated    bool   `json:"truncated,omitempty"`
	MentionsSelf bool   `json:"mentions_self,omitempty"`

	Attachments []Attachment   `json:"attachments,omitempty"`
	Embeds      []Embed        `json:"embeds,omitempty"`
	Extra       map[string]any `json:"extra,omitempty"`
}

// Stats is a snapshot for the status tool.
type Stats struct {
	Size     int    `json:"size"`
	Capacity int    `json:"capacity"`
	Total    uint64 `json:"total"`
	Dropped  uint64 `json:"dropped"`
	MaxSeq   uint64 `json:"max_seq"`
	LastSeq  uint64 `json:"last_seq"`
}

// Bus holds the ring and broadcasts arrivals.
type Bus struct {
	mu   sync.Mutex
	cap  int
	ring []*Event
	next int // write cursor (index of the oldest slot when full)
	size int

	total   uint64
	dropped uint64
	maxSeq  uint64

	notify chan struct{}
}

// New returns a Bus that keeps the last cap events.
func New(cap int) *Bus {
	if cap < 1 {
		cap = 1
	}
	return &Bus{
		cap:    cap,
		ring:   make([]*Event, cap),
		notify: make(chan struct{}),
	}
}

// Publish assigns the next sequence number, stores e (overwriting the oldest
// entry when full) and wakes every waiting poll. It never blocks.
func (b *Bus) Publish(e *Event) uint64 {
	if e == nil {
		return 0
	}
	b.mu.Lock()
	b.maxSeq++
	e.Seq = b.maxSeq
	b.total++
	b.ring[b.next] = e
	b.next = (b.next + 1) % b.cap
	if b.size < b.cap {
		b.size++
	} else {
		b.dropped++
	}
	close(b.notify)
	b.notify = make(chan struct{})
	b.mu.Unlock()
	return e.Seq
}

// Notify returns a channel closed on the next published event. Re-read it after
// each wake (the previous channel is already closed).
func (b *Bus) Notify() <-chan struct{} {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.notify
}

// After returns up to limit events with Seq > seq that pass filter, oldest
// first, plus the highest Seq among the returned events (0 when none). The
// caller uses that as its next cursor, so events beyond limit stay unread.
func (b *Bus) After(seq uint64, filter func(*Event) bool, limit int) ([]*Event, uint64) {
	if limit <= 0 {
		limit = 1
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	start := 0
	if b.size == b.cap {
		start = b.next // oldest entry when the ring has wrapped
	}
	out := make([]*Event, 0, minInt(limit, b.size))
	last := uint64(0)
	for i := 0; i < b.size; i++ {
		e := b.ring[(start+i)%b.cap]
		if e == nil || e.Seq <= seq {
			continue
		}
		if filter != nil && !filter(e) {
			continue
		}
		out = append(out, e)
		last = e.Seq
		if len(out) >= limit {
			break
		}
	}
	return out, last
}

// Latest returns up to limit stored events that pass filter, oldest first,
// keeping the NEWEST ones when more than limit match (ignores cursors).
func (b *Bus) Latest(filter func(*Event) bool, limit int) []*Event {
	if limit <= 0 {
		limit = 1
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	start := 0
	if b.size == b.cap {
		start = b.next
	}
	out := make([]*Event, 0, minInt(limit, b.size))
	for i := 0; i < b.size; i++ {
		e := b.ring[(start+i)%b.cap]
		if e == nil || (filter != nil && !filter(e)) {
			continue
		}
		out = append(out, e)
		if len(out) > limit {
			out = out[len(out)-limit:]
		}
	}
	return out
}

// Stats returns the current counters.
func (b *Bus) Stats() Stats {
	b.mu.Lock()
	defer b.mu.Unlock()
	last := uint64(0)
	if b.size > 0 {
		i := (b.next - 1 + b.cap) % b.cap
		if e := b.ring[i]; e != nil {
			last = e.Seq
		}
	}
	return Stats{
		Size: b.size, Capacity: b.cap, Total: b.total,
		Dropped: b.dropped, MaxSeq: b.maxSeq, LastSeq: last,
	}
}

// Unread counts stored events with Seq > seq that pass filter (capped at limit
// so it stays cheap).
func (b *Bus) Unread(seq uint64, filter func(*Event) bool, limit int) int {
	evs, _ := b.After(seq, filter, limit)
	return len(evs)
}

// Cursor renders a sequence number as an opaque cursor string. Seq 0 means
// "nothing consumed yet" and is written as the empty string, so a caller can
// simply echo back whatever it was given.
func Cursor(seq uint64) string {
	if seq == 0 {
		return ""
	}
	return "s" + strconv.FormatUint(seq, 10)
}

// ParseCursor is the inverse of Cursor. ok is false for empty/garbage input.
func ParseCursor(s string) (uint64, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	s = strings.TrimPrefix(s, "s")
	n, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

// ClassifyMedia guesses the media kind of an attachment from its content type,
// falling back to the filename extension.
func ClassifyMedia(filename, contentType string) string {
	ct := strings.ToLower(strings.TrimSpace(contentType))
	switch {
	case strings.HasPrefix(ct, "image/"):
		return KindImage
	case strings.HasPrefix(ct, "audio/"):
		return KindAudio
	case strings.HasPrefix(ct, "video/"):
		return KindVideo
	}
	switch strings.ToLower(filepath.Ext(filename)) {
	case ".png", ".jpg", ".jpeg", ".gif", ".webp", ".bmp", ".avif", ".tif", ".tiff", ".heic", ".svg":
		return KindImage
	case ".mp3", ".ogg", ".oga", ".wav", ".m4a", ".aac", ".flac", ".opus", ".amr":
		return KindAudio
	case ".mp4", ".mov", ".webm", ".mkv", ".avi", ".m4v", ".3gp", ".ts":
		return KindVideo
	}
	return KindOther
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
