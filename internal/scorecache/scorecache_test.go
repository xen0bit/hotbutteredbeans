package scorecache

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestRoundTripAndTornRecord(t *testing.T) {
	dir := t.TempDir()
	c, err := Open(dir, "m", 3)
	if err != nil {
		t.Fatal(err)
	}
	c.Put(KeyOf("a"), []float32{1, 2, 3})
	c.Put(KeyOf("b"), []float32{4, 5, 6})
	c.Close()
	// a torn write at the end (a killed process) must not lose the others
	f, _ := os.OpenFile(filepath.Join(dir, "m.bin"), os.O_APPEND|os.O_WRONLY, 0)
	f.Write([]byte{1, 2, 3})
	f.Close()
	c, err = Open(dir, "m", 3)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if v, ok := c.Get(KeyOf("b")); !ok || !slices.Equal(v, []float32{4, 5, 6}) {
		t.Errorf("got %v %v", v, ok)
	}
	if c.Len() != 2 {
		t.Errorf("len %d", c.Len())
	}
	var nilCache *Cache
	nilCache.Put(KeyOf("x"), []float32{1})
	if _, ok := nilCache.Get(KeyOf("x")); ok {
		t.Error("a nil cache caches nothing")
	}
}
