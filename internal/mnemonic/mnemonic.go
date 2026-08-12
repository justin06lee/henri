// Package mnemonic turns a group secret into words you can read off one screen
// and type into another.
//
// It implements BIP-39, the same scheme cryptocurrency wallets use for seed
// phrases, over the standard 2048-word English list. Twelve words carry 128
// bits of entropy plus a 4-bit checksum, which is both far beyond what anyone
// could guess and enough to catch a typo before it turns into a device that
// silently never syncs.
//
// The word list is chosen so that the first four letters identify a word
// uniquely, so `Decode` accepts any abbreviation of four letters or more. Every
// word in it is ASCII, so "four letters" and "four bytes" are the same thing
// here; the code counts bytes.
package mnemonic

import (
	"crypto/rand"
	"crypto/sha256"
	_ "embed"
	"errors"
	"fmt"
	"strings"
	"unicode"
)

//go:embed english.txt
var wordlistRaw string

// prefixLen is how much of a word has to be typed for henri to recognise it.
// The BIP-39 English list is built so that four bytes are always enough.
const prefixLen = 4

var (
	words []string
	// byWord finds a word typed in full, byPrefix one typed as an
	// abbreviation. Both are built once, because resolving an abbreviation by
	// scanning all 2048 words is a scan per word of every phrase henri reads.
	byWord   map[string]int
	byPrefix map[string]int
)

func init() {
	words = strings.Fields(wordlistRaw)
	if len(words) != 2048 {
		panic(fmt.Sprintf("mnemonic: word list has %d entries, want 2048", len(words)))
	}
	byWord = make(map[string]int, len(words))
	byPrefix = make(map[string]int, len(words))
	for i, w := range words {
		byWord[w] = i
		if len(w) < prefixLen {
			// Nothing to abbreviate: a word this short has to be typed whole,
			// and byWord already finds it.
			continue
		}
		p := w[:prefixLen]
		// The uniqueness of four-byte prefixes is a property of this
		// particular word list, and everything about abbreviations rests on
		// it. Check it here, once, so that swapping the list in for another
		// one fails loudly at startup rather than quietly resolving a typed
		// abbreviation to whichever word happened to come first.
		if j, clash := byPrefix[p]; clash {
			panic(fmt.Sprintf("mnemonic: %q and %q share their first %d bytes, "+
				"so abbreviations are ambiguous", words[j], w, prefixLen))
		}
		byPrefix[p] = i
	}
}

// WordCounts are the phrase lengths henri accepts, shortest first.
var WordCounts = []int{12, 15, 18, 21, 24}

// EntropyBitsFor returns how many bits of entropy a phrase of n words carries.
func EntropyBitsFor(n int) (int, bool) {
	for _, c := range WordCounts {
		if c == n {
			return c * 11 * 32 / 33, true
		}
	}
	return 0, false
}

// New generates a fresh phrase carrying the given number of entropy bits.
// Use 128 (12 words) unless you have a reason not to.
func New(bits int) (string, []byte, error) {
	if bits < 128 || bits > 256 || bits%32 != 0 {
		return "", nil, fmt.Errorf("mnemonic: entropy must be 128-256 bits in steps of 32, got %d", bits)
	}
	entropy := make([]byte, bits/8)
	if _, err := rand.Read(entropy); err != nil {
		return "", nil, err
	}
	phrase, err := Encode(entropy)
	if err != nil {
		return "", nil, err
	}
	return phrase, entropy, nil
}

// Encode renders entropy as a phrase.
func Encode(entropy []byte) (string, error) {
	ent := len(entropy) * 8
	if ent < 128 || ent > 256 || ent%32 != 0 {
		return "", fmt.Errorf("mnemonic: entropy must be 16-32 bytes in steps of 4, got %d", len(entropy))
	}
	cs := ent / 32
	sum := sha256.Sum256(entropy)

	total := ent + cs
	out := make([]string, 0, total/11)
	for i := 0; i < total; i += 11 {
		idx := 0
		for j := 0; j < 11; j++ {
			idx = idx<<1 | bitAt(entropy, sum[:], ent, i+j)
		}
		out = append(out, words[idx])
	}
	return strings.Join(out, " "), nil
}

// bitAt reads bit n of entropy||checksum.
func bitAt(entropy, sum []byte, ent, n int) int {
	src, i := entropy, n
	if n >= ent {
		src, i = sum, n-ent
	}
	return int(src[i/8]>>(7-uint(i%8))) & 1
}

// Decode validates a phrase and returns the entropy it carries.
//
// It is forgiving about how the phrase is typed — case, extra whitespace,
// punctuation between words and four-letter abbreviations are all fine — but
// strict about the checksum, so a mistyped word is reported rather than
// silently producing the wrong key.
func Decode(phrase string) ([]byte, error) {
	given := Split(phrase)
	if len(given) == 0 {
		return nil, errors.New("mnemonic: the phrase is empty")
	}
	ent, ok := EntropyBitsFor(len(given))
	if !ok {
		return nil, fmt.Errorf("mnemonic: a phrase is %s words, but this one has %d",
			joinCounts(), len(given))
	}

	idxs := make([]int, len(given))
	for i, w := range given {
		idx, err := lookup(w)
		if err != nil {
			return nil, fmt.Errorf("mnemonic: word %d: %w", i+1, err)
		}
		idxs[i] = idx
	}

	cs := ent / 32
	total := ent + cs
	buf := make([]byte, (total+7)/8)
	pos := 0
	for _, idx := range idxs {
		for j := 10; j >= 0; j-- {
			if idx>>uint(j)&1 == 1 {
				buf[pos/8] |= 1 << (7 - uint(pos%8))
			}
			pos++
		}
	}

	entropy := make([]byte, ent/8)
	copy(entropy, buf[:ent/8])

	sum := sha256.Sum256(entropy)
	for i := 0; i < cs; i++ {
		want := int(sum[i/8]>>(7-uint(i%8))) & 1
		got := int(buf[(ent+i)/8]>>(7-uint((ent+i)%8))) & 1
		if want != got {
			return nil, errors.New("mnemonic: that phrase does not check out — " +
				"every word is real, so one of them is probably in the wrong place or slightly wrong")
		}
	}
	return entropy, nil
}

// lookup resolves one typed word, accepting unique four-or-more letter
// prefixes and offering a correction when it can.
func lookup(w string) (int, error) {
	if i, ok := byWord[w]; ok {
		return i, nil
	}
	if len(w) >= prefixLen {
		// One lookup settles it: the first four bytes name at most one word, so
		// anything longer only has to agree with the word they name.
		if i, ok := byPrefix[w[:prefixLen]]; ok && strings.HasPrefix(words[i], w) {
			return i, nil
		}
	}
	if s := suggest(w); s != "" {
		return 0, fmt.Errorf("%q is not one of the words — did you mean %q?", w, s)
	}
	return 0, fmt.Errorf("%q is not one of the words", w)
}

// suggest finds the closest real word, if one is close enough to be a typo.
func suggest(w string) string {
	best, bestDist := "", 3
	for _, full := range words {
		if abs(len(full)-len(w)) >= bestDist {
			continue
		}
		if d := distance(w, full); d < bestDist {
			best, bestDist = full, d
		}
	}
	return best
}

// distance is Damerau-Levenshtein edit distance, restricted form. Counting a
// transposition as one edit rather than two matters here: swapping two letters
// is the typo people actually make, and plain Levenshtein ranks "socail" as
// closer to "local" than to "social".
func distance(a, b string) int {
	d := make([][]int, len(a)+1)
	for i := range d {
		d[i] = make([]int, len(b)+1)
		d[i][0] = i
	}
	for j := 0; j <= len(b); j++ {
		d[0][j] = j
	}
	for i := 1; i <= len(a); i++ {
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			d[i][j] = min(min(d[i-1][j]+1, d[i][j-1]+1), d[i-1][j-1]+cost)
			if i > 1 && j > 1 && a[i-1] == b[j-2] && a[i-2] == b[j-1] {
				d[i][j] = min(d[i][j], d[i-2][j-2]+1)
			}
		}
	}
	return d[len(a)][len(b)]
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

// Split normalises a typed phrase into its words. Anything that is not a letter
// separates one word from the next, so punctuation and stray whitespace are
// simply gaps. FieldsFunc never returns an empty field, so there is nothing to
// filter out afterwards.
func Split(phrase string) []string {
	return strings.FieldsFunc(strings.ToLower(phrase), func(r rune) bool {
		return !unicode.IsLetter(r)
	})
}

func joinCounts() string {
	parts := make([]string, len(WordCounts))
	for i, c := range WordCounts {
		parts[i] = fmt.Sprint(c)
	}
	return strings.Join(parts[:len(parts)-1], ", ") + " or " + parts[len(parts)-1]
}
