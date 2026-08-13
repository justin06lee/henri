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

// waitDelay is how long Wait may keep waiting on the helper's output after the
// deadline has already killed it.
//
// Killing the process is not enough on its own. Any child it left behind
// inherits our end of the stdout pipe, and Wait stays blocked until every
// writer closes it -- so a helper that forks and hangs made the read outlast
// the timeout by as long as the child cared to live. This is the same trap
// writeWith avoids by detaching entirely; here we still want the output, so we
// take it up to the point where waiting stops being worth it.
const waitDelay = time.Second

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

	// env is applied on top of the inherited environment for every call.
	//
	// This exists because pbcopy and pbpaste choose their text encoding from the
	// locale variables, and fall back to plain C when there are none -- which
	// silently mangles every accented character, curly quote, em-dash, CJK
	// character and emoji in both directions. A daemon started by launchd
	// inherits no LANG, LC_ALL or LC_CTYPE at all, so the environment we are
	// handed cannot be trusted; we state what we need instead.
	env []string

	// forks is set for helpers that hand the selection to a background process
	// and return. On X11 and Wayland the clipboard is owned by a live process
	// rather than the display server, so xclip, xsel and wl-copy all daemonise
	// themselves. See writeWith for why that matters.
	forks bool

	// trimCRLF strips the trailing CRLF that the Windows helper appends.
	trimCRLF bool
}

var (
	resolveMu sync.Mutex
	resolved  *tool
	// resolvedFor records the WAYLAND_DISPLAY the cached pick was made under, so
	// a session appearing later is noticed rather than ignored.
	resolvedFor string
)

// candidates lists the tools to try, best first, for the current platform.
func candidates() []tool {
	switch runtime.GOOS {
	case "darwin":
		return []tool{{
			name:    "pbpaste",
			readBin: "pbpaste",
			// -Prefer txt keeps pbpaste on plain text. Left to itself it falls
			// back to Encapsulated PostScript and then Rich Text when the
			// pasteboard holds no plain flavour, so a copy out of an RTF-only
			// app would sync `{\rtf1\ansi...}` to every peer.
			readArgs: []string{"-Prefer", "txt"},
			writeBin: "pbcopy",
			needBins: []string{"pbpaste", "pbcopy"},
			env:      []string{"LC_ALL=en_US.UTF-8"},
		}}

	case "windows":
		ps := []string{"-NoProfile", "-NonInteractive", "-Command"}
		// Both halves state their encoding first. Get-Clipboard writes through
		// [Console]::OutputEncoding, and [Console]::In.ReadToEnd() decodes our
		// stdin with [Console]::InputEncoding; in powershell 5.1 both default to
		// the OEM code page, which corrupts anything outside ASCII either way.
		//
		// The pipeline form rather than -Value: Set-Clipboard -Value '' throws
		// "Cannot bind argument ... empty string" while -Command still exits 0,
		// so clearing the clipboard would silently do nothing.
		const readScript = `[Console]::OutputEncoding=[Text.Encoding]::UTF8; Get-Clipboard -Raw`
		const writeScript = `[Console]::InputEncoding=[Text.Encoding]::UTF8; [Console]::In.ReadToEnd() | Set-Clipboard`
		var shells []tool
		// pwsh first: PowerShell 7 is the one people install deliberately, and
		// it defaults to UTF-8 rather than the code page.
		for _, bin := range []string{"pwsh", "powershell"} {
			shells = append(shells, tool{
				name:      bin,
				readBin:   bin,
				readArgs:  append(append([]string{}, ps...), readScript),
				writeBin:  bin,
				writeArgs: append(append([]string{}, ps...), writeScript),
				needBins:  []string{bin},
				trimCRLF:  true,
			})
		}
		return shells

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
		// -t UTF8_STRING and --utf8 for the same reason the Wayland candidate
		// names a charset: X11's default target is XA_STRING, which is
		// ISO-8859-1. Without asking, a read of anything with CJK, emoji or
		// curly quotes comes back lossily converted, and a write is only
		// published as STRING, so apps that ask for UTF8_STRING get nothing.
		x11 := []tool{
			{
				name:        "xclip",
				readBin:     "xclip",
				readArgs:    []string{"-selection", "clipboard", "-t", "UTF8_STRING", "-o"},
				primaryBin:  "xclip",
				primaryArgs: []string{"-selection", "primary", "-t", "UTF8_STRING", "-o"},
				writeBin:    "xclip",
				writeArgs:   []string{"-selection", "clipboard", "-t", "UTF8_STRING", "-i"},
				needBins:    []string{"xclip"},
				forks:       true,
			},
			{
				name:        "xsel",
				readBin:     "xsel",
				readArgs:    []string{"--clipboard", "--utf8", "--output"},
				primaryBin:  "xsel",
				primaryArgs: []string{"--primary", "--utf8", "--output"},
				writeBin:    "xsel",
				writeArgs:   []string{"--clipboard", "--utf8", "--input"},
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

// resolve picks a clipboard tool, caching success but never failure.
//
// Caching the failure was wrong under a service manager. launchd and systemd
// start henri before the graphical session exists, so the first attempt can run
// with no WAYLAND_DISPLAY set and, on a fresh machine, no helper on PATH at
// all. Remembering either answer forever means henri prefers the wrong helper
// for the rest of the session, or returns ErrUnsupported from every call even
// after the user installs wl-clipboard. So a failure is simply retried, and a
// success is dropped when the session type changes underneath us, since that is
// what decides the preference order.
func resolve() (*tool, error) {
	session := os.Getenv("WAYLAND_DISPLAY")

	resolveMu.Lock()
	defer resolveMu.Unlock()
	if resolved != nil && resolvedFor == session {
		return resolved, nil
	}

	var tried []string
	for _, c := range candidates() {
		tried = append(tried, c.name)
		if !hasAll(c.needBins) {
			continue
		}
		t := c
		resolved, resolvedFor = &t, session
		return resolved, nil
	}
	resolved, resolvedFor = nil, ""
	return nil, fmt.Errorf("%w (tried: %s)", ErrUnsupported, strings.Join(tried, ", "))
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
	cmd.Env = append(os.Environ(), t.env...)
	cmd.WaitDelay = waitDelay
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("clipboard: %s: timed out after %s", t.name, timeout)
		}
		// An empty clipboard, or one holding something that is not text, is not
		// an error worth surfacing: there is simply nothing to sync. Several
		// helpers report both by exiting non-zero.
		//
		// But only where the helper actually said so. A helper that was killed,
		// that is missing from PATH, or that failed without a word all produce
		// exactly this shape -- no output, nothing on stderr -- and reporting
		// that as an empty clipboard is how a permanently broken clipboard came
		// to report itself healthy: the daemon reads nil as success, clears the
		// recorded fault, logs that the clipboard is readable again, and stops
		// syncing without ever warning anyone.
		if stdout.Len() == 0 && strings.TrimSpace(stderr.String()) != "" && looksEmpty(stderr.String()) {
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
	cmd.Env = append(os.Environ(), t.env...)
	cmd.WaitDelay = waitDelay
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("clipboard: %s: timed out after %s", t.name, timeout)
		}
		// Silence is not the same as "nothing is highlighted"; see Read.
		if stdout.Len() == 0 && strings.TrimSpace(stderr.String()) != "" && looksEmpty(stderr.String()) {
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
	if t.trimCRLF {
		// The mirror of the normalisation in Read: text in the group travels
		// with \n, and pasting that into a CRLF-expecting Windows app used to
		// land as a single line.
		data = toCRLF(data)
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, t.writeBin, t.writeArgs...)
	cmd.Env = append(os.Environ(), t.env...)
	if err := writeWith(cmd, data, t.forks); err != nil {
		return fmt.Errorf("clipboard: write via %s: %w", t.name, err)
	}
	return nil
}

// toCRLF renders every line ending as \r\n, leaving endings that already are
// alone rather than doubling them.
func toCRLF(b []byte) []byte {
	b = bytes.ReplaceAll(b, []byte("\r\n"), []byte("\n"))
	return bytes.ReplaceAll(b, []byte("\n"), []byte("\r\n"))
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
		"no selection",                     // xsel, when nobody owns the selection
		"nothing is copied",                // wl-paste, empty clipboard
		"no suitable type",                 // wl-paste, clipboard holds a non-text type
		"target string not available",      // xclip, empty clipboard
		"target utf8_string not available", // xclip, the target we actually ask for
	} {
		if strings.Contains(s, phrase) {
			return true
		}
	}
	return false
}
