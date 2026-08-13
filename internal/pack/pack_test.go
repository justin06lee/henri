package pack

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestFilesAndFoldersRoundTrip(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	write(t, filepath.Join(src, "note.txt"), "a note")
	write(t, filepath.Join(src, "album", "one.txt"), "track one")
	write(t, filepath.Join(src, "album", "deep", "two.txt"), "track two")

	data, names, err := Build([]string{
		filepath.Join(src, "note.txt"),
		filepath.Join(src, "album"),
	}, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 2 || names[0] != "note.txt" || names[1] != "album" {
		t.Fatalf("names = %v", names)
	}

	created, err := Extract(data, dst, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if len(created) != 2 {
		t.Fatalf("created = %v", created)
	}
	for path, want := range map[string]string{
		filepath.Join(dst, "note.txt"):                 "a note",
		filepath.Join(dst, "album", "one.txt"):         "track one",
		filepath.Join(dst, "album", "deep", "two.txt"): "track two",
	} {
		got, err := os.ReadFile(path)
		if err != nil || string(got) != want {
			t.Fatalf("%s = %q, %v; want %q", path, got, err, want)
		}
	}
}

// Two sources with the same base name must both arrive.
func TestSameNamedSourcesAreBothKept(t *testing.T) {
	a, b, dst := t.TempDir(), t.TempDir(), t.TempDir()
	write(t, filepath.Join(a, "x.txt"), "from a")
	write(t, filepath.Join(b, "x.txt"), "from b")

	data, names, err := Build([]string{filepath.Join(a, "x.txt"), filepath.Join(b, "x.txt")}, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if names[0] != "x.txt" || names[1] != "x (2).txt" {
		t.Fatalf("names = %v", names)
	}
	if _, err := Extract(data, dst, 1<<20); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(filepath.Join(dst, "x (2).txt"))
	if string(got) != "from b" {
		t.Fatalf("x (2).txt = %q", got)
	}
}

// What is already in the receive directory must never be overwritten.
func TestExtractionNeverOverwrites(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	write(t, filepath.Join(src, "report.pdf"), "new arrival")
	write(t, filepath.Join(dst, "report.pdf"), "already here")

	data, _, err := Build([]string{filepath.Join(src, "report.pdf")}, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	created, err := Extract(data, dst, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(dst, "report.pdf")); string(got) != "already here" {
		t.Fatalf("the existing file was overwritten: %q", got)
	}
	if len(created) != 1 || filepath.Base(created[0]) != "report (2).pdf" {
		t.Fatalf("created = %v, want report (2).pdf", created)
	}
}

func TestBuildRefusesTooMuchWithoutArchivingIt(t *testing.T) {
	src := t.TempDir()
	write(t, filepath.Join(src, "big"), strings.Repeat("x", 4096))
	_, _, err := Build([]string{filepath.Join(src, "big")}, 1024)
	if !errors.Is(err, ErrTooBig) {
		t.Fatalf("err = %v, want ErrTooBig", err)
	}
}

func TestSymlinksAndDSStoreStayHome(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	write(t, filepath.Join(src, "folder", "real.txt"), "real")
	write(t, filepath.Join(src, "folder", ".DS_Store"), "finder droppings")
	if err := os.Symlink("/etc/passwd", filepath.Join(src, "folder", "link")); err != nil {
		t.Skip("no symlinks here")
	}

	data, _, err := Build([]string{filepath.Join(src, "folder")}, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Extract(data, dst, 1<<20); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{".DS_Store", "link"} {
		if _, err := os.Lstat(filepath.Join(dst, "folder", name)); err == nil {
			t.Fatalf("%s travelled", name)
		}
	}
}

// hostileArchive builds a tar.gz by hand, the way an attacker would.
func hostileArchive(t *testing.T, entries map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, content := range entries {
		if err := tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Size: int64(len(content)), Mode: 0o644}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

func TestHostileNamesAreRefused(t *testing.T) {
	dst := t.TempDir()
	for _, name := range []string{
		"../escape.txt",
		"a/../../escape.txt",
		"/etc/cron.d/henri",
		"..",
	} {
		_, err := Extract(hostileArchive(t, map[string]string{name: "gotcha"}), dst, 1<<20)
		if err == nil {
			t.Fatalf("an archive naming %q was extracted", name)
		}
		if _, statErr := os.Stat(filepath.Join(dst, "..", "escape.txt")); statErr == nil {
			t.Fatalf("%q wrote outside the target directory", name)
		}
	}
}

// A symlink entry followed by a write through it is the classic two-step
// escape. henri skips link entries entirely, so the write lands nowhere.
func TestSymlinkEntriesAreSkippedOnExtract(t *testing.T) {
	dst := t.TempDir()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	_ = tw.WriteHeader(&tar.Header{Name: "out", Typeflag: tar.TypeSymlink, Linkname: "/tmp"})
	_ = tw.WriteHeader(&tar.Header{Name: "keep.txt", Typeflag: tar.TypeReg, Size: 4, Mode: 0o644})
	_, _ = tw.Write([]byte("safe"))
	tw.Close()
	gz.Close()

	created, err := Extract(buf.Bytes(), dst, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(dst, "out")); err == nil {
		t.Fatal("a symlink entry was written to disk")
	}
	if len(created) != 1 || filepath.Base(created[0]) != "keep.txt" {
		t.Fatalf("created = %v, want only keep.txt: the clipboard must list only what exists", created)
	}
	if got, _ := os.ReadFile(filepath.Join(dst, "keep.txt")); string(got) != "safe" {
		t.Fatalf("keep.txt = %q", got)
	}
}

// A small frame of well-chosen zeros must not unpack into gigabytes.
func TestDecompressionIsCapped(t *testing.T) {
	dst := t.TempDir()
	bomb := hostileArchive(t, map[string]string{"zeros": strings.Repeat("\x00", 1<<20)})
	if len(bomb) > 8*1024 {
		t.Fatalf("the bomb did not compress (%d bytes); the test is not testing anything", len(bomb))
	}
	_, err := Extract(bomb, dst, 64*1024)
	if !errors.Is(err, ErrTooBig) {
		t.Fatalf("err = %v, want ErrTooBig", err)
	}
	if _, statErr := os.Stat(filepath.Join(dst, "zeros")); statErr == nil {
		t.Fatal("the partial bomb was left on disk")
	}
}

func TestSetuidDoesNotSurvive(t *testing.T) {
	dst := t.TempDir()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	_ = tw.WriteHeader(&tar.Header{Name: "tool", Typeflag: tar.TypeReg, Size: 2, Mode: 0o4755})
	_, _ = tw.Write([]byte("hi"))
	tw.Close()
	gz.Close()
	if _, err := Extract(buf.Bytes(), dst, 1<<20); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(dst, "tool"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSetuid != 0 {
		t.Fatal("setuid survived extraction")
	}
}
