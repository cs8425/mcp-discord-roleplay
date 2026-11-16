// Package tz centralises the timezone used for every timestamp this server
// hands to the model, plus small formatting helpers so all tools emit
// identical time strings.
package tz

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// Loc is the location all emitted timestamps are converted to.
// Set once at startup from --tz; default Asia/Taipei (the project's zone).
var Loc = time.FixedZone("Asia/Taipei", 8*3600)

// Set configures Loc. name accepts an IANA zone ("Asia/Taipei"), the keywords
// "UTC"/"GMT"/"Local", or a fixed offset ("+08:00", "-0500", "+8").
func Set(name string) error {
	s := strings.TrimSpace(name)
	if s == "" {
		return nil
	}
	switch strings.ToUpper(s) {
	case "UTC", "GMT":
		Loc = time.UTC
		return nil
	case "LOCAL":
		Loc = time.Local
		return nil
	}
	if off, ok := parseOffset(s); ok {
		Loc = time.FixedZone(s, off)
		return nil
	}
	l, err := time.LoadLocation(s)
	if err != nil {
		return fmt.Errorf("invalid timezone %q: %w", s, err)
	}
	Loc = l
	return nil
}

// parseOffset understands +H, +HH, +HHMM and +HH:MM.
func parseOffset(s string) (int, bool) {
	if len(s) < 2 || (s[0] != '+' && s[0] != '-') {
		return 0, false
	}
	body := s[1:]
	var hh, mm int
	switch len(body) {
	case 1, 2: // +8, +08
		h, err := strconv.Atoi(body)
		if err != nil {
			return 0, false
		}
		hh = h
	case 4: // +0800
		h, err := strconv.Atoi(body[:2])
		if err != nil {
			return 0, false
		}
		m, err := strconv.Atoi(body[2:])
		if err != nil {
			return 0, false
		}
		hh, mm = h, m
	case 5: // +08:00
		if body[2] != ':' {
			return 0, false
		}
		h, err := strconv.Atoi(body[:2])
		if err != nil {
			return 0, false
		}
		m, err := strconv.Atoi(body[3:])
		if err != nil {
			return 0, false
		}
		hh, mm = h, m
	default:
		return 0, false
	}
	if hh > 23 || mm > 59 {
		return 0, false
	}
	off := hh*3600 + mm*60
	if s[0] == '-' {
		off = -off
	}
	return off, true
}

// Format renders t in the configured zone as RFC3339 (second precision).
// The zero time formats as "".
func Format(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.In(Loc).Format(time.RFC3339)
}

// Now returns the current time in the configured zone.
func Now() time.Time { return time.Now().In(Loc) }

// AgeS returns seconds elapsed since t rounded to milliseconds.
// A zero time yields -1 so callers can tell "never" from "just now".
func AgeS(t time.Time) float64 {
	if t.IsZero() {
		return -1
	}
	d := time.Since(t).Seconds()
	if d < 0 {
		d = 0
	}
	return math.Round(d*1000) / 1000
}
