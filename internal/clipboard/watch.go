package clipboard

import (
	"bufio"
	"context"
	"os/exec"
	"time"
)

// startupGrace is how long an event source gets to prove it survives. The
// mechanisms below fail by exiting immediately when the compositor does not
// support them, so anything still alive after this is working.
const startupGrace = 400 * time.Millisecond

// Events returns a channel that fires whenever the clipboard may have changed,
// along with the name of the mechanism providing it. It returns nil when this
// system has no such mechanism and the caller should fall back to polling.
//
// Polling means spawning a helper process several times a second for as long as
// henri runs. That is wasteful, and on some desktops it is worse than wasteful:
// Wayland ties clipboard access to keyboard focus, so unless the compositor
// implements a data-control protocol, every read briefly takes focus and the
// screen visibly flickers. Where the system will tell us about changes instead,
// we let it, and touch the clipboard only when something actually happened.
func Events(ctx context.Context) (<-chan struct{}, string) {
	t, err := resolve()
	if err != nil {
		return nil, ""
	}
	switch t.name {
	case "wl-clipboard":
		// --watch needs wlr-data-control (or ext-data-control). Compositors
		// that have it also let wl-paste read without focus, so this both
		// removes the polling and removes the flicker.
		if ch := pipeWatcher(ctx, "wl-paste", "--watch", "echo"); ch != nil {
			return ch, "wl-paste --watch"
		}
	case "xclip", "xsel":
		// clipnotify blocks until the selection changes and then exits. It
		// ships separately and is usually absent, so this is opportunistic.
		if _, err := exec.LookPath("clipnotify"); err == nil {
			if ch := loopWatcher(ctx, "clipnotify"); ch != nil {
				return ch, "clipnotify"
			}
		}
	}
	return nil, ""
}

// pipeWatcher runs a long-lived command that writes a line per clipboard
// change, and turns those lines into channel sends.
func pipeWatcher(ctx context.Context, name string, args ...string) chan struct{} {
	cmd := exec.CommandContext(ctx, name, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil
	}
	if err := cmd.Start(); err != nil {
		return nil
	}

	exited := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(exited)
	}()

	// If the mechanism is unsupported the process dies right away; better to
	// find that out here than to hand back a channel that never fires.
	select {
	case <-exited:
		return nil
	case <-time.After(startupGrace):
	}

	ch := make(chan struct{}, 1)
	go func() {
		defer close(ch)
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			select {
			case ch <- struct{}{}:
			default: // a signal is already pending; one is as good as two
			}
		}
		<-exited
	}()
	return ch
}

// loopWatcher repeatedly runs a command that blocks until the clipboard
// changes, signalling once per completed run.
func loopWatcher(ctx context.Context, name string, args ...string) chan struct{} {
	// Prove it works before promising anything: a first run that fails
	// instantly means the tool cannot reach the display.
	probe := exec.CommandContext(ctx, name, args...)
	if err := probe.Start(); err != nil {
		return nil
	}
	probeDone := make(chan error, 1)
	go func() { probeDone <- probe.Wait() }()
	select {
	case err := <-probeDone:
		if err != nil {
			return nil
		}
	case <-time.After(startupGrace):
		// Still blocking, which is exactly what it should do.
	}

	ch := make(chan struct{}, 1)
	go func() {
		defer close(ch)
		// Drain the probe, which counts as the first observation.
		select {
		case <-probeDone:
			signal(ch)
		case <-ctx.Done():
			_ = probe.Process.Kill()
			return
		}
		for ctx.Err() == nil {
			cmd := exec.CommandContext(ctx, name, args...)
			if err := cmd.Run(); err != nil {
				if ctx.Err() != nil {
					return
				}
				// A failing watcher would otherwise spin; let the caller poll.
				return
			}
			signal(ch)
		}
	}()
	return ch
}

func signal(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}
