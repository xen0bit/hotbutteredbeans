package ort

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"
)

// The logits of the conformance windows (testdata/conformance/scores.json, written by
// secjev's export.py) against Python's ONNX Runtime on CPU. Needs HBB_TEST_ORT_LIB (an
// onnxruntime library) and HBB_TEST_BUNDLE (an exported bundle folder); HBB_TEST_CUDA=1
// runs on CUDA too.

type fixture struct {
	Windows []struct {
		IDs []int64 `json:"ids"`
	} `json:"windows"`
	ONNX map[string][][]float64 `json:"onnx_cpu"`
}

func setup(t *testing.T) (*Library, string, fixture) {
	lib, dir := os.Getenv("HBB_TEST_ORT_LIB"), os.Getenv("HBB_TEST_BUNDLE")
	if lib == "" || dir == "" {
		t.Skip("set HBB_TEST_ORT_LIB and HBB_TEST_BUNDLE")
	}
	l, err := Load(lib)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join("..", "..", "testdata", "conformance", "scores.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fx fixture
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatal(err)
	}
	return l, dir, fx
}

func check(t *testing.T, s *Session, fx fixture, want [][]float64, tol float64) {
	t.Helper()
	worst := 0.0
	for i, w := range fx.Windows {
		got, err := s.Run(w.IDs)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != len(want[i]) {
			t.Fatalf("window %d: %d logits, want %d", i, len(got), len(want[i]))
		}
		for q := range got {
			worst = math.Max(worst, math.Abs(float64(got[q])-want[i][q]))
		}
	}
	t.Logf("max |logit - Python ORT CPU| = %.2g over %d windows", worst, len(fx.Windows))
	if worst > tol {
		t.Errorf("logits differ by %.3g (tolerance %.g)", worst, tol)
	}
}

func TestFromFile(t *testing.T) {
	l, dir, fx := setup(t)
	t.Logf("onnxruntime %s, providers %v", l.Version, must(l.Providers()))
	s, err := l.NewSession(ModelSource{Path: filepath.Join(dir, "model_q8.onnx")}, "input_ids", "logits", SessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	check(t, s, fx, fx.ONNX["model_q8.onnx"], 1e-3)
}

func TestFromMemory(t *testing.T) {
	l, dir, fx := setup(t)
	graph := must(os.ReadFile(filepath.Join(dir, "model_q8.onnx")))
	weights := mapReadOnly(t, filepath.Join(dir, "model_q8.weights"))
	s, err := l.NewSession(ModelSource{Graph: graph, External: map[string][]byte{"model_q8.weights": weights}},
		"input_ids", "logits", SessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	check(t, s, fx, fx.ONNX["model_q8.onnx"], 1e-3)
}

func TestCUDA(t *testing.T) {
	if os.Getenv("HBB_TEST_CUDA") == "" {
		t.Skip("set HBB_TEST_CUDA=1")
	}
	l, dir, fx := setup(t)
	s, err := l.NewSession(ModelSource{Path: filepath.Join(dir, "model_q8.onnx")}, "input_ids", "logits",
		SessionOptions{Provider: CUDA})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	check(t, s, fx, fx.ONNX["model_q8.onnx"], 3e-2) // CUDA multiplies in TF32
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}
