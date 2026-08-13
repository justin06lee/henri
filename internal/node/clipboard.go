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
	// Look reads whatever the clipboard holds: text, file references, or an
	// image.
	Look() (clipboard.Snapshot, error)
	// WriteFiles puts references to local files on the clipboard.
	WriteFiles(paths []string) error
	// WriteImage puts PNG data on the clipboard.
	WriteImage(png []byte) error
	// Name identifies the backing tool, for `henri status`.
	Name() string
}

type systemClipboard struct{}

func (systemClipboard) Read() ([]byte, error)             { return clipboard.Read() }
func (systemClipboard) ReadPrimary() ([]byte, error)      { return clipboard.ReadPrimary() }
func (systemClipboard) Write(d []byte) error              { return clipboard.Write(d) }
func (systemClipboard) Look() (clipboard.Snapshot, error) { return clipboard.Look() }
func (systemClipboard) WriteFiles(paths []string) error   { return clipboard.WriteFiles(paths) }
func (systemClipboard) WriteImage(png []byte) error       { return clipboard.WriteImage(png) }
func (systemClipboard) Name() string                      { return clipboard.Tool() }
