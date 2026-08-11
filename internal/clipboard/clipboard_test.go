package clipboard

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// fakeForkingHelper writes a script that behaves the way xclip, xsel and
// wl-copy do: consume stdin, leave a background process holding the inherited
// file descriptors, and exit.
func fakeForkingHelper(t *testing.T, holdSeconds int) (script, captured string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("needs a POSIX shell")
	}
	dir := t.TempDir()
	script = filepath.Join(dir, "fake-xclip")
	captured = filepath.Join(dir, "captured")
	body := "#!/bin/sh\n" +
		"cat > \"" + captured + "\"\n" +
		fmt.Sprintf("sleep %d &\n", holdSeconds) + // inherits our streams, like a real clipboard owner
		"exit 0\n"
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	return script, captured
}

// The regression this guards: with a pipe-backed stderr, Run waits for the
// backgrounded child too, so every copy on X11/Wayland would stall until the
// timeout killed the helper. Detaching the streams fixes it.
func TestWriteWithDetachesFromForkingHelper(t *testing.T) {
	// The helper holds our streams for a long time, so the two outcomes are far
	// apart: a detached write returns in milliseconds, while an attached one
	// would wait the full hold. The threshold sits between them with room to
	// spare, since a loaded machine can take a while just to spawn a process.
	const hold = 10
	script, captured := fakeForkingHelper(t, hold)
	payload := []byte("copied on linux")

	start := time.Now()
	if err := writeWith(exec.Command(script), payload, true); err != nil {
		t.Fatalf("detached write failed: %v", err)
	}
	elapsed := time.Since(start)

	if elapsed > 4*time.Second {
		t.Fatalf("detached write blocked for %s against a helper holding the streams for %ds; "+
			"it should return as soon as the foreground process exits", elapsed, hold)
	}
	got, err := os.ReadFile(captured)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(payload) {
		t.Fatalf("helper received %q, want %q", got, payload)
	}
}

// The other half of the same story: without detaching, the call really does
// block. If this ever stops being true the flag can go away.
func TestWriteWithoutDetachingBlocksOnForkingHelper(t *testing.T) {
	if testing.Short() {
		t.Skip("timing test")
	}
	script, _ := fakeForkingHelper(t, 2)

	start := time.Now()
	if err := writeWith(exec.Command(script), []byte("x"), false); err != nil {
		t.Fatalf("attached write failed: %v", err)
	}
	if elapsed := time.Since(start); elapsed < 1200*time.Millisecond {
		t.Skipf("attached write returned in %s; this platform does not reproduce the inherited-pipe stall", elapsed)
	}
}

func TestWriteWithReportsFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs a POSIX shell")
	}
	dir := t.TempDir()
	script := filepath.Join(dir, "failing")
	body := "#!/bin/sh\ncat > /dev/null\necho 'no display' >&2\nexit 1\n"
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	err := writeWith(exec.Command(script), []byte("x"), false)
	if err == nil {
		t.Fatal("a failing helper was reported as success")
	}
	if got := err.Error(); got == "" {
		t.Fatal("error message was empty")
	}
}

func TestLooksEmpty(t *testing.T) {
	// "target STRING not available" used to be asserted as NOT empty, which is
	// what xclip actually says about an ordinary empty clipboard -- so every
	// empty clipboard on X11 surfaced as a hard error with a scary warning.
	empty := map[string]bool{
		"":                                     true,
		"   ":                                  true,
		"Error: target STRING not available":   true,
		"Error: target UTF8_STRING not availa": false, // truncated, and rightly unmatched
		"Error: target UTF8_STRING not available": true,
		"xclip: No selection":                     true,
		"wl-paste: Nothing is copied":             true,
		"No suitable type of content copied":      true,
		// Real failures, which must never be mistaken for an empty clipboard.
		"Error: Can't open display: (null)":           false,
		"wl-paste: failed to connect to a compositor": false,
		"emptying the trash":                          false, // the bare "empty" match was this broad
	}
	for s, want := range empty {
		if got := looksEmpty(s); got != want {
			t.Errorf("looksEmpty(%q) = %v, want %v", s, got, want)
		}
	}
}

// Wayland sessions must not be handed an XWayland clipboard the compositor
// does not share.
func TestWaylandIsPreferredOnWaylandSessions(t *testing.T) {
	if runtime.GOOS == "darwin" || runtime.GOOS == "windows" {
		t.Skip("unix session selection only")
	}
	t.Setenv("WAYLAND_DISPLAY", "wayland-0")
	if first := candidates()[0].name; first != "wl-clipboard" {
		t.Fatalf("on a Wayland session the first candidate is %q, want wl-clipboard", first)
	}
	t.Setenv("WAYLAND_DISPLAY", "")
	if first := candidates()[0].name; first != "xclip" {
		t.Fatalf("off Wayland the first candidate is %q, want xclip", first)
	}
}

// Every candidate must declare the binaries it needs, or resolve would pick a
// tool that is only half present.
func TestCandidatesDeclareTheirBinaries(t *testing.T) {
	for _, c := range candidates() {
		if len(c.needBins) == 0 {
			t.Errorf("%s declares no required binaries", c.name)
		}
		if c.readBin == "" || c.writeBin == "" {
			t.Errorf("%s is missing a read or write binary", c.name)
		}
	}
}

// useTool points the package at a helper of our own for one test.
func useTool(t *testing.T, tl tool) {
	t.Helper()
	resolveMu.Lock()
	prev, prevFor := resolved, resolvedFor
	resolved, resolvedFor = &tl, os.Getenv("WAYLAND_DISPLAY")
	resolveMu.Unlock()
	t.Cleanup(func() {
		resolveMu.Lock()
		resolved, resolvedFor = prev, prevFor
		resolveMu.Unlock()
	})
}

// writeScript puts an executable /bin/sh script in dir.
func writeScript(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

// The regression this guards is the one that hid everything else: a helper that
// fails without a word produces no output and no stderr, which looked exactly
// like an empty clipboard. Read then returned nil, nil, the daemon read that as
// success, cleared the recorded fault and logged that the clipboard was
// readable again -- so a permanently broken clipboard reported healthy and sync
// simply stopped.
func TestReadReportsSilentFailuresAsErrors(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs a POSIX shell")
	}
	dir := t.TempDir()

	cases := []struct {
		name    string
		bin     string
		wantErr bool
	}{
		{"silent non-zero exit", writeScript(t, dir, "silent", "exit 3\n"), true},
		{"killed outright", writeScript(t, dir, "killed", "kill -9 $$\n"), true},
		{"missing from PATH", filepath.Join(dir, "definitely-not-here"), true},
		// These two stay as they were: a helper that says the clipboard is
		// empty, and a helper that exits 0 with nothing to say, both mean there
		// is nothing to sync.
		{"helper says it is empty", writeScript(t, dir, "spoken", "echo 'Nothing is copied' >&2\nexit 1\n"), false},
		{"clean exit, no output", writeScript(t, dir, "quiet", "exit 0\n"), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			useTool(t, tool{name: "fake", readBin: c.bin, writeBin: c.bin})
			out, err := Read()
			switch {
			case c.wantErr && err == nil:
				t.Fatalf("Read returned (%q, nil); a failure must not be reported as an empty clipboard", out)
			case !c.wantErr && err != nil:
				t.Fatalf("Read returned %v, want an empty clipboard", err)
			}
		})
	}
}

// A helper that never returns is a failure too, and one the caller especially
// needs to hear about.
func TestReadReportsATimeout(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs a POSIX shell")
	}
	if testing.Short() {
		t.Skip("waits out the clipboard timeout")
	}
	dir := t.TempDir()
	// The helper wedges and leaves a child holding our stdout pipe, which is
	// what used to make Read outlast its own deadline by the child's lifetime.
	useTool(t, tool{name: "fake", readBin: writeScript(t, dir, "wedged", "sleep 60 &\nsleep 60\n")})

	start := time.Now()
	if out, err := Read(); err == nil {
		t.Fatalf("Read returned (%q, nil) after being killed at the deadline", out)
	}
	elapsed := time.Since(start)
	if elapsed < timeout {
		t.Errorf("Read gave up after %s, before the %s deadline", elapsed, timeout)
	}
	if elapsed > timeout+waitDelay+2*time.Second {
		t.Errorf("Read took %s to give up on a %s deadline; the helper's child is still holding the pipe", elapsed, timeout)
	}
}

// pbcopy and pbpaste choose their encoding from the locale, and a launchd job
// inherits none -- so the candidate has to carry one itself.
func TestDarwinCandidateSetsALocale(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS only")
	}
	c := candidates()[0]
	var found bool
	for _, kv := range c.env {
		if strings.HasPrefix(kv, "LC_ALL=") || strings.HasPrefix(kv, "LANG=") || strings.HasPrefix(kv, "LC_CTYPE=") {
			found = true
		}
	}
	if !found {
		t.Errorf("the %s candidate declares no locale (env = %v); non-ASCII text will be mangled under launchd", c.name, c.env)
	}
}

// resetResolve clears the cached tool for the duration of one test.
func resetResolve(t *testing.T) {
	t.Helper()
	resolveMu.Lock()
	prev, prevFor := resolved, resolvedFor
	resolved, resolvedFor = nil, ""
	resolveMu.Unlock()
	t.Cleanup(func() {
		resolveMu.Lock()
		resolved, resolvedFor = prev, prevFor
		resolveMu.Unlock()
	})
}

// A failure must never be remembered. Under launchd and systemd henri can start
// before the graphical session exists and before anything is installed, and
// caching that answer meant Read and Write returned ErrUnsupported for the rest
// of the process -- long after the user had installed the helper.
func TestResolveDoesNotCacheFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs a POSIX shell")
	}
	resetResolve(t)

	dir := t.TempDir()
	t.Setenv("PATH", dir)
	if _, err := resolve(); err == nil {
		t.Fatal("resolve found a clipboard tool on an empty PATH")
	}

	// The user installs the helper while henri is running.
	for _, bin := range candidates()[0].needBins {
		writeScript(t, dir, bin, "exit 0\n")
	}
	if _, err := resolve(); err != nil {
		t.Fatalf("resolve still fails after the helper appeared: %v", err)
	}
}

// The session decides which helper is right, and under a service manager it
// frequently arrives after henri does.
func TestResolveRefreshesWhenTheSessionAppears(t *testing.T) {
	if runtime.GOOS == "darwin" || runtime.GOOS == "windows" {
		t.Skip("unix session selection only")
	}
	resetResolve(t)

	dir := t.TempDir()
	t.Setenv("PATH", dir)
	for _, bin := range []string{"xclip", "xsel", "wl-paste", "wl-copy"} {
		writeScript(t, dir, bin, "exit 0\n")
	}

	t.Setenv("WAYLAND_DISPLAY", "")
	first, err := resolve()
	if err != nil {
		t.Fatal(err)
	}
	if first.name != "xclip" {
		t.Fatalf("off Wayland resolve picked %q, want xclip", first.name)
	}

	t.Setenv("WAYLAND_DISPLAY", "wayland-0")
	second, err := resolve()
	if err != nil {
		t.Fatal(err)
	}
	if second.name != "wl-clipboard" {
		t.Fatalf("the Wayland session appeared and resolve still says %q", second.name)
	}
}
