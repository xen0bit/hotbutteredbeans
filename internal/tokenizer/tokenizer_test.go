package tokenizer

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/xen0bit/hotbutteredbeans/internal/testutil"
)

func TestConformance(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(testutil.BundleDir(t), "tokenizer.json"))
	if err != nil {
		t.Fatal(err)
	}
	tok, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	const maxLength = 8192
	c := testutil.LoadCases(t)
	for _, tc := range c.Tokenizer {
		if got := tok.Encode(tc.Text, maxLength); !slices.Equal(got, tc.IDs) {
			t.Errorf("Encode(%.60q):\n got %v\nwant %v", tc.Text, got, tc.IDs)
		}
	}
	n := 0
	for _, f := range c.Files {
		for i, w := range f.Windows {
			n++
			if got := tok.Encode(w.Text, maxLength); !slices.Equal(got, w.IDs) {
				t.Errorf("%s window %d: %d ids, want %d", f.Path, i, len(got), len(w.IDs))
			}
		}
	}
	t.Logf("%d texts and %d windows match", len(c.Tokenizer), n)
}
