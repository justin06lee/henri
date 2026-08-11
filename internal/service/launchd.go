package service

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// launchdLabel is the reverse-DNS name launchd knows henri by.
const launchdLabel = "com.justin06lee." + Label

type launchd struct {
	plist  string
	log    string
	domain string // gui/<uid>, the user's login session
	// run is how launchctl is invoked. It is a field only so tests can watch
	// what would be run without ever touching the user's real launchd.
	run func(name string, args ...string) (string, error)
}

func newLaunchd() (Manager, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	return &launchd{
		plist:  filepath.Join(home, "Library", "LaunchAgents", launchdLabel+".plist"),
		log:    filepath.Join(home, "Library", "Logs", Label+".log"),
		domain: fmt.Sprintf("gui/%d", os.Getuid()),
		run:    run,
	}, nil
}

func (l *launchd) Name() string     { return "launchd" }
func (l *launchd) UnitPath() string { return l.plist }

func (l *launchd) Install(binary string) error {
	// Under sudo the domain built above is gui/0, which is root's login session
	// and not the one holding the pasteboard. The agent would be installed
	// somewhere it can never see a clipboard, while the user's own domain stays
	// empty -- and everything would report success.
	if os.Getuid() == 0 {
		return errors.New("service: run `henri service install` as yourself, not with sudo: a LaunchAgent belongs to your own login session")
	}

	plist, err := l.render(binary)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(l.log), 0o755); err != nil {
		return err
	}
	if err := writeUnit(l.plist, plist); err != nil {
		return err
	}

	// bootstrap refuses if the label is already loaded, so clear it first. A
	// failure here just means it was not loaded, which is what we want anyway.
	_, _ = l.run("launchctl", "bootout", l.domain+"/"+launchdLabel)

	if _, err := l.run("launchctl", "bootstrap", l.domain, l.plist); err != nil {
		// Older systems only have the deprecated spelling. Its exit status says
		// nothing useful -- `launchctl load -w` prints "Load failed: 5" and
		// still exits 0 -- so ask launchd whether the label actually arrived,
		// and if it did not, report why bootstrap refused rather than claiming
		// an install that did not happen.
		if _, ferr := l.run("launchctl", "load", "-w", l.plist); ferr != nil {
			return err
		}
		if _, lerr := l.run("launchctl", "list", launchdLabel); lerr != nil {
			return err
		}
	}
	return nil
}

func (l *launchd) Uninstall() error {
	_, _ = l.run("launchctl", "bootout", l.domain+"/"+launchdLabel)
	_, _ = l.run("launchctl", "unload", "-w", l.plist)
	if err := os.Remove(l.plist); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func (l *launchd) Restart() error {
	_, err := l.run("launchctl", "kickstart", "-k", l.domain+"/"+launchdLabel)
	return err
}

func (l *launchd) Status() (Status, error) {
	st := Status{
		Manager:  "launchd",
		UnitPath: l.plist,
		LogHint:  "tail -f " + l.log,
		LogCmd:   []string{"tail", "-f", l.log},
	}
	if _, err := os.Stat(l.plist); err == nil {
		st.Installed = true
	}
	st.Enabled = l.enabledAtLogin(st.Installed)

	out, err := l.run("launchctl", "list", launchdLabel)
	if err != nil {
		// "Could not find service" is the ordinary answer for an agent that is
		// not loaded, and it comes back as a non-zero exit. Anything else went
		// wrong somewhere the user can do something about, so say so instead of
		// quietly reporting "not running".
		if !strings.Contains(out, "Could not find service") {
			st.Detail = strings.TrimSpace(out)
		}
		return st, nil
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

// enabledAtLogin reports whether launchd will start the agent at the next login.
//
// The plist existing is not the answer. `launchctl bootout` and `launchctl
// disable` record a disabled state in launchd's own override database, and that
// survives a reboot and any number of rewrites of the file -- so an agent that
// will never come back looks identical on disk to one that will. Only launchd
// knows, and print-disabled is how it says.
func (l *launchd) enabledAtLogin(installed bool) bool {
	out, err := l.run("launchctl", "print-disabled", l.domain)
	if err != nil {
		// Older launchctl has no print-disabled; the file is all we have.
		return installed
	}
	for _, line := range strings.Split(out, "\n") {
		if !strings.Contains(line, `"`+launchdLabel+`"`) {
			continue
		}
		// Recent macOS prints `=> disabled` where older ones print `=> true`.
		return !strings.Contains(line, "disabled") && !strings.Contains(line, "true")
	}
	// No override recorded, so it does whatever the plist says.
	return installed
}

func (l *launchd) render(binary string) (string, error) {
	// The dict is written whether or not the user has anything of their own to
	// put in it, because launchd hands a job an environment with no locale at
	// all -- and pbcopy and pbpaste pick their text encoding from LANG and
	// LC_ALL, falling back to plain C when neither is set. Without this every
	// accented character, curly quote, em-dash, CJK character and emoji is
	// mangled in both directions, but only under launchd, so it looks fine
	// everywhere it is easy to test.
	captured := map[string]string{"LANG": "en_US.UTF-8"}
	for k, v := range configEnv() {
		captured[k] = v
	}

	if err := checkUnitValue("the henri binary path", binary); err != nil {
		return "", err
	}
	var env strings.Builder
	env.WriteString("\n  <key>EnvironmentVariables</key>\n  <dict>\n")
	for _, k := range sortedKeys(captured) {
		// XML escaping alone is not enough: a control character survives it and
		// produces a plist launchd then refuses to parse.
		if err := checkUnitValue(k, captured[k]); err != nil {
			return "", err
		}
		env.WriteString("    <key>" + xmlEscape(k) + "</key>\n")
		env.WriteString("    <string>" + xmlEscape(captured[k]) + "</string>\n")
	}
	env.WriteString("  </dict>\n")

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

  <!-- Come back when henri crashes, but not when it exits cleanly. A plain
       KeepAlive restarts on any exit at all, so a daemon that gives up at
       startup -- bad config, port already taken, binary on an unplugged drive
       -- respawns every few seconds forever, appends to an unrotated log, and
       cannot be stopped by killing it. -->
  <key>KeepAlive</key>
  <dict>
    <key>SuccessfulExit</key>
    <false/>
  </dict>
  <key>ThrottleInterval</key>
  <integer>10</integer>

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
`, nil
}

func xmlEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")
	return r.Replace(s)
}
