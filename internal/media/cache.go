package media

// The in-memory CDN cache, and why only two kinds of attachment are in it.
//
// An emoji or a sticker we synthesized lives at a URL built from its snowflake id
// (see internal/discord/emoji.go), and Discord never replaces the bytes behind an
// id: renaming an emoji keeps the name and produces a new id for the old one, and
// re-uploading a picture gives it a fresh id too. So "same URL, same answer" is
// true here in a way it is not true of an uploaded attachment, whose URL is a path
// on a CDN that can be re-served. That, plus the fact that these files are small
// by policy (an emoji is capped at 128px and a few hundred KB), is what makes a
// plain map of bytes worth keeping.
//
// Only the inline path fills it. Saving has its own reuse check that costs no
// memory — a deterministic file name whose file, if present, is the answer — and
// it streams the body anyway, so caching on the way to disk would mean buffering
// a file for someone who is not going to read it from here.

import (
	"bytes"
	"container/list"
	"io"
	"net/http"
	"sync"

	"mcp-discord-roleplay/internal/bus"
)

const (
	// cacheMaxEntries and cacheMaxBytes bound the whole cache. The numbers are
	// sized for emoji and stickers, not for photographs: a few hundred of them is
	// a couple of megabytes, and an agent that needs more than that is the signal
	// to move this onto disk (the --emoji-cache idea on the backlog).
	cacheMaxEntries = 256
	cacheMaxBytes   = 8 << 20

	// cacheMaxEntryBytes is what one file may cost to sit here. Anything bigger
	// than an emoji or a sticker is not the thing this cache is for, so we drop it
	// rather than let it push the useful entries out.
	cacheMaxEntryBytes = 512 << 10
)

// cdnCache is a small LRU keyed by URL. The Fetcher that owns it is shared by
// every agent and every poll, so the lock is real: two agents polling at the same
// moment can reach the same emoji concurrently.
type cdnCache struct {
	mu    sync.Mutex
	max   int
	items map[string]*list.Element
	order *list.List // front = most recently used
	bytes int
}

type cacheEntry struct {
	url  string
	raw  []byte
	mime string
}

func newCDNCache() *cdnCache {
	return &cdnCache{
		max:   cacheMaxEntries,
		items: make(map[string]*list.Element, cacheMaxEntries),
		order: list.New(),
	}
}

// cacheable is the eligibility question: only files we synthesized from an id,
// and only ones small enough to belong here.
func cacheable(a bus.Attachment, n int) bool {
	return a.Origin != "" && a.URL != "" && n > 0 && n <= cacheMaxEntryBytes
}

// get returns the bytes we are holding for a, if any. A non-cacheable attachment
// is not an error, it simply never reaches the map.
func (c *cdnCache) get(a bus.Attachment) ([]byte, string, bool) {
	if c == nil || a.Origin == "" || a.URL == "" {
		return nil, "", false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.items[a.URL]
	if !ok {
		return nil, "", false
	}
	c.order.MoveToFront(el)
	e := el.Value.(*cacheEntry)
	return e.raw, e.mime, true
}

// put stores a fetch result. Files that are too big to belong here are ignored,
// not remembered as misses: the next poll will just download them again, which is
// what it would have done with no cache at all.
func (c *cdnCache) put(a bus.Attachment, raw []byte, mime string) {
	if c == nil || !cacheable(a, len(raw)) {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[a.URL]; ok {
		e := el.Value.(*cacheEntry)
		c.bytes += len(raw) - len(e.raw)
		e.raw, e.mime = raw, mime
		c.order.MoveToFront(el)
	} else {
		el := c.order.PushFront(&cacheEntry{url: a.URL, raw: raw, mime: mime})
		c.items[a.URL] = el
		c.bytes += len(raw)
	}
	// Evict by count first and then by weight, either of which can be the one
	// that bites: 256 entries of 512 KB would be far past the byte budget.
	for c.order.Len() > c.max || c.bytes > cacheMaxBytes {
		back := c.order.Back()
		if back == nil {
			break
		}
		e := c.order.Remove(back).(*cacheEntry)
		delete(c.items, e.url)
		c.bytes -= len(e.raw)
	}
}

// entries is for the log and the tests: how many files are held right now.
func (c *cdnCache) entries() int {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.order.Len()
}

// cachedResponse wraps stored bytes in the one shape get's callers look at: a 200
// with a readable body. Nothing reads more of it — the status line was already
// checked by whoever filled the cache, and the callers only drain the body.
func cachedResponse(raw []byte, mime string) *http.Response {
	h := http.Header{}
	if mime != "" {
		h.Set("Content-Type", mime)
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     h,
		Body:       io.NopCloser(bytes.NewReader(raw)),
	}
}
