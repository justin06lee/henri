// Package pack turns a clipboard's worth of files and folders into one
// payload and back.
//
// A file copy is a list of paths, and paths mean nothing on another machine --
// what travels is the contents, as a gzip'd tar built here. The receiver
// unpacks into a directory of its own choosing and puts references to the
// unpacked files on its clipboard, so a paste lands real files.
//
// Extract treats the archive as hostile: it arrives off the network, and tar
// is an old format full of ways to write outside the directory it was given.
package pack

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// contentSum fingerprints what the files say, not what they are called or
// where they sit.
//
// Names change in transit -- a receiver renames collisions "name (2)" style --
// so an archive's bytes never survive a round trip, and the byte-equality
// that keeps text and images from echoing between devices does not exist for
// files. This sum restores it: one digest per file over its contents and
// size, combined order-independently, names excluded. The same files sum the
// same however they have been renamed or reordered along the way.
type contentSum struct {
	digests []string
}

// file starts one file's digest; feed it the contents, then close it.
func (c *contentSum) file() *fileDigest { return &fileDigest{h: sha256.New(), sum: c} }

func (c *contentSum) sum() string {
	sort.Strings(c.digests)
	h := sha256.New()
	for _, d := range c.digests {
		h.Write([]byte(d))
	}
	return hex.EncodeToString(h.Sum(nil))
}

type fileDigest struct {
	h   hash.Hash
	n   int64
	sum *contentSum
}

func (f *fileDigest) Write(p []byte) (int, error) {
	f.n += int64(len(p))
	return f.h.Write(p)
}

// close seals the file's contribution; the size keeps "ab","c" and "a","bc"
// apart.
func (f *fileDigest) close() {
	_ = binary.Write(f.h, binary.BigEndian, f.n)
	f.sum.digests = append(f.sum.digests, hex.EncodeToString(f.h.Sum(nil)))
}

// ErrTooBig means the files exceed the limit the caller set. It is a size
// answer, not a failure: the caller decides whether to tell the user or skip.
var ErrTooBig = errors.New("pack: the files are larger than the limit")

// Build archives the named files and directories, stopping at limit bytes of
// content. It returns the archive and the top-level names it contains, which
// are the file names the receiver will see.
//
// Symlinks are skipped: a link's target is somewhere on the sending machine,
// and shipping the link would dangle while shipping the target would surprise.
// .DS_Store never deserves to travel.
func Build(paths []string, limit int) (data []byte, names []string, content string, err error) {
	if len(paths) == 0 {
		return nil, nil, "", errors.New("pack: nothing to archive")
	}

	// Sizes first, so a folder full of video fails in milliseconds instead of
	// after archiving gigabytes of it.
	var total int64
	for _, p := range paths {
		n, err := sizeOf(p)
		if err != nil {
			return nil, nil, "", err
		}
		total += n
		if total > int64(limit) {
			return nil, nil, "", fmt.Errorf("%w (%d bytes so far, limit %d)", ErrTooBig, total, limit)
		}
	}

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	cs := &contentSum{}
	seen := map[string]bool{}
	for _, p := range paths {
		top := uniqueName(filepath.Base(p), func(name string) bool { return seen[name] })
		seen[top] = true
		if err := addPath(tw, p, top, cs); err != nil {
			return nil, nil, "", err
		}
		names = append(names, top)
	}
	if err := tw.Close(); err != nil {
		return nil, nil, "", err
	}
	if err := gz.Close(); err != nil {
		return nil, nil, "", err
	}
	// tar rounds up in 512-byte blocks, so a limit-sized file of incompressible
	// bytes can end up a shade over. The wire cap is on the payload; hold it.
	if buf.Len() > limit {
		return nil, nil, "", fmt.Errorf("%w (%d bytes archived, limit %d)", ErrTooBig, buf.Len(), limit)
	}
	return buf.Bytes(), names, cs.sum(), nil
}

// sizeOf is the recursive size of one path, skipping what Build skips.
func sizeOf(path string) (int64, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return 0, err
	}
	switch {
	case info.Mode()&fs.ModeSymlink != 0:
		return 0, nil
	case info.IsDir():
		var total int64
		err := filepath.WalkDir(path, func(_ string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.Type()&fs.ModeSymlink != 0 || d.Name() == ".DS_Store" {
				return nil
			}
			if d.Type().IsRegular() {
				fi, err := d.Info()
				if err != nil {
					return err
				}
				total += fi.Size()
			}
			return nil
		})
		return total, err
	case info.Mode().IsRegular():
		return info.Size(), nil
	default:
		return 0, fmt.Errorf("pack: %s is not a file or directory", path)
	}
}

// addPath writes one top-level file or directory into the archive as top.
func addPath(tw *tar.Writer, path, top string, cs *contentSum) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&fs.ModeSymlink != 0 {
		return nil
	}
	if !info.IsDir() {
		return addFile(tw, path, top, info, cs)
	}
	return filepath.WalkDir(path, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type()&fs.ModeSymlink != 0 || d.Name() == ".DS_Store" {
			return nil
		}
		rel, err := filepath.Rel(path, p)
		if err != nil {
			return err
		}
		name := top
		if rel != "." {
			name = top + "/" + filepath.ToSlash(rel)
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		if d.IsDir() {
			return tw.WriteHeader(&tar.Header{
				Name: name + "/", Typeflag: tar.TypeDir,
				Mode: int64(fi.Mode().Perm()), ModTime: fi.ModTime(),
			})
		}
		if !d.Type().IsRegular() {
			return nil // sockets, pipes, devices: nothing a clipboard should carry
		}
		return addFile(tw, p, name, fi, cs)
	})
}

func addFile(tw *tar.Writer, path, name string, info fs.FileInfo, cs *contentSum) error {
	if err := tw.WriteHeader(&tar.Header{
		Name: name, Typeflag: tar.TypeReg, Size: info.Size(),
		Mode: int64(info.Mode().Perm()), ModTime: info.ModTime(),
	}); err != nil {
		return err
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	fd := cs.file()
	if _, err := io.Copy(io.MultiWriter(tw, fd), f); err != nil {
		return err
	}
	fd.close()
	return nil
}

// Extract unpacks an archive into dir and returns the top-level paths it
// created, in the order a clipboard should list them. Names that already
// exist in dir are renamed "name (2)" style rather than overwritten: the
// clipboard must never destroy what is already on disk.
//
// limit caps the total unpacked bytes. The archive passed the wire's size
// check compressed; without this, a small frame of well-chosen zeros unpacks
// into anything at all.
func Extract(data []byte, dir string, limit int) (created []string, content string, err error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, "", err
	}
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, "", fmt.Errorf("pack: not a henri archive: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	cs := &contentSum{}

	// Top-level names in the archive are mapped to what they are actually
	// called on disk, once, so a renamed folder carries its contents along.
	renames := map[string]string{}
	var tops []string
	var total int64
	fail := func(err error) ([]string, string, error) {
		for _, t := range tops {
			os.RemoveAll(filepath.Join(dir, t))
		}
		return nil, "", err
	}

	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fail(fmt.Errorf("pack: reading archive: %w", err))
		}

		name := filepath.FromSlash(hdr.Name)
		top, rest, ok := splitTop(name)
		if !ok {
			return fail(fmt.Errorf("pack: archive names %q, which henri refuses to write", hdr.Name))
		}
		actual, known := renames[top]
		if !known {
			actual = uniqueName(top, func(n string) bool {
				_, err := os.Lstat(filepath.Join(dir, n))
				return err == nil
			})
			renames[top] = actual
			tops = append(tops, actual)
		}
		target := filepath.Join(dir, actual)
		if rest != "" {
			target = filepath.Join(target, rest)
		}
		// Belt and braces after splitTop: whatever the header said, the file
		// being written lives under dir or is not written.
		if rel, err := filepath.Rel(dir, target); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
			return fail(fmt.Errorf("pack: archive names %q, which escapes the target directory", hdr.Name))
		}

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, sane(hdr.Mode, 0o755)); err != nil {
				return fail(err)
			}
		case tar.TypeReg:
			total += hdr.Size
			if total > int64(limit) {
				return fail(fmt.Errorf("%w (%d bytes unpacked, limit %d)", ErrTooBig, total, limit))
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return fail(err)
			}
			f, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, sane(hdr.Mode, 0o644))
			if err != nil {
				return fail(err)
			}
			// LimitReader rather than trusting hdr.Size alone: the count above
			// is the guard, this makes it unconditional.
			fd := cs.file()
			if _, err := io.Copy(io.MultiWriter(f, fd), io.LimitReader(tr, hdr.Size)); err != nil {
				f.Close()
				return fail(err)
			}
			if err := f.Close(); err != nil {
				return fail(err)
			}
			fd.close()
			_ = os.Chtimes(target, hdr.ModTime, hdr.ModTime)
		default:
			// Symlinks, hard links, devices: skipped on the way in, and
			// refused rather than skipped on the way out would punish archives
			// this code never builds. Skip, silently.
		}
	}
	// A top-level name can be claimed by an entry that was then skipped -- a
	// lone symlink, say. Only what is actually on disk goes on the clipboard.
	sort.Strings(tops)
	for _, t := range tops {
		p := filepath.Join(dir, t)
		if _, err := os.Lstat(p); err == nil {
			created = append(created, p)
		}
	}
	if len(created) == 0 {
		return fail(errors.New("pack: the archive contained nothing henri writes"))
	}
	return created, cs.sum(), nil
}

// splitTop splits an archive name into its top-level entry and the rest,
// refusing anything that could name a place outside the extraction directory.
func splitTop(name string) (top, rest string, ok bool) {
	name = strings.TrimSuffix(name, string(os.PathSeparator))
	if name == "" || filepath.IsAbs(name) || strings.HasPrefix(name, "~") {
		return "", "", false
	}
	clean := filepath.Clean(name)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(os.PathSeparator)) {
		return "", "", false
	}
	// On Windows, Clean leaves "C:" and friends alone; a volume name is a
	// place, not a name.
	if filepath.VolumeName(clean) != "" {
		return "", "", false
	}
	parts := strings.SplitN(clean, string(os.PathSeparator), 2)
	top = parts[0]
	if len(parts) == 2 {
		rest = parts[1]
	}
	return top, rest, top != ""
}

// uniqueName returns name, or the first "name (2)"-style variant for which
// taken says no, keeping the extension where a Finder or Explorer would.
func uniqueName(name string, taken func(string) bool) string {
	if !taken(name) {
		return name
	}
	ext := filepath.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	for i := 2; ; i++ {
		candidate := fmt.Sprintf("%s (%d)%s", stem, i, ext)
		if !taken(candidate) {
			return candidate
		}
	}
}

// sane clamps a mode that arrived off the network to something a downloaded
// file may have: no setuid, no sticky bits, owner-writable.
func sane(mode int64, fallback fs.FileMode) fs.FileMode {
	m := fs.FileMode(mode) & 0o777
	if m == 0 {
		return fallback
	}
	return m | 0o600
}
