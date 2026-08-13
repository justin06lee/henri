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

	"strings"

	"github.com/justin06lee/henri/internal/clipboard"
	"github.com/justin06lee/henri/internal/config"
	"github.com/justin06lee/henri/internal/pack"
	"github.com/justin06lee/henri/internal/secure"
)

// freshness bounds how far a message's timestamp may drift from ours before we
// treat it as a replay. Generous enough to survive un-synced clocks.
const freshness = 2 * time.Minute

// dialTimeout bounds a single push to one peer.
const dialTimeout = 4 * time.Second

// maxHandlers bounds how many inbound connections are served at once. Anyone
// who can reach the port can open sockets faster than we can close them, and
// each one costs a goroutine and a buffer before it has proved anything.
const maxHandlers = 48

// maxReplay is how many recently seen frame nonces are remembered at once.
const maxReplay = 4096

// pendingRetry and pendingFor govern a copy that reached nobody. Discovery can
// take an announce interval to turn up the first peer, and a laptop that has
// just woken takes longer, so the copy is offered again -- but backing off, and
// not for ever: what the user wants on their other device is what they copied a
// moment ago, not what they copied before lunch.
//
// Variables rather than constants only so the tests need not wait seconds.
var (
	pendingRetry = 5 * time.Second
	pendingFor   = 2 * time.Minute
)

// Node is a running henri daemon.
type Node struct {
	cfg    *config.Config
	sync   *secure.Box
	disco  *secure.Box
	peers  *peerSet
	clip   Clipboard
	log    *slog.Logger
	replay *replayCache
	// sem bounds concurrent connection handlers.
	sem chan struct{}
	// join is how listenBeacons gets its sockets. A field so the tests can hand
	// it sockets that have nothing to do with multicast.
	join func() []*net.UDPConn

	startedAt time.Time

	// clipMu serialises everything that touches the system clipboard, so the
	// watcher can never read it halfway through somebody else's write.
	clipMu sync.Mutex

	// receiveDir is where incoming files are unpacked; empty when it could
	// not be resolved, which fails file payloads and nothing else.
	receiveDir string

	mu         sync.Mutex
	lastHash   string // fingerprint of the clipboard content currently in sync
	lastBytes  int
	lastFrom   string
	lastFormat string // what the last sync was: text (empty), files, or an image
	lastCount  int    // how many files, when lastFormat says files
	lastSync   time.Time
	// pendingHash is content that has been copied but has not reached anybody
	// yet. It is deliberately not lastHash: calling it synced is how a copy
	// made before the first peer turned up used to disappear for good.
	pendingHash  string
	pendingSince time.Time
	pendingTried time.Time
	pendingWait  time.Duration
	// skippedHash is content too large to sync. Kept apart from lastHash so
	// `henri status` does not describe one payload with another's size.
	skippedHash string
	clipErr     string // why the last clipboard read failed, if it did
	watchMode   string // how changes are noticed: an event source, or polling

	sent       atomic.Int64
	received   atomic.Int64
	beacons    atomic.Int64
	lastBeacon atomic.Int64 // unix millis of the last beacon from another device
	heard      atomic.Int64 // unix millis of the last beacon of any kind
	deafWarned atomic.Bool
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
	if log == nil {
		// Finding out about this on the first line the daemon logs is no way to
		// find out about it.
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	// Work from a copy. A zero poll interval panics time.NewTicker and a zero
	// payload limit refuses every copy silently; neither is worth taking the
	// daemon down for, or going quiet over, whatever handed us the config.
	local := *cfg
	if local.PollMillis <= 0 {
		local.PollMillis = config.DefaultPollMillis
	}
	if local.MaxBytes <= 0 {
		local.MaxBytes = config.DefaultMaxBytes
	}
	if local.MaxFileBytes <= 0 {
		local.MaxFileBytes = config.DefaultMaxFileBytes
	}
	cfg = &local

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
	n := &Node{
		cfg:       cfg,
		sync:      syncBox,
		disco:     discoBox,
		peers:     newPeerSet(cfg.Peers),
		clip:      clip,
		log:       log,
		replay:    newReplayCache(maxReplay),
		sem:       make(chan struct{}, maxHandlers),
		startedAt: time.Now(),
	}
	if dir, err := cfg.ReceivePath(); err == nil {
		n.receiveDir = dir
	} else {
		// Text still syncs; only file payloads need somewhere to land.
		log.Warn("no directory to receive files into; file payloads will be refused", "err", err)
	}
	n.join = n.joinMulticast
	return n, nil
}

// Run blocks until ctx is cancelled or a listener fails fatally.
func (n *Node) Run(ctx context.Context) error {
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", n.cfg.ListenPort))
	if err != nil {
		return fmt.Errorf("listen on port %d: %w", n.cfg.ListenPort, err)
	}

	n.log.Info("henri is watching",
		"device", n.cfg.DeviceName,
		"group", n.cfg.GroupID,
		"port", n.cfg.ListenPort,
		"clipboard", n.clip.Name(),
		"discovery", n.cfg.Discovery)

	// Seed lastHash with whatever is already on the clipboard so starting the
	// daemon does not immediately broadcast stale content -- files and images
	// included. A read that fails here is the interesting case -- a daemon
	// started outside the graphical session -- and used to be thrown away,
	// leaving `henri status` claiming everything was fine until the first
	// poll tick.
	if snap, err := n.clip.Look(); err != nil {
		n.noteClipErr(err)
	} else if fp, size, _ := n.fingerprint(snap); fp != "" {
		n.mu.Lock()
		n.lastHash = fp
		n.lastBytes = size
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
	// Accept only comes back when the listener is closed, so this has to happen
	// before waiting on the goroutines rather than in a defer after it.
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
		} else if clipboard.PollingStealsFocus() {
			// Reading costs keyboard focus here, so polling would flicker the
			// screen a few times a second. Watching nothing is the better of
			// two bad options; `henri send`, on a key, costs one read when
			// asked for.
			n.setWatchMode("press-to-send (this compositor cannot be watched in the background)")
			n.log.Warn("this compositor hands the clipboard only to the focused window, " +
				"so henri cannot watch it in the background without making the screen flicker. " +
				"Copies will not sync until you press a key: run `henri hotkey install`, " +
				"or set clipboard_poll_only in the config to poll anyway")
			<-ctx.Done()
			return
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
		// announce prunes too, but discovery may be switched off entirely and
		// then nothing else would ever drop a peer that has gone.
		n.peers.prune()
		n.checkClipboard(ctx)
	}
}

func (n *Node) setWatchMode(mode string) {
	n.mu.Lock()
	n.watchMode = mode
	n.mu.Unlock()
}

// noteClipErr records a failing clipboard read, and says so out loud once. A
// service started outside the graphical session finds the helper but cannot
// reach the display, and would otherwise sit there syncing nothing.
func (n *Node) noteClipErr(err error) {
	n.log.Debug("clipboard read failed", "err", err)
	n.mu.Lock()
	first := n.clipErr == ""
	n.clipErr = err.Error()
	n.mu.Unlock()
	if first {
		n.log.Warn("cannot read the clipboard", "err", err)
	}
}

func (n *Node) noteClipOK() {
	n.mu.Lock()
	recovered := n.clipErr != ""
	n.clipErr = ""
	n.mu.Unlock()
	if recovered {
		n.log.Info("clipboard is readable again")
	}
}

// fingerprint is the local identity of whatever the clipboard holds, plus a
// size for the caps and the status page. An empty fingerprint means there is
// nothing to sync.
//
// Text and images fingerprint their bytes. Files fingerprint their paths with
// each one's size and modify time -- deliberately not their contents, because
// hashing a folder of gigabytes two and a half times a second is not a
// watcher, it is a space heater. Copying the same paths after editing a file
// re-syncs; editing deep inside a copied folder may not, and a fresh copy is
// the answer there.
func (n *Node) fingerprint(snap clipboard.Snapshot) (fp string, size int, err error) {
	switch snap.Kind {
	case clipboard.ContentFiles:
		if len(snap.Paths) == 0 {
			return "", 0, nil
		}
		var b strings.Builder
		b.WriteString("files\x00")
		for _, p := range snap.Paths {
			info, err := os.Stat(p)
			if err != nil {
				// The copy outlived its files; nothing worth syncing.
				return "", 0, err
			}
			fmt.Fprintf(&b, "%s\x00%d\x00%d\x00", p, info.Size(), info.ModTime().UnixNano())
			if info.Mode().IsRegular() {
				size += int(info.Size())
			}
		}
		return secure.Hash([]byte(b.String())), size, nil
	case clipboard.ContentImage:
		if len(snap.Image) == 0 {
			return "", 0, nil
		}
		return secure.Hash(snap.Image), len(snap.Image), nil
	default:
		if len(snap.Text) == 0 {
			return "", 0, nil
		}
		return secure.Hash(snap.Text), len(snap.Text), nil
	}
}

// payload is one clipboard's worth of content on its way to the group.
type payload struct {
	format string   // FormatText, FormatFiles or FormatImage
	data   []byte   // the text, the archive, or the PNG
	names  []string // FormatFiles: the archive's top-level names
	fp     string   // the local fingerprint claimed when the push lands
}

// build turns a snapshot into what actually travels. For files that means
// archiving them now, at send time, so a retry archives the files as they are
// rather than as they were.
func (n *Node) build(snap clipboard.Snapshot, fp string) (payload, error) {
	switch snap.Kind {
	case clipboard.ContentFiles:
		data, names, err := pack.Build(snap.Paths, n.cfg.MaxFileBytes)
		if err != nil {
			return payload{}, err
		}
		return payload{format: FormatFiles, data: data, names: names, fp: fp}, nil
	case clipboard.ContentImage:
		return payload{format: FormatImage, data: snap.Image, fp: fp}, nil
	default:
		return payload{format: FormatText, data: snap.Text, fp: fp}, nil
	}
}

// describe is how a payload reads in a log line.
func describe(format string, count int) string {
	switch format {
	case FormatFiles:
		if count == 1 {
			return "1 file"
		}
		return fmt.Sprintf("%d files", count)
	case FormatImage:
		return "an image"
	default:
		return "text"
	}
}

// checkClipboard reads the clipboard once and pushes it if it is new.
func (n *Node) checkClipboard(ctx context.Context) {
	// Held across the read and the decision that follows it. applyClip holds
	// the same lock across its claim and its write, so the watcher can never
	// catch the moment where the fingerprint says one thing and the clipboard
	// still holds another -- which it would read as a local copy, and send.
	n.clipMu.Lock()

	snap, err := n.clip.Look()
	if err != nil {
		n.clipMu.Unlock()
		n.noteClipErr(err)
		return
	}
	n.noteClipOK()

	fp, size, err := n.fingerprint(snap)
	if err != nil || fp == "" {
		// Nothing to sync, or a file copy whose files are already gone.
		n.clipMu.Unlock()
		return
	}
	now := time.Now()

	// The early size gate. Text and images know their size exactly; a file
	// copy knows the raw total, which is what pack.Build enforces too.
	limit := n.cfg.MaxBytes
	if snap.Kind != clipboard.ContentText {
		limit = n.cfg.MaxFileBytes
	}
	if size > limit {
		n.mu.Lock()
		warn := n.skippedHash != fp
		n.skippedHash = fp
		n.pendingHash = "" // whatever was waiting is not on the clipboard now
		n.mu.Unlock()
		n.clipMu.Unlock()
		if warn {
			n.log.Warn("clipboard too large to sync", "bytes", size, "limit", limit)
		}
		return
	}

	n.mu.Lock()
	n.skippedHash = ""
	if fp == n.lastHash {
		n.mu.Unlock()
		n.clipMu.Unlock()
		return
	}
	first := n.pendingHash != fp
	switch {
	case first:
		n.pendingHash, n.pendingSince, n.pendingTried, n.pendingWait = fp, now, now, pendingRetry
	case now.Sub(n.pendingSince) > pendingFor:
		// Long enough. Stop offering a device that has only just appeared
		// something the user copied several minutes ago.
		n.lastHash, n.lastBytes, n.lastFrom, n.lastSync = fp, size, "local", now
		n.pendingHash = ""
		n.mu.Unlock()
		n.clipMu.Unlock()
		n.log.Warn("gave up on a copy that reached no peer", "bytes", size)
		return
	case now.Sub(n.pendingTried) < n.pendingWait:
		n.mu.Unlock()
		n.clipMu.Unlock()
		return
	default:
		n.pendingTried = now
		n.pendingWait *= 2
	}
	prev := n.lastHash
	n.mu.Unlock()
	n.clipMu.Unlock()

	p, err := n.build(snap, fp)
	if err != nil {
		// Archiving said no -- the files grew past the limit, vanished, or
		// cannot be read. Warn once per copy rather than once per tick.
		n.mu.Lock()
		warn := n.skippedHash != fp
		n.skippedHash, n.pendingHash = fp, ""
		n.mu.Unlock()
		if warn {
			n.log.Warn("cannot send the copied files", "err", err)
		}
		return
	}

	sent, tried := n.push(ctx, p)
	if sent == 0 {
		// Leaving lastHash alone is the whole point: it is what makes the next
		// tick try this content again instead of calling it synced.
		if first && tried == 0 {
			n.log.Warn("copied, but no peer is known yet; will keep trying",
				"what", describe(p.format, len(p.names)), "bytes", len(p.data))
		}
		return
	}

	n.mu.Lock()
	// Something may have written the clipboard while the push was in flight.
	// That content is newer than this, so leave its fingerprint alone.
	if n.lastHash == prev {
		n.lastHash, n.lastBytes, n.lastFrom, n.lastSync = fp, size, "local", now
		n.lastFormat, n.lastCount = p.format, len(p.names)
	}
	if n.pendingHash == fp {
		n.pendingHash = ""
	}
	n.mu.Unlock()
}

// push sends one clipboard payload to every known peer, concurrently. It
// returns how many peers took it and how many were tried.
func (n *Node) push(ctx context.Context, p payload) (sent, tried int) {
	addrs := n.peers.addrs()
	if len(addrs) == 0 {
		n.log.Debug("copied, but no peers are known yet", "bytes", len(p.data))
		return 0, 0
	}
	msg := &Message{
		// Text stays v1 so older devices keep receiving it; files and images
		// go as v2, which an old device refuses whole instead of pasting an
		// archive as if it were text.
		V:      versionFor(p.format),
		Kind:   KindClip,
		Device: n.cfg.DeviceID,
		Name:   n.cfg.DeviceName,
		TS:     time.Now().UnixMilli(),
		// Without this the far end has no way to learn where we live, and sync
		// silently runs one way the moment discovery stops working. Devices
		// that predate it ignore the field.
		Port:   n.cfg.ListenPort,
		Hash:   secure.Hash(p.data),
		Data:   p.data,
		Format: p.format,
		Names:  p.names,
	}

	var wg sync.WaitGroup
	var ok atomic.Int64
	var once sync.Once
	var firstErr error
	for _, addr := range addrs {
		wg.Add(1)
		go func(addr string) {
			defer wg.Done()
			resp, err := n.request(ctx, addr, msg)
			if err != nil {
				n.log.Debug("push failed", "peer", addr, "err", err)
				once.Do(func() { firstErr = err })
				return
			}
			ok.Add(1)
			// Whoever answered is the only thing that can say a configured
			// address and a discovered device are the same machine.
			if resp.Device != "" && resp.Device != n.cfg.DeviceID {
				n.peers.answered(addr, resp.Device, resp.Name)
			}
		}(addr)
	}
	wg.Wait()

	sent = int(ok.Load())
	if sent == 0 {
		n.log.Warn("the clipboard reached none of the known peers",
			"peers", len(addrs), "err", firstErr)
		return 0, len(addrs)
	}
	n.sent.Add(1)
	n.log.Info("sent clipboard", "what", describe(p.format, len(p.names)),
		"bytes", len(p.data), "peers", sent, "of", len(addrs))
	return sent, len(addrs)
}

// request dials a peer, sends one message and returns the reply.
func (n *Node) request(ctx context.Context, addr string, msg *Message) (*Message, error) {
	d := net.Dialer{Timeout: dialTimeout}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(dialTimeout + 10*time.Second))

	if err := writeFrame(conn, n.sync, msg); err != nil {
		return nil, err
	}
	resp, _, err := readFrame(conn, n.sync, n.frameCap())
	if err != nil {
		return nil, err
	}
	if resp.Err != "" {
		return nil, errors.New(resp.Err)
	}
	return resp, nil
}

// frameCap is the biggest inbound frame this device would ever agree to: its
// own payload limits, whichever is larger, plus encoding overhead.
func (n *Node) frameCap() int {
	m := n.cfg.MaxBytes
	if n.cfg.MaxFileBytes > m {
		m = n.cfg.MaxFileBytes
	}
	return frameLimit(m)
}

// serve accepts inbound peer connections.
//
// It comes back only when the listener is closed or the daemon is shutting
// down. Treating every accept error as fatal meant running out of file
// descriptors -- a passing condition -- killed the daemon, and under launchd or
// systemd that turns into a crash loop.
func (n *Node) serve(ctx context.Context, ln net.Listener) error {
	var handlers sync.WaitGroup
	defer handlers.Wait()

	var delay time.Duration
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			// Back off and try again, the way net/http does. Running out of
			// file descriptors is a passing condition, and taking the daemon
			// down for it just hands the crash loop to launchd.
			delay = 2*delay + 5*time.Millisecond
			if delay > time.Second {
				delay = time.Second
			}
			n.log.Warn("could not accept a connection; retrying", "err", err, "in", delay)
			if !sleepFor(ctx, delay) {
				return nil
			}
			continue
		}
		delay = 0

		select {
		case n.sem <- struct{}{}:
			handlers.Add(1)
			go func() {
				defer handlers.Done()
				defer func() { <-n.sem }()
				n.handle(ctx, conn)
			}()
		default:
			// At the ceiling. Dropping the connection is the only answer that
			// does not let whoever is opening them decide how much memory this
			// process uses.
			remote := conn.RemoteAddr().String()
			conn.Close()
			n.log.Warn("too many connections at once; dropped one",
				"remote", remote, "limit", maxHandlers)
		}
	}
}

func (n *Node) handle(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	// A connection that has gone quiet must not hold up shutdown for the whole
	// of the deadline below.
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))

	msg, nonce, err := readFrame(conn, n.sync, n.frameCap())
	if err != nil {
		if !errors.Is(err, io.EOF) {
			// A failure here is usually a port scan or a device with the wrong
			// join code. Log quietly; it is not actionable.
			n.log.Debug("rejected connection", "remote", conn.RemoteAddr().String(), "err", err)
		}
		return
	}

	reply := &Message{
		V: ProtocolVersion, Kind: KindAck,
		Device: n.cfg.DeviceID, Name: n.cfg.DeviceName,
		TS: time.Now().UnixMilli(),
	}

	if err := n.authorise(msg, nonce, conn.RemoteAddr()); err != nil {
		reply.Err = err.Error()
		n.log.Debug("refused a message", "remote", conn.RemoteAddr().String(),
			"kind", msg.Kind, "err", err)
	} else {
		switch msg.Kind {
		case KindClip:
			if err := n.applyClip(msg, conn.RemoteAddr()); err != nil {
				reply.Err = err.Error()
			}
		case KindStatus:
			reply.Kind = KindState
			reply.State = n.state()
		case KindPush:
			format, count, err := n.pushCurrent(ctx, msg.Primary)
			if err != nil {
				reply.Err = err.Error()
			} else {
				// So `henri send` can say what was sent instead of guessing.
				reply.Format, reply.Count = format, count
			}
		default:
			reply.Err = fmt.Sprintf("unknown message kind %q", msg.Kind)
		}
	}

	if err := writeFrame(conn, n.sync, reply); err != nil {
		n.log.Debug("reply failed", "remote", conn.RemoteAddr().String(), "err", err)
	}
}

// authorise decides whether a message may be acted on at all.
//
// Freshness used to be checked for clipboard payloads and nothing else, which
// left the control messages open to anyone who could capture a frame and send
// it again: a replayed status query hands out device names, addresses, a PID
// and the clipboard's fingerprint, and a replayed push makes this device read
// whatever is highlighted on screen and broadcast it. Both are local commands,
// so they are refused from anywhere but this machine as well.
func (n *Node) authorise(msg *Message, nonce []byte, remote net.Addr) error {
	if err := checkFresh(msg.TS); err != nil {
		return err
	}
	if !n.replay.fresh(nonce) {
		return errors.New("this message has already been delivered once")
	}
	switch msg.Kind {
	case KindStatus, KindPush:
		if !isLoopback(remote) {
			return fmt.Errorf("%q may only be asked for from this device", msg.Kind)
		}
	}
	return nil
}

func isLoopback(addr net.Addr) bool {
	host, _, err := net.SplitHostPort(addr.String())
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// applyClip validates an inbound clipboard payload and writes it locally.
func (n *Node) applyClip(msg *Message, remote net.Addr) error {
	if msg.Device == n.cfg.DeviceID {
		return nil // our own push, looped back
	}
	if len(msg.Data) == 0 {
		return errors.New("empty payload")
	}
	limit := n.cfg.MaxBytes
	if msg.Format != FormatText {
		limit = n.cfg.MaxFileBytes
	}
	if len(msg.Data) > limit {
		return fmt.Errorf("payload of %d bytes exceeds this device's limit of %d", len(msg.Data), limit)
	}
	// Once: the payload can be megabytes, and this used to hash it twice.
	hash := secure.Hash(msg.Data)
	// The hash is advisory, but a mismatch means something is wrong upstream.
	if msg.Hash != "" && hash != msg.Hash {
		return errors.New("payload does not match its hash")
	}

	// Worth learning even if the write below fails. A device that only ever
	// receives otherwise never finds out how to reach anybody.
	if host, _, err := net.SplitHostPort(remote.String()); err == nil && msg.Port > 0 {
		n.peers.seen(msg.Device, msg.Name, net.JoinHostPort(host, fmt.Sprint(msg.Port)))
	}

	switch msg.Format {
	case FormatText:
		return n.applyLocal(msg, hash, len(msg.Data), 0, func() error { return n.clip.Write(msg.Data) })
	case FormatImage:
		return n.applyLocal(msg, hash, len(msg.Data), 0, func() error { return n.clip.WriteImage(msg.Data) })
	case FormatFiles:
		return n.applyFiles(msg)
	default:
		// A format from a build newer than this one. Refusing with a name is
		// kinder than writing bytes we do not understand somewhere visible.
		return fmt.Errorf("this build does not understand %q payloads; upgrade henri here", msg.Format)
	}
}

// applyLocal claims a fingerprint and runs one clipboard write, undoing the
// claim if the write fails so the sender's retry is not acknowledged away.
func (n *Node) applyLocal(msg *Message, fp string, size, count int, write func() error) error {
	n.clipMu.Lock()
	defer n.clipMu.Unlock()

	n.mu.Lock()
	if fp == n.lastHash {
		n.mu.Unlock()
		return nil // already in sync; do not bounce it back
	}
	// Claim the fingerprint before writing so the watch loop sees the new
	// content as already-known and does not re-broadcast it.
	prevHash, prevBytes, prevFrom, prevSync := n.lastHash, n.lastBytes, n.lastFrom, n.lastSync
	prevFormat, prevCount := n.lastFormat, n.lastCount
	n.lastHash, n.lastBytes = fp, size
	n.lastFormat, n.lastCount = msg.Format, count
	n.lastFrom, n.lastSync = displayName(msg), time.Now()
	n.mu.Unlock()

	if err := write(); err != nil {
		// Put the old fingerprint back, or one passing failure loses this
		// payload for good: the sender's next attempt would match lastHash and
		// be dropped with a cheerful acknowledgement.
		n.mu.Lock()
		n.lastHash, n.lastBytes = prevHash, prevBytes
		n.lastFrom, n.lastSync = prevFrom, prevSync
		n.lastFormat, n.lastCount = prevFormat, prevCount
		n.mu.Unlock()
		return err
	}
	n.received.Add(1)
	n.log.Info("received clipboard", "what", describe(msg.Format, count),
		"from", displayName(msg), "bytes", len(msg.Data))
	return nil
}

// applyFiles unpacks a files payload and puts the unpacked paths on the
// clipboard, so a paste here lands the real files.
func (n *Node) applyFiles(msg *Message) error {
	if n.receiveDir == "" {
		return errors.New("this device has nowhere to receive files; set receive_dir in its config")
	}
	// Unpacking happens before any lock: it is disk work, and the clipboard
	// has no business waiting on it.
	created, err := pack.Extract(msg.Data, n.receiveDir, n.cfg.MaxFileBytes)
	if err != nil {
		return err
	}
	// The claim is the fingerprint of what the watcher will see here: the
	// unpacked paths, not the sender's. It has to be computed the same way.
	fp, size, err := n.fingerprint(clipboard.Snapshot{Kind: clipboard.ContentFiles, Paths: created})
	if err != nil {
		return err
	}
	err = n.applyLocal(msg, fp, size, len(created), func() error { return n.clip.WriteFiles(created) })
	if err != nil {
		// The files themselves stay: they are safely in the receive directory
		// and picking them up there beats downloading them again.
		return fmt.Errorf("%d files landed in %s, but the clipboard write failed: %w", len(created), n.receiveDir, err)
	}
	return nil
}

// pushCurrent sends the clipboard, or the highlighted text, to the group. It
// returns what it sent -- the format and, for files, how many -- so the CLI
// can say something true.
//
// With primary set it takes the PRIMARY selection and puts it on the local
// clipboard on the way past, so one keypress both copies and sends. That is the
// whole trick for compositors henri cannot watch: highlighted text is already
// published, so nothing has to synthesise a Ctrl+C to get at it. Highlighted
// text is always text; when the clipboard holds files or an image, those win
// only after the selection turns out to be empty.
func (n *Node) pushCurrent(ctx context.Context, primary bool) (string, int, error) {
	n.clipMu.Lock()

	var data []byte
	var err error
	if primary {
		if data, err = n.clip.ReadPrimary(); err != nil {
			// macOS and Windows have no separate selection for highlighted text
			// and every backend there says so. That is not a failure, it means
			// there is nothing highlighted to take -- and treating it as one is
			// why the key did nothing at all on those platforms.
			if !errors.Is(err, clipboard.ErrNoPrimary) {
				n.clipMu.Unlock()
				return "", 0, err
			}
			data = nil
		}
	}

	if len(data) == 0 {
		// Nothing highlighted; fall back to the clipboard, whatever it holds,
		// so the key still does something sensible.
		primary = false
		snap, err := n.clip.Look()
		if err != nil {
			n.clipMu.Unlock()
			return "", 0, err
		}
		if snap.Kind != clipboard.ContentText {
			return n.pushRich(ctx, snap)
		}
		data = snap.Text
	}
	if len(data) == 0 {
		n.clipMu.Unlock()
		return "", 0, errors.New("nothing is highlighted and the clipboard is empty")
	}
	if len(data) > n.cfg.MaxBytes {
		n.clipMu.Unlock()
		return "", 0, fmt.Errorf("clipboard is %d bytes, over the %d byte limit", len(data), n.cfg.MaxBytes)
	}

	h := secure.Hash(data)
	n.mu.Lock()
	prevHash, prevBytes, prevFrom, prevSync := n.lastHash, n.lastBytes, n.lastFrom, n.lastSync
	prevFormat, prevCount := n.lastFormat, n.lastCount
	prevPending, prevPendingSince, prevPendingTried, prevPendingWait := n.pendingHash, n.pendingSince, n.pendingTried, n.pendingWait
	n.lastHash, n.lastBytes = h, len(data)
	n.lastFormat, n.lastCount = FormatText, 0
	n.lastFrom, n.lastSync = "local", time.Now()
	n.pendingHash = ""
	n.mu.Unlock()

	// Claiming the hash above means the watcher will not treat this as a new
	// copy and send it twice.
	if primary {
		if err := n.clip.Write(data); err != nil {
			// Put the old fingerprint back, exactly as applyClip does. Without
			// this the daemon believed it held the highlighted text while the
			// clipboard still held the old content -- and the next watch tick
			// read that old content, saw an unknown hash, and re-broadcast it
			// to the group as a fresh copy.
			n.mu.Lock()
			n.lastHash, n.lastBytes = prevHash, prevBytes
			n.lastFrom, n.lastSync = prevFrom, prevSync
			n.lastFormat, n.lastCount = prevFormat, prevCount
			n.pendingHash, n.pendingSince, n.pendingTried, n.pendingWait = prevPending, prevPendingSince, prevPendingTried, prevPendingWait
			n.mu.Unlock()
			n.clipMu.Unlock()
			return "", 0, fmt.Errorf("could not copy to this device's clipboard, so nothing was sent: %w", err)
		}
	}
	n.clipMu.Unlock()

	if sent, tried := n.push(ctx, payload{format: FormatText, data: data, fp: h}); sent == 0 && tried == 0 {
		n.log.Warn("sent, but no peer is known yet", "bytes", len(data))
	}
	return FormatText, 0, nil
}

// pushRich sends the files or image the clipboard holds. Called with clipMu
// held; releases it.
func (n *Node) pushRich(ctx context.Context, snap clipboard.Snapshot) (string, int, error) {
	fp, size, err := n.fingerprint(snap)
	if err != nil {
		n.clipMu.Unlock()
		return "", 0, fmt.Errorf("the copied files are gone: %w", err)
	}
	if fp == "" {
		n.clipMu.Unlock()
		return "", 0, errors.New("the clipboard is empty")
	}
	if snap.Kind == clipboard.ContentImage && size > n.cfg.MaxFileBytes {
		n.clipMu.Unlock()
		return "", 0, fmt.Errorf("the image is %d bytes, over the %d byte limit", size, n.cfg.MaxFileBytes)
	}

	n.mu.Lock()
	n.lastHash, n.lastBytes = fp, size
	n.lastFrom, n.lastSync = "local", time.Now()
	n.pendingHash = ""
	n.mu.Unlock()
	n.clipMu.Unlock()

	p, err := n.build(snap, fp)
	if err != nil {
		return "", 0, err
	}
	n.mu.Lock()
	n.lastFormat, n.lastCount = p.format, len(p.names)
	n.mu.Unlock()
	if sent, tried := n.push(ctx, p); sent == 0 && tried == 0 {
		n.log.Warn("sent, but no peer is known yet", "what", describe(p.format, len(p.names)))
	}
	return p.format, len(p.names), nil
}

func (n *Node) state() *State {
	n.mu.Lock()
	lastHash, lastBytes, lastFrom, lastSync := n.lastHash, n.lastBytes, n.lastFrom, n.lastSync
	lastFormat, lastCount := n.lastFormat, n.lastCount
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
		LastHash:     shortHash(lastHash),
		LastBytes:    lastBytes,
		LastFrom:     lastFrom,
		LastFormat:   lastFormat,
		LastCount:    lastCount,
		Sent:         n.sent.Load(),
		Received:     n.received.Load(),
		Beacons:      n.beacons.Load(),
		LastBeaconAt: n.lastBeacon.Load(),
		Peers:        n.peers.list(),
	}
	if !lastSync.IsZero() {
		st.LastSyncAt = lastSync.UnixMilli()
	}
	return st
}

// shortHash is as much of a fingerprint as a status query is entitled to. The
// whole SHA-256 of something short and guessable -- a PIN, a card number -- is
// the content, to anyone willing to spend an afternoon on it.
func shortHash(h string) string {
	if len(h) > 8 {
		return h[:8]
	}
	return h
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
