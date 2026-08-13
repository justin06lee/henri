// Package config loads and stores henri's on-disk configuration.
//
// The config file holds the group's shared secret, so it is always written
// with 0600 permissions and refused if the file is readable by anyone else.
package config

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/justin06lee/henri/internal/mnemonic"
	"github.com/justin06lee/henri/internal/secure"
)

// Defaults used by `henri init` and filled in for older config files.
const (
	DefaultListenPort    = 47600
	DefaultDiscoveryPort = 47601
	DefaultPollMillis    = 400
	DefaultMaxBytes      = 4 << 20 // 4 MiB

	// DefaultEntropyBits is the strength of a new group's recovery phrase.
	// 160 bits is fifteen words. BIP-39 phrase lengths are always a multiple of
	// three -- each word carries 11 bits and the checksum is a 32nd of the
	// entropy, so the word count works out to 3*bits/32 (128 bits is twelve
	// words) and only 12, 15, 18, 21 and 24 come out whole.
	DefaultEntropyBits = 160
)

// Config is the whole of henri's persistent state.
type Config struct {
	// GroupID identifies the clipboard group. Every device in a group shares
	// the same GroupID and Key.
	GroupID string `json:"group_id"`
	// Key is the group's 32-byte master secret, base64 (std, padded).
	// Everything on the wire is derived from it.
	Key string `json:"key"`
	// Phrase is the recovery phrase this group was built from. GroupID and Key
	// are both derived from it, so it is the only thing worth writing down.
	// Configs created before phrases existed leave this empty and keep working.
	Phrase string `json:"phrase,omitempty"`

	DeviceID   string `json:"device_id"`
	DeviceName string `json:"device_name"`

	ListenPort    int  `json:"listen_port"`
	DiscoveryPort int  `json:"discovery_port"`
	Discovery     bool `json:"discovery"`

	// Peers are dialled directly in addition to anything discovery finds.
	// Use these for devices that are not on the same LAN.
	Peers []string `json:"peers"`

	PollMillis int `json:"poll_interval_ms"`
	// PollOnly disables event-driven clipboard watching and goes back to
	// checking on a timer. Phrased as an opt-out so the default is the good
	// one; set it if an event source misbehaves on your desktop.
	PollOnly bool `json:"clipboard_poll_only,omitempty"`
	MaxBytes int  `json:"max_payload_bytes"`

	// HideMenuBarIcon turns off the icon the daemon shows in the macOS menu
	// bar while it runs. Phrased as an opt-out so the default is the good one;
	// it does nothing anywhere else, where there is no icon to hide.
	HideMenuBarIcon bool `json:"hide_menu_bar_icon,omitempty"`
}

// Path returns the config file location, honouring $HENRI_CONFIG and
// $XDG_CONFIG_HOME.
func Path() (string, error) {
	if p := os.Getenv("HENRI_CONFIG"); p != "" {
		return p, nil
	}
	if dir := os.Getenv("XDG_CONFIG_HOME"); dir != "" {
		return filepath.Join(dir, "henri", "config.json"), nil
	}
	if runtime.GOOS == "windows" {
		dir, err := os.UserConfigDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(dir, "henri", "config.json"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "henri", "config.json"), nil
}

// New starts a brand new group: a fresh recovery phrase, with the group's
// identity derived from it. Zero bits means DefaultEntropyBits, so a caller
// with no opinion about phrase length does not have to have one.
func New(deviceName string, bits int) (*Config, error) {
	if bits == 0 {
		bits = DefaultEntropyBits
	}
	phrase, entropy, err := mnemonic.New(bits)
	if err != nil {
		return nil, err
	}
	return fromEntropy(phrase, entropy, deviceName)
}

// Join builds this device's config for an existing group from its phrase.
func Join(phrase, deviceName string) (*Config, error) {
	entropy, err := mnemonic.Decode(phrase)
	if err != nil {
		return nil, err
	}
	// Store the phrase the way it is meant to be written, not however it was
	// typed, so `henri code` always shows the canonical form.
	canonical, err := mnemonic.Encode(entropy)
	if err != nil {
		return nil, err
	}
	return fromEntropy(canonical, entropy, deviceName)
}

// fromEntropy assembles a config around a phrase and the identity it implies.
// The device ID is always fresh: the group is shared, the device is not.
func fromEntropy(phrase string, entropy []byte, deviceName string) (*Config, error) {
	groupID, master, err := secure.DeriveGroup(entropy)
	if err != nil {
		return nil, err
	}
	device, err := randomID(8)
	if err != nil {
		return nil, err
	}
	return &Config{
		GroupID:       groupID,
		Key:           base64.StdEncoding.EncodeToString(master),
		Phrase:        phrase,
		DeviceID:      device,
		DeviceName:    deviceName,
		ListenPort:    DefaultListenPort,
		DiscoveryPort: DefaultDiscoveryPort,
		Discovery:     true,
		// An empty list rather than nil, so a fresh config file says
		// "peers": [] the way the documentation does, instead of "peers": null.
		Peers:      []string{},
		PollMillis: DefaultPollMillis,
		MaxBytes:   DefaultMaxBytes,
	}, nil
}

// Load reads the config from disk and validates it.
func Load() (*Config, error) {
	path, err := Path()
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("no config at %s: run `henri init` or `henri join <words>` first", path)
	}
	if err != nil {
		return nil, err
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("%s is group/world readable (%04o) and holds the group key; run: chmod 600 %s",
			path, info.Mode().Perm(), path)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	c.applyDefaults()
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

// applyDefaults fills in what an older config file, or one written by hand,
// leaves out. A missing number arrives here as zero, which is why the absent
// value and the default are the same thing. It is deliberately not a repair
// shop: a negative port is a mistake worth reporting, not one to paper over, so
// every field uses the same <= 0 test and Validate has the final say.
func (c *Config) applyDefaults() {
	if c.ListenPort <= 0 {
		c.ListenPort = DefaultListenPort
	}
	if c.DiscoveryPort <= 0 {
		c.DiscoveryPort = DefaultDiscoveryPort
	}
	if c.PollMillis <= 0 {
		c.PollMillis = DefaultPollMillis
	}
	if c.MaxBytes <= 0 {
		c.MaxBytes = DefaultMaxBytes
	}
	if c.DeviceName == "" {
		c.DeviceName, _ = os.Hostname()
	}
	if c.Peers == nil {
		c.Peers = []string{}
	}
}

// Validate checks the invariants the rest of the program relies on.
//
// Everything here is something that would otherwise fail much later and much
// further away: a zero poll interval panics the daemon's ticker, a zero payload
// limit refuses every copy without a word, and a port of 99999 surfaces as
// "listen tcp: address 99999: invalid port" from a command that never mentioned
// a port. Load and Save both call it, so neither a file nor a Config built in
// code can carry one of those past this point.
func (c *Config) Validate() error {
	if strings.TrimSpace(c.GroupID) == "" {
		return errors.New("config: group_id is empty")
	}
	if strings.TrimSpace(c.DeviceID) == "" {
		return errors.New("config: device_id is empty")
	}
	key, err := c.MasterKey()
	if err != nil {
		return err
	}
	if len(key) != 32 {
		return fmt.Errorf("config: key must be 32 bytes, got %d", len(key))
	}
	if err := checkPort("listen_port", c.ListenPort); err != nil {
		return err
	}
	// The discovery port only has to be a port when discovery is on: with it
	// off, nothing ever binds that socket, and a config that says nothing about
	// a port it will not use is not wrong.
	if c.Discovery || c.DiscoveryPort != 0 {
		if err := checkPort("discovery_port", c.DiscoveryPort); err != nil {
			return err
		}
		if c.ListenPort == c.DiscoveryPort {
			return fmt.Errorf("config: listen_port and discovery_port are both %d, "+
				"but one is TCP for clipboard payloads and the other UDP for discovery; give them different numbers",
				c.ListenPort)
		}
	}
	if c.PollMillis <= 0 {
		return fmt.Errorf("config: poll_interval_ms must be at least 1, got %d", c.PollMillis)
	}
	if c.MaxBytes <= 0 {
		return fmt.Errorf("config: max_payload_bytes must be at least 1, got %d "+
			"(a limit of zero silently refuses every copy)", c.MaxBytes)
	}
	return c.checkPhrase()
}

// checkPort rejects a number that is not a port. Zero is included: an absent
// field is filled in by applyDefaults long before this, so a zero that reaches
// here was asked for.
func checkPort(what string, port int) error {
	if port < 1 || port > 65535 {
		return fmt.Errorf("config: %s must be between 1 and 65535, got %d", what, port)
	}
	return nil
}

// checkPhrase makes sure the group this config joins is the group its recovery
// phrase names.
//
// group_id and key are both derived from the phrase, so they cannot disagree
// with it by accident -- but they can be made to. Anyone who can write the file
// can swap in their own key and group ID and leave the user's real words in
// place: the daemon then encrypts everything to their group, `henri code` still
// prints the familiar words, and there is nothing on screen to notice. Deriving
// the identity again and comparing is what makes the phrase the authority it is
// documented to be.
//
// Configs from before phrases existed have no phrase and are left alone; that
// is what the legacy `henri1:` join code produces, and those groups still work.
func (c *Config) checkPhrase() error {
	if c.Phrase == "" {
		return nil
	}
	entropy, err := mnemonic.Decode(c.Phrase)
	if err != nil {
		return fmt.Errorf("config: the recovery phrase in this config is not a valid phrase: %w", err)
	}
	groupID, master, err := secure.DeriveGroup(entropy)
	if err != nil {
		return err
	}
	key, err := c.MasterKey()
	if err != nil {
		return err
	}
	// Constant time for the key: it is the secret, and the comparison is one an
	// attacker who can rewrite the file can also run repeatedly.
	if groupID != c.GroupID || subtle.ConstantTimeCompare(key, master) != 1 {
		return errors.New("config: this config does not match its recovery phrase.\n" +
			"       group_id and key are supposed to be derived from the words in it, and they are not,\n" +
			"       so this device would sync with a group other than the one those words name.\n" +
			"       If you did not edit the file yourself, treat it as tampered with: run\n" +
			"       `henri join <words>` with the phrase you trust to rebuild it")
	}
	return nil
}

// MasterKey decodes the group secret.
func (c *Config) MasterKey() ([]byte, error) {
	key, err := base64.StdEncoding.DecodeString(c.Key)
	if err != nil {
		return nil, fmt.Errorf("config: key is not valid base64: %w", err)
	}
	return key, nil
}

// tempPrefix and tempSuffix bracket the name of a half-written config. Every
// such file is a complete copy of the group key, so the pattern is also what
// Remove looks for when it takes the key off this device.
const (
	tempPrefix = ".config-"
	tempSuffix = ".json"
)

// Save writes the config atomically with 0600 permissions.
//
// "Atomically" is meant properly, which it was not before. The temp file gets a
// unique name from os.CreateTemp rather than a fixed one, because O_CREATE's
// mode is ignored when the file already exists: a leftover config.json.tmp at
// 0644 made the saved config -- group key and all -- world readable, after
// which every henri command refused to run. A unique name also stops two henri
// processes writing the same temp file over each other. The contents are
// flushed before the rename and the directory after it, so a crash leaves
// either the old config or the new one and never a truncated file.
//
// The rename lands on the path with its symlinks resolved. `~/.config` as a
// link into a dotfiles directory is a setup henri documents and checks for
// elsewhere, and renaming onto the link replaced it with a regular file: reads
// carried on working, and the copy under version control silently stopped being
// the one in use.
func (c *Config) Save() error {
	path, err := Path()
	if err != nil {
		return err
	}
	// Refusing to write an invalid config here is what stops `henri init -port
	// 99999` reporting success and leaving a config nothing can load. Note that
	// applyDefaults is deliberately not called first: filling in a missing
	// field is for reading an older file, and doing it on the way out would
	// turn `-port 0` into 47600 rather than saying that 0 is not a port.
	if err := c.Validate(); err != nil {
		return err
	}
	if c.Peers == nil {
		// nil marshals as null, and the documented shape of a fresh config is
		// an empty array.
		c.Peers = []string{}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	target, err := saveTarget(path)
	if err != nil {
		return err
	}
	raw, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')

	dir := filepath.Dir(target)
	f, err := os.CreateTemp(dir, tempPrefix+"*"+tempSuffix)
	if err != nil {
		return err
	}
	tmp := f.Name()
	fail := func(err error) error {
		f.Close()
		os.Remove(tmp)
		return err
	}
	// CreateTemp already makes the file 0600, but this file holds the group key
	// for as long as it exists, so it says so rather than relying on it.
	if err := f.Chmod(0o600); err != nil {
		return fail(err)
	}
	if _, err := f.Write(raw); err != nil {
		return fail(err)
	}
	if err := f.Sync(); err != nil {
		return fail(err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, target); err != nil {
		os.Remove(tmp)
		return err
	}
	syncDir(dir)
	return nil
}

// saveTarget resolves the path Save should rename onto, so a symlinked config
// is still a symlink afterwards.
//
// The link is only followed where it lands on an ordinary file. A config path
// that turns out to be a directory, a socket or a device is not something to
// write the group key through, and a link that resolves to nothing at all is
// more likely to be a mistake than an intention.
func saveTarget(path string) (string, error) {
	fi, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return path, nil
	}
	if err != nil {
		return "", err
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		if !fi.Mode().IsRegular() {
			return "", fmt.Errorf("config: %s is a %s, not a regular file; refusing to write over it",
				path, describeMode(fi.Mode()))
		}
		return path, nil
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("config: %s is a symlink that leads nowhere henri can write: %w", path, err)
	}
	rfi, err := os.Stat(resolved)
	if err != nil {
		return "", err
	}
	if !rfi.Mode().IsRegular() {
		return "", fmt.Errorf("config: %s points at %s, which is a %s rather than a regular file; refusing to write through it",
			path, resolved, describeMode(rfi.Mode()))
	}
	return resolved, nil
}

func describeMode(m os.FileMode) string {
	switch {
	case m.IsDir():
		return "directory"
	case m&os.ModeSocket != 0:
		return "socket"
	case m&os.ModeNamedPipe != 0:
		return "named pipe"
	case m&os.ModeDevice != 0:
		return "device"
	default:
		return m.Type().String()
	}
}

// syncDir flushes a directory entry so that a rename survives a crash rather
// than only a process exit. Best effort on purpose: not every filesystem or
// platform lets a directory be opened for this, and failing to flush is no
// reason to report a config that is on disk as unwritten.
func syncDir(dir string) {
	d, err := os.Open(dir)
	if err != nil {
		return
	}
	_ = d.Sync()
	_ = d.Close()
}

// Remove deletes the config file, taking this device out of its group.
// It returns the path that was removed.
//
// It takes the half-written copies with it. `henri leave` tells the user there
// is no way back into the group from this device once the config is gone, and
// an abandoned config.json.tmp -- which every interrupted save used to leave
// behind -- held the group key in full, so that was not true. Where the config
// is a symlink, the file it points at goes too: removing only the link would
// leave the key sitting in the dotfiles directory it came from.
func Remove() (string, error) {
	path, err := Path()
	if err != nil {
		return "", err
	}
	target := linkTarget(path)
	if target != "" {
		if err := os.Remove(target); err != nil && !errors.Is(err, os.ErrNotExist) {
			return target, err
		}
	}
	if err := os.Remove(path); err != nil {
		return path, err
	}
	removeStrays(path)
	if target != "" {
		removeStrays(target)
	}
	return path, nil
}

// linkTarget returns what path's own last component points at, or "" when path
// is not itself a symlink.
//
// EvalSymlinks alone cannot answer this: it resolves the whole path, parent
// directories included, and on macOS the temporary directory alone comes back
// as /private/var/... for /var/... -- the same file under another name, not a
// second one to delete.
func linkTarget(path string) string {
	fi, err := os.Lstat(path)
	if err != nil || fi.Mode()&os.ModeSymlink == 0 {
		return ""
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil || resolved == path {
		return ""
	}
	return resolved
}

// removeStrays deletes the temporary files a save can leave behind, each one a
// complete copy of the config. ErrNotExist is the normal case and is ignored;
// so is anything else, because failing to tidy up is not a reason to report a
// leave that has already happened as failed.
func removeStrays(path string) {
	// The fixed name older versions of henri used.
	_ = os.Remove(path + ".tmp")

	matches, err := filepath.Glob(filepath.Join(filepath.Dir(path), tempPrefix+"*"+tempSuffix))
	if err != nil {
		return
	}
	for _, m := range matches {
		// os.CreateTemp fills the wildcard with digits and nothing else, so
		// anything else in there is a file henri did not write.
		middle := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(m), tempPrefix), tempSuffix)
		if middle == "" || strings.ContainsFunc(middle, func(r rune) bool { return r < '0' || r > '9' }) {
			continue
		}
		_ = os.Remove(m)
	}
}

func randomID(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// NewDeviceID returns a fresh identifier for this device.
func NewDeviceID() (string, error) { return randomID(8) }

// ResolvedPath returns the config location with symlinks followed. It is the
// path the kernel actually opens, which is what matters when deciding whether
// the file lives somewhere a background service can reach: `~/.config` is very
// often a link into a dotfiles directory somewhere else entirely.
func ResolvedPath() (string, error) {
	path, err := Path()
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved, nil
	}
	// The file may not exist yet; resolve as much of the directory as we can.
	if dir, err := filepath.EvalSymlinks(filepath.Dir(path)); err == nil {
		return filepath.Join(dir, filepath.Base(path)), nil
	}
	return path, nil
}
