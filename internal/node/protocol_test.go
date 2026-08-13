package node

import (
	"bytes"
	"encoding/binary"
	"io"
	"testing"
	"time"

	"github.com/justin06lee/henri/internal/secure"
)

func testBox(t *testing.T) *secure.Box {
	t.Helper()
	box, err := secure.NewBox(bytes.Repeat([]byte{7}, 32), "group", secure.InfoSync)
	if err != nil {
		t.Fatal(err)
	}
	return box
}

func TestFrameRoundTrip(t *testing.T) {
	box := testBox(t)
	want := &Message{
		V: ProtocolVersion, Kind: KindClip, Device: "dev", Name: "laptop",
		TS: time.Now().UnixMilli(), Data: []byte("payload"), Hash: "abc",
	}
	var buf bytes.Buffer
	if err := writeFrame(&buf, box, want); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(buf.Bytes(), []byte("payload")) {
		t.Fatal("the payload is readable on the wire")
	}
	got, nonce, err := readFrame(&buf, box, frameLimit(1<<20))
	if err != nil {
		t.Fatal(err)
	}
	if got.Kind != want.Kind || string(got.Data) != string(want.Data) || got.Device != want.Device {
		t.Fatalf("round trip changed the message: %+v", got)
	}
	if len(nonce) != nonceSize {
		t.Fatalf("readFrame returned a %d byte nonce, want %d", len(nonce), nonceSize)
	}
}

func TestReadFrameRejectsHugeLength(t *testing.T) {
	var buf bytes.Buffer
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], maxFrame+1)
	buf.Write(hdr[:])
	if _, _, err := readFrame(&buf, testBox(t), frameLimit(1<<20)); err == nil {
		t.Fatal("a frame larger than the limit was accepted")
	}
}

func TestReadFrameRejectsEmptyFrame(t *testing.T) {
	var buf bytes.Buffer
	buf.Write([]byte{0, 0, 0, 0})
	if _, _, err := readFrame(&buf, testBox(t), frameLimit(1<<20)); err == nil {
		t.Fatal("a zero-length frame was accepted")
	}
}

func TestReadFrameSpeaksBothVersionsAndNoOthers(t *testing.T) {
	box := testBox(t)
	// v1 (text, and every older device) and v2 (files and images) both pass;
	// anything else is refused whole.
	for _, v := range []int{ProtocolVersion, ProtocolVersionRich} {
		var buf bytes.Buffer
		if err := writeFrame(&buf, box, &Message{V: v, Kind: KindAck}); err != nil {
			t.Fatal(err)
		}
		if _, _, err := readFrame(&buf, box, frameLimit(1<<20)); err != nil {
			t.Fatalf("protocol v%d was refused: %v", v, err)
		}
	}
	var buf bytes.Buffer
	if err := writeFrame(&buf, box, &Message{V: ProtocolVersionRich + 1, Kind: KindAck}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := readFrame(&buf, box, frameLimit(1<<20)); err == nil {
		t.Fatal("a message from a future protocol version was accepted")
	}
}

func TestCheckFresh(t *testing.T) {
	if err := checkFresh(time.Now().UnixMilli()); err != nil {
		t.Fatalf("a current timestamp was rejected: %v", err)
	}
	if err := checkFresh(0); err == nil {
		t.Fatal("a missing timestamp was accepted")
	}
	if err := checkFresh(time.Now().Add(-time.Hour).UnixMilli()); err == nil {
		t.Fatal("an hour-old timestamp was accepted")
	}
	if err := checkFresh(time.Now().Add(time.Hour).UnixMilli()); err == nil {
		t.Fatal("a timestamp an hour in the future was accepted")
	}
}

// countingReader records how much of a body was actually read.
type countingReader struct{ n int }

func (c *countingReader) Read(p []byte) (int, error) {
	c.n += len(p)
	for i := range p {
		p[i] = 'x'
	}
	return len(p), nil
}

// The length prefix arrives before the sender has proved anything at all, so a
// frame over the limit has to be refused on the strength of the header alone.
func TestOversizedFrameIsRefusedWithoutReadingIt(t *testing.T) {
	limit := frameLimit(1 << 20)
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(limit)+1)

	var body countingReader
	r := io.MultiReader(bytes.NewReader(hdr[:]), &body)
	if _, _, err := readFrame(r, testBox(t), limit); err == nil {
		t.Fatal("a frame over the limit was accepted")
	}
	if body.n != 0 {
		t.Fatalf("%d bytes were read from a frame that had already been refused", body.n)
	}
}

// And a frame that is within the limit but never arrives must cost only what
// was actually sent, not what was announced.
func TestFrameShorterThanItClaimsIsRefused(t *testing.T) {
	limit := frameLimit(1 << 20)
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(limit))

	r := io.MultiReader(bytes.NewReader(hdr[:]), bytes.NewReader([]byte("hello")))
	if _, _, err := readFrame(r, testBox(t), limit); err == nil {
		t.Fatal("a frame that stopped short of its own length was accepted")
	}
}

func TestFrameLimitFollowsThePayloadLimit(t *testing.T) {
	if got := frameLimit(4 << 20); got >= maxFrame {
		t.Fatalf("a 4 MiB payload limit allows a %d byte frame", got)
	}
	if got := frameLimit(4 << 20); got <= 4<<20 {
		t.Fatalf("a 4 MiB payload does not fit in a %d byte frame once it is base64", got)
	}
	if got := frameLimit(0); got != frameFloor {
		t.Fatalf("frameLimit(0) gave %d, want the floor %d", got, frameFloor)
	}
}

func TestReplayCacheRemembersAndForgets(t *testing.T) {
	c := newReplayCache(4)
	if !c.fresh([]byte("one")) {
		t.Fatal("the first sighting of a nonce was called a replay")
	}
	if c.fresh([]byte("one")) {
		t.Fatal("the same nonce was accepted twice")
	}
	// It stays bounded whatever anyone sends.
	for i := range 100 {
		c.fresh([]byte{byte(i), byte(i >> 8), 9})
	}
	if len(c.seen) > 4 {
		t.Fatalf("the cache grew to %d entries, past its cap of 4", len(c.seen))
	}
}
