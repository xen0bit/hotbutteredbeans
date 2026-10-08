// Package bundle reads an exported secjev model bundle: bundle.json (labels, questions,
// per-language limits, temperature, window rules, extension map) and tokenizer.json,
// from any fs.FS, so an embedded bundle and one on disk load the same way.
package bundle

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"math"
	"path"
	"strings"

	"github.com/xen0bit/hotbutteredbeans/internal/tokenizer"
	"github.com/xen0bit/hotbutteredbeans/internal/window"
)

// Limit says which languages a question is asked of.
type Limit struct {
	Langs    []string `json:"Langs"`
	NotLangs []string `json:"NotLangs"`
}

// Asked reports whether the question is asked of code in lang.
func (l Limit) Asked(lang string) bool {
	in := func(xs []string) bool {
		for _, x := range xs {
			if x == lang {
				return true
			}
		}
		return false
	}
	return (len(l.Langs) == 0 || in(l.Langs)) && !in(l.NotLangs)
}

// Bundle is a loaded model bundle, minus the graphs themselves.
type Bundle struct {
	Format       int                 `json:"format"`
	BaseModel    string              `json:"base_model"`
	Models       map[string]string   `json:"models"`        // variant -> graph file
	ExternalData map[string][]string `json:"external_data"` // graph file -> the weight files it references
	Input        []string            `json:"inputs"`
	Output       string              `json:"output"`
	Labels       []string            `json:"labels"`
	Questions    map[string]string   `json:"questions"`
	Limits       map[string]Limit    `json:"limits"`
	Temperature  float64             `json:"temperature"`
	MaxLength    int                 `json:"max_length"`
	Tokens       struct {
		BOS int32 `json:"bos"`
		Pad int32 `json:"pad"`
	} `json:"tokens"`
	Window     window.Rules      `json:"window"`
	Extensions map[string]string `json:"extensions"`
	SkipDirs   []string          `json:"skip_dirs"`

	Tokenizer *tokenizer.Tokenizer `json:"-"`
}

// Load reads bundle.json and tokenizer.json from the root of fsys.
func Load(fsys fs.FS) (*Bundle, error) {
	raw, err := fs.ReadFile(fsys, "bundle.json")
	if err != nil {
		return nil, err
	}
	b := &Bundle{Window: window.DefaultRules, Input: []string{"input_ids"}, Output: "logits"}
	if err := json.Unmarshal(raw, b); err != nil {
		return nil, fmt.Errorf("bundle.json: %w", err)
	}
	if b.Format != 1 {
		return nil, fmt.Errorf("bundle.json: format %d, hbb reads format 1", b.Format)
	}
	if len(b.Labels) == 0 || b.Temperature <= 0 || b.MaxLength <= 0 {
		return nil, fmt.Errorf("bundle.json: labels, temperature and max_length are required")
	}
	if len(b.Input) != 1 {
		return nil, fmt.Errorf("bundle.json: %d inputs, hbb feeds exactly one", len(b.Input))
	}
	tok, err := fs.ReadFile(fsys, "tokenizer.json")
	if err != nil {
		return nil, err
	}
	if b.Tokenizer, err = tokenizer.Parse(tok); err != nil {
		return nil, fmt.Errorf("tokenizer.json: %w", err)
	}
	if b.Tokenizer.BOS() != b.Tokens.BOS {
		return nil, fmt.Errorf("tokenizer BOS %d, bundle.json says %d", b.Tokenizer.BOS(), b.Tokens.BOS)
	}
	return b, nil
}

// Graph is the file name of a variant's graph ("q8", "fp32") and the weight files it needs.
func (b *Bundle) Graph(variant string) (file string, external []string, err error) {
	file, ok := b.Models[variant]
	if !ok {
		var have []string
		for v := range b.Models {
			have = append(have, v)
		}
		return "", nil, fmt.Errorf("the bundle has no %q graph (it has %s)", variant, strings.Join(have, ", "))
	}
	return file, b.ExternalData[file], nil
}

// Asked reports whether question q is asked of code in lang.
func (b *Bundle) Asked(q, lang string) bool { return b.Limits[q].Asked(lang) }

// LangOf is the language a file is read as by its extension, "" for one hbb skips.
func (b *Bundle) LangOf(name string) string {
	ext := path.Ext(name)
	if ext == "" {
		return ""
	}
	return b.Extensions[strings.ToLower(ext[1:])]
}

// Probability is the calibrated P(yes) of a logit: sigmoid(logit / temperature).
func (b *Bundle) Probability(logit float32) float64 {
	return 1 / (1 + math.Exp(-float64(logit)/b.Temperature))
}
