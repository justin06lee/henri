package config

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func withTempConfig(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	t.Setenv("HENRI_CONFIG", path)
	return path
}

func TestSaveLoadRoundTrip(t *testing.T) {
	path := withTempConfig(t)

	cfg, err := New("laptop")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Peers = []string{"10.0.0.5:47600"}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}

	got, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if got.GroupID != cfg.GroupID || got.Key != cfg.Key || got.DeviceID != cfg.DeviceID {
		t.Fatal("identity did not survive the round trip")
	}
	if len(got.Peers) != 1 || got.Peers[0] != "10.0.0.5:47600" {
		t.Fatalf("peers came back as %v", got.Peers)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
}

// The file holds the group secret, so it must never be written readable by
// anyone else.
func TestSaveIsPrivate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix permissions only")
	}
	path := withTempConfig(t)
	cfg, err := New("laptop")
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("config written with mode %04o, want 0600", perm)
	}
}

func TestLoadRefusesLoosePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix permissions only")
	}
	path := withTempConfig(t)
	cfg, err := New("laptop")
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(); err == nil {
		t.Fatal("a world-readable config holding the group key was accepted")
	}
}

func TestLoadWithoutConfigExplainsItself(t *testing.T) {
	withTempConfig(t)
	_, err := Load()
	if err == nil {
		t.Fatal("expected an error when no config exists")
	}
	if got := err.Error(); got == "" {
		t.Fatal("error message was empty")
	}
}

func TestNewGeneratesDistinctIdentities(t *testing.T) {
	a, err := New("a")
	if err != nil {
		t.Fatal(err)
	}
	b, err := New("b")
	if err != nil {
		t.Fatal(err)
	}
	if a.Key == b.Key {
		t.Fatal("two new groups share a key")
	}
	if a.GroupID == b.GroupID || a.DeviceID == b.DeviceID {
		t.Fatal("two new groups share an identifier")
	}
	key, err := a.MasterKey()
	if err != nil {
		t.Fatal(err)
	}
	if len(key) != 32 {
		t.Fatalf("generated a %d byte key, want 32", len(key))
	}
}

func TestValidateRejectsBadKey(t *testing.T) {
	cfg := &Config{GroupID: "g", DeviceID: "d", Key: "not base64!!"}
	if err := cfg.Validate(); err == nil {
		t.Fatal("a non-base64 key was accepted")
	}
	cfg.Key = "c2hvcnQ=" // "short"
	if err := cfg.Validate(); err == nil {
		t.Fatal("an undersized key was accepted")
	}
}

func TestDefaultsAreFilledIn(t *testing.T) {
	withTempConfig(t)
	cfg, err := New("laptop")
	if err != nil {
		t.Fatal(err)
	}
	cfg.PollMillis = 0
	cfg.MaxBytes = 0
	cfg.ListenPort = 0
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	got, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if got.PollMillis != DefaultPollMillis || got.MaxBytes != DefaultMaxBytes || got.ListenPort != DefaultListenPort {
		t.Fatalf("defaults were not applied: %+v", got)
	}
}
