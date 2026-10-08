package engine

import (
	"context"
	"math"
	"os"
	"testing"

	"github.com/xen0bit/hotbutteredbeans/internal/assets"
	"github.com/xen0bit/hotbutteredbeans/internal/testutil"
)

// The whole path, tokenizer included, against Python's ONNX Runtime on the conformance
// windows: needs HBB_TEST_BUNDLE and HBB_TEST_ORT_LIB (make test-model).
func TestScoresMatchPython(t *testing.T) {
	dir := testutil.BundleDir(t)
	lib := os.Getenv("HBB_TEST_ORT_LIB")
	if lib == "" {
		t.Skip("set HBB_TEST_ORT_LIB")
	}
	fx := testutil.LoadScores(t)
	e, err := Open(context.Background(), Config{Options: assets.Options{CacheDir: t.TempDir(), Offline: true},
		ModelDir: dir, ORTLib: lib, Device: CPU, NoScoreCache: true})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	var items []Item
	for _, w := range fx.Windows {
		items = append(items, Item{Text: w.Text})
	}
	logits, st, err := e.Score(context.Background(), items, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := fx.ONNX["model_q8.onnx"]
	worst := 0.0
	for i := range logits {
		for q := range logits[i] {
			worst = math.Max(worst, math.Abs(float64(logits[i][q])-want[i][q]))
		}
	}
	t.Logf("%d windows, %d tokens: max |logit - Python| %.2g", st.Windows, st.Tokens, worst)
	if worst > 1e-3 {
		t.Errorf("logits differ from Python's by %.3g", worst)
	}
}
