package node

import (
	"bytes"
	"encoding/binary"
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
	got, err := readFrame(&buf, box)
	if err != nil {
		t.Fatal(err)
	}
	if got.Kind != want.Kind || string(got.Data) != string(want.Data) || got.Device != want.Device {
		t.Fatalf("round trip changed the message: %+v", got)
	}
}

func TestReadFrameRejectsHugeLength(t *testing.T) {
	var buf bytes.Buffer
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], maxFrame+1)
	buf.Write(hdr[:])
	if _, err := readFrame(&buf, testBox(t)); err == nil {
		t.Fatal("a frame larger than the limit was accepted")
	}
}

func TestReadFrameRejectsEmptyFrame(t *testing.T) {
	var buf bytes.Buffer
	buf.Write([]byte{0, 0, 0, 0})
	if _, err := readFrame(&buf, testBox(t)); err == nil {
		t.Fatal("a zero-length frame was accepted")
	}
}

func TestReadFrameRejectsOtherProtocolVersion(t *testing.T) {
	box := testBox(t)
	var buf bytes.Buffer
	if err := writeFrame(&buf, box, &Message{V: ProtocolVersion + 1, Kind: KindAck}); err != nil {
		t.Fatal(err)
	}
	if _, err := readFrame(&buf, box); err == nil {
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
