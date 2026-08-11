// Command henri keeps the clipboard in sync across your devices.
package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"bufio"

	"github.com/justin06lee/henri/internal/clipboard"
	"github.com/justin06lee/henri/internal/config"
	"github.com/justin06lee/henri/internal/hotkey"
	"github.com/justin06lee/henri/internal/mnemonic"
	"github.com/justin06lee/henri/internal/node"
	"github.com/justin06lee/henri/internal/service"
)

// version is overridden at build time with -ldflags "-X main.version=...".
var version = "0.9.0"

// codePrefix tags a join code so a mistyped paste fails early and clearly.
const codePrefix = "henri1:"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "henri: "+err.Error())
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
		return cmdCode()
	case "daemon", "run", "start":
		return cmdDaemon(args[1:])
	case "status":
		return cmdStatus()
	case "peers":
		return cmdPeers(args[1:])
	case "send", "push":
		return cmdSend(args[1:])
	case "leave", "forget":
		return cmdLeave(args[1:])
	case "service":
		return cmdService(args[1:])
	case "hotkey", "key":
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
  join <words>    join an existing group using its recovery phrase
  code            print this group's recovery phrase
  daemon          run the sync daemon in the foreground
  service         run henri in the background, starting at login
  hotkey          bind a key to ` + "`henri send`" + `, for desktops henri cannot watch
  status          show what the local daemon is doing
  peers           list known devices; ` + "`peers add|rm <host:port>`" + ` to edit
  send            send the clipboard to the group (-highlighted for the selection)
  leave           remove this device's config and leave the group
  version         print the version

Run 'henri <command> -h' for the flags a command accepts.
`)
}

// --- init ------------------------------------------------------------------

func cmdInit(args []string) error {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	name := fs.String("name", defaultDeviceName(), "name for this device")
	port := fs.Int("port", config.DefaultListenPort, "TCP port to receive clipboard updates on")
	discovery := fs.Bool("discovery", true, "find other devices on the LAN automatically")
	words := fs.Int("words", 15, "length of the recovery phrase (12, 15, 18, 21 or 24)")
	force := fs.Bool("force", false, "overwrite an existing config")
	if err := fs.Parse(args); err != nil {
		return err
	}
	bits, ok := mnemonic.EntropyBitsFor(*words)
	if !ok {
		return fmt.Errorf("-words must be 12, 15, 18, 21 or 24, not %d — a phrase length is "+
			"always a multiple of three, because each word carries 11 bits and the checksum "+
			"is a 32nd of the entropy", *words)
	}

	path, err := config.Path()
	if err != nil {
		return err
	}
	if _, err := os.Stat(path); err == nil && !*force {
		return fmt.Errorf("a config already exists at %s (use -force to replace it, which leaves this device's current group)", path)
	}

	cfg, err := config.New(*name, bits)
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
	fmt.Printf("\nOn every other device, run:\n\n  henri join %s\n\n", cfg.Phrase)
	fmt.Printf("Anyone who has those words can read everything you copy. Read them out\n")
	fmt.Printf("loud or type them in by hand — don't paste them through anything you\n")
	fmt.Printf("don't control.\n\n")
	fmt.Printf("Then start the daemon on each device:  henri daemon\n")
	if err := clipboard.Available(); err != nil {
		fmt.Printf("\nHeads up: %v\n", err)
	}
	return nil
}

// --- join ------------------------------------------------------------------

func cmdJoin(args []string) error {
	fs := flag.NewFlagSet("join", flag.ContinueOnError)
	name := fs.String("name", defaultDeviceName(), "name for this device")
	port := fs.Int("port", config.DefaultListenPort, "TCP port to receive clipboard updates on")
	discovery := fs.Bool("discovery", true, "find other devices on the LAN automatically")
	force := fs.Bool("force", false, "overwrite an existing config")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() == 0 {
		return errors.New("usage: henri join <word> <word> ...  (the words `henri init` printed)")
	}

	path, err := config.Path()
	if err != nil {
		return err
	}
	if _, err := os.Stat(path); err == nil && !*force {
		return fmt.Errorf("a config already exists at %s (use -force to replace it)", path)
	}

	// The words can arrive as separate arguments or as one quoted string, and
	// older releases handed out a base64 join code instead.
	input := strings.Join(fs.Args(), " ")
	var cfg *config.Config
	if strings.HasPrefix(strings.TrimSpace(input), legacyCodePrefix) {
		cfg, err = joinLegacyCode(input, *name)
	} else {
		cfg, err = config.Join(input, *name)
	}
	if err != nil {
		return err
	}

	cfg.ListenPort = *port
	cfg.Discovery = *discovery
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

func cmdCode() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if cfg.Phrase == "" {
		return errors.New("this config was made before recovery phrases existed, so there is no " +
			"phrase to show; re-run `henri init` on one device and `henri join` on the rest to move over")
	}
	fmt.Print(formatPhrase(cfg.Phrase))
	fmt.Printf("\n  henri join %s\n", cfg.Phrase)
	return nil
}

// formatPhrase lays the words out numbered, four to a row, so they are easy to
// read off one screen while typing them into another.
func formatPhrase(phrase string) string {
	w := mnemonic.Split(phrase)
	var b strings.Builder
	for i := 0; i < len(w); i += 4 {
		b.WriteString("  ")
		for j := i; j < i+4 && j < len(w); j++ {
			b.WriteString(fmt.Sprintf("%3d. %-11s", j+1, w[j]))
		}
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

func joinLegacyCode(code, deviceName string) (*config.Config, error) {
	code = strings.TrimSpace(code)
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(code, legacyCodePrefix))
	if err != nil {
		return nil, fmt.Errorf("that join code is damaged: %w", err)
	}
	var p legacyCode
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, fmt.Errorf("that join code is damaged: %w", err)
	}
	device, err := config.NewDeviceID()
	if err != nil {
		return nil, err
	}
	return &config.Config{
		GroupID:       p.G,
		Key:           p.K,
		DeviceID:      device,
		DeviceName:    deviceName,
		ListenPort:    config.DefaultListenPort,
		DiscoveryPort: config.DefaultDiscoveryPort,
		Discovery:     true,
		PollMillis:    config.DefaultPollMillis,
		MaxBytes:      config.DefaultMaxBytes,
	}, nil
}

// --- daemon ----------------------------------------------------------------

func cmdDaemon(args []string) error {
	fs := flag.NewFlagSet("daemon", flag.ContinueOnError)
	verbose := fs.Bool("verbose", false, "log every decision, including rejected connections")
	if err := fs.Parse(args); err != nil {
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

func cmdStatus() error {
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
			return nil
		}
		return err
	}
	st := resp.State
	if st == nil {
		return errors.New("the daemon replied without any status")
	}

	fmt.Printf("henri  ● running\n\n")
	fmt.Printf("  device     %s  (%s)\n", st.Name, st.Device)
	fmt.Printf("  group      %s\n", st.Group)
	if st.ClipboardErr != "" {
		fmt.Printf("  clipboard  %s  ⚠ not readable: %s\n", st.Tool, st.ClipboardErr)
	} else {
		fmt.Printf("  clipboard  %s\n", st.Tool)
	}
	if st.WatchMode != "" {
		fmt.Printf("  watching   %s\n", st.WatchMode)
	}
	if strings.HasPrefix(st.WatchMode, "press-to-send") {
		defer func() {
			fmt.Printf("\nThis compositor only gives the clipboard to the focused window, so henri\n")
			fmt.Printf("cannot notice copies on its own. Bind a key to push them:\n\n")
			fmt.Printf("    henri hotkey install\n")
			fmt.Printf("\nThat key copies whatever you have highlighted and sends it in one press.\n")
		}()
	}
	fmt.Printf("  listening  :%d   discovery %s\n", st.ListenPort, onOff(st.Discovery))
	fmt.Printf("  uptime     %s   pid %d\n", since(st.StartedAt), st.PID)
	fmt.Printf("  traffic    %d sent · %d received\n", st.Sent, st.Received)
	if st.LastSyncAt != 0 {
		fmt.Printf("  last       %s from %s, %s ago\n", humanBytes(st.LastBytes), st.LastFrom, since(st.LastSyncAt))
	} else {
		fmt.Printf("  last       nothing synced yet\n")
	}
	fmt.Println()
	printPeers(st.Peers)
	return nil
}

// --- peers -----------------------------------------------------------------

func cmdPeers(args []string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	if len(args) > 0 {
		switch args[0] {
		case "add":
			if len(args) != 2 {
				return errors.New("usage: henri peers add <host:port>")
			}
			addr, err := normalizeAddr(args[1], cfg.ListenPort)
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
		case "rm", "remove":
			if len(args) != 2 {
				return errors.New("usage: henri peers rm <host:port>")
			}
			addr, err := normalizeAddr(args[1], cfg.ListenPort)
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
		default:
			return fmt.Errorf("unknown peers subcommand %q", args[0])
		}
	}

	resp, err := node.Query(cfg, node.KindStatus)
	if err != nil {
		if errors.Is(err, node.ErrNotRunning) {
			if len(cfg.Peers) == 0 {
				fmt.Println("The daemon is not running and no peers are configured.")
				return nil
			}
			fmt.Println("The daemon is not running. Peers from the config:")
			for _, p := range cfg.Peers {
				fmt.Printf("  ○ %-28s config\n", p)
			}
			return nil
		}
		return err
	}
	if resp.State == nil {
		return errors.New("the daemon replied without any status")
	}
	printPeers(resp.State.Peers)
	return nil
}

func printPeers(peers []node.PeerInfo) {
	if len(peers) == 0 {
		fmt.Println("peers      none yet")
		fmt.Println()
		fmt.Println("Devices on the same network find each other automatically.")
		fmt.Println("For anything else:  henri peers add <host:port>")
		return
	}
	fmt.Println("peers")
	for _, p := range peers {
		mark := "○"
		last := "never"
		if p.LastSeenAt != 0 {
			mark = "●"
			last = since(p.LastSeenAt) + " ago"
		}
		name := p.Name
		if name == "" {
			name = "—"
		}
		fmt.Printf("  %s %-18s %-22s %-11s %s\n", mark, name, p.Addr, p.Source, last)
	}
}

// --- send ------------------------------------------------------------------

func cmdSend(args []string) error {
	fs := flag.NewFlagSet("send", flag.ContinueOnError)
	highlighted := fs.Bool("highlighted", false,
		"send the highlighted text (the PRIMARY selection) and copy it here too, rather than the clipboard")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if _, err := node.QueryPush(cfg, *highlighted); err != nil {
		return err
	}
	if *highlighted {
		fmt.Println("Copied the highlighted text and sent it to the group.")
	} else {
		fmt.Println("Sent the current clipboard to the group.")
	}
	return nil
}

// --- leave -----------------------------------------------------------------

func cmdLeave(args []string) error {
	fs := flag.NewFlagSet("leave", flag.ContinueOnError)
	yes := fs.Bool("yes", false, "skip the confirmation prompt")
	force := fs.Bool("force", false, "remove the config even while the daemon is running")
	if err := fs.Parse(args); err != nil {
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

	fmt.Printf("This removes %s\n\n", path)
	fmt.Printf("  device   %s\n", cfg.DeviceName)
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
	if len(args) == 0 {
		return errors.New("usage: henri service <install|uninstall|restart|status|logs>")
	}
	mgr, err := service.New()
	if err != nil {
		return err
	}
	switch args[0] {
	case "install", "enable":
		fs := flag.NewFlagSet("service install", flag.ContinueOnError)
		force := fs.Bool("force", false, "install even if the binary is somewhere that may not always be there")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		return cmdServiceInstall(mgr, *force)
	case "uninstall", "remove", "disable":
		return cmdServiceUninstall(mgr)
	case "restart":
		if err := mgr.Restart(); err != nil {
			return err
		}
		fmt.Println("Restarted.")
		return nil
	case "status":
		return cmdServiceStatus(mgr)
	case "logs":
		return cmdServiceLogs(mgr)
	default:
		return fmt.Errorf("unknown service command %q (try install, uninstall, restart, status or logs)", args[0])
	}
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
		fmt.Printf("         make build && sudo make install\n")
		fmt.Printf("         %s/bin/henri service install\n\n", "/usr/local")
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

	st, _ := mgr.Status()
	if !up {
		fmt.Printf("\nInstalled, but it has not answered yet.\n")
		if st.Detail != "" {
			fmt.Printf("  %s reports: %s\n", mgr.Name(), st.Detail)
		}
		fmt.Printf("\nCheck the logs:  %s\n", st.LogHint)
		return nil
	}

	fmt.Printf("\nhenri is running in the background and will start again at login.\n\n")
	fmt.Printf("  henri status          see what it is doing\n")
	fmt.Printf("  henri service logs    follow its output\n")
	fmt.Printf("  henri service uninstall\n\n")
	fmt.Printf("The service points at that exact binary — reinstall if you move it.\n")
	return nil
}

func cmdServiceUninstall(mgr service.Manager) error {
	st, _ := mgr.Status()
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
	parts := strings.Fields(st.LogHint)
	if len(parts) == 0 {
		return errors.New("no log command for this platform")
	}
	cmd := exec.Command(parts[0], parts[1:]...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	return cmd.Run()
}

// --- hotkey ----------------------------------------------------------------

func cmdHotkey(args []string) error {
	if len(args) == 0 {
		args = []string{"status"}
	}
	fs := flag.NewFlagSet("hotkey", flag.ContinueOnError)
	accel := fs.String("accel", hotkey.DefaultAccel, "the shortcut to bind, in GNOME accelerator syntax")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}

	switch args[0] {
	case "install", "add", "enable":
		return cmdHotkeyInstall(*accel)
	case "uninstall", "remove", "disable":
		if err := hotkey.Uninstall(); err != nil {
			if errors.Is(err, hotkey.ErrManual) {
				return fmt.Errorf("henri did not set up a shortcut on %s, so there is nothing to remove", hotkey.Desktop())
			}
			return err
		}
		fmt.Println("Removed henri's shortcut.")
		return nil
	case "status":
		return cmdHotkeyStatus(*accel)
	default:
		return fmt.Errorf("unknown hotkey command %q (try install, uninstall or status)", args[0])
	}
}

func cmdHotkeyInstall(accel string) error {
	binary, err := service.BinaryPath()
	if err != nil {
		return err
	}
	command := binary + " send -highlighted"

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
		binary, _ := service.BinaryPath()
		fmt.Printf("  managed    no — henri cannot script shortcuts here\n\n")
		fmt.Print(hotkey.Instructions(binary+" send", accel))
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

func defaultDeviceName() string {
	if h, err := os.Hostname(); err == nil && h != "" {
		return h
	}
	return "device"
}

func normalizeAddr(addr string, defaultPort int) (string, error) {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return "", errors.New("the address is empty")
	}
	if !strings.Contains(addr, ":") {
		return addr + ":" + strconv.Itoa(defaultPort), nil
	}
	return addr, nil
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

func since(unixMilli int64) string {
	d := time.Since(time.UnixMilli(unixMilli))
	switch {
	case d < time.Second:
		return "0s"
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm%ds", int(d.Minutes()), int(d.Seconds())%60)
	default:
		return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
	}
}

func humanBytes(n int) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := int64(n) / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGT"[exp])
}
