package config

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

// LoadDotEnv reads a .env style file and applies KEY=VALUE pairs to the
// process environment WITHOUT overriding variables that are already set — the
// real environment always wins, so `DISCORD_TOKEN=... mcp-discord-roleplay`
// beats a stale value in ./.env.
//
// Supported syntax (deliberately tiny; we refuse to pull in a dependency for
// this): blank lines, `#` comments, optional `export ` prefix, unquoted or
// single/double quoted values, `\n`/`\t`/`\r`/`\\`/`\"` escapes inside double
// quotes. Inline comments after an unquoted value are stripped.
//
// It returns the list of keys it actually set (never their values).
func LoadDotEnv(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var set []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	line := 0
	for sc.Scan() {
		line++
		l := strings.TrimSpace(sc.Text())
		if l == "" || strings.HasPrefix(l, "#") || strings.HasPrefix(l, ";") {
			continue
		}
		l = strings.TrimPrefix(l, "export ")
		l = strings.TrimSpace(l)
		eq := strings.IndexByte(l, '=')
		if eq <= 0 {
			return set, fmt.Errorf("%s:%d: expected KEY=VALUE", path, line)
		}
		key := strings.TrimSpace(l[:eq])
		if !validKey(key) {
			return set, fmt.Errorf("%s:%d: invalid key %q", path, line, key)
		}
		val := strings.TrimSpace(l[eq+1:])
		val = unquote(val)
		if _, ok := os.LookupEnv(key); ok {
			continue // real environment wins
		}
		if err := os.Setenv(key, val); err != nil {
			return set, fmt.Errorf("%s:%d: %w", path, line, err)
		}
		set = append(set, key)
	}
	if err := sc.Err(); err != nil {
		return set, fmt.Errorf("%s: %w", path, err)
	}
	return set, nil
}

func validKey(k string) bool {
	if k == "" {
		return false
	}
	for i, r := range k {
		switch {
		case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z', r == '_':
		case i > 0 && r >= '0' && r <= '9':
		default:
			return false
		}
	}
	return true
}

func unquote(v string) string {
	if len(v) >= 2 {
		q := v[0]
		if v[len(v)-1] == q && (q == '"' || q == '\'') {
			inner := v[1 : len(v)-1]
			if q == '\'' {
				return inner
			}
			return unescape(inner)
		}
	}
	// unquoted: strip trailing comment
	if i := strings.Index(v, " #"); i >= 0 {
		return strings.TrimSpace(v[:i])
	}
	return v
}

func unescape(s string) string {
	if !strings.ContainsRune(s, '\\') {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' || i+1 >= len(s) {
			b.WriteByte(s[i])
			continue
		}
		i++
		switch s[i] {
		case 'n':
			b.WriteByte('\n')
		case 't':
			b.WriteByte('\t')
		case 'r':
			b.WriteByte('\r')
		case '\\':
			b.WriteByte('\\')
		case '"':
			b.WriteByte('"')
		default:
			b.WriteByte('\\')
			b.WriteByte(s[i])
		}
	}
	return b.String()
}
