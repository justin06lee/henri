package clipboard

import (
	"reflect"
	"strings"
	"testing"
)

func TestURIListRoundTrip(t *testing.T) {
	paths := []string{
		"/home/you/plain.txt",
		"/home/you/with space.txt",
		"/home/you/naïve#note.txt",
	}
	got := parseURIList(buildURIList(paths))
	if !reflect.DeepEqual(got, paths) {
		t.Fatalf("round trip = %v, want %v", got, paths)
	}
}

func TestParseURIListReadsWhatDesktopsWrite(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want []string
	}{
		{
			"nautilus copied-files body",
			"copy\nfile:///home/you/a.png\nfile:///home/you/b%20c.png\n",
			[]string{"/home/you/a.png", "/home/you/b c.png"},
		},
		{
			"crlf uri-list",
			"file:///home/you/a.txt\r\nfile:///home/you/b.txt\r\n",
			[]string{"/home/you/a.txt", "/home/you/b.txt"},
		},
		{
			"comments and blanks",
			"# a comment\n\nfile:///tmp/x\n",
			[]string{"/tmp/x"},
		},
		{
			"remote urls are not local files",
			"https://example.com/a.png\nfile://otherhost/tmp/x\n",
			nil,
		},
		{
			"localhost is this machine",
			"file://localhost/tmp/x\n",
			[]string{"/tmp/x"},
		},
	}
	for _, tc := range cases {
		if got := parseURIList(tc.raw); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: parseURIList = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// The look script's first line is a contract with parseLook; pin both sides.
func TestParseLook(t *testing.T) {
	cases := []struct {
		out               string
		kind, count, body string
		bad               bool
	}{
		{out: "same 41", kind: "same", count: "41"},
		{out: "text 42", kind: "text", count: "42"},
		{out: "files 43\n/a\n/b", kind: "files", count: "43", body: "/a\n/b"},
		{out: "image 44\naGVucmk=", kind: "image", count: "44", body: "aGVucmk="},
		{out: "", bad: true},
		{out: "files", bad: true},
		{out: "files nan\n/a", bad: true},
	}
	for _, tc := range cases {
		kind, count, body, err := parseLook(tc.out)
		if tc.bad {
			if err == nil {
				t.Errorf("parseLook(%q) accepted", tc.out)
			}
			continue
		}
		if err != nil || kind != tc.kind || count != tc.count || body != tc.body {
			t.Errorf("parseLook(%q) = %q %q %q %v", tc.out, kind, count, body, err)
		}
	}
}

func TestLookScriptSpeaksTheParsedContract(t *testing.T) {
	for _, needle := range []string{`"same " + count`, `"files " + count`, `"image " + count`, `"text " + count`} {
		if !strings.Contains(lookScript, needle) {
			t.Errorf("look script no longer answers %s", needle)
		}
	}
}

func TestGnomeGetsItsOwnTarget(t *testing.T) {
	t.Setenv("XDG_CURRENT_DESKTOP", "ubuntu:GNOME")
	if !onGNOME() {
		t.Fatal("ubuntu:GNOME not recognised")
	}
	t.Setenv("XDG_CURRENT_DESKTOP", "KDE")
	if onGNOME() {
		t.Fatal("KDE is not GNOME")
	}
	t.Setenv("XDG_CURRENT_DESKTOP", "")
	if onGNOME() {
		t.Fatal("no desktop is not GNOME")
	}
}
