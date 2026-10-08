// Package tokenizer is the byte-level BPE of the secjev bundle's tokenizer.json in pure
// Go, reproducing Hugging Face tokenizers for this model id for id.
package tokenizer

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"
)

// Tokenizer is the byte-level BPE of the bundle's tokenizer.json, in pure Go. It
// reproduces Hugging Face tokenizers for this model exactly: added tokens split out
// first (leftmost-longest), the pre-tokenizer regex below (Oniguruma semantics), the
// GPT-2 byte-to-character map, BPE by merge rank, and <|startoftext|> in front.
//
//	(?i:'s|'t|'re|'ve|'m|'ll|'d)|[^\r\n\p{L}\p{N}]?\p{L}+|\p{N}{1,3}| ?[^\s\p{L}\p{N}]+[\r\n]*|\s*[\r\n]+|\s+(?!\S)|\s+
//
// Go's regexp has no look-ahead, so the pattern is matched by hand (splitPieces).
type Tokenizer struct {
	vocab     map[string]int32
	ranks     map[[2]string]int
	added     [2][]addedToken // [0]: matched on the raw text; [1]: then on what is left ("normalized")
	bos       int32
	byteChar  [256]string
	cacheMu   sync.RWMutex
	cache     map[string][]int32
	cacheSize int
}

type addedToken struct {
	content string
	id      int32
}

type tokenizerFile struct {
	AddedTokens []struct {
		ID         int32  `json:"id"`
		Content    string `json:"content"`
		Normalized bool   `json:"normalized"`
		Lstrip     bool   `json:"lstrip"`
		Rstrip     bool   `json:"rstrip"`
		SingleWord bool   `json:"single_word"`
	} `json:"added_tokens"`
	Normalizer    json.RawMessage `json:"normalizer"`
	PreTokenizer  json.RawMessage `json:"pre_tokenizer"`
	PostProcessor json.RawMessage `json:"post_processor"`
	Model         struct {
		Type         string            `json:"type"`
		Vocab        map[string]int32  `json:"vocab"`
		Merges       []json.RawMessage `json:"merges"`
		ByteFallback bool              `json:"byte_fallback"`
		IgnoreMerges bool              `json:"ignore_merges"`
	} `json:"model"`
}

// The pre-tokenizer this implementation reproduces; any other is refused.
const supportedSplit = `(?i:'s|'t|'re|'ve|'m|'ll|'d)|[^\r\n\p{L}\p{N}]?\p{L}+|\p{N}{1,3}| ?[^\s\p{L}\p{N}]+[\r\n]*|\s*[\r\n]+|\s+(?!\S)|\s+`

// Parse reads a tokenizer.json. It refuses a file whose pipeline differs from the one
// implemented here, rather than tokenizing differently from the model's training.
func Parse(raw []byte) (*Tokenizer, error) {
	var f tokenizerFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, err
	}
	if f.Model.Type != "BPE" || f.Model.ByteFallback || f.Model.IgnoreMerges {
		return nil, fmt.Errorf("unsupported model (want byte-level BPE without byte fallback)")
	}
	if n := strings.TrimSpace(string(f.Normalizer)); n != "" && n != "null" {
		return nil, fmt.Errorf("unsupported normalizer %s", n)
	}
	var pre struct {
		Type          string `json:"type"`
		Pretokenizers []struct {
			Type    string `json:"type"`
			Pattern struct {
				Regex string `json:"Regex"`
			} `json:"pattern"`
			Behavior       string `json:"behavior"`
			Invert         bool   `json:"invert"`
			AddPrefixSpace bool   `json:"add_prefix_space"`
			UseRegex       bool   `json:"use_regex"`
		} `json:"pretokenizers"`
	}
	if err := json.Unmarshal(f.PreTokenizer, &pre); err != nil || len(pre.Pretokenizers) != 2 ||
		pre.Pretokenizers[0].Type != "Split" || pre.Pretokenizers[0].Pattern.Regex != supportedSplit ||
		pre.Pretokenizers[0].Behavior != "Isolated" || pre.Pretokenizers[0].Invert ||
		pre.Pretokenizers[1].Type != "ByteLevel" || pre.Pretokenizers[1].AddPrefixSpace || pre.Pretokenizers[1].UseRegex {
		return nil, fmt.Errorf("unsupported pre-tokenizer")
	}
	t := &Tokenizer{vocab: f.Model.Vocab, ranks: make(map[[2]string]int, len(f.Model.Merges)), cache: map[string][]int32{}}
	for i, m := range f.Model.Merges {
		var pair []string
		if err := json.Unmarshal(m, &pair); err != nil {
			var s string
			if err := json.Unmarshal(m, &s); err != nil {
				return nil, fmt.Errorf("merge %d: %w", i, err)
			}
			pair = strings.SplitN(s, " ", 2)
		}
		if len(pair) != 2 {
			return nil, fmt.Errorf("merge %d is not a pair", i)
		}
		k := [2]string{pair[0], pair[1]}
		if _, dup := t.ranks[k]; !dup {
			t.ranks[k] = i
		}
	}
	bos := int32(-1)
	for _, a := range f.AddedTokens {
		if a.Lstrip || a.Rstrip || a.SingleWord {
			return nil, fmt.Errorf("added token %q: lstrip/rstrip/single_word not supported", a.Content)
		}
		g := 0
		if a.Normalized {
			g = 1
		}
		t.added[g] = append(t.added[g], addedToken{a.Content, a.ID})
		if a.Content == "<|startoftext|>" {
			bos = a.ID
		}
	}
	if bos < 0 {
		return nil, fmt.Errorf("no <|startoftext|>")
	}
	t.bos = bos
	for g := range t.added { // longest first, so the first match at a position is the longest
		sort.SliceStable(t.added[g], func(i, j int) bool { return len(t.added[g][i].content) > len(t.added[g][j].content) })
	}
	// GPT-2's bytes_to_unicode
	n := 0
	for b := 0; b < 256; b++ {
		if (b >= '!' && b <= '~') || (b >= 0xA1 && b <= 0xAC) || (b >= 0xAE && b <= 0xFF) {
			t.byteChar[b] = string(rune(b))
		} else {
			t.byteChar[b] = string(rune(256 + n))
			n++
		}
	}
	return t, nil
}

// BOS is the id put in front of every sequence.
func (t *Tokenizer) BOS() int32 { return t.bos }

// Encode returns the ids the model reads for text: <|startoftext|> and then the text's
// tokens, cut so the whole is at most maxLength ids (0: no limit), as
// tokenizer(text, truncation=True, max_length=maxLength) does.
func (t *Tokenizer) Encode(text string, maxLength int) []int32 {
	ids := []int32{t.bos}
	for _, seg := range t.splitAdded(text) {
		if seg.id >= 0 {
			ids = append(ids, seg.id)
			continue
		}
		for _, piece := range splitPieces(seg.text) {
			ids = append(ids, t.bpe(piece)...)
		}
		if maxLength > 0 && len(ids) >= maxLength {
			break
		}
	}
	if maxLength > 0 && len(ids) > maxLength {
		ids = ids[:maxLength]
	}
	return ids
}

type segment struct {
	text string
	id   int32 // -1: ordinary text
}

func (t *Tokenizer) splitAdded(text string) []segment {
	segs := []segment{{text, -1}}
	for g := range t.added {
		if len(t.added[g]) == 0 {
			continue
		}
		var next []segment
		for _, s := range segs {
			if s.id >= 0 {
				next = append(next, s)
				continue
			}
			next = append(next, splitOn(s.text, t.added[g])...)
		}
		segs = next
	}
	return segs
}

// splitOn cuts text at the added tokens, leftmost first and the longest at a position.
func splitOn(text string, toks []addedToken) []segment {
	var first [256]bool
	for _, a := range toks {
		first[a.content[0]] = true
	}
	var out []segment
	last := 0
	for i := 0; i < len(text); i++ {
		if !first[text[i]] {
			continue
		}
		for _, a := range toks {
			if strings.HasPrefix(text[i:], a.content) {
				if i > last {
					out = append(out, segment{text[last:i], -1})
				}
				out = append(out, segment{a.content, a.id})
				i += len(a.content) - 1
				last = i + 1
				break
			}
		}
	}
	if last < len(text) {
		out = append(out, segment{text[last:], -1})
	}
	return out
}

func isL(r rune) bool { return unicode.IsLetter(r) }
func isN(r rune) bool { return unicode.IsNumber(r) }

// isS is Oniguruma's \s for UTF-8: \t \n \v \f \r, U+0085 and the Zs, Zl, Zp categories,
// which is Unicode's White_Space property.
func isS(r rune) bool { return unicode.IsSpace(r) }

// foldEq reports whether r matches the ASCII letter c case-insensitively, by Unicode
// simple case folding (so U+017F LONG S matches 's').
func foldEq(r rune, c rune) bool {
	if r == c {
		return true
	}
	for f := unicode.SimpleFold(c); f != c; f = unicode.SimpleFold(f) {
		if f == r {
			return true
		}
	}
	return false
}

var contractions = [][]rune{{'s'}, {'t'}, {'r', 'e'}, {'v', 'e'}, {'m'}, {'l', 'l'}, {'d'}}

// splitPieces applies the pre-tokenizer pattern: the alternatives tried in order at each
// position, as a backtracking engine does, the matches cutting the text into pieces.
func splitPieces(s string) []string {
	rs := []rune(s)
	n := len(rs)
	var out []string
	// byte offsets of each rune, for slicing s
	off := make([]int, n+1)
	o := 0
	for i, r := range rs {
		off[i] = o
		o += utf8.RuneLen(r)
		if r == utf8.RuneError {
			// the decoded string never holds invalid UTF-8, but keep offsets honest
			_, sz := utf8.DecodeRuneInString(s[off[i]:])
			o = off[i] + sz
		}
	}
	off[n] = len(s)
	at := func(i int) rune {
		if i < n {
			return rs[i]
		}
		return -1
	}
	i := 0
	for i < n {
		j := match(rs, i, n, at)
		out = append(out, s[off[i]:off[j]])
		i = j
	}
	return out
}

// match returns the end of the pattern's match at i (always > i: every character is
// matched by some alternative).
func match(rs []rune, i, n int, at func(int) rune) int {
	c := rs[i]
	// (?i:'s|'t|'re|'ve|'m|'ll|'d)
	if c == '\'' {
		for _, w := range contractions {
			k := 0
			for k < len(w) && i+1+k < n && foldEq(rs[i+1+k], w[k]) {
				k++
			}
			if k == len(w) {
				return i + 1 + k
			}
		}
	}
	// [^\r\n\p{L}\p{N}]?\p{L}+
	if c != '\r' && c != '\n' && !isL(c) && !isN(c) && isL(at(i+1)) {
		j := i + 2
		for j < n && isL(rs[j]) {
			j++
		}
		return j
	}
	if isL(c) {
		j := i + 1
		for j < n && isL(rs[j]) {
			j++
		}
		return j
	}
	// \p{N}{1,3}
	if isN(c) {
		j := i + 1
		for j < n && j < i+3 && isN(rs[j]) {
			j++
		}
		return j
	}
	// ' ?[^\s\p{L}\p{N}]+[\r\n]*'
	other := func(r rune) bool { return r >= 0 && !isS(r) && !isL(r) && !isN(r) }
	if start := i; other(c) || (c == ' ' && other(at(i+1))) {
		j := start
		if c == ' ' {
			j++
		}
		for j < n && other(rs[j]) {
			j++
		}
		for j < n && (rs[j] == '\r' || rs[j] == '\n') {
			j++
		}
		return j
	}
	// the rest start with whitespace
	if !isS(c) {
		return i + 1 // unreachable: a character is a letter, a number, whitespace or other
	}
	end := i
	for end < n && isS(rs[end]) {
		end++
	}
	// \s*[\r\n]+ : up to the run's last line break
	for k := end - 1; k >= i; k-- {
		if rs[k] == '\r' || rs[k] == '\n' {
			return k + 1
		}
	}
	// \s+(?!\S) : the run, less its last character when a non-space follows
	if end == n {
		return end
	}
	if end-1 > i {
		return end - 1
	}
	// \s+
	return end
}

const maxCache = 1 << 18

func (t *Tokenizer) bpe(piece string) []int32 {
	t.cacheMu.RLock()
	ids, ok := t.cache[piece]
	t.cacheMu.RUnlock()
	if ok {
		return ids
	}
	var b strings.Builder
	for i := 0; i < len(piece); i++ {
		b.WriteString(t.byteChar[piece[i]])
	}
	mapped := b.String()
	if id, ok := t.vocab[mapped]; ok && utf8.RuneCountInString(mapped) == 1 {
		ids = []int32{id}
	} else {
		parts := make([]string, 0, len(mapped))
		for _, r := range mapped {
			parts = append(parts, string(r))
		}
		for len(parts) > 1 {
			best, at := -1, -1
			for k := 0; k+1 < len(parts); k++ {
				if r, ok := t.ranks[[2]string{parts[k], parts[k+1]}]; ok && (best < 0 || r < best) {
					best, at = r, k
				}
			}
			if at < 0 {
				break
			}
			parts[at] = parts[at] + parts[at+1]
			parts = append(parts[:at+1], parts[at+2:]...)
		}
		ids = make([]int32, len(parts))
		for k, p := range parts {
			id, ok := t.vocab[p]
			if !ok {
				id = -1 // cannot happen with a byte-level vocabulary
			}
			ids[k] = id
		}
	}
	t.cacheMu.Lock()
	if t.cacheSize < maxCache {
		t.cache[piece] = ids
		t.cacheSize++
	}
	t.cacheMu.Unlock()
	return ids
}
