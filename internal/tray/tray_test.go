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
// renders as a solid square and pale fills render as mist. The embedded icon
// is a few bold black shapes on transparency; this keeps it that way.
func TestIconIsBoldTransparentShapes(t *testing.T) {
	if !bytes.Contains(icon, []byte("<svg")) {
		t.Fatal("the embedded icon is not an SVG")
	}
	if bytes.Contains(icon, []byte(`fill="#ffffff"`)) || bytes.Contains(icon, []byte("<rect")) {
		t.Fatal("the embedded icon has an opaque background and would render as a solid square")
	}
	if !bytes.Contains(icon, []byte(`fill="#000"`)) {
		t.Fatal("the embedded icon has no solid shapes; thin line art is invisible at menu bar size")
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
