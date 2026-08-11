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
	}
	for accel, want := range cases {
		if got := Human(accel); got != want {
			t.Errorf("Human(%q) = %q, want %q", accel, got, want)
		}
	}
}

// The custom-keybindings key is a GVariant array literal. henri has to add its
// own path without disturbing shortcuts the user already set.
func TestCustomListParsing(t *testing.T) {
	cases := []struct {
		out  string
		want []string
	}{
		{"@as []", nil},
		{"[]", nil},
		{"['/org/gnome/.../custom0/']", []string{"/org/gnome/.../custom0/"}},
		{"['/a/', '/b/', '/c/']", []string{"/a/", "/b/", "/c/"}},
		{"@as ['/a/']", []string{"/a/"}},
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
	// Not executed, just checking the literal we would hand gsettings.
	list := []string{"/a/", "/b/"}
	quoted := make([]string, len(list))
	for i, p := range list {
		quoted[i] = "'" + p + "'"
	}
	got := "[" + strings.Join(quoted, ", ") + "]"
	if got != "['/a/', '/b/']" {
		t.Fatalf("rendered %q", got)
	}
	// And it must round-trip through the parser.
	var back []string
	for _, m := range pathPattern.FindAllStringSubmatch(got, -1) {
		back = append(back, m[1])
	}
	if len(back) != 2 || back[0] != "/a/" || back[1] != "/b/" {
		t.Fatalf("round trip gave %v", back)
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
