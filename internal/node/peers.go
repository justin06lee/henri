package node

import (
	"slices"
	"strings"
	"sync"
	"time"
)

// peerTTL is how long a discovered peer survives without a beacon. Peers listed
// in the config never expire.
const peerTTL = 3 * time.Minute

// maxPeers caps the set. Beacons are authenticated, so this is not a defence
// against strangers; it is a bound on a daemon that runs for months on a big
// network and would otherwise remember every device it ever heard.
const maxPeers = 256

type peer struct {
	device string
	name   string
	// addr is where the device was last heard from. configAddr is what the
	// config says, if anything. A device can have both -- written down and
	// found on the network -- and it is still one device.
	addr       string
	configAddr string
	lastSeen   time.Time
}

func (p *peer) static() bool { return p.configAddr != "" }

// dialAddr is the one address worth sending to. A device that has gone quiet
// falls back to whatever the config says, which never expires: it may still be
// reachable where the user put it.
func (p *peer) dialAddr(now time.Time) string {
	if p.addr != "" && now.Sub(p.lastSeen) <= peerTTL {
		return p.addr
	}
	return p.configAddr
}

// lastAddr is the best address to show, expired or not.
func (p *peer) lastAddr() string {
	if p.addr != "" {
		return p.addr
	}
	return p.configAddr
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
		ps.m[staticKey(addr)] = &peer{addr: addr, configAddr: addr}
	}
	return ps
}

func staticKey(addr string) string { return "static:" + addr }

// seen records a beacon or an inbound connection from a device.
func (ps *peerSet) seen(device, name, addr string) {
	if device == "" || addr == "" {
		return
	}
	ps.mu.Lock()
	defer ps.mu.Unlock()

	p := ps.m[device]
	if p == nil {
		ps.makeRoom()
		p = &peer{device: device}
		ps.m[device] = p
	}
	p.name = name
	p.addr = addr
	p.lastSeen = time.Now()

	// A device that is both in the config and on the network is one device.
	// Left as two entries it gets every payload twice and is listed twice.
	if st := ps.m[staticKey(addr)]; st != nil {
		p.configAddr = st.configAddr
		delete(ps.m, staticKey(addr))
	}
}

// answered records that a push to a configured address was replied to by a
// device. It is the only thing that can tie the two together: the config
// usually holds a hostname and discovery only ever learns an IP, so nothing
// else says that laptop.local:47600 and 192.168.1.9:47600 are one machine.
func (ps *peerSet) answered(addr, device, name string) {
	if addr == "" || device == "" {
		return
	}
	ps.mu.Lock()
	defer ps.mu.Unlock()

	key := staticKey(addr)
	st := ps.m[key]
	if st == nil {
		return // not a configured address, so it is already keyed by device
	}
	delete(ps.m, key)
	p := ps.m[device]
	if p == nil {
		// First contact: re-key the config entry under the device it turned out
		// to be, so a later beacon updates this entry instead of adding one.
		st.device = device
		if name != "" {
			st.name = name
		}
		ps.m[device] = st
		return
	}
	p.configAddr = st.configAddr
	if p.name == "" {
		p.name = name
	}
}

// addrs returns every address worth pushing a clipboard payload to.
func (ps *peerSet) addrs() []string {
	ps.mu.RLock()
	defer ps.mu.RUnlock()
	now := time.Now()
	seen := make(map[string]bool)
	var out []string
	for _, p := range ps.m {
		addr := p.dialAddr(now)
		if addr == "" || seen[addr] {
			continue
		}
		seen[addr] = true
		out = append(out, addr)
	}
	slices.Sort(out)
	return out
}

// list renders the set for `henri peers`.
func (ps *peerSet) list() []PeerInfo {
	ps.mu.RLock()
	defer ps.mu.RUnlock()
	out := make([]PeerInfo, 0, len(ps.m))
	for _, p := range ps.m {
		info := PeerInfo{Device: p.device, Name: p.name, Addr: p.lastAddr(), Source: "discovered"}
		if p.static() {
			info.Source = "config"
		}
		if !p.lastSeen.IsZero() {
			info.LastSeenAt = p.lastSeen.UnixMilli()
		}
		out = append(out, info)
	}
	slices.SortFunc(out, func(a, b PeerInfo) int { return strings.Compare(a.Addr, b.Addr) })
	return out
}

// prune drops discovered peers that have gone quiet.
func (ps *peerSet) prune() {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	now := time.Now()
	for k, p := range ps.m {
		if !p.static() && now.Sub(p.lastSeen) > peerTTL {
			delete(ps.m, k)
		}
	}
}

// makeRoom drops an entry when the set is full: an expired one for preference,
// otherwise the device heard from longest ago. Configured peers stay, because
// the user asked for them by name. The caller holds the lock.
func (ps *peerSet) makeRoom() {
	if len(ps.m) < maxPeers {
		return
	}
	now := time.Now()
	for k, p := range ps.m {
		if !p.static() && now.Sub(p.lastSeen) > peerTTL {
			delete(ps.m, k)
		}
	}
	if len(ps.m) < maxPeers {
		return
	}
	var oldest string
	var at time.Time
	for k, p := range ps.m {
		if p.static() {
			continue
		}
		if oldest == "" || p.lastSeen.Before(at) {
			oldest, at = k, p.lastSeen
		}
	}
	if oldest != "" {
		delete(ps.m, oldest)
	}
}
