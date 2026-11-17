package media

// Tests for the three policies against a fake CDN. What is under test is not
// the HTTP plumbing but the promises: inline really puts bytes into a content
// block, save really writes one file and never a second copy of it, skip costs
// zero requests, and nothing ever degrades into another mode behind the
// agent's back — an agent that asked for pixels and got a URL must be able to
// tell, and so must the request counter.

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"mcp-discord-roleplay/internal/bus"
	"mcp-discord-roleplay/internal/config"
)

func testCfg(t *testing.T, args ...string) *config.Config {
	t.Helper()
	// -selftest is here for one reason: finalize() refuses to build a Config
	// without a bot token unless selftest (or -version) was asked for, and a
	// unit test has no business reading the developer's environment. It changes
	// nothing about the media policy under test.
	cfg, err := config.Load(append([]string{"-selftest"}, args...))
	if err != nil {
		t.Fatalf("config.Load(%v): %v", args, err)
	}
	return cfg
}

// cdn serves one fixed payload at every path except /404, and counts requests,
// so a test can tell "we looked at the metadata and decided" apart from "we
// downloaded it and then decided".
type cdn struct {
	srv  *httptest.Server
	hits atomic.Int64
}

func newCDN(t *testing.T, body []byte) *cdn {
	t.Helper()
	c := &cdn{}
	c.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.hits.Add(1)
		if r.URL.Path == "/404" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(body)
	}))
	t.Cleanup(c.srv.Close)
	return c
}

func (c *cdn) at(p string) string { return c.srv.URL + p }
func (c *cdn) count() int64       { return c.hits.Load() }

// fetcherFor builds the Fetcher a test needs and closes it afterwards.
func fetcherFor(t *testing.T, cfg *config.Config) *Fetcher {
	t.Helper()
	f := New(cfg, Deps{})
	t.Cleanup(func() { _ = f.Close() })
	return f
}

func png(id, url string, size int) bus.Attachment {
	return bus.Attachment{
		ID: id, Filename: "shot.png", ContentType: "image/png",
		Kind: bus.KindImage, Size: size, URL: url,
	}
}

func TestInlineProducesAnImageBlock(t *testing.T) {
	body := []byte{0x89, 'P', 'N', 'G', 1, 2, 3, 4}
	c := newCDN(t, body)
	s := fetcherFor(t, testCfg(t, "-media-image=inline")).NewSession(context.Background())

	h := s.Handle(png("a1", c.at("/shot.png"), len(body)))
	if h.Media != MediaBase64 {
		t.Fatalf("inline must answer %q, got %q (reason %q)", MediaBase64, h.Media, h.Reason)
	}
	if h.Reason != "" || h.Path != "" {
		t.Errorf("an inlined attachment carries no reason and no path: %+v", h)
	}
	if h.Bytes != int64(len(body)) {
		t.Errorf("bytes = %d, want %d", h.Bytes, len(body))
	}
	if h.ContentIndex != 0 {
		t.Errorf("the session's first block is index 0 (the caller offsets it), got %d", h.ContentIndex)
	}

	blocks := s.Content()
	if len(blocks) != 1 {
		t.Fatalf("one image must produce exactly one content block, got %d", len(blocks))
	}
	if blocks[0].Type != "image" {
		t.Errorf("a KindImage must become an image block, got %q", blocks[0].Type)
	}
	if blocks[0].Data != base64.StdEncoding.EncodeToString(body) {
		t.Errorf("block data is not the payload: %q", blocks[0].Data)
	}
	if blocks[0].MimeType != "image/png" {
		t.Errorf("mime should come from Discord's content-type, got %q", blocks[0].MimeType)
	}
	if c.count() != 1 {
		t.Errorf("one attachment, one download: %d", c.count())
	}
}

// A side that is too long is decided from metadata, so it must not cost a
// request — and it must NOT be downgraded to a save or retried smaller: we do
// not resize, and we do not change the operator's policy.
func TestInlineSkipsABigSideWithoutFetching(t *testing.T) {
	c := newCDN(t, make([]byte, 8))
	s := fetcherFor(t, testCfg(t, "-media-image=inline", "-inline-max-side=100")).NewSession(context.Background())

	a := png("a1", c.at("/shot.png"), 8)
	a.Width, a.Height = 400, 300
	h := s.Handle(a)
	if h.Media != MediaURL || h.Reason != SkipSide {
		t.Fatalf("want %q/%q, got %q/%q", MediaURL, SkipSide, h.Media, h.Reason)
	}
	if len(s.Content()) != 0 || c.count() != 0 {
		t.Errorf("a skip decided by metadata must not download anything: blocks=%d hits=%d",
			len(s.Content()), c.count())
	}
}

func TestInlineOverPerImageCapIsTooBig(t *testing.T) {
	c := newCDN(t, make([]byte, 8))
	s := fetcherFor(t, testCfg(t, "-media-image=inline", "-inline-max-bytes-image=10")).NewSession(context.Background())

	h := s.Handle(png("a1", c.at("/shot.png"), 999))
	if h.Media != MediaURL || h.Reason != SkipTooBig {
		t.Fatalf("want %q/%q, got %q/%q", MediaURL, SkipTooBig, h.Media, h.Reason)
	}
	if c.count() != 0 {
		t.Errorf("Discord already told us the size; do not download a file we will throw away")
	}
}

// The count cap is per result, not per process: the third picture in one poll
// is the one that runs out of room.
func TestInlineCountCapIsPerResult(t *testing.T) {
	c := newCDN(t, make([]byte, 8))
	s := fetcherFor(t, testCfg(t, "-media-image=inline", "-inline-max-count=2")).NewSession(context.Background())

	for i, id := range []string{"a1", "a2", "a3"} {
		h := s.Handle(png(id, c.at("/"+id), 8))
		want := MediaBase64
		if i == 2 {
			want = MediaURL
		}
		if h.Media != want {
			t.Fatalf("%s: want %q, got %q/%q", id, want, h.Media, h.Reason)
		}
		if i == 2 && h.Reason != SkipNoBudget {
			t.Errorf("the third one must say why it ran out: %q", h.Reason)
		}
	}
	if c.count() != 2 {
		t.Errorf("only the two that fit may be downloaded, got %d", c.count())
	}
	if got := len(s.Content()); got != 2 {
		t.Errorf("blocks = %d, want 2", got)
	}
}

// MCP has no video content block at all (mcptransport only emits data for
// image and audio), so inline video is not "expensive", it is impossible.
func TestInlineVideoIsUnsupported(t *testing.T) {
	c := newCDN(t, make([]byte, 8))
	s := fetcherFor(t, testCfg(t, "-media-video=inline")).NewSession(context.Background())

	a := png("a1", c.at("/shot.png"), 8)
	a.Kind = bus.KindVideo
	h := s.Handle(a)
	if h.Media != MediaURL || h.Reason != SkipUnsupported {
		t.Fatalf("want %q/%q, got %q/%q", MediaURL, SkipUnsupported, h.Media, h.Reason)
	}
	if len(s.Content()) != 0 || c.count() != 0 {
		t.Errorf("an unsupported block must not be paid for: blocks=%d hits=%d", len(s.Content()), c.count())
	}
}

func TestSkipPolicyTouchesNothing(t *testing.T) {
	c := newCDN(t, make([]byte, 8))
	s := fetcherFor(t, testCfg(t, "-media-audio=skip")).NewSession(context.Background())

	a := png("a1", c.at("/clip.opus"), 8)
	a.Kind, a.Filename = bus.KindAudio, "clip.opus"
	h := s.Handle(a)
	if h.Media != MediaURL || h.Reason != SkipPolicy {
		t.Fatalf("want %q/%q, got %q/%q", MediaURL, SkipPolicy, h.Media, h.Reason)
	}
	if c.count() != 0 {
		t.Errorf("skip means zero I/O, got %d requests", c.count())
	}
}

// The round trip the whole design rests on: the name poll hands out must be a
// file that exists, and it must be the same name the next time the same
// attachment shows up.
func TestSaveWritesOneFileAndReusesIt(t *testing.T) {
	dir := t.TempDir()
	body := []byte("a picture worth a thousand tokens")
	c := newCDN(t, body)
	f := fetcherFor(t, testCfg(t, "-media-image=save", "-media-out="+dir, "-local-path=/dc-cache"))

	a := png("a1", c.at("/shot.png"), len(body))
	h1 := f.NewSession(context.Background()).Handle(a)
	if h1.Media != MediaFile || h1.Reason != "" {
		t.Fatalf("want %q, got %q/%q", MediaFile, h1.Media, h1.Reason)
	}
	if !strings.HasPrefix(h1.Path, "/dc-cache/") || !strings.HasSuffix(h1.Path, "-shot.png") {
		t.Fatalf("local_path must be the declared prefix plus the token name, got %q", h1.Path)
	}
	base := filepath.Base(h1.Path)
	if len(base) < 10 || base[8] != '-' {
		t.Fatalf("name must be <8 hex>-<safe name>, got %q", base)
	}
	if _, err := hex.DecodeString(base[:8]); err != nil {
		t.Errorf("the token is 8 hex chars, %q is not: %v", base[:8], err)
	}
	if h1.Bytes != int64(len(body)) {
		t.Errorf("bytes = %d, want %d", h1.Bytes, len(body))
	}

	raw, err := os.ReadFile(filepath.Join(dir, base))
	if err != nil {
		t.Fatalf("the file behind local_path must exist: %v", err)
	}
	if string(raw) != string(body) {
		t.Errorf("saved %d bytes, want the payload back", len(raw))
	}

	// A second poll — another call, even another agent — finds it and does not
	// ask the CDN again.
	h2 := f.NewSession(context.Background()).Handle(a)
	if h2.Path != h1.Path {
		t.Errorf("the name must be stable inside one process: %q then %q", h1.Path, h2.Path)
	}
	if h2.Media != MediaFile {
		t.Errorf("a reused file is still a file, got %q/%q", h2.Media, h2.Reason)
	}
	if c.count() != 1 {
		t.Errorf("the second look must not re-download, requests = %d", c.count())
	}

	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 1 {
		t.Errorf("one attachment is one file, got %d entries", len(ents))
	}
}

func TestSaveOverCapIsRefusedBeforeFetching(t *testing.T) {
	dir := t.TempDir()
	c := newCDN(t, make([]byte, 8))
	s := fetcherFor(t, testCfg(t,
		"-media-image=save", "-media-out="+dir, "-media-save-max-bytes=100")).NewSession(context.Background())

	h := s.Handle(png("a1", c.at("/shot.png"), 5000))
	if h.Media != MediaURL || h.Reason != SkipTooBig {
		t.Fatalf("want %q/%q, got %q/%q", MediaURL, SkipTooBig, h.Media, h.Reason)
	}
	if c.count() != 0 {
		t.Errorf("a file we may not keep must not be fetched, got %d requests", c.count())
	}
}

// --media-save-max-bytes=0 turns the save mode off, and then 'save' has to say
// so instead of failing mysteriously.
func TestSaveOffReportsSaveOff(t *testing.T) {
	dir := t.TempDir()
	c := newCDN(t, make([]byte, 8))
	s := fetcherFor(t, testCfg(t,
		"-media-image=save", "-media-out="+dir, "-media-save-max-bytes=0")).NewSession(context.Background())

	h := s.Handle(png("a1", c.at("/shot.png"), 8))
	if h.Media != MediaURL || h.Reason != SkipSaveOff {
		t.Fatalf("want %q/%q, got %q/%q", MediaURL, SkipSaveOff, h.Media, h.Reason)
	}
	if c.count() != 0 {
		t.Errorf("no root, no download, got %d requests", c.count())
	}
}

// proxy_url is a second chance at the SAME host only: that is the one case
// where a retry cannot turn our fetch into a redirect we did not choose.
func TestProxyURLFallbackSameHost(t *testing.T) {
	body := []byte{1, 2, 3}
	c := newCDN(t, body)
	s := fetcherFor(t, testCfg(t, "-media-image=inline")).NewSession(context.Background())

	a := png("a1", c.at("/404"), len(body))
	a.ProxyURL = c.at("/proxy/shot.png")
	h := s.Handle(a)
	if h.Media != MediaBase64 {
		t.Fatalf("the proxy URL should have rescued this fetch, got %q/%q", h.Media, h.Reason)
	}
	if c.count() != 2 {
		t.Errorf("want one failed attempt plus the proxy, got %d", c.count())
	}
}

func TestProxyURLNotFollowedOffHost(t *testing.T) {
	c := newCDN(t, []byte{1, 2, 3})
	s := fetcherFor(t, testCfg(t, "-media-image=inline")).NewSession(context.Background())

	a := png("a1", "http://127.0.0.1:1/gone.png", 3) // nothing listens on 1
	a.ProxyURL = c.at("/shot.png")
	h := s.Handle(a)
	if h.Media != MediaURL || h.Reason != SkipError {
		t.Fatalf("an off-host proxy URL must not be fetched, got %q/%q", h.Media, h.Reason)
	}
	if c.count() != 0 {
		t.Errorf("the CDN was never asked, got %d requests", c.count())
	}
}

func TestSaveNameIsStableAndNotGuessable(t *testing.T) {
	a := bus.Attachment{ID: "a1", Filename: "../../etc/passwd"}
	n1 := saveName(a, []byte("secret-one"))
	n2 := saveName(a, []byte("secret-one"))
	n3 := saveName(a, []byte("secret-two"))

	if n1 != n2 {
		t.Errorf("the same attachment must name the same file: %q vs %q", n1, n2)
	}
	if n1 == n3 {
		t.Errorf("another process must not land on the same name by luck: %q", n1)
	}
	if strings.Contains(n1, "..") || strings.ContainsAny(n1, "/\\") {
		t.Errorf("a name with path characters in it is not a name: %q", n1)
	}
	if !strings.HasSuffix(n1, "-passwd") {
		t.Errorf("the readable part must survive so a human can recognise the file: %q", n1)
	}
}

// --- cleanup ---------------------------------------------------------------

func touchFile(t *testing.T, dir, name string, size int, mod time.Time) {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, make([]byte, size), 0o640); err != nil {
		t.Fatal(err)
	}
	if !mod.IsZero() {
		if err := os.Chtimes(p, mod, mod); err != nil {
			t.Fatal(err)
		}
	}
}

// The flags exist, so the sweep has to honour them: --media-keep by mtime,
// --media-max-mb by LRU, and never a thing it did not write itself.
func TestCleanupAgeThenKeepsTheRest(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	touchFile(t, dir, "old.png", 100, now.Add(-10*time.Hour))
	touchFile(t, dir, "new.png", 100, now.Add(-time.Minute))
	touchFile(t, dir, "sneaky.png", 100, now.Add(time.Hour))
	if err := os.Mkdir(filepath.Join(dir, "someone-elses-folder"), 0o750); err != nil {
		t.Fatal(err)
	}

	rep, err := Cleanup(testCfg(t, "-media-out="+dir, "-media-keep=2h", "-media-max-mb=0",
		"-media-save-max-bytes=1048576"), nil)
	if err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	if rep.Removed != 1 || rep.Bytes != 100 {
		t.Errorf("only the 10h-old file is expired: %+v", rep)
	}
	if rep.Kept != 2 {
		t.Errorf("two files should still be there: %+v", rep)
	}
	if rep.Skipped != 1 {
		t.Errorf("a subdirectory is skipped, not deleted, not counted as a file: %+v", rep)
	}
	if _, err := os.Stat(filepath.Join(dir, "someone-elses-folder")); err != nil {
		t.Errorf("the sweep must not remove what it did not write: %v", err)
	}
}

// --media-keep=0 is "the cache is disposable", not "the rule is off" — off is
// a negative value.
func TestCleanupKeepZeroWipesEverything(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	touchFile(t, dir, "a.png", 10, now.Add(-time.Hour))
	touchFile(t, dir, "b.png", 20, now.Add(-time.Minute))

	rep, err := Cleanup(testCfg(t, "-media-out="+dir, "-media-keep=0", "-media-max-mb=0",
		"-media-save-max-bytes=1048576"), nil)
	if err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	if rep.Removed != 2 || rep.Kept != 0 {
		t.Errorf("--media-keep=0 must empty the cache: %+v", rep)
	}
}

func TestCleanupNegativeKeepIsForever(t *testing.T) {
	dir := t.TempDir()
	touchFile(t, dir, "ancient.png", 10, time.Now().Add(-365*24*time.Hour))

	rep, err := Cleanup(testCfg(t, "-media-out="+dir, "-media-keep=-1s", "-media-max-mb=0",
		"-media-save-max-bytes=1048576"), nil)
	if err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	if rep.Removed != 0 || rep.Kept != 1 {
		t.Errorf("a negative --media-keep turns the age rule off: %+v", rep)
	}
}

func TestCleanupLRUEvictsOldestUntilUnderLimit(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	const chunk = 600 << 10 // 600 KiB: three of them do not fit in 1 MiB
	touchFile(t, dir, "oldest.png", chunk, now.Add(-3*time.Hour))
	touchFile(t, dir, "middle.png", chunk, now.Add(-2*time.Hour))
	touchFile(t, dir, "newest.png", chunk, now.Add(-1*time.Hour))

	rep, err := Cleanup(testCfg(t, "-media-out="+dir, "-media-keep=-1s", "-media-max-mb=1",
		"-media-save-max-bytes=1048576"), nil)
	if err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	if rep.Removed != 2 || rep.Kept != 1 {
		t.Fatalf("1.75 MiB has to shrink to one 600 KiB file: %+v", rep)
	}
	if _, err := os.Stat(filepath.Join(dir, "newest.png")); err != nil {
		t.Errorf("the newest file is the one LRU keeps: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "oldest.png")); !os.IsNotExist(err) {
		t.Errorf("the oldest file is the one LRU drops, got %v", err)
	}
}

func TestCleanupMissingCacheIsQuiet(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "never-created")
	rep, err := Cleanup(testCfg(t, "-media-out="+dir, "-media-max-mb=0",
		"-media-save-max-bytes=1048576"), nil)
	if err != nil {
		t.Fatalf("an absent cache is not an error, it is a first run: %v", err)
	}
	if rep != (Report{}) {
		t.Errorf("nothing existed, nothing happened: %+v", rep)
	}
}

// The prefix the agent sees is what makes local_path usable; a bare name would
// only work by accident.
func TestPrefixComesFromConfig(t *testing.T) {
	dir := t.TempDir()
	f := fetcherFor(t, testCfg(t, "-media-image=save", "-media-out="+dir, "-local-path=/dc-cache"))
	if got := f.Prefix(); got != "/dc-cache" {
		t.Errorf("prefix = %q", got)
	}
	if got := f.Dir(); got != dir {
		t.Errorf("dir = %q, want the cache directory %q", got, dir)
	}
	if joinPrefix("", "x.png") != "x.png" {
		t.Errorf("no prefix must mean the bare name")
	}
}
