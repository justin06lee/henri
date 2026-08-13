package clipboard

// A live round trip through the real system clipboard, for the machine of
// whoever is working on this package. It clobbers the actual clipboard, which
// ordinary test runs must never do, so it only runs when asked:
//
//     HENRI_LIVE_CLIPBOARD=1 go test ./internal/clipboard -run TestLive -v

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// settleLook polls Look until want says yes, invalidating the change-count
// cache between attempts so each look is a real read.
func settleLook(t *testing.T, want func(Snapshot) bool) Snapshot {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		snap, err := Look()
		if err == nil && want(snap) {
			return snap
		}
		if time.Now().After(deadline) {
			t.Fatalf("the clipboard never settled: %+v, %v", snap, err)
		}
		invalidateLook()
		time.Sleep(100 * time.Millisecond)
	}
}

func TestLiveRichClipboardRoundTrip(t *testing.T) {
	if os.Getenv("HENRI_LIVE_CLIPBOARD") == "" {
		t.Skip("set HENRI_LIVE_CLIPBOARD=1 to run against the real clipboard")
	}
	if err := Available(); err != nil {
		t.Skipf("no clipboard here: %v", err)
	}
	saved, _ := Read()
	t.Cleanup(func() { _ = Write(saved) })

	dir := t.TempDir()
	f1 := filepath.Join(dir, "a.txt")
	f2 := filepath.Join(dir, "b.txt")
	for _, f := range []string{f1, f2} {
		if err := os.WriteFile(f, []byte("live"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	if err := WriteFiles([]string{f1, f2}); err != nil {
		t.Fatalf("WriteFiles: %v", err)
	}
	// A process reading within milliseconds of the writer's exit can see a
	// partial view while the pasteboard server adopts the items; it settles
	// almost immediately. The daemon never reads its own writes that fast --
	// polls are hundreds of milliseconds apart -- so the test waits the way
	// the world does.
	snap := settleLook(t, func(s Snapshot) bool { return s.Kind == ContentFiles && len(s.Paths) == 2 })
	if snap.Paths[0] != f1 || snap.Paths[1] != f2 {
		t.Fatalf("files round trip = %+v", snap)
	}
	if again, err := Look(); err != nil || again.Kind != ContentFiles {
		t.Fatalf("unchanged clipboard re-look = %+v, %v", again, err)
	}

	// A tiny valid PNG: 1x1 transparent pixel.
	png := []byte{
		0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a,
		0, 0, 0, 0x0d, 'I', 'H', 'D', 'R', 0, 0, 0, 1, 0, 0, 0, 1,
		8, 6, 0, 0, 0, 0x1f, 0x15, 0xc4, 0x89,
		0, 0, 0, 0x0a, 'I', 'D', 'A', 'T',
		0x78, 0x9c, 0x63, 0x00, 0x01, 0x00, 0x00, 0x05, 0x00, 0x01,
		0x0d, 0x0a, 0x2d, 0xb4,
		0, 0, 0, 0, 'I', 'E', 'N', 'D', 0xae, 0x42, 0x60, 0x82,
	}
	if err := WriteImage(png); err != nil {
		t.Fatalf("WriteImage: %v", err)
	}
	snap = settleLook(t, func(s Snapshot) bool { return s.Kind == ContentImage })
	if !bytes.Equal(snap.Image, png) {
		t.Fatalf("image round trip: %d bytes back, want %d", len(snap.Image), len(png))
	}

	if err := Write([]byte("plain again")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	snap = settleLook(t, func(s Snapshot) bool { return s.Kind == ContentText && string(s.Text) == "plain again" })
	_ = snap
}
