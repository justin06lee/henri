package node

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/justin06lee/henri/internal/config"
	"github.com/justin06lee/henri/internal/secure"
)

// fakeClipboard stands in for the system clipboard so tests never touch the
// developer's real one.
type fakeClipboard struct {
	mu      sync.Mutex
	data    []byte
	primary []byte
}

func (f *fakeClipboard) Read() ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]byte(nil), f.data...), nil
}

func (f *fakeClipboard) Write(d []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.data = append([]byte(nil), d...)
	return nil
}

func (f *fakeClipboard) ReadPrimary() ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]byte(nil), f.primary...), nil
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
	if _, err := readFrame(conn, box); err == nil {
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
	resp, err := readFrame(conn, box)
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
	resp, err := readFrame(conn, box)
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
		PollMillis: 20, MaxBytes: 1 << 20,
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
