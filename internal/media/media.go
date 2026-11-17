// Package media turns inbound Discord attachments into MCP content.
//
// The gateway hands us a URL plus metadata and no bytes, so every mode that
// wants the payload has to fetch it. Three policies (mirrored from
// --media-image/-audio/-video/-other) decide what a poll does with one
// attachment:
//
//   - skip: no I/O at all, the attachment keeps only its URL in the JSON.
//   - inline: the bytes go into the tool result as an MCP content block
//     (base64), which is the only way the model actually sees a picture.
//   - save: the bytes go to a file under the media directory and the JSON
//     carries the path.
//
// There is deliberately no fallback chain between them. When a policy cannot
// be honoured the attachment reports media:"url" plus a machine-readable
// reason, and the URL is still there — losing information quietly is worse
// than letting the agent decide what a skip meant.
//
// A Session belongs to one poll call and is not safe for concurrent use; the
// Fetcher behind it is stateless and shared by every agent.
package media

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"mcp-discord-roleplay/internal/bus"
	"mcp-discord-roleplay/internal/config"
	"mcp-discord-roleplay/internal/files"

	mcp "mcptransport"
)

// Mode is what the config says should happen to one kind of attachment.
type Mode string

const (
	ModeInline Mode = "inline"
	ModeSave   Mode = "save"
	ModeSkip   Mode = "skip"
)

// Values of Handle.Media, one vocabulary for all three answers: "skip" always
// carries a reason, the other two say what the agent got.
const (
	MediaBase64 = "base64"
	MediaFile   = "file"
	MediaURL    = "url"
)

// Reasons for media:"url" — the answer "we did not hand over any bytes".
// Stable strings so an agent can branch on them.
const (
	SkipPolicy      = "policy"      // the config says skip for this kind
	SkipUnsupported = "unsupported" // MCP has no block for it, or the mode is capped to zero
	SkipTooBig      = "too_big"     // over the per-attachment cap, checked before fetching
	SkipNoBudget    = "no_budget"   // this result's inline count/byte cap is used up
	SkipSide        = "side"        // longest side exceeds --inline-max-side
	SkipSaveOff     = "save_off"    // save asked for but there is no usable --media-out
	SkipError       = "error"       // fetch or write failed; the detail is in the log
)

// defaultTimeout bounds one attachment fetch. It is a per-file budget, not a
// per-poll one, so a slow CDN cannot eat the whole call; --poll-min-wait stays
// the floor for how long the agent waits either way.
const defaultTimeout = 15 * time.Second

// Deps is what New wants. Both fields are optional: a zero Deps means a
// default http.Client and a discard logger.
type Deps struct {
	HTTPClient *http.Client
	Log        *slog.Logger
}

// Fetcher is the shared half: the HTTP client, the policy from the config, and
// the directory saving writes into.
type Fetcher struct {
	cfg *config.Config
	hc  *http.Client
	log *slog.Logger

	dir string // cleaned media directory; "" means saving is off

	// root is an open handle on dir. Every name we write goes through it, so
	// nothing Discord calls a filename can get outside the cache directory
	// (os.Root follows symlinks but never lets them escape, and its methods
	// are safe from several goroutines at once).
	root *os.Root

	// prefix is what we write in front of a saved name when we tell the agent
	// about it (--local-path, or a reading of --media-out when that is left
	// empty). The real directory is the operator's business; this string is
	// the name the agent should be able to hand back to discord_send.
	prefix string

	// secret backs saveName's token: random per process, never on the wire.
	secret []byte
}

// New prepares the process-wide fetcher. It creates the media directory when
// saving is enabled, because that is cheaper to report once at startup than as
// a per-attachment failure later: a bad directory turns save into skip:save_off
// and does not stop the server.
func New(cfg *config.Config, d Deps) *Fetcher {
	f := &Fetcher{cfg: cfg, hc: d.HTTPClient, log: d.Log}
	if f.hc == nil {
		f.hc = &http.Client{Timeout: defaultTimeout}
	}
	if f.log == nil {
		f.log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	f.dir = cleanDir(cfg.MediaOut)
	f.prefix = cfg.LocalPathPrefix()
	f.secret = newSecret()
	if f.dir != "" && cfg.SaveEnabled() && cfg.AnySave() {
		if err := f.openSaveRoot(); err != nil {
			f.log.Warn("media: the save directory is unusable, 'save' will skip",
				"dir", f.dir, "err", err.Error())
			f.dir = ""
		}
	}
	return f
}

// openSaveRoot creates the media directory and keeps a root open on it.
//
// The directory is named by the operator's flag, not by the model, but it is
// still created through a root where that is possible: a relative path (and
// the '/'-prefixed form that means "inside the working directory") goes
// through a root on the working directory, so even the mkdir cannot land
// somewhere unexpected. A genuinely absolute --media-out is the operator
// pointing us elsewhere on purpose — a cwd root would refuse it — so that one
// case uses the plain path call. After this point every file name, including
// the one Discord chose, is resolved inside f.root.
func (f *Fetcher) openSaveRoot() error {
	if filepath.IsAbs(filepath.FromSlash(f.dir)) {
		if err := os.MkdirAll(f.dir, 0o750); err != nil {
			return err
		}
	} else {
		cwd, err := os.OpenRoot(".")
		if err != nil {
			return err
		}
		defer cwd.Close()
		if err := cwd.MkdirAll(f.dir, 0o750); err != nil {
			return err
		}
	}
	root, err := os.OpenRoot(f.dir)
	if err != nil {
		return err
	}
	f.root = root
	return nil
}

// Close releases the directory handle. Safe to call on a fetcher that never
// opened one.
func (f *Fetcher) Close() error {
	if f == nil || f.root == nil {
		return nil
	}
	err := f.root.Close()
	f.root = nil
	return err
}

// Dir reports where save writes, "" when saving is off.
func (f *Fetcher) Dir() string { return f.dir }

// Ready reports whether 'save' can actually be honoured: the cache directory is
// open through its root. Dir() can name a directory we never opened (no kind is
// set to save, or --media-save-max-bytes turned the mode off), so the status
// line that answers "why is everything save_off" has to ask this instead.
func (f *Fetcher) Ready() bool { return f != nil && f.root != nil }

// Prefix reports the alias a saved file's name carries on its way to the
// agent: "" means the bare name is used.
func (f *Fetcher) Prefix() string { return f.prefix }

// Handle is the outcome for one attachment: what the agent got and nothing
// else. Error details go to the log, not to the wire.
//
// It carries no JSON tags on purpose: the caller copies these fields into its
// own wire struct, because the field names and defaults a tool result uses
// (omitempty everywhere) are the tool's business, not this package's.
type Handle struct {
	// Media is MediaBase64, MediaFile or MediaURL. MediaURL always comes with a
	// Reason; the other two never do.
	Media  string
	Reason string
	// Bytes is what was actually fetched or written. The attachment's own
	// metadata size may disagree (or be 0); both are reported because the
	// disagreement is information.
	Bytes int64
	// Path is the name we hand the agent for the saved file: the prefix from
	// --local-path (see Fetcher.Prefix) and the file name. It is written the
	// way discord_send takes a path, which is the whole point of the prefix —
	// the agent can send the file back without knowing where it really lives.
	Path string
	// ContentIndex is this attachment's block position WITHIN THE SESSION, and
	// -1 when there is no block (file/url answers). The caller has to offset it
	// to get the real position in the tool result, because that array starts
	// with the caller's own text block — 0 here is a legitimate first block.
	ContentIndex int
}

// Session is one poll's working state: what it has inlined so far and the
// content array it is appending to.
type Session struct {
	f   *Fetcher
	log *slog.Logger
	ctx context.Context

	content []mcp.ContentPart

	inlined      int
	inlinedBytes int64
}

// NewSession starts a per-poll session. The caller defers Close.
func (f *Fetcher) NewSession(ctx context.Context) *Session {
	return &Session{f: f, log: f.log, ctx: ctx, content: []mcp.ContentPart{}}
}

// Content returns the blocks collected so far; the text part belongs to the
// caller and is normally prepended.
func (s *Session) Content() []mcp.ContentPart { return s.content }

// Close releases nothing today — the fetcher owns the client — but it keeps
// the call site honest and gives cleanup a place to grow.
func (s *Session) Close() error { return nil }

// Handle applies the policy for a.Kind and does whatever it asks for.
func (s *Session) Handle(a bus.Attachment) Handle {
	switch s.f.cfg.MediaFor(a.Kind) {
	case config.MediaMode(ModeInline):
		return s.inline(a)
	case config.MediaMode(ModeSave):
		return s.save(a)
	default:
		return skip(SkipPolicy)
	}
}

// inline fetches the file and appends it as a content block. Only image and
// audio have a block type in MCP at all (mcptransport/content.go emits data
// for nothing else), so a video set to inline is reported as unsupported
// instead of quietly becoming something else.
func (s *Session) inline(a bus.Attachment) Handle {
	if !blockable(a.Kind) {
		return skip(SkipUnsupported)
	}
	if !s.f.cfg.CanInline() {
		return skip(SkipUnsupported)
	}
	if r := s.checkCaps(a); r != "" {
		return skip(r)
	}
	resp, err := s.get(a)
	if err != nil {
		s.log.Warn("media: inline fetch failed", "id", a.ID, "kind", a.Kind, "err", err.Error())
		return skip(SkipError)
	}
	defer resp.Body.Close()
	raw, err := readCapped(resp.Body, s.readCap())
	if err != nil {
		if errors.Is(err, errTooBig) {
			return skip(SkipTooBig)
		}
		s.log.Warn("media: inline read failed", "id", a.ID, "err", err.Error())
		return skip(SkipError)
	}
	s.inlined++
	s.inlinedBytes += int64(len(raw))
	s.content = append(s.content, block(a.Kind, base64.StdEncoding.EncodeToString(raw), pickMIME(a, raw)))
	return Handle{
		Media:        MediaBase64,
		Bytes:        int64(len(raw)),
		ContentIndex: len(s.content) - 1,
	}
}

// save writes the file into the media directory under an unguessable name
// (see saveName). The copy is streamed, so saving does not hold the whole
// file in memory.
func (s *Session) save(a bus.Attachment) Handle {
	if s.f.root == nil {
		return skip(SkipSaveOff)
	}
	lim := int64(s.f.cfg.MediaSaveMaxBytes)
	if !config.Unlimited(s.f.cfg.MediaSaveMaxBytes) && int64(a.Size) > lim {
		return skip(SkipTooBig)
	}
	name := saveName(a, s.f.secret)
	dst := path.Join(s.f.dir, name) // for the log only: writes go through the root

	// The name is deterministic inside one process, so a second look at the same
	// attachment finds the file we wrote the first time: hand that back instead of
	// downloading it again. A zero-length leftover (a crash mid-copy) is not
	// trusted and gets rewritten.
	if fi, err := fs.Stat(s.f.root.FS(), name); err == nil && fi.Mode().IsRegular() && fi.Size() > 0 {
		return Handle{Media: MediaFile, Bytes: fi.Size(), Path: joinPrefix(s.f.prefix, name), ContentIndex: -1}
	}
	resp, err := s.get(a)
	if err != nil {
		s.log.Warn("media: save fetch failed", "id", a.ID, "err", err.Error())
		return skip(SkipError)
	}
	defer resp.Body.Close()
	f, err := s.f.root.OpenFile(name, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o640)
	if err != nil {
		s.log.Warn("media: save open failed", "path", dst, "err", err.Error())
		return skip(SkipError)
	}
	var room int64 = -1 // -1 tells LimitReader to stop limiting
	if lim >= 0 {
		room = lim + 1
	}
	written, err := io.Copy(f, io.LimitReader(resp.Body, room))
	if err == nil && lim >= 0 && written > lim {
		err = errTooBig
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		if rmErr := s.f.root.Remove(name); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
			s.log.Warn("media: could not clean up a failed save", "path", dst, "err", rmErr.Error())
		}
		if errors.Is(err, errTooBig) {
			return skip(SkipTooBig)
		}
		s.log.Warn("media: save write failed", "path", dst, "err", err.Error())
		return skip(SkipError)
	}
	return Handle{Media: MediaFile, Bytes: written, Path: joinPrefix(s.f.prefix, name), ContentIndex: -1}
}

// joinPrefix puts the declared alias in front of a file name without ever
// producing a doubled slash, and leaves the bare name when there is no alias.
func joinPrefix(prefix, name string) string {
	if prefix == "" {
		return name
	}
	return prefix + "/" + name
}

// checkCaps runs the inline gates that metadata alone can answer, so we do not
// spend a fetch on a file we would throw away. A size of 0 (unknown) passes
// every size gate: we only refuse what we can prove is too big.
func (s *Session) checkCaps(a bus.Attachment) string {
	cfg := s.f.cfg
	if !config.Unlimited(cfg.InlineMaxBytesImage) && int64(a.Size) > int64(cfg.InlineMaxBytesImage) {
		return SkipTooBig
	}
	if side := cfg.InlineMaxSide; !config.Unlimited(side) && a.Width > 0 && a.Height > 0 {
		if max(a.Width, a.Height) > side {
			return SkipSide
		}
	}
	if lim := cfg.InlineMaxCount; !config.Unlimited(lim) && s.inlined >= lim {
		return SkipNoBudget
	}
	if lim := cfg.InlineMaxTotalBytes; !config.Unlimited(lim) && s.inlinedBytes >= int64(lim) {
		return SkipNoBudget
	}
	return ""
}

// readCap is how many bytes an inline fetch may pull: the per-image cap and the
// room left under the total cap, whichever bites first. -1 reads to the end,
// which is what "no limit" costs you.
func (s *Session) readCap() int64 {
	var per, total int64 = -1, -1
	if !config.Unlimited(s.f.cfg.InlineMaxBytesImage) {
		per = int64(s.f.cfg.InlineMaxBytesImage)
	}
	if !config.Unlimited(s.f.cfg.InlineMaxTotalBytes) {
		total = int64(s.f.cfg.InlineMaxTotalBytes) - s.inlinedBytes
	}
	return capMin(per, total)
}

var errTooBig = errors.New("media: over the size cap")

// get asks for an attachment, falling back to the proxy URL once when the
// first attempt fails. Callers close the body.
func (s *Session) get(a bus.Attachment) (*http.Response, error) {
	if a.URL == "" {
		return nil, errors.New("media: attachment has no url")
	}
	resp, err := s.open(a.URL)
	if err == nil {
		return resp, nil
	}
	if a.ProxyURL == "" || !sameHost(a.URL, a.ProxyURL) {
		return nil, err
	}
	s.log.Debug("media: retrying on the proxy url", "id", a.ID, "err", err.Error())
	return s.open(a.ProxyURL)
}

func (s *Session) open(rawURL string) (*http.Response, error) {
	ctx := s.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, defaultTimeout)
		defer cancel()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := s.f.hc.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("media: %s returned %s", rawURL, resp.Status)
	}
	return resp, nil
}

// readCapped reads at most n bytes (-1 = no limit) and reports errTooBig when
// there was more than that behind the reader.
func readCapped(r io.Reader, n int64) ([]byte, error) {
	if n < 0 {
		return io.ReadAll(r)
	}
	raw, err := io.ReadAll(io.LimitReader(r, n+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > n {
		return nil, errTooBig
	}
	return raw, nil
}

// saveName builds the on-disk name: <8 hex>-<safe filename>.
//
// The token is HMAC-shaped — a per-process secret mixed with the attachment ID
// and the sanitized name — which buys two things at once. It is not guessable
// from the outside: knowing that a picture was called photo.png does not tell
// another agent (or the model) which file to open. And it is stable inside one
// process, so seeing the same attachment twice overwrites the same file
// instead of leaving a trail of copies behind.
func saveName(a bus.Attachment, secret []byte) string {
	base := files.SafeName(a.Filename)
	h := sha256.New()
	h.Write(secret)
	h.Write([]byte{0})
	h.Write([]byte(a.ID))
	h.Write([]byte{0})
	h.Write([]byte(base))
	return hex.EncodeToString(h.Sum(nil)[:4]) + "-" + base
}

// newSecret draws the bytes behind that token. crypto/rand does not fail in
// practice; if it ever did we would rather have a weaker name than a dead
// server, so the fallback is the clock.
func newSecret() []byte {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return []byte(time.Now().String())
	}
	return b
}

// block picks the MCP content constructor for a kind.
func block(kind, data, mimeStr string) mcp.ContentPart {
	if kind == bus.KindAudio {
		return mcp.Audio(data, mimeStr)
	}
	return mcp.Image(data, mimeStr)
}

// blockable reports whether MCP has a content block for this kind at all.
func blockable(kind string) bool {
	return kind == bus.KindImage || kind == bus.KindAudio
}

func skip(reason string) Handle {
	return Handle{Media: MediaURL, Reason: reason, ContentIndex: -1}
}

// pickMIME decides what mimeType an inlined block carries. Discord's own
// Content-Type wins; a placeholder octet-stream does not, because that is what
// they say when they did not look.
func pickMIME(a bus.Attachment, raw []byte) string {
	ct := strings.TrimSpace(a.ContentType)
	if ct != "" && ct != "application/octet-stream" {
		return ct
	}
	if len(raw) > 0 {
		if s := http.DetectContentType(raw); s != "" && s != "text/plain; charset=utf-8" {
			return s
		}
	}
	if m := mime.TypeByExtension(strings.ToLower(path.Ext(a.Filename))); m != "" {
		return m
	}
	if ct != "" {
		return ct
	}
	return "application/octet-stream"
}

func capMin(a, b int64) int64 {
	if a < 0 {
		return b
	}
	if b < 0 {
		return a
	}
	if a < b {
		return a
	}
	return b
}

// cleanDir turns a config path into something the os package can use: slashes,
// cleaned, and a leading "/" read as "inside the working directory" — the same
// way discord_send has read attachment paths since M2a.
func cleanDir(p string) string {
	p = strings.TrimSpace(filepath.ToSlash(p))
	if p == "" {
		return ""
	}
	if filepath.IsAbs(filepath.FromSlash(p)) {
		return path.Clean(p)
	}
	return path.Clean(strings.TrimPrefix(p, "/"))
}

// sameHost guards the proxy fallback: proxy_url is a different CDN host than
// media_url, and we do not want to follow a configured host to an arbitrary
// one because a record happened to carry both fields.
func sameHost(a, b string) bool {
	ua, ea := url.Parse(a)
	ub, eb := url.Parse(b)
	if ea != nil || eb != nil {
		return false
	}
	return ua.Host != "" && ua.Host == ub.Host
}
