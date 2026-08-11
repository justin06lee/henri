// Package config loads and stores henri's on-disk configuration.
//
// The config file holds the group's shared secret, so it is always written
// with 0600 permissions and refused if the file is readable by anyone else.
package config

import (
	"crypto/rand"
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
	// entropy, so the word count works out to 32*bits/3 and only 12, 15, 18, 21
	// and 24 come out whole.
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
// identity derived from it.
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
		PollMillis:    DefaultPollMillis,
		MaxBytes:      DefaultMaxBytes,
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

func (c *Config) applyDefaults() {
	if c.ListenPort == 0 {
		c.ListenPort = DefaultListenPort
	}
	if c.DiscoveryPort == 0 {
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
}

// Validate checks the invariants the rest of the program relies on.
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

// Save writes the config atomically with 0600 permissions.
func (c *Config) Save() error {
	path, err := Path()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')

	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// Remove deletes the config file, taking this device out of its group.
// It returns the path that was removed.
func Remove() (string, error) {
	path, err := Path()
	if err != nil {
		return "", err
	}
	if err := os.Remove(path); err != nil {
		return path, err
	}
	return path, nil
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
