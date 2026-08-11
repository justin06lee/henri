package secure

import (
	"bytes"
	"crypto/rand"
	"testing"
)

func testKey(t *testing.T) []byte {
	t.Helper()
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		t.Fatal(err)
	}
	return k
}

func TestSealOpenRoundTrip(t *testing.T) {
	box, err := NewBox(testKey(t), "group-1", InfoSync)
	if err != nil {
		t.Fatal(err)
	}
	want := []byte("something copied")
	sealed, err := box.Seal(want)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(sealed, want) {
		t.Fatal("plaintext is visible in the sealed message")
	}
	got, err := box.Open(sealed)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("round trip gave %q, want %q", got, want)
	}
}

func TestOpenRejectsWrongKey(t *testing.T) {
	a, err := NewBox(testKey(t), "group-1", InfoSync)
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewBox(testKey(t), "group-1", InfoSync)
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := a.Seal([]byte("secret"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Open(sealed); err != ErrBadCiphertext {
		t.Fatalf("a different group key opened the message: %v", err)
	}
}

// A sync message must not open as a discovery message even though both derive
// from the same master secret.
func TestPurposesAreSeparated(t *testing.T) {
	master := testKey(t)
	syncBox, err := NewBox(master, "group-1", InfoSync)
	if err != nil {
		t.Fatal(err)
	}
	discoBox, err := NewBox(master, "group-1", InfoDiscovery)
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := syncBox.Seal([]byte("clipboard payload"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := discoBox.Open(sealed); err != ErrBadCiphertext {
		t.Fatalf("discovery key opened a sync message: %v", err)
	}
}

// The same secret used by two groups must still not cross over.
func TestGroupIsBoundIn(t *testing.T) {
	master := testKey(t)
	a, err := NewBox(master, "group-a", InfoSync)
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewBox(master, "group-b", InfoSync)
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := a.Seal([]byte("hello"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Open(sealed); err != ErrBadCiphertext {
		t.Fatalf("group-b opened group-a's message: %v", err)
	}
}

func TestOpenRejectsTampering(t *testing.T) {
	box, err := NewBox(testKey(t), "group-1", InfoSync)
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := box.Seal([]byte("original"))
	if err != nil {
		t.Fatal(err)
	}
	sealed[len(sealed)-1] ^= 0x01
	if _, err := box.Open(sealed); err != ErrBadCiphertext {
		t.Fatalf("tampered message was accepted: %v", err)
	}
}

func TestOpenRejectsShortInput(t *testing.T) {
	box, err := NewBox(testKey(t), "group-1", InfoSync)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := box.Open([]byte{1, 2, 3}); err != ErrBadCiphertext {
		t.Fatalf("short input was accepted: %v", err)
	}
}

func TestNewBoxRejectsWrongKeySize(t *testing.T) {
	if _, err := NewBox(make([]byte, 16), "g", InfoSync); err == nil {
		t.Fatal("a 16-byte master key was accepted")
	}
}
