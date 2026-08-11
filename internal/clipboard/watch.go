package clipboard

import (
	"bufio"
	"context"
	"os"
	"os/exec"
	"sync"
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
		// It finished inside the grace period, which happens whenever the
		// selection changed while we were starting. Put the result back: the
		// goroutine below waits on this same channel and would otherwise block
		// on it forever, handing the caller a channel that never fires and
		// never closes. The send cannot block, since the buffer of one was just
		// emptied by this receive.
		probeDone <- err
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

var (
	dataControlOnce sync.Once
	dataControlOK   bool
)

// hasDataControl reports whether the compositor implements a clipboard-manager
// protocol (wlr-data-control or ext-data-control).
//
// There is no way to ask wl-clipboard directly, so this asks the only question
// that matters: does `wl-paste --watch` survive being started? It exits at once
// when the protocol is missing.
func hasDataControl() bool {
	dataControlOnce.Do(func() {
		if _, err := exec.LookPath("wl-paste"); err != nil {
			return
		}
		cmd := exec.Command("wl-paste", "--watch", "echo")
		if err := cmd.Start(); err != nil {
			return
		}
		exited := make(chan struct{})
		go func() { _ = cmd.Wait(); close(exited) }()
		select {
		case <-exited:
			dataControlOK = false
		case <-time.After(startupGrace):
			dataControlOK = true
			_ = cmd.Process.Kill()
			<-exited
		}
	})
	return dataControlOK
}

// PollingStealsFocus reports whether every clipboard read costs keyboard focus
// on this system.
//
// True on Wayland compositors that implement no data-control protocol. The core
// protocol delivers the selection only to the client holding keyboard focus, so
// wl-paste has to take focus to read anything. Polling there is not merely
// wasteful, it makes the screen flicker several times a second, which is worse
// than not watching at all.
func PollingStealsFocus() bool {
	// With no Wayland session there is no focus to steal, and the probe below
	// cannot tell "this compositor lacks data-control" from "there is no
	// compositor": wl-paste dies either way. Guessing true costs the caller
	// clipboard watching for the entire life of the process, which is exactly
	// wrong on an X11 machine that merely has wl-clipboard installed, or in a
	// service that started before the compositor did.
	if os.Getenv("WAYLAND_DISPLAY") == "" {
		return false
	}
	t, err := resolve()
	if err != nil {
		return false
	}
	return t.name == "wl-clipboard" && !hasDataControl()
}
