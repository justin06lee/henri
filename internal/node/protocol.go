package node

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/justin06lee/henri/internal/secure"
)

// ProtocolVersion is bumped on any incompatible wire change.
//
// Text still travels as v1 so groups with older devices keep syncing text.
// Files and images travel as v2, which an old device refuses whole -- far
// better than the alternative, an old device pasting a tar archive into a
// document as if it were text.
const (
	ProtocolVersion     = 1
	ProtocolVersionRich = 2
)

// Message kinds.
const (
	KindClip   = "clip"   // a new clipboard payload, pushed to peers over TCP
	KindHello  = "hello"  // a presence beacon, broadcast over UDP
	KindStatus = "status" // ask the local daemon what it is doing
	KindPush   = "push"   // ask the local daemon to re-send the clipboard
	KindAck    = "ack"    // generic success reply
	KindState  = "state"  // reply to KindStatus
)

// Clip payload formats. Text is the empty string because that is what every
// frame said before the field existed.
const (
	FormatText  = ""
	FormatFiles = "files"     // Data is a gzip'd tar; Names are its top-level entries
	FormatImage = "image/png" // Data is one PNG
)

// versionFor is the wire version a clip payload of this format travels as.
func versionFor(format string) int {
	if format == FormatText {
		return ProtocolVersion
	}
	return ProtocolVersionRich
}

// maxFrame is the hard ceiling on a single TCP frame. Nothing henri sends comes
// close to it; frameLimit is what an inbound frame is really held to.
const maxFrame = 64 << 20 // 64 MiB

// frameSlack is what a frame may cost on top of the payload it carries: JSON
// base64-encodes the bytes, which is a third again, and the envelope adds
// device names, a hash and a GCM tag.
const frameSlack = 64 << 10

// frameFloor is the smallest limit worth applying. A status reply carries the
// whole peer list and no clipboard at all, so it is bounded by this rather than
// by anyone's payload limit.
const frameFloor = 256 << 10

// nonceSize is the length of the random prefix secure.Seal puts in front of
// every frame. It is unique per frame, which is what makes it a replay key.
const nonceSize = 12

// Message is the single envelope for everything henri sends. It is JSON so the
// protocol stays inspectable, and it is only ever seen encrypted on the wire.
type Message struct {
	V      int    `json:"v"`
	Kind   string `json:"kind"`
	Device string `json:"device"`
	Name   string `json:"name,omitempty"`
	// TS is unix milliseconds, used to reject stale replays.
	TS int64 `json:"ts"`

	// Hello, and clip: where to reach the sender. A clip without it teaches the
	// receiver nothing, so sync only works in the direction discovery happens
	// to have found.
	Port int `json:"port,omitempty"`
	// Addr is the address a beacon was sent from. Devices built before this
	// field existed do not send it and are not held to it; when it is there it
	// has to match the datagram, which is what stops a captured beacon being
	// replayed from somewhere else to re-home a peer.
	Addr string `json:"addr,omitempty"`

	// Push: take the highlighted text rather than the clipboard.
	Primary bool `json:"primary,omitempty"`

	// Clip.
	Hash string `json:"hash,omitempty"`
	Data []byte `json:"data,omitempty"`
	// Format says what Data is: text when empty, or one of the Format
	// constants. Names are the file names inside a files payload, so the
	// receiver can log and show what arrived without opening the archive.
	// Count rides on a push ack so `henri send` can say how many went.
	Format string   `json:"format,omitempty"`
	Names  []string `json:"names,omitempty"`
	Count  int      `json:"count,omitempty"`

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
	WatchMode  string `json:"watch_mode,omitempty"`
	LastSyncAt int64  `json:"last_sync_at"`
	// LastHash is only the first few characters of the fingerprint. The whole
	// SHA-256 of something short and guessable -- a PIN, a password -- is not
	// something a status query should hand out.
	LastHash  string `json:"last_hash"`
	LastBytes int    `json:"last_bytes"`
	LastFrom  string `json:"last_from"`
	// LastFormat and LastCount say what the last sync was: text when empty,
	// or a format constant, with LastCount the number of files it carried.
	LastFormat string `json:"last_format,omitempty"`
	LastCount  int    `json:"last_count,omitempty"`
	Sent       int64  `json:"sent"`
	Received   int64  `json:"received"`
	// Beacons counts the discovery beacons heard from other devices, and
	// LastBeaconAt is when the most recent one arrived. A daemon that has been
	// up for hours with no beacons is deaf, however healthy it otherwise looks.
	Beacons      int64      `json:"beacons,omitempty"`
	LastBeaconAt int64      `json:"last_beacon_at,omitempty"`
	Peers        []PeerInfo `json:"peers"`
}

// PeerInfo describes one known device.
type PeerInfo struct {
	Device     string `json:"device"`
	Name       string `json:"name"`
	Addr       string `json:"addr"`
	LastSeenAt int64  `json:"last_seen_at"`
	Source     string `json:"source"` // "config" or "discovered"
}

// frameLimit turns a configured payload limit into the largest frame worth
// reading. The length prefix arrives long before the sender has proved it holds
// the group key, so it is held to what this device would ever agree to accept
// rather than to a number with room for a film in it.
func frameLimit(maxBytes int) int {
	limit := frameFloor
	if maxBytes > 0 {
		limit = maxBytes + maxBytes/3 + frameSlack
	}
	if limit < frameFloor {
		limit = frameFloor
	}
	if limit > maxFrame || limit < 0 { // < 0 only on an absurd configuration
		limit = maxFrame
	}
	return limit
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
	// One buffer, one write: the header and the body as separate writes are two
	// syscalls and, with Nagle off, two segments for every message.
	out := make([]byte, 4+len(sealed))
	binary.BigEndian.PutUint32(out[:4], uint32(len(sealed)))
	copy(out[4:], sealed)
	_, err = w.Write(out)
	return err
}

// readFrame reads and opens one frame, refusing anything bigger than limit. It
// returns the frame's nonce alongside the message so the caller can remember it
// and refuse the same frame twice.
func readFrame(r io.Reader, box *secure.Box, limit int) (*Message, []byte, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, nil, err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n == 0 {
		return nil, nil, errors.New("henri: empty frame")
	}
	if limit <= 0 || limit > maxFrame {
		limit = maxFrame
	}
	if n > uint32(limit) {
		return nil, nil, fmt.Errorf("henri: peer announced a %d byte frame, refusing", n)
	}
	// Grow into the frame rather than allocating what it claims up front. A
	// stranger can announce the largest frame allowed and then send five bytes;
	// doing what they say costs the whole allocation before they have proved
	// anything at all, and a few dozen sockets doing it exhausts the machine.
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(io.LimitReader(r, int64(n))); err != nil {
		return nil, nil, err
	}
	if buf.Len() != int(n) {
		return nil, nil, io.ErrUnexpectedEOF
	}
	sealed := buf.Bytes()
	plain, err := box.Open(sealed)
	if err != nil {
		return nil, nil, err
	}
	var msg Message
	if err := json.Unmarshal(plain, &msg); err != nil {
		return nil, nil, fmt.Errorf("henri: malformed message: %w", err)
	}
	if msg.V != ProtocolVersion && msg.V != ProtocolVersionRich {
		return nil, nil, fmt.Errorf("henri: peer speaks protocol v%d, this build speaks v%d", msg.V, ProtocolVersionRich)
	}
	// Open has already established the frame is at least a nonce and a tag.
	return &msg, append([]byte(nil), sealed[:nonceSize]...), nil
}

// replayCache remembers the nonces of frames that have already been acted on.
//
// Freshness on its own leaves a window the length of the clock-skew allowance
// in which a captured frame can be delivered again, byte for byte: long enough
// to put an old clipboard back at the moment someone pastes, or to re-home a
// peer to an attacker with a replayed beacon. Every frame carries a fresh
// random nonce, so remembering nonces for the length of that window closes it.
type replayCache struct {
	mu   sync.Mutex
	seen map[string]time.Time
	max  int
}

func newReplayCache(max int) *replayCache {
	return &replayCache{seen: make(map[string]time.Time), max: max}
}

// fresh records a nonce and reports whether it had not been seen before.
func (c *replayCache) fresh(nonce []byte) bool {
	if len(nonce) == 0 {
		return false
	}
	key := string(nonce)
	now := time.Now()

	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.seen[key]; ok {
		return false
	}
	if len(c.seen) >= c.max {
		for k, t := range c.seen {
			if now.Sub(t) > freshness {
				delete(c.seen, k)
			}
		}
	}
	if len(c.seen) >= c.max {
		// Still full, so something is flooding us. Drop the oldest rather than
		// grow: forgetting a nonce only costs the protection freshness gave us
		// before this cache existed.
		var oldest string
		at := now
		for k, t := range c.seen {
			if oldest == "" || t.Before(at) {
				oldest, at = k, t
			}
		}
		delete(c.seen, oldest)
	}
	c.seen[key] = now
	return true
}
