package clipboard

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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
	empty := []string{
		"",
		"   ",
		"Error: target STRING not available", // not matched below, see want=false
		"xclip: No selection",
		"wl-paste: Nothing is copied",
		"No suitable type of content copied",
	}
	want := []bool{true, true, false, true, true, true}
	for i, s := range empty {
		if got := looksEmpty(s); got != want[i] {
			t.Errorf("looksEmpty(%q) = %v, want %v", s, got, want[i])
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

// The UUID is the one part of GetElementAtIndex's reply henri parses by hand;
// the value itself goes through gpaste-client precisely so that a clipboard
// full of quotes and newlines never has to survive a GVariant literal.
func TestGPasteUUIDParsing(t *testing.T) {
	cases := map[string]string{
		`('4f1cfd11-8b8e-4d3a-9c1f-2a7b6e5d4c3b', 'hello')`:      "4f1cfd11-8b8e-4d3a-9c1f-2a7b6e5d4c3b",
		`('00000000-0000-0000-0000-000000000000', '')`:           "00000000-0000-0000-0000-000000000000",
		`('4f1cfd11-8b8e-4d3a-9c1f-2a7b6e5d4c3b', 'it\'s')`:      "4f1cfd11-8b8e-4d3a-9c1f-2a7b6e5d4c3b",
		`('4f1cfd11-8b8e-4d3a-9c1f-2a7b6e5d4c3b', 'a, b, c')`:    "4f1cfd11-8b8e-4d3a-9c1f-2a7b6e5d4c3b",
		`('4f1cfd11-8b8e-4d3a-9c1f-2a7b6e5d4c3b', 'line\nline')`: "4f1cfd11-8b8e-4d3a-9c1f-2a7b6e5d4c3b",
	}
	for reply, want := range cases {
		m := uuidPattern.FindStringSubmatch(reply)
		if m == nil {
			t.Errorf("no UUID found in %s", reply)
			continue
		}
		if m[1] != want {
			t.Errorf("parsed %q from %s, want %q", m[1], reply, want)
		}
	}

	// Anything that is not a well-formed reply must not yield a UUID.
	for _, bad := range []string{"", "()", "error: no such element", `('not a uuid!', 'x')`} {
		if m := uuidPattern.FindStringSubmatch(bad); m != nil {
			t.Errorf("parsed %q out of %q, want no match", m[1], bad)
		}
	}
}

// GPaste must never displace wl-clipboard where the compositor can serve it
// properly, so the candidate carrying the runtime check is the one gated.
func TestGPasteCandidateIsRuntimeChecked(t *testing.T) {
	if runtime.GOOS == "darwin" || runtime.GOOS == "windows" {
		t.Skip("unix session selection only")
	}
	t.Setenv("WAYLAND_DISPLAY", "wayland-0")
	for _, c := range candidates() {
		if c.name == "gpaste" {
			if c.usable == nil {
				t.Fatal("the gpaste candidate has no runtime check, so a bare install would win")
			}
			if c.readCustom == nil {
				t.Fatal("the gpaste candidate has no custom reader")
			}
			return
		}
	}
}
