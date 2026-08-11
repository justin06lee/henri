package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
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

	cfg, err := New("laptop", 0)
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
	cfg, err := New("laptop", 0)
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
	cfg, err := New("laptop", 0)
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
	a, err := New("a", 0)
	if err != nil {
		t.Fatal(err)
	}
	b, err := New("b", 0)
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
	cfg, err := New("laptop", 0)
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

// The phrase is the whole secret: joining with it must land on exactly the
// same group, while still giving this device its own identity.
func TestJoinWithPhraseReproducesTheGroup(t *testing.T) {
	withTempConfig(t)
	origin, err := New("first-device", 0)
	if err != nil {
		t.Fatal(err)
	}
	if origin.Phrase == "" {
		t.Fatal("a new group has no recovery phrase")
	}

	joined, err := Join(origin.Phrase, "second-device")
	if err != nil {
		t.Fatal(err)
	}
	if joined.GroupID != origin.GroupID {
		t.Fatalf("joined group %s, want %s", joined.GroupID, origin.GroupID)
	}
	if joined.Key != origin.Key {
		t.Fatal("joining the same phrase produced a different key")
	}
	if joined.DeviceID == origin.DeviceID {
		t.Fatal("the joining device reused the first device's ID")
	}
	if joined.DeviceName != "second-device" {
		t.Fatalf("device name is %q", joined.DeviceName)
	}
}

// However the words were typed, the stored phrase should be canonical.
func TestJoinNormalisesThePhrase(t *testing.T) {
	withTempConfig(t)
	origin, err := New("first", 0)
	if err != nil {
		t.Fatal(err)
	}
	messy := "  " + strings.ToUpper(strings.ReplaceAll(origin.Phrase, " ", ",  ")) + "  "
	joined, err := Join(messy, "second")
	if err != nil {
		t.Fatalf("a messily typed phrase was rejected: %v", err)
	}
	if joined.Phrase != origin.Phrase {
		t.Fatalf("stored phrase %q, want %q", joined.Phrase, origin.Phrase)
	}
	if joined.Key != origin.Key {
		t.Fatal("a messily typed phrase produced a different key")
	}
}

func TestJoinRejectsABadPhrase(t *testing.T) {
	withTempConfig(t)
	if _, err := Join("not actually a recovery phrase at all", "x"); err == nil {
		t.Fatal("nonsense was accepted as a phrase")
	}
}

func TestWordCountControlsPhraseLength(t *testing.T) {
	withTempConfig(t)
	for bits, want := range map[int]int{128: 12, 256: 24} {
		cfg, err := New("laptop", bits)
		if err != nil {
			t.Fatal(err)
		}
		if got := len(strings.Fields(cfg.Phrase)); got != want {
			t.Errorf("%d bits gave %d words, want %d", bits, got, want)
		}
	}
}

func TestRemoveDeletesTheConfig(t *testing.T) {
	path := withTempConfig(t)
	cfg, err := New("laptop", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	removed, err := Remove()
	if err != nil {
		t.Fatal(err)
	}
	if removed != path {
		t.Fatalf("removed %s, want %s", removed, path)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("the config file is still there")
	}
	if _, err := Load(); err == nil {
		t.Fatal("Load succeeded after Remove")
	}
}

func TestRemoveWithoutAConfigReportsIt(t *testing.T) {
	withTempConfig(t)
	if _, err := Remove(); err == nil {
		t.Fatal("removing a config that does not exist reported success")
	}
}

// The default phrase length is a product decision, not an accident: check it
// stays put.
func TestDefaultPhraseIsFifteenWords(t *testing.T) {
	withTempConfig(t)
	cfg, err := New("laptop", 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(strings.Fields(cfg.Phrase)); got != 15 {
		t.Fatalf("the default phrase is %d words, want 15", got)
	}
}
