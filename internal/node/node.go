// Package node runs the henri daemon: it watches the local clipboard, pushes
// every change to the group over authenticated encryption, and applies
// changes that arrive from peers.
package node

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/justin06lee/henri/internal/clipboard"
	"github.com/justin06lee/henri/internal/config"
	"github.com/justin06lee/henri/internal/secure"
)

// freshness bounds how far a message's timestamp may drift from ours before we
// treat it as a replay. Generous enough to survive un-synced clocks.
const freshness = 2 * time.Minute

// dialTimeout bounds a single push to one peer.
const dialTimeout = 4 * time.Second

// Node is a running henri daemon.
type Node struct {
	cfg   *config.Config
	sync  *secure.Box
	disco *secure.Box
	peers *peerSet
	clip  Clipboard
	log   *slog.Logger

	startedAt time.Time

	mu        sync.Mutex
	lastHash  string // fingerprint of the clipboard content currently in sync
	lastBytes int
	lastFrom  string
	lastSync  time.Time
	clipErr   string // why the last clipboard read failed, if it did
	watchMode string // how changes are noticed: an event source, or polling

	sent     atomic.Int64
	received atomic.Int64
}

// New builds a Node backed by the real system clipboard.
func New(cfg *config.Config, log *slog.Logger) (*Node, error) {
	if err := clipboard.Available(); err != nil {
		return nil, err
	}
	return NewWith(cfg, log, systemClipboard{})
}

// NewWith builds a Node over an arbitrary clipboard implementation.
func NewWith(cfg *config.Config, log *slog.Logger, clip Clipboard) (*Node, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	master, err := cfg.MasterKey()
	if err != nil {
		return nil, err
	}
	syncBox, err := secure.NewBox(master, cfg.GroupID, secure.InfoSync)
	if err != nil {
		return nil, err
	}
	discoBox, err := secure.NewBox(master, cfg.GroupID, secure.InfoDiscovery)
	if err != nil {
		return nil, err
	}
	return &Node{
		cfg:       cfg,
		sync:      syncBox,
		disco:     discoBox,
		peers:     newPeerSet(cfg.Peers),
		clip:      clip,
		log:       log,
		startedAt: time.Now(),
	}, nil
}

// Run blocks until ctx is cancelled or a listener fails fatally.
func (n *Node) Run(ctx context.Context) error {
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", n.cfg.ListenPort))
	if err != nil {
		return fmt.Errorf("listen on port %d: %w", n.cfg.ListenPort, err)
	}
	defer ln.Close()

	n.log.Info("henri is watching",
		"device", n.cfg.DeviceName,
		"group", n.cfg.GroupID,
		"port", n.cfg.ListenPort,
		"clipboard", n.clip.Name(),
		"discovery", n.cfg.Discovery)

	// Seed lastHash with whatever is already on the clipboard so starting the
	// daemon does not immediately broadcast stale content.
	if data, err := n.clip.Read(); err == nil && len(data) > 0 {
		n.mu.Lock()
		n.lastHash = secure.Hash(data)
		n.lastBytes = len(data)
		n.mu.Unlock()
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var wg sync.WaitGroup
	errc := make(chan error, 1)

	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := n.serve(ctx, ln); err != nil && ctx.Err() == nil {
			select {
			case errc <- err:
			default:
			}
		}
	}()

	wg.Add(1)
	go func() { defer wg.Done(); n.watch(ctx) }()

	if n.cfg.Discovery {
		wg.Add(1)
		go func() { defer wg.Done(); n.listenBeacons(ctx) }()
		wg.Add(1)
		go func() { defer wg.Done(); n.announce(ctx) }()
	}

	var runErr error
	select {
	case <-ctx.Done():
	case runErr = <-errc:
	}
	cancel()
	ln.Close()
	wg.Wait()
	return runErr
}

// watch notices local copies and pushes them to the group.
//
// Given an event source it waits on that and touches the clipboard only when
// something actually changed; otherwise it falls back to checking on a timer.
func (n *Node) watch(ctx context.Context) {
	polling := time.Duration(n.cfg.PollMillis) * time.Millisecond
	interval := polling

	var events <-chan struct{}
	if !n.cfg.PollOnly {
		var kind string
		events, kind = clipboard.Events(ctx)
		if events != nil {
			n.setWatchMode(kind)
			n.log.Info("watching for clipboard changes", "via", kind)
			// With a real event source there is no reason to poll hard. Keep a
			// slow tick purely so a dropped event cannot leave this device out
			// of sync indefinitely.
			interval = 30 * time.Second
		} else {
			n.setWatchMode("polling every " + polling.String())
			n.log.Debug("no clipboard event source; polling", "every", polling)
		}
	} else {
		n.setWatchMode("polling every " + polling.String() + " (events disabled)")
	}

	tick := time.NewTicker(interval)
	defer tick.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case _, ok := <-events:
			if !ok {
				// The event source stopped. Go back to polling rather than
				// sitting silently and syncing nothing.
				events = nil
				tick.Reset(polling)
				n.setWatchMode("polling every " + polling.String() + " (event source stopped)")
				n.log.Warn("clipboard event source stopped; polling instead", "every", polling)
				continue
			}
		case <-tick.C:
		}
		n.checkClipboard(ctx)
	}
}

func (n *Node) setWatchMode(mode string) {
	n.mu.Lock()
	n.watchMode = mode
	n.mu.Unlock()
}

// checkClipboard reads the clipboard once and pushes it if it is new.
func (n *Node) checkClipboard(ctx context.Context) {
	data, err := n.clip.Read()
	if err != nil {
		n.log.Debug("clipboard read failed", "err", err)
		n.mu.Lock()
		first := n.clipErr == ""
		n.clipErr = err.Error()
		n.mu.Unlock()
		// Worth saying out loud once: a service started outside the graphical
		// session finds the helper but cannot reach the display, and would
		// otherwise just sit there syncing nothing.
		if first {
			n.log.Warn("cannot read the clipboard", "err", err)
		}
		return
	}
	n.mu.Lock()
	recovered := n.clipErr != ""
	n.clipErr = ""
	n.mu.Unlock()
	if recovered {
		n.log.Info("clipboard is readable again")
	}

	if len(data) == 0 {
		return
	}
	if len(data) > n.cfg.MaxBytes {
		// Record the hash anyway so we do not warn about it every time.
		h := secure.Hash(data)
		n.mu.Lock()
		skip := n.lastHash == h
		n.lastHash = h
		n.mu.Unlock()
		if !skip {
			n.log.Warn("clipboard too large to sync", "bytes", len(data), "limit", n.cfg.MaxBytes)
		}
		return
	}

	h := secure.Hash(data)
	n.mu.Lock()
	if h == n.lastHash {
		n.mu.Unlock()
		return
	}
	n.lastHash = h
	n.lastBytes = len(data)
	n.lastFrom = "local"
	n.lastSync = time.Now()
	n.mu.Unlock()

	n.push(ctx, data, h)
}

// push sends one clipboard payload to every known peer, concurrently.
func (n *Node) push(ctx context.Context, data []byte, hash string) {
	addrs := n.peers.addrs()
	if len(addrs) == 0 {
		n.log.Debug("copied, but no peers are known yet", "bytes", len(data))
		return
	}
	msg := &Message{
		V:      ProtocolVersion,
		Kind:   KindClip,
		Device: n.cfg.DeviceID,
		Name:   n.cfg.DeviceName,
		TS:     time.Now().UnixMilli(),
		Hash:   hash,
		Data:   data,
	}

	var wg sync.WaitGroup
	var ok atomic.Int64
	for _, addr := range addrs {
		wg.Add(1)
		go func(addr string) {
			defer wg.Done()
			if err := n.request(ctx, addr, msg, nil); err != nil {
				n.log.Debug("push failed", "peer", addr, "err", err)
				return
			}
			ok.Add(1)
		}(addr)
	}
	wg.Wait()

	n.sent.Add(1)
	n.log.Info("sent clipboard", "bytes", len(data), "peers", ok.Load(), "of", len(addrs))
}

// request dials a peer, sends one message and optionally decodes the reply.
func (n *Node) request(ctx context.Context, addr string, msg *Message, reply **Message) error {
	d := net.Dialer{Timeout: dialTimeout}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(dialTimeout + 10*time.Second))

	if err := writeFrame(conn, n.sync, msg); err != nil {
		return err
	}
	resp, err := readFrame(conn, n.sync)
	if err != nil {
		return err
	}
	if resp.Err != "" {
		return errors.New(resp.Err)
	}
	if reply != nil {
		*reply = resp
	}
	return nil
}

// serve accepts inbound peer connections.
func (n *Node) serve(ctx context.Context, ln net.Listener) error {
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			return err
		}
		go n.handle(conn)
	}
}

func (n *Node) handle(conn net.Conn) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))

	msg, err := readFrame(conn, n.sync)
	if err != nil {
		if !errors.Is(err, io.EOF) {
			// A failure here is usually a port scan or a device with the wrong
			// join code. Log quietly; it is not actionable.
			n.log.Debug("rejected connection", "remote", conn.RemoteAddr().String(), "err", err)
		}
		return
	}

	reply := &Message{V: ProtocolVersion, Device: n.cfg.DeviceID, Name: n.cfg.DeviceName, TS: time.Now().UnixMilli()}

	switch msg.Kind {
	case KindClip:
		if err := n.applyClip(msg, conn.RemoteAddr()); err != nil {
			reply.Kind = KindAck
			reply.Err = err.Error()
		} else {
			reply.Kind = KindAck
		}
	case KindStatus:
		reply.Kind = KindState
		reply.State = n.state()
	case KindPush:
		reply.Kind = KindAck
		if err := n.pushCurrent(); err != nil {
			reply.Err = err.Error()
		}
	default:
		reply.Kind = KindAck
		reply.Err = fmt.Sprintf("unknown message kind %q", msg.Kind)
	}

	if err := writeFrame(conn, n.sync, reply); err != nil {
		n.log.Debug("reply failed", "remote", conn.RemoteAddr().String(), "err", err)
	}
}

// applyClip validates an inbound clipboard payload and writes it locally.
func (n *Node) applyClip(msg *Message, remote net.Addr) error {
	if msg.Device == n.cfg.DeviceID {
		return nil // our own push, looped back
	}
	if err := checkFresh(msg.TS); err != nil {
		return err
	}
	if len(msg.Data) == 0 {
		return errors.New("empty payload")
	}
	if len(msg.Data) > n.cfg.MaxBytes {
		return fmt.Errorf("payload of %d bytes exceeds this device's limit of %d", len(msg.Data), n.cfg.MaxBytes)
	}
	// The hash is advisory, but a mismatch means something is wrong upstream.
	if got := secure.Hash(msg.Data); msg.Hash != "" && got != msg.Hash {
		return errors.New("payload does not match its hash")
	}
	hash := secure.Hash(msg.Data)

	n.mu.Lock()
	if hash == n.lastHash {
		n.mu.Unlock()
		return nil // already in sync; do not bounce it back
	}
	// Claim the hash before writing so the watch loop sees the new content as
	// already-known and does not re-broadcast it.
	n.lastHash = hash
	n.lastBytes = len(msg.Data)
	n.lastFrom = displayName(msg)
	n.lastSync = time.Now()
	n.mu.Unlock()

	if err := n.clip.Write(msg.Data); err != nil {
		return err
	}
	if host, _, err := net.SplitHostPort(remote.String()); err == nil && msg.Port > 0 {
		n.peers.seen(msg.Device, msg.Name, net.JoinHostPort(host, fmt.Sprint(msg.Port)))
	}
	n.received.Add(1)
	n.log.Info("received clipboard", "from", displayName(msg), "bytes", len(msg.Data))
	return nil
}

// pushCurrent re-sends whatever is on the clipboard right now.
func (n *Node) pushCurrent() error {
	data, err := n.clip.Read()
	if err != nil {
		return err
	}
	if len(data) == 0 {
		return errors.New("clipboard is empty")
	}
	if len(data) > n.cfg.MaxBytes {
		return fmt.Errorf("clipboard is %d bytes, over the %d byte limit", len(data), n.cfg.MaxBytes)
	}
	h := secure.Hash(data)
	n.mu.Lock()
	n.lastHash = h
	n.lastBytes = len(data)
	n.lastFrom = "local"
	n.lastSync = time.Now()
	n.mu.Unlock()
	n.push(context.Background(), data, h)
	return nil
}

func (n *Node) state() *State {
	n.mu.Lock()
	lastHash, lastBytes, lastFrom, lastSync := n.lastHash, n.lastBytes, n.lastFrom, n.lastSync
	clipErr, watchMode := n.clipErr, n.watchMode
	n.mu.Unlock()

	st := &State{
		Device:       n.cfg.DeviceID,
		Name:         n.cfg.DeviceName,
		Group:        n.cfg.GroupID,
		PID:          os.Getpid(),
		StartedAt:    n.startedAt.UnixMilli(),
		ListenPort:   n.cfg.ListenPort,
		Discovery:    n.cfg.Discovery,
		Tool:         n.clip.Name(),
		ClipboardErr: clipErr,
		WatchMode:    watchMode,
		LastHash:     lastHash,
		LastBytes:    lastBytes,
		LastFrom:     lastFrom,
		Sent:         n.sent.Load(),
		Received:     n.received.Load(),
		Peers:        n.peers.list(),
	}
	if !lastSync.IsZero() {
		st.LastSyncAt = lastSync.UnixMilli()
	}
	return st
}

func checkFresh(ts int64) error {
	if ts == 0 {
		return errors.New("message has no timestamp")
	}
	skew := time.Since(time.UnixMilli(ts))
	if skew < 0 {
		skew = -skew
	}
	if skew > freshness {
		return fmt.Errorf("message is %s out of date; check the clocks on both devices", skew.Round(time.Second))
	}
	return nil
}

func displayName(msg *Message) string {
	if msg.Name != "" {
		return msg.Name
	}
	return msg.Device
}
