package service

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// launchdLabel is the reverse-DNS name launchd knows henri by.
const launchdLabel = "com.justin06lee.henri"

type launchd struct {
	plist  string
	log    string
	domain string // gui/<uid>, the user's login session
}

func newLaunchd() (Manager, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	return &launchd{
		plist:  filepath.Join(home, "Library", "LaunchAgents", launchdLabel+".plist"),
		log:    filepath.Join(home, "Library", "Logs", "henri.log"),
		domain: fmt.Sprintf("gui/%d", os.Getuid()),
	}, nil
}

func (l *launchd) Name() string     { return "launchd" }
func (l *launchd) UnitPath() string { return l.plist }

func (l *launchd) Install(binary string) error {
	if err := os.MkdirAll(filepath.Dir(l.log), 0o755); err != nil {
		return err
	}
	if err := writeUnit(l.plist, l.render(binary)); err != nil {
		return err
	}

	// bootstrap refuses if the label is already loaded, so clear it first. A
	// failure here just means it was not loaded, which is what we want anyway.
	_, _ = run("launchctl", "bootout", l.domain+"/"+launchdLabel)

	if _, err := run("launchctl", "bootstrap", l.domain, l.plist); err != nil {
		// Older systems only have the deprecated spelling.
		if _, ferr := run("launchctl", "load", "-w", l.plist); ferr != nil {
			return err
		}
	}
	return nil
}

func (l *launchd) Uninstall() error {
	_, _ = run("launchctl", "bootout", l.domain+"/"+launchdLabel)
	_, _ = run("launchctl", "unload", "-w", l.plist)
	if err := os.Remove(l.plist); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func (l *launchd) Restart() error {
	_, err := run("launchctl", "kickstart", "-k", l.domain+"/"+launchdLabel)
	return err
}

func (l *launchd) Status() (Status, error) {
	st := Status{
		Manager:  "launchd",
		UnitPath: l.plist,
		LogHint:  "tail -f " + l.log,
	}
	if _, err := os.Stat(l.plist); err == nil {
		st.Installed = true
		// A LaunchAgent with RunAtLoad comes back at login purely by existing.
		st.Enabled = true
	}

	out, err := run("launchctl", "list", launchdLabel)
	if err != nil {
		return st, nil // not loaded; not an error worth reporting
	}
	st.Running = strings.Contains(out, "\"PID\" =")
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "\"PID\"") || strings.Contains(line, "\"LastExitStatus\"") {
			st.Detail += strings.TrimSpace(line) + " "
		}
	}
	st.Detail = strings.TrimSpace(st.Detail)
	return st, nil
}

func (l *launchd) render(binary string) string {
	var env strings.Builder
	if captured := configEnv(); len(captured) > 0 {
		env.WriteString("\n  <key>EnvironmentVariables</key>\n  <dict>\n")
		for _, k := range sortedKeys(captured) {
			env.WriteString("    <key>" + xmlEscape(k) + "</key>\n")
			env.WriteString("    <string>" + xmlEscape(captured[k]) + "</string>\n")
		}
		env.WriteString("  </dict>\n")
	}

	return `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<!-- Written by ` + "`henri service install`" + `. Edit with care; reinstalling overwrites it. -->
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>` + launchdLabel + `</string>

  <key>ProgramArguments</key>
  <array>
    <string>` + xmlEscape(binary) + `</string>
    <string>daemon</string>
  </array>

` + env.String() + `
  <key>RunAtLoad</key>
  <true/>
  <key>KeepAlive</key>
  <true/>

  <!-- A LaunchAgent, never a LaunchDaemon: the clipboard belongs to the login
       session, so a system-wide service would have nothing to read. -->
  <key>ProcessType</key>
  <string>Interactive</string>

  <key>StandardOutPath</key>
  <string>` + xmlEscape(l.log) + `</string>
  <key>StandardErrorPath</key>
  <string>` + xmlEscape(l.log) + `</string>
</dict>
</plist>
`
}

func xmlEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")
	return r.Replace(s)
}
