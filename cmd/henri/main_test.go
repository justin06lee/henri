package main

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/justin06lee/henri/internal/config"
	"github.com/justin06lee/henri/internal/node"
)

// isolate points henri at a config under the test's own temporary directory.
//
// Every test that calls a command function has to do this. The alternative is a
// test run that reads, rewrites or removes the real ~/.config/henri/config.json
// and the group key in it.
func isolate(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	t.Setenv("HENRI_CONFIG", path)
	return path
}

// captureStdout collects what fn writes to stdout.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	saved := os.Stdout
	os.Stdout = w
	done := make(chan string, 1)
	go func() {
		var b strings.Builder
		_, _ = io.Copy(&b, r)
		done <- b.String()
	}()
	fn()
	os.Stdout = saved
	_ = w.Close()
	out := <-done
	_ = r.Close()
	return out
}

// --- the recovery phrase must not fall out of a request for help -----------

// `henri code` took no arguments at all, so the dispatcher never gave it a
// chance to answer -h: asking for help printed all fifteen words and the whole
// `henri join` line to stdout and exited 0. That phrase is the group key, and
// help is what people ask for in shared terminals, screen recordings and CI.
func TestCodeHelpDoesNotPrintTheRecoveryPhrase(t *testing.T) {
	isolate(t)
	cfg, err := config.New("laptop", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}

	for _, arg := range []string{"-h", "-help", "--help"} {
		var cmdErr error
		out := captureStdout(t, func() { cmdErr = cmdCode([]string{arg}) })
		if cmdErr != nil {
			t.Fatalf("henri code %s: %v", arg, cmdErr)
		}
		if strings.Contains(out, cfg.Phrase) {
			t.Fatalf("henri code %s printed the recovery phrase", arg)
		}
		if strings.Contains(out, "  1. ") {
			t.Fatalf("henri code %s printed the numbered phrase block", arg)
		}
		if !strings.Contains(out, "usage:") {
			t.Fatalf("henri code %s printed no usage:\n%s", arg, out)
		}
	}

	// And the command still does its job when it is asked to.
	var cmdErr error
	out := captureStdout(t, func() { cmdErr = cmdCode(nil) })
	if cmdErr != nil {
		t.Fatal(cmdErr)
	}
	if !strings.Contains(out, "  1. ") {
		t.Fatalf("henri code printed no numbered phrase:\n%s", out)
	}
	for _, w := range strings.Fields(cfg.Phrase) {
		if !strings.Contains(out, w) {
			t.Fatalf("henri code left the word %d out of the phrase", len(w))
		}
	}
}

func TestCodeRefusesStrayArguments(t *testing.T) {
	isolate(t)
	err := cmdCode([]string{"please"})
	if err == nil {
		t.Fatal("henri code accepted an argument")
	}
	if !strings.Contains(err.Error(), "takes no arguments") {
		t.Fatalf("unhelpful error: %v", err)
	}
}

// `henri status` has to reject its arguments before it loads a config or opens
// a socket, which is also what makes this safe to test.
func TestStatusRefusesStrayArguments(t *testing.T) {
	isolate(t)
	for _, args := range [][]string{{"now"}, {"--verbose"}} {
		if err := cmdStatus(args); err == nil {
			t.Fatalf("henri status %v was accepted", args)
		}
	}
}

// --- service, hotkey and peers arguments -----------------------------------

// Only `service install` ever built a FlagSet. `henri service uninstall --nope`
// went straight past the flag it did not understand and removed a real launchd
// agent, reporting only that it had done so.
func TestServiceSubcommandsRejectTheirArguments(t *testing.T) {
	for _, args := range [][]string{
		{"uninstall", "--nope"},
		{"uninstall", "extra"},
		{"restart", "-now"},
		{"restart", "please"},
		{"status", "-v"},
		{"logs", "-f"},
		{"install", "-bogus"},
		{"install", "somewhere"},
		{"wibble"},
	} {
		sub, _, done, err := parseServiceArgs(args)
		if err == nil {
			t.Errorf("henri service %s was accepted as %q (done=%v)", strings.Join(args, " "), sub, done)
		}
	}
}

func TestServiceSubcommandsAcceptWhatTheyShould(t *testing.T) {
	for _, tc := range []struct {
		args  []string
		sub   string
		force bool
	}{
		{[]string{"install"}, "install", false},
		{[]string{"install", "-force"}, "install", true},
		{[]string{"enable"}, "install", false},
		{[]string{"uninstall"}, "uninstall", false},
		{[]string{"remove"}, "uninstall", false},
		{[]string{"disable"}, "uninstall", false},
		{[]string{"restart"}, "restart", false},
		{[]string{"status"}, "status", false},
		{[]string{"logs"}, "logs", false},
	} {
		sub, force, done, err := parseServiceArgs(tc.args)
		if err != nil || done {
			t.Errorf("henri service %s: %v (done=%v)", strings.Join(tc.args, " "), err, done)
			continue
		}
		if sub != tc.sub || force != tc.force {
			t.Errorf("henri service %s gave (%q, %v), want (%q, %v)",
				strings.Join(tc.args, " "), sub, force, tc.sub, tc.force)
		}
	}
}

// -h is a question, not a mistake, and it must not reach the service manager.
func TestServiceHelpIsNotAnError(t *testing.T) {
	for _, args := range [][]string{{"install", "-h"}, {"uninstall", "-h"}, {"logs", "-h"}} {
		var sub string
		var done bool
		var err error
		out := captureStdout(t, func() { sub, _, done, err = parseServiceArgs(args) })
		if err != nil {
			t.Errorf("henri service %s: %v", strings.Join(args, " "), err)
		}
		if !done {
			t.Errorf("henri service %s did not stop after printing help (sub=%q)", strings.Join(args, " "), sub)
		}
		if !strings.Contains(out, "usage:") {
			t.Errorf("henri service %s printed no usage:\n%s", strings.Join(args, " "), out)
		}
	}
}

func TestHotkeyArgumentsAreChecked(t *testing.T) {
	for _, args := range [][]string{
		{"uninstall", "--nope"},
		{"install", "extra"},
		{"status", "-accel"},
		{"wibble"},
	} {
		if _, _, _, err := parseHotkeyArgs(args); err == nil {
			t.Errorf("henri hotkey %s was accepted", strings.Join(args, " "))
		}
	}

	for _, tc := range []struct {
		args  []string
		sub   string
		accel string
	}{
		{nil, "status", "<Super><Shift>c"},
		{[]string{"install"}, "install", "<Super><Shift>c"},
		{[]string{"install", "-accel", "<Super><Alt>c"}, "install", "<Super><Alt>c"},
		{[]string{"uninstall"}, "uninstall", "<Super><Shift>c"},
		{[]string{"-accel", "<Ctrl>x"}, "status", "<Ctrl>x"},
	} {
		sub, accel, done, err := parseHotkeyArgs(tc.args)
		if err != nil || done {
			t.Errorf("henri hotkey %s: %v (done=%v)", strings.Join(tc.args, " "), err, done)
			continue
		}
		if sub != tc.sub || accel != tc.accel {
			t.Errorf("henri hotkey %s gave (%q, %q), want (%q, %q)",
				strings.Join(tc.args, " "), sub, accel, tc.sub, tc.accel)
		}
	}
}

func TestPeersArgumentsAreChecked(t *testing.T) {
	for _, args := range [][]string{
		{"add"},
		{"add", "a", "b"},
		{"rm"},
		{"add", "-force", "1.2.3.4"},
		{"wibble"},
		{"-bogus"},
	} {
		if _, _, _, err := parsePeersArgs(args); err == nil {
			t.Errorf("henri peers %s was accepted", strings.Join(args, " "))
		}
	}

	for _, tc := range []struct {
		args      []string
		sub, addr string
	}{
		{nil, "", ""},
		{[]string{"add", "10.0.0.5"}, "add", "10.0.0.5"},
		{[]string{"rm", "10.0.0.5:47600"}, "rm", "10.0.0.5:47600"},
		{[]string{"remove", "10.0.0.5"}, "rm", "10.0.0.5"},
	} {
		sub, addr, done, err := parsePeersArgs(tc.args)
		if err != nil || done {
			t.Errorf("henri peers %s: %v (done=%v)", strings.Join(tc.args, " "), err, done)
			continue
		}
		if sub != tc.sub || addr != tc.addr {
			t.Errorf("henri peers %s gave (%q, %q), want (%q, %q)",
				strings.Join(tc.args, " "), sub, addr, tc.sub, tc.addr)
		}
	}
}

// --- a flag written after the words ----------------------------------------

// Go's flag package stops at the first non-flag argument, and mnemonic.Split
// throws away everything that is not a letter. So `henri join <14 words>
// -force` became fifteen tokens, every one of them a real BIP-39 word, and the
// user was told their phrase did not check out.
func TestJoinRejectsAFlagWrittenAfterTheWords(t *testing.T) {
	isolate(t)
	words := strings.Fields("legal winner thank year wave sausage worth useful legal winner thank yellow")
	for _, trailing := range []string{"-force", "-name", "--discovery=false"} {
		args := append(append([]string{}, words...), trailing)
		err := cmdJoin(args)
		if err == nil {
			t.Fatalf("henri join <words> %s was accepted", trailing)
		}
		if !strings.Contains(err.Error(), "Flags go first") {
			t.Fatalf("henri join <words> %s blamed the phrase instead of the flag: %v", trailing, err)
		}
	}

	// A flag in front of the words is the correct way round and must not be
	// caught by this.
	fine := append([]string{"-name", "laptop"}, words...)
	var joinErr error
	captureStdout(t, func() { joinErr = cmdJoin(fine) })
	if joinErr != nil {
		t.Fatalf("a flag before the words was rejected: %v", joinErr)
	}
}

// --- addresses --------------------------------------------------------------

func TestNormalizeAddr(t *testing.T) {
	const defaultPort = 47600
	for _, tc := range []struct {
		in, want string
	}{
		{"10.0.0.5", "10.0.0.5:47600"},
		{" 10.0.0.5 ", "10.0.0.5:47600"},
		{"10.0.0.5:47700", "10.0.0.5:47700"},
		{"desktop.local", "desktop.local:47600"},
		{"desktop.local:1", "desktop.local:1"},
		{"desktop.local:65535", "desktop.local:65535"},
		// IPv6, which is nothing but colons and used to be stored verbatim and
		// then rejected by every later net.SplitHostPort.
		{"::1", "[::1]:47600"},
		{"[::1]", "[::1]:47600"},
		{"[::1]:47700", "[::1]:47700"},
		{"fe80::1", "[fe80::1]:47600"},
		{"[fe80::1%eth0]:47600", "[fe80::1%eth0]:47600"},
		{"2001:db8::42", "[2001:db8::42]:47600"},
	} {
		got, err := normalizeAddr(tc.in, defaultPort)
		if err != nil {
			t.Errorf("normalizeAddr(%q): %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("normalizeAddr(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}

	for _, bad := range []string{
		"",
		"   ",
		":",
		":47600",
		"host:",
		"host:abc",
		"host:-1",
		"host:0",
		"host:65536",
		"host:99999",
		"host:47600:extra",
	} {
		if got, err := normalizeAddr(bad, defaultPort); err == nil {
			t.Errorf("normalizeAddr(%q) was accepted as %q", bad, got)
		}
	}
}

// Whatever normalizeAddr returns has to be something the daemon can dial, which
// is the guarantee that was missing: a peer that fails SplitHostPort later on
// is listed as normal and silently never syncs.
func TestNormalizedAddressesCanBeSplitAgain(t *testing.T) {
	for _, in := range []string{"10.0.0.5", "::1", "[fe80::1]:47700", "desktop.local"} {
		got, err := normalizeAddr(in, 47600)
		if err != nil {
			t.Fatalf("normalizeAddr(%q): %v", in, err)
		}
		if _, _, err := net.SplitHostPort(got); err != nil {
			t.Fatalf("normalizeAddr(%q) = %q, which cannot be split again: %v", in, got, err)
		}
	}
}

// --- printing ----------------------------------------------------------------

func TestHumanBytes(t *testing.T) {
	for _, tc := range []struct {
		in   int
		want string
	}{
		{0, "0 B"},
		{1, "1 B"},
		{1023, "1023 B"},
		{1024, "1.0 KiB"},
		{1536, "1.5 KiB"},
		{4 << 20, "4.0 MiB"},
		{1 << 30, "1.0 GiB"},
		{1 << 40, "1.0 TiB"},
		// The old unit table ran out here and indexed past the end of a string.
		{1 << 50, "1.0 PiB"},
		{1 << 60, "1.0 EiB"},
	} {
		if got := humanBytes(tc.in); got != tc.want {
			t.Errorf("humanBytes(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestSince(t *testing.T) {
	now := time.Now()
	ms := func(d time.Duration) int64 { return now.Add(-d).UnixMilli() }
	for _, tc := range []struct {
		in   int64
		want string
	}{
		// A missing timestamp is "this never happened", not the age of the
		// epoch, which is what 496247h16m was.
		{0, "unknown"},
		{-1, "unknown"},
		{ms(0), "0s"},
		{ms(3 * time.Second), "3s"},
		{ms(90 * time.Second), "1m30s"},
		{ms(2*time.Hour + 5*time.Minute), "2h5m"},
		{ms(23 * time.Hour), "23h0m"},
		{ms(25 * time.Hour), "1d1h"},
		{ms(9*24*time.Hour + 3*time.Hour), "9d3h"},
	} {
		if got := since(tc.in); got != tc.want {
			t.Errorf("since(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
	// A timestamp from the future, which a peer with a fast clock can produce,
	// must not come back negative.
	if got := since(now.Add(time.Hour).UnixMilli()); got != "0s" {
		t.Errorf("since(a future timestamp) = %q, want 0s", got)
	}
}

// The phrase is something people copy, paste and put in a password manager.
// Padding the last column put invisible trailing spaces on every line of it.
func TestFormatPhraseHasNoTrailingWhitespace(t *testing.T) {
	for _, phrase := range []string{
		"legal winner thank year wave sausage worth useful legal winner thank yellow",
		"glue report social awake strike piano arm awesome wage eternal bean smile real release couple",
		"one two three four five",
		"solo",
	} {
		out := formatPhrase(phrase)
		if !strings.HasSuffix(out, "\n") {
			t.Errorf("formatPhrase(%q) does not end in a newline", phrase)
		}
		for i, line := range strings.Split(strings.TrimSuffix(out, "\n"), "\n") {
			if line != strings.TrimRight(line, " \t") {
				t.Errorf("formatPhrase(%q) line %d ends in whitespace: %q", phrase, i+1, line)
			}
		}
	}
}

func TestFormatPhraseNumbersFourToARow(t *testing.T) {
	out := formatPhrase("legal winner thank year wave sausage worth useful legal winner thank yellow")
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("twelve words came out as %d rows:\n%s", len(lines), out)
	}
	if !strings.HasPrefix(lines[0], "    1. legal") {
		t.Fatalf("first row is %q", lines[0])
	}
	if !strings.Contains(lines[2], "12. yellow") {
		t.Fatalf("last row is %q", lines[2])
	}
}

// A device name is chosen on another machine by somebody else. Written to a
// terminal unfiltered it is not text: it can repaint the line and decide what
// every other member's `henri status` appears to say.
func TestSafeStripsControlSequences(t *testing.T) {
	for _, tc := range []struct {
		in, want string
	}{
		{"laptop", "laptop"},
		{"  laptop  ", "laptop"},
		{"lap\x1b[2Ktop", "lap[2Ktop"},
		{"lap\x1b]0;pwned\x07top", "lap]0;pwnedtop"},
		{"lap\rtop", "laptop"},
		{"lap\ntop", "laptop"},
		{"lap\ttop", "laptop"},
		{"lap\u009btop", "laptop"}, // a C1 control on its own
		{"lap\u200etop", "laptop"}, // an invisible bidi mark
		{"desktop 2", "desktop 2"}, // ordinary spaces survive
	} {
		if got := safe(tc.in, 64); got != tc.want {
			t.Errorf("safe(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	if got := safe(strings.Repeat("a", 100), 8); got != "aaaaaaaa…" {
		t.Errorf("safe did not bound the length: %q", got)
	}
	if strings.ContainsAny(safe("\x1b[31mred\x1b[0m", 64), "\x1b") {
		t.Error("an escape survived")
	}
}

// One layout for a device, whether the daemon is answering or the list came out
// of the config.
func TestPeerLineIsTheSameEitherWay(t *testing.T) {
	running := peerLine("●", "desktop", "192.168.1.42:47600", "discovered", "3s ago")
	stopped := peerLine("○", "—", "10.8.0.4:47600", "config", "never")
	for _, line := range []string{running, stopped} {
		if !strings.HasPrefix(line, "  ") {
			t.Errorf("a peer row is not indented like every other row: %q", line)
		}
	}
	// Compared in characters, not bytes: the em dash is three bytes wide and
	// one column wide, and it is the column that has to line up.
	col := func(line, needle string) int { return len([]rune(line[:strings.Index(line, needle)])) }
	if a, b := col(running, "192.168.1.42"), col(stopped, "10.8.0.4"); a != b {
		t.Errorf("the address column is at %d and %d:\n%q\n%q", a, b, running, stopped)
	}
}

// --- discovery health --------------------------------------------------------

// The display that would have said the daemon had gone deaf, instead of
// printing "discovery on" beside an empty peer list for seventeen minutes.
func TestDiscoveryLine(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name string
		st   node.State
		want string
	}{
		{"off", node.State{Discovery: false, Beacons: 9}, "off"},
		{"nothing heard yet", node.State{Discovery: true}, "on · 0 beacons · none heard yet"},
		{"healthy", node.State{
			Discovery:    true,
			Beacons:      412,
			LastBeaconAt: now.Add(-4 * time.Second).UnixMilli(),
		}, "on · 412 beacons · last 4s ago"},
	} {
		st := tc.st
		if got := discoveryLine(&st); got != tc.want {
			t.Errorf("%s: discoveryLine = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestDiscoveryWarning(t *testing.T) {
	now := time.Now()
	fresh := node.State{
		Discovery:    true,
		StartedAt:    now.Add(-time.Hour).UnixMilli(),
		Beacons:      12,
		LastBeaconAt: now.Add(-8 * time.Second).UnixMilli(),
	}
	if w := discoveryWarning(&fresh); w != "" {
		t.Errorf("a healthy daemon was warned about:\n%s", w)
	}

	off := fresh
	off.Discovery = false
	off.LastBeaconAt = 0
	off.StartedAt = now.Add(-time.Hour).UnixMilli()
	if w := discoveryWarning(&off); w != "" {
		t.Errorf("discovery is off, so there is nothing to warn about:\n%s", w)
	}

	young := node.State{Discovery: true, StartedAt: now.Add(-5 * time.Second).UnixMilli()}
	if w := discoveryWarning(&young); w != "" {
		t.Errorf("a daemon five seconds old has not had a chance yet:\n%s", w)
	}

	deaf := fresh
	deaf.LastBeaconAt = now.Add(-17 * time.Minute).UnixMilli()
	w := discoveryWarning(&deaf)
	if w == "" {
		t.Fatal("seventeen minutes of silence went unmentioned")
	}
	for _, want := range []string{"17m", "henri peers add"} {
		if !strings.Contains(w, want) {
			t.Errorf("the warning does not mention %q:\n%s", want, w)
		}
	}

	// Never a beacon at all, on a daemon that has been up long enough to
	// expect one.
	silent := node.State{Discovery: true, StartedAt: now.Add(-30 * time.Minute).UnixMilli()}
	if discoveryWarning(&silent) == "" {
		t.Error("half an hour with no beacon at all went unmentioned")
	}
}

// --- flag plumbing ----------------------------------------------------------

// Asking for help is not a failure. `henri init -h` printed the help and then
// "henri: flag: help requested", and exited 1.
func TestHelpIsNotAnError(t *testing.T) {
	isolate(t)
	for _, tc := range []struct {
		name string
		fn   func([]string) error
	}{
		{"init", cmdInit},
		{"join", cmdJoin},
		{"daemon", cmdDaemon},
		{"send", cmdSend},
		{"leave", cmdLeave},
		{"peers", cmdPeers},
		{"status", cmdStatus},
		{"code", cmdCode},
	} {
		var err error
		out := captureStdout(t, func() { err = tc.fn([]string{"-h"}) })
		if err != nil {
			t.Errorf("henri %s -h returned %v", tc.name, err)
		}
		if !strings.Contains(out, "usage: henri "+tc.name) {
			t.Errorf("henri %s -h printed no usage line:\n%s", tc.name, out)
		}
	}
}

// A mistyped flag is a failure, and it should be reported once.
func TestAnUnknownFlagIsAnError(t *testing.T) {
	isolate(t)
	for _, tc := range []struct {
		name string
		fn   func([]string) error
	}{
		{"init", cmdInit},
		{"daemon", cmdDaemon},
		{"send", cmdSend},
	} {
		if err := tc.fn([]string{"-bogus"}); err == nil {
			t.Errorf("henri %s -bogus was accepted", tc.name)
		}
	}
}

// --- ports -------------------------------------------------------------------

// `henri init -port 99999` reported success and wrote a config nothing could
// load. It has to be caught before any of that happens.
func TestInitRejectsAPortThatIsNotAPort(t *testing.T) {
	path := isolate(t)
	for _, port := range []string{"99999", "0", "-1", "65536"} {
		if err := cmdInit([]string{"-port", port}); err == nil {
			t.Errorf("henri init -port %s was accepted", port)
		}
		if _, err := os.Stat(path); err == nil {
			t.Fatalf("henri init -port %s wrote a config anyway", port)
		}
	}
	if err := checkPortFlag(47600); err != nil {
		t.Errorf("a real port was rejected: %v", err)
	}
}

// A legacy join code carries the port its group was created on. It was parsed
// and thrown away, so those groups rejoined on 47600 and never synced.
func TestLegacyJoinCodeKeepsItsPort(t *testing.T) {
	cfg, err := joinLegacyCode(legacyCodeFor(t, 47700), "laptop")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ListenPort != 47700 {
		t.Fatalf("the join code named port 47700 and the config says %d", cfg.ListenPort)
	}
	// A code from before the field existed still gets the default.
	cfg, err = joinLegacyCode(legacyCodeFor(t, 0), "laptop")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ListenPort != config.DefaultListenPort {
		t.Fatalf("a code with no port gave %d, want %d", cfg.ListenPort, config.DefaultListenPort)
	}
	if _, err := joinLegacyCode(legacyCodeFor(t, 99999), "laptop"); err == nil {
		t.Fatal("a join code naming port 99999 was accepted")
	}
}

// legacyCodeFor builds one of the base64 join codes henri handed out before
// recovery phrases, so the path that still accepts them stays tested.
func legacyCodeFor(t *testing.T, port int) string {
	t.Helper()
	body := struct {
		G string `json:"g"`
		K string `json:"k"`
		P int    `json:"p,omitempty"`
	}{
		G: "testgroup",
		K: base64.StdEncoding.EncodeToString(make([]byte, 32)),
		P: port,
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	return legacyCodePrefix + base64.RawURLEncoding.EncodeToString(raw)
}

func TestFormatNote(t *testing.T) {
	cases := []struct {
		format string
		count  int
		want   string
	}{
		{"", 0, ""},
		{"files", 1, " (1 file)"},
		{"files", 3, " (3 files)"},
		{"image/png", 0, " (image)"},
	}
	for _, tc := range cases {
		if got := formatNote(tc.format, tc.count); got != tc.want {
			t.Errorf("formatNote(%q, %d) = %q, want %q", tc.format, tc.count, got, tc.want)
		}
	}
}
