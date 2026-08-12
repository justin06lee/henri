package node

import (
	"context"
	"encoding/json"
	"net"
	"sync/atomic"
	"testing"
	"time"
)

func mustUDP(t *testing.T) *net.UDPConn {
	t.Helper()
	c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// sealBeacon builds a hello as another device would send it. An empty addr is
// what a device built before that field existed sends.
func sealBeacon(t *testing.T, n *Node, device, name string, port int, addr string) []byte {
	t.Helper()
	plain, err := json.Marshal(&Message{
		V: ProtocolVersion, Kind: KindHello, Device: device, Name: name,
		TS: time.Now().UnixMilli(), Port: port, Addr: addr,
	})
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := n.disco.Seal(plain)
	if err != nil {
		t.Fatal(err)
	}
	return sealed
}

func sendUDP(t *testing.T, to *net.UDPConn, payload []byte) {
	t.Helper()
	out, err := net.DialUDP("udp4", nil, to.LocalAddr().(*net.UDPAddr))
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	if _, err := out.Write(payload); err != nil {
		t.Fatal(err)
	}
}

// shortRounds makes a round of membership take milliseconds instead of a
// minute, so the loop that re-establishes it can be watched going round.
func shortRounds(t *testing.T, rejoin, deaf time.Duration) {
	t.Helper()
	oldRejoin, oldDeaf := rejoinEvery, deafFor
	rejoinEvery, deafFor = rejoin, deaf
	t.Cleanup(func() { rejoinEvery, deafFor = oldRejoin, oldDeaf })
}

// One interface going away must not take the working ones with it. It used to:
// the first reader to exit ended the whole thing and the deferred cleanup shut
// every socket, so one bad interface left the device deaf on all of them.
func TestOneDeadDiscoverySocketDoesNotStopTheOthers(t *testing.T) {
	n := offlineNode(t, "alpha")
	dead, alive := mustUDP(t), mustUDP(t)
	dead.Close() // already gone; its reader has nothing to do but exit

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); n.readBeacons(ctx, []*net.UDPConn{dead, alive}) }()

	sendUDP(t, alive, sealBeacon(t, n, "beta-id", "beta", 47600, "127.0.0.1"))
	waitFor(t, "the surviving socket to record the beacon", func() bool { return n.beacons.Load() == 1 })

	cancel()
	<-done
}

// Nothing to join yet is the normal case under launchd: the daemon starts
// before Wi-Fi has associated. Giving up permanently leaves it deaf for the
// life of the process.
func TestDiscoveryKeepsTryingWhenNothingWillJoin(t *testing.T) {
	shortRounds(t, 100*time.Millisecond, 10*time.Second)
	n := offlineNode(t, "alpha")

	var joins atomic.Int64
	n.join = func() []*net.UDPConn {
		if joins.Add(1) == 1 {
			return nil // no usable interface yet
		}
		return []*net.UDPConn{mustUDP(t)}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); n.listenBeacons(ctx) }()

	waitFor(t, "discovery to try joining again", func() bool { return joins.Load() >= 3 })
	cancel()
	<-done
}

// The failure that started all this: membership silently dropped, the socket
// still open, nothing ever arriving again. Hearing nothing at all has to be
// treated as a fault rather than as quiet.
func TestDiscoveryRejoinsWhenItGoesDeaf(t *testing.T) {
	shortRounds(t, 10*time.Second, 150*time.Millisecond)
	n := offlineNode(t, "alpha")

	var joins atomic.Int64
	n.join = func() []*net.UDPConn {
		joins.Add(1)
		return []*net.UDPConn{mustUDP(t)}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); n.listenBeacons(ctx) }()

	waitFor(t, "discovery to re-join after hearing nothing", func() bool { return joins.Load() >= 3 })
	if !n.deafWarned.Load() {
		t.Fatal("discovery went deaf without saying so")
	}
	cancel()
	<-done
}

// A device on the old build sends no address, and must keep working exactly as
// it did.
func TestBeaconWithoutAnAddressIsStillAccepted(t *testing.T) {
	n := offlineNode(t, "alpha")
	src := &net.UDPAddr{IP: net.ParseIP("192.168.1.9"), Port: 47601}

	n.handleBeacon(sealBeacon(t, n, "beta-id", "beta", 47600, ""), src)
	if got := n.peers.addrs(); len(got) != 1 || got[0] != "192.168.1.9:47600" {
		t.Fatalf("peers are %v, want beta at the address the beacon came from", got)
	}
}

// A captured beacon replayed from somewhere else would otherwise point every
// payload at whoever replayed it: handleBeacon takes the address from the
// datagram, not from the message.
func TestReplayedBeaconCannotReHomeAPeer(t *testing.T) {
	n := offlineNode(t, "alpha")
	src := &net.UDPAddr{IP: net.ParseIP("192.168.1.9"), Port: 47601}
	sealed := sealBeacon(t, n, "beta-id", "beta", 47600, "192.168.1.9")

	n.handleBeacon(sealed, src)
	if n.beacons.Load() != 1 {
		t.Fatalf("the beacon was not accepted (%d)", n.beacons.Load())
	}
	n.handleBeacon(sealed, src)
	if got := n.beacons.Load(); got != 1 {
		t.Fatalf("the same beacon was accepted %d times", got)
	}

	elsewhere := &net.UDPAddr{IP: net.ParseIP("192.168.1.66"), Port: 47601}
	n.handleBeacon(sealed, elsewhere)
	if got := n.peers.addrs(); len(got) != 1 || got[0] != "192.168.1.9:47600" {
		t.Fatalf("a replayed beacon moved beta to %v", got)
	}
}

// The address a beacon claims is the address it is sent from, per interface --
// a laptop with Wi-Fi, Ethernet and a VPN up announces from all three.
func TestBeaconCarriesTheAddressItLeavesFrom(t *testing.T) {
	n := offlineNode(t, "alpha")
	sealed, err := n.beacon(&net.UDPAddr{IP: net.ParseIP("192.168.1.20"), Port: 5000})
	if err != nil {
		t.Fatal(err)
	}
	plain, err := n.disco.Open(sealed)
	if err != nil {
		t.Fatal(err)
	}
	var msg Message
	if err := json.Unmarshal(plain, &msg); err != nil {
		t.Fatal(err)
	}
	if msg.Addr != "192.168.1.20" {
		t.Fatalf("the beacon claims %q, want the address it is sent from", msg.Addr)
	}
	if msg.Port != n.cfg.ListenPort {
		t.Fatalf("the beacon advertises port %d, want %d", msg.Port, n.cfg.ListenPort)
	}
}

// An unspecified source means the kernel picked, and there is nothing to claim.
func TestBeaconClaimsNothingWhenTheSourceIsUnknown(t *testing.T) {
	n := offlineNode(t, "alpha")
	sealed, err := n.beacon(&net.UDPAddr{IP: net.IPv4zero, Port: 5000})
	if err != nil {
		t.Fatal(err)
	}
	plain, _ := n.disco.Open(sealed)
	var msg Message
	if err := json.Unmarshal(plain, &msg); err != nil {
		t.Fatal(err)
	}
	if msg.Addr != "" {
		t.Fatalf("the beacon claims %q from an unspecified source", msg.Addr)
	}
}
