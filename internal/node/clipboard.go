package node

import "github.com/justin06lee/henri/internal/clipboard"

// Clipboard is the slice of the system clipboard the daemon depends on.
// Keeping it an interface lets the tests run a whole group of nodes without
// touching (or clobbering) the real clipboard.
type Clipboard interface {
	Read() ([]byte, error)
	// ReadPrimary returns the highlighted text, where the platform tracks it
	// separately from the clipboard.
	ReadPrimary() ([]byte, error)
	Write(data []byte) error
	// Name identifies the backing tool, for `henri status`.
	Name() string
}

type systemClipboard struct{}

func (systemClipboard) Read() ([]byte, error)        { return clipboard.Read() }
func (systemClipboard) ReadPrimary() ([]byte, error) { return clipboard.ReadPrimary() }
func (systemClipboard) Write(d []byte) error         { return clipboard.Write(d) }
func (systemClipboard) Name() string                 { return clipboard.Tool() }
