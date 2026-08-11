package service

import (
	"errors"
	"os"
	"path/filepath"
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
	out, err := l.render("/usr/local/bin/henri")
	if err != nil {
		t.Fatal(err)
	}

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
	// The environment block is unconditional now: launchd gives a job no locale
	// at all, and pbcopy and pbpaste quietly fall back to the C encoding
	// without one. This test used to assert the opposite.
	if !strings.Contains(out, "<key>LANG</key>") ||
		!strings.Contains(out, "<string>en_US.UTF-8</string>") {
		t.Errorf("plist does not set a locale:\n%s", out)
	}
}

// A plain KeepAlive restarts the daemon after a clean exit too, so a henri that
// gives up at startup respawns forever and cannot be stopped by killing it.
func TestLaunchdPlistRestartsOnlyOnFailure(t *testing.T) {
	t.Setenv("HOME", "/Users/tester")
	t.Setenv("HENRI_CONFIG", "")
	t.Setenv("XDG_CONFIG_HOME", "")

	m, err := newLaunchd()
	if err != nil {
		t.Fatal(err)
	}
	out, err := m.(*launchd).render("/usr/local/bin/henri")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "<key>KeepAlive</key>\n  <true/>") {
		t.Error("KeepAlive is unconditional; a clean exit will be restarted")
	}
	for _, want := range []string{"<key>SuccessfulExit</key>", "<false/>", "<key>ThrottleInterval</key>"} {
		if !strings.Contains(out, want) {
			t.Errorf("plist is missing %q:\n%s", want, out)
		}
	}
}

// A newline in an environment value survives XML escaping and produces a plist
// launchd will not parse, so it has to be refused outright.
func TestLaunchdPlistRejectsControlCharacters(t *testing.T) {
	t.Setenv("HOME", "/Users/tester")
	t.Setenv("HENRI_CONFIG", "/tmp/ok\n<key>Label</key><string>evil</string>")

	m, err := newLaunchd()
	if err != nil {
		t.Fatal(err)
	}
	if out, err := m.(*launchd).render("/usr/local/bin/henri"); err == nil {
		t.Fatalf("a newline in HENRI_CONFIG was accepted:\n%s", out)
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
	out, err := m.(*launchd).render("/usr/local/bin/henri")
	if err != nil {
		t.Fatal(err)
	}

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
	out, err := m.(*launchd).render("/usr/local/bin/hen<ri")
	if err != nil {
		t.Fatal(err)
	}
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
	out, err := m.(*systemd).render("/usr/local/bin/henri")
	if err != nil {
		t.Fatal(err)
	}

	// Everything is quoted now, and the unit hangs off graphical-session rather
	// than default: this test asserted the unquoted forms and default.target
	// before, both of which were the bug.
	for _, want := range []string{
		`ExecStart="/usr/local/bin/henri" daemon`,
		"Restart=on-failure",
		`Environment="DISPLAY=:0"`,
		`Environment="WAYLAND_DISPLAY=wayland-1"`,
		`Environment="XDG_RUNTIME_DIR=/run/user/1000"`,
		`Environment="HENRI_CONFIG=/home/tester/.henri/config.json"`,
		"After=graphical-session.target",
		"PartOf=graphical-session.target",
		"WantedBy=graphical-session.target",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("unit is missing %q:\n%s", want, out)
		}
	}
	// A private /tmp hides X11's socket directory, so the helpers this unit
	// exists to feed could never open the display. Matched as a directive
	// rather than as a substring, since the unit explains itself in a comment.
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "PrivateTmp=") {
			t.Errorf("unit sets PrivateTmp, which breaks X11:\n%s", out)
		}
	}
}

// systemd's parser splits on whitespace, reads "%" as a specifier, and treats a
// newline as the end of the directive -- so an environment value carrying one
// could add directives of its own.
func TestSystemdUnitEscapesEnvironment(t *testing.T) {
	t.Setenv("HOME", "/home/tester")
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("WAYLAND_DISPLAY", "")
	t.Setenv("DISPLAY", "")
	t.Setenv("XDG_RUNTIME_DIR", "")
	t.Setenv("XAUTHORITY", "/home/me/my auth/100% file")
	t.Setenv("HENRI_CONFIG", `/home/me/"quoted"\dir/config.json`)

	m, err := newSystemd()
	if err != nil {
		t.Fatal(err)
	}
	out, err := m.(*systemd).render("/usr/local/bin/hen ri")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`Environment="XAUTHORITY=/home/me/my auth/100%% file"`,
		`Environment="HENRI_CONFIG=/home/me/\"quoted\"\\dir/config.json"`,
		`ExecStart="/usr/local/bin/hen ri" daemon`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("unit is missing %q:\n%s", want, out)
		}
	}

	// A newline cannot be escaped into safety, only refused.
	t.Setenv("HENRI_CONFIG", "/home/me/config.json\nExecStartPre=/bin/rm -rf /")
	if out, err := m.(*systemd).render("/usr/local/bin/henri"); err == nil {
		t.Fatalf("a newline in HENRI_CONFIG was accepted:\n%s", out)
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
	first, err := m.(*systemd).render("/usr/local/bin/henri")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		got, err := m.(*systemd).render("/usr/local/bin/henri")
		if err != nil {
			t.Fatal(err)
		}
		if got != first {
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

// `launchctl load -w` reports failure by printing "Load failed: 5" and exiting
// 0, so its exit status cannot be believed. Before this was checked, a genuine
// refusal -- a label disabled with `launchctl disable`, which persists, or a
// missing domain -- came back from Install as success while nothing ran.
//
// The runner is faked throughout: this test must never touch the real launchd.
func TestLaunchdInstallDoesNotMaskABootstrapFailure(t *testing.T) {
	bootErr := errors.New("Bootstrap failed: 5: Input/output error")

	newFake := func(t *testing.T, labelLoaded bool) (*launchd, *[]string) {
		t.Helper()
		t.Setenv("HOME", t.TempDir())
		t.Setenv("HENRI_CONFIG", "")
		t.Setenv("XDG_CONFIG_HOME", "")
		m, err := newLaunchd()
		if err != nil {
			t.Fatal(err)
		}
		l := m.(*launchd)
		var calls []string
		l.run = func(name string, args ...string) (string, error) {
			calls = append(calls, name+" "+strings.Join(args, " "))
			switch args[0] {
			case "bootstrap":
				return "", bootErr
			case "load":
				return "Load failed: 5: Input/output error", nil // exits 0 anyway
			case "list":
				if labelLoaded {
					return "{\n\t\"PID\" = 123;\n}", nil
				}
				return "Could not find service \"" + launchdLabel + "\"", errors.New("exit status 113")
			}
			return "", nil
		}
		return l, &calls
	}

	l, calls := newFake(t, false)
	err := l.Install("/usr/local/bin/henri")
	if err == nil {
		t.Fatalf("Install reported success when the label never loaded; ran: %v", *calls)
	}
	if !errors.Is(err, bootErr) {
		t.Errorf("Install returned %v, want the original bootstrap error", err)
	}

	// And when the fallback really did load it, the failure is not reported.
	l, _ = newFake(t, true)
	if err := l.Install("/usr/local/bin/henri"); err != nil {
		t.Errorf("Install failed even though the label loaded: %v", err)
	}
}

// The unit records DISPLAY, XAUTHORITY and the config path, so it is nobody
// else's business; and where its directory is shared, a symlink planted at the
// path would make `service install` overwrite whatever it names.
func TestWriteUnitIsPrivateAndRefusesSymlinks(t *testing.T) {
	dir := t.TempDir()

	unit := filepath.Join(dir, "henri.service")
	if err := writeUnit(unit, "[Service]\n"); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(unit)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("unit written with mode %04o, want 0600", perm)
	}

	// An older henri left a world-readable file behind; rewriting tightens it.
	if err := os.Chmod(unit, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeUnit(unit, "[Service]\n"); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(unit); err != nil {
		t.Fatal(err)
	} else if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("rewriting left mode %04o, want 0600", perm)
	}

	target := filepath.Join(dir, "elsewhere")
	link := filepath.Join(dir, "linked.service")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("cannot create symlinks here: %v", err)
	}
	if err := writeUnit(link, "[Service]\n"); err == nil {
		t.Error("writeUnit followed a symlink")
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Errorf("writeUnit wrote through the symlink to %s", target)
	}
}
