// Package testutil loads the conformance fixtures (testdata/conformance, written by
// secjev's export.py from the Python reference) for the tests of other packages.
package testutil

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
)

// Cases is testdata/conformance/cases.json: inputs chosen to break ports (invalid
// UTF-8, CRLF, long lines, halving, text that spells special tokens, Unicode classes)
// with Python's windows and token ids.
type Cases struct {
	Tokenizer []struct {
		Text string  `json:"text"`
		IDs  []int32 `json:"ids"`
	} `json:"tokenizer"`
	Files []struct {
		Path    string `json:"path"`
		DataB64 string `json:"data_b64"`
		Windows []struct {
			From    int     `json:"from"`
			To      int     `json:"to"`
			Text    string  `json:"text"`
			Content string  `json:"content"`
			IDs     []int32 `json:"ids"`
		} `json:"windows"`
	} `json:"files"`
}

// Scores is testdata/conformance/scores.json: windows with their logits from PyTorch
// and from each graph on ONNX Runtime's CPU provider.
type Scores struct {
	Labels  []string `json:"labels"`
	Windows []struct {
		Text string  `json:"text"`
		IDs  []int64 `json:"ids"`
	} `json:"windows"`
	PyTorch [][]float64            `json:"pytorch_fp32"`
	ONNX    map[string][][]float64 `json:"onnx_cpu"`
}

// Dir is testdata/conformance.
func Dir() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..", "testdata", "conformance")
}

func load(t testing.TB, name string, v any) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(Dir(), name))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, v); err != nil {
		t.Fatal(err)
	}
}

func LoadCases(t testing.TB) Cases   { var c Cases; load(t, "cases.json", &c); return c }
func LoadScores(t testing.TB) Scores { var s Scores; load(t, "scores.json", &s); return s }

// Run is the indices of the score fixtures to run: all of them, or with
// $HBB_TEST_MAX_TOKENS set, those of at most that many tokens (CI's small runners
// take minutes per 8,192-token window on the CPU).
func (s Scores) Run(t testing.TB) []int {
	limit := 0
	if v := os.Getenv("HBB_TEST_MAX_TOKENS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			t.Fatalf("HBB_TEST_MAX_TOKENS=%q: %v", v, err)
		}
		limit = n
	}
	var out []int
	for i, w := range s.Windows {
		if limit == 0 || len(w.IDs) <= limit {
			out = append(out, i)
		}
	}
	if len(out) < len(s.Windows) {
		t.Logf("%d of %d fixture windows (HBB_TEST_MAX_TOKENS=%d)", len(out), len(s.Windows), limit)
	}
	return out
}

// Data decodes a case file's bytes.
func Data(t testing.TB, b64 string) []byte {
	b, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// BundleDir is a model bundle folder for tests that need the tokenizer or the graphs:
// $HBB_TEST_BUNDLE, else the test is skipped. `make test-model` sets it.
func BundleDir(t testing.TB) string {
	dir := os.Getenv("HBB_TEST_BUNDLE")
	if dir == "" {
		t.Skip("set HBB_TEST_BUNDLE to a model bundle folder (make test-model does)")
	}
	return dir
}
