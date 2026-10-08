package window

import (
	"testing"

	"github.com/xen0bit/hotbutteredbeans/internal/testutil"
)

func TestConformance(t *testing.T) {
	c := testutil.LoadCases(t)
	for _, f := range c.Files {
		got := DefaultRules.Windows(f.Path, testutil.Data(t, f.DataB64))
		if len(got) != len(f.Windows) {
			t.Errorf("%s: %d windows, want %d", f.Path, len(got), len(f.Windows))
			continue
		}
		for i, w := range f.Windows {
			g := got[i]
			if g.From != w.From || g.To != w.To || g.Text != w.Text || g.Content != w.Content {
				t.Errorf("%s window %d: got %d-%d %.60q, want %d-%d %.60q", f.Path, i, g.From, g.To, g.Text, w.From, w.To, w.Text)
			}
		}
	}
}

func TestIsBinary(t *testing.T) {
	r := DefaultRules
	for _, tc := range []struct {
		data string
		want bool
	}{{"", true}, {"a\x00b", true}, {"plain", false}} {
		if got := r.IsBinary([]byte(tc.data)); got != tc.want {
			t.Errorf("IsBinary(%q) = %v", tc.data, got)
		}
	}
}

func TestDecodeUTF8(t *testing.T) {
	// Python: b"a\xe2\x82b\xff".decode("utf-8", "replace") == "a�b�"
	if got := DecodeUTF8([]byte("a\xe2\x82b\xff")); got != "a�b�" {
		t.Errorf("got %q", got)
	}
}
