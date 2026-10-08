// Package engine runs the model: it resolves the model and ONNX Runtime, picks the
// device (the GPU when there is a usable one, else the CPU), and scores window texts,
// reusing cached scores.
package engine

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/xen0bit/hotbutteredbeans/internal/assets"
	"github.com/xen0bit/hotbutteredbeans/internal/bundle"
	"github.com/xen0bit/hotbutteredbeans/internal/ort"
	"github.com/xen0bit/hotbutteredbeans/internal/scorecache"
)

// Device is what to run on.
type Device string

const (
	Auto   Device = "auto"   // the GPU when one is usable, else the CPU
	GPU    Device = "gpu"    // the GPU (CUDA) or fail
	CPU    Device = "cpu"    // the CPU only
	CoreML Device = "coreml" // Apple's Core ML (macOS), or fail
)

// ParseDevice reads a device name ("cuda" is "gpu").
func ParseDevice(s string) (Device, error) {
	switch d := Device(strings.ToLower(strings.TrimSpace(s))); d {
	case "", Auto:
		return Auto, nil
	case GPU, "cuda":
		return GPU, nil
	case CPU, CoreML:
		return d, nil
	}
	return "", fmt.Errorf("unknown device %q (auto, gpu, cpu, coreml)", s)
}

// Config is how to open the engine.
type Config struct {
	assets.Options
	ModelDir string
	Model    assets.ModelRef
	ORTLib   string
	Device   Device
	GPUID    int
	Threads  int
	// FetchGPURuntime allows auto mode to download the GPU build of ONNX Runtime
	// (~235 MB) when the CUDA libraries are present but it is not cached yet.
	FetchGPURuntime bool
	NoScoreCache    bool
}

// Engine is an open model.
type Engine struct {
	Model   *assets.Model
	Runtime *assets.Runtime
	Lib     *ort.Library
	Device  Device // what it runs on: GPU, CPU or CoreML
	// Fallback says why auto mode did not use the GPU ("" when it did, or was not asked).
	Fallback string
	Load     time.Duration

	sess  *ort.Session
	cache *scorecache.Cache
}

// Open resolves everything and loads the model.
func Open(ctx context.Context, c Config) (*Engine, error) {
	m, err := assets.ResolveModel(ctx, assets.ModelOptions{Options: c.Options, Dir: c.ModelDir, Ref: c.Model})
	if err != nil {
		return nil, err
	}
	return OpenModel(ctx, c, m)
}

// OpenModel loads a resolved model on a device.
func OpenModel(ctx context.Context, c Config, m *assets.Model) (*Engine, error) {
	t0 := time.Now()
	e := &Engine{Model: m}
	if err := e.openSession(ctx, c); err != nil {
		return nil, err
	}
	e.Load = time.Since(t0)
	if m.Source == "embedded" {
		for _, b := range m.Graph.External {
			release(b)
		}
	}
	if !c.NoScoreCache {
		cc, err := scorecache.Open(filepath.Join(c.Options.CacheDirOrDefault(), "scores"), m.ID+"-"+string(e.Device), len(m.Bundle.Labels))
		if err == nil {
			e.cache = cc
		} else {
			c.Options.Logf("score cache disabled: %v", err)
		}
	}
	return e, nil
}

func (e *Engine) openSession(ctx context.Context, c Config) error {
	b := e.Model.Bundle
	in, out := b.Input[0], b.Output
	so := ort.SessionOptions{Threads: c.Threads, Device: c.GPUID}

	tryGPU := c.Device == GPU || c.Device == Auto && assets.CUDAPlatform()
	if tryGPU {
		err := e.openGPU(ctx, c, so, in, out)
		if err == nil {
			return nil
		}
		if c.Device == GPU {
			return fmt.Errorf("GPU: %w (use --device cpu to run on the CPU)", err)
		}
		e.Fallback = err.Error()
	} else if c.Device == Auto {
		e.Fallback = fmt.Sprintf("ONNX Runtime has no CUDA build for %s", assets.Platform())
	}

	rt, err := assets.ResolveRuntime(ctx, assets.RuntimeOptions{Options: c.Options, Lib: c.ORTLib})
	if err != nil {
		return err
	}
	lib, err := ort.Load(rt.Path)
	if err != nil {
		return err
	}
	e.Runtime, e.Lib = rt, lib
	if c.Device == CoreML {
		so.Provider = ort.CoreML
		if e.sess, err = lib.NewSession(e.Model.Graph, in, out, so); err != nil {
			return fmt.Errorf("Core ML: %w (use --device cpu to run on the CPU)", err)
		}
		e.Device = CoreML
		return nil
	}
	so.Provider = ort.CPU
	if e.sess, err = lib.NewSession(e.Model.Graph, in, out, so); err != nil {
		return fmt.Errorf("loading the model on the CPU: %w", err)
	}
	e.Device = CPU
	return nil
}

// openGPU loads the model on CUDA: the CUDA libraries are found and preloaded, the GPU
// build of ONNX Runtime is taken from the cache (or fetched), and the session is created
// with the CUDA provider.
func (e *Engine) openGPU(ctx context.Context, c Config, so ort.SessionOptions, in, out string) error {
	g := assets.FindGPU(&c.Options)
	if !g.Usable() {
		return errors.New(g.Why())
	}
	var rt *assets.Runtime
	var err error
	if c.ORTLib != "" {
		rt = &assets.Runtime{Path: c.ORTLib, Source: "flag"}
	} else {
		fetch := c.FetchGPURuntime || c.Device == GPU
		rt, err = assets.ResolveRuntime(ctx, assets.RuntimeOptions{Options: c.Options, CUDA: true, FetchCUDA: fetch})
		if errors.Is(err, assets.ErrNotFetched) {
			return errors.New("the GPU build of ONNX Runtime is not downloaded yet; `hbb runtime fetch --gpu` gets it")
		}
		if err != nil {
			return err
		}
	}
	lib, err := ort.Load(rt.Path)
	if err != nil {
		return err
	}
	if !lib.HasProvider("CUDAExecutionProvider") {
		return fmt.Errorf("%s is not a GPU build of ONNX Runtime", rt.Path)
	}
	if err := ort.Preload(g.Libs); err != nil {
		return fmt.Errorf("loading the CUDA libraries: %w", err)
	}
	so.Provider = ort.CUDA
	sess, err := lib.NewSession(e.Model.Graph, in, out, so)
	if err != nil {
		return err
	}
	rt.CUDA = true
	e.Runtime, e.Lib, e.sess, e.Device = rt, lib, sess, GPU
	return nil
}

// Item is a window text to score.
type Item struct {
	Text string
}

// Stats is what scoring spent.
type Stats struct {
	Windows, Cached, Tokens, Truncated int
	Tokenize, Score                    time.Duration
}

// Score returns each item's logits (len(Bundle.Labels) each, before the temperature).
// progress, if not nil, is called after each window.
func (e *Engine) Score(ctx context.Context, items []Item, progress func(done, total int)) ([][]float32, Stats, error) {
	b := e.Model.Bundle
	st := Stats{Windows: len(items)}
	out := make([][]float32, len(items))
	keys := make([]scorecache.Key, len(items))
	var todo []int
	for i, it := range items {
		keys[i] = scorecache.KeyOf(it.Text)
		if v, ok := e.cache.Get(keys[i]); ok {
			out[i] = v
			st.Cached++
			continue
		}
		todo = append(todo, i)
	}
	done := st.Cached
	if progress != nil && done > 0 {
		progress(done, len(items))
	}

	// Tokenize ahead of the model, on another goroutine.
	type job struct {
		i   int
		ids []int64
		dur time.Duration
	}
	jobs := make(chan job, 4)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		defer close(jobs)
		for _, i := range todo {
			t := time.Now()
			ids32 := b.Tokenizer.Encode(items[i].Text, b.MaxLength)
			ids := make([]int64, len(ids32))
			for k, v := range ids32 {
				ids[k] = int64(v)
			}
			select {
			case jobs <- job{i, ids, time.Since(t)}:
			case <-ctx.Done():
				return
			}
		}
	}()
	for j := range jobs {
		if err := ctx.Err(); err != nil {
			return nil, st, err
		}
		st.Tokens += len(j.ids)
		st.Tokenize += j.dur
		if len(j.ids) >= b.MaxLength {
			st.Truncated++
		}
		t := time.Now()
		logits, err := e.sess.Run(j.ids)
		st.Score += time.Since(t)
		if err != nil {
			return nil, st, fmt.Errorf("scoring a window: %w", err)
		}
		if len(logits) != len(b.Labels) {
			return nil, st, fmt.Errorf("the model returned %d logits, the bundle has %d labels", len(logits), len(b.Labels))
		}
		out[j.i] = logits
		e.cache.Put(keys[j.i], logits)
		done++
		if progress != nil {
			progress(done, len(items))
		}
	}
	return out, st, ctx.Err()
}

// Logits scores one window text, bypassing the score cache.
func (e *Engine) Logits(text string) ([]float32, error) {
	b := e.Model.Bundle
	ids32 := b.Tokenizer.Encode(text, b.MaxLength)
	ids := make([]int64, len(ids32))
	for i, v := range ids32 {
		ids[i] = int64(v)
	}
	return e.sess.Run(ids)
}

// Bundle is the model's bundle.
func (e *Engine) Bundle() *bundle.Bundle { return e.Model.Bundle }

// CachedScores is the number of windows in the score cache.
func (e *Engine) CachedScores() int { return e.cache.Len() }

// Close frees the session.
func (e *Engine) Close() {
	if e.sess != nil {
		e.sess.Close()
	}
	e.cache.Close()
	runtime.KeepAlive(e.Model)
}
