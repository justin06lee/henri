package node

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/justin06lee/henri/internal/secure"
)

// ProtocolVersion is bumped on any incompatible wire change.
const ProtocolVersion = 1

// Message kinds.
const (
	KindClip   = "clip"   // a new clipboard payload, pushed to peers over TCP
	KindHello  = "hello"  // a presence beacon, broadcast over UDP
	KindStatus = "status" // ask the local daemon what it is doing
	KindPush   = "push"   // ask the local daemon to re-send the clipboard
	KindAck    = "ack"    // generic success reply
	KindState  = "state"  // reply to KindStatus
)

// maxFrame caps a single TCP frame so a peer cannot make us allocate without
// bound. It is deliberately larger than the configured payload limit; the
// payload limit itself is enforced separately and with a clearer error.
const maxFrame = 64 << 20 // 64 MiB

// Message is the single envelope for everything henri sends. It is JSON so the
// protocol stays inspectable, and it is only ever seen encrypted on the wire.
type Message struct {
	V      int    `json:"v"`
	Kind   string `json:"kind"`
	Device string `json:"device"`
	Name   string `json:"name,omitempty"`
	// TS is unix milliseconds, used to reject stale replays.
	TS int64 `json:"ts"`

	// Hello.
	Port int `json:"port,omitempty"`

	// Push: take the highlighted text rather than the clipboard.
	Primary bool `json:"primary,omitempty"`

	// Clip.
	Hash string `json:"hash,omitempty"`
	Data []byte `json:"data,omitempty"`

	// Replies.
	State *State `json:"state,omitempty"`
	Err   string `json:"err,omitempty"`
}

// State is what `henri status` prints.
type State struct {
	Device     string `json:"device"`
	Name       string `json:"name"`
	Group      string `json:"group"`
	PID        int    `json:"pid"`
	StartedAt  int64  `json:"started_at"`
	ListenPort int    `json:"listen_port"`
	Discovery  bool   `json:"discovery"`
	Tool       string `json:"clipboard_tool"`
	// ClipboardErr is non-empty when reads are failing -- usually a daemon
	// running outside the graphical session.
	ClipboardErr string `json:"clipboard_error,omitempty"`
	// WatchMode is how local copies are noticed: an event source, or a timer.
	WatchMode  string     `json:"watch_mode,omitempty"`
	LastSyncAt int64      `json:"last_sync_at"`
	LastHash   string     `json:"last_hash"`
	LastBytes  int        `json:"last_bytes"`
	LastFrom   string     `json:"last_from"`
	Sent       int64      `json:"sent"`
	Received   int64      `json:"received"`
	Peers      []PeerInfo `json:"peers"`
}

// PeerInfo describes one known device.
type PeerInfo struct {
	Device     string `json:"device"`
	Name       string `json:"name"`
	Addr       string `json:"addr"`
	LastSeenAt int64  `json:"last_seen_at"`
	Source     string `json:"source"` // "config" or "discovered"
}

// writeFrame seals msg and writes it as a length-prefixed frame.
func writeFrame(w io.Writer, box *secure.Box, msg *Message) error {
	plain, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	sealed, err := box.Seal(plain)
	if err != nil {
		return err
	}
	if len(sealed) > maxFrame {
		return fmt.Errorf("henri: frame of %d bytes exceeds limit", len(sealed))
	}
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(sealed)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	_, err = w.Write(sealed)
	return err
}

// readFrame reads and opens one frame.
func readFrame(r io.Reader, box *secure.Box) (*Message, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n == 0 {
		return nil, errors.New("henri: empty frame")
	}
	if n > maxFrame {
		return nil, fmt.Errorf("henri: peer announced a %d byte frame, refusing", n)
	}
	sealed := make([]byte, n)
	if _, err := io.ReadFull(r, sealed); err != nil {
		return nil, err
	}
	plain, err := box.Open(sealed)
	if err != nil {
		return nil, err
	}
	var msg Message
	if err := json.Unmarshal(plain, &msg); err != nil {
		return nil, fmt.Errorf("henri: malformed message: %w", err)
	}
	if msg.V != ProtocolVersion {
		return nil, fmt.Errorf("henri: peer speaks protocol v%d, this build speaks v%d", msg.V, ProtocolVersion)
	}
	return &msg, nil
}
