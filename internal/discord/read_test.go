package discord

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestReadErrorsSpeakOfReading: a read that Discord refuses must not answer with
// send words. The error numbers are shared -- 50001 is "Cannot send messages to
// this user" when you post and "Missing Access" when you read a channel the bot may
// not view -- so the code has to be translated along with the sentence. An agent
// that sees "cannot_send_to_channel" after a history call starts hunting for a write
// permission it already has, while the truth is that we may not even look here.
func TestReadErrorsSpeakOfReading(t *testing.T) {
	c, buf := logClient(slog.LevelWarn)

	se := c.readError("history", restErr(http.StatusForbidden, 50001, "Missing Access"), time.Now())
	if se.Code != CodeNoAccess {
		t.Errorf("code: got %q, want %q", se.Code, CodeNoAccess)
	}
	if !strings.Contains(se.Msg, "read") {
		t.Errorf("the hint is all the agent gets, and it said nothing about reading: %q", se.Msg)
	}
	if strings.Contains(se.Msg, "send") {
		t.Errorf("a read failure must not borrow send words: %q", se.Msg)
	}
	if !strings.Contains(se.Msg, "Missing Access") {
		t.Errorf("Discord's own message should survive the translation: %q", se.Msg)
	}

	// The deleted channel is the other answer of this shape (50008), and it means the
	// same thing to a reader.
	if got := c.readError("get", restErr(http.StatusForbidden, 50008, "This channel was deleted"), time.Now()).Code; got != CodeNoAccess {
		t.Errorf("a deleted channel is a read failure too, not a failed send: %q", got)
	}

	// Codes that are already neutral stay put: what we are saving is the model's
	// vocabulary, and a synonym for "unknown_channel" would teach it twice.
	if got := c.readError("history", restErr(http.StatusNotFound, 10003, "Unknown Channel"), time.Now()).Code; got != CodeUnknownChannel {
		t.Errorf("404 should stay %q, got %q", CodeUnknownChannel, got)
	}

	// A refusal that arrives with no message at all still gets a sentence: an empty
	// hint is a hint nobody can act on.
	if got := c.readError("history", restErr(http.StatusForbidden, 50001, ""), time.Now()); !strings.Contains(got.Msg, "no access") {
		t.Errorf("empty Discord message: %q", got.Msg)
	}

	if !strings.Contains(buf.String(), "rest history failed") {
		t.Errorf("the WARN is the thing that says this cost a round trip: %s", buf.String())
	}
}

// TestReadTimeoutKeepsItsOwnWords: classify writes "send timed out" for a deadlined
// call, and the read branch replaces that with our own timeout -- the one number the
// agent can compare against -- without dragging it through the no_access branch.
func TestReadTimeoutKeepsItsOwnWords(t *testing.T) {
	c, _ := logClient(slog.LevelWarn)
	se := c.readError("history", context.DeadlineExceeded, time.Now())
	if se.Code != CodeCancelled {
		t.Fatalf("a deadlined read should still be %q, got %q (%s)", CodeCancelled, se.Code, se.Msg)
	}
	if !strings.Contains(se.Msg, "timed out after") {
		t.Errorf("the hint should name our timeout: %q", se.Msg)
	}
	if strings.Contains(se.Msg, "send") {
		t.Errorf("classify said %q and the read path did not correct it: %q", "send timed out", se.Msg)
	}
}
