// Package hotkey binds a key to a command in the desktop's own shortcut system.
//
// It exists for compositors that will not let henri watch the clipboard in the
// background. Wayland hands the selection only to the client holding keyboard
// focus, and where the compositor implements no data-control protocol there is
// no way around that: reading takes focus, and doing it on a timer makes the
// screen flicker. Pressing a key instead costs one read, when you ask for it.
package hotkey

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
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
var pathPattern = regexp.MustCompile(`'([^']*)'`)

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
	if !gnomeAvailable() {
		return ErrManual
	}
	list, err := customList()
	if err != nil {
		return err
	}
	kept := make([]string, 0, len(list))
	for _, p := range list {
		if p != gnomePath {
			kept = append(kept, p)
		}
	}
	if len(kept) == len(list) {
		return nil
	}
	if err := setCustomList(kept); err != nil {
		return err
	}
	// Reset the keys too, so a reinstall does not inherit stale values.
	for _, k := range []string{"name", "command", "binding"} {
		_, _ = run("gsettings", "reset", gnomeSub, k)
	}
	return nil
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
	r := strings.NewReplacer("<Super>", "Super+", "<Shift>", "Shift+",
		"<Control>", "Ctrl+", "<Ctrl>", "Ctrl+", "<Alt>", "Alt+")
	out := r.Replace(accel)
	if i := strings.LastIndex(out, "+"); i >= 0 && i+1 < len(out) {
		out = out[:i+1] + strings.ToUpper(out[i+1:])
	}
	return out
}

func gnomeAvailable() bool {
	if _, err := exec.LookPath("gsettings"); err != nil {
		return false
	}
	// The schema is only present where GNOME's settings daemon is installed.
	out, err := run("gsettings", "list-schemas")
	if err != nil {
		return false
	}
	return strings.Contains(out, gnomeSchema)
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
		quoted[i] = "'" + p + "'"
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
	return strings.Trim(strings.TrimSpace(out), "'"), nil
}

func contains(list []string, want string) bool {
	for _, p := range list {
		if p == want {
			return true
		}
	}
	return false
}

func run(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).CombinedOutput()
	text := strings.TrimSpace(string(out))
	if err != nil {
		return text, fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, text)
	}
	return text, nil
}
