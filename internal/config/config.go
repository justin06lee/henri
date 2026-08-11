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
)

// Defaults used by `henri init` and filled in for older config files.
const (
	DefaultListenPort    = 47600
	DefaultDiscoveryPort = 47601
	DefaultPollMillis    = 400
	DefaultMaxBytes      = 4 << 20 // 4 MiB
)

// Config is the whole of henri's persistent state.
type Config struct {
	// GroupID identifies the clipboard group. Every device in a group shares
	// the same GroupID and Key.
	GroupID string `json:"group_id"`
	// Key is the group's 32-byte master secret, base64 (std, padded).
	// Everything on the wire is derived from it.
	Key string `json:"key"`

	DeviceID   string `json:"device_id"`
	DeviceName string `json:"device_name"`

	ListenPort    int  `json:"listen_port"`
	DiscoveryPort int  `json:"discovery_port"`
	Discovery     bool `json:"discovery"`

	// Peers are dialled directly in addition to anything discovery finds.
	// Use these for devices that are not on the same LAN.
	Peers []string `json:"peers"`

	PollMillis int `json:"poll_interval_ms"`
	MaxBytes   int `json:"max_payload_bytes"`
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

// New builds a fresh config for a brand new group.
func New(deviceName string) (*Config, error) {
	group, err := randomID(8)
	if err != nil {
		return nil, err
	}
	device, err := randomID(8)
	if err != nil {
		return nil, err
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	return &Config{
		GroupID:       group,
		Key:           base64.StdEncoding.EncodeToString(key),
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
		return nil, fmt.Errorf("no config at %s: run `henri init` or `henri join <code>` first", path)
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

func randomID(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
