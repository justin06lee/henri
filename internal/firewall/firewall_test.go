package firewall

import (
	"net"
	"strings"
	"testing"
	"time"
)

func TestVerdictReadsRealToolOutput(t *testing.T) {
	// Verbatim shapes from the tools themselves.
	const firewalldPorts = "47600/tcp 47601/udp"
	const firewalldRich = `rule family="ipv4" source address="192.168.1.0/24" port port="47600" protocol="tcp" accept`
	const ufwActive = `Status: active

To                         Action      From
--                         ------      ----
47600/tcp                  ALLOW       192.168.1.0/24`
	const ufwDeny = `Status: active

To                         Action      From
--                         ------      ----
47600/tcp                  DENY        Anywhere`

	cases := []struct {
		name  string
		out   string
		port  int
		proto string
		want  Verdict
	}{
		{"firewalld --list-ports", firewalldPorts, 47600, "tcp", Allowed},
		{"firewalld --list-ports udp", firewalldPorts, 47601, "udp", Allowed},
		{"firewalld rich rule", firewalldRich, 47600, "tcp", Allowed},
		{"ufw allow", ufwActive, 47600, "tcp", Allowed},
		{"ufw missing udp", ufwActive, 47601, "udp", Unknown},
		// A DENY line names the port too. Reading that as "allowed" is how a
		// doctor tells someone their firewall is fine while it drops the
		// traffic, so the deny keywords have to win.
		{"ufw deny is not an allow", ufwDeny, 47600, "tcp", Unknown},
		{"nothing at all", "", 47600, "tcp", Unknown},
	}
	for _, tc := range cases {
		if got := verdictFor(tc.out, tc.port, tc.proto); got != tc.want {
			t.Errorf("%s: verdictFor(...) = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestBlockingTreatsUnknownAsWorthMentioning(t *testing.T) {
	// An inactive firewall is never the problem, whatever we could not read.
	if (Status{Name: "ufw", Active: false, TCP: Unknown, UDP: Unknown}).Blocking() {
		t.Error("an inactive firewall must not be reported as blocking")
	}
	// An active one whose rules henri could not read is exactly the case worth
	// raising: ufw refuses to answer a non-root user, and staying quiet there
	// is what leaves someone debugging henri instead of their firewall.
	if !(Status{Name: "ufw", Active: true, TCP: Unknown, UDP: Unknown}).Blocking() {
		t.Error("an active firewall with unreadable rules must be reported")
	}
	if (Status{Name: "ufw", Active: true, TCP: Allowed, UDP: Allowed}).Blocking() {
		t.Error("an active firewall with both ports open must not be reported")
	}
	if !(Status{Name: "ufw", Active: true, TCP: Allowed, UDP: Blocked}).Blocking() {
		t.Error("one closed port is still blocking")
	}
}

func TestGeneratedRulesAreScopedToTheLan(t *testing.T) {
	for _, c := range firewalldCmds(47600, 47601, "192.168.1.0/24") {
		joined := strings.Join(c, " ")
		if strings.Contains(joined, "add-rich-rule") && !strings.Contains(joined, "192.168.1.0/24") {
			t.Errorf("firewalld rule is not scoped to the LAN: %s", joined)
		}
	}
	for _, c := range ufwCmds(47600, 47601, "192.168.1.0/24") {
		joined := strings.Join(c, " ")
		if !strings.Contains(joined, "192.168.1.0/24") {
			t.Errorf("ufw rule is not scoped to the LAN: %s", joined)
		}
	}
	// With no LAN to scope to, the rules still have to be valid rather than
	// carrying an empty address.
	for _, c := range firewalldCmds(47600, 47601, "") {
		if strings.Contains(strings.Join(c, " "), `address=""`) {
			t.Errorf("unscoped rule carries an empty address: %v", c)
		}
	}
}

func TestBothPortsAreAlwaysOpened(t *testing.T) {
	// Opening only TCP is a trap: the clipboard would transfer once discovery
	// somehow worked, and discovery never would.
	for _, cmds := range [][][]string{
		firewalldCmds(47600, 47601, "192.168.1.0/24"),
		firewalldCmds(47600, 47601, ""),
		ufwCmds(47600, 47601, "192.168.1.0/24"),
		ufwCmds(47600, 47601, ""),
	} {
		all := strings.Join(flatten(cmds), " ")
		if !strings.Contains(all, "47600") || !strings.Contains(all, "tcp") {
			t.Errorf("tcp port missing from %v", cmds)
		}
		if !strings.Contains(all, "47601") || !strings.Contains(all, "udp") {
			t.Errorf("udp port missing from %v", cmds)
		}
	}
}

func flatten(cmds [][]string) []string {
	var out []string
	for _, c := range cmds {
		out = append(out, c...)
	}
	return out
}

func TestReachDistinguishesRefusedFromDropped(t *testing.T) {
	// A closed port on a host that is up: the connection is refused straight
	// away. That is the answer that says "a firewall rejected it, or henri is
	// not running there" rather than "the packet went nowhere".
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close() // nothing is listening now

	ok, reason := Reach(addr, 2*time.Second)
	if ok {
		t.Fatalf("expected %s to be unreachable", addr)
	}
	if !strings.Contains(reason, "refused") {
		t.Errorf("closed port should report a refusal, got %q", reason)
	}

	// And a listener that is up must come back clean, or the doctor would
	// blame a firewall for a working connection.
	ln2, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln2.Close()
	if ok, reason := Reach(ln2.Addr().String(), 2*time.Second); !ok {
		t.Errorf("a live listener should be reachable, got %q", reason)
	}
}

func TestLocalNetworkIsACidrOrNothing(t *testing.T) {
	// Whatever this machine's networking looks like, the answer has to be
	// something the firewall tools will accept.
	lan := LocalNetwork()
	if lan == "" {
		t.Skip("no private IPv4 network on this machine")
	}
	ip, ipnet, err := net.ParseCIDR(lan)
	if err != nil {
		t.Fatalf("LocalNetwork() = %q, which is not a CIDR: %v", lan, err)
	}
	if !ip.Equal(ipnet.IP) {
		t.Errorf("LocalNetwork() = %q, want the network address, not a host in it", lan)
	}
}

func TestDetectDoesNotInventAFirewall(t *testing.T) {
	// Detect shells out to whatever is installed here, so the only thing worth
	// asserting is that it stays self-consistent: no commands to run unless
	// something is actually active, and never a verdict without a name.
	st := Detect(47600, 47601, LocalNetwork())
	if st.Name == "" && (st.Active || len(st.OpenCmds) > 0) {
		t.Errorf("unnamed firewall reported as active or fixable: %+v", st)
	}
	if !st.Active && len(st.OpenCmds) > 0 {
		t.Errorf("inactive firewall should need no commands: %+v", st)
	}
}
