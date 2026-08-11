package node

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
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

// announce periodically broadcasts this device's presence to the group.
func (n *Node) announce(ctx context.Context) {
	group := &net.UDPAddr{IP: net.ParseIP(MulticastGroup), Port: n.cfg.DiscoveryPort}

	send := func() {
		msg := &Message{
			V:      ProtocolVersion,
			Kind:   KindHello,
			Device: n.cfg.DeviceID,
			Name:   n.cfg.DeviceName,
			TS:     time.Now().UnixMilli(),
			Port:   n.cfg.ListenPort,
		}
		plain, err := json.Marshal(msg)
		if err != nil {
			return
		}
		sealed, err := n.disco.Seal(plain)
		if err != nil {
			return
		}
		for _, src := range multicastSources() {
			conn, err := net.DialUDP("udp4", src, group)
			if err != nil {
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

// multicastSources returns one local address per multicast-capable interface,
// plus a nil entry that lets the kernel choose. Binding the source address is
// what steers the beacon out of a specific interface, which matters on laptops
// with Wi-Fi, Ethernet and a VPN up at once.
func multicastSources() []*net.UDPAddr {
	out := []*net.UDPAddr{nil}
	ifaces, err := net.Interfaces()
	if err != nil {
		return out
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
	return out
}

// listenBeacons joins the multicast group on every capable interface and
// records the devices it hears from.
func (n *Node) listenBeacons(ctx context.Context) {
	conns := n.joinMulticast()
	if len(conns) == 0 {
		n.log.Warn("discovery is on but no interface would join the multicast group; " +
			"add peers to the config instead")
		return
	}
	defer func() {
		for _, c := range conns {
			c.Close()
		}
	}()

	go func() {
		<-ctx.Done()
		for _, c := range conns {
			c.Close()
		}
	}()

	done := make(chan struct{})
	for _, c := range conns {
		go func(c *net.UDPConn) {
			defer func() {
				select {
				case done <- struct{}{}:
				default:
				}
			}()
			buf := make([]byte, maxBeacon)
			for {
				nb, src, err := c.ReadFromUDP(buf)
				if err != nil {
					if ctx.Err() != nil {
						return
					}
					var ne net.Error
					if errors.As(err, &ne) && ne.Timeout() {
						continue
					}
					return
				}
				n.handleBeacon(buf[:nb], src)
			}
		}(c)
	}

	select {
	case <-ctx.Done():
	case <-done:
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
			_ = conn.SetReadBuffer(maxBeacon * 16)
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
	addr := net.JoinHostPort(src.IP.String(), fmt.Sprint(msg.Port))
	n.peers.seen(msg.Device, msg.Name, addr)
}
