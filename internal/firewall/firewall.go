// Package firewall finds the host firewall and works out whether it is the
// reason henri cannot be reached.
//
// This exists because of how the failure presents. A firewall only filters
// inbound traffic, so a blocked device still announces itself perfectly well
// and still pushes its own clipboard to everyone else -- it is only the traffic
// coming back that disappears. What the user sees is a device that shows up in
// `henri peers` on both machines and syncs in exactly one direction, which
// looks like a bug in henri and is not one. Naming the firewall, and printing
// the one command that opens the ports, turns that into a thirty-second fix.
package firewall

import (
	"context"
	"fmt"
	"net"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// probeTimeout bounds one call to a firewall tool. These shell out to things
// that occasionally talk to a system bus, and `henri doctor` hanging with no
// output is worse than an incomplete answer.
const probeTimeout = 5 * time.Second

// Verdict is what we could establish about a rule. Unknown is a real and common
// answer: reading a firewall's configuration is not always possible without
// root, and guessing would be worse than admitting it.
type Verdict int

const (
	Unknown Verdict = iota
	Allowed
	Blocked
)

func (v Verdict) String() string {
	switch v {
	case Allowed:
		return "allowed"
	case Blocked:
		return "blocked"
	default:
		return "unknown"
	}
}

// Status describes the host firewall and what it does with henri's ports.
type Status struct {
	// Name is the firewall henri found, or "" when there is none it recognises.
	Name string
	// Active is whether that firewall is actually enforcing anything.
	Active bool
	// TCP and UDP are what we could determine about henri's two ports.
	TCP, UDP Verdict
	// OpenCmds are the commands that would open both ports, in order. Empty
	// when nothing needs opening or when henri does not know how to say it for
	// this firewall.
	OpenCmds [][]string
	// NeedsRoot is whether OpenCmds have to run as root.
	NeedsRoot bool
	// Note carries anything the user should know that the fields above cannot
	// express -- most often that henri could not read the rules.
	Note string
}

// Blocking reports whether the firewall is a plausible cause of henri being
// unreachable. Unknown counts: an unreadable ruleset on an active firewall is
// exactly the situation worth mentioning.
func (s Status) Blocking() bool {
	if !s.Active {
		return false
	}
	return s.TCP != Allowed || s.UDP != Allowed
}

// Detect finds the host firewall and inspects it for henri's ports.
//
// lan is the local network in CIDR form, used to scope the generated rules to
// the network henri actually runs on rather than opening the ports to
// everything. An empty lan falls back to unscoped rules.
func Detect(tcpPort, udpPort int, lan string) Status {
	switch runtime.GOOS {
	case "linux", "freebsd":
		if st, ok := detectFirewalld(tcpPort, udpPort, lan); ok {
			return st
		}
		if st, ok := detectUFW(tcpPort, udpPort, lan); ok {
			return st
		}
		if st, ok := detectNftables(tcpPort, udpPort); ok {
			return st
		}
		if st, ok := detectIptables(tcpPort, udpPort); ok {
			return st
		}
		return Status{}
	case "darwin":
		return detectMacOS()
	default:
		return Status{}
	}
}

// --- firewalld -------------------------------------------------------------

// firewalld is the default on Fedora and common on Arch, and it rejects rather
// than drops -- which is why a blocked henri reports "connection refused"
// immediately instead of timing out, and why it reads like nothing is
// listening on the other end.
func detectFirewalld(tcpPort, udpPort int, lan string) (Status, bool) {
	if !have("firewall-cmd") {
		return Status{}, false
	}
	st := Status{Name: "firewalld", NeedsRoot: true}
	state, err := run("firewall-cmd", "--state")
	if err != nil && !strings.Contains(state, "running") {
		// Installed but not running blocks nothing.
		return Status{Name: "firewalld", Active: false}, true
	}
	st.Active = strings.Contains(state, "running")
	if !st.Active {
		return st, true
	}

	ports, perr := run("firewall-cmd", "--list-ports")
	rich, rerr := run("firewall-cmd", "--list-rich-rules")
	if perr != nil && rerr != nil {
		st.TCP, st.UDP = Unknown, Unknown
		st.Note = "henri could not read the firewalld rules; run `sudo firewall-cmd --list-all` to check them yourself"
	} else {
		all := ports + "\n" + rich
		st.TCP = verdictFor(all, tcpPort, "tcp")
		st.UDP = verdictFor(all, udpPort, "udp")
	}

	if st.TCP != Allowed || st.UDP != Allowed {
		st.OpenCmds = firewalldCmds(tcpPort, udpPort, lan)
	}
	return st, true
}

func firewalldCmds(tcpPort, udpPort int, lan string) [][]string {
	if lan == "" {
		return [][]string{
			{"firewall-cmd", "--permanent", "--add-port=" + strconv.Itoa(tcpPort) + "/tcp"},
			{"firewall-cmd", "--permanent", "--add-port=" + strconv.Itoa(udpPort) + "/udp"},
			{"firewall-cmd", "--reload"},
		}
	}
	// Scoped to the local network on purpose: henri has no business being
	// reachable from anywhere else, and a rich rule says so precisely.
	rich := func(port int, proto string) string {
		return fmt.Sprintf(`rule family="ipv4" source address="%s" port port="%d" protocol="%s" accept`,
			lan, port, proto)
	}
	return [][]string{
		{"firewall-cmd", "--permanent", "--add-rich-rule=" + rich(tcpPort, "tcp")},
		{"firewall-cmd", "--permanent", "--add-rich-rule=" + rich(udpPort, "udp")},
		{"firewall-cmd", "--reload"},
	}
}

// --- ufw -------------------------------------------------------------------

func detectUFW(tcpPort, udpPort int, lan string) (Status, bool) {
	if !have("ufw") {
		return Status{}, false
	}
	st := Status{Name: "ufw", NeedsRoot: true}
	out, err := run("ufw", "status")
	if err != nil {
		// ufw refuses to report status to a non-root user, which is the usual
		// case here. Say so rather than pretending to know.
		st.Active = true
		st.TCP, st.UDP = Unknown, Unknown
		st.Note = "henri could not read the ufw rules without root; run `sudo ufw status` to check them"
		st.OpenCmds = ufwCmds(tcpPort, udpPort, lan)
		return st, true
	}
	st.Active = strings.Contains(strings.ToLower(out), "status: active")
	if !st.Active {
		return st, true
	}
	st.TCP = verdictFor(out, tcpPort, "tcp")
	st.UDP = verdictFor(out, udpPort, "udp")
	if st.TCP != Allowed || st.UDP != Allowed {
		st.OpenCmds = ufwCmds(tcpPort, udpPort, lan)
	}
	return st, true
}

func ufwCmds(tcpPort, udpPort int, lan string) [][]string {
	if lan == "" {
		return [][]string{
			{"ufw", "allow", strconv.Itoa(tcpPort) + "/tcp"},
			{"ufw", "allow", strconv.Itoa(udpPort) + "/udp"},
		}
	}
	return [][]string{
		{"ufw", "allow", "from", lan, "to", "any", "port", strconv.Itoa(tcpPort), "proto", "tcp"},
		{"ufw", "allow", "from", lan, "to", "any", "port", strconv.Itoa(udpPort), "proto", "udp"},
	}
}

// --- nftables and iptables -------------------------------------------------

// A bare nftables or iptables setup is hand-written, so henri reads it to see
// whether the ports appear but never offers to edit it. Generating rules for a
// ruleset we do not understand risks putting them in the wrong chain, or
// after a catch-all drop where they do nothing.
func detectNftables(tcpPort, udpPort int) (Status, bool) {
	if !have("nft") {
		return Status{}, false
	}
	out, err := run("nft", "list", "ruleset")
	if err != nil {
		return Status{
			Name: "nftables", Active: true, TCP: Unknown, UDP: Unknown,
			Note: "henri could not read the nftables ruleset without root; run `sudo nft list ruleset` to check it",
		}, true
	}
	if strings.TrimSpace(out) == "" {
		return Status{Name: "nftables", Active: false}, true
	}
	return Status{
		Name: "nftables", Active: true,
		TCP: verdictFor(out, tcpPort, "tcp"), UDP: verdictFor(out, udpPort, "udp"),
		Note: "henri does not edit a hand-written nftables ruleset; add accept rules for these ports yourself",
	}, true
}

func detectIptables(tcpPort, udpPort int) (Status, bool) {
	if !have("iptables") {
		return Status{}, false
	}
	out, err := run("iptables", "-S")
	if err != nil {
		return Status{
			Name: "iptables", Active: true, TCP: Unknown, UDP: Unknown,
			Note: "henri could not read the iptables rules without root; run `sudo iptables -S` to check them",
		}, true
	}
	if !strings.Contains(out, "-P INPUT DROP") && !strings.Contains(out, "-P INPUT REJECT") {
		// A default-accept INPUT chain is not blocking anything henri cares
		// about, whatever else is in there.
		return Status{Name: "iptables", Active: false}, true
	}
	return Status{
		Name: "iptables", Active: true,
		TCP: verdictFor(out, tcpPort, "tcp"), UDP: verdictFor(out, udpPort, "udp"),
		Note: "henri does not edit a hand-written iptables ruleset; add accept rules for these ports yourself",
	}, true
}

// --- macOS -----------------------------------------------------------------

// macOS filters by application rather than by port, and it prompts the user the
// first time a program listens. There is nothing to open, so this only reports
// the two states worth knowing about.
func detectMacOS() Status {
	const fw = "/usr/libexec/ApplicationFirewall/socketfilterfw"
	out, err := run(fw, "--getglobalstate")
	if err != nil {
		return Status{}
	}
	st := Status{Name: "macOS application firewall", Active: strings.Contains(out, "State = 1")}
	if !st.Active {
		return st
	}
	if blockAll, err := run(fw, "--getblockall"); err == nil && strings.Contains(blockAll, "set to enabled") {
		st.TCP, st.UDP = Blocked, Blocked
		st.Note = "\"Block all incoming connections\" is on, which stops henri receiving anything; " +
			"turn it off in System Settings > Network > Firewall"
		return st
	}
	// Per-application filtering, and henri is allowed to listen or it would not
	// have got this far. Nothing to open.
	st.TCP, st.UDP = Allowed, Allowed
	return st
}

// --- shared ----------------------------------------------------------------

// verdictFor looks for a port in a firewall tool's own output. It is
// deliberately literal: it answers Allowed only when the port and protocol both
// appear on one line, and Unknown rather than Blocked when they do not, because
// every one of these tools has more ways to permit traffic than henri can
// parse -- a service definition, a zone, a policy, an interface rule.
func verdictFor(out string, port int, proto string) Verdict {
	p := strconv.Itoa(port)
	for _, line := range strings.Split(out, "\n") {
		l := strings.ToLower(line)
		if strings.Contains(l, "deny") || strings.Contains(l, "reject") || strings.Contains(l, "drop") {
			continue
		}
		if strings.Contains(l, p) && strings.Contains(l, proto) {
			return Allowed
		}
	}
	return Unknown
}

func have(bin string) bool {
	_, err := exec.LookPath(bin)
	return err == nil
}

func run(name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// LocalNetwork returns the CIDR of the network this machine is on, so generated
// rules can be scoped to it. It returns "" when there is no obvious answer,
// which the callers treat as "do not scope".
func LocalNetwork() string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return ""
	}
	for _, ifi := range ifaces {
		if ifi.Flags&net.FlagUp == 0 || ifi.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := ifi.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok || ipnet.IP.To4() == nil {
				continue
			}
			if !ipnet.IP.IsPrivate() {
				continue
			}
			return (&net.IPNet{IP: ipnet.IP.Mask(ipnet.Mask), Mask: ipnet.Mask}).String()
		}
	}
	return ""
}

// Reach reports what happens when something tries to open a TCP connection to
// addr, in the words the user needs.
//
// The distinction it draws is the one that matters: a refusal means the packet
// arrived and was turned away, so the host is up and either nothing is
// listening or a firewall is rejecting; a timeout means the packet vanished,
// which is a firewall dropping or a network that cannot carry it.
func Reach(addr string, timeout time.Duration) (ok bool, reason string) {
	conn, err := net.DialTimeout("tcp", addr, timeout)
	if err == nil {
		conn.Close()
		return true, "reachable"
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, "refused"):
		return false, "refused — the host answered but turned the connection away, " +
			"which is a firewall rejecting it or henri not running there"
	case strings.Contains(msg, "timeout"), strings.Contains(msg, "timed out"), strings.Contains(msg, "deadline"):
		return false, "no answer — the connection went nowhere, which is a firewall dropping it " +
			"or a network that does not carry traffic between these devices"
	case strings.Contains(msg, "no route"):
		return false, "no route to that address"
	default:
		return false, msg
	}
}
