// Package hotkey binds a key to a command in the desktop's own shortcut system.
//
// It exists for compositors that will not let henri watch the clipboard in the
// background. Wayland hands the selection only to the client holding keyboard
// focus, and where the compositor implements no data-control protocol there is
// no way around that: reading takes focus, and doing it on a timer makes the
// screen flicker. Pressing a key instead costs one read, when you ask for it.
package hotkey

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"
)

// DefaultAccel is the shortcut henri binds, in GNOME's accelerator syntax.
const DefaultAccel = "<Super><Shift>c"

// ErrManual means this desktop has no scriptable shortcut system, so the user
// has to add the binding themselves. Instructions carries the how.
var ErrManual = errors.New("hotkey: this desktop's shortcuts have to be set by hand")

// GNOME keeps custom shortcuts in three keys under one dconf path.
const (
	gnomeSchema = "org.gnome.settings-daemon.plugins.media-keys"
	gnomePath   = "/org/gnome/settings-daemon/plugins/media-keys/custom-keybindings/henri/"
	gnomeSub    = gnomeSchema + ".custom-keybinding:" + gnomePath
)

// pathPattern pulls the entries out of a gsettings string-array literal.
// dconf paths contain no quotes, so this needs no escaping care.
//
// It is anchored to /org/gnome/ deliberately. Every custom keybinding lives
// under that prefix, and matching any quoted run at all swallowed dconf's own
// startup warning -- `dconf-WARNING **: unable to open file
// '/etc/dconf/db/local'` is exactly the same shape -- which then got written
// straight back into the user's real custom-keybindings list.
var pathPattern = regexp.MustCompile(`'(/org/gnome/[^']*)'`)

// Status describes the current binding.
type Status struct {
	Desktop   string
	Installed bool
	Accel     string
	Command   string
}

// Available reports whether henri can manage shortcuts on this desktop.
func Available() bool { return gnomeAvailable() }

// Desktop names the shortcut system in use, for messages.
func Desktop() string {
	if gnomeAvailable() {
		return "GNOME"
	}
	if d := os.Getenv("XDG_CURRENT_DESKTOP"); d != "" {
		return d
	}
	return "this desktop"
}

// Install binds accel to command, replacing any previous henri binding.
func Install(command, accel string) error {
	if !gnomeAvailable() {
		return ErrManual
	}
	if err := checkArg("shortcut", accel); err != nil {
		return err
	}
	if err := checkArg("command", command); err != nil {
		return err
	}
	list, err := customList()
	if err != nil {
		return err
	}
	if !contains(list, gnomePath) {
		list = append(list, gnomePath)
		if err := setCustomList(list); err != nil {
			return err
		}
	}
	for _, kv := range [][2]string{
		{"name", "henri send"},
		{"command", command},
		{"binding", accel},
	} {
		if _, err := run("gsettings", "set", gnomeSub, kv[0], kv[1]); err != nil {
			return err
		}
	}
	return nil
}

// Uninstall removes henri's binding, leaving any others alone.
func Uninstall() error {
	_, err := UninstallReport()
	return err
}

// UninstallReport removes henri's binding and says whether there was one.
//
// Uninstall cannot: it returns nil both when it removed a binding and when it
// found none, so the CLI thanks people for removing a shortcut they never had.
func UninstallReport() (removed bool, err error) {
	if !gnomeAvailable() {
		return false, ErrManual
	}
	list, err := customList()
	if err != nil {
		return false, err
	}
	kept := make([]string, 0, len(list))
	for _, p := range list {
		if p != gnomePath {
			kept = append(kept, p)
		}
	}
	if len(kept) == len(list) {
		return false, nil
	}
	if err := setCustomList(kept); err != nil {
		return false, err
	}
	// Reset the keys too, so a reinstall does not inherit stale values.
	for _, k := range []string{"name", "command", "binding"} {
		_, _ = run("gsettings", "reset", gnomeSub, k)
	}
	return true, nil
}

// Get reports the current binding.
func Get() (Status, error) {
	st := Status{Desktop: Desktop()}
	if !gnomeAvailable() {
		return st, ErrManual
	}
	list, err := customList()
	if err != nil {
		return st, err
	}
	if !contains(list, gnomePath) {
		return st, nil
	}
	st.Installed = true
	st.Accel, _ = getString("binding")
	st.Command, _ = getString("command")
	return st, nil
}

// Instructions explains how to add the binding by hand.
func Instructions(command, accel string) string {
	human := Human(accel)
	var b strings.Builder
	fmt.Fprintf(&b, "Bind %s to this command in your compositor's config:\n\n", human)
	fmt.Fprintf(&b, "    %s\n\n", command)
	fmt.Fprintf(&b, "  sway / i3      bindsym $mod+Shift+c exec %s\n", command)
	fmt.Fprintf(&b, "  hyprland       bind = SUPER SHIFT, C, exec, %s\n", command)
	fmt.Fprintf(&b, "  KDE            System Settings > Shortcuts > Add Command\n")
	fmt.Fprintf(&b, "  GNOME          Settings > Keyboard > Custom Shortcuts\n")
	return b.String()
}

// Human renders an accelerator the way a person would write it.
func Human(accel string) string {
	// <Primary> is GNOME's canonical spelling for Ctrl, and it is what its own
	// Settings UI writes into the binding key, so leaving it out meant `henri
	// hotkey status` printing raw markup at anyone who set the shortcut there.
	r := strings.NewReplacer("<Super>", "Super+", "<Shift>", "Shift+",
		"<Primary>", "Ctrl+", "<Control>", "Ctrl+", "<Ctrl>", "Ctrl+", "<Alt>", "Alt+")
	out := r.Replace(accel)
	if i := strings.LastIndex(out, "+"); i >= 0 && i+1 < len(out) {
		out = out[:i+1] + strings.ToUpper(out[i+1:])
	}
	return out
}

var (
	gnomeOnce sync.Once
	gnomeOK   bool
)

// gnomeAvailable reports whether GNOME's settings daemon is here to be asked.
//
// Answered once: the schema cannot appear or vanish while henri runs, and
// listing every schema on the system costs a subprocess and several hundred
// lines of output -- which `henri hotkey status` was paying for twice to
// answer one question.
func gnomeAvailable() bool {
	gnomeOnce.Do(func() {
		if _, err := exec.LookPath("gsettings"); err != nil {
			return
		}
		// The schema is only present where GNOME's settings daemon is installed.
		out, err := run("gsettings", "list-schemas")
		if err != nil {
			return
		}
		// A whole line, not a substring: another schema that merely starts with
		// this name would otherwise answer for it.
		for _, line := range strings.Split(out, "\n") {
			if strings.TrimSpace(line) == gnomeSchema {
				gnomeOK = true
				return
			}
		}
	})
	return gnomeOK
}

func customList() ([]string, error) {
	out, err := run("gsettings", "get", gnomeSchema, "custom-keybindings")
	if err != nil {
		return nil, err
	}
	var list []string
	for _, m := range pathPattern.FindAllStringSubmatch(out, -1) {
		if m[1] != "" {
			list = append(list, m[1])
		}
	}
	return list, nil
}

func setCustomList(list []string) error {
	quoted := make([]string, len(list))
	for i, p := range list {
		// Paths that came back from dconf hold no quotes or backslashes, but
		// this literal is handed to a parser, so escape rather than trust.
		esc := strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(p)
		quoted[i] = "'" + esc + "'"
	}
	value := "[" + strings.Join(quoted, ", ") + "]"
	_, err := run("gsettings", "set", gnomeSchema, "custom-keybindings", value)
	return err
}

func getString(key string) (string, error) {
	out, err := run("gsettings", "get", gnomeSub, key)
	if err != nil {
		return "", err
	}
	return unquote(out), nil
}

// unquote turns the GVariant string literal gsettings prints back into its
// value: one quote off each end, not every quote there is, and the escapes the
// literal introduced undone.
func unquote(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 && strings.HasPrefix(s, "'") && strings.HasSuffix(s, "'") {
		s = s[1 : len(s)-1]
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) && (s[i+1] == '\'' || s[i+1] == '\\') {
			i++
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// checkArg refuses a value that would be read as an option rather than as
// itself. gsettings takes its arguments by position, so this is belt and
// braces -- but a shortcut or command starting with a dash is a mistake in any
// case, and silently binding one is worse than saying so.
func checkArg(what, v string) error {
	if v == "" {
		return fmt.Errorf("hotkey: the %s is empty", what)
	}
	if strings.HasPrefix(v, "-") {
		return fmt.Errorf("hotkey: the %s %q starts with a dash, which would be read as an option", what, v)
	}
	return nil
}

func contains(list []string, want string) bool {
	for _, p := range list {
		if p == want {
			return true
		}
	}
	return false
}

// commandTimeout bounds one gsettings call. gsettings talks to dconf over
// D-Bus, and a wedged bus would otherwise hang `henri hotkey status` forever
// with nothing on screen.
const commandTimeout = 10 * time.Second

// run executes a command and returns its stdout only.
//
// The separation matters. dconf greets many sessions with `dconf-WARNING **:
// unable to open file '/etc/dconf/db/local'` on stderr, and folding that into
// the output meant the keybinding parser read the path out of the warning and
// wrote it into the user's real shortcut list. stderr belongs in the error, and
// nowhere else.
func run(name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, name, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()

	text := strings.TrimSpace(stdout.String())
	if err != nil {
		if ctx.Err() != nil {
			return text, fmt.Errorf("%s %s: timed out after %s", name, strings.Join(args, " "), commandTimeout)
		}
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return text, fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, msg)
		}
		return text, fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return text, nil
}
