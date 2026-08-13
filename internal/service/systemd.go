package service

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

const systemdUnit = Label + ".service"

type systemd struct {
	unit string
}

func newSystemd() (Manager, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		dir = filepath.Join(home, ".config")
	}
	return &systemd{unit: filepath.Join(dir, "systemd", "user", systemdUnit)}, nil
}

func (s *systemd) Name() string     { return "systemd" }
func (s *systemd) UnitPath() string { return s.unit }

func (s *systemd) Install(binary string) error {
	// Under sudo, HOME and XDG_CONFIG_HOME are root's, so the unit lands in
	// /root/.config and is enabled against root's user manager -- a session
	// with no display and no clipboard, while the user's own stays empty.
	// launchd has refused this from the start; systemd just failed later and
	// more confusingly.
	if os.Getuid() == 0 {
		return errors.New("service: run `henri service install` as yourself, not with sudo: a systemd user unit belongs to your own login session")
	}

	unit, err := s.render(binary)
	if err != nil {
		return err
	}
	if err := writeUnit(s.unit, unit); err != nil {
		return err
	}
	if _, err := run("systemctl", "--user", "daemon-reload"); err != nil {
		return err
	}
	if _, err := run("systemctl", "--user", "enable", "--now", systemdUnit); err != nil {
		return err
	}
	// enable --now starts it, but if it was already running with an older unit
	// it keeps the old binary until restarted.
	_, _ = run("systemctl", "--user", "restart", systemdUnit)
	return nil
}

func (s *systemd) Uninstall() error {
	_, _ = run("systemctl", "--user", "disable", "--now", systemdUnit)
	if err := os.Remove(s.unit); err != nil && !os.IsNotExist(err) {
		return err
	}
	_, _ = run("systemctl", "--user", "daemon-reload")
	return nil
}

func (s *systemd) Restart() error {
	_, err := run("systemctl", "--user", "restart", systemdUnit)
	return err
}

func (s *systemd) Status() (Status, error) {
	st := Status{
		Manager:  "systemd",
		UnitPath: s.unit,
		LogHint:  "journalctl --user -u " + Label + " -f",
		LogCmd:   []string{"journalctl", "--user", "-u", systemdUnit, "-f"},
	}
	if _, err := os.Stat(s.unit); err == nil {
		st.Installed = true
	}
	// is-active and is-enabled exit non-zero for the negative answer, so the
	// output is the reliable part, not the error.
	active, _ := run("systemctl", "--user", "is-active", systemdUnit)
	enabled, _ := run("systemctl", "--user", "is-enabled", systemdUnit)
	st.Running = active == "active"
	st.Enabled = enabled == "enabled"
	st.Detail = strings.TrimSpace(active + " / " + enabled)
	return st, nil
}

// systemdValue renders one value for a unit file.
//
// systemd does not treat everything after the "=" as the value. Whitespace
// splits a single Environment= assignment into two, "%" introduces a specifier
// and has to be doubled to mean itself, and the quoted form needs its own
// quotes and backslashes escaped. Callers must have rejected newlines first;
// nothing here can make one safe.
func systemdValue(s string) string {
	s = strings.ReplaceAll(s, "%", "%%")
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`)
	return `"` + r.Replace(s) + `"`
}

func (s *systemd) render(binary string) (string, error) {
	captured := sessionEnv()
	for k, v := range configEnv() {
		captured[k] = v
	}

	if err := checkUnitValue("the henri binary path", binary); err != nil {
		return "", err
	}
	var env strings.Builder
	for _, k := range sortedKeys(captured) {
		if err := checkUnitValue(k, captured[k]); err != nil {
			return "", err
		}
		env.WriteString("Environment=" + systemdValue(k+"="+captured[k]) + "\n")
	}

	return `# Written by ` + "`henri service install`" + `. Edit with care; reinstalling overwrites it.
#
# A user unit, never a system one: the clipboard belongs to your graphical
# session, so a service running as root would have nothing to read.

[Unit]
Description=henri - shared clipboard
Documentation=https://github.com/justin06lee/henri

# After= on its own only orders henri against the graphical session when both
# are being started in the same transaction, which they are not: WantedBy=
# below is what starts henri, and pairing it with PartOf= is what makes the
# session actually pull henri in -- after the compositor, and only once there is
# one. Started from default.target instead, henri reliably came up with no
# display at all.
After=graphical-session.target
PartOf=graphical-session.target

[Service]
Type=simple
ExecStart=` + systemdValue(binary) + ` daemon
Restart=on-failure
RestartSec=5

# Captured from the session that ran ` + "`henri service install`" + `. The systemd
# user manager often does not inherit these, and the clipboard helpers cannot
# reach the display without them.
` + env.String() + `
# The config holds the group key; keep the process from wandering.
#
# No PrivateTmp: X11's local transport is a socket in /tmp/.X11-unix, so a
# private /tmp means xclip and xsel can never open the display -- and where
# unprivileged user namespaces are off, the unit fails outright at
# 226/NAMESPACE. The key it was meant to protect lives in ~/.config anyway.
NoNewPrivileges=true

[Install]
WantedBy=graphical-session.target
`, nil
}
