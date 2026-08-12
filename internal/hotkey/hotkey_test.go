package hotkey

import (
	"strings"
	"testing"
)

func TestHuman(t *testing.T) {
	cases := map[string]string{
		"<Super><Shift>c":   "Super+Shift+C",
		"<Control><Alt>v":   "Ctrl+Alt+V",
		"<Ctrl><Shift>x":    "Ctrl+Shift+X",
		"<Super>space":      "Super+SPACE",
		"<Alt><Super>Print": "Alt+Super+PRINT",
		// GNOME's own Settings UI writes Ctrl as <Primary>.
		"<Primary><Shift>c": "Ctrl+Shift+C",
	}
	for accel, want := range cases {
		if got := Human(accel); got != want {
			t.Errorf("Human(%q) = %q, want %q", accel, got, want)
		}
	}
}

// The custom-keybindings key is a GVariant array literal. henri has to add its
// own path without disturbing shortcuts the user already set.
//
// The fixtures used to be bare paths like '/a/'. They are real dconf paths now,
// because the parser is anchored to /org/gnome/ -- see the warning case at the
// end, which is the reason.
func TestCustomListParsing(t *testing.T) {
	const base = "/org/gnome/settings-daemon/plugins/media-keys/custom-keybindings/"
	cases := []struct {
		out  string
		want []string
	}{
		{"@as []", nil},
		{"[]", nil},
		{"['" + base + "custom0/']", []string{base + "custom0/"}},
		{"['" + base + "custom0/', '" + base + "custom1/', '" + base + "henri/']",
			[]string{base + "custom0/", base + "custom1/", base + "henri/"}},
		{"@as ['" + base + "custom0/']", []string{base + "custom0/"}},
		// dconf greets many sessions with this on stderr, and it is exactly the
		// shape of a quoted path. Reading it as one put /etc/dconf/db/local into
		// the user's real shortcut list.
		{"dconf-WARNING **: unable to open file '/etc/dconf/db/local'\n['" + base + "henri/']",
			[]string{base + "henri/"}},
	}
	for _, c := range cases {
		var got []string
		for _, m := range pathPattern.FindAllStringSubmatch(c.out, -1) {
			if m[1] != "" {
				got = append(got, m[1])
			}
		}
		if len(got) != len(c.want) {
			t.Errorf("parsing %q gave %v, want %v", c.out, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("parsing %q gave %v, want %v", c.out, got, c.want)
				break
			}
		}
	}
}

func TestSetCustomListRendersAValidLiteral(t *testing.T) {
	// Not executed, just checking the literal we would hand gsettings. The
	// paths are real dconf paths now; see TestCustomListParsing.
	const base = "/org/gnome/settings-daemon/plugins/media-keys/custom-keybindings/"
	list := []string{base + "custom0/", base + "henri/"}
	quoted := make([]string, len(list))
	for i, p := range list {
		esc := strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(p)
		quoted[i] = "'" + esc + "'"
	}
	got := "[" + strings.Join(quoted, ", ") + "]"
	if got != "['"+base+"custom0/', '"+base+"henri/']" {
		t.Fatalf("rendered %q", got)
	}
	// And it must round-trip through the parser.
	var back []string
	for _, m := range pathPattern.FindAllStringSubmatch(got, -1) {
		back = append(back, m[1])
	}
	if len(back) != 2 || back[0] != list[0] || back[1] != list[1] {
		t.Fatalf("round trip gave %v", back)
	}
}

// gsettings prints a GVariant literal, not a bare string: one quote at each end
// and its own quotes and backslashes escaped inside.
func TestUnquote(t *testing.T) {
	cases := map[string]string{
		"'/usr/local/bin/henri send'": "/usr/local/bin/henri send",
		"  '<Super><Shift>c'  ":       "<Super><Shift>c",
		`'henri send \'this\''`:       `henri send 'this'`,
		`'C:\\henri'`:                 `C:\henri`,
		"''":                          "",
		"'''":                         "'", // trims one quote per end, not all
		"/no/quotes/at/all":           "/no/quotes/at/all",
	}
	for in, want := range cases {
		if got := unquote(in); got != want {
			t.Errorf("unquote(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestInstructionsMentionTheCommandAndKey(t *testing.T) {
	out := Instructions("/usr/local/bin/henri send", DefaultAccel)
	for _, want := range []string{"/usr/local/bin/henri send", "Super+Shift+C", "sway", "hyprland", "KDE", "GNOME"} {
		if !strings.Contains(out, want) {
			t.Errorf("instructions do not mention %q", want)
		}
	}
}
