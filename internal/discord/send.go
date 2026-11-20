// Outbound sending, with the rate-limit discipline that goes with it.
//
// Why the discipline lives here and not in the tools layer: every path that can
// put a message on Discord must pass through it, including the future HTTP
// transport and any internal retry. A limiter next to the REST call cannot be
// bypassed by forgetting to wrap a call site.
//
// discordgo's own handling is deliberately switched OFF (ShouldRetryOnRateLimit
// = false, set in Start). With it ON, a 429 never reaches us as an error: the
// library sleeps and retries the same bucket with no context, so the breaker
// below would be decoration and a tool call could block indefinitely
// (restapi.go:279-298 of v0.29.0). With it OFF we get *RateLimitError carrying
// Discord's retry_after and decide for ourselves.
package discord

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/bwmarrin/discordgo"

	"mcp-discord-roleplay/internal/config"
	"mcp-discord-roleplay/internal/files"
)

// DryRunMessageID stands in for a message id when --dry-run is active.
const DryRunMessageID = "dry-run"

// MaxContentRunes is Discord's hard limit for one message body. It is not a
// flag: going above it is a rejected request, so the only sane behaviour is to
// split, and splitting is only correct at exactly this number.
const MaxContentRunes = 2000

// breakerHold is how long the circuit stays open once it trips, and
// maxQueuedWait caps how long we queue a send locally (our own budget/gap, or a
// Discord retry_after) before handing the decision back to the model. A tool
// call that silently blocks for a minute is worse to an agent than an honest
// "rate_limited, retry in 43s" it can act on.
const (
	breakerHold   = 60 * time.Second
	maxQueuedWait = 5 * time.Second
	probeBackoff  = 2 * time.Second
)

// Outbound error codes, part of the tool contract.
const (
	CodeNotConnected    = "not_connected"
	CodeCircuitOpen     = "circuit_open"
	CodeBudgetExhausted = "budget_exhausted"
	CodeRateLimited     = "rate_limited"
	CodeCancelled       = "cancelled"
	CodeUnauthorized    = "unauthorized"
	CodeForbidden       = "forbidden"
	CodeUnknownChannel  = "unknown_channel"
	CodeCannotSend      = "cannot_send_to_channel"
	CodeBadRequest      = "bad_request"
	CodeServerError     = "server_error"
	CodeNetwork         = "network_error"
	CodeAPIError        = "api_error"
	CodeFileFailed      = "attachment_failed"
	CodeDMFailed        = "dm_channel_failed"
)

// SendError is what Send and DMChannel return. Every field is meant to be
// forwarded to the model: the code to branch on, retry_after_ms to wait,
// discord_code for the operator, breaker_open so it knows the door is shut.
type SendError struct {
	Code         string
	Msg          string
	Status       int
	DiscordCode  string
	RetryAfterMS int64
	BreakerOpen  bool
}

func (e *SendError) Error() string {
	parts := []string{e.Code}
	if e.Status != 0 {
		parts = append(parts, "http:"+strconv.Itoa(e.Status))
	}
	if e.DiscordCode != "" {
		parts = append(parts, "discord:"+e.DiscordCode)
	}
	if e.Msg != "" {
		parts = append(parts, e.Msg)
	}
	if e.RetryAfterMS > 0 {
		parts = append(parts, "retry_after_ms="+strconv.FormatInt(e.RetryAfterMS, 10))
	}
	return strings.Join(parts, " ")
}

// SendRequest is one message. Text and Files are the whole surface — an embed
// is not offered, because a link is pasted into Text and Discord renders it.
type SendRequest struct {
	ChannelID string
	Text      string
	Files     []*files.Opened

	// ReplyTo is the id of the message this one answers; empty sends a plain
	// message. See buildMessageSend for what a reference costs when the id is not
	// there any more.
	ReplyTo string
}

// SendResult describes the message that landed.
type SendResult struct {
	MessageID   string
	ChannelID   string
	CreatedAt   time.Time
	Attachments int
	DryRun      bool
}

// Send posts one message, respecting budget, per-channel gap and breaker.
// It is safe for concurrent use; ordering between two concurrent sends to the
// same channel is whatever the scheduler picks, which is why the gap is
// enforced at admit time rather than assumed.
func (c *Client) Send(ctx context.Context, req SendRequest) (*SendResult, error) {
	if strings.TrimSpace(req.Text) == "" && len(req.Files) == 0 {
		return nil, &SendError{Code: CodeBadRequest, Msg: "nothing to send: text and files are both empty"}
	}
	if c.cfg.DryRun {
		// No session, no REST, no budget consumed: --dry-run must also work while
		// the gateway is down, so it rehearses the decision, not the request.
		names := make([]string, 0, len(req.Files))
		for _, f := range req.Files {
			names = append(names, f.Name)
		}
		c.log.Info("dry-run send",
			"channel", req.ChannelID,
			"runes", utf8.RuneCountInString(req.Text),
			"files", strings.Join(names, ","))
		return &SendResult{
			MessageID:   DryRunMessageID,
			ChannelID:   req.ChannelID,
			CreatedAt:   time.Now(),
			Attachments: len(req.Files),
			DryRun:      true,
		}, nil
	}

	post, err := c.poster()
	if err != nil {
		return nil, err
	}

	delay, code, retryMS := c.lim.reserve(req.ChannelID)
	if code != "" {
		return nil, &SendError{
			Code:         code,
			RetryAfterMS: retryMS,
			BreakerOpen:  code == CodeCircuitOpen,
			Msg:          limiterHint(code, retryMS),
		}
	}
	if err := c.wait(ctx, delay); err != nil {
		return nil, &SendError{Code: CodeCancelled, Msg: err.Error()}
	}

	res, serr := c.attempt(req, post)
	if serr == nil {
		c.lim.noteSuccess()
		return res, nil
	}
	if serr.Code != CodeRateLimited {
		c.lim.noteOtherEnds()
		return nil, serr
	}

	// One bounded, honest retry: absorb the small retry_after values ourselves
	// and hand the big ones back to the model as data.
	rl := time.Duration(serr.RetryAfterMS) * time.Millisecond
	retryOK, opened := c.lim.noteRateLimit(rl)
	serr.BreakerOpen = opened
	if !retryOK {
		return nil, serr
	}
	if err := c.wait(ctx, rl); err != nil {
		return nil, serr // the 429 answer is still the truth worth reporting
	}
	res, serr2 := c.attempt(req, post)
	if serr2 == nil {
		c.lim.noteSuccess()
		return res, nil
	}
	if serr2.Code == CodeRateLimited {
		_, opened2 := c.lim.noteRateLimit(time.Duration(serr2.RetryAfterMS) * time.Millisecond)
		serr2.BreakerOpen = opened2
		return nil, serr2
	}
	c.lim.noteOtherEnds()
	return nil, serr2
}

// attempt does one REST round trip after rewinding every attachment.
func (c *Client) attempt(req SendRequest, post poster) (*SendResult, *SendError) {
	for _, f := range req.Files {
		if err := f.Rewind(); err != nil {
			return nil, &SendError{Code: CodeFileFailed, Msg: err.Error()}
		}
	}
	msg, err := post(req.ChannelID, buildMessageSend(req))
	if err != nil {
		return nil, classify(err)
	}
	if msg == nil {
		return nil, &SendError{Code: CodeAPIError, Msg: "discord returned no message object"}
	}
	ch := msg.ChannelID
	if ch == "" {
		ch = req.ChannelID
	}
	return &SendResult{
		MessageID:   msg.ID,
		ChannelID:   ch,
		CreatedAt:   messageTime(msg),
		Attachments: len(msg.Attachments),
	}, nil
}

// buildMessageSend is the single place a discordgo payload is assembled, so the
// mention policy can be read (and audited) in one screenful.
func buildMessageSend(req SendRequest) *discordgo.MessageSend {
	data := &discordgo.MessageSend{
		Content: req.Text,
		// Hard-coded, no flag to turn it off: the model must never be able to
		// ping @everyone, a role or a user. An EMPTY slice is what means "parse
		// nothing" — Parse has no omitempty, so a nil field would be dropped from
		// the JSON entirely and Discord would default to parsing everything.
		AllowedMentions: &discordgo.MessageAllowedMentions{
			Parse: []discordgo.AllowedMentionType{},
		},
	}
	if req.ReplyTo != "" {
		// Quoting does not ping: the allowed-mentions above already sends
		// replied_user=false, which is what an in-character reply wants — answering
		// someone in their channel a dozen times a day should not notify them every
		// time. The reference is soft (fail_if_not_exists=false), because an id that
		// is too old to quote, or one Discord cannot see from here, should cost us
		// the quote and not the message: the words are already written.
		soft := false
		data.Reference = &discordgo.MessageReference{
			MessageID:       req.ReplyTo,
			FailIfNotExists: &soft,
		}
	}
	if len(req.Files) > 0 {
		data.Files = make([]*discordgo.File, 0, len(req.Files))
		for _, f := range req.Files {
			data.Files = append(data.Files, &discordgo.File{
				Name:        f.Name,
				ContentType: f.ContentType,
				Reader:      f.Reader(),
			})
		}
	}
	return data
}

// poster is the REST seam (tests replace it; production leaves it nil).
type poster func(channelID string, data *discordgo.MessageSend) (*discordgo.Message, error)

type channelPoster func(recipientID string) (*discordgo.Channel, error)

func (c *Client) poster() (poster, error) {
	if c.post != nil {
		return c.post, nil
	}
	sess := c.Session()
	if sess == nil {
		return nil, &SendError{Code: CodeNotConnected,
			Msg: "no gateway session: the server never started (bad token, or the gateway refused us)"}
	}
	return func(ch string, d *discordgo.MessageSend) (*discordgo.Message, error) {
		return sess.ChannelMessageSendComplex(ch, d)
	}, nil
}

// DMChannel returns the DM channel id for a user, creating it on first use and
// caching it. Discord DM channel ids are stable, so this cache never needs
// invalidation; a gateway CHANNEL_DELETE for a DM is not something we react to.
func (c *Client) DMChannel(ctx context.Context, recipientID string) (string, error) {
	id := strings.TrimSpace(recipientID)
	if id == "" {
		return "", &SendError{Code: CodeBadRequest, Msg: "empty recipient id for a DM target"}
	}
	if c.cfg.DryRun {
		return "dry-run-dm:" + id, nil
	}
	if v, ok := c.dm.Load(id); ok {
		if s, isStr := v.(string); isStr && s != "" {
			return s, nil
		}
	}
	create := c.dmCreate
	if create == nil {
		sess := c.Session()
		if sess == nil {
			return "", &SendError{Code: CodeNotConnected,
				Msg: "no gateway session: cannot open a DM channel"}
		}
		create = func(r string) (*discordgo.Channel, error) { return sess.UserChannelCreate(r) }
	}
	ch, err := create(id)
	if err != nil {
		se := classify(err)
		se.Msg = "cannot open a DM channel with " + id + ": " + se.Msg
		return "", se
	}
	if ch == nil || ch.ID == "" {
		return "", &SendError{Code: CodeDMFailed, Msg: "discord returned no DM channel"}
	}
	c.dm.Store(id, ch.ID)
	c.log.Debug("dm channel resolved", "recipient", id, "channel", ch.ID)
	return ch.ID, nil
}

// wait is the ctx-aware sleep used for gap, budget and the single 429 retry.
// time.Sleep is deliberately absent: a blocked goroutine here blocks a tool call
// that the caller can no longer cancel.
func (c *Client) wait(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	if c.sleep != nil {
		return c.sleep(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return fmt.Errorf("cancelled while waiting %s: %w", d, ctx.Err())
	case <-t.C:
		return nil
	}
}

// ---------------------------------------------------------------------------
// limiter
// ---------------------------------------------------------------------------

// sendLimiter implements: a sliding-minute global send budget, a minimum gap per
// channel, and a circuit breaker that trips after N consecutive rate-limit
// answers and holds for breakerHold with a single half-open probe afterwards.
type sendLimiter struct {
	mu        sync.Mutex
	perMin    int
	window    time.Duration
	gap       time.Duration
	maxFail   int
	retryWait time.Duration

	hits      []time.Time
	last      map[string]time.Time
	fails     int
	openUntil time.Time
	probing   bool

	now func() time.Time // test seam
}

func newSendLimiter(cfg *config.Config) *sendLimiter {
	return &sendLimiter{
		perMin:    cfg.MaxSendsPerMin,
		window:    time.Minute,
		gap:       time.Duration(cfg.SendGapMS) * time.Millisecond,
		maxFail:   cfg.BreakerErrors,
		retryWait: time.Duration(cfg.SendRetryWaitMS) * time.Millisecond,
		last:      map[string]time.Time{},
		now:       time.Now,
	}
}

// reserve asks whether a send to channelID may go out. It returns the delay to
// wait first, or a refusal code with how long to come back after. The slot is
// taken at the moment the request will fire, so two concurrent callers cannot
// both be promised the same budget.
func (l *sendLimiter) reserve(channelID string) (delay time.Duration, code string, retryMS int64) {
	if l == nil {
		return 0, "", 0
	}
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()

	if !l.openUntil.IsZero() {
		if now.Before(l.openUntil) {
			return 0, CodeCircuitOpen, millis(l.openUntil.Sub(now))
		}
		if l.probing {
			// The probe is still in flight; do not stampede behind it.
			return 0, CodeCircuitOpen, millis(probeBackoff)
		}
		l.probing = true
	}

	l.pruneLocked(now)
	if l.perMin > 0 && len(l.hits) >= l.perMin {
		w := l.hits[0].Add(l.window).Sub(now)
		if w > maxQueuedWait {
			return 0, CodeBudgetExhausted, millis(w)
		}
		if w > delay {
			delay = w
		}
	}
	if t, ok := l.last[channelID]; ok {
		if w := t.Add(l.gap).Sub(now); w > delay {
			delay = w
		}
	}
	if delay > maxQueuedWait {
		return 0, CodeBudgetExhausted, millis(delay)
	}

	at := now.Add(delay)
	l.hits = append(l.hits, at)
	l.last[channelID] = at
	return delay, "", 0
}

func (l *sendLimiter) pruneLocked(now time.Time) {
	n := 0
	for _, t := range l.hits {
		if now.Sub(t) < l.window {
			l.hits[n] = t
			n++
		}
	}
	l.hits = l.hits[:n]
}

// noteSuccess closes the breaker and clears the consecutive-failure streak.
func (l *sendLimiter) noteSuccess() {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.fails = 0
	l.openUntil = time.Time{}
	l.probing = false
}

// noteRateLimit records a 429 and reports whether the caller may wait locally
// and retry once, and whether the breaker has now opened (in which case the
// answer to the model is the breaker, not another sleep).
func (l *sendLimiter) noteRateLimit(retryAfter time.Duration) (retryOK, opened bool) {
	if l == nil {
		return false, false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.fails++
	l.probing = false
	if l.fails >= l.maxFail {
		l.openUntil = l.now().Add(breakerHold)
		return false, true
	}
	return retryAfter > 0 && retryAfter <= l.retryWait, false
}

// noteOtherEnds clears the probe slot after a non-rate-limit failure, so a 500
// or a revoked token cannot leave the breaker stuck half-open forever.
func (l *sendLimiter) noteOtherEnds() {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.probing = false
}

// SendStats is the limiter view handed to discord_status.
type SendStats struct {
	BudgetLeft     int   `json:"budget_left"`
	SendsPerMin    int   `json:"sends_per_min"`
	GapMS          int   `json:"gap_ms"`
	Consecutive429 int   `json:"consecutive_429"`
	BreakerOpen    bool  `json:"breaker_open"`
	RetryAfterMS   int64 `json:"retry_after_ms,omitempty"`
	Probing        bool  `json:"probing,omitempty"`
	BreakerHoldS   int   `json:"breaker_hold_s"`
}

// Stats reports the limiter state without changing it.
func (c *Client) Stats() SendStats { return c.lim.stats() }

func (l *sendLimiter) stats() SendStats {
	out := SendStats{BreakerHoldS: int(breakerHold / time.Second)}
	if l == nil {
		return out
	}
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	l.pruneLocked(now)
	out.SendsPerMin = l.perMin
	out.GapMS = int(l.gap / time.Millisecond)
	out.Consecutive429 = l.fails
	if l.perMin > 0 {
		out.BudgetLeft = l.perMin - len(l.hits)
		if out.BudgetLeft < 0 {
			out.BudgetLeft = 0
		}
	}
	if !l.openUntil.IsZero() && now.Before(l.openUntil) {
		out.BreakerOpen = true
		out.RetryAfterMS = millis(l.openUntil.Sub(now))
	}
	out.Probing = l.probing
	return out
}

func limiterHint(code string, retryMS int64) string {
	switch code {
	case CodeCircuitOpen:
		return fmt.Sprintf("too many consecutive rate-limit answers: sending is paused for %s; "+
			"wait, or talk to the user in the meantime", time.Duration(retryMS)*time.Millisecond)
	case CodeBudgetExhausted:
		return fmt.Sprintf("this process has used its whole per-minute send budget; "+
			"come back in %s or raise --max-sends-per-min", time.Duration(retryMS)*time.Millisecond)
	default:
		return "not allowed to send right now"
	}
}

func millis(d time.Duration) int64 {
	if d < 0 {
		return 0
	}
	return d.Milliseconds()
}

// ---------------------------------------------------------------------------
// error classification
// ---------------------------------------------------------------------------

// classify turns a discordgo error into something the model can branch on. The
// raw HTTP body never reaches the tool output: it is parsed for Discord's own
// numeric code and shortened.
func classify(err error) *SendError {
	if err == nil {
		return nil
	}
	var rl *discordgo.RateLimitError
	if errors.As(err, &rl) {
		ms := int64(1000)
		if rl.RateLimit != nil && rl.TooManyRequests != nil && rl.RetryAfter > 0 {
			ms = rl.RetryAfter.Milliseconds()
		}
		return &SendError{
			Code:         CodeRateLimited,
			Status:       http.StatusTooManyRequests,
			RetryAfterMS: ms,
			Msg:          "discord told us to slow down",
		}
	}
	var re *discordgo.RESTError
	if errors.As(err, &re) {
		status := 0
		if re.Response != nil {
			status = re.Response.StatusCode
		}
		se := &SendError{Status: status, Msg: restErrText(re)}
		if code := restAPICode(re); code != "" {
			se.DiscordCode = code
		}
		se.Code = classifyStatus(status, se.DiscordCode)
		return se
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return &SendError{Code: CodeCancelled, Msg: "send timed out"}
	case errors.Is(err, context.Canceled):
		return &SendError{Code: CodeCancelled, Msg: "send cancelled"}
	}
	var ue *url.Error
	if errors.As(err, &ue) {
		return &SendError{Code: CodeNetwork, Msg: "could not reach discord: " + truncate(ue.Err.Error(), 160)}
	}
	if errors.Is(err, discordgo.ErrUnauthorized) {
		return &SendError{Code: CodeUnauthorized, Msg: "discord rejected the token (401)"}
	}
	return &SendError{Code: CodeAPIError, Msg: truncate(err.Error(), 200)}
}

// discordCodes maps the numeric codes that mean "this target will never work"
// into one vocabulary the model can act on.
var discordCodes = map[string]string{
	"10003": CodeUnknownChannel, // Unknown Channel
	"10004": CodeUnknownChannel, // Unknown Guild
	"40003": CodeUnknownChannel, // Missing Permissions (bad id form)
	"50001": CodeCannotSend,     // Cannot send messages to this user
	"50007": CodeCannotSend,     // Cannot send messages in this channel
	"50008": CodeCannotSend,     // This channel was deleted
	"20024": CodeCannotSend,     // Cannot send messages in a thread
	"50013": CodeForbidden,      // Missing Permissions
	"40140": CodeForbidden,      // Bots cannot use this endpoint
	"30001": CodeForbidden,      // Unauthorized: DMs disabled
}

func classifyStatus(status int, code string) string {
	if mapped, ok := discordCodes[code]; ok {
		return mapped
	}
	switch status {
	case http.StatusUnauthorized:
		return CodeUnauthorized
	case http.StatusForbidden:
		return CodeForbidden
	case http.StatusNotFound:
		return CodeUnknownChannel
	case http.StatusTooManyRequests:
		return CodeRateLimited
	case http.StatusBadRequest:
		return CodeBadRequest
	case http.StatusRequestEntityTooLarge:
		return CodeBadRequest
	}
	if status >= 500 {
		return CodeServerError
	}
	return CodeAPIError
}

func restErrText(re *discordgo.RESTError) string {
	if re.Message != nil && strings.TrimSpace(re.Message.Message) != "" {
		return truncate(re.Message.Message, 200)
	}
	if body := strings.TrimSpace(string(re.ResponseBody)); body != "" {
		return truncate(body, 200)
	}
	if re.Response != nil {
		return re.Response.Status
	}
	return "discord rejected the request"
}

func restAPICode(re *discordgo.RESTError) string {
	if re.Message != nil && re.Message.Code != 0 {
		return strconv.Itoa(re.Message.Code)
	}
	var m struct {
		Code int `json:"code"`
	}
	if len(re.ResponseBody) > 0 && json.Unmarshal(re.ResponseBody, &m) == nil && m.Code != 0 {
		return strconv.Itoa(m.Code)
	}
	return ""
}
