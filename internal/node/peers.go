package node

import (
	"sort"
	"sync"
	"time"
)

// peerTTL is how long a discovered peer survives without a beacon. Peers listed
// in the config never expire.
const peerTTL = 3 * time.Minute

type peer struct {
	device   string
	name     string
	addr     string
	lastSeen time.Time
	static   bool
}

// peerSet is the daemon's view of the group. Discovered peers are keyed by
// device ID; statically configured peers are keyed by address, since we do not
// know their device ID until they talk to us.
type peerSet struct {
	mu sync.RWMutex
	m  map[string]*peer
}

func newPeerSet(static []string) *peerSet {
	ps := &peerSet{m: make(map[string]*peer)}
	for _, addr := range static {
		if addr == "" {
			continue
		}
		ps.m["static:"+addr] = &peer{addr: addr, static: true}
	}
	return ps
}

// seen records a beacon or an inbound connection from a device.
func (ps *peerSet) seen(device, name, addr string) {
	if device == "" || addr == "" {
		return
	}
	ps.mu.Lock()
	defer ps.mu.Unlock()
	p, ok := ps.m[device]
	if !ok {
		p = &peer{device: device}
		ps.m[device] = p
	}
	p.name = name
	p.addr = addr
	p.lastSeen = time.Now()
}

// addrs returns every address worth pushing a clipboard payload to.
func (ps *peerSet) addrs() []string {
	ps.mu.RLock()
	defer ps.mu.RUnlock()
	now := time.Now()
	seen := make(map[string]bool)
	var out []string
	for _, p := range ps.m {
		if !p.static && now.Sub(p.lastSeen) > peerTTL {
			continue
		}
		if seen[p.addr] {
			continue
		}
		seen[p.addr] = true
		out = append(out, p.addr)
	}
	sort.Strings(out)
	return out
}

// list renders the set for `henri peers`.
func (ps *peerSet) list() []PeerInfo {
	ps.mu.RLock()
	defer ps.mu.RUnlock()
	out := make([]PeerInfo, 0, len(ps.m))
	for _, p := range ps.m {
		info := PeerInfo{Device: p.device, Name: p.name, Addr: p.addr, Source: "discovered"}
		if p.static {
			info.Source = "config"
		}
		if !p.lastSeen.IsZero() {
			info.LastSeenAt = p.lastSeen.UnixMilli()
		}
		out = append(out, info)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Addr < out[j].Addr })
	return out
}

// prune drops discovered peers that have gone quiet.
func (ps *peerSet) prune() {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	now := time.Now()
	for k, p := range ps.m {
		if !p.static && now.Sub(p.lastSeen) > peerTTL {
			delete(ps.m, k)
		}
	}
}
