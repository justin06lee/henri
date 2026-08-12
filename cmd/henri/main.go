// Command henri keeps the clipboard in sync across your devices.
package main

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode"

	"github.com/justin06lee/henri/internal/clipboard"
	"github.com/justin06lee/henri/internal/config"
	"github.com/justin06lee/henri/internal/firewall"
	"github.com/justin06lee/henri/internal/hotkey"
	"github.com/justin06lee/henri/internal/mnemonic"
	"github.com/justin06lee/henri/internal/node"
	"github.com/justin06lee/henri/internal/service"
)

// version is overridden at build time with -ldflags "-X main.version=...".
var version = "0.9.0"

// errSaidItAlready means the command has already told the user everything there
// is to say and only the exit status is left to report.
//
// `henri status` with the daemon stopped is the display working exactly as
// intended, and it still has to fail: `until henri status; do sleep 1; done` is
// how people wait for the daemon to come up, and a loop like that could never
// finish while a stopped daemon exited 0.
var errSaidItAlready = errors.New("henri: nothing further to report")

func main() {
	if err := run(os.Args[1:]); err != nil {
		if !errors.Is(err, errSaidItAlready) {
			fmt.Fprintln(os.Stderr, "henri: "+err.Error())
		}
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		usage()
		return nil
	}
	switch args[0] {
	case "init":
		return cmdInit(args[1:])
	case "join":
		return cmdJoin(args[1:])
	case "code":
		return cmdCode(args[1:])
	case "daemon":
		return cmdDaemon(args[1:])
	case "status":
		return cmdStatus(args[1:])
	case "doctor":
		return cmdDoctor(args[1:])
	case "peers":
		return cmdPeers(args[1:])
	case "send":
		return cmdSend(args[1:])
	case "leave":
		return cmdLeave(args[1:])
	case "service":
		return cmdService(args[1:])
	case "hotkey":
		return cmdHotkey(args[1:])
	case "version", "--version", "-v":
		fmt.Println("henri " + version)
		return nil
	case "help", "--help", "-h":
		usage()
		return nil
	default:
		usage()
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func usage() {
	fmt.Print(`henri - a shared clipboard for all your devices

usage: henri <command> [flags]

  init            start a new clipboard group on this device
  join            join an existing group; asks for the recovery phrase
  code            print this group's recovery phrase
  daemon          run the sync daemon in the foreground
  service         run henri in the background, starting at login
  hotkey          bind a key to ` + "`henri send -highlighted`" + `, for desktops henri cannot watch
  status          show what the local daemon is doing
  doctor          check everything sync needs and say what is wrong (-fix to repair)
  peers           list known devices; ` + "`peers add|rm <host:port>`" + ` to edit
  send            send the clipboard to the group (-highlighted for the selection)
  leave           remove this device's config and leave the group
  version         print the version

Run 'henri <command> -h' for the flags a command accepts.
`)
}

// --- flag plumbing ---------------------------------------------------------

// parseFlags parses one command's flags. done means the command has nothing
// left to do -- the user asked for help and got it.
//
// It exists because flag.ContinueOnError makes two things awkward, and every
// call site used to get both wrong. Asking for help comes back as
// flag.ErrHelp, an error like any other, so `henri init -h` printed the help
// and then "henri: flag: help requested" and exited 1; asking for help is not a
// failure. And the FlagSet prints its own message before returning any error,
// so handing that error back printed it a second time. Here the FlagSet's own
// output is silenced, help goes to stdout where a pager can take it, and a real
// mistake is reported once.
func parseFlags(fs *flag.FlagSet, args []string, usageLine string) (done bool, err error) {
	fs.SetOutput(io.Discard)
	err = fs.Parse(args)
	switch {
	case errors.Is(err, flag.ErrHelp):
		fs.SetOutput(os.Stdout)
		fmt.Printf("usage: %s\n", usageLine)
		if hasFlags(fs) {
			fmt.Println()
			fs.PrintDefaults()
		}
		return true, nil
	case err != nil:
		fs.SetOutput(os.Stderr)
		fmt.Fprintf(os.Stderr, "usage: %s\n", usageLine)
		if hasFlags(fs) {
			fmt.Fprintln(os.Stderr)
			fs.PrintDefaults()
		}
		fmt.Fprintln(os.Stderr)
		return false, err
	}
	return false, nil
}

func hasFlags(fs *flag.FlagSet) bool {
	any := false
	fs.VisitAll(func(*flag.Flag) { any = true })
	return any
}

// noArgs rejects stray words after a command that takes none.
//
// Quietly ignoring them is how `henri service uninstall --nope` removed a real
// launchd agent and reported only "Removed the launchd service" -- the flag it
// did not understand was never mentioned, and neither was the fact that it had
// gone ahead anyway.
func noArgs(fs *flag.FlagSet) error {
	if fs.NArg() == 0 {
		return nil
	}
	return fmt.Errorf("`henri %s` takes no arguments, but got %q; "+
		"run `henri %s -h` for the flags it does take", fs.Name(), fs.Arg(0), fs.Name())
}

// bareCommand parses a subcommand that takes neither flags nor arguments, so
// that anything typed after it is reported rather than acted through.
func bareCommand(name string, args []string) (done bool, err error) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	if done, err := parseFlags(fs, args, "henri "+name); done || err != nil {
		return done, err
	}
	return false, noArgs(fs)
}

// flagWasGiven reports whether a flag was actually typed, as opposed to sitting
// at its default value.
func flagWasGiven(fs *flag.FlagSet, name string) bool {
	given := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			given = true
		}
	})
	return given
}

// checkPortFlag rejects a port before anything is done with it. `henri init
// -port 99999` used to report success and write a config nothing could load;
// the failure surfaced much later as "listen tcp: address 99999: invalid port",
// out of commands that had never mentioned a port at all.
func checkPortFlag(port int) error {
	if port < 1 || port > 65535 {
		return fmt.Errorf("-port must be between 1 and 65535, not %d", port)
	}
	return nil
}

// --- init ------------------------------------------------------------------

func cmdInit(args []string) error {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	// The default is resolved after parsing, not here: os.Hostname() at
	// flag-definition time runs on every `henri init`, including the ones that
	// pass -name and never look at it.
	name := fs.String("name", "", "name for this device (default: this machine's hostname)")
	port := fs.Int("port", config.DefaultListenPort, "TCP port to receive clipboard updates on")
	discovery := fs.Bool("discovery", true, "find other devices on the LAN automatically")
	words := fs.Int("words", 15, "length of the recovery phrase (12, 15, 18, 21 or 24)")
	force := fs.Bool("force", false, "overwrite an existing config")
	if done, err := parseFlags(fs, args, "henri init [flags]"); done || err != nil {
		return err
	}
	if err := noArgs(fs); err != nil {
		return err
	}
	bits, ok := mnemonic.EntropyBitsFor(*words)
	if !ok {
		return fmt.Errorf("-words must be 12, 15, 18, 21 or 24, not %d — a phrase length is "+
			"always a multiple of three, because each word carries 11 bits and the checksum "+
			"is a 32nd of the entropy", *words)
	}
	if err := checkPortFlag(*port); err != nil {
		return err
	}

	path, err := config.Path()
	if err != nil {
		return err
	}
	if _, err := os.Stat(path); err == nil && !*force {
		return fmt.Errorf("a config already exists at %s (use -force to replace it, which leaves this device's current group)", path)
	}

	cfg, err := config.New(deviceName(*name), bits)
	if err != nil {
		return err
	}
	cfg.ListenPort = *port
	cfg.Discovery = *discovery
	if err := cfg.Save(); err != nil {
		return err
	}

	fmt.Printf("Started a new clipboard group.\n\n")
	fmt.Printf("  config   %s\n", path)
	fmt.Printf("  device   %s\n", cfg.DeviceName)
	fmt.Printf("  group    %s\n\n", cfg.GroupID)
	fmt.Printf("Your recovery phrase — these %d words are the whole secret:\n\n", *words)
	fmt.Print(formatPhrase(cfg.Phrase))
	fmt.Printf("\nOn every other device, run `henri join` and type those words in when\n")
	fmt.Printf("it asks for them.\n\n")
	fmt.Printf("Anyone who has those words can read everything you copy. Read them out\n")
	fmt.Printf("loud or type them in by hand — don't paste them through anything you\n")
	fmt.Printf("don't control. `henri join <words>` still works, but a command line is\n")
	fmt.Printf("readable by every other user on the machine and your shell writes it\n")
	fmt.Printf("into its history file, so it is the worse way to move a group key.\n\n")
	fmt.Printf("Then start the daemon on each device:  henri daemon\n")
	if err := clipboard.Available(); err != nil {
		fmt.Printf("\nHeads up: %v\n", err)
	}
	return nil
}

// --- join ------------------------------------------------------------------

func cmdJoin(args []string) error {
	fs := flag.NewFlagSet("join", flag.ContinueOnError)
	name := fs.String("name", "", "name for this device (default: this machine's hostname)")
	port := fs.Int("port", config.DefaultListenPort, "TCP port to receive clipboard updates on")
	discovery := fs.Bool("discovery", true, "find other devices on the LAN automatically")
	force := fs.Bool("force", false, "overwrite an existing config")
	if done, err := parseFlags(fs, args, "henri join [flags] [<word> <word> ...]"); done || err != nil {
		return err
	}
	if err := checkFlagsCameFirst(fs); err != nil {
		return err
	}
	if err := checkPortFlag(*port); err != nil {
		return err
	}

	path, err := config.Path()
	if err != nil {
		return err
	}
	if _, err := os.Stat(path); err == nil && !*force {
		return fmt.Errorf("a config already exists at %s (use -force to replace it)", path)
	}

	// The words can arrive as separate arguments or as one quoted string, and
	// older releases handed out a base64 join code instead. With none of them
	// on the command line they are asked for, which is the form worth using.
	input := strings.Join(fs.Args(), " ")
	if fs.NArg() == 0 {
		if input, err = readPhrase(); err != nil {
			return err
		}
	}

	var cfg *config.Config
	if strings.HasPrefix(strings.TrimSpace(input), legacyCodePrefix) {
		cfg, err = joinLegacyCode(input, deviceName(*name))
	} else {
		cfg, err = config.Join(input, deviceName(*name))
	}
	if err != nil {
		return err
	}

	// -port only wins when it was actually typed. A legacy join code carries
	// the port the group was created on, and overwriting that with the flag's
	// default silently rejoined those groups on 47600 instead.
	if flagWasGiven(fs, "port") {
		cfg.ListenPort = *port
	}
	if flagWasGiven(fs, "discovery") {
		cfg.Discovery = *discovery
	}
	if err := cfg.Validate(); err != nil {
		return err
	}
	if err := cfg.Save(); err != nil {
		return err
	}

	fmt.Printf("Joined group %s as %q.\n\n", cfg.GroupID, cfg.DeviceName)
	fmt.Printf("  config  %s\n\n", path)
	fmt.Printf("Start the daemon:  henri daemon\n")
	if err := clipboard.Available(); err != nil {
		fmt.Printf("\nHeads up: %v\n", err)
	}
	return nil
}

// checkFlagsCameFirst rejects a flag written after the words.
//
// Go's flag package stops at the first argument that is not a flag, and
// mnemonic.Split throws away everything that is not a letter -- so `henri join
// <14 words> -force` became fifteen tokens, every one of them a real BIP-39
// word ("force" and "name" both are), and the user was told their phrase did
// not check out and that one of the words was probably in the wrong place.
// Their phrase was perfect.
func checkFlagsCameFirst(fs *flag.FlagSet) error {
	for _, a := range fs.Args() {
		if strings.HasPrefix(a, "-") {
			return fmt.Errorf("%q came after the words, so henri would have read it as one of them "+
				"rather than as a flag.\n"+
				"       Flags go first:  henri join -name laptop <words>", a)
		}
	}
	return nil
}

// readPhrase asks for the recovery phrase rather than taking it from the
// command line.
//
// Everything in argv is readable by every other user on the machine through
// `ps auxww`, and the shell writes it to a history file, so `henri join
// <words>` hands the group key to anyone who looks later. This is the form to
// use. The command-line one still works: it is in the README and in people's
// fingers, and it is the only place a phrase should still be accepted that way.
//
// The words are echoed as they are typed. Turning the echo off needs the
// terminal put into raw mode, which the standard library does not do, and it
// would be the wrong trade anyway: this is a phrase read off another screen and
// checked word by word on the way in.
func readPhrase() (string, error) {
	if isTerminal(os.Stdin) {
		fmt.Print("Recovery phrase (the words `henri init` printed): ")
		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil && strings.TrimSpace(line) == "" {
			fmt.Println()
			return "", errors.New("no recovery phrase given")
		}
		return strings.TrimSpace(line), nil
	}
	raw, err := io.ReadAll(os.Stdin)
	if err != nil {
		return "", err
	}
	phrase := strings.TrimSpace(string(raw))
	if phrase == "" {
		return "", errors.New("no recovery phrase on standard input.\n" +
			"       Run `henri join` in a terminal and type the words, or pipe them in:\n" +
			"       henri join < phrase.txt")
	}
	return phrase, nil
}

// isTerminal reports whether f is a terminal rather than a pipe or a file, so
// henri knows whether there is anyone there to prompt.
func isTerminal(f *os.File) bool {
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

func cmdCode(args []string) error {
	// A FlagSet even though there are no flags, because without one the
	// dispatcher never gave this command a chance to answer `-h`: `henri code
	// -h` printed all fifteen words and the whole `henri join` line to stdout
	// and exited 0. That phrase is the group key, and someone asking for help
	// is often doing it in a shared terminal, a screen recording or a CI log.
	if done, err := bareCommand("code", args); done || err != nil {
		return err
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if cfg.Phrase == "" {
		return errors.New("this config was made before recovery phrases existed, so there is no " +
			"phrase to show; re-run `henri init` on one device and `henri join` on the rest to move over")
	}
	fmt.Print(formatPhrase(cfg.Phrase))
	fmt.Printf("\nOn another device, run `henri join` and type them in when it asks.\n")
	return nil
}

// formatPhrase lays the words out numbered, four to a row, so they are easy to
// read off one screen while typing them into another. The padding on the last
// word of each row is trimmed: it was invisible, but it put trailing whitespace
// on every line of something people copy, paste and diff.
func formatPhrase(phrase string) string {
	w := mnemonic.Split(phrase)
	var b strings.Builder
	for i := 0; i < len(w); i += 4 {
		var row strings.Builder
		for j := i; j < i+4 && j < len(w); j++ {
			fmt.Fprintf(&row, "%3d. %-11s", j+1, w[j])
		}
		b.WriteString("  ")
		b.WriteString(strings.TrimRight(row.String(), " "))
		b.WriteString("\n")
	}
	return b.String()
}

// legacyCodePrefix tagged the base64 join codes henri handed out before
// recovery phrases. Still accepted so groups created then keep working.
const legacyCodePrefix = "henri1:"

type legacyCode struct {
	G string `json:"g"`
	K string `json:"k"`
	P int    `json:"p,omitempty"`
}

func joinLegacyCode(code, device string) (*config.Config, error) {
	code = strings.TrimSpace(code)
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(code, legacyCodePrefix))
	if err != nil {
		return nil, fmt.Errorf("that join code is damaged: %w", err)
	}
	var p legacyCode
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, fmt.Errorf("that join code is damaged: %w", err)
	}
	// The port in the code is the port the group was created on. It was parsed
	// and then thrown away, so a group on anything but 47600 rejoined on the
	// wrong one and silently never synced.
	listen := config.DefaultListenPort
	if p.P != 0 {
		if p.P < 1 || p.P > 65535 {
			return nil, fmt.Errorf("that join code names port %d, which is not a port", p.P)
		}
		listen = p.P
	}
	id, err := config.NewDeviceID()
	if err != nil {
		return nil, err
	}
	return &config.Config{
		GroupID:       p.G,
		Key:           p.K,
		DeviceID:      id,
		DeviceName:    device,
		ListenPort:    listen,
		DiscoveryPort: config.DefaultDiscoveryPort,
		Discovery:     true,
		Peers:         []string{},
		PollMillis:    config.DefaultPollMillis,
		MaxBytes:      config.DefaultMaxBytes,
	}, nil
}

// --- daemon ----------------------------------------------------------------

func cmdDaemon(args []string) error {
	fs := flag.NewFlagSet("daemon", flag.ContinueOnError)
	verbose := fs.Bool("verbose", false, "log every decision, including rejected connections")
	if done, err := parseFlags(fs, args, "henri daemon [flags]"); done || err != nil {
		return err
	}
	if err := noArgs(fs); err != nil {
		return err
	}

	cfg, err := loadConfigWatched()
	if err != nil {
		return err
	}

	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	n, err := node.New(cfg, log)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := n.Run(ctx); err != nil {
		return err
	}
	log.Info("henri stopped")
	return nil
}

// loadConfigWatched reads the config, complaining if it takes suspiciously long.
//
// Opening a file normally returns in microseconds. When it does not, the config
// is usually on a volume that is not answering -- an unmounted external disk, a
// network mount, or on macOS a removable volume whose access prompt nothing is
// present to accept. Without this the daemon just hangs with an empty log,
// which is a miserable thing to debug.
func loadConfigWatched() (*config.Config, error) {
	type result struct {
		cfg *config.Config
		err error
	}
	done := make(chan result, 1)
	go func() {
		cfg, err := config.Load()
		done <- result{cfg, err}
	}()

	warn := time.NewTimer(10 * time.Second)
	defer warn.Stop()
	for {
		select {
		case r := <-done:
			return r.cfg, r.err
		case <-warn.C:
			path, _ := config.Path()
			resolved, _ := config.ResolvedPath()
			fmt.Fprintf(os.Stderr, "henri: still waiting to read %s after 10s.\n", path)
			if resolved != "" && resolved != path {
				fmt.Fprintf(os.Stderr, "       It resolves to %s\n", resolved)
			}
			fmt.Fprintf(os.Stderr, "       If that is on an external or network volume, it may not be\n")
			fmt.Fprintf(os.Stderr, "       mounted, or the system may be waiting on a permission prompt\n")
			fmt.Fprintf(os.Stderr, "       that a background service cannot answer.\n")
			warn.Reset(60 * time.Second)
		}
	}
}

// --- status ----------------------------------------------------------------

func cmdStatus(args []string) error {
	if done, err := bareCommand("status", args); done || err != nil {
		return err
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	resp, err := node.Query(cfg, node.KindStatus)
	if err != nil {
		if errors.Is(err, node.ErrNotRunning) {
			fmt.Printf("henri  ○ stopped\n\n")
			fmt.Printf("  device   %s\n", cfg.DeviceName)
			fmt.Printf("  group    %s\n\n", cfg.GroupID)
			fmt.Printf("Start it with:  henri daemon\n")
			fmt.Printf("Or in the background at login:  henri service install\n")
			// Everything above is on stdout and reads as intended; only the
			// exit status says the daemon is down, which is what a wait loop
			// has to be able to see.
			return errSaidItAlready
		}
		return daemonQueryError(cfg.ListenPort, err)
	}
	st := resp.State
	if st == nil {
		return errors.New("the daemon replied without any status")
	}

	fmt.Printf("henri  ● running\n\n")
	fmt.Printf("  device     %s  (%s)\n", safe(st.Name, maxName), safe(st.Device, maxName))
	fmt.Printf("  group      %s\n", safe(st.Group, maxName))
	if st.ClipboardErr != "" {
		fmt.Printf("  clipboard  %s  ⚠ not readable: %s\n", safe(st.Tool, maxName), safe(st.ClipboardErr, maxDetail))
	} else {
		fmt.Printf("  clipboard  %s\n", safe(st.Tool, maxName))
	}
	if st.WatchMode != "" {
		fmt.Printf("  watching   %s\n", safe(st.WatchMode, maxDetail))
	}
	if strings.HasPrefix(st.WatchMode, "press-to-send") {
		defer func() {
			fmt.Printf("\nThis compositor only gives the clipboard to the focused window, so henri\n")
			fmt.Printf("cannot notice copies on its own. Bind a key to push them:\n\n")
			fmt.Printf("    henri hotkey install\n")
			fmt.Printf("\nThat key copies whatever you have highlighted and sends it in one press.\n")
		}()
	}
	fmt.Printf("  listening  :%d\n", st.ListenPort)
	fmt.Printf("  discovery  %s\n", discoveryLine(st))
	fmt.Printf("  uptime     %s   pid %d\n", since(st.StartedAt), st.PID)
	fmt.Printf("  traffic    %d sent · %d received\n", st.Sent, st.Received)
	if st.LastSyncAt != 0 {
		fmt.Printf("  last       %s from %s, %s ago\n",
			humanBytes(st.LastBytes), safe(st.LastFrom, maxName), since(st.LastSyncAt))
	} else {
		fmt.Printf("  last       nothing synced yet\n")
	}
	fmt.Println()
	printPeers(st.Peers)
	if w := discoveryWarning(st); w != "" {
		fmt.Println()
		fmt.Print(w)
	}
	return nil
}

// discoveryLine says whether discovery is on and, more usefully, whether it is
// hearing anything.
//
// "discovery on" on its own is what a daemon whose multicast membership has
// been dropped goes on printing while it hears nothing at all -- for seventeen
// minutes, in the case this line exists because of. The beacon count and the
// age of the last one are what make the difference visible.
func discoveryLine(st *node.State) string {
	if !st.Discovery {
		return "off"
	}
	line := fmt.Sprintf("on · %d beacons", st.Beacons)
	if st.LastBeaconAt != 0 {
		return line + " · last " + since(st.LastBeaconAt) + " ago"
	}
	return line + " · none heard yet"
}

// deafFor is how long `henri status` waits before saying discovery looks
// broken. Devices announce themselves every ten seconds, so two minutes is a
// dozen missed in a row: long enough that a laptop waking up or a moment of
// packet loss says nothing, short enough to catch the failure while the user is
// still looking at the screen.
const deafFor = 2 * time.Minute

// discoveryWarning explains a silent network, or returns "" when there is
// nothing to explain.
func discoveryWarning(st *node.State) string {
	if !st.Discovery {
		return ""
	}
	// With nothing ever heard, the daemon's own uptime is how long the silence
	// has lasted. A daemon that started ten seconds ago has not had a chance.
	last := st.LastBeaconAt
	if last == 0 {
		last = st.StartedAt
	}
	if last == 0 || time.Since(time.UnixMilli(last)) < deafFor {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "⚠  Discovery is on, but nothing has been heard for %s.\n\n", since(last))
	fmt.Fprintf(&b, "   Devices announce themselves every 10 seconds, so either nothing else in\n")
	fmt.Fprintf(&b, "   this group is running, or this device has stopped hearing them: a Wi-Fi\n")
	fmt.Fprintf(&b, "   roam, a suspend or a VPN coming up drops the multicast membership without\n")
	fmt.Fprintf(&b, "   telling the socket. henri takes it out again about once a minute.\n\n")
	fmt.Fprintf(&b, "   If another device is definitely running, check `henri status` there, and\n")
	fmt.Fprintf(&b, "   that UDP %d is allowed on both. To stop depending on multicast:\n\n", config.DefaultDiscoveryPort)
	fmt.Fprintf(&b, "       henri peers add <host:port>\n")
	return b.String()
}

// daemonQueryError names the port when a conversation with the local daemon
// fails for any reason other than nothing being there.
//
// ErrNotRunning is only returned for a refused connection. Everything else --
// another program holding the port, a half-open socket, something that is not
// henri answering -- arrived as a bare `henri: EOF`, which says nothing about
// where to look.
func daemonQueryError(port int, err error) error {
	return fmt.Errorf("the daemon on 127.0.0.1:%d did not answer: %w\n"+
		"       Something is listening on that port but is not replying as henri does.\n"+
		"       Check what has it (lsof -i :%d), or set listen_port in the config to\n"+
		"       something else and restart the daemon", port, err, port)
}

// --- doctor ----------------------------------------------------------------

// reachTimeout bounds one connection attempt to a peer. Short: this runs once
// per peer while somebody watches, and a peer that has not answered in three
// seconds on a local network is not going to.
const reachTimeout = 3 * time.Second

// cmdDoctor walks the whole path a copy has to travel and reports the first
// thing in the way.
//
// It exists because the failures henri actually produces do not look like
// failures. A firewall filters only inbound traffic, so a blocked device still
// announces itself and still pushes its own clipboard out -- both machines list
// each other as peers and sync runs in exactly one direction, which reads as a
// bug in henri rather than as a closed port. Nothing in `henri status` said so:
// it showed a green dot beside a peer this device had never once succeeded in
// reaching.
func cmdDoctor(args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fix := fs.Bool("fix", false, "offer to repair what can be repaired, rather than only reporting it")
	if done, err := parseFlags(fs, args, "henri doctor [-fix]"); done || err != nil {
		return err
	}
	if err := noArgs(fs); err != nil {
		return err
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	path, _ := config.Path()

	fmt.Printf("henri doctor\n\n")
	fmt.Printf("  config     %s\n", path)
	fmt.Printf("  device     %s\n", safe(cfg.DeviceName, maxName))
	fmt.Printf("  group      %s\n\n", safe(cfg.GroupID, maxName))

	var problems []string

	// The clipboard first: without it nothing else matters, and it is the one
	// piece that fails differently under a service than it does in a terminal.
	if err := clipboard.Available(); err != nil {
		problems = append(problems, "Install a clipboard helper: wl-clipboard on Wayland, xclip or xsel on X11.")
		fmt.Printf("  ✗ clipboard  %v\n", err)
	} else if _, rerr := clipboard.Read(); rerr != nil {
		problems = append(problems, fmt.Sprintf("The clipboard is not readable: %v\n"+
			"     A daemon started outside your graphical session cannot reach it.", rerr))
		fmt.Printf("  ✗ clipboard  %s, not readable: %v\n", clipboard.Tool(), rerr)
	} else {
		fmt.Printf("  ✓ clipboard  %s, readable\n", clipboard.Tool())
	}

	// Then the daemon, which everything after this depends on.
	var st *node.State
	resp, qerr := node.Query(cfg, node.KindStatus)
	switch {
	case qerr == nil && resp.State != nil:
		st = resp.State
		fmt.Printf("  ✓ daemon     running, pid %d, up %s\n", st.PID, since(st.StartedAt))
	case errors.Is(qerr, node.ErrNotRunning):
		fmt.Printf("  ✗ daemon     not running\n")
		problems = append(problems, "Start the daemon:  henri service install   (or `henri daemon` in a terminal)")
	default:
		fmt.Printf("  ✗ daemon     did not answer on :%d: %v\n", cfg.ListenPort, qerr)
		problems = append(problems, fmt.Sprintf("Something is on port %d that is not henri; check with `lsof -i :%d`.",
			cfg.ListenPort, cfg.ListenPort))
	}

	if st != nil {
		fmt.Printf("  ✓ listening  :%d\n", st.ListenPort)
		if st.Discovery {
			mark := "✓"
			if st.LastBeaconAt == 0 || time.Since(time.UnixMilli(st.LastBeaconAt)) > deafFor {
				mark = "⚠"
				problems = append(problems, "Discovery is hearing nothing. If the other device is running, its\n"+
					"     inbound UDP port is probably closed — see the firewall note below.")
			}
			fmt.Printf("  %s discovery  %s\n", mark, discoveryLine(st))
		} else {
			fmt.Printf("  – discovery  off (peers come from the config only)\n")
		}
	}

	// The firewall on THIS machine governs what reaches us, which is the half
	// of the problem the other device's owner cannot see.
	lan := firewall.LocalNetwork()
	fw := firewall.Detect(cfg.ListenPort, cfg.DiscoveryPort, lan)
	switch {
	case fw.Name == "":
		fmt.Printf("  ✓ firewall   none found\n")
	case !fw.Active:
		fmt.Printf("  ✓ firewall   %s is installed but not active\n", fw.Name)
	case fw.Blocking():
		fmt.Printf("  ⚠ firewall   %s is active; henri's ports are %s (tcp/%d) and %s (udp/%d)\n",
			fw.Name, fw.TCP, cfg.ListenPort, fw.UDP, cfg.DiscoveryPort)
	default:
		fmt.Printf("  ✓ firewall   %s is active and henri's ports are open\n", fw.Name)
	}
	if fw.Note != "" {
		fmt.Printf("               %s\n", fw.Note)
	}

	// And finally the peers, one connection each. This is the check that would
	// have named the problem straight away.
	if st != nil {
		fmt.Println()
		if len(st.Peers) == 0 {
			fmt.Printf("  no peers known yet\n")
		}
		for _, p := range st.Peers {
			name := safe(p.Name, maxName)
			if name == "" {
				name = safe(p.Addr, maxAddr)
			}
			ok, reason := firewall.Reach(p.Addr, reachTimeout)
			if ok {
				fmt.Printf("  ✓ %-16s %s\n", name, safe(p.Addr, maxAddr))
				continue
			}
			fmt.Printf("  ✗ %-16s %s\n", name, safe(p.Addr, maxAddr))
			fmt.Printf("    %s\n", reason)
			problems = append(problems, fmt.Sprintf("henri on %s cannot be reached. That is a firewall on THAT machine,\n"+
				"     not this one — open tcp/%d and udp/%d there. `henri doctor -fix` on\n"+
				"     that device will do it.", name, cfg.ListenPort, cfg.DiscoveryPort))
		}
	}

	if len(problems) == 0 && !fw.Blocking() {
		fmt.Printf("\nEverything checks out.\n")
		return nil
	}

	fmt.Printf("\nwhat to do\n\n")
	for i, p := range problems {
		fmt.Printf("  %d. %s\n\n", i+1, p)
	}

	// Only offer -fix when there is something on THIS machine to fix. The
	// common case is the opposite one -- the closed port is on the device that
	// cannot be reached -- and telling the user to run -fix here would send
	// them to the wrong machine.
	if len(fw.OpenCmds) > 0 {
		return offerFirewall(fw, *fix)
	}
	fmt.Printf("Nothing here needs henri's help; the fix is on the other device.\n")
	return nil
}

// offerFirewall prints the commands that open henri's ports, and with -fix runs
// them after asking. henri never edits a firewall without being told to twice:
// once by the flag, once by the answer.
func offerFirewall(fw firewall.Status, fix bool) error {
	fmt.Printf("  Open henri's ports in %s:\n\n", fw.Name)
	for _, c := range fw.OpenCmds {
		fmt.Printf("      %s\n", commandLine(c, fw.NeedsRoot))
	}
	fmt.Println()

	if !fix {
		fmt.Printf("Run `henri doctor -fix` to have henri run those for you.\n")
		return nil
	}
	ok, err := confirm("Run them now?")
	if err != nil {
		return err
	}
	if !ok {
		fmt.Println("Left the firewall alone.")
		return nil
	}
	for _, c := range fw.OpenCmds {
		line := commandLine(c, fw.NeedsRoot)
		fmt.Printf("  %s\n", line)
		name, rest := c[0], c[1:]
		if fw.NeedsRoot && os.Geteuid() != 0 {
			name, rest = "sudo", c
		}
		cmd := exec.Command(name, rest...)
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("%s: %w", line, err)
		}
	}
	fmt.Printf("\nOpened. Run `henri doctor` again to confirm, and on the other device too.\n")
	return nil
}

// commandLine renders a command the way the user would have to type it.
func commandLine(argv []string, needsRoot bool) string {
	quoted := make([]string, len(argv))
	for i, a := range argv {
		if strings.ContainsAny(a, " \t\"'") {
			a = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
		}
		quoted[i] = a
	}
	line := strings.Join(quoted, " ")
	if needsRoot && os.Geteuid() != 0 {
		return "sudo " + line
	}
	return line
}

// --- peers -----------------------------------------------------------------

func cmdPeers(args []string) error {
	sub, addr, done, err := parsePeersArgs(args)
	if done || err != nil {
		return err
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}

	switch sub {
	case "add":
		addr, err := normalizeAddr(addr, cfg.ListenPort)
		if err != nil {
			return err
		}
		for _, p := range cfg.Peers {
			if p == addr {
				fmt.Printf("%s is already a peer.\n", addr)
				return nil
			}
		}
		cfg.Peers = append(cfg.Peers, addr)
		if err := cfg.Save(); err != nil {
			return err
		}
		fmt.Printf("Added %s. Restart the daemon to pick it up.\n", addr)
		return nil
	case "rm":
		addr, err := normalizeAddr(addr, cfg.ListenPort)
		if err != nil {
			return err
		}
		kept := cfg.Peers[:0]
		found := false
		for _, p := range cfg.Peers {
			if p == addr {
				found = true
				continue
			}
			kept = append(kept, p)
		}
		if !found {
			return fmt.Errorf("%s is not in the peer list", addr)
		}
		cfg.Peers = kept
		if err := cfg.Save(); err != nil {
			return err
		}
		fmt.Printf("Removed %s. Restart the daemon to pick it up.\n", addr)
		return nil
	}

	resp, err := node.Query(cfg, node.KindStatus)
	if err != nil {
		if errors.Is(err, node.ErrNotRunning) {
			// Listing what the config says is a complete answer to `henri
			// peers`, so unlike `henri status` this is not a failure.
			if len(cfg.Peers) == 0 {
				fmt.Println("The daemon is not running and no peers are configured.")
				return nil
			}
			fmt.Println("The daemon is not running. Peers from the config:")
			fmt.Println()
			for _, p := range cfg.Peers {
				fmt.Println(peerLine("○", "—", p, "config", "never"))
			}
			return nil
		}
		return daemonQueryError(cfg.ListenPort, err)
	}
	if resp.State == nil {
		return errors.New("the daemon replied without any status")
	}
	printPeers(resp.State.Peers)
	return nil
}

// parsePeersArgs validates a `henri peers ...` invocation. sub is "", "add" or
// "rm"; addr is set for the two that take one.
//
// Split out from cmdPeers so that the argument handling can be tested without
// a config, a daemon or a network.
func parsePeersArgs(args []string) (sub, addr string, done bool, err error) {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		fs := flag.NewFlagSet("peers", flag.ContinueOnError)
		if done, err := parseFlags(fs, args, "henri peers [add|rm <host:port>]"); done || err != nil {
			return "", "", done, err
		}
		return "", "", false, noArgs(fs)
	}

	switch args[0] {
	case "add":
		sub = "add"
	case "rm", "remove":
		sub = "rm"
	default:
		return "", "", false, fmt.Errorf("unknown peers subcommand %q (try add, rm, or `henri peers` on its own to list)", args[0])
	}

	fs := flag.NewFlagSet("peers "+sub, flag.ContinueOnError)
	if done, err := parseFlags(fs, args[1:], "henri peers "+sub+" <host:port>"); done || err != nil {
		return "", "", done, err
	}
	if fs.NArg() != 1 {
		return "", "", false, fmt.Errorf("usage: henri peers %s <host:port>", sub)
	}
	return sub, fs.Arg(0), false, nil
}

// peerLine is the one layout for a device in a list. The running daemon's list
// and the config's used to be laid out differently, which made two views of the
// same thing look like two different programs.
func peerLine(mark, name, addr, source, last string) string {
	return "  " + mark + " " + pad(name, 18) + " " + pad(addr, 22) + " " + pad(source, 11) + " " + last
}

// pad left-aligns s in a column n characters wide.
//
// fmt's %-18s counts bytes rather than characters, so a device name with an
// accent in it -- or the em dash henri prints for a device whose name it does
// not know -- pushed every column after it out of line. Counting runes is right
// for everything but the double-width glyphs of CJK and emoji, which the
// standard library gives no way to measure.
func pad(s string, n int) string {
	if w := len([]rune(s)); w < n {
		return s + strings.Repeat(" ", n-w)
	}
	return s
}

func printPeers(peers []node.PeerInfo) {
	fmt.Println("peers")
	if len(peers) == 0 {
		fmt.Println("  none yet")
		fmt.Println()
		fmt.Println("Devices on the same network find each other automatically.")
		fmt.Println("For anything else:  henri peers add <host:port>")
		return
	}
	for _, p := range peers {
		mark := "○"
		last := "never"
		if p.LastSeenAt != 0 {
			mark = "●"
			last = since(p.LastSeenAt) + " ago"
		}
		name := safe(p.Name, maxName)
		if name == "" {
			name = "—"
		}
		fmt.Println(peerLine(mark, name, safe(p.Addr, maxAddr), p.Source, last))
	}
}

// --- send ------------------------------------------------------------------

func cmdSend(args []string) error {
	fs := flag.NewFlagSet("send", flag.ContinueOnError)
	highlighted := fs.Bool("highlighted", false,
		"send the highlighted text (the PRIMARY selection) and copy it here too, rather than the clipboard")
	if done, err := parseFlags(fs, args, "henri send [-highlighted]"); done || err != nil {
		return err
	}
	if err := noArgs(fs); err != nil {
		return err
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if _, err := node.QueryPush(cfg, *highlighted); err != nil {
		if errors.Is(err, node.ErrNotRunning) {
			return err
		}
		return daemonQueryError(cfg.ListenPort, err)
	}
	if !*highlighted {
		fmt.Println("Sent the current clipboard to the group.")
		return nil
	}
	// This used to say "Copied the highlighted text and sent it" on the
	// strength of the flag alone. Where there is no PRIMARY selection at all --
	// macOS and Windows -- the daemon sends the clipboard and writes nothing
	// locally, so the message was simply untrue and the next paste was stale.
	if !clipboard.HasPrimary() {
		fmt.Println("This system has no separate selection for highlighted text, so henri sent")
		fmt.Println("the clipboard instead. Nothing was copied to this device.")
		return nil
	}
	fmt.Println("Sent the highlighted text to the group and copied it here.")
	fmt.Println("(With nothing highlighted, henri sends the clipboard and copies nothing.)")
	return nil
}

// --- leave -----------------------------------------------------------------

func cmdLeave(args []string) error {
	fs := flag.NewFlagSet("leave", flag.ContinueOnError)
	yes := fs.Bool("yes", false, "skip the confirmation prompt")
	force := fs.Bool("force", false, "remove the config even while the daemon is running")
	if done, err := parseFlags(fs, args, "henri leave [flags]"); done || err != nil {
		return err
	}
	if err := noArgs(fs); err != nil {
		return err
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	path, err := config.Path()
	if err != nil {
		return err
	}

	// A running daemon already holds the key in memory and would happily keep
	// syncing after the file was gone, which looks like henri ignoring you.
	if _, qerr := node.Query(cfg, node.KindStatus); qerr == nil && !*force {
		return errors.New("the daemon is still running, and it would keep syncing after the config was removed.\n" +
			"       Stop it first (Ctrl-C, `systemctl --user stop henri`, or `launchctl unload ...`),\n" +
			"       or re-run with -force if you know it is about to stop")
	}

	fmt.Printf("This removes %s\n", path)
	// Where the config is itself a symlink, the file it links to goes as well:
	// taking only the link would leave the group key in the dotfiles directory
	// it came from. Only the last component counts -- a resolved parent
	// directory is the same file under another name, not a second one.
	if fi, lerr := os.Lstat(path); lerr == nil && fi.Mode()&os.ModeSymlink != 0 {
		if resolved, rerr := config.ResolvedPath(); rerr == nil && resolved != path {
			fmt.Printf("and the file it links to, %s\n", resolved)
		}
	}
	fmt.Printf("\n  device   %s\n", cfg.DeviceName)
	fmt.Printf("  group    %s\n\n", cfg.GroupID)

	if cfg.Phrase != "" {
		fmt.Printf("Only this device leaves. Your other devices carry on without it, and\n")
		fmt.Printf("you can rejoin any time with the same phrase.\n\n")
		fmt.Printf("If you have not written the phrase down, stop and run `henri code` first.\n\n")
	} else {
		fmt.Printf("This config has no recovery phrase, so there is no way back into this\n")
		fmt.Printf("group from this device once it is gone.\n\n")
	}

	if !*yes {
		ok, err := confirm("Remove it?")
		if err != nil {
			return err
		}
		if !ok {
			fmt.Println("Left it alone.")
			return nil
		}
	}

	removed, err := config.Remove()
	if err != nil {
		return err
	}
	fmt.Printf("\nRemoved %s\n", removed)
	fmt.Printf("This device is no longer in a clipboard group. `henri init` or `henri join` to set it up again.\n")
	return nil
}

// confirm asks a yes/no question. Anything other than an explicit yes is a no,
// and a closed stdin counts as no rather than silently proceeding.
func confirm(question string) (bool, error) {
	fmt.Printf("%s [y/N] ", question)
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil {
		if line == "" {
			fmt.Println()
			return false, nil
		}
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true, nil
	}
	return false, nil
}

// --- service ---------------------------------------------------------------

func cmdService(args []string) error {
	sub, force, done, err := parseServiceArgs(args)
	if done || err != nil {
		return err
	}
	mgr, err := service.New()
	if err != nil {
		return err
	}
	switch sub {
	case "install":
		return cmdServiceInstall(mgr, force)
	case "uninstall":
		return cmdServiceUninstall(mgr)
	case "restart":
		if err := mgr.Restart(); err != nil {
			return err
		}
		fmt.Println("Restarted.")
		return nil
	case "status":
		return cmdServiceStatus(mgr)
	default: // "logs"
		return cmdServiceLogs(mgr)
	}
}

// parseServiceArgs validates a `henri service ...` invocation and does nothing
// else. It returns the canonical subcommand name.
//
// It is separate from cmdService on purpose: this is the half that can be
// tested, because the other half installs and removes real launchd agents and
// systemd user units. It is also why the flags are parsed before the service
// manager is asked for -- only `install` ever had a FlagSet, so `henri service
// uninstall --nope` went straight through and removed the service, reporting
// only that it had done so.
func parseServiceArgs(args []string) (sub string, force, done bool, err error) {
	if len(args) == 0 {
		return "", false, false, errors.New("usage: henri service <install|uninstall|restart|status|logs>")
	}
	switch args[0] {
	case "install", "enable":
		fs := flag.NewFlagSet("service install", flag.ContinueOnError)
		f := fs.Bool("force", false, "install even if the binary is somewhere that may not always be there")
		if done, err := parseFlags(fs, args[1:], "henri service install [-force]"); done || err != nil {
			return "", false, done, err
		}
		if err := noArgs(fs); err != nil {
			return "", false, false, err
		}
		return "install", *f, false, nil
	case "uninstall", "remove", "disable":
		sub = "uninstall"
	case "restart":
		sub = "restart"
	case "status":
		sub = "status"
	case "logs":
		sub = "logs"
	default:
		return "", false, false, fmt.Errorf("unknown service command %q (try install, uninstall, restart, status or logs)", args[0])
	}
	if done, err := bareCommand("service "+args[0], args[1:]); done || err != nil {
		return "", false, done, err
	}
	return sub, false, false, nil
}

func cmdServiceInstall(mgr service.Manager, force bool) error {
	// Installing a service that immediately exits for want of a config is a
	// confusing way to find out.
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	binary, err := service.BinaryPath()
	if err != nil {
		return err
	}

	fmt.Printf("Installing henri as a %s service.\n\n", mgr.Name())
	fmt.Printf("  binary   %s\n", binary)
	fmt.Printf("  unit     %s\n\n", mgr.UnitPath())

	if why := service.VolatileLocation(binary); why != "" && !force {
		fmt.Printf("  ⚠  That binary is on %s. The service runs this exact path, so\n", why)
		fmt.Printf("     henri would fail to start whenever it is not mounted.\n\n")
		fmt.Printf("     Install it somewhere permanent first, then install the service\n")
		fmt.Printf("     from there:\n\n")
		fmt.Printf("         make build && sudo make install   # or PREFIX=$HOME/.local, without sudo\n")
		fmt.Printf("         henri service install             # run the installed copy, not this one\n\n")
		return errors.New("refusing to point a login service at a volatile path (use -force to insist)")
	}

	// The config matters as much as the binary. `~/.config` is very often a
	// symlink into a dotfiles directory, and if that lands on removable media
	// the daemon cannot read it at login: the volume may not be mounted yet,
	// and macOS gates removable volumes behind a consent prompt that a headless
	// service has no way to answer. The open() simply blocks forever.
	if resolved, rerr := config.ResolvedPath(); rerr == nil {
		if why := service.VolatileLocation(resolved); why != "" && !force {
			logical, _ := config.Path()
			fmt.Printf("  ⚠  Your config lives on %s.\n\n", why)
			if logical != resolved {
				fmt.Printf("       %s\n     is really %s\n\n", logical, resolved)
			}
			fmt.Printf("     A service started at login cannot count on that being readable:\n")
			fmt.Printf("     the volume may not be mounted yet, and macOS gates removable\n")
			fmt.Printf("     volumes behind a permission prompt that nothing is there to answer.\n\n")
			fmt.Printf("     Move it somewhere on the internal disk and point henri at it:\n\n")
			fmt.Printf("         mkdir -p ~/.henri && mv %s ~/.henri/config.json\n", logical)
			fmt.Printf("         export HENRI_CONFIG=$HOME/.henri/config.json   # add to your shell rc\n")
			fmt.Printf("         henri service install\n\n")
			fmt.Printf("     (install records HENRI_CONFIG in the service, so it will follow.)\n")
			return errors.New("refusing to install a login service that cannot reach its config (use -force to insist)")
		}
	}

	if err := mgr.Install(binary); err != nil {
		return err
	}

	// Confirm it actually came up rather than claiming success and leaving the
	// user to discover otherwise.
	fmt.Printf("Waiting for it to start")
	var up bool
	for i := 0; i < 20; i++ {
		time.Sleep(250 * time.Millisecond)
		if _, err := node.Query(cfg, node.KindStatus); err == nil {
			up = true
			break
		}
		fmt.Print(".")
	}
	fmt.Println()

	st, sterr := mgr.Status()
	if !up {
		fmt.Printf("\nInstalled, but it has not answered yet.\n")
		// The status error was thrown away here, at exactly the moment the user
		// most needs to know what the service manager thinks -- and then the
		// empty LogHint that came with it was printed as a command to run.
		if sterr != nil {
			fmt.Printf("  asking %s about it failed: %v\n", mgr.Name(), sterr)
		}
		if st.Detail != "" {
			fmt.Printf("  %s reports: %s\n", mgr.Name(), st.Detail)
		}
		if st.LogHint != "" {
			fmt.Printf("\nCheck the logs:  %s\n", st.LogHint)
		} else {
			fmt.Printf("\nRun `henri daemon` in a terminal to see what it is failing on.\n")
		}
		return nil
	}

	fmt.Printf("\nhenri is running in the background and will start again at login.\n\n")
	fmt.Printf("  henri status          see what it is doing\n")
	fmt.Printf("  henri service logs    follow its output\n")
	fmt.Printf("  henri service uninstall\n\n")
	fmt.Printf("The service points at that exact binary — reinstall if you move it.\n")

	// Say this here rather than leaving it to be discovered. A closed inbound
	// port does not stop henri starting, and it does not stop this device
	// announcing itself or pushing its own clipboard out -- so both machines
	// list each other as peers and sync runs in one direction only, which reads
	// as henri being broken. It is the last thing standing between a fresh
	// install and a working one, and the only one henri cannot see by itself.
	fw := firewall.Detect(cfg.ListenPort, cfg.DiscoveryPort, firewall.LocalNetwork())
	if fw.Blocking() {
		fmt.Printf("\n⚠  %s is running here, and henri needs two ports open to receive:\n", fw.Name)
		fmt.Printf("   tcp/%d for clipboard contents, udp/%d for finding devices.\n\n", cfg.ListenPort, cfg.DiscoveryPort)
		if len(fw.OpenCmds) > 0 {
			fmt.Printf("   Open them with:  henri doctor -fix\n\n")
		}
		fmt.Printf("   Without that, this device can still send, so the other machine will\n")
		fmt.Printf("   see it and sync will work in one direction only.\n")
	}
	return nil
}

func cmdServiceUninstall(mgr service.Manager) error {
	st, err := mgr.Status()
	if err != nil {
		// Without an answer there is no telling "nothing is installed" from
		// "the service manager is not talking", and the two want opposite
		// things done. Discarding the error meant the first was reported for
		// both, so a busy systemctl looked like a clean machine.
		return fmt.Errorf("could not ask %s whether henri is installed: %w", mgr.Name(), err)
	}
	if !st.Installed {
		fmt.Println("No henri service is installed.")
		return nil
	}
	if err := mgr.Uninstall(); err != nil {
		return err
	}
	fmt.Printf("Removed the %s service. henri no longer starts at login.\n", mgr.Name())
	fmt.Printf("Your config and group are untouched — `henri daemon` still works.\n")
	return nil
}

func cmdServiceStatus(mgr service.Manager) error {
	st, err := mgr.Status()
	if err != nil {
		return err
	}
	mark := func(b bool) string {
		if b {
			return "yes"
		}
		return "no"
	}
	fmt.Printf("henri service\n\n")
	fmt.Printf("  manager    %s\n", st.Manager)
	fmt.Printf("  installed  %s\n", mark(st.Installed))
	fmt.Printf("  at login   %s\n", mark(st.Enabled))
	fmt.Printf("  running    %s\n", mark(st.Running))
	fmt.Printf("  unit       %s\n", st.UnitPath)
	fmt.Printf("  logs       %s\n", st.LogHint)
	if st.Detail != "" {
		fmt.Printf("  detail     %s\n", st.Detail)
	}
	if !st.Installed {
		fmt.Printf("\nInstall it with:  henri service install\n")
	}
	return nil
}

func cmdServiceLogs(mgr service.Manager) error {
	st, err := mgr.Status()
	if err != nil {
		return err
	}
	// LogCmd, not strings.Fields(LogHint). The hint is a line for a person to
	// read and it contains paths: a home directory with a space in it turned
	// one argument into two, and the log command failed on a machine where the
	// printed hint was perfectly correct.
	if len(st.LogCmd) == 0 {
		if st.LogHint != "" {
			return fmt.Errorf("henri cannot follow the logs for you on this platform; run:  %s", st.LogHint)
		}
		return errors.New("no log command for this platform")
	}
	cmd := exec.Command(st.LogCmd[0], st.LogCmd[1:]...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	return cmd.Run()
}

// --- hotkey ----------------------------------------------------------------

func cmdHotkey(args []string) error {
	sub, accel, done, err := parseHotkeyArgs(args)
	if done || err != nil {
		return err
	}
	switch sub {
	case "install":
		return cmdHotkeyInstall(accel)
	case "uninstall":
		removed, err := hotkey.UninstallReport()
		if err != nil {
			if errors.Is(err, hotkey.ErrManual) {
				return fmt.Errorf("henri did not set up a shortcut on %s, so there is nothing to remove", hotkey.Desktop())
			}
			return err
		}
		// Uninstall returned nil whether or not there had been a binding, so
		// henri thanked people for removing a shortcut they never had.
		if !removed {
			fmt.Printf("henri had no shortcut bound on %s, so there was nothing to remove.\n", hotkey.Desktop())
			return nil
		}
		fmt.Println("Removed henri's shortcut.")
		return nil
	default: // "status"
		return cmdHotkeyStatus(accel)
	}
}

// parseHotkeyArgs validates a `henri hotkey ...` invocation, so that a flag
// typed after the subcommand is reported rather than dropped.
func parseHotkeyArgs(args []string) (sub, accel string, done bool, err error) {
	sub = "status"
	rest := args
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		switch args[0] {
		case "install", "add", "enable":
			sub = "install"
		case "uninstall", "remove", "disable":
			sub = "uninstall"
		case "status":
			sub = "status"
		default:
			return "", "", false, fmt.Errorf("unknown hotkey command %q (try install, uninstall or status)", args[0])
		}
		rest = args[1:]
	}

	fs := flag.NewFlagSet("hotkey "+sub, flag.ContinueOnError)
	a := fs.String("accel", hotkey.DefaultAccel, "the shortcut to bind, in GNOME accelerator syntax")
	if done, err := parseFlags(fs, rest, "henri hotkey [install|uninstall|status] [-accel <shortcut>]"); done || err != nil {
		return "", "", done, err
	}
	if err := noArgs(fs); err != nil {
		return "", "", false, err
	}
	return sub, *a, false, nil
}

// sendCommand is what a hotkey is bound to: copy the highlighted text and send
// it in one press.
//
// One function because `hotkey install` and `hotkey status` have to agree.
// Status printed `henri send` instead, which is the command that sends the
// clipboard -- so on every desktop henri cannot script, which is the entire
// point of the feature, the instructions told people to bind a key that did
// nothing until they had also pressed Ctrl+C.
func sendCommand() string {
	binary, err := service.BinaryPath()
	if err != nil || binary == "" {
		binary = "henri"
	}
	return binary + " send -highlighted"
}

func cmdHotkeyInstall(accel string) error {
	command := sendCommand()

	if err := hotkey.Install(command, accel); err != nil {
		if errors.Is(err, hotkey.ErrManual) {
			fmt.Printf("henri cannot set shortcuts on %s automatically.\n\n", hotkey.Desktop())
			fmt.Print(hotkey.Instructions(command, accel))
			return nil
		}
		return err
	}

	fmt.Printf("Bound %s.\n\n", hotkey.Human(accel))
	fmt.Printf("  command  %s\n\n", command)
	fmt.Printf("Highlight some text and press %s. It copies the highlighted\n", hotkey.Human(accel))
	fmt.Printf("text to this device's clipboard and sends it to the others in one go —\n")
	fmt.Printf("no Ctrl+C needed, because highlighted text is already published.\n\n")
	fmt.Printf("Receiving stays automatic; this is only about sending.\n")
	if !clipboard.HasPrimary() {
		fmt.Printf("\nNote: this system has no separate selection for highlighted text,\n")
		fmt.Printf("so the key will send the clipboard instead.\n")
	}
	return nil
}

func cmdHotkeyStatus(accel string) error {
	st, err := hotkey.Get()
	if err != nil && !errors.Is(err, hotkey.ErrManual) {
		return err
	}
	fmt.Printf("henri hotkey\n\n")
	fmt.Printf("  desktop    %s\n", st.Desktop)
	if errors.Is(err, hotkey.ErrManual) {
		fmt.Printf("  managed    no — henri cannot script shortcuts here\n\n")
		fmt.Print(hotkey.Instructions(sendCommand(), accel))
		return nil
	}
	if !st.Installed {
		fmt.Printf("  bound      no\n\n")
		fmt.Printf("Bind one with:  henri hotkey install\n")
		return nil
	}
	fmt.Printf("  bound      %s\n", hotkey.Human(st.Accel))
	fmt.Printf("  command    %s\n", st.Command)
	return nil
}

// --- helpers ---------------------------------------------------------------

// deviceName resolves the -name flag, falling back to the hostname. Looked up
// only when it is needed: as the default of the flag itself it ran on every
// init and join, including the ones that gave a name and never used it.
func deviceName(given string) string {
	if given != "" {
		return given
	}
	if h, err := os.Hostname(); err == nil && h != "" {
		return h
	}
	return "device"
}

// normalizeAddr turns what someone typed into an address henri can dial.
//
// It used to decide whether an address already had a port by looking for a
// colon, which every IPv6 address is full of: `henri peers add ::1` stored
// "::1", net.SplitHostPort rejected it later on, and the peer silently never
// synced while `henri peers` listed it looking perfectly normal. A port that is
// not a number, or not a port, was stored just as happily.
func normalizeAddr(addr string, defaultPort int) (string, error) {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return "", errors.New("the address is empty")
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		// No port at all, or a bare IPv6 address, which looks like one.
		host, port = strings.Trim(addr, "[]"), strconv.Itoa(defaultPort)
	}
	host = strings.TrimSpace(host)
	if host == "" {
		return "", fmt.Errorf("%q has no host in it — an address is <host> or <host>:<port>", addr)
	}
	// A host with a colon left in it has to be an IPv6 literal; anything else
	// is a mistyped address that JoinHostPort would otherwise bracket and
	// accept. The zone after a % is part of a link-local address, not the IP.
	if strings.Contains(host, ":") {
		bare, _, _ := strings.Cut(host, "%")
		if net.ParseIP(bare) == nil {
			return "", fmt.Errorf("%q is not an address henri can dial — "+
				"write a name or an IPv4 address as <host>:<port>, and an IPv6 one as [<address>]:<port>", addr)
		}
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return "", fmt.Errorf("%q does not have a usable port — it must be a number from 1 to 65535", addr)
	}
	return net.JoinHostPort(host, strconv.Itoa(n)), nil
}

// Bounds on anything printed that another device chose. See safe.
const (
	maxName   = 32
	maxAddr   = 47 // "[" + a full IPv6 address + "]:65535"
	maxDetail = 200
)

// safe renders a string that came from somewhere else.
//
// Device names, addresses and clipboard error text all arrive over the network
// or out of a helper program's stderr, and written to a terminal unfiltered an
// escape sequence in one of them is not text at all: it can move the cursor,
// repaint the line, or decide what every other member's `henri status` appears
// to say. So only printable characters survive -- which excludes both the C0
// controls and the C1 range, and the invisible formatting characters that can
// reorder a line -- and only so many of them.
func safe(s string, max int) string {
	var b strings.Builder
	n := 0
	for _, r := range strings.TrimSpace(s) {
		if !unicode.IsPrint(r) {
			continue
		}
		if n == max {
			b.WriteString("…")
			break
		}
		b.WriteRune(r)
		n++
	}
	return b.String()
}

// since renders how long ago something happened.
func since(unixMilli int64) string {
	// A zero timestamp is "this never happened", not 1970: since(0) used to
	// report 496247h16m, which is the age of the epoch and not an uptime.
	if unixMilli <= 0 {
		return "unknown"
	}
	d := time.Since(time.UnixMilli(unixMilli))
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Second:
		return "0s"
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm%ds", int(d.Minutes()), int(d.Seconds())%60)
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
	default:
		return fmt.Sprintf("%dd%dh", int(d.Hours())/24, int(d.Hours())%24)
	}
}

// humanBytes renders a byte count the way a person reads it.
//
// The unit table is bounded and the loop stops at the end of it. It used to
// index a four-character string with whatever exponent the loop reached, so a
// payload limit of a pebibyte -- reachable by hand-editing max_payload_bytes --
// crashed the command on an index out of range rather than printing a slightly
// silly number.
func humanBytes(n int) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	const units = "KMGTPE"
	div, exp := int64(unit), 0
	for v := int64(n) / unit; v >= unit && exp < len(units)-1; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), units[exp])
}
