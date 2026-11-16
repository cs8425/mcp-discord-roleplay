package discord

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"

	"mcp-discord-roleplay/internal/bus"
	"mcp-discord-roleplay/internal/config"
	"mcp-discord-roleplay/internal/files"
	"mcp-discord-roleplay/internal/router"
)

// The whole point of these tests is that nothing here touches the network: the
// poster, the DM creator and the timer are all seams on Client. Timing is a fake
// clock advanced by the sleep stub, so a rate-limit sequence that takes a minute
// of real time runs in microseconds and is perfectly reproducible.

func testCfg(t *testing.T, extra ...string) *config.Config {
	t.Helper()
	args := append([]string{"-selftest", "-config=" + filepath.Join(t.TempDir(), "absent.json")}, extra...)
	cfg, err := config.Load(args)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	return cfg
}

type clock struct {
	mu     sync.Mutex
	t      time.Time
	slept  []time.Duration
	ctxErr error
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) adv(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// sleep stands in for Client.wait's timer: it moves the clock instead of burning
// real time, and remembers what it was asked to wait.
func (c *clock) sleep(ctx context.Context, d time.Duration) error {
	if c.ctxErr != nil {
		return c.ctxErr
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.slept = append(c.slept, d)
	c.mu.Unlock()
	return nil
}

func (c *clock) waits() []time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]time.Duration(nil), c.slept...)
}

type recorder struct {
	mu    sync.Mutex
	n     int
	seen  []*discordgo.MessageSend
	chans []string
	sizes []int   // attachment bytes handed to each call
	errs  []error // per-call error; "" index beyond the slice means success
}

func (r *recorder) post(channelID string, data *discordgo.MessageSend) (*discordgo.Message, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	idx := r.n
	r.n++
	r.seen = append(r.seen, data)
	r.chans = append(r.chans, channelID)
	sz := 0
	for _, f := range data.Files {
		if f.Reader != nil {
			b, _ := io.ReadAll(f.Reader)
			sz += len(b)
		}
	}
	r.sizes = append(r.sizes, sz)
	if idx < len(r.errs) && r.errs[idx] != nil {
		return nil, r.errs[idx]
	}
	return &discordgo.Message{
		ID:        fmt.Sprintf("m%d", idx+1),
		ChannelID: channelID,
		// discordgo parses Discord's RFC3339 timestamp into a time.Time for us
		// (v0.29.0 message.go:66 `Timestamp time.Time` — a struct, not a string).
		Timestamp: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
	}, nil
}

func (r *recorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.n
}

func (r *recorder) size(i int) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	if i < 0 || i >= len(r.sizes) {
		return -1
	}
	return r.sizes[i]
}

func newTestClient(t *testing.T, cfg *config.Config) (*Client, *recorder, *clock) {
	t.Helper()
	rt, err := router.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	c := New(cfg, nil, bus.NewHub(cfg.BusCapacity, rt.AgentIDs(), nil), rt)
	cl := &clock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	r := &recorder{}
	c.post = r.post
	c.sleep = cl.sleep
	c.lim.now = cl.now
	return c, r, cl
}

func code(tb testing.TB, err error) string {
	tb.Helper()
	if err == nil {
		tb.Fatal("expected an error, got nil")
	}
	var se *SendError
	if !errors.As(err, &se) {
		tb.Fatalf("error is not a *SendError: %T %v", err, err)
	}
	return se.Code
}

func asSend(tb testing.TB, err error) *SendError {
	tb.Helper()
	if err == nil {
		tb.Fatal("expected an error, got nil")
	}
	var se *SendError
	if !errors.As(err, &se) {
		tb.Fatalf("error is not a *SendError: %T %v", err, err)
	}
	return se
}

func rlErr(after time.Duration) error {
	return &discordgo.RateLimitError{RateLimit: &discordgo.RateLimit{
		URL: "channels/123/messages",
		TooManyRequests: &discordgo.TooManyRequests{
			Bucket:     "channel:123",
			Message:    "you are being rate limited",
			RetryAfter: after,
		},
	}}
}

func restErr(status int, discordCode int, msg string) error {
	return &discordgo.RESTError{
		Response: &http.Response{StatusCode: status, Status: fmt.Sprintf("%d %s", status, msg)},
		Message:  &discordgo.APIErrorMessage{Code: discordCode, Message: msg},
	}
}

// ---------------------------------------------------------------------------
// payload shape
// ---------------------------------------------------------------------------

func TestSendNothingIsRejected(t *testing.T) {
	c, r, _ := newTestClient(t, testCfg(t))
	if _, err := c.Send(context.Background(), SendRequest{ChannelID: "1"}); code(t, err) != CodeBadRequest {
		t.Fatalf("want bad_request, got %v", err)
	}
	if _, err := c.Send(context.Background(), SendRequest{ChannelID: "1", Text: "   \n "}); code(t, err) != CodeBadRequest {
		t.Fatalf("whitespace-only is still nothing: got %v", err)
	}
	if r.count() != 0 {
		t.Errorf("nothing may reach discordgo: %d calls", r.count())
	}
}

func TestAllowedMentionsAreHardOff(t *testing.T) {
	// A nil Parse means "the field is absent" which means Discord parses
	// @everyone. This is the single most important assertion in the file.
	c, r, _ := newTestClient(t, testCfg(t))
	if _, err := c.Send(context.Background(), SendRequest{ChannelID: "1", Text: "hi"}); err != nil {
		t.Fatal(err)
	}
	data := r.seen[0]
	if data.AllowedMentions == nil {
		t.Fatal("AllowedMentions must be set, never nil")
	}
	if data.AllowedMentions.Parse == nil {
		t.Fatal("Parse must be a non-nil empty slice; nil would be dropped from the JSON")
	}
	if len(data.AllowedMentions.Parse) != 0 {
		t.Errorf("Parse must be empty, got %v", data.AllowedMentions.Parse)
	}
	if len(data.AllowedMentions.Roles) != 0 || len(data.AllowedMentions.Users) != 0 {
		t.Error("no explicit role or user allow-list")
	}
	if data.Embeds != nil {
		t.Error("send builds no embeds: links are pasted into the content")
	}
}

func TestSendResultCarriesIDs(t *testing.T) {
	c, _, _ := newTestClient(t, testCfg(t))
	res, err := c.Send(context.Background(), SendRequest{ChannelID: "ch1", Text: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	if res.MessageID != "m1" || res.ChannelID != "ch1" {
		t.Errorf("res = %+v", res)
	}
	if res.DryRun {
		t.Error("not a dry run")
	}
}

// ---------------------------------------------------------------------------
// budget, gap, breaker
// ---------------------------------------------------------------------------

func TestGapBetweenSendsToTheSameChannel(t *testing.T) {
	c, r, cl := newTestClient(t, testCfg(t, "-send-gap-ms=250"))
	if _, err := c.Send(context.Background(), SendRequest{ChannelID: "a", Text: "one"}); err != nil {
		t.Fatal(err)
	}
	if got := cl.waits(); len(got) != 0 {
		t.Errorf("the first send should not wait, slept %v", got)
	}
	if _, err := c.Send(context.Background(), SendRequest{ChannelID: "a", Text: "two"}); err != nil {
		t.Fatal(err)
	}
	waits := cl.waits()
	if len(waits) != 1 || waits[0] != 250*time.Millisecond {
		t.Errorf("second send to the same channel must wait out the gap, slept %v", waits)
	}
	// A different channel is not held up by this one.
	if _, err := c.Send(context.Background(), SendRequest{ChannelID: "b", Text: "other"}); err != nil {
		t.Fatal(err)
	}
	if len(cl.waits()) != 1 {
		t.Errorf("a fresh channel should not inherit the gap, slept %v", cl.waits())
	}
	if r.count() != 3 {
		t.Errorf("calls = %d, want 3", r.count())
	}
}

func TestBudgetRefusesAndComesBack(t *testing.T) {
	cfg := testCfg(t, "-max-sends-per-min=2", "-send-gap-ms=0")
	c, r, cl := newTestClient(t, cfg)
	ctx := context.Background()

	for i := 0; i < 2; i++ {
		if _, err := c.Send(ctx, SendRequest{ChannelID: "a", Text: "x"}); err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
	}
	_, err := c.Send(ctx, SendRequest{ChannelID: "a", Text: "third"})
	se := asSend(t, err)
	if se.Code != CodeBudgetExhausted {
		t.Fatalf("want %s, got %q", CodeBudgetExhausted, se.Code)
	}
	if se.RetryAfterMS < 55000 || se.RetryAfterMS > 60000 {
		t.Errorf("retry_after_ms = %d, want roughly a minute", se.RetryAfterMS)
	}
	if r.count() != 2 {
		t.Errorf("the refusal must not reach discordgo: %d calls", r.count())
	}

	cl.adv(61 * time.Second)
	if _, err := c.Send(ctx, SendRequest{ChannelID: "a", Text: "later"}); err != nil {
		t.Fatalf("after the window the budget must return: %v", err)
	}
	if r.count() != 3 {
		t.Errorf("calls = %d, want 3", r.count())
	}
}

func TestShortQueuedWaitIsAbsorbed(t *testing.T) {
	// One slot a minute: the second send is legal 60s later, and 0.4s after we
	// have advanced the clock almost all the way. A wait under maxQueuedWait is
	// ours to take, not the model's problem.
	cfg := testCfg(t, "-max-sends-per-min=1", "-send-gap-ms=0")
	c, r, cl := newTestClient(t, cfg)
	ctx := context.Background()
	if _, err := c.Send(ctx, SendRequest{ChannelID: "a", Text: "one"}); err != nil {
		t.Fatal(err)
	}
	cl.adv(56 * time.Second) // four seconds short of the window rolling over
	if _, err := c.Send(ctx, SendRequest{ChannelID: "a", Text: "two"}); err != nil {
		t.Fatalf("a four-second wait is ours to absorb, got %v", err)
	}
	if r.count() != 2 {
		t.Errorf("calls = %d, want 2", r.count())
	}
	if w := cl.waits(); len(w) != 1 || w[0] <= 0 {
		t.Errorf("expected one positive wait, got %v", w)
	}
}

func TestBreakerOpensThenRefusesEverything(t *testing.T) {
	cfg := testCfg(t, "-breaker-errors=2", "-send-retry-wait-ms=0", "-send-gap-ms=0", "-max-sends-per-min=100")
	c, r, cl := newTestClient(t, cfg)
	r.errs = []error{rlErr(10 * time.Second), rlErr(10 * time.Second), rlErr(10 * time.Second)}
	ctx := context.Background()

	se := asSend(t, sendErr(c, ctx))
	if se.Code != CodeRateLimited || se.RetryAfterMS != 10000 {
		t.Fatalf("first 429: %+v", se)
	}
	if se.BreakerOpen {
		t.Error("one 429 must not open the breaker")
	}

	se = asSend(t, sendErr(c, ctx))
	if se.Code != CodeRateLimited {
		t.Fatalf("want rate_limited, got %q", se.Code)
	}
	if !se.BreakerOpen {
		t.Error("the second 429 reached --breaker-errors, the answer must say the breaker opened")
	}

	// Everything after that is refused without touching discordgo at all.
	n := r.count()
	for i := 0; i < 5; i++ {
		se = asSend(t, sendErr(c, ctx))
		if se.Code != CodeCircuitOpen {
			t.Fatalf("call %d during the hold: %q", i, se.Code)
		}
		if se.RetryAfterMS <= 0 {
			t.Error("circuit_open without a time to come back is useless to the model")
		}
	}
	if r.count() != n {
		t.Errorf("the breaker let %d requests through anyway", r.count()-n)
	}

	// Half-open: exactly one probe gets through, and its failure closes the door
	// again for another hold.
	cl.adv(breakerHold + time.Second)
	se = asSend(t, sendErr(c, ctx))
	if r.count() != n+1 {
		t.Fatalf("after the hold one send must be attempted: calls %d want %d", r.count(), n+1)
	}
	if se.Code != CodeRateLimited {
		t.Errorf("a failed probe reports the 429 itself, got %q", se.Code)
	}
	// and the next one is blocked again, because the probe failed.
	if got := code(t, sendErr(c, ctx)); got != CodeCircuitOpen {
		t.Errorf("after a failed probe the door closes again, got %q", got)
	}
}

func TestProbeSuccessClosesTheBreaker(t *testing.T) {
	cfg := testCfg(t, "-breaker-errors=1", "-send-retry-wait-ms=0", "-send-gap-ms=0", "-max-sends-per-min=100")
	c, r, cl := newTestClient(t, cfg)
	r.errs = []error{rlErr(5 * time.Second)}
	ctx := context.Background()

	if got := code(t, sendErr(c, ctx)); got != CodeRateLimited {
		t.Fatalf("want rate_limited, got %q", got)
	}
	if got := code(t, sendErr(c, ctx)); got != CodeCircuitOpen {
		t.Fatalf("breaker should hold, got %q", got)
	}
	cl.adv(breakerHold + time.Second)
	if _, err := c.Send(ctx, SendRequest{ChannelID: "a", Text: "probe"}); err != nil {
		t.Fatalf("the probe now succeeds and should close the breaker: %v", err)
	}
	if _, err := c.Send(ctx, SendRequest{ChannelID: "a", Text: "normal"}); err != nil {
		t.Fatalf("sending must be back to normal: %v", err)
	}
	if st := c.Stats(); st.Consecutive429 != 0 || st.BreakerOpen {
		t.Errorf("stats after recovery: %+v", st)
	}
}

func TestSmallRetryAfterIsAbsorbedOnce(t *testing.T) {
	cfg := testCfg(t, "-send-retry-wait-ms=1500", "-send-gap-ms=0", "-breaker-errors=5")
	c, r, cl := newTestClient(t, cfg)
	r.errs = []error{rlErr(800 * time.Millisecond)} // first fails, second succeeds
	ctx := context.Background()

	res, err := c.Send(ctx, SendRequest{ChannelID: "a", Text: "hi"})
	if err != nil {
		t.Fatalf("an 800ms retry_after is ours to absorb: %v", err)
	}
	if res.MessageID != "m2" {
		t.Errorf("the retry is the message that landed: %+v", res)
	}
	if r.count() != 2 {
		t.Errorf("calls = %d, want 2", r.count())
	}
	if w := cl.waits(); len(w) != 1 || w[0] != 800*time.Millisecond {
		t.Errorf("expected one 800ms wait, got %v", w)
	}
	if st := c.Stats(); st.Consecutive429 != 0 {
		t.Errorf("a recovered send must clear the streak: %+v", st)
	}
}

func TestNoRetryLoopAfterTheSecond429(t *testing.T) {
	cfg := testCfg(t, "-send-retry-wait-ms=1500", "-send-gap-ms=0", "-breaker-errors=9")
	c, r, _ := newTestClient(t, cfg)
	r.errs = []error{rlErr(500 * time.Millisecond), rlErr(500 * time.Millisecond)}
	_, err := c.Send(context.Background(), SendRequest{ChannelID: "a", Text: "hi"})
	se := asSend(t, err)
	if se.Code != CodeRateLimited {
		t.Fatalf("want rate_limited, got %q", se.Code)
	}
	if r.count() != 2 {
		t.Errorf("exactly one local retry, got %d calls", r.count())
	}
}

func TestCancelledWaitDoesNotSend(t *testing.T) {
	cfg := testCfg(t, "-send-gap-ms=5000")
	c, r, cl := newTestClient(t, cfg)
	ctx, cancel := context.WithCancel(context.Background())
	if _, err := c.Send(ctx, SendRequest{ChannelID: "a", Text: "one"}); err != nil {
		t.Fatal(err)
	}
	cancel()
	cl.ctxErr = context.Canceled // stand in for a timer that never fires
	se := asSend(t, sendErr(c, ctx))
	if se.Code != CodeCancelled {
		t.Fatalf("want cancelled, got %q", se.Code)
	}
	if r.count() != 1 {
		t.Errorf("a cancelled wait must not post: %d calls", r.count())
	}
}

func TestNoSessionMeansNotConnected(t *testing.T) {
	// A client that never started has no session and no seams: this is the real
	// production answer when the gateway is down.
	cfg := testCfg(t)
	rt := mustRouter(t, cfg)
	c := New(cfg, nil, bus.NewHub(cfg.BusCapacity, rt.AgentIDs(), nil), rt)
	c.lim = newSendLimiter(cfg)
	if _, err := c.Send(context.Background(), SendRequest{ChannelID: "a", Text: "hi"}); code(t, err) != CodeNotConnected {
		t.Fatalf("want not_connected, got %v", err)
	}
	if _, err := c.DMChannel(context.Background(), "u1"); code(t, err) != CodeNotConnected {
		t.Fatalf("dm: want not_connected, got %v", err)
	}
}

func TestDryRunTouchesNeitherRESTNorBudget(t *testing.T) {
	cfg := testCfg(t, "-dry-run", "-max-sends-per-min=1")
	c, r, _ := newTestClient(t, cfg)
	before := c.Stats().BudgetLeft
	for i := 0; i < 4; i++ {
		res, err := c.Send(context.Background(), SendRequest{ChannelID: "a", Text: "rehearsal"})
		if err != nil {
			t.Fatalf("dry run %d: %v", i, err)
		}
		if res.MessageID != DryRunMessageID || !res.DryRun {
			t.Errorf("dry run answer should be honest about itself: %+v", res)
		}
	}
	if r.count() != 0 {
		t.Errorf("dry run called discordgo %d times", r.count())
	}
	if after := c.Stats().BudgetLeft; after != before {
		t.Errorf("dry run must not spend the budget: %d -> %d", before, after)
	}
}

// ---------------------------------------------------------------------------
// attachments
// ---------------------------------------------------------------------------

func TestRetryRewindsTheAttachment(t *testing.T) {
	dir := t.TempDir()
	body := strings.Repeat("payload!", 100) // 800 bytes
	if err := write(filepath.Join(dir, "scene.txt"), body); err != nil {
		t.Fatal(err)
	}
	cfg := testCfg(t, "-file-root", dir, "-send-retry-wait-ms=1500", "-send-gap-ms=0", "-breaker-errors=9")
	store := files.New(cfg, nil)
	o, err := store.Open("scene.txt", "", "text/plain")
	if err != nil {
		t.Fatal(err)
	}
	defer o.Close()

	c, r, _ := newTestClient(t, cfg)
	r.errs = []error{rlErr(300 * time.Millisecond)}
	if _, err := c.Send(context.Background(), SendRequest{ChannelID: "a", Text: "look", Files: []*files.Opened{o}}); err != nil {
		t.Fatalf("retry should have saved it: %v", err)
	}
	if r.count() != 2 {
		t.Fatalf("calls = %d", r.count())
	}
	if r.size(0) != 800 {
		t.Errorf("first attempt read %d bytes, want the whole file", r.size(0))
	}
	if r.size(1) != 800 {
		t.Errorf("retry read %d bytes: the reader was not rewound, Discord would get an empty file", r.size(1))
	}
	if len(r.seen[1].Files) != 1 || r.seen[1].Files[0].Name != "scene.txt" {
		t.Errorf("attachment not on the retry: %+v", r.seen[1].Files)
	}
	if r.seen[1].Files[0].ContentType != "text/plain" {
		t.Errorf("ContentType = %q", r.seen[1].Files[0].ContentType)
	}
}

func TestAttachmentsWithoutTextAreAllowed(t *testing.T) {
	dir := t.TempDir()
	if err := write(filepath.Join(dir, "f.bin"), "bytes"); err != nil {
		t.Fatal(err)
	}
	cfg := testCfg(t, "-file-root", dir)
	store := files.New(cfg, nil)
	o, err := store.Open("f.bin", "", "")
	if err != nil {
		t.Fatal(err)
	}
	defer o.Close()
	c, r, _ := newTestClient(t, cfg)
	if _, err := c.Send(context.Background(), SendRequest{ChannelID: "a", Files: []*files.Opened{o}}); err != nil {
		t.Fatal(err)
	}
	if r.count() != 1 || r.seen[0].Content != "" {
		t.Errorf("an attachment-only message is legal: %+v", r.seen[0])
	}
}

// ---------------------------------------------------------------------------
// DM channels
// ---------------------------------------------------------------------------

func mustRouter(t *testing.T, cfg *config.Config) *router.Router {
	t.Helper()
	rt, err := router.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return rt
}

func TestDMChannelIsCreatedOnceAndCached(t *testing.T) {
	cfg := testCfg(t)
	c, _, _ := newTestClient(t, cfg)
	calls := 0
	c.dmCreate = func(recipient string) (*discordgo.Channel, error) {
		calls++
		if recipient != "u1" {
			t.Errorf("recipient = %q", recipient)
		}
		return &discordgo.Channel{ID: "dm-chan-1", Type: discordgo.ChannelTypeDM}, nil
	}
	for i := 0; i < 3; i++ {
		id, err := c.DMChannel(context.Background(), "u1")
		if err != nil {
			t.Fatal(err)
		}
		if id != "dm-chan-1" {
			t.Fatalf("id = %q", id)
		}
	}
	if calls != 1 {
		t.Errorf("UserChannelCreate called %d times, want 1", calls)
	}
	if _, err := c.DMChannel(context.Background(), "  "); code(t, err) != CodeBadRequest {
		t.Errorf("empty recipient: want bad_request, got %v", err)
	}
}

func TestDMChannelDryRunNeedsNoSession(t *testing.T) {
	cfg := testCfg(t, "-dry-run")
	rt := mustRouter(t, cfg)
	c := New(cfg, nil, bus.NewHub(cfg.BusCapacity, rt.AgentIDs(), nil), rt)
	id, err := c.DMChannel(context.Background(), "u9")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(id, "dry-run-dm:") {
		t.Errorf("id = %q", id)
	}
}

func TestDMChannelFailureIsClassified(t *testing.T) {
	cfg := testCfg(t)
	c, _, _ := newTestClient(t, cfg)
	c.dmCreate = func(string) (*discordgo.Channel, error) {
		return nil, restErr(http.StatusForbidden, 30001, "Unauthorized")
	}
	_, err := c.DMChannel(context.Background(), "u1")
	se := asSend(t, err)
	if se.Code != CodeForbidden || se.DiscordCode != "30001" {
		t.Errorf("want forbidden/30001, got %+v", se)
	}
	if !strings.Contains(se.Msg, "u1") {
		t.Errorf("the message should name the recipient so the model knows which DM failed: %q", se.Msg)
	}
}

// ---------------------------------------------------------------------------
// classification
// ---------------------------------------------------------------------------

func TestClassify(t *testing.T) {
	cases := []struct {
		name      string
		err       error
		code      string
		status    int
		retryMS   int64
		discordID string
	}{
		{"nil", nil, "", 0, 0, ""},
		{"429 with 3s", rlErr(3 * time.Second), CodeRateLimited, http.StatusTooManyRequests, 3000, ""},
		{"429 without detail", &discordgo.RateLimitError{}, CodeRateLimited, http.StatusTooManyRequests, 1000, ""},
		{"401", restErr(http.StatusUnauthorized, 0, "Unauthorized"), CodeUnauthorized, 401, 0, ""},
		{"403 missing perms", restErr(http.StatusForbidden, 50013, "Missing Permissions"), CodeForbidden, 403, 0, "50013"},
		{"403 bots cannot", restErr(http.StatusBadRequest, 40140, "Bots cannot use this endpoint"), CodeForbidden, 400, 0, "40140"},
		{"403 dms disabled", restErr(http.StatusForbidden, 30001, "Unauthorized"), CodeForbidden, 403, 0, "30001"},
		{"404 unknown channel", restErr(http.StatusNotFound, 10003, "Unknown Channel"), CodeUnknownChannel, 404, 0, "10003"},
		{"403 cannot send to user", restErr(http.StatusForbidden, 50001, "Cannot send messages to this user"), CodeCannotSend, 403, 0, "50001"},
		{"400 slowmode-ish", restErr(http.StatusBadRequest, 0, "Invalid Form Body"), CodeBadRequest, 400, 0, ""},
		{"413 too large", restErr(http.StatusRequestEntityTooLarge, 0, "Too big"), CodeBadRequest, 413, 0, ""},
		{"500", restErr(http.StatusInternalServerError, 0, "Internal"), CodeServerError, 500, 0, ""},
		{"503", restErr(http.StatusServiceUnavailable, 0, "Unavailable"), CodeServerError, 503, 0, ""},
		{"deadline", context.DeadlineExceeded, CodeCancelled, 0, 0, ""},
		{"canceled", context.Canceled, CodeCancelled, 0, 0, ""},
		{"network", &url.Error{Op: "Post", URL: "https://discord.com", Err: errors.New("connection refused")}, CodeNetwork, 0, 0, ""},
		{"library unauthorized", discordgo.ErrUnauthorized, CodeUnauthorized, 0, 0, ""},
		{"mystery", errors.New("something else entirely"), CodeAPIError, 0, 0, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			se := classify(tc.err)
			if tc.err == nil {
				if se != nil {
					t.Fatalf("classify(nil) = %+v", se)
				}
				return
			}
			if se == nil {
				t.Fatalf("classify(%v) = nil", tc.err)
			}
			if se.Code != tc.code {
				t.Errorf("code = %q, want %q (%v)", se.Code, tc.code, tc.err)
			}
			if se.Status != tc.status {
				t.Errorf("status = %d, want %d", se.Status, tc.status)
			}
			if se.RetryAfterMS != tc.retryMS {
				t.Errorf("retry_after_ms = %d, want %d", se.RetryAfterMS, tc.retryMS)
			}
			if se.DiscordCode != tc.discordID {
				t.Errorf("discord_code = %q, want %q", se.DiscordCode, tc.discordID)
			}
			if se.Msg == "" {
				t.Error("an error with no message is useless to the model")
			}
			if !strings.Contains(se.Error(), se.Code) {
				t.Errorf("Error() should lead with the code: %q", se.Error())
			}
		})
	}
}

func TestClassifyHidesTheRawBody(t *testing.T) {
	// A REST error carries the whole HTTP body; echoing it would dump JSON and
	// possibly a token-bearing URL into the model's context.
	re := &discordgo.RESTError{
		Response:     &http.Response{StatusCode: 400},
		ResponseBody: []byte(strings.Repeat("{", 5000)),
	}
	se := classify(re)
	if len(se.Msg) > 260 {
		t.Errorf("raw body leaked into the message (%d chars)", len(se.Msg))
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// sendErr is a one-line send whose result we only care about as an error.
func sendErr(c *Client, ctx context.Context) error {
	_, err := c.Send(ctx, SendRequest{ChannelID: "a", Text: "ping"})
	return err
}

func write(path, body string) error { return os.WriteFile(path, []byte(body), 0o600) }
