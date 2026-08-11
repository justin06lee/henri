package node

import (
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
