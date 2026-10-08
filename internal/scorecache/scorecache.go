// Package scorecache remembers a model's logits for window texts it has scored, so
// windows that did not change (most of them, between two commits or two CI runs) are
// never scored twice. One append-only file per model: a record is the sha256 of the
// window text, the logits, and a CRC; a torn or corrupt record is skipped on load.
package scorecache

import (
	"bufio"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"io"
	"math"
	"os"
	"path/filepath"
	"sync"
)

const maxSize = 128 << 20 // start over past this

// Key identifies a window text.
type Key [32]byte

// KeyOf is the key of a window text.
func KeyOf(text string) Key { return sha256.Sum256([]byte(text)) }

// Cache is one model's scores. A nil *Cache caches nothing.
type Cache struct {
	mu     sync.Mutex
	n      int // logits per record
	scores map[Key][]float32
	f      *os.File
}

// Open loads (or creates) the cache of model id under dir, for n logits per window.
func Open(dir, id string, n int) (*Cache, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, id+".bin")
	if st, err := os.Stat(path); err == nil && st.Size() > maxSize {
		os.Remove(path)
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	c := &Cache{n: n, scores: map[Key][]float32{}, f: f}
	if err := c.load(); err != nil {
		f.Close()
		return nil, err
	}
	return c, nil
}

func (c *Cache) recSize() int { return 32 + 4*c.n + 4 }

func (c *Cache) load() error {
	if _, err := c.f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	r := bufio.NewReaderSize(c.f, 1<<16)
	rec := make([]byte, c.recSize())
	for {
		if _, err := io.ReadFull(r, rec); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return nil
			}
			return err
		}
		body := rec[:len(rec)-4]
		if crc32.ChecksumIEEE(body) != binary.LittleEndian.Uint32(rec[len(rec)-4:]) {
			continue
		}
		var k Key
		copy(k[:], body)
		v := make([]float32, c.n)
		for i := range v {
			v[i] = math.Float32frombits(binary.LittleEndian.Uint32(body[32+4*i:]))
		}
		c.scores[k] = v
	}
}

// Get returns the logits for a key.
func (c *Cache) Get(k Key) ([]float32, bool) {
	if c == nil {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.scores[k]
	return v, ok
}

// Put records logits; write errors only lose the cache entry.
func (c *Cache) Put(k Key, logits []float32) {
	if c == nil || len(logits) != c.n {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.scores[k] = logits
	rec := make([]byte, c.recSize())
	copy(rec, k[:])
	for i, v := range logits {
		binary.LittleEndian.PutUint32(rec[32+4*i:], math.Float32bits(v))
	}
	binary.LittleEndian.PutUint32(rec[len(rec)-4:], crc32.ChecksumIEEE(rec[:len(rec)-4]))
	_, _ = c.f.Write(rec) // one write per record: O_APPEND keeps concurrent writers whole
}

// Len is the number of cached windows.
func (c *Cache) Len() int {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.scores)
}

// Close closes the file.
func (c *Cache) Close() error {
	if c == nil {
		return nil
	}
	return c.f.Close()
}
