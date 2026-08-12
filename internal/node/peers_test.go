package node

import (
	"fmt"
	"testing"
	"time"
)

func TestStaticPeersAreAlwaysDialled(t *testing.T) {
	ps := newPeerSet([]string{"10.0.0.5:47600", ""})
	addrs := ps.addrs()
	if len(addrs) != 1 || addrs[0] != "10.0.0.5:47600" {
		t.Fatalf("addrs() gave %v, want the one configured peer", addrs)
	}
	ps.prune()
	if len(ps.addrs()) != 1 {
		t.Fatal("prune dropped a statically configured peer")
	}
}

func TestDiscoveredPeersExpire(t *testing.T) {
	ps := newPeerSet(nil)
	ps.seen("dev-1", "laptop", "192.168.1.9:47600")
	if len(ps.addrs()) != 1 {
		t.Fatal("a freshly seen peer was not returned")
	}

	ps.mu.Lock()
	ps.m["dev-1"].lastSeen = time.Now().Add(-2 * peerTTL)
	ps.mu.Unlock()

	if got := ps.addrs(); len(got) != 0 {
		t.Fatalf("a peer silent for %s was still dialled: %v", 2*peerTTL, got)
	}
	ps.prune()
	if len(ps.list()) != 0 {
		t.Fatal("prune left an expired peer behind")
	}
}

func TestSeenUpdatesAddress(t *testing.T) {
	ps := newPeerSet(nil)
	ps.seen("dev-1", "laptop", "192.168.1.9:47600")
	ps.seen("dev-1", "laptop", "192.168.1.22:47600") // moved networks
	addrs := ps.addrs()
	if len(addrs) != 1 || addrs[0] != "192.168.1.22:47600" {
		t.Fatalf("addrs() gave %v, want only the new address", addrs)
	}
}

func TestSeenIgnoresIncompleteEntries(t *testing.T) {
	ps := newPeerSet(nil)
	ps.seen("", "laptop", "192.168.1.9:47600")
	ps.seen("dev-1", "laptop", "")
	if got := ps.list(); len(got) != 0 {
		t.Fatalf("incomplete peers were recorded: %+v", got)
	}
}

func TestListLabelsSource(t *testing.T) {
	ps := newPeerSet([]string{"10.0.0.5:47600"})
	ps.seen("dev-1", "laptop", "192.168.1.9:47600")
	var static, discovered int
	for _, p := range ps.list() {
		switch p.Source {
		case "config":
			static++
		case "discovered":
			discovered++
		}
	}
	if static != 1 || discovered != 1 {
		t.Fatalf("list() labelled sources wrong: %+v", ps.list())
	}
}

// A device that is both written down in the config and found on the network is
// one device. Two entries means every payload goes to it twice and `henri
// peers` lists it twice.
func TestConfiguredPeerIsReconciledWhenDiscovered(t *testing.T) {
	ps := newPeerSet([]string{"192.168.1.9:47600"})
	ps.seen("dev-1", "laptop", "192.168.1.9:47600")

	if got := ps.addrs(); len(got) != 1 {
		t.Fatalf("addrs() gave %v, want the one machine once", got)
	}
	list := ps.list()
	if len(list) != 1 {
		t.Fatalf("list() gave %+v, want one entry", list)
	}
	if list[0].Device != "dev-1" || list[0].Source != "config" {
		t.Fatalf("the reconciled entry is %+v, want dev-1 labelled config", list[0])
	}
	// And it still never expires, because the user asked for it by name.
	ps.mu.Lock()
	ps.m["dev-1"].lastSeen = time.Now().Add(-2 * peerTTL)
	ps.mu.Unlock()
	ps.prune()
	if got := ps.addrs(); len(got) != 1 || got[0] != "192.168.1.9:47600" {
		t.Fatalf("addrs() gave %v after the peer went quiet, want the configured address", got)
	}
}

// The config usually holds a hostname and discovery only ever learns an IP, so
// the only thing that can tie them together is who answers.
func TestAnsweringTiesAConfiguredAddressToItsDevice(t *testing.T) {
	ps := newPeerSet([]string{"laptop.local:47600"})
	ps.seen("dev-1", "laptop", "192.168.1.9:47600")
	if got := ps.addrs(); len(got) != 2 {
		t.Fatalf("addrs() gave %v; nothing has said these are the same machine yet", got)
	}

	ps.answered("laptop.local:47600", "dev-1", "laptop")
	got := ps.addrs()
	if len(got) != 1 || got[0] != "192.168.1.9:47600" {
		t.Fatalf("addrs() gave %v, want the one machine at the address it announced", got)
	}
	if list := ps.list(); len(list) != 1 || list[0].Source != "config" {
		t.Fatalf("list() gave %+v, want one entry labelled config", list)
	}
}

func TestPeerSetStaysBounded(t *testing.T) {
	ps := newPeerSet([]string{"10.0.0.5:47600"})
	for i := range maxPeers * 2 {
		ps.seen(fmt.Sprintf("dev-%d", i), "device", fmt.Sprintf("10.1.0.%d:47600", i%250))
	}
	ps.mu.RLock()
	size := len(ps.m)
	ps.mu.RUnlock()
	if size > maxPeers {
		t.Fatalf("the set grew to %d entries, past its cap of %d", size, maxPeers)
	}
	// The configured peer is never the one thrown away.
	for _, p := range ps.list() {
		if p.Addr == "10.0.0.5:47600" {
			return
		}
	}
	t.Fatal("the configured peer was evicted to make room for discovered ones")
}
