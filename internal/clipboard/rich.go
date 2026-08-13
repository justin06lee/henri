package clipboard

// This file is the clipboard beyond text: file copies and images.
//
// Every platform publishes both on the same clipboard text travels through,
// just under different names -- file URLs and PNG data on the macOS
// pasteboard, text/uri-list and image/png targets on X11 and Wayland, a file
// drop list and a bitmap on Windows. All of it is reachable through tools the
// platform ships: osascript's ObjC bridge, xclip and wl-paste's -t flags, and
// PowerShell. Nothing here adds a dependency.

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
)

// Snapshot is one look at the clipboard: what kind of thing it holds, and the
// thing itself. Exactly one field is set for its Kind.
type Snapshot struct {
	Kind  ContentKind
	Text  []byte
	Paths []string // absolute paths, in clipboard order
	Image []byte   // one PNG
}

// ContentKind says what a clipboard holds.
type ContentKind int

const (
	// ContentText covers text, an empty clipboard, and everything henri does
	// not sync; the Text field alone is authoritative.
	ContentText ContentKind = iota
	ContentFiles
	ContentImage
)

// Look reads the clipboard, whatever it holds. Files win over an image and an
// image over text, which is the order the platforms themselves imply: a
// Finder copy of a JPEG puts both the file reference and the file's name on
// the pasteboard, and the reference is the copy's meaning.
func Look() (Snapshot, error) {
	switch runtime.GOOS {
	case "darwin":
		return lookDarwin()
	case "windows":
		return lookWindows()
	default:
		return lookUnix()
	}
}

// WriteFiles puts references to these files on the clipboard, so a paste in
// the file manager lands them. The files themselves stay where they are.
func WriteFiles(paths []string) error {
	if len(paths) == 0 {
		return errors.New("clipboard: no files to write")
	}
	switch runtime.GOOS {
	case "darwin":
		return writeFilesDarwin(paths)
	case "windows":
		return writeFilesWindows(paths)
	default:
		return writeFilesUnix(paths)
	}
}

// WriteImage puts PNG data on the clipboard, so a paste into an editor lands
// pixels.
func WriteImage(png []byte) error {
	if len(png) == 0 {
		return errors.New("clipboard: no image to write")
	}
	switch runtime.GOOS {
	case "darwin":
		return writeImageDarwin(png)
	case "windows":
		return writeImageWindows(png)
	default:
		return writeImageUnix(png)
	}
}

// --- darwin -----------------------------------------------------------------

// The pasteboard is reached through osascript's JavaScript-for-Automation
// bridge: pbpaste speaks only text, and AppKit needs cgo, which henri does
// not take. Each operation is one short-lived script.

// lookScript reports what the pasteboard holds. Its first line is
// "<kind> <changeCount>"; files and images follow on later lines. argv[0] is
// the changeCount of the previous look: when nothing has changed the script
// says so without reading anything, which makes polling nearly free -- and is
// something pbpaste-based polling never had.
const lookScript = `ObjC.import("AppKit");
function run(argv) {
  const pb = $.NSPasteboard.generalPasteboard;
  const count = pb.changeCount;
  if (argv.length > 0 && argv[0] !== "" && Number(argv[0]) === count) return "same " + count;
  const opts = $.NSMutableDictionary.alloc.init;
  opts.setObjectForKey(true, $.NSPasteboardURLReadingFileURLsOnlyKey);
  const urls = pb.readObjectsForClassesOptions($([$.NSURL.class]), opts);
  if (!urls.isNil() && urls.count > 0) {
    const lines = [];
    for (let i = 0; i < urls.count; i++) lines.push(urls.objectAtIndex(i).path.js);
    return "files " + count + "\n" + lines.join("\n");
  }
  let png = pb.dataForType($.NSPasteboardTypePNG);
  if (png.isNil()) {
    const tiff = pb.dataForType($.NSPasteboardTypeTIFF);
    if (!tiff.isNil()) {
      const rep = $.NSBitmapImageRep.imageRepWithData(tiff);
      if (!rep.isNil()) png = rep.representationUsingTypeProperties($.NSBitmapImageFileTypePNG, $.NSDictionary.dictionary);
    }
  }
  if (!png.isNil() && png.length > 0) return "image " + count + "\n" + png.base64EncodedStringWithOptions(0).js;
  return "text " + count;
}`

// writeFilesScript puts file references on the pasteboard, as a Finder copy
// would.
//
// Not writeObjects with NSURLs, although that is the modern way: each URL
// becomes its own pasteboard item, and items past the first are lost if the
// writing process exits before the pasteboard server has taken them -- which
// a one-shot script does, and which loses files on a coin flip. The legacy
// filenames type is one plist in one item, durable the moment it is set, and
// the URL-reading API and Finder both read it exactly as if the URLs had been
// written the modern way.
const writeFilesScript = `ObjC.import("AppKit");
function run(argv) {
  const pb = $.NSPasteboard.generalPasteboard;
  pb.declareTypesOwner($(["NSFilenamesPboardType"]), null);
  pb.setPropertyListForType($(argv), "NSFilenamesPboardType");
  return "";
}`

// writeImageScript reads a PNG from the path in argv[0] and puts it on the
// pasteboard as both PNG and TIFF: some paste targets still only take TIFF,
// and the pasteboard serves whichever flavour the app asks for.
const writeImageScript = `ObjC.import("AppKit");
function run(argv) {
  const png = $.NSData.dataWithContentsOfFile(argv[0]);
  if (png.isNil()) throw new Error("could not read the image");
  const pb = $.NSPasteboard.generalPasteboard;
  pb.clearContents;
  if (!pb.setDataForType(png, $.NSPasteboardTypePNG)) throw new Error("the pasteboard refused the image");
  const img = $.NSImage.alloc.initWithData(png);
  if (!img.isNil()) pb.setDataForType(img.TIFFRepresentation, $.NSPasteboardTypeTIFF);
  return "";
}`

// lastLook caches the changeCount and result of the previous darwin look
// under resolveMu, so an unchanged pasteboard costs one cheap script and no
// content transfer.
var lastLook struct {
	count string
	snap  Snapshot
	valid bool
}

func lookDarwin() (Snapshot, error) {
	resolveMu.Lock()
	prev := ""
	if lastLook.valid {
		prev = lastLook.count
	}
	resolveMu.Unlock()

	out, err := runOsascript(lookScript, prev)
	if err != nil {
		return Snapshot{}, err
	}
	kind, count, body, err := parseLook(out)
	if err != nil {
		return Snapshot{}, err
	}

	var snap Snapshot
	switch kind {
	case "same":
		resolveMu.Lock()
		defer resolveMu.Unlock()
		if lastLook.valid {
			return lastLook.snap, nil
		}
		return Snapshot{}, errors.New("clipboard: nothing cached for an unchanged pasteboard")
	case "files":
		snap = Snapshot{Kind: ContentFiles, Paths: splitLines(body)}
	case "image":
		img, err := base64.StdEncoding.DecodeString(strings.TrimSpace(body))
		if err != nil {
			return Snapshot{}, fmt.Errorf("clipboard: image from pasteboard: %w", err)
		}
		snap = Snapshot{Kind: ContentImage, Image: img}
	case "text":
		// The text itself still comes from pbpaste: it is the reader every
		// copy of henri has ever agreed with about encoding and flavour.
		text, err := Read()
		if err != nil {
			return Snapshot{}, err
		}
		snap = Snapshot{Kind: ContentText, Text: text}
	default:
		return Snapshot{}, fmt.Errorf("clipboard: unexpected look answer %q", kind)
	}

	resolveMu.Lock()
	lastLook.count, lastLook.snap, lastLook.valid = count, snap, true
	resolveMu.Unlock()
	return snap, nil
}

// parseLook splits a look script's answer into its parts. Split out so the
// format is pinned by a test that needs no pasteboard.
func parseLook(out string) (kind, count, body string, err error) {
	head, body, _ := strings.Cut(out, "\n")
	kind, count, ok := strings.Cut(strings.TrimSpace(head), " ")
	if !ok || kind == "" || count == "" {
		return "", "", "", fmt.Errorf("clipboard: unexpected look answer %q", head)
	}
	if _, err := strconv.Atoi(count); err != nil {
		return "", "", "", fmt.Errorf("clipboard: unexpected look count %q", head)
	}
	return kind, count, body, nil
}

func writeFilesDarwin(paths []string) error {
	_, err := runOsascript(writeFilesScript, paths...)
	invalidateLook()
	return err
}

func writeImageDarwin(png []byte) error {
	f, err := os.CreateTemp("", "henri-image-*.png")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(png); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	_, err = runOsascript(writeImageScript, f.Name())
	invalidateLook()
	return err
}

// invalidateLook drops the changeCount cache after henri itself writes the
// pasteboard, so the next look reads what was actually written rather than
// trusting a count taken before the write.
func invalidateLook() {
	resolveMu.Lock()
	lastLook.valid = false
	resolveMu.Unlock()
}

func runOsascript(script string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	argv := append([]string{"-l", "JavaScript", "-e", script}, args...)
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, "osascript", argv...)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	cmd.WaitDelay = waitDelay
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return "", fmt.Errorf("clipboard: osascript: timed out after %s", timeout)
		}
		return "", fmt.Errorf("clipboard: osascript: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimRight(stdout.String(), "\n"), nil
}

// --- x11 and wayland --------------------------------------------------------

// On X11 and Wayland everything is a named target on the same selection, so
// files and images are the tools henri already drives, pointed at targets
// other than text. xsel has no target support and stays text-only.

func lookUnix() (Snapshot, error) {
	t, err := resolve()
	if err != nil {
		return Snapshot{}, err
	}
	targets, ok := listTargets(t)
	if !ok {
		text, err := Read()
		if err != nil {
			return Snapshot{}, err
		}
		return Snapshot{Kind: ContentText, Text: text}, nil
	}
	switch {
	case targets["text/uri-list"] || targets["x-special/gnome-copied-files"]:
		target := "text/uri-list"
		if !targets[target] {
			target = "x-special/gnome-copied-files"
		}
		raw, err := readTarget(t, target)
		if err != nil {
			return Snapshot{}, err
		}
		paths := parseURIList(string(raw))
		if len(paths) == 0 {
			// A uri-list of remote URLs, say. Not files henri can read.
			break
		}
		return Snapshot{Kind: ContentFiles, Paths: paths}, nil
	case targets["image/png"]:
		raw, err := readTarget(t, "image/png")
		if err != nil {
			return Snapshot{}, err
		}
		if len(raw) > 0 {
			return Snapshot{Kind: ContentImage, Image: raw}, nil
		}
	}
	text, err := Read()
	if err != nil {
		return Snapshot{}, err
	}
	return Snapshot{Kind: ContentText, Text: text}, nil
}

// listTargets asks what flavours the selection is offered in. ok is false
// where the backend cannot say (xsel), which means text.
func listTargets(t *tool) (map[string]bool, bool) {
	var bin string
	var args []string
	switch t.name {
	case "xclip":
		bin, args = "xclip", []string{"-selection", "clipboard", "-t", "TARGETS", "-o"}
	case "wl-clipboard":
		bin, args = "wl-paste", []string{"--list-types"}
	default:
		return nil, false
	}
	out, err := capture(t, bin, args)
	if err != nil {
		// An empty clipboard, or no owner: no targets is a fine answer.
		return map[string]bool{}, true
	}
	targets := map[string]bool{}
	for _, line := range splitLines(string(out)) {
		targets[strings.TrimSpace(line)] = true
	}
	return targets, true
}

func readTarget(t *tool, target string) ([]byte, error) {
	switch t.name {
	case "xclip":
		return capture(t, "xclip", []string{"-selection", "clipboard", "-t", target, "-o"})
	case "wl-clipboard":
		return capture(t, "wl-paste", []string{"--no-newline", "--type", target})
	}
	return nil, fmt.Errorf("clipboard: %s cannot read %s", t.name, target)
}

func writeTarget(t *tool, target string, data []byte) error {
	var cmd *exec.Cmd
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	switch t.name {
	case "xclip":
		cmd = exec.CommandContext(ctx, "xclip", "-selection", "clipboard", "-t", target, "-i")
	case "wl-clipboard":
		cmd = exec.CommandContext(ctx, "wl-copy", "--type", target)
	default:
		return fmt.Errorf("clipboard: %s cannot write %s", t.name, target)
	}
	cmd.Env = append(os.Environ(), t.env...)
	if err := writeWith(cmd, data, t.forks); err != nil {
		return fmt.Errorf("clipboard: write %s via %s: %w", target, t.name, err)
	}
	return nil
}

// capture runs one read helper to completion, with the package's timeout.
func capture(t *tool, bin string, args []string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = append(os.Environ(), t.env...)
	cmd.WaitDelay = waitDelay
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("clipboard: %s: timed out after %s", bin, timeout)
		}
		return nil, fmt.Errorf("clipboard: %s: %w: %s", bin, err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

func writeFilesUnix(paths []string) error {
	t, err := resolve()
	if err != nil {
		return err
	}
	// GNOME's file managers paste from their own target, whose body is the
	// verb followed by the uri-list. Everyone else takes text/uri-list.
	target, body := "text/uri-list", buildURIList(paths)
	if onGNOME() {
		target, body = "x-special/gnome-copied-files", "copy\n"+body
	}
	return writeTarget(t, target, []byte(body))
}

func writeImageUnix(png []byte) error {
	t, err := resolve()
	if err != nil {
		return err
	}
	return writeTarget(t, "image/png", png)
}

// onGNOME is true where the desktop understands GNOME's copied-files target
// -- GNOME itself and its close relatives.
func onGNOME() bool {
	desktop := strings.ToLower(os.Getenv("XDG_CURRENT_DESKTOP"))
	for _, d := range []string{"gnome", "cinnamon", "mate", "unity", "x-cinnamon"} {
		if strings.Contains(desktop, d) {
			return true
		}
	}
	return false
}

// parseURIList turns a text/uri-list (or the tail of a gnome-copied-files)
// into local paths. Lines that are not local files -- comments, remote URLs,
// the copy/cut verb -- fall away.
func parseURIList(raw string) []string {
	var paths []string
	for _, line := range splitLines(raw) {
		line = strings.TrimRight(strings.TrimSpace(line), "\r")
		if line == "" || strings.HasPrefix(line, "#") || line == "copy" || line == "cut" {
			continue
		}
		u, err := url.Parse(line)
		if err != nil || u.Scheme != "file" {
			continue
		}
		if u.Host != "" && u.Host != "localhost" {
			continue // a file on some other machine is not a file here
		}
		if u.Path == "" {
			continue
		}
		paths = append(paths, u.Path)
	}
	return paths
}

// buildURIList renders paths as a text/uri-list body, percent-encoding the
// way url.URL does -- spaces, hashes and friends survive the trip.
func buildURIList(paths []string) string {
	var b strings.Builder
	for _, p := range paths {
		u := url.URL{Scheme: "file", Path: p}
		b.WriteString(u.String())
		b.WriteString("\n")
	}
	return b.String()
}

func splitLines(s string) []string {
	var lines []string
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimRight(l, "\r"); l != "" {
			lines = append(lines, l)
		}
	}
	return lines
}

// --- windows ----------------------------------------------------------------

// Windows.Forms sees the file drop list and the bitmap that Get-Clipboard's
// text form does not. -STA because the Windows clipboard is a COM citizen and
// refuses apartment-less callers.

const winLookScript = `[Console]::OutputEncoding = [System.Text.Encoding]::UTF8
Add-Type -AssemblyName System.Windows.Forms | Out-Null
$cb = [System.Windows.Forms.Clipboard]
if ($cb::ContainsFileDropList()) {
  'files'
  $cb::GetFileDropList() | ForEach-Object { $_ }
} elseif ($cb::ContainsImage()) {
  Add-Type -AssemblyName System.Drawing | Out-Null
  $img = $cb::GetImage()
  $ms = New-Object System.IO.MemoryStream
  $img.Save($ms, [System.Drawing.Imaging.ImageFormat]::Png)
  'image'
  [Convert]::ToBase64String($ms.ToArray())
} else {
  'text'
}`

const winWriteFilesScript = `Add-Type -AssemblyName System.Windows.Forms | Out-Null
$paths = New-Object System.Collections.Specialized.StringCollection
foreach ($line in $input) { if ($line) { [void]$paths.Add($line) } }
[System.Windows.Forms.Clipboard]::SetFileDropList($paths)`

func lookWindows() (Snapshot, error) {
	t, err := resolve()
	if err != nil {
		return Snapshot{}, err
	}
	out, err := capture(t, t.readBin, []string{"-NoProfile", "-NonInteractive", "-STA", "-Command", winLookScript})
	if err != nil {
		return Snapshot{}, err
	}
	lines := splitLines(string(out))
	if len(lines) == 0 {
		return Snapshot{Kind: ContentText}, nil
	}
	switch lines[0] {
	case "files":
		return Snapshot{Kind: ContentFiles, Paths: lines[1:]}, nil
	case "image":
		if len(lines) < 2 {
			return Snapshot{Kind: ContentText}, nil
		}
		img, err := base64.StdEncoding.DecodeString(lines[1])
		if err != nil {
			return Snapshot{}, fmt.Errorf("clipboard: image from clipboard: %w", err)
		}
		return Snapshot{Kind: ContentImage, Image: img}, nil
	default:
		text, err := Read()
		if err != nil {
			return Snapshot{}, err
		}
		return Snapshot{Kind: ContentText, Text: text}, nil
	}
}

func writeFilesWindows(paths []string) error {
	t, err := resolve()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, t.readBin, "-NoProfile", "-NonInteractive", "-STA", "-Command", winWriteFilesScript)
	cmd.Env = append(os.Environ(), t.env...)
	return writeWith(cmd, []byte(strings.Join(paths, "\r\n")+"\r\n"), false)
}

func writeImageWindows(png []byte) error {
	t, err := resolve()
	if err != nil {
		return err
	}
	f, err := os.CreateTemp("", "henri-image-*.png")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(png); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	// The path goes into the script single-quoted; PowerShell doubles
	// embedded quotes. Temp paths do not contain quotes, but the escape
	// costs nothing.
	quoted := strings.ReplaceAll(f.Name(), "'", "''")
	script := `Add-Type -AssemblyName System.Windows.Forms | Out-Null
Add-Type -AssemblyName System.Drawing | Out-Null
$img = [System.Drawing.Image]::FromFile('` + quoted + `')
[System.Windows.Forms.Clipboard]::SetImage($img)
$img.Dispose()`
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, t.readBin, "-NoProfile", "-NonInteractive", "-STA", "-Command", script)
	cmd.Env = append(os.Environ(), t.env...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("clipboard: write image: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}
