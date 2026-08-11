// Package service installs henri into whatever runs background programs on
// this machine, so the daemon starts at login and stays out of the way.
//
// On macOS that is a launchd LaunchAgent, on Linux a systemd user unit. Both
// are per-user on purpose: the clipboard belongs to a graphical session, so a
// system-wide service running as root would have nothing to read or write.
package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

// Label identifies henri to the service manager. Both managers build their own
// name from it: launchd wants reverse DNS, systemd wants a unit filename.
const Label = "henri"

// ErrUnsupported means this platform has no integration yet.
var ErrUnsupported = errors.New("service: henri does not know how to install a background service on this system")

// Status describes henri's presence in the service manager.
type Status struct {
	Manager   string // "launchd" or "systemd"
	Installed bool   // the unit file is on disk
	Enabled   bool   // it will come back at login
	Running   bool   // it is running right now
	UnitPath  string
	LogHint   string // how to read the logs, as a person would type it
	// LogCmd is the same thing as an argv, for callers that want to run it.
	// LogHint cannot be split on spaces to get here: a home directory with a
	// space in it turns one path into two arguments.
	LogCmd []string
	Detail string // whatever the service manager said
}

// Manager installs and controls the background service.
type Manager interface {
	// Install writes the unit for binary and starts it. It is idempotent:
	// running it again updates the unit and restarts.
	Install(binary string) error
	Uninstall() error
	Restart() error
	Status() (Status, error)
	UnitPath() string
	Name() string
}

// New returns the manager for this platform.
func New() (Manager, error) {
	switch runtime.GOOS {
	case "darwin":
		return newLaunchd()
	case "linux", "freebsd":
		return newSystemd()
	default:
		return nil, ErrUnsupported
	}
}

// BinaryPath returns the absolute, symlink-free path of the running henri, so
// the unit points at a real file rather than whatever was on $PATH at the time.
func BinaryPath() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	return filepath.Abs(exe)
}

// sessionEnv captures the handful of variables the clipboard helpers need.
//
// This matters on Linux. `henri service install` is run from a terminal inside
// the graphical session, so it can see DISPLAY and WAYLAND_DISPLAY; the systemd
// user manager frequently cannot, because importing them is left to the desktop
// environment and plenty of setups never do it. Recording them in the unit
// means the service works whether or not the session bothers.
func sessionEnv() map[string]string {
	out := make(map[string]string)
	for _, k := range []string{"WAYLAND_DISPLAY", "DISPLAY", "XDG_RUNTIME_DIR", "XAUTHORITY"} {
		if v := os.Getenv(k); v != "" {
			out[k] = v
		}
	}
	return out
}

// configEnv captures the variables that decide where henri looks for its
// config. A service manager starts with a bare environment, so without these a
// user who points HENRI_CONFIG or XDG_CONFIG_HOME somewhere would get a
// background daemon quietly reading a different file than their shell does.
func configEnv() map[string]string {
	out := make(map[string]string)
	for _, k := range []string{"HENRI_CONFIG", "XDG_CONFIG_HOME"} {
		if v := os.Getenv(k); v != "" {
			out[k] = v
		}
	}
	return out
}

// sortedKeys gives map iteration a stable order so regenerating a unit does not
// churn the file.
func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// commandTimeout bounds one service-manager call. Both systemctl --user and
// launchctl talk over a socket or a bus, and a wedged one would otherwise hang
// `henri status` forever with nothing on screen -- worse than any error.
const commandTimeout = 10 * time.Second

// run executes a command and folds its output into any error, since service
// managers put the useful part on stderr.
func run(name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.CombinedOutput()
	text := strings.TrimSpace(string(out))
	if err != nil {
		if ctx.Err() != nil {
			return text, fmt.Errorf("%s %s: timed out after %s", name, strings.Join(args, " "), commandTimeout)
		}
		if text != "" {
			return text, fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, text)
		}
		return text, fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return text, nil
}

// checkUnitValue rejects the characters that no unit format can carry.
//
// A newline is the dangerous one: both formats are line- or tag-oriented, so a
// value containing one stops being a value and starts being another directive.
// Where the environment is not entirely the user's own -- SSH AcceptEnv, sudo
// -E, a wrapper script -- that is arbitrary content in the unit file.
func checkUnitValue(what, v string) error {
	if i := strings.IndexAny(v, "\n\r\x00"); i >= 0 {
		return fmt.Errorf("service: %s contains a control character (byte %d at offset %d); refusing to write it into a unit file", what, v[i], i)
	}
	return nil
}

// writeUnit writes a unit file, creating its directory.
func writeUnit(path, content string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	// Never write through a symlink. Where XDG_CONFIG_HOME points at somewhere
	// shared, a link planted at this path turns `henri service install` into an
	// overwrite of whatever it names.
	fi, err := os.Lstat(path)
	switch {
	case err == nil && fi.Mode()&os.ModeSymlink != 0:
		return fmt.Errorf("service: %s is a symlink; refusing to write through it", path)
	case err != nil && !os.IsNotExist(err):
		return err
	}

	// 0600, not 0644. The unit records DISPLAY, XAUTHORITY, XDG_RUNTIME_DIR and
	// the config path, and every other local user could read them. The explicit
	// Chmod is for the file that is already there: OpenFile's mode applies only
	// when it creates one, so an install over a unit written by an older henri
	// would otherwise keep the old permissions.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return err
	}
	if _, err := f.WriteString(content); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// VolatileLocation reports why path is a poor thing to point a login service
// at, or "" if it looks stable.
//
// A service records one exact path and runs it forever after. A binary on an
// external drive means henri fails to start every time the drive is not
// plugged in, which looks like henri being broken rather than absent.
func VolatileLocation(path string) string {
	switch {
	case strings.HasPrefix(path, "/Volumes/"):
		return "an external volume"
	case strings.HasPrefix(path, "/media/"), strings.HasPrefix(path, "/mnt/"), strings.HasPrefix(path, "/run/media/"):
		return "a mounted volume"
	case strings.HasPrefix(path, "/tmp/"), strings.HasPrefix(path, "/private/tmp/"), strings.HasPrefix(path, "/var/tmp/"):
		return "a temporary directory"
	}
	return ""
}
