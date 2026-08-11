package clipboard

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"
)

// GPaste is a clipboard manager for GNOME. It matters here because GNOME's
// compositor implements neither wlr-data-control nor ext-data-control, on the
// grounds that letting any background process read the clipboard is exactly
// the thing Wayland set out to prevent. Without one of those protocols there is
// no way to read a selection without taking keyboard focus, which is why
// polling wl-paste makes the screen flicker.
//
// GPaste sidesteps that legitimately: it runs as a daemon paired with a GNOME
// Shell extension, so the privileged half of the pair can watch the clipboard,
// and it publishes what it sees on D-Bus. henri reads from there instead of
// touching the compositor at all.
const (
	gpasteName   = "org.gnome.GPaste"
	gpasteObject = "/org/gnome/GPaste"
)

// gpasteInterfaces are tried in order; the D-Bus interface is versioned
// separately from the package, and both spellings are in the wild.
var gpasteInterfaces = []string{"org.gnome.GPaste2", "org.gnome.GPaste1"}

var (
	gpasteOnce  sync.Once
	gpasteIface string // the interface that answered, "" if GPaste is unusable
)

// uuidPattern matches the first field of GetElementAtIndex's reply. UUIDs are
// hex and dashes, so this needs none of the care that parsing the value would.
var uuidPattern = regexp.MustCompile(`^\('([0-9a-fA-F-]+)'`)

// gpasteAvailable reports whether a GPaste daemon is answering, and caches the
// interface version it speaks. The daemon is D-Bus activated, so this starts it
// if it is installed but not yet running.
func gpasteAvailable() bool {
	gpasteOnce.Do(func() {
		if !hasAll([]string{"gpaste-client", "gdbus"}) {
			return
		}
		for _, iface := range gpasteInterfaces {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			err := exec.CommandContext(ctx, "gdbus", "call", "--session",
				"--dest", gpasteName, "--object-path", gpasteObject,
				"--method", iface+".GetHistorySize", "").Run()
			cancel()
			if err == nil {
				gpasteIface = iface
				return
			}
		}
	})
	return gpasteIface != ""
}

// gpasteRead returns the newest history entry, which is the current clipboard.
//
// It takes two calls: D-Bus for the UUID, then gpaste-client for the value.
// Going through the client for the content avoids parsing a GVariant literal,
// where a clipboard full of quotes and newlines would be a liability; the UUID
// is a safe token to pull out of the reply by hand.
func gpasteRead() ([]byte, error) {
	if !gpasteAvailable() {
		return nil, fmt.Errorf("clipboard: no GPaste daemon is answering on D-Bus")
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	out, err := exec.CommandContext(ctx, "gdbus", "call", "--session",
		"--dest", gpasteName, "--object-path", gpasteObject,
		"--method", gpasteIface+".GetElementAtIndex", "0").Output()
	if err != nil {
		// An empty history is not a failure, just an empty clipboard.
		return nil, nil
	}
	m := uuidPattern.FindSubmatch(bytes.TrimSpace(out))
	if m == nil {
		return nil, nil
	}

	value, err := exec.CommandContext(ctx, "gpaste-client", "get", string(m[1])).Output()
	if err != nil {
		return nil, fmt.Errorf("clipboard: gpaste-client get: %w", err)
	}
	return value, nil
}

// gpasteEvents turns GPaste's D-Bus Update signal into change notifications.
func gpasteEvents(ctx context.Context) chan struct{} {
	return pipeWatcherFunc(ctx, func(line string) bool {
		return strings.Contains(line, ".Update")
	}, "gdbus", "monitor", "--session", "--dest", gpasteName)
}
