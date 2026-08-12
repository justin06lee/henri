package config

import (
	"fmt"
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

// A config file written by an older henri, or edited by hand, can be missing
// numbers henri now needs. Load fills them in -- but only on the way in: Save
// rejects a zero rather than quietly substituting one, so `henri init -port 0`
// is reported instead of silently becoming 47600.
func TestDefaultsAreFilledInWhenReadingAFile(t *testing.T) {
	withTempConfig(t)
	cfg, err := New("laptop", 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{
		// The fields are simply not there.
		fmt.Sprintf(`{"group_id":%q,"key":%q,"device_id":"abc","device_name":"old"}`, cfg.GroupID, cfg.Key),
		// Or they are there and zero.
		fmt.Sprintf(`{"group_id":%q,"key":%q,"device_id":"abc","device_name":"old",`+
			`"listen_port":0,"discovery_port":0,"poll_interval_ms":0,"max_payload_bytes":0}`, cfg.GroupID, cfg.Key),
	} {
		writeRawConfig(t, body)
		got, err := Load()
		if err != nil {
			t.Fatalf("%s: %v", body, err)
		}
		if got.PollMillis != DefaultPollMillis || got.MaxBytes != DefaultMaxBytes ||
			got.ListenPort != DefaultListenPort || got.DiscoveryPort != DefaultDiscoveryPort {
			t.Fatalf("defaults were not applied: %+v", got)
		}
	}
}

// writeRawConfig puts a config file on disk without going through Save, which
// is the only way to produce the shapes an older henri wrote.
func writeRawConfig(t *testing.T, body string) {
	t.Helper()
	path, err := Path()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body+"\n"), 0o600); err != nil {
		t.Fatal(err)
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

// `~/.config` is very often a symlink into a dotfiles directory -- henri's own
// service-install checks are built around that being normal. Renaming onto the
// link replaced it with a regular file, and the copy under version control
// silently stopped being the one in use.
func TestSaveKeepsASymlinkedConfigASymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need a privilege on windows")
	}
	dir := t.TempDir()
	dotfiles := filepath.Join(dir, "dotfiles")
	if err := os.MkdirAll(dotfiles, 0o700); err != nil {
		t.Fatal(err)
	}
	real := filepath.Join(dotfiles, "config.json")
	if err := os.WriteFile(real, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "config.json")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HENRI_CONFIG", link)

	cfg, err := New("laptop", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}

	fi, err := os.Lstat(link)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Fatal("saving replaced the symlink with a regular file")
	}
	raw, err := os.ReadFile(real)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), cfg.GroupID) {
		t.Fatal("the file the symlink points at was not the one written")
	}
	// And the temp file went in the directory it was renamed into, not the
	// directory the link lives in.
	if entries, _ := os.ReadDir(dir); len(entries) != 2 { // dotfiles + config.json
		t.Fatalf("%s has %d entries after a save, want 2", dir, len(entries))
	}
}

func TestSaveRefusesToWriteThroughASymlinkToADirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need a privilege on windows")
	}
	dir := t.TempDir()
	link := filepath.Join(dir, "config.json")
	if err := os.Symlink(t.TempDir(), link); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HENRI_CONFIG", link)

	cfg, err := New("laptop", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Save(); err == nil {
		t.Fatal("saving through a symlink to a directory was allowed")
	}
}

// The temp file used to have a fixed name, and O_CREATE's mode is ignored for a
// file that already exists. One leftover config.json.tmp at 0644 was enough to
// publish the group key to every other user on the machine -- after which henri
// refused to run at all, because Load checks the mode it just wrote.
func TestSaveDoesNotInheritAStrayTempFilesPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix permissions only")
	}
	path := withTempConfig(t)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".tmp", []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}
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
		t.Fatalf("the config was written %04o, want 0600", perm)
	}
	if _, err := Load(); err != nil {
		t.Fatalf("the config henri just wrote will not load: %v", err)
	}
}

// A successful save leaves nothing behind but the config.
func TestSaveLeavesNoTemporaryFile(t *testing.T) {
	path := withTempConfig(t)
	cfg, err := New("laptop", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != filepath.Base(path) {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Fatalf("after a save the directory holds %v, want just the config", names)
	}
}

// `henri leave` says there is no way back into the group from this device once
// the config is gone. A temp file holding the same group key made that false.
func TestRemoveTakesTheTemporaryCopiesToo(t *testing.T) {
	path := withTempConfig(t)
	cfg, err := New("laptop", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(path)
	strays := []string{
		path + ".tmp",                        // what older henris left behind
		filepath.Join(dir, ".config-1.json"), // what an interrupted save leaves now
	}
	for _, s := range strays {
		if err := os.WriteFile(s, []byte(cfg.Key), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := Remove(); err != nil {
		t.Fatal(err)
	}
	for _, s := range strays {
		if _, err := os.Stat(s); !os.IsNotExist(err) {
			t.Fatalf("%s survived `henri leave`, and it holds the group key", s)
		}
	}
}

// Anyone who can write the config can put their own group in it and leave the
// user's real words in place. The daemon would then encrypt everything to that
// group while `henri code` went on printing the familiar phrase.
func TestValidateRejectsAKeyThatDoesNotMatchThePhrase(t *testing.T) {
	withTempConfig(t)
	mine, err := New("laptop", 0)
	if err != nil {
		t.Fatal(err)
	}
	theirs, err := New("attacker", 0)
	if err != nil {
		t.Fatal(err)
	}

	swappedKey := *mine
	swappedKey.Key = theirs.Key
	if err := swappedKey.Validate(); err == nil {
		t.Fatal("a config whose key does not derive from its phrase was accepted")
	}

	swappedGroup := *mine
	swappedGroup.GroupID = theirs.GroupID
	if err := swappedGroup.Validate(); err == nil {
		t.Fatal("a config whose group_id does not derive from its phrase was accepted")
	}

	swappedPhrase := *mine
	swappedPhrase.Phrase = theirs.Phrase
	if err := swappedPhrase.Validate(); err == nil {
		t.Fatal("a config carrying somebody else's phrase was accepted")
	}

	if err := mine.Validate(); err != nil {
		t.Fatalf("an untampered config was rejected: %v", err)
	}
}

// Configs made before recovery phrases existed have no phrase, which is also
// what the legacy `henri1:` join code produces. Those groups still work.
func TestValidateAllowsAConfigWithNoPhrase(t *testing.T) {
	withTempConfig(t)
	cfg, err := New("laptop", 0)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Phrase = ""
	if err := cfg.Validate(); err != nil {
		t.Fatalf("a legacy config without a phrase was rejected: %v", err)
	}
	if err := cfg.Save(); err != nil {
		t.Fatalf("a legacy config without a phrase could not be saved: %v", err)
	}
}

// `henri init -port 99999` used to report success and write a config nothing
// could use; the failure surfaced much later as "invalid port", from commands
// that had never mentioned a port.
func TestValidateRejectsPortsThatAreNotPorts(t *testing.T) {
	withTempConfig(t)
	for _, tc := range []struct {
		what          string
		listen, disco int
	}{
		{"listen too high", 99999, DefaultDiscoveryPort},
		{"listen zero", 0, DefaultDiscoveryPort},
		{"listen negative", -1, DefaultDiscoveryPort},
		{"discovery too high", DefaultListenPort, 70000},
		{"discovery zero", DefaultListenPort, 0},
		{"both the same", 47600, 47600},
	} {
		cfg, err := New("laptop", 0)
		if err != nil {
			t.Fatal(err)
		}
		cfg.ListenPort, cfg.DiscoveryPort = tc.listen, tc.disco
		if err := cfg.Validate(); err == nil {
			t.Errorf("%s: accepted listen=%d discovery=%d", tc.what, tc.listen, tc.disco)
		}
		if err := cfg.Save(); err == nil {
			t.Errorf("%s: Save wrote listen=%d discovery=%d to disk", tc.what, tc.listen, tc.disco)
		}
	}
}

// applyDefaults repairs these on the Load path only, so a Config built in code
// and handed to the node could still carry a zero: time.NewTicker(0) panics,
// and a zero payload limit refuses every copy without saying anything.
func TestValidateRejectsAZeroPollIntervalOrPayloadLimit(t *testing.T) {
	withTempConfig(t)
	cfg, err := New("laptop", 0)
	if err != nil {
		t.Fatal(err)
	}
	zeroPoll := *cfg
	zeroPoll.PollMillis = 0
	if err := zeroPoll.Validate(); err == nil {
		t.Error("a zero poll interval was accepted")
	}
	zeroMax := *cfg
	zeroMax.MaxBytes = 0
	if err := zeroMax.Validate(); err == nil {
		t.Error("a zero payload limit was accepted")
	}
	negative := *cfg
	negative.PollMillis = -5
	if err := negative.Validate(); err == nil {
		t.Error("a negative poll interval was accepted")
	}
}

// The documented shape of a fresh config has "peers": [], not "peers": null.
func TestAFreshConfigWritesAnEmptyPeerList(t *testing.T) {
	path := withTempConfig(t)
	cfg, err := New("laptop", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), `"peers": null`) {
		t.Fatalf("a fresh config says peers is null:\n%s", raw)
	}
	if !strings.Contains(string(raw), `"peers": []`) {
		t.Fatalf("a fresh config does not list peers as an empty array:\n%s", raw)
	}
}

// A config that turns discovery off has no use for a discovery port, and
// saying nothing about a socket that will never be bound is not an error. The
// node builds configs exactly like this.
func TestDiscoveryPortIsOnlyRequiredWhenDiscoveryIsOn(t *testing.T) {
	withTempConfig(t)
	cfg, err := New("laptop", 0)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Discovery = false
	cfg.DiscoveryPort = 0
	if err := cfg.Validate(); err != nil {
		t.Fatalf("discovery is off, so a missing discovery port is fine: %v", err)
	}
	cfg.Discovery = true
	if err := cfg.Validate(); err == nil {
		t.Fatal("discovery is on with no port to do it on, and that was accepted")
	}
}
