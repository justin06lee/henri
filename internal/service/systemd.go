package service

import (
	"os"
	"path/filepath"
	"strings"
)

const systemdUnit = "henri.service"

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
	if err := writeUnit(s.unit, s.render(binary)); err != nil {
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
		LogHint:  "journalctl --user -u henri -f",
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

func (s *systemd) render(binary string) string {
	captured := sessionEnv()
	for k, v := range configEnv() {
		captured[k] = v
	}
	var env strings.Builder
	for _, k := range sortedKeys(captured) {
		env.WriteString("Environment=" + k + "=" + captured[k] + "\n")
	}

	return `# Written by ` + "`henri service install`" + `. Edit with care; reinstalling overwrites it.
#
# A user unit, never a system one: the clipboard belongs to your graphical
# session, so a service running as root would have nothing to read.

[Unit]
Description=henri - shared clipboard
Documentation=https://github.com/justin06lee/henri
After=graphical-session.target

[Service]
Type=simple
ExecStart=` + binary + ` daemon
Restart=on-failure
RestartSec=5

# Captured from the session that ran ` + "`henri service install`" + `. The systemd
# user manager often does not inherit these, and the clipboard helpers cannot
# reach the display without them.
` + env.String() + `
# The config holds the group key; keep the process from wandering.
NoNewPrivileges=true
PrivateTmp=true

[Install]
WantedBy=default.target
`
}
