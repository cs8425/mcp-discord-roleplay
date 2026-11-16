// Package files is the only door between this process and the local
// filesystem. It exists so one rule is enforced in exactly one place: the model
// may name a file, it may never choose where that file comes from.
//
// A path handed to Open is always interpreted RELATIVE TO a --file-root
// directory, using plain filepath rules, and is opened through os.Root:
//
//   - '/' is the only separator we care about, on every OS, and a leading '/' is
//     cosmetic: "/pose.png" names the same file as "pose.png", because the roots
//     are the whole namespace. The name is Clean()ed first, so "." and ".."
//     elements resolve ("./a/../b" is just "b"); what is left is then checked
//     with filepath.IsLocal, which rejects a surviving "..", and separately "~/",
//     which we never expand;
//   - os.Root then rejects any component that resolves outside the root,
//     including through symlinks ("Methods on Root will follow symbolic links,
//     but symbolic links may not reference a location outside the root.
//     Symbolic links must not be absolute." — $GOROOT/src/os/root.go). We
//     therefore do not implement a link walker of our own: the kernel does it,
//     without the check-then-use race a hand-rolled one would have.
//
// What os.Root does NOT do: it does not stop at filesystem boundaries and does
// not block device nodes or /proc. Hence the IsRegular() check below, and hence
// the selftest warning about mounting "/".
//
// URLs are validated here but never fetched here. An attachment is either read
// off disk or pasted into the message as a link and downloaded by Discord, so
// this process makes no outbound request other than to the Discord API.
package files

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"mcp-discord-roleplay/internal/config"
)

// Machine-readable error codes. They belong to the tool contract — the model is
// told these names and branches on them — so keep them stable and distinct.
const (
	CodeDisabled = "files_disabled" // no --file-root in effect
	CodeEmpty    = "empty_path"
	CodeEscapes  = "path_escapes" // "..", "~", volume name
	CodeNotFound = "not_found"
	CodeNotAFile = "not_a_file" // directory, device, fifo, socket
	CodeTooLarge = "too_large"  // over --max-upload-bytes
	CodeTooMany  = "too_many_files"
	CodeBadURL   = "bad_url"     // not a clean http(s) link
	CodeOpen     = "open_failed" // permission, vanished root, link that escaped
)

// Error carries the code the model sees. Path echoes the requested value so a
// rejection is self-explanatory; the root list is included in not_found so an
// operator can see where we actually looked.
type Error struct {
	Code string
	Msg  string
	Path string
}

func (e *Error) Error() string {
	if e.Path == "" {
		return e.Code + ": " + e.Msg
	}
	return e.Code + ": " + e.Msg + " (" + e.Path + ")"
}

func errf(code, p, format string, a ...any) *Error {
	return &Error{Code: code, Msg: fmt.Sprintf(format, a...), Path: p}
}

// CodeOf returns the tool-level code of an error from this package, or "".
func CodeOf(err error) string {
	var fe *Error
	if errors.As(err, &fe) {
		return fe.Code
	}
	return ""
}

// Ref is one entry of discord_send.files, exactly as the model writes it.
// Path and URL are mutually exclusive; Path wins.
type Ref struct {
	Name string `json:"name,omitempty"`
	Path string `json:"path,omitempty"`
	URL  string `json:"url,omitempty"`
	MIME string `json:"mime,omitempty"`
}

// IsURL reports whether this entry is a link to paste rather than a file to
// read from disk.
func (r Ref) IsURL() bool {
	return strings.TrimSpace(r.Path) == "" && strings.TrimSpace(r.URL) != ""
}

// Opened is an attachment that has passed every check and is ready to upload.
// The caller owns it and must Close it. Rewind is called before a retry, since
// the first attempt already drained the reader.
type Opened struct {
	Ref         string // path as requested
	Root        string // which --file-root matched it
	Name        string // sanitized filename handed to Discord
	ContentType string
	Size        int64

	f *os.File
}

// Reader hands the open file to discordgo. It stays valid after the Root is
// closed, because it holds its own descriptor.
func (o *Opened) Reader() io.Reader { return o.f }

// Rewind seeks to the start so a rate-limit retry re-sends the bytes instead of
// an empty file.
func (o *Opened) Rewind() error {
	if _, err := o.f.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("rewind %s: %w", o.Name, err)
	}
	return nil
}

// Close releases the descriptor. Calling it twice is harmless.
func (o *Opened) Close() error {
	if o == nil || o.f == nil {
		return nil
	}
	err := o.f.Close()
	o.f = nil
	return err
}

// Store resolves attachment paths against the configured roots. It is immutable
// after New and safe for concurrent use.
type Store struct {
	roots    []string
	maxBytes int64
	log      *slog.Logger
}

// New builds the store from the resolved configuration. config.Load already
// checked that every root exists and is a directory; roots that vanish later
// are reported per-open rather than fatal at startup.
func New(cfg *config.Config, log *slog.Logger) *Store {
	if log == nil {
		log = slog.Default()
	}
	return &Store{
		roots:    append([]string(nil), cfg.FileRoots...),
		maxBytes: int64(cfg.MaxUploadBytes),
		log:      log,
	}
}

// Enabled reports whether local files may be read at all.
func (s *Store) Enabled() bool { return s != nil && len(s.roots) > 0 }

// Roots returns the --file-root directories in the order they are tried.
func (s *Store) Roots() []string { return append([]string(nil), s.roots...) }

// MaxBytes is the per-file upload ceiling (0 = unchecked).
func (s *Store) MaxBytes() int64 { return s.maxBytes }

// toRootPath turns whatever the caller typed into the name we look up inside a
// root: '/' only, on every OS, cleaned so "." and ".." resolve, with the leading
// '/' (the root itself) removed.
//
//	"x"           -> "x"
//	"./x"         -> "x"
//	"/x"          -> "x"
//	"x/"          -> "x"
//	"a/../b"      -> "b"      // inside a root, so it is simply b
//	"/a/../b"     -> "b"
//	"a/../../x"   -> "../x"   // still escapes; the caller refuses it
//	"."           -> "."
//	"/"           -> "."
//	""            -> "."
//	"~/x"         -> "~/x"    // the '~' check below still catches it
//
// path.Clean rather than filepath.Clean on purpose: filepath treats '\\' as a
// separator on Windows, and we promised '/' only. ToSlash runs first so that on
// Windows a back-slashed path from a sloppy client reads as separators; on POSIX
// a backslash is an ordinary filename character and stays one.
func toRootPath(raw string) string {
	s := path.Clean(filepath.ToSlash(strings.TrimSpace(raw)))
	s = strings.TrimPrefix(s, "/")
	if s == "" {
		// path.Clean("/") keeps the leading slash, so a bare "/" would empty out.
		// "." is the honest answer: it names the root, not a file inside it.
		return "."
	}
	return s
}

// OpenRef opens the file part of a Ref (a URL Ref is an error: it is pasted,
// never read).
func (s *Store) OpenRef(r Ref) (*Opened, error) {
	return s.Open(r.Path, r.Name, r.MIME)
}

// Open resolves a path under the --file-root directories and returns it ready
// for upload. Roots are tried in declaration order and the first hit wins;
// Opened.Root says which one, so "why did it send THAT file" is always
// answerable afterwards.
func (s *Store) Open(req, givenName, givenMIME string) (*Opened, error) {
	if strings.TrimSpace(req) == "" {
		return nil, errf(CodeEmpty, req, "path is empty")
	}
	if !s.Enabled() {
		return nil, errf(CodeDisabled, req, "local files are disabled: no --file-root is in effect")
	}
	// Separator policy: '/' only, on every OS. Clean first (so "./a/../b" means
	// "b", the way the caller meant it), then drop the leading '/', which carries
	// no information here: the roots ARE the whole namespace.
	p := toRootPath(req)
	if p == "" || p == "." {
		return nil, errf(CodeNotAFile, req,
			"that names a file root itself, not a file inside one (roots: %s)", strings.Join(s.roots, ", "))
	}
	// IsLocal rejects a ".." that survived cleaning, and volume names. "~/x" IS a
	// legal relative path to IsLocal, so "~" gets its own check: we never expand it.
	if !filepath.IsLocal(p) || strings.HasPrefix(p, "~") {
		return nil, errf(CodeEscapes, req,
			"path must stay inside a --file-root directory: no '..', no volume name, no '~' "+
				"(we do not expand it); a leading '/' is fine and means the root; resolved to %q", p)
	}

	var (
		lastErr error
		sawMiss bool
		sawDir  bool
	)
	for _, root := range s.roots {
		rt, err := os.OpenRoot(root)
		if err != nil {
			lastErr = err
			continue
		}
		f, err := rt.Open(p)
		// Closing the Root does not invalidate f: it has its own descriptor.
		_ = rt.Close()
		if err != nil {
			lastErr = err
			if errors.Is(err, os.ErrNotExist) {
				sawMiss = true
			} else {
				// Permission denied, or a symlink that tried to leave the root.
				s.log.Warn("attachment open rejected", "root", root, "path", p, "err", err)
			}
			continue
		}
		st, err := f.Stat()
		if err != nil {
			_ = f.Close()
			lastErr = err
			continue
		}
		if !st.Mode().IsRegular() {
			// os.Root does not block devices or /proc, so that gate is ours.
			_ = f.Close()
			sawDir = true
			continue
		}
		if s.maxBytes > 0 && st.Size() > s.maxBytes {
			_ = f.Close()
			return nil, errf(CodeTooLarge, req, "file is %s, over the --max-upload-bytes limit of %s",
				humanBytes(st.Size()), humanBytes(s.maxBytes))
		}

		name := SafeName(firstNonEmpty(givenName, filepath.Base(p)))
		o := &Opened{
			Ref:  req,
			Root: root,
			Name: name,
			Size: st.Size(),
			f:    f,
		}
		o.ContentType = s.contentType(o, givenMIME)
		s.log.Debug("attachment opened", "root", root, "path", p, "size", st.Size(), "mime", o.ContentType)
		return o, nil
	}

	switch {
	case sawDir:
		return nil, errf(CodeNotAFile, req, "that path is a directory or not a regular file; name a file inside it")
	case sawMiss:
		return nil, errf(CodeNotFound, req, "no such file under the file roots (%s)", strings.Join(s.roots, ", "))
	default:
		if lastErr == nil {
			lastErr = os.ErrNotExist
		}
		return nil, errf(CodeOpen, req, "cannot open: %v", lastErr)
	}
}

// contentType resolves the upload content type as: caller's mime → filename
// extension → the file's own first bytes → application/octet-stream. The sniff
// read is undone with a Seek so the upload starts at offset 0.
func (s *Store) contentType(o *Opened, given string) string {
	if m := normalizeMIME(given); m != "" {
		return m
	}
	if ext := strings.ToLower(filepath.Ext(o.Name)); ext != "" {
		// TypeByExtension returns a single string; an unknown extension gives "".
		if t := mime.TypeByExtension(ext); t != "" && t != "application/octet-stream" {
			return t
		}
	}
	if t := s.sniff(o); t != "" {
		return t
	}
	return "application/octet-stream"
}

func (s *Store) sniff(o *Opened) string {
	buf := make([]byte, 512)
	n, err := o.f.Read(buf)
	out := ""
	if n > 0 {
		out = http.DetectContentType(buf[:n])
	}
	if err != nil && !errors.Is(err, io.EOF) {
		s.log.Warn("content sniff failed", "path", o.Ref, "err", err)
	}
	if _, serr := o.f.Seek(0, io.SeekStart); serr != nil {
		s.log.Warn("seek after sniff failed", "path", o.Ref, "err", serr)
	}
	return out
}

// ValidateURL accepts a link to paste into the message body verbatim. Only
// http/https, it must carry a host, and it may contain no whitespace or control
// character — a newline in there would smuggle extra lines into the message.
// Nothing is fetched, so there is no SSRF surface to guard.
func ValidateURL(raw string) (string, error) {
	u := strings.TrimSpace(raw)
	if u == "" {
		return "", errf(CodeBadURL, raw, "url is empty")
	}
	if len(u) > 2048 {
		return "", errf(CodeBadURL, raw, "url is longer than 2048 characters")
	}
	for _, r := range u {
		if r < 0x21 || r == 0x7f {
			return "", errf(CodeBadURL, raw, "url contains whitespace or a control character")
		}
	}
	parsed, err := url.Parse(u)
	if err != nil {
		return "", errf(CodeBadURL, raw, "unparsable url: %v", err)
	}
	switch strings.ToLower(parsed.Scheme) {
	case "http", "https":
	default:
		return "", errf(CodeBadURL, raw, "url scheme must be http or https, got %q", parsed.Scheme)
	}
	if parsed.Host == "" {
		return "", errf(CodeBadURL, raw, "url has no host")
	}
	return parsed.String(), nil
}

// SafeName turns a model-supplied name into something safe to hand Discord as an
// attachment filename: no directory components, none of the characters the
// filename rules dislike (they, and control characters, become '_'), capped
// length with the extension kept.
func SafeName(s string) string {
	const (
		bad      = `*?"<>|:/\`
		maxRunes = 120
	)
	s = strings.ReplaceAll(strings.TrimSpace(s), "\\", "/")
	s = path.Base(s)
	var b strings.Builder
	for _, r := range s {
		switch {
		case r < 0x20 || r == 0x7f:
			b.WriteRune('_') // a control character is a separator, not a vanish
		case strings.ContainsRune(bad, r):
			b.WriteRune('_')
		default:
			b.WriteRune(r)
		}
	}
	out := strings.Trim(strings.TrimSpace(b.String()), ".")
	if out == "" {
		return "file.bin"
	}
	if utf8.RuneCountInString(out) <= maxRunes {
		return out
	}
	ext := path.Ext(out)
	if utf8.RuneCountInString(ext) > 24 {
		ext = string([]rune(ext)[:24])
	}
	base := []rune(strings.TrimSuffix(out, ext))
	keep := maxRunes - utf8.RuneCountInString(ext)
	if keep < 1 {
		return string([]rune(ext)[:maxRunes])
	}
	if len(base) > keep {
		base = base[:keep]
	}
	return string(base) + ext
}

func normalizeMIME(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	mt, _, err := mime.ParseMediaType(s)
	if err != nil || mt == "" || mt == "application/octet-stream" {
		return ""
	}
	return strings.ToLower(mt)
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}
