package tray

import (
	"bytes"
	"context"
	"os"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

func TestSupportedMatchesThePlatform(t *testing.T) {
	if Supported() != (runtime.GOOS == "darwin") {
		t.Fatalf("Supported() = %v on %s", Supported(), runtime.GOOS)
	}
}

// The child reads its world from argv, so the order here is a contract with
// tray.js: daemon pid, icon path, device name, binary path.
func TestCommandPassesTheContractInOrder(t *testing.T) {
	cmd := command(context.Background(), "/cache/tray.js", "/cache/icon.svg",
		Info{DeviceName: "laptop", Binary: "/usr/local/bin/henri"})
	want := []string{"osascript", "-l", "JavaScript", "/cache/tray.js",
		strconv.Itoa(os.Getpid()), "/cache/icon.svg", "laptop", "/usr/local/bin/henri"}
	if len(cmd.Args) != len(want) {
		t.Fatalf("argv is %v, want %v", cmd.Args, want)
	}
	for i := range want {
		if cmd.Args[i] != want[i] {
			t.Fatalf("argv[%d] = %q, want %q (full argv %v)", i, cmd.Args[i], want[i], cmd.Args)
		}
	}
}

// A template image is drawn from its alpha channel, so an opaque background
// renders as a solid square, and hairline art renders as invisible mist. The
// embedded icon is the README panel with the background dropped and the line
// work dilated by a heavy stroke; this keeps both properties in place.
func TestIconIsThickenedTransparentArt(t *testing.T) {
	if !bytes.Contains(icon, []byte("<svg")) {
		t.Fatal("the embedded icon is not an SVG")
	}
	if bytes.Contains(icon, []byte(`<rect width="512" height="512" fill="#ffffff"/>`)) {
		t.Fatal("the embedded icon has an opaque background and would render as a solid square")
	}
	if !bytes.Contains(icon, []byte(`stroke-width="200"`)) {
		t.Fatal("the art lost its thickening stroke; hairline art is invisible at menu bar size")
	}
}

// The script's death watch is what keeps a stale icon out of the menu bar when
// the daemon dies uncleanly; make sure nobody trims it as dead code.
func TestScriptWatchesForTheDaemonsDeath(t *testing.T) {
	s := string(script)
	for _, needle := range []string{"getppid", "run(argv)", "NSStatusBar"} {
		if !strings.Contains(s, needle) {
			t.Fatalf("tray.js no longer mentions %q", needle)
		}
	}
}
