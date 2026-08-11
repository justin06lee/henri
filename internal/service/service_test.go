package service

import (
	"strings"
	"testing"
)

func TestVolatileLocation(t *testing.T) {
	volatile := []string{
		"/Volumes/T7/Stockpile/.dotfiles/.config/henri/config.json",
		"/media/usb/henri",
		"/mnt/backup/henri",
		"/run/media/you/stick/henri",
		"/tmp/henri",
		"/private/tmp/henri",
		"/var/tmp/henri",
	}
	for _, p := range volatile {
		if VolatileLocation(p) == "" {
			t.Errorf("VolatileLocation(%q) = \"\", want a reason", p)
		}
	}
	stable := []string{
		"/usr/local/bin/henri",
		"/opt/homebrew/bin/henri",
		"/home/you/.local/bin/henri",
		"/Users/you/go/bin/henri",
		"/usr/bin/henri",
		"/Users/you/.henri/config.json",
	}
	for _, p := range stable {
		if why := VolatileLocation(p); why != "" {
			t.Errorf("VolatileLocation(%q) = %q, want \"\"", p, why)
		}
	}
}

func TestLaunchdPlist(t *testing.T) {
	t.Setenv("HOME", "/Users/tester")
	t.Setenv("HENRI_CONFIG", "")
	t.Setenv("XDG_CONFIG_HOME", "")

	m, err := newLaunchd()
	if err != nil {
		t.Fatal(err)
	}
	l := m.(*launchd)
	out := l.render("/usr/local/bin/henri")

	for _, want := range []string{
		"<string>com.justin06lee.henri</string>",
		"<string>/usr/local/bin/henri</string>",
		"<string>daemon</string>",
		"<key>RunAtLoad</key>",
		"<key>KeepAlive</key>",
		"/Users/tester/Library/Logs/henri.log",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("plist is missing %q", want)
		}
	}
	// A LaunchAgent, not a LaunchDaemon: it must stay in the user's session.
	if !strings.Contains(out, "<string>Interactive</string>") {
		t.Error("plist does not mark the job as Interactive")
	}
	if strings.Contains(out, "EnvironmentVariables") {
		t.Error("plist declares an environment when none was set")
	}
}

// A service manager starts with a bare environment, so a custom config
// location has to be recorded or the daemon reads a different file than the
// shell that installed it.
func TestLaunchdPlistCarriesConfigLocation(t *testing.T) {
	t.Setenv("HOME", "/Users/tester")
	t.Setenv("HENRI_CONFIG", "/Users/tester/.henri/config.json")

	m, err := newLaunchd()
	if err != nil {
		t.Fatal(err)
	}
	out := m.(*launchd).render("/usr/local/bin/henri")

	if !strings.Contains(out, "<key>EnvironmentVariables</key>") {
		t.Fatal("plist has no EnvironmentVariables block")
	}
	if !strings.Contains(out, "<key>HENRI_CONFIG</key>") ||
		!strings.Contains(out, "<string>/Users/tester/.henri/config.json</string>") {
		t.Errorf("plist does not carry HENRI_CONFIG:\n%s", out)
	}
}

func TestLaunchdPlistEscapesXML(t *testing.T) {
	t.Setenv("HOME", "/Users/tester")
	t.Setenv("HENRI_CONFIG", "/tmp/a&b/config.json")
	m, err := newLaunchd()
	if err != nil {
		t.Fatal(err)
	}
	out := m.(*launchd).render("/usr/local/bin/hen<ri")
	if strings.Contains(out, "hen<ri") {
		t.Error("binary path was not XML-escaped")
	}
	if !strings.Contains(out, "a&amp;b") {
		t.Error("config path was not XML-escaped")
	}
}

func TestSystemdUnit(t *testing.T) {
	t.Setenv("HOME", "/home/tester")
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("WAYLAND_DISPLAY", "wayland-1")
	t.Setenv("DISPLAY", ":0")
	t.Setenv("XDG_RUNTIME_DIR", "/run/user/1000")
	t.Setenv("HENRI_CONFIG", "/home/tester/.henri/config.json")

	m, err := newSystemd()
	if err != nil {
		t.Fatal(err)
	}
	out := m.(*systemd).render("/usr/local/bin/henri")

	for _, want := range []string{
		"ExecStart=/usr/local/bin/henri daemon",
		"Restart=on-failure",
		"Environment=DISPLAY=:0",
		"Environment=WAYLAND_DISPLAY=wayland-1",
		"Environment=XDG_RUNTIME_DIR=/run/user/1000",
		"Environment=HENRI_CONFIG=/home/tester/.henri/config.json",
		"WantedBy=default.target",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("unit is missing %q:\n%s", want, out)
		}
	}
}

// The captured environment is a map; rendering has to be stable or reinstalling
// churns the file for no reason.
func TestSystemdUnitIsDeterministic(t *testing.T) {
	t.Setenv("HOME", "/home/tester")
	t.Setenv("DISPLAY", ":0")
	t.Setenv("WAYLAND_DISPLAY", "wayland-1")
	t.Setenv("XDG_RUNTIME_DIR", "/run/user/1000")
	t.Setenv("HENRI_CONFIG", "/home/tester/.henri/config.json")

	m, err := newSystemd()
	if err != nil {
		t.Fatal(err)
	}
	first := m.(*systemd).render("/usr/local/bin/henri")
	for i := 0; i < 20; i++ {
		if got := m.(*systemd).render("/usr/local/bin/henri"); got != first {
			t.Fatal("rendering the unit twice produced different output")
		}
	}
}

func TestSystemdUnitPathHonoursXDG(t *testing.T) {
	t.Setenv("HOME", "/home/tester")
	t.Setenv("XDG_CONFIG_HOME", "/home/tester/cfg")
	m, err := newSystemd()
	if err != nil {
		t.Fatal(err)
	}
	if got := m.UnitPath(); got != "/home/tester/cfg/systemd/user/henri.service" {
		t.Fatalf("unit path is %q", got)
	}
}

func TestSessionEnvSkipsUnset(t *testing.T) {
	t.Setenv("WAYLAND_DISPLAY", "wayland-0")
	t.Setenv("DISPLAY", "")
	t.Setenv("XDG_RUNTIME_DIR", "")
	t.Setenv("XAUTHORITY", "")
	env := sessionEnv()
	if _, ok := env["DISPLAY"]; ok {
		t.Error("an empty DISPLAY was captured")
	}
	if env["WAYLAND_DISPLAY"] != "wayland-0" {
		t.Errorf("WAYLAND_DISPLAY = %q", env["WAYLAND_DISPLAY"])
	}
}
