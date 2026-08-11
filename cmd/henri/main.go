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
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/justin06lee/henri/internal/clipboard"
	"github.com/justin06lee/henri/internal/config"
	"github.com/justin06lee/henri/internal/node"
)

// version is overridden at build time with -ldflags "-X main.version=...".
var version = "0.1.0"

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
		return cmdSend()
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
  join <code>     join an existing group using its join code
  code            print this group's join code
  daemon          run the sync daemon in the foreground
  status          show what the local daemon is doing
  peers           list known devices; ` + "`peers add|rm <host:port>`" + ` to edit
  send            re-send the current clipboard to the group
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
	force := fs.Bool("force", false, "overwrite an existing config")
	if err := fs.Parse(args); err != nil {
		return err
	}

	path, err := config.Path()
	if err != nil {
		return err
	}
	if _, err := os.Stat(path); err == nil && !*force {
		return fmt.Errorf("a config already exists at %s (use -force to replace it, which leaves this device's current group)", path)
	}

	cfg, err := config.New(*name)
	if err != nil {
		return err
	}
	cfg.ListenPort = *port
	cfg.Discovery = *discovery
	if err := cfg.Save(); err != nil {
		return err
	}

	code, err := joinCode(cfg)
	if err != nil {
		return err
	}
	fmt.Printf("Started a new clipboard group.\n\n")
	fmt.Printf("  config   %s\n", path)
	fmt.Printf("  device   %s\n", cfg.DeviceName)
	fmt.Printf("  group    %s\n\n", cfg.GroupID)
	fmt.Printf("Run this on every other device to join:\n\n  henri join %s\n\n", code)
	fmt.Printf("That code is the group's secret key. Anyone holding it can read\n")
	fmt.Printf("everything you copy, so send it over something you trust.\n\n")
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
	if fs.NArg() != 1 {
		return errors.New("usage: henri join <code>")
	}

	path, err := config.Path()
	if err != nil {
		return err
	}
	if _, err := os.Stat(path); err == nil && !*force {
		return fmt.Errorf("a config already exists at %s (use -force to replace it)", path)
	}

	cfg, err := parseJoinCode(fs.Arg(0))
	if err != nil {
		return err
	}
	// A new device ID: the group is shared, the identity is not.
	fresh, err := config.New(*name)
	if err != nil {
		return err
	}
	cfg.DeviceID = fresh.DeviceID
	cfg.DeviceName = *name
	cfg.ListenPort = *port
	cfg.Discovery = *discovery
	cfg.PollMillis = config.DefaultPollMillis
	cfg.MaxBytes = config.DefaultMaxBytes
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
	code, err := joinCode(cfg)
	if err != nil {
		return err
	}
	fmt.Println(code)
	return nil
}

// joinCodePayload is the minimum another device needs to enter the group.
type joinCodePayload struct {
	G string `json:"g"`
	K string `json:"k"`
	P int    `json:"p,omitempty"`
}

func joinCode(cfg *config.Config) (string, error) {
	raw, err := json.Marshal(joinCodePayload{G: cfg.GroupID, K: cfg.Key, P: cfg.ListenPort})
	if err != nil {
		return "", err
	}
	return codePrefix + base64.RawURLEncoding.EncodeToString(raw), nil
}

func parseJoinCode(code string) (*config.Config, error) {
	code = strings.TrimSpace(code)
	if !strings.HasPrefix(code, codePrefix) {
		return nil, fmt.Errorf("that does not look like a join code (it should start with %q)", codePrefix)
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(code, codePrefix))
	if err != nil {
		return nil, fmt.Errorf("the join code is damaged: %w", err)
	}
	var p joinCodePayload
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, fmt.Errorf("the join code is damaged: %w", err)
	}
	return &config.Config{GroupID: p.G, Key: p.K, Discovery: true}, nil
}

// --- daemon ----------------------------------------------------------------

func cmdDaemon(args []string) error {
	fs := flag.NewFlagSet("daemon", flag.ContinueOnError)
	verbose := fs.Bool("verbose", false, "log every decision, including rejected connections")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := config.Load()
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
	fmt.Printf("  clipboard  %s\n", st.Tool)
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

func cmdSend() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if _, err := node.Query(cfg, node.KindPush); err != nil {
		return err
	}
	fmt.Println("Sent the current clipboard to the group.")
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
		return "just now"
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
