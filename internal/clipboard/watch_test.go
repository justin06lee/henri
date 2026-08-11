package clipboard

import (
	"context"
	"runtime"
	"testing"
	"time"
)

// loopWatcher's probe finishes inside the grace period whenever the selection
// changed while henri was starting, which is common at login. The result was
// consumed twice -- once by the outer select and once by the goroutine -- so
// the goroutine blocked on a channel nobody would ever send to again. The
// caller got back a channel that never fired and never closed, which reads as
// "clipnotify is working" while the clipboard is in fact only polled every 30
// seconds, for as long as henri runs.
func TestLoopWatcherSurvivesAProbeThatReturnsEarly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs a POSIX shell")
	}
	dir := t.TempDir()
	// Back well inside startupGrace, the way clipnotify returns when something
	// was copied a moment ago.
	watcher := writeScript(t, dir, "fake-clipnotify", "sleep 0.05\nexit 0\n")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch := loopWatcher(ctx, watcher)
	if ch == nil {
		t.Fatal("loopWatcher gave up on a watcher that works")
	}
	select {
	case <-ch:
	case <-time.After(10 * time.Second):
		t.Fatal("the watcher never signalled; its goroutine is stuck waiting for a probe result that was already taken")
	}

	// And it has to stop when asked, or the caller never learns to fall back.
	cancel()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				return
			}
		case <-deadline:
			t.Fatal("the watcher's channel was never closed after the context was cancelled")
		}
	}
}

// The probe cannot tell a compositor without data-control from no compositor at
// all, so without a Wayland session there is nothing to conclude -- and
// concluding it costs the caller clipboard watching for the whole run.
func TestPollingDoesNotStealFocusOutsideWayland(t *testing.T) {
	t.Setenv("WAYLAND_DISPLAY", "")
	if PollingStealsFocus() {
		t.Fatal("PollingStealsFocus is true with no Wayland session; watching would be abandoned on an X11 machine that merely has wl-clipboard installed")
	}
}
