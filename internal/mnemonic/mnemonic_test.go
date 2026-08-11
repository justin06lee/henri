package mnemonic

import (
	"bytes"
	"encoding/hex"
	"strings"
	"testing"
)

// officialVectors are the BIP-39 English test vectors published with the
// reference implementation. Matching them is what makes a phrase generated
// here readable by any other BIP-39 tool, and vice versa.
var officialVectors = [][2]string{
	{"00000000000000000000000000000000", "abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon about"},
	{"7f7f7f7f7f7f7f7f7f7f7f7f7f7f7f7f", "legal winner thank year wave sausage worth useful legal winner thank yellow"},
	{"80808080808080808080808080808080", "letter advice cage absurd amount doctor acoustic avoid letter advice cage above"},
	{"ffffffffffffffffffffffffffffffff", "zoo zoo zoo zoo zoo zoo zoo zoo zoo zoo zoo wrong"},
	{"000000000000000000000000000000000000000000000000", "abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon agent"},
	{"7f7f7f7f7f7f7f7f7f7f7f7f7f7f7f7f7f7f7f7f7f7f7f7f", "legal winner thank year wave sausage worth useful legal winner thank year wave sausage worth useful legal will"},
	{"808080808080808080808080808080808080808080808080", "letter advice cage absurd amount doctor acoustic avoid letter advice cage absurd amount doctor acoustic avoid letter always"},
	{"ffffffffffffffffffffffffffffffffffffffffffffffff", "zoo zoo zoo zoo zoo zoo zoo zoo zoo zoo zoo zoo zoo zoo zoo zoo zoo when"},
	{"0000000000000000000000000000000000000000000000000000000000000000", "abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon art"},
	{"7f7f7f7f7f7f7f7f7f7f7f7f7f7f7f7f7f7f7f7f7f7f7f7f7f7f7f7f7f7f7f7f", "legal winner thank year wave sausage worth useful legal winner thank year wave sausage worth useful legal winner thank year wave sausage worth title"},
	{"8080808080808080808080808080808080808080808080808080808080808080", "letter advice cage absurd amount doctor acoustic avoid letter advice cage absurd amount doctor acoustic avoid letter advice cage absurd amount doctor acoustic bless"},
	{"ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff", "zoo zoo zoo zoo zoo zoo zoo zoo zoo zoo zoo zoo zoo zoo zoo zoo zoo zoo zoo zoo zoo zoo zoo vote"},
	{"9e885d952ad362caeb4efe34a8e91bd2", "ozone drill grab fiber curtain grace pudding thank cruise elder eight picnic"},
	{"6610b25967cdcca9d59875f5cb50b0ea75433311869e930b", "gravity machine north sort system female filter attitude volume fold club stay feature office ecology stable narrow fog"},
	{"68a79eaca2324873eacc50cb9c6eca8cc68ea5d936f98787c60c7ebc74e6ce7c", "hamster diagram private dutch cause delay private meat slide toddler razor book happy fancy gospel tennis maple dilemma loan word shrug inflict delay length"},
	{"c0ba5a8e914111210f2bd131f3d5e08d", "scheme spot photo card baby mountain device kick cradle pact join borrow"},
	{"6d9be1ee6ebd27a258115aad99b7317b9c8d28b6d76431c3", "horn tenant knee talent sponsor spell gate clip pulse soap slush warm silver nephew swap uncle crack brave"},
	{"9f6a2878b2520799a44ef18bc7df394e7061a224d2c33cd015b157d746869863", "panda eyebrow bullet gorilla call smoke muffin taste mesh discover soft ostrich alcohol speed nation flash devote level hobby quick inner drive ghost inside"},
	{"23db8160a31d3e0dca3688ed941adbf3", "cat swing flag economy stadium alone churn speed unique patch report train"},
	{"8197a4a47f0425faeaa69deebc05ca29c0a5b5cc76ceacc0", "light rule cinnamon wrap drastic word pride squirrel upgrade then income fatal apart sustain crack supply proud access"},
	{"066dca1a2bb7e8a1db2832148ce9933eea0f3ac9548d793112d9a95c9407efad", "all hour make first leader extend hole alien behind guard gospel lava path output census museum junior mass reopen famous sing advance salt reform"},
	{"f30f8c1da665478f49b001d94c5fc452", "vessel ladder alter error federal sibling chat ability sun glass valve picture"},
	{"c10ec20dc3cd9f652c7fac2f1230f7a3c828389a14392f05", "scissors invite lock maple supreme raw rapid void congress muscle digital elegant little brisk hair mango congress clump"},
	{"f585c11aec520db57dd353c69554b21a89b20fb0650966fa0a9d6f74fd989d8f", "void come effort suffer camp survey warrior heavy shoot primary clutch crush open amazing screen patrol group space point ten exist slush involve unfold"},
}

func TestOfficialVectors(t *testing.T) {
	for _, v := range officialVectors {
		entropy, err := hex.DecodeString(v[0])
		if err != nil {
			t.Fatal(err)
		}
		got, err := Encode(entropy)
		if err != nil {
			t.Fatalf("Encode(%s): %v", v[0], err)
		}
		if got != v[1] {
			t.Errorf("Encode(%s)\n got: %s\nwant: %s", v[0], got, v[1])
		}
		back, err := Decode(v[1])
		if err != nil {
			t.Errorf("Decode(%q): %v", v[1], err)
			continue
		}
		if !bytes.Equal(back, entropy) {
			t.Errorf("Decode(%q) = %x, want %s", v[1], back, v[0])
		}
	}
}

func TestNewRoundTrip(t *testing.T) {
	for _, bits := range []int{128, 160, 192, 224, 256} {
		phrase, entropy, err := New(bits)
		if err != nil {
			t.Fatal(err)
		}
		wantWords := bits / 11 * 33 / 32 / 11 * 11 / 11 // see below; computed directly instead
		_ = wantWords
		n := len(Split(phrase))
		if got, ok := EntropyBitsFor(n); !ok || got != bits {
			t.Fatalf("%d bits produced %d words, which maps back to %d bits", bits, n, got)
		}
		back, err := Decode(phrase)
		if err != nil {
			t.Fatalf("Decode of a freshly generated phrase failed: %v", err)
		}
		if !bytes.Equal(back, entropy) {
			t.Fatalf("round trip changed the entropy")
		}
	}
}

func TestTwelveWordsIs128Bits(t *testing.T) {
	phrase, entropy, err := New(128)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(Split(phrase)); n != 12 {
		t.Fatalf("128 bits gave %d words, want 12", n)
	}
	if len(entropy) != 16 {
		t.Fatalf("got %d bytes of entropy, want 16", len(entropy))
	}
}

func TestDistinctPhrasesEachTime(t *testing.T) {
	seen := make(map[string]bool)
	for i := 0; i < 50; i++ {
		p, _, err := New(128)
		if err != nil {
			t.Fatal(err)
		}
		if seen[p] {
			t.Fatal("New produced the same phrase twice")
		}
		seen[p] = true
	}
}

// The checksum is what turns a typo into a clear error instead of a device
// that quietly never syncs.
func TestChecksumCatchesASwappedWord(t *testing.T) {
	phrase, _, err := New(128)
	if err != nil {
		t.Fatal(err)
	}
	w := Split(phrase)
	w[0], w[1] = w[1], w[0]
	if _, err := Decode(strings.Join(w, " ")); err == nil {
		t.Fatal("swapping two words was accepted")
	}
}

func TestChecksumCatchesAWrongWord(t *testing.T) {
	// A valid phrase with one word replaced by a different real word.
	base := "legal winner thank year wave sausage worth useful legal winner thank yellow"
	if _, err := Decode(base); err != nil {
		t.Fatalf("the control phrase should be valid: %v", err)
	}
	broken := strings.Replace(base, "sausage", "abandon", 1)
	if _, err := Decode(broken); err == nil {
		t.Fatal("a phrase with a wrong word was accepted")
	}
}

func TestDecodeIsForgivingAboutFormatting(t *testing.T) {
	canonical := "legal winner thank year wave sausage worth useful legal winner thank yellow"
	want, err := Decode(canonical)
	if err != nil {
		t.Fatal(err)
	}
	for _, variant := range []string{
		"  LEGAL   Winner\tthank\nyear wave sausage worth useful legal winner thank yellow  ",
		"legal, winner, thank, year, wave, sausage, worth, useful, legal, winner, thank, yellow",
		"1. legal 2. winner 3. thank 4. year 5. wave 6. sausage 7. worth 8. useful 9. legal 10. winner 11. thank 12. yellow",
		// four-letter abbreviations, which the word list guarantees are unique
		"lega winn than year wave saus wort usef lega winn than yell",
	} {
		got, err := Decode(variant)
		if err != nil {
			t.Errorf("Decode(%q): %v", variant, err)
			continue
		}
		if !bytes.Equal(got, want) {
			t.Errorf("Decode(%q) gave different entropy", variant)
		}
	}
}

func TestDecodeRejectsWrongWordCount(t *testing.T) {
	if _, err := Decode("legal winner thank"); err == nil {
		t.Fatal("a three word phrase was accepted")
	}
	if _, err := Decode(""); err == nil {
		t.Fatal("an empty phrase was accepted")
	}
}

func TestDecodeSuggestsACorrection(t *testing.T) {
	// "sausge" is a typo for "sausage"; nothing else is close.
	_, err := Decode("legal winner thank year wave sausge worth useful legal winner thank yellow")
	if err == nil {
		t.Fatal("a misspelled word was accepted")
	}
	if !strings.Contains(err.Error(), "sausage") {
		t.Fatalf("error did not suggest the intended word: %v", err)
	}
	if !strings.Contains(err.Error(), "word 6") {
		t.Fatalf("error did not say which word was wrong: %v", err)
	}
}

func TestWordListShape(t *testing.T) {
	if len(words) != 2048 {
		t.Fatalf("word list has %d entries, want 2048", len(words))
	}
	seen := make(map[string]bool)
	for _, w := range words {
		if seen[w] {
			t.Fatalf("duplicate word %q", w)
		}
		seen[w] = true
	}
	// The four-letter prefix property is what makes abbreviations safe.
	pre := make(map[string]string)
	for _, w := range words {
		p := w
		if len(p) > 4 {
			p = p[:4]
		}
		if other, ok := pre[p]; ok {
			t.Fatalf("%q and %q share the prefix %q", other, w, p)
		}
		pre[p] = w
	}
}
