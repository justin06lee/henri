package node

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/justin06lee/henri/internal/clipboard"
	"github.com/justin06lee/henri/internal/config"
	"github.com/justin06lee/henri/internal/pack"
	"github.com/justin06lee/henri/internal/secure"
)

// fakeClipboard stands in for the system clipboard so tests never touch the
// developer's real one.
type fakeClipboard struct {
	mu      sync.Mutex
	data    []byte
	files   []string
	image   []byte
	primary []byte
	// The failure modes a real clipboard has and this one used not to: a
	// backend with no PRIMARY selection, a write that fails, and a write that
	// takes a while to land.
	primaryErr error
	writeErr   error
	writeDelay time.Duration
}

func (f *fakeClipboard) Read() ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]byte(nil), f.data...), nil
}

func (f *fakeClipboard) Look() (clipboard.Snapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case len(f.files) > 0:
		return clipboard.Snapshot{Kind: clipboard.ContentFiles, Paths: append([]string(nil), f.files...)}, nil
	case len(f.image) > 0:
		return clipboard.Snapshot{Kind: clipboard.ContentImage, Image: append([]byte(nil), f.image...)}, nil
	default:
		return clipboard.Snapshot{Kind: clipboard.ContentText, Text: append([]byte(nil), f.data...)}, nil
	}
}

func (f *fakeClipboard) Write(d []byte) error {
	f.mu.Lock()
	delay, err := f.writeDelay, f.writeErr
	f.mu.Unlock()
	// Outside the lock, like the subprocess a real backend shells out to: the
	// old content is still readable for the whole time the write is in flight.
	if delay > 0 {
		time.Sleep(delay)
	}
	if err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.data = append([]byte(nil), d...)
	f.files, f.image = nil, nil
	return nil
}

func (f *fakeClipboard) WriteFiles(paths []string) error {
	f.mu.Lock()
	err := f.writeErr
	f.mu.Unlock()
	if err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.files = append([]string(nil), paths...)
	f.data, f.image = nil, nil
	return nil
}

func (f *fakeClipboard) WriteImage(png []byte) error {
	f.mu.Lock()
	err := f.writeErr
	f.mu.Unlock()
	if err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.image = append([]byte(nil), png...)
	f.data, f.files = nil, nil
	return nil
}

func (f *fakeClipboard) setFiles(paths []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.files = append([]string(nil), paths...)
	f.data, f.image = nil, nil
}

func (f *fakeClipboard) getFiles() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.files...)
}

func (f *fakeClipboard) setImage(png []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.image = append([]byte(nil), png...)
	f.data, f.files = nil, nil
}

func (f *fakeClipboard) getImage() []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]byte(nil), f.image...)
}

func (f *fakeClipboard) ReadPrimary() ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.primaryErr != nil {
		return nil, f.primaryErr
	}
	return append([]byte(nil), f.primary...), nil
}

func (f *fakeClipboard) failPrimary(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.primaryErr = err
}

func (f *fakeClipboard) failWrites(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.writeErr = err
}

func (f *fakeClipboard) slowWrites(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.writeDelay = d
}

func (f *fakeClipboard) Name() string { return "fake" }

func (f *fakeClipboard) highlight(s string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.primary = []byte(s)
}

func (f *fakeClipboard) set(s string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.data = []byte(s)
	f.files, f.image = nil, nil
}

func (f *fakeClipboard) get() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return string(f.data)
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func groupKey(t *testing.T) string {
	t.Helper()
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(k)
}

// startNode brings up a node on a free port and returns it with its clipboard.
func startNode(t *testing.T, ctx context.Context, name, group, key string, port int, peers []string) (*Node, *fakeClipboard) {
	t.Helper()
	cfg := &config.Config{
		GroupID:       group,
		Key:           key,
		DeviceID:      name + "-id",
		DeviceName:    name,
		ListenPort:    port,
		DiscoveryPort: 0,
		Discovery:     false,
		Peers:         peers,
		PollMillis:    20,
		MaxBytes:      1 << 20,
		MaxFileBytes:  1 << 20,
		// Never the real ~/Downloads: tests must not write into the
		// developer's home.
		ReceiveDir: t.TempDir(),
	}
	clip := &fakeClipboard{}
	n, err := NewWith(cfg, quietLogger(), clip)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- n.Run(ctx) }()
	t.Cleanup(func() {
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Log("node did not shut down in time")
		}
	})
	waitForPort(t, port)
	return n, clip
}

func waitForPort(t *testing.T, port int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 100*time.Millisecond)
		if err == nil {
			c.Close()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("node on port %d never started listening", port)
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// A copy on one device must land on the other, and must not bounce back.
func TestClipboardPropagatesBetweenPeers(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	group, key := "test-group", groupKey(t)
	portA, portB := freePort(t), freePort(t)

	a, clipA := startNode(t, ctx, "alpha", group, key, portA, []string{fmt.Sprintf("127.0.0.1:%d", portB)})
	b, clipB := startNode(t, ctx, "beta", group, key, portB, []string{fmt.Sprintf("127.0.0.1:%d", portA)})

	clipA.set("first copy")
	waitFor(t, "beta to receive the clipboard", func() bool { return clipB.get() == "first copy" })

	if got := b.received.Load(); got != 1 {
		t.Fatalf("beta received %d payloads, want 1", got)
	}
	// The echo guard: beta must not re-broadcast what it just applied.
	time.Sleep(300 * time.Millisecond)
	if got := b.sent.Load(); got != 0 {
		t.Fatalf("beta echoed the payload back %d times", got)
	}
	if got := a.received.Load(); got != 0 {
		t.Fatalf("alpha received its own payload back %d times", got)
	}

	// And it works in the other direction.
	clipB.set("reply copy")
	waitFor(t, "alpha to receive the reply", func() bool { return clipA.get() == "reply copy" })
}

// A device holding a different key must not be able to inject a clipboard.
func TestForeignKeyIsRejected(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	port := freePort(t)
	_, clip := startNode(t, ctx, "alpha", "test-group", groupKey(t), port, nil)
	clip.set("untouched")

	// Seal a perfectly well-formed message under an unrelated key.
	box, err := secure.NewBox(mustKey(t, groupKey(t)), "test-group", secure.InfoSync)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	msg := &Message{
		V: ProtocolVersion, Kind: KindClip, Device: "attacker",
		TS: time.Now().UnixMilli(), Data: []byte("injected"), Hash: secure.Hash([]byte("injected")),
	}
	if err := writeFrame(conn, box, msg); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	if _, _, err := readFrame(conn, box, frameLimit(1<<20)); err == nil {
		t.Fatal("the daemon replied to a message sealed with a foreign key")
	}
	time.Sleep(200 * time.Millisecond)
	if got := clip.get(); got != "untouched" {
		t.Fatalf("clipboard was overwritten by a foreign key: %q", got)
	}
}

// Replayed traffic must not be applied.
func TestStaleMessageIsRejected(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	group, key := "test-group", groupKey(t)
	port := freePort(t)
	_, clip := startNode(t, ctx, "alpha", group, key, port, nil)
	clip.set("untouched")

	box, err := secure.NewBox(mustKey(t, key), group, secure.InfoSync)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	msg := &Message{
		V: ProtocolVersion, Kind: KindClip, Device: "old-device",
		TS:   time.Now().Add(-1 * time.Hour).UnixMilli(),
		Data: []byte("replayed"), Hash: secure.Hash([]byte("replayed")),
	}
	if err := writeFrame(conn, box, msg); err != nil {
		t.Fatal(err)
	}
	resp, _, err := readFrame(conn, box, frameLimit(1<<20))
	if err != nil {
		t.Fatal(err)
	}
	if resp.Err == "" {
		t.Fatal("a message from an hour ago was accepted")
	}
	if got := clip.get(); got != "untouched" {
		t.Fatalf("clipboard was overwritten by a replay: %q", got)
	}
}

// A payload whose hash does not match its bytes is a corrupted or crafted
// message and must be refused.
func TestHashMismatchIsRejected(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	group, key := "test-group", groupKey(t)
	port := freePort(t)
	_, clip := startNode(t, ctx, "alpha", group, key, port, nil)
	clip.set("untouched")

	box, err := secure.NewBox(mustKey(t, key), group, secure.InfoSync)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	msg := &Message{
		V: ProtocolVersion, Kind: KindClip, Device: "other",
		TS: time.Now().UnixMilli(), Data: []byte("real bytes"), Hash: secure.Hash([]byte("different bytes")),
	}
	if err := writeFrame(conn, box, msg); err != nil {
		t.Fatal(err)
	}
	resp, _, err := readFrame(conn, box, frameLimit(1<<20))
	if err != nil {
		t.Fatal(err)
	}
	if resp.Err == "" {
		t.Fatal("a payload that did not match its hash was accepted")
	}
	if got := clip.get(); got != "untouched" {
		t.Fatalf("clipboard was overwritten: %q", got)
	}
}

// Content over the configured limit is skipped rather than sent.
func TestOversizedClipboardIsNotSent(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	group, key := "test-group", groupKey(t)
	portA, portB := freePort(t), freePort(t)
	a, clipA := startNode(t, ctx, "alpha", group, key, portA, []string{fmt.Sprintf("127.0.0.1:%d", portB)})
	_, clipB := startNode(t, ctx, "beta", group, key, portB, []string{fmt.Sprintf("127.0.0.1:%d", portA)})

	clipA.set(string(bytes.Repeat([]byte("x"), (1<<20)+1)))
	time.Sleep(400 * time.Millisecond)
	if got := a.sent.Load(); got != 0 {
		t.Fatalf("an oversized clipboard was sent %d times", got)
	}
	if clipB.get() != "" {
		t.Fatal("beta's clipboard was written with oversized content")
	}
}

// Starting the daemon must not broadcast whatever was already on the clipboard.
func TestExistingClipboardIsNotBroadcastOnStart(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	group, key := "test-group", groupKey(t)
	portA, portB := freePort(t), freePort(t)

	cfg := &config.Config{
		GroupID: group, Key: key, DeviceID: "alpha-id", DeviceName: "alpha",
		ListenPort: portA, Peers: []string{fmt.Sprintf("127.0.0.1:%d", portB)},
		PollMillis: 20, MaxBytes: 1 << 20, MaxFileBytes: 1 << 20,
	}
	clipA := &fakeClipboard{}
	clipA.set("already here before henri started")
	a, err := NewWith(cfg, quietLogger(), clipA)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	t.Cleanup(func() { <-done })
	waitForPort(t, portA)

	_, clipB := startNode(t, ctx, "beta", group, key, portB, nil)
	time.Sleep(300 * time.Millisecond)
	if got := a.sent.Load(); got != 0 {
		t.Fatalf("alpha broadcast its pre-existing clipboard %d times", got)
	}
	if clipB.get() != "" {
		t.Fatal("beta received a clipboard it should not have")
	}
}

func TestStatusQuery(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	group, key := "test-group", groupKey(t)
	port := freePort(t)
	startNode(t, ctx, "alpha", group, key, port, []string{"10.0.0.5:47600"})

	cfg := &config.Config{GroupID: group, Key: key, DeviceID: "cli", DeviceName: "cli", ListenPort: port}
	resp, err := Query(cfg, KindStatus)
	if err != nil {
		t.Fatal(err)
	}
	if resp.State == nil {
		t.Fatal("status reply carried no state")
	}
	if resp.State.Name != "alpha" {
		t.Fatalf("status reported device %q, want alpha", resp.State.Name)
	}
	if len(resp.State.Peers) != 1 || resp.State.Peers[0].Source != "config" {
		t.Fatalf("status reported peers %+v, want the one configured peer", resp.State.Peers)
	}
}

func TestQueryReportsDaemonNotRunning(t *testing.T) {
	cfg := &config.Config{
		GroupID: "test-group", Key: groupKey(t), DeviceID: "cli", DeviceName: "cli",
		ListenPort: freePort(t),
	}
	if _, err := Query(cfg, KindStatus); err != ErrNotRunning {
		t.Fatalf("got %v, want ErrNotRunning", err)
	}
	// main() prefixes every error it prints with "henri: ", so the sentinel
	// must not carry its own -- `henri send` printed "henri: henri: ...".
	if strings.HasPrefix(ErrNotRunning.Error(), "henri:") {
		t.Fatalf("ErrNotRunning carries its own prefix: %q", ErrNotRunning)
	}
}

func mustKey(t *testing.T, b64 string) []byte {
	t.Helper()
	k, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// One keypress has to do both halves: put the highlighted text on this
// device's clipboard, and send it to the others.
func TestPushHighlightedCopiesLocallyAndSends(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	group, key := "test-group", groupKey(t)
	portA, portB := freePort(t), freePort(t)
	_, clipA := startNode(t, ctx, "alpha", group, key, portA, []string{fmt.Sprintf("127.0.0.1:%d", portB)})
	_, clipB := startNode(t, ctx, "beta", group, key, portB, nil)

	clipA.set("old clipboard")
	clipA.highlight("highlighted words")
	waitFor(t, "alpha's own copy to settle", func() bool { return clipB.get() == "old clipboard" })

	cfg := &config.Config{GroupID: group, Key: key, DeviceID: "cli", DeviceName: "cli", ListenPort: portA}
	if _, err := QueryPush(cfg, true); err != nil {
		t.Fatal(err)
	}

	if got := clipA.get(); got != "highlighted words" {
		t.Fatalf("alpha's clipboard is %q, want the highlighted text", got)
	}
	waitFor(t, "beta to receive the highlighted text", func() bool { return clipB.get() == "highlighted words" })
}

// Pressing the key with nothing highlighted should still send the clipboard
// rather than reporting an error.
func TestPushHighlightedFallsBackToClipboard(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	group, key := "test-group", groupKey(t)
	portA, portB := freePort(t), freePort(t)
	_, clipA := startNode(t, ctx, "alpha", group, key, portA, []string{fmt.Sprintf("127.0.0.1:%d", portB)})
	_, clipB := startNode(t, ctx, "beta", group, key, portB, nil)

	clipA.set("only the clipboard")
	clipA.highlight("")
	waitFor(t, "the initial copy to land", func() bool { return clipB.get() == "only the clipboard" })

	cfg := &config.Config{GroupID: group, Key: key, DeviceID: "cli", DeviceName: "cli", ListenPort: portA}
	if _, err := QueryPush(cfg, true); err != nil {
		t.Fatalf("pressing the key with nothing highlighted failed: %v", err)
	}
}

// And it must not double-send: claiming the hash before writing locally is what
// stops the watcher treating the local copy as a fresh one.
func TestPushHighlightedDoesNotDoubleSend(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	group, key := "test-group", groupKey(t)
	portA, portB := freePort(t), freePort(t)
	a, clipA := startNode(t, ctx, "alpha", group, key, portA, []string{fmt.Sprintf("127.0.0.1:%d", portB)})
	startNode(t, ctx, "beta", group, key, portB, nil)

	clipA.highlight("just this once")
	cfg := &config.Config{GroupID: group, Key: key, DeviceID: "cli", DeviceName: "cli", ListenPort: portA}
	if _, err := QueryPush(cfg, true); err != nil {
		t.Fatal(err)
	}
	before := a.sent.Load()
	time.Sleep(400 * time.Millisecond)
	if after := a.sent.Load(); after != before {
		t.Fatalf("the highlighted text was sent again by the watcher (%d -> %d)", before, after)
	}
}

// offlineNode builds a node without running it, for the decisions that are
// worth testing without a network underneath them.
func offlineNode(t *testing.T, name string) *Node {
	t.Helper()
	cfg := &config.Config{
		GroupID: "test-group", Key: groupKey(t), DeviceID: name + "-id", DeviceName: name,
		ListenPort: freePort(t), PollMillis: 20, MaxBytes: 1 << 20, MaxFileBytes: 1 << 20,
		ReceiveDir: t.TempDir(),
	}
	n, err := NewWith(cfg, quietLogger(), &fakeClipboard{})
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func syncBox(t *testing.T, group, key string) *secure.Box {
	t.Helper()
	box, err := secure.NewBox(mustKey(t, key), group, secure.InfoSync)
	if err != nil {
		t.Fatal(err)
	}
	return box
}

// exchange sends one message to a daemon and returns its reply.
func exchange(t *testing.T, port int, box *secure.Box, msg *Message) *Message {
	t.Helper()
	var frame bytes.Buffer
	if err := writeFrame(&frame, box, msg); err != nil {
		t.Fatal(err)
	}
	return exchangeRaw(t, port, box, frame.Bytes())
}

// exchangeRaw sends bytes that have already been sealed, which is the only way
// to send the same frame twice.
func exchangeRaw(t *testing.T, port int, box *secure.Box, frame []byte) *Message {
	t.Helper()
	conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := conn.Write(frame); err != nil {
		t.Fatal(err)
	}
	resp, _, err := readFrame(conn, box, frameLimit(1<<20))
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func clipMessage(device string, payload []byte) *Message {
	return &Message{
		V: ProtocolVersion, Kind: KindClip, Device: device, Name: device,
		TS: time.Now().UnixMilli(), Hash: secure.Hash(payload), Data: payload,
	}
}

// Receiving a payload has to teach the receiver where the sender lives, or
// everything works in whichever direction discovery happened to find first.
func TestReceivingAPayloadTeachesTheSendersAddress(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	group, key := "test-group", groupKey(t)
	portA, portB := freePort(t), freePort(t)
	// alpha has beta in its config. beta has been told nothing at all.
	_, clipA := startNode(t, ctx, "alpha", group, key, portA, []string{fmt.Sprintf("127.0.0.1:%d", portB)})
	b, clipB := startNode(t, ctx, "beta", group, key, portB, nil)

	clipA.set("outbound")
	waitFor(t, "beta to receive the payload", func() bool { return clipB.get() == "outbound" })

	want := fmt.Sprintf("127.0.0.1:%d", portA)
	waitFor(t, "beta to learn where alpha lives", func() bool {
		addrs := b.peers.addrs()
		return len(addrs) == 1 && addrs[0] == want
	})

	clipB.set("and back again")
	waitFor(t, "alpha to receive the reply", func() bool { return clipA.get() == "and back again" })
}

// While an inbound payload is being written the clipboard still holds the old
// content. The watcher must not read that and decide the user copied it.
func TestSlowClipboardWriteDoesNotRevertTheSender(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	group, key := "test-group", groupKey(t)
	portA, portB := freePort(t), freePort(t)
	_, clipA := startNode(t, ctx, "alpha", group, key, portA, []string{fmt.Sprintf("127.0.0.1:%d", portB)})
	b, clipB := startNode(t, ctx, "beta", group, key, portB, []string{fmt.Sprintf("127.0.0.1:%d", portA)})

	clipB.set("older content")
	waitFor(t, "both devices to settle", func() bool { return clipA.get() == "older content" })

	// Beta's clipboard now takes a while to actually change, as a real one does.
	clipB.slowWrites(300 * time.Millisecond)
	before := b.sent.Load()

	clipA.set("newer content")
	waitFor(t, "beta to apply the new content", func() bool { return clipB.get() == "newer content" })
	time.Sleep(200 * time.Millisecond)

	if got := clipA.get(); got != "newer content" {
		t.Fatalf("alpha's clipboard was reverted to %q by beta re-broadcasting what it was overwriting", got)
	}
	if after := b.sent.Load(); after != before {
		t.Fatalf("beta broadcast the content it was in the middle of replacing (%d -> %d)", before, after)
	}
}

// A clipboard write that fails must leave no trace, or the sender's next
// attempt is dropped as already-delivered and the payload is gone for good.
func TestFailedClipboardWriteIsNotRememberedAsDelivered(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	group, key := "test-group", groupKey(t)
	port := freePort(t)
	_, clip := startNode(t, ctx, "alpha", group, key, port, nil)
	box := syncBox(t, group, key)
	payload := []byte("worth keeping")

	clip.failWrites(errors.New("the clipboard tool went away"))
	if resp := exchange(t, port, box, clipMessage("beta-id", payload)); resp.Err == "" {
		t.Fatal("a clipboard write that failed was acknowledged as a success")
	}

	clip.failWrites(nil)
	if resp := exchange(t, port, box, clipMessage("beta-id", payload)); resp.Err != "" {
		t.Fatalf("the sender's retry was refused: %s", resp.Err)
	}
	if got := clip.get(); got != string(payload) {
		t.Fatalf("the clipboard holds %q after the retry, want the payload", got)
	}
}

// A stranger announcing an enormous frame must not be able to make the daemon
// allocate it, nor take the daemon down.
func TestHugeFrameHeaderDoesNotDisturbTheDaemon(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	group, key := "test-group", groupKey(t)
	port := freePort(t)
	startNode(t, ctx, "alpha", group, key, port, nil)

	for range 8 {
		conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err != nil {
			t.Fatal(err)
		}
		var hdr [4]byte
		binary.BigEndian.PutUint32(hdr[:], 60<<20) // 60 MiB, and then five bytes
		if _, err := conn.Write(append(hdr[:], "hello"...)); err != nil {
			t.Fatal(err)
		}
		_ = conn.SetReadDeadline(time.Now().Add(time.Second))
		if _, _, err := readFrame(conn, syncBox(t, group, key), frameLimit(1<<20)); err == nil {
			t.Fatal("a frame far over the payload limit was answered")
		}
		conn.Close()
	}

	cfg := &config.Config{GroupID: group, Key: key, DeviceID: "cli", DeviceName: "cli", ListenPort: port}
	if _, err := Query(cfg, KindStatus); err != nil {
		t.Fatalf("the daemon stopped answering after being lied to about a frame size: %v", err)
	}
}

// Every macOS and Windows backend reports that it has no PRIMARY selection.
// That is not a failure, and the key must still send the clipboard.
func TestPushHighlightedFallsBackWhereThereIsNoPrimarySelection(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	group, key := "test-group", groupKey(t)
	portA, portB := freePort(t), freePort(t)
	a, clipA := startNode(t, ctx, "alpha", group, key, portA, []string{fmt.Sprintf("127.0.0.1:%d", portB)})
	_, clipB := startNode(t, ctx, "beta", group, key, portB, nil)

	clipA.failPrimary(clipboard.ErrNoPrimary)
	clipA.set("only the clipboard")
	waitFor(t, "the copy to land on beta", func() bool { return clipB.get() == "only the clipboard" })

	before := a.sent.Load()
	cfg := &config.Config{GroupID: group, Key: key, DeviceID: "cli", DeviceName: "cli", ListenPort: portA}
	if _, err := QueryPush(cfg, true); err != nil {
		t.Fatalf("the hotkey failed on a system with no PRIMARY selection: %v", err)
	}
	if a.sent.Load() == before {
		t.Fatal("nothing was sent: the fallback to the clipboard never happened")
	}
}

// Control messages drive this device. They have to be fresh, and they have to
// come from this device.
func TestControlMessagesMustBeLocalAndFresh(t *testing.T) {
	n := offlineNode(t, "alpha")
	now := time.Now().UnixMilli()
	stale := time.Now().Add(-time.Hour).UnixMilli()
	here := &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 51000}
	elsewhere := &net.TCPAddr{IP: net.IPv4(192, 168, 1, 50), Port: 51000}

	if err := n.authorise(&Message{Kind: KindStatus, TS: now}, []byte("nonce-1"), elsewhere); err == nil {
		t.Fatal("a status query from another device was allowed: it hands out names, addresses and a PID")
	}
	if err := n.authorise(&Message{Kind: KindPush, TS: now}, []byte("nonce-2"), elsewhere); err == nil {
		t.Fatal("a push from another device was allowed: it broadcasts whatever is highlighted here")
	}
	if err := n.authorise(&Message{Kind: KindStatus, TS: stale}, []byte("nonce-3"), here); err == nil {
		t.Fatal("a status query from an hour ago was allowed")
	}
	if err := n.authorise(&Message{Kind: KindPush, TS: stale}, []byte("nonce-4"), here); err == nil {
		t.Fatal("a push from an hour ago was allowed")
	}
	if err := n.authorise(&Message{Kind: KindStatus, TS: now}, []byte("nonce-5"), here); err != nil {
		t.Fatalf("a status query from this device was refused: %v", err)
	}
	// Clipboard payloads still arrive from anywhere on the network.
	if err := n.authorise(&Message{Kind: KindClip, TS: now}, []byte("nonce-6"), elsewhere); err != nil {
		t.Fatalf("a clipboard payload from a peer was refused: %v", err)
	}
}

// Freshness leaves a two minute window. Within it, a captured frame used to be
// as good as the original -- enough to put an old clipboard back under someone
// at the moment they paste.
func TestACapturedFrameCannotBeDeliveredTwice(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	group, key := "test-group", groupKey(t)
	port := freePort(t)
	_, clip := startNode(t, ctx, "alpha", group, key, port, nil)
	box := syncBox(t, group, key)

	var frame bytes.Buffer
	if err := writeFrame(&frame, box, clipMessage("beta-id", []byte("the old content"))); err != nil {
		t.Fatal(err)
	}
	if resp := exchangeRaw(t, port, box, frame.Bytes()); resp.Err != "" {
		t.Fatalf("the first delivery was refused: %s", resp.Err)
	}
	waitFor(t, "the payload to be applied", func() bool { return clip.get() == "the old content" })

	if resp := exchangeRaw(t, port, box, frame.Bytes()); resp.Err == "" {
		t.Fatal("a captured frame was delivered a second time")
	}
}

// A copy made before any peer is known must not be quietly written off: it is
// the first thing anybody does after starting the daemon on a second device.
func TestCopyMadeBeforeAnyPeerIsKnownIsNotLost(t *testing.T) {
	old := pendingRetry
	pendingRetry = 50 * time.Millisecond
	t.Cleanup(func() { pendingRetry = old })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	group, key := "test-group", groupKey(t)
	portA, portB := freePort(t), freePort(t)
	a, clipA := startNode(t, ctx, "alpha", group, key, portA, nil)
	_, clipB := startNode(t, ctx, "beta", group, key, portB, nil)

	clipA.set("copied while alone")
	time.Sleep(200 * time.Millisecond)
	if got := clipB.get(); got != "" {
		t.Fatalf("beta has %q already, so this test proves nothing", got)
	}

	// Beta turns up, which is what discovery does ten seconds in.
	a.peers.seen("beta-id", "beta", fmt.Sprintf("127.0.0.1:%d", portB))
	waitFor(t, "the held copy to reach beta once it appears", func() bool {
		return clipB.get() == "copied while alone"
	})
}

// A failed local write must not leave the daemon believing the clipboard holds
// the highlighted text. It did: pushCurrent claimed the new hash, the write
// failed, and the next watch tick read the old content still on the clipboard,
// saw an unknown hash, and re-broadcast the old clipboard to the group --
// undoing the very copy the key press was for.
func TestFailedHighlightedCopyRollsTheFingerprintBack(t *testing.T) {
	cfg := &config.Config{
		GroupID: "test-group", Key: groupKey(t), DeviceID: "alpha-id", DeviceName: "alpha",
		ListenPort: freePort(t), PollMillis: 20, MaxBytes: 1 << 20, MaxFileBytes: 1 << 20,
		ReceiveDir: t.TempDir(),
	}
	clip := &fakeClipboard{}
	n, err := NewWith(cfg, quietLogger(), clip)
	if err != nil {
		t.Fatal(err)
	}

	clip.set("old")
	n.mu.Lock()
	n.lastHash, n.lastBytes = secure.Hash([]byte("old")), len("old")
	n.mu.Unlock()

	clip.highlight("new")
	clip.failWrites(errors.New("no display"))
	if _, _, err := n.pushCurrent(context.Background(), true); err == nil {
		t.Fatal("a failed clipboard write did not report an error")
	}

	n.mu.Lock()
	got := n.lastHash
	n.mu.Unlock()
	if got != secure.Hash([]byte("old")) {
		t.Fatal("the fingerprint was not rolled back; the watcher would re-broadcast the old clipboard")
	}
}

// A file copied on one device must land on the other as real files, on the
// clipboard as references, and must not bounce back.
func TestFilesPropagateBetweenPeers(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	group, key := "test-group", groupKey(t)
	portA, portB := freePort(t), freePort(t)
	a, clipA := startNode(t, ctx, "alpha", group, key, portA, []string{fmt.Sprintf("127.0.0.1:%d", portB)})
	b, clipB := startNode(t, ctx, "beta", group, key, portB, nil)

	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "notes.txt"), []byte("travelling"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(src, "album"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "album", "one.txt"), []byte("track"), 0o644); err != nil {
		t.Fatal(err)
	}
	clipA.setFiles([]string{filepath.Join(src, "notes.txt"), filepath.Join(src, "album")})

	waitFor(t, "beta's clipboard to hold the received files", func() bool { return len(clipB.getFiles()) == 2 })
	got := clipB.getFiles()
	for _, p := range got {
		if !strings.HasPrefix(p, b.receiveDir) {
			t.Fatalf("received file %q is outside beta's receive dir %q", p, b.receiveDir)
		}
	}
	content, err := os.ReadFile(filepath.Join(b.receiveDir, "notes.txt"))
	if err != nil || string(content) != "travelling" {
		t.Fatalf("notes.txt = %q, %v", content, err)
	}
	content, err = os.ReadFile(filepath.Join(b.receiveDir, "album", "one.txt"))
	if err != nil || string(content) != "track" {
		t.Fatalf("album/one.txt = %q, %v", content, err)
	}

	// The echo guard, for files: beta must not re-broadcast what it received.
	time.Sleep(300 * time.Millisecond)
	if got := b.sent.Load(); got != 0 {
		t.Fatalf("beta echoed the files back %d times", got)
	}
	if got := a.received.Load(); got != 0 {
		t.Fatalf("alpha received its own files back %d times", got)
	}
}

// An image copied on one device must arrive as the same bytes, and not echo.
func TestImagePropagatesBetweenPeers(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	group, key := "test-group", groupKey(t)
	portA, portB := freePort(t), freePort(t)
	a, clipA := startNode(t, ctx, "alpha", group, key, portA, []string{fmt.Sprintf("127.0.0.1:%d", portB)})
	b, clipB := startNode(t, ctx, "beta", group, key, portB, nil)

	png := []byte("\x89PNG\r\n\x1a\nnot really pixels but faithfully carried")
	clipA.setImage(png)

	waitFor(t, "beta to receive the image", func() bool { return bytes.Equal(clipB.getImage(), png) })

	time.Sleep(300 * time.Millisecond)
	if got := b.sent.Load(); got != 0 {
		t.Fatalf("beta echoed the image back %d times", got)
	}
	if got := a.received.Load(); got != 0 {
		t.Fatalf("alpha received its own image back %d times", got)
	}
}

// Files past the limit are skipped with a warning, not sent.
func TestOversizedFileCopyIsNotSent(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	group, key := "test-group", groupKey(t)
	portA, portB := freePort(t), freePort(t)
	a, clipA := startNode(t, ctx, "alpha", group, key, portA, []string{fmt.Sprintf("127.0.0.1:%d", portB)})
	_, clipB := startNode(t, ctx, "beta", group, key, portB, nil)

	src := t.TempDir()
	big := make([]byte, (1<<20)+1) // one byte over startNode's MaxFileBytes
	if err := os.WriteFile(filepath.Join(src, "big.bin"), big, 0o644); err != nil {
		t.Fatal(err)
	}
	clipA.setFiles([]string{filepath.Join(src, "big.bin")})

	time.Sleep(400 * time.Millisecond)
	if got := a.sent.Load(); got != 0 {
		t.Fatalf("an oversized file copy was sent %d times", got)
	}
	if got := clipB.getFiles(); len(got) != 0 {
		t.Fatalf("beta received %v", got)
	}
}

// A files payload arrives as protocol v2; text stays v1 so old devices keep
// syncing text. This sends a hand-built v2 frame end to end.
func TestFilesArriveOverTheWireAsV2(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	group, key := "test-group", groupKey(t)
	port := freePort(t)
	n, clip := startNode(t, ctx, "alpha", group, key, port, nil)

	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "wire.txt"), []byte("framed"), 0o644); err != nil {
		t.Fatal(err)
	}
	data, names, _, err := pack.Build([]string{filepath.Join(src, "wire.txt")}, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	box := syncBox(t, group, key)
	resp := exchange(t, port, box, &Message{
		V: ProtocolVersionRich, Kind: KindClip, Device: "sender-id", Name: "sender",
		TS: time.Now().UnixMilli(), Format: FormatFiles, Names: names,
		Hash: secure.Hash(data), Data: data,
	})
	if resp.Err != "" {
		t.Fatalf("the v2 files frame was refused: %s", resp.Err)
	}
	waitFor(t, "the file to land on the clipboard", func() bool { return len(clip.getFiles()) == 1 })
	got, err := os.ReadFile(filepath.Join(n.receiveDir, "wire.txt"))
	if err != nil || string(got) != "framed" {
		t.Fatalf("wire.txt = %q, %v", got, err)
	}
}

func TestTextTravelsAsV1(t *testing.T) {
	if v := versionFor(FormatText); v != ProtocolVersion {
		t.Fatalf("text travels as v%d; older devices would stop receiving it", v)
	}
	for _, f := range []string{FormatFiles, FormatImage} {
		if v := versionFor(f); v != ProtocolVersionRich {
			t.Fatalf("%s travels as v%d, want v%d so old devices refuse it whole", f, v, ProtocolVersionRich)
		}
	}
}

// Two daemons sharing one clipboard -- the single-box testing setup the
// README describes -- must not lob a file copy back and forth forever. Text
// never had this problem because its bytes survive the trip; files come back
// renamed and re-homed, so their identity has to be carried by their content.
func TestTwoDaemonsOnOneClipboardDoNotStormOverFiles(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	group, key := "test-group", groupKey(t)
	portA, portB := freePort(t), freePort(t)

	// One clipboard, two nodes: startNode would make two, so build by hand.
	shared := &fakeClipboard{}
	mk := func(name string, port, peer int) *Node {
		cfg := &config.Config{
			GroupID: group, Key: key, DeviceID: name + "-id", DeviceName: name,
			ListenPort: port, PollMillis: 20, MaxBytes: 1 << 20, MaxFileBytes: 1 << 20,
			Peers:      []string{fmt.Sprintf("127.0.0.1:%d", peer)},
			ReceiveDir: t.TempDir(),
		}
		n, err := NewWith(cfg, quietLogger(), shared)
		if err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { done <- n.Run(ctx) }()
		t.Cleanup(func() {
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				t.Log("node did not shut down in time")
			}
		})
		waitForPort(t, port)
		return n
	}
	a := mk("alpha", portA, portB)
	b := mk("beta", portB, portA)

	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "travel.txt"), []byte("once only"), 0o644); err != nil {
		t.Fatal(err)
	}
	shared.setFiles([]string{filepath.Join(src, "travel.txt")})

	// Let it settle. Without the content guard this window produces a dozen
	// laps and a receive dir full of "travel (2) (2) (2).txt".
	time.Sleep(1200 * time.Millisecond)

	for name, n := range map[string]*Node{"alpha": a, "beta": b} {
		entries, err := os.ReadDir(n.receiveDir)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) > 1 {
			var names []string
			for _, e := range entries {
				names = append(names, e.Name())
			}
			t.Fatalf("%s's receive dir filled with duplicates: %v", name, names)
		}
	}
	if total := a.sent.Load() + b.sent.Load(); total > 3 {
		t.Fatalf("the copy went around %d times; the content guard is not holding", total)
	}
}
