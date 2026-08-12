package node

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"
)

// MulticastGroup is an administratively scoped IPv4 multicast address, so
// beacons stay on the local network. Multicast is used rather than broadcast
// because it needs no SO_BROADCAST socket option, which keeps this file free of
// per-platform syscall code.
const MulticastGroup = "239.42.47.60"

// announceEvery is how often each device says hello.
const announceEvery = 10 * time.Second

// maxBeacon caps a UDP read. Beacons are tiny; anything larger is not ours.
const maxBeacon = 2048

// rejoinEvery and deafFor are how discovery survives a network that moves under
// it. A multicast membership is bound to an interface, and the kernel drops it
// when that interface goes: a Wi-Fi roam, a sleep, a DHCP renew, a VPN coming
// up. Nothing tells the socket, so it sits in ReadFromUDP for as long as the
// daemon runs while beacons carry on going out -- shouting, stone deaf. So
// membership is thrown away and taken out again on a timer, and sooner than
// that if nothing has been heard at all.
//
// They are variables rather than constants only so the tests can drive a whole
// round in milliseconds.
var (
	rejoinEvery = 60 * time.Second
	deafFor     = 40 * time.Second
)

// announce periodically broadcasts this device's presence to the group.
func (n *Node) announce(ctx context.Context) {
	group := &net.UDPAddr{IP: net.ParseIP(MulticastGroup), Port: n.cfg.DiscoveryPort}

	send := func() {
		for _, src := range multicastSources() {
			conn, err := net.DialUDP("udp4", src, group)
			if err != nil {
				continue
			}
			// One beacon per source address: the receiver checks a beacon
			// against the address it arrived from, and a laptop with Wi-Fi,
			// Ethernet and a VPN up announces from all three.
			sealed, err := n.beacon(conn.LocalAddr())
			if err != nil {
				conn.Close()
				continue
			}
			_ = conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
			_, _ = conn.Write(sealed)
			conn.Close()
		}
	}

	send() // say hello immediately rather than waiting a full interval
	tick := time.NewTicker(announceEvery)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			n.peers.prune()
			send()
		}
	}
}

// beacon seals one hello, stamped with the address it is about to leave from.
func (n *Node) beacon(local net.Addr) ([]byte, error) {
	msg := &Message{
		V:      ProtocolVersion,
		Kind:   KindHello,
		Device: n.cfg.DeviceID,
		Name:   n.cfg.DeviceName,
		TS:     time.Now().UnixMilli(),
		Port:   n.cfg.ListenPort,
	}
	if ua, ok := local.(*net.UDPAddr); ok && ua.IP != nil && !ua.IP.IsUnspecified() {
		msg.Addr = ua.IP.String()
	}
	plain, err := json.Marshal(msg)
	if err != nil {
		return nil, err
	}
	return n.disco.Seal(plain)
}

// multicastSources returns one local address per multicast-capable interface.
// Binding the source address is what steers the beacon out of a specific
// interface, which matters on laptops with Wi-Fi, Ethernet and a VPN up at
// once. Only when there is nothing to steer with does it fall back to a nil
// entry and let the kernel choose -- including it as well as the interfaces
// sends every beacon down the default route twice.
func multicastSources() []*net.UDPAddr {
	var out []*net.UDPAddr
	ifaces, err := net.Interfaces()
	if err != nil {
		return []*net.UDPAddr{nil}
	}
	for _, ifi := range ifaces {
		if ifi.Flags&net.FlagUp == 0 || ifi.Flags&net.FlagMulticast == 0 {
			continue
		}
		if ifi.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := ifi.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip4 := ipnet.IP.To4()
			if ip4 == nil || ip4.IsLoopback() || ip4.IsUnspecified() {
				continue
			}
			out = append(out, &net.UDPAddr{IP: ip4})
			break // one address per interface is enough
		}
	}
	if len(out) == 0 {
		return []*net.UDPAddr{nil}
	}
	return out
}

// listenBeacons keeps this device in the multicast group and records the
// devices it hears from. Membership is re-established round after round, so a
// network that changes shape under a long-running daemon costs a minute of
// discovery rather than every minute after it.
func (n *Node) listenBeacons(ctx context.Context) {
	warned := false
	for ctx.Err() == nil {
		started := time.Now()
		conns := n.join()
		if len(conns) == 0 {
			// Not fatal, and never permanent: under launchd the daemon starts
			// before Wi-Fi has associated, and there is simply nothing to join
			// yet. Say so once and keep asking.
			if !warned {
				n.log.Warn("discovery is on but no interface would join the multicast group; " +
					"still trying, and peers listed in the config are unaffected")
				warned = true
			}
			if !sleepFor(ctx, rejoinEvery) {
				return
			}
			continue
		}
		warned = false

		if n.readBeacons(ctx, conns) && time.Since(started) < time.Second {
			// Every socket died the moment it opened, so the interfaces are not
			// usable. Wait, rather than spinning on them.
			if !sleepFor(ctx, time.Second) {
				return
			}
		}
	}
}

// readBeacons reads from every socket until the round ends, then closes them
// and waits for the readers to stop. It reports whether the round ended because
// every reader died, which is worth backing off over.
func (n *Node) readBeacons(ctx context.Context, conns []*net.UDPConn) bool {
	closing := make(chan struct{})
	var wg sync.WaitGroup
	for _, c := range conns {
		wg.Add(1)
		go func(c *net.UDPConn) {
			defer wg.Done()
			buf := make([]byte, maxBeacon)
			for {
				nb, src, err := c.ReadFromUDP(buf)
				if err != nil {
					var ne net.Error
					if errors.As(err, &ne) && ne.Timeout() {
						continue
					}
					select {
					case <-closing:
					default:
						// One interface going away must not take the working
						// ones with it, which is what a shared done channel and
						// a deferred close-everything used to do.
						n.log.Warn("a discovery socket stopped reading",
							"local", c.LocalAddr(), "err", err)
					}
					return
				}
				n.handleBeacon(buf[:nb], src)
			}
		}(c)
	}

	dead := make(chan struct{})
	go func() { wg.Wait(); close(dead) }()

	allDied := n.watchRound(ctx, dead)
	close(closing)
	for _, c := range conns {
		c.Close()
	}
	wg.Wait()
	return allDied
}

// watchRound blocks until this round of membership should end: the re-join
// timer, every reader having died, or nothing being heard for deafFor.
func (n *Node) watchRound(ctx context.Context, dead <-chan struct{}) bool {
	started := time.Now()
	deadline := started.Add(rejoinEvery)

	// Checked several times a period so neither deadline is noticed late.
	every := min(deafFor, rejoinEvery) / 4
	if every < 10*time.Millisecond {
		every = 10 * time.Millisecond
	}
	tick := time.NewTicker(every)
	defer tick.Stop()

	for {
		select {
		case <-ctx.Done():
			return false
		case <-dead:
			return true
		case now := <-tick.C:
			last := n.heardAt()
			if last.Before(started) {
				last = started
			}
			if now.Sub(last) >= deafFor {
				// This is the safety net. A daemon whose membership was dropped
				// hears nothing at all -- not even its own beacon coming back
				// off the network -- and looks perfectly healthy while it does.
				if !n.deafWarned.Swap(true) {
					n.log.Warn("discovery has heard nothing on the network; re-joining the multicast group",
						"quiet_for", now.Sub(last).Round(time.Second))
				}
				return false
			}
			if now.After(deadline) {
				return false
			}
		}
	}
}

// sleepFor waits, and reports whether it got to the end rather than being cut
// short by shutdown.
func sleepFor(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// joinMulticast opens one listening socket per multicast-capable interface,
// falling back to letting the kernel pick.
func (n *Node) joinMulticast() []*net.UDPConn {
	group := &net.UDPAddr{IP: net.ParseIP(MulticastGroup), Port: n.cfg.DiscoveryPort}

	var conns []*net.UDPConn
	ifaces, err := net.Interfaces()
	if err == nil {
		for i := range ifaces {
			ifi := ifaces[i]
			if ifi.Flags&net.FlagUp == 0 || ifi.Flags&net.FlagMulticast == 0 {
				continue
			}
			if ifi.Flags&net.FlagLoopback != 0 {
				continue
			}
			conn, err := net.ListenMulticastUDP("udp4", &ifi, group)
			if err != nil {
				n.log.Debug("could not join multicast group", "iface", ifi.Name, "err", err)
				continue
			}
			conns = append(conns, conn)
		}
	}
	if len(conns) == 0 {
		if conn, err := net.ListenMulticastUDP("udp4", nil, group); err == nil {
			conns = append(conns, conn)
		} else {
			n.log.Debug("multicast listen failed", "err", err)
		}
	}
	return conns
}

// handleBeacon decrypts a hello and records the sender as a peer.
func (n *Node) handleBeacon(sealed []byte, src *net.UDPAddr) {
	if len(sealed) < nonceSize {
		return
	}
	plain, err := n.disco.Open(sealed)
	if err != nil {
		// Another henri group on the same LAN, or unrelated traffic on this
		// port. Either way it is not ours.
		return
	}
	var msg Message
	if err := json.Unmarshal(plain, &msg); err != nil {
		return
	}
	if msg.V != ProtocolVersion || msg.Kind != KindHello {
		return
	}
	// Anything that decrypts is proof the membership is alive, including this
	// device's own beacon coming back: that is what keeps a device alone on the
	// network from declaring itself deaf every forty seconds.
	n.heard.Store(time.Now().UnixMilli())
	n.deafWarned.Store(false)

	if msg.Device == n.cfg.DeviceID {
		return // our own beacon, echoed back by the network
	}
	if err := checkFresh(msg.TS); err != nil {
		n.log.Debug("stale beacon", "from", displayName(&msg), "err", err)
		return
	}
	if msg.Port <= 0 || msg.Port > 65535 {
		return
	}
	if msg.Addr != "" {
		// Only checked when it is there, so older devices keep working. When it
		// is there, a beacon whose claimed source is not where it came from is
		// a replay aimed at pointing this device's clipboard at somebody else.
		if claimed := net.ParseIP(msg.Addr); claimed == nil || !claimed.Equal(src.IP) {
			n.log.Debug("beacon does not come from the address it claims",
				"claimed", msg.Addr, "from", src.IP)
			return
		}
	}
	if !n.replay.fresh(sealed[:nonceSize]) {
		// Either a replay, or the same datagram arriving on two interfaces we
		// have both joined. Neither is worth acting on twice.
		return
	}

	addr := net.JoinHostPort(src.IP.String(), fmt.Sprint(msg.Port))
	n.peers.seen(msg.Device, msg.Name, addr)
	n.beacons.Add(1)
	n.lastBeacon.Store(time.Now().UnixMilli())
}

// heardAt is when anything at all was last heard on the discovery socket.
func (n *Node) heardAt() time.Time {
	ms := n.heard.Load()
	if ms == 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms)
}
