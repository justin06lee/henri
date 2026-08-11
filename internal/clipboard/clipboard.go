// Package clipboard reads and writes the system clipboard by shelling out to
// the platform's own tool.
//
// This keeps henri a single static binary with no cgo and no third-party
// dependencies, at the cost of one short-lived subprocess per poll. The tools
// used are pbcopy/pbpaste on macOS, wl-copy/wl-paste or xclip or xsel on Linux
// and other X11 systems, and PowerShell's Get-Clipboard/Set-Clipboard on
// Windows.
//
// Everything here deals in UTF-8 bytes with "\n" line endings, which is what
// macOS and Linux both produce natively; the Windows helpers are normalised to
// match so a copy on one platform pastes unchanged on another.
package clipboard

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"
)

// ErrUnsupported means no usable clipboard tool was found on this machine.
var ErrUnsupported = errors.New("clipboard: no supported clipboard tool found")

// timeout bounds a single clipboard call so a wedged helper cannot stall the
// watch loop forever.
const timeout = 5 * time.Second

// tool describes how to drive one clipboard helper.
type tool struct {
	// name is what shows up in `henri status`.
	name string
	// readBin/readArgs produce the current clipboard on stdout.
	readBin  string
	readArgs []string
	// writeBin/writeArgs consume new contents on stdin.
	writeBin  string
	writeArgs []string
	// primaryBin/primaryArgs read the PRIMARY selection -- the text you have
	// highlighted, which X11 and Wayland both track separately from the
	// clipboard and update without anyone pressing a key. Empty where the
	// platform has no such concept.
	primaryBin  string
	primaryArgs []string
	// needBins must all exist on PATH for this tool to be usable.
	needBins []string

	// forks is set for helpers that hand the selection to a background process
	// and return. On X11 and Wayland the clipboard is owned by a live process
	// rather than the display server, so xclip, xsel and wl-copy all daemonise
	// themselves. See writeWith for why that matters.
	forks bool

	// trimCRLF strips the trailing CRLF that the Windows helper appends.
	trimCRLF bool
}

var (
	once     sync.Once
	resolved *tool
	resErr   error
)

// candidates lists the tools to try, best first, for the current platform.
func candidates() []tool {
	switch runtime.GOOS {
	case "darwin":
		return []tool{{
			name:     "pbpaste",
			readBin:  "pbpaste",
			writeBin: "pbcopy",
			needBins: []string{"pbpaste", "pbcopy"},
		}}

	case "windows":
		ps := []string{"-NoProfile", "-NonInteractive", "-Command"}
		return []tool{{
			name:      "powershell",
			readBin:   "powershell",
			readArgs:  append(append([]string{}, ps...), "Get-Clipboard -Raw"),
			writeBin:  "powershell",
			writeArgs: append(append([]string{}, ps...), "Set-Clipboard -Value ([Console]::In.ReadToEnd())"),
			needBins:  []string{"powershell"},
			trimCRLF:  true,
		}}

	default: // linux, freebsd, and the other X11/Wayland systems
		wayland := tool{
			name: "wl-clipboard",
			// -t text asks for a plain-text flavour specifically: without it a
			// copy from a browser can come back as text/html, or as an image,
			// which is not what the other devices want to paste.
			readBin:     "wl-paste",
			readArgs:    []string{"--no-newline", "--type", "text"},
			primaryBin:  "wl-paste",
			primaryArgs: []string{"--primary", "--no-newline", "--type", "text"},
			writeBin:    "wl-copy",
			writeArgs:   []string{"--type", "text/plain;charset=utf-8", "--"},
			needBins:    []string{"wl-paste", "wl-copy"},
			forks:       true,
		}
		x11 := []tool{
			{
				name:        "xclip",
				readBin:     "xclip",
				readArgs:    []string{"-selection", "clipboard", "-o"},
				primaryBin:  "xclip",
				primaryArgs: []string{"-selection", "primary", "-o"},
				writeBin:    "xclip",
				writeArgs:   []string{"-selection", "clipboard", "-i"},
				needBins:    []string{"xclip"},
				forks:       true,
			},
			{
				name:        "xsel",
				readBin:     "xsel",
				readArgs:    []string{"--clipboard", "--output"},
				primaryBin:  "xsel",
				primaryArgs: []string{"--primary", "--output"},
				writeBin:    "xsel",
				writeArgs:   []string{"--clipboard", "--input"},
				needBins:    []string{"xsel"},
				forks:       true,
			},
		}

		// Prefer whichever matches the session actually in use: on a Wayland
		// session xclip may exist but talk to an XWayland clipboard the
		// compositor does not share, and on X11 wl-paste simply fails.
		if os.Getenv("WAYLAND_DISPLAY") != "" {
			return append([]tool{wayland}, x11...)
		}
		return append(x11, wayland)
	}
}

// resolve picks a clipboard tool once and caches the result.
func resolve() (*tool, error) {
	once.Do(func() {
		var tried []string
		for _, c := range candidates() {
			tried = append(tried, c.name)
			if !hasAll(c.needBins) {
				continue
			}
			t := c
			resolved = &t
			return
		}
		resErr = fmt.Errorf("%w (tried: %s)", ErrUnsupported, strings.Join(tried, ", "))
	})
	return resolved, resErr
}

func hasAll(bins []string) bool {
	for _, b := range bins {
		if _, err := exec.LookPath(b); err != nil {
			return false
		}
	}
	return true
}

// Available reports whether a clipboard tool could be found.
func Available() error {
	_, err := resolve()
	return err
}

// Tool returns the name of the helper in use, for `henri status`.
func Tool() string {
	t, err := resolve()
	if err != nil {
		return "none"
	}
	return t.name
}

// Read returns the current clipboard contents as UTF-8 text.
func Read() ([]byte, error) {
	t, err := resolve()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, t.readBin, t.readArgs...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		// An empty clipboard, or one holding something that is not text, is not
		// an error worth surfacing: there is simply nothing to sync. Several
		// helpers report both by exiting non-zero.
		if stdout.Len() == 0 && looksEmpty(stderr.String()) {
			return nil, nil
		}
		return nil, fmt.Errorf("clipboard: %s: %w: %s", t.name, err, strings.TrimSpace(stderr.String()))
	}

	out := stdout.Bytes()
	if t.trimCRLF {
		out = bytes.TrimSuffix(out, []byte("\r\n"))
		// Normalise to LF so text copied on Windows pastes cleanly on macOS
		// and Linux.
		out = bytes.ReplaceAll(out, []byte("\r\n"), []byte("\n"))
	}
	return out, nil
}

// ErrNoPrimary means this platform has no PRIMARY selection. macOS and Windows
// have only one clipboard; highlighting text there does not publish it
// anywhere.
var ErrNoPrimary = errors.New("clipboard: this system has no separate selection for highlighted text")

// ReadPrimary returns the PRIMARY selection: whatever is highlighted right now.
//
// X11 and Wayland both maintain this alongside the clipboard, and every app
// updates it as you drag over text -- no copy command involved. It is what
// middle-click pastes.
func ReadPrimary() ([]byte, error) {
	t, err := resolve()
	if err != nil {
		return nil, err
	}
	if t.primaryBin == "" {
		return nil, ErrNoPrimary
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, t.primaryBin, t.primaryArgs...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if stdout.Len() == 0 && looksEmpty(stderr.String()) {
			return nil, nil // nothing highlighted
		}
		return nil, fmt.Errorf("clipboard: %s: %w: %s", t.name, err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

// HasPrimary reports whether this system tracks highlighted text separately.
func HasPrimary() bool {
	t, err := resolve()
	return err == nil && t.primaryBin != ""
}

// Write replaces the clipboard contents.
func Write(data []byte) error {
	t, err := resolve()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, t.writeBin, t.writeArgs...)
	if err := writeWith(cmd, data, t.forks); err != nil {
		return fmt.Errorf("clipboard: write via %s: %w", t.name, err)
	}
	return nil
}

// writeWith feeds data to cmd's stdin and waits for it.
//
// forks is the subtle part. X11 and Wayland have no clipboard daemon: the
// process that copied something stays alive to serve it, so xclip, xsel and
// wl-copy all fork a background child and let the foreground process exit. That
// child inherits our stdout and stderr. If those are pipes — which is what
// os/exec creates for a bytes.Buffer — then Wait blocks until every writer
// closes them, and the background child holds its end open for as long as it
// owns the selection. The result is that every copy hangs until the timeout
// fires and the helper is killed, which also drops the clipboard.
//
// Leaving Stdout and Stderr nil makes os/exec attach /dev/null instead, so
// nothing is inherited and Wait returns as soon as the foreground process does.
// The cost is that these helpers cannot report why they failed beyond an exit
// status, which is a fair trade for copy working at all.
func writeWith(cmd *exec.Cmd, data []byte, forks bool) error {
	cmd.Stdin = bytes.NewReader(data)

	if forks {
		cmd.Stdout = nil
		cmd.Stderr = nil
		return cmd.Run()
	}

	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return fmt.Errorf("%w: %s", err, msg)
		}
		return err
	}
	return nil
}

// looksEmpty recognises the various ways a helper says "there is nothing here",
// as opposed to a real failure.
func looksEmpty(stderr string) bool {
	s := strings.ToLower(strings.TrimSpace(stderr))
	if s == "" {
		return true
	}
	for _, phrase := range []string{
		"no selection",       // xclip, xsel
		"nothing is copied",  // wl-paste
		"no suitable type",   // wl-paste, clipboard holds a non-text type
		"empty",              // assorted
		"selection is empty", //
	} {
		if strings.Contains(s, phrase) {
			return true
		}
	}
	return false
}
