package files

import (
	"errors"
	"io"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"mcp-discord-roleplay/internal/config"
)

// newStore builds a Store through the real config loader, so the flag names the
// operator types are what the tests exercise.
func newStore(t *testing.T, extra ...string) *Store {
	t.Helper()
	args := append([]string{"-selftest", "-config=" + filepath.Join(t.TempDir(), "absent.json")}, extra...)
	cfg, err := config.Load(args)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	return New(cfg, nil)
}

// mustWrite puts content at rel under root and returns the full path.
func mustWrite(t *testing.T, root, rel, content string) string {
	t.Helper()
	p := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func codeOf(t *testing.T, err error) string {
	t.Helper()
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	var fe *Error
	if !errors.As(err, &fe) {
		t.Fatalf("error is not a *files.Error: %T %v", err, err)
	}
	return fe.Code
}

func TestToRootPath(t *testing.T) {
	// The whole path policy hangs on this one mapping, so pin it down directly.
	cases := []struct{ in, want string }{
		{"x", "x"},
		{"./x", "x"},
		{"/x", "x"},
		{"//x", "x"},
		{"x/", "x"},
		{"x/.", "x"},
		{"a/../b", "b"},
		{"/a/../b", "b"},
		{"./a/../a/x", "a/x"},
		{"a/../../x", "../x"},
		{"/../etc/passwd", "etc/passwd"},
		{".", "."},
		{"/", "."},
		{"//", "."},
		{"", "."},
		{"   ", "."},
		{"~/x", "~/x"},
	}
	for _, c := range cases {
		if got := toRootPath(c.in); got != c.want {
			t.Errorf("toRootPath(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestPathPolicyRejectsEverythingOutside(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, root, "inside.txt", "hello")
	mustWrite(t, root, "sub/nested.txt", "nested")
	s := newStore(t, "-file-root", root)

	bad := []struct {
		path string
		want string
	}{
		{"", CodeEmpty},
		{"   ", CodeEmpty},
		{"../outside.txt", CodeEscapes}, // parent
		// Only a ".." that survives cleaning leaves the root, and that is the only
		// kind we refuse. "~" is never expanded.
		{"a/../../x", CodeEscapes},
		{"sub/../../x", CodeEscapes},
		{"~/secret.txt", CodeEscapes},
		{"~", CodeEscapes},
		{"~/.ssh/id_ed25519", CodeEscapes},
		{"/", CodeNotAFile}, // the root itself is not a file
		{".", CodeNotAFile},
		{"./", CodeNotAFile},
		{"//", CodeNotAFile},
	}
	for _, c := range bad {
		if _, err := s.Open(c.path, "", ""); codeOf(t, err) != c.want {
			t.Errorf("Open(%q) code = %q, want %q", c.path, codeOf(t, err), c.want)
		}
	}

	// '/' is cosmetic: it names the root, never the host filesystem. So the
	// classic "send me /etc/passwd" is just a lookup of etc/passwd under the
	// roots, and the classic absolute host path is a lookup that misses. Both are
	// refusals, but honest ones: no bypass, and the answer says where it looked.
	if runtime.GOOS != "windows" {
		for _, p := range []string{"/etc/passwd", "/../etc/passwd", "/" + filepath.Join(root, "inside.txt")} {
			if _, err := s.Open(p, "", ""); codeOf(t, err) != CodeNotFound {
				t.Errorf("Open(%q) code = %q, want %q (must miss inside the sandbox, not escape it)",
					p, codeOf(t, err), CodeNotFound)
			}
		}
	}

	// What IS allowed, and worth pinning down: dot-relative, slash-prefixed,
	// subdirectory and collapsed-".." forms all name a file inside the root.
	for _, ok := range []string{
		"inside.txt", "./inside.txt", "/inside.txt", "//inside.txt", "inside.txt/", "/./inside.txt",
		"./sub/../inside.txt", "sub/nested.txt", "/sub/../sub/nested.txt",
	} {
		o, err := s.Open(ok, "", "")
		if err != nil {
			t.Errorf("Open(%q) failed: %v", ok, err)
			continue
		}
		// The name we report is the file's base name inside the root, with the
		// cosmetic leading slashes gone. Note this is path.Base on a slashed string,
		// NOT filepath.Base: on Windows "//inside.txt" reads as a UNC server with no
		// share, so filepath.Base returns "\\" and the assertion would fail on a
		// correct answer (live: GHA windows job, 2026-11-20).
		if want := path.Base(filepath.ToSlash(ok)); o.Name != want {
			t.Errorf("Open(%q) name = %q, want %q", ok, o.Name, want)
		}
		_ = o.Close()
	}
}

func TestRootsAreTriedInDeclaredOrder(t *testing.T) {
	a := t.TempDir()
	b := t.TempDir()
	mustWrite(t, a, "dup.txt", "from-A")
	mustWrite(t, b, "dup.txt", "from-B")
	mustWrite(t, b, "only-b.txt", "b")

	s := newStore(t, "-file-root", a, "-file-root", b)

	o, err := s.Open("dup.txt", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if o.Root != a {
		t.Errorf("first declared root must win: Root=%q want %q", o.Root, a)
	}
	if o.Name != "dup.txt" {
		t.Errorf("Name = %q", o.Name)
	}
	got, _ := io.ReadAll(o.Reader())
	_ = o.Close()
	if string(got) != "from-A" {
		t.Errorf("content = %q, want the copy from the first root", got)
	}

	// A path only the second root holds still resolves.
	o2, err := s.Open("only-b.txt", "", "")
	if err != nil {
		t.Fatalf("second root should be searched: %v", err)
	}
	if o2.Root != b {
		t.Errorf("Root = %q, want %q", o2.Root, b)
	}
	_ = o2.Close()

	// Flip the order and the other copy wins — that is the whole contract.
	flipped := newStore(t, "-file-root", b, "-file-root", a)
	o3, err := flipped.Open("dup.txt", "", "")
	if err != nil {
		t.Fatal(err)
	}
	defer o3.Close()
	if o3.Root != b {
		t.Errorf("declaration order must decide: Root=%q want %q", o3.Root, b)
	}
}

func TestNotFoundSaysWhereItLooked(t *testing.T) {
	root := t.TempDir()
	s := newStore(t, "-file-root", root)
	_, err := s.Open("ghost.png", "", "")
	if codeOf(t, err) != CodeNotFound {
		t.Fatalf("want %s, got %q", CodeNotFound, err)
	}
	if !strings.Contains(err.Error(), root) {
		t.Errorf("the message should name the roots so a mistake is debuggable: %v", err)
	}
}

func TestDirectoryIsNotAFile(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	s := newStore(t, "-file-root", root)
	if _, err := s.Open("sub", "", ""); codeOf(t, err) != CodeNotAFile {
		t.Fatalf("want %s, got %q", CodeNotAFile, err)
	}
	if _, err := s.Open("sub/", "", ""); codeOf(t, err) != CodeNotAFile {
		t.Fatalf("trailing slash: want %s, got %q", CodeNotAFile, err)
	}
}

func TestMaxUploadBytesIsEnforced(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, root, "big.bin", strings.Repeat("x", 4096))
	s := newStore(t, "-file-root", root, "-max-upload-bytes", "1024")

	_, err := s.Open("big.bin", "", "")
	if codeOf(t, err) != CodeTooLarge {
		t.Fatalf("want %s, got %q", CodeTooLarge, err)
	}
	if !strings.Contains(err.Error(), "KiB") {
		t.Errorf("the message should humanize sizes: %v", err)
	}

	small := newStore(t, "-file-root", root, "-max-upload-bytes", "8192")
	o, err := small.Open("big.bin", "", "")
	if err != nil {
		t.Fatalf("under the cap it must open: %v", err)
	}
	defer o.Close()
	if o.Size != 4096 {
		t.Errorf("Size = %d, want 4096", o.Size)
	}
}

func TestNoFileRootDisablesLocalFiles(t *testing.T) {
	s := newStore(t, "-no-file-root")
	if s.Enabled() {
		t.Fatal("Enabled() must be false with --no-file-root")
	}
	if _, err := s.Open("anything.txt", "", ""); codeOf(t, err) != CodeDisabled {
		t.Fatalf("want %s, got %q", CodeDisabled, err)
	}
	// A nil store must answer the same way instead of panicking: main can hand
	// tools.Deps a nil Files when the feature is off.
	var nilStore *Store
	if _, err := nilStore.Open("x.txt", "", ""); codeOf(t, err) != CodeDisabled {
		t.Fatalf("nil store: want %s, got %q", CodeDisabled, err)
	}
	if nilStore.Enabled() {
		t.Error("nil store must report Enabled() == false")
	}
}

func TestSymlinkOutOfTheRootIsRefused(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation on Windows needs privileges")
	}
	root := t.TempDir()
	outside := t.TempDir()
	secret := mustWrite(t, outside, "secret.txt", "do not send me")
	if err := os.Symlink(secret, filepath.Join(root, "link.txt")); err != nil {
		t.Skipf("cannot symlink here: %v", err)
	}
	s := newStore(t, "-file-root", root)
	if o, err := s.Open("link.txt", "", ""); err == nil {
		_ = o.Close()
		t.Fatal("a symlink leaving the root must not be readable")
	}

	// Absolute links are refused by os.Root as well; relative escape too.
	if err := os.Symlink(filepath.Join("..", "outside2.txt"), filepath.Join(root, "rel.txt")); err == nil {
		if o, err := s.Open("rel.txt", "", ""); err == nil {
			_ = o.Close()
			t.Error("a relative symlink escaping the root must not be readable")
		}
	}
}

func TestSymlinkWithinTheRootWorks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation on Windows needs privileges")
	}
	root := t.TempDir()
	mustWrite(t, root, "real/pose.png", "pngbytes")
	if err := os.Symlink("real/pose.png", filepath.Join(root, "current.png")); err != nil {
		t.Skipf("cannot symlink here: %v", err)
	}
	s := newStore(t, "-file-root", root)
	o, err := s.Open("current.png", "", "")
	if err != nil {
		t.Fatalf("a link that stays inside the root is fine: %v", err)
	}
	defer o.Close()
	if o.Name != "current.png" {
		t.Errorf("Name = %q, want the requested name", o.Name)
	}
	if got, _ := io.ReadAll(o.Reader()); string(got) != "pngbytes" {
		t.Errorf("link content = %q", got)
	}
}

func TestSafeName(t *testing.T) {
	cases := []struct{ in, want string }{
		{"pose.png", "pose.png"},
		{"  spaced.png  ", "spaced.png"},
		{"../../etc/passwd", "passwd"},
		{`C:\evil\shell.exe`, "shell.exe"},
		{`weird*?"<>|name.txt`, "weird______name.txt"},
		{"tab\tname", "tab_name"}, // control characters are a boundary, not a vanish
		{".", "file.bin"},
		{"..", "file.bin"},
		{"...", "file.bin"},
		{"", "file.bin"},
		{".hidden.png", "hidden.png"},
	}
	for _, c := range cases {
		if got := SafeName(c.in); got != c.want {
			t.Errorf("SafeName(%q) = %q, want %q", c.in, got, c.want)
		}
	}

	// Whatever comes out must be a single, boring filename.
	for _, in := range []string{"/", "..", "a/b/c", `x\y`, "nul\x00byte", "   ", strings.Repeat("!", 400)} {
		got := SafeName(in)
		if got == "" {
			t.Errorf("SafeName(%q) is empty", in)
			continue
		}
		if strings.ContainsAny(got, `/\\*?"<>|`) {
			t.Errorf("SafeName(%q) = %q still has a dangerous character", in, got)
		}
		if strings.HasPrefix(got, ".") {
			t.Errorf("SafeName(%q) = %q starts with a dot", in, got)
		}
		if n := len([]rune(got)); n > 120 {
			t.Errorf("SafeName(%q) = %q is %d runes, over the cap", in, got, n)
		}
	}

	// The extension must survive a truncation, or Discord shows a nameless blob.
	long := strings.Repeat("名", 300) + ".png"
	got := SafeName(long)
	if !strings.HasSuffix(got, ".png") {
		t.Errorf("SafeName truncation dropped the extension: %q", got)
	}
	if n := len([]rune(got)); n > 120 {
		t.Errorf("SafeName truncation left %d runes", n)
	}
}

func TestContentTypePriority(t *testing.T) {
	root := t.TempDir()
	// PNG magic, but a name that claims nothing.
	png := []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a, 'x', 'y', 'z'}
	if err := os.WriteFile(filepath.Join(root, "mystery.bin"), png, 0o600); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, root, "notes.txt", "just text")
	mustWrite(t, root, "raw", "no extension at all")
	s := newStore(t, "-file-root", root)

	// 1. the caller's mime wins
	o, err := s.Open("notes.txt", "", "image/png")
	if err != nil {
		t.Fatal(err)
	}
	if o.ContentType != "image/png" {
		t.Errorf("given mime must win, got %q", o.ContentType)
	}
	_ = o.Close()

	// 2. else the extension
	o, err = s.Open("notes.txt", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(o.ContentType, "text/plain") {
		t.Errorf("extension detection: got %q", o.ContentType)
	}
	_ = o.Close()

	// 3. else the bytes
	o, err = s.Open("mystery.bin", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if o.ContentType != "image/png" {
		t.Errorf("sniffing a PNG must give image/png, got %q", o.ContentType)
	}
	if off, err := o.f.Seek(0, io.SeekCurrent); err != nil || off != 0 {
		t.Errorf("the sniff must be undone (offset %d, err %v)", off, err)
	}
	_ = o.Close()

	// 4. else octet-stream
	o, err = s.Open("raw", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if o.ContentType == "" {
		t.Error("content type must never be empty: multipart needs one")
	}
	_ = o.Close()

	// application/octet-stream as a *given* mime is a shrug, not an answer: we
	// should still try the extension.
	o, err = s.Open("notes.txt", "", "application/octet-stream")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(o.ContentType, "text/plain") {
		t.Errorf("octet-stream should fall through to detection, got %q", o.ContentType)
	}
	_ = o.Close()
}

func TestRewindRestartsTheReader(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, root, "f.txt", "0123456789")
	s := newStore(t, "-file-root", root)
	o, err := s.Open("f.txt", "", "")
	if err != nil {
		t.Fatal(err)
	}
	defer o.Close()

	buf := make([]byte, 4)
	if _, err := io.ReadFull(o.Reader(), buf); err != nil {
		t.Fatal(err)
	}
	if string(buf) != "0123" {
		t.Fatalf("first read = %q", buf)
	}
	if err := o.Rewind(); err != nil {
		t.Fatal(err)
	}
	again := make([]byte, 4)
	if _, err := io.ReadFull(o.Reader(), again); err != nil {
		t.Fatal(err)
	}
	if string(again) != "0123" {
		t.Errorf("after Rewind the upload must start over, got %q", again)
	}
}

func TestValidateURL(t *testing.T) {
	good := []struct{ in, want string }{
		{"https://cdn.example.com/a.png", "https://cdn.example.com/a.png"},
		{"  http://example.com  ", "http://example.com"},
		{"https://example.com/a%20b?q=1#f", "https://example.com/a%20b?q=1#f"},
	}
	for _, c := range good {
		got, err := ValidateURL(c.in)
		if err != nil {
			t.Errorf("ValidateURL(%q) failed: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("ValidateURL(%q) = %q, want %q", c.in, got, c.want)
		}
	}

	bad := []string{
		"",
		"   ",
		"file:///etc/passwd",
		"data:text/html,<script>alert(1)</script>",
		"javascript:alert(1)",
		"ftp://example.com/x",
		"//example.com/x", // scheme-relative: no scheme, no thank you
		"https://",
		"http:///nohost",
		"https://example.com/a b",
		"https://example.com/a\nBROKEN",
		"https://example.com/a\tb",
		strings.Repeat("a", 2100),
	}
	for _, in := range bad {
		if got, err := ValidateURL(in); err == nil {
			t.Errorf("ValidateURL(%q) accepted %q", in, got)
		} else if codeOf(t, err) != CodeBadURL {
			t.Errorf("ValidateURL(%q) code = %q, want %s", in, codeOf(t, err), CodeBadURL)
		}
	}
}

func TestRefIsURL(t *testing.T) {
	cases := []struct {
		r    Ref
		want bool
	}{
		{Ref{URL: "https://x/y"}, true},
		{Ref{Path: "y"}, false},
		{Ref{Path: "y", URL: "https://x/y"}, false}, // path wins: it is an upload
		{Ref{Path: "  ", URL: " https://x/y "}, true},
		{Ref{}, false},
	}
	for _, c := range cases {
		if got := c.r.IsURL(); got != c.want {
			t.Errorf("%+v IsURL = %v, want %v", c.r, got, c.want)
		}
	}
}

func TestOpenRefUsesPathWhenPresent(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, root, "pic.png", "bytes")
	s := newStore(t, "-file-root", root)

	o, err := s.OpenRef(Ref{Path: "pic.png", Name: "renamed.png"})
	if err != nil {
		t.Fatal(err)
	}
	defer o.Close()
	if o.Name != "renamed.png" {
		t.Errorf("explicit name must be honoured: %q", o.Name)
	}
	if o.Ref != "pic.png" {
		t.Errorf("Ref should echo what was asked: %q", o.Ref)
	}

	if _, err := s.OpenRef(Ref{URL: "https://x/y"}); codeOf(t, err) != CodeEmpty {
		t.Errorf("a URL ref must not reach the filesystem: got %v", err)
	}
}
