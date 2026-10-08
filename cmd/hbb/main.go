// Command hbb (Hot Buttered Beans) ranks source code by how likely it is to hold one of
// the CWE Top 25 weaknesses, with a local encoder model: a whole tree (hbb scan), or
// only what a change touched (hbb diff), as a git hook, in CI, or as context for an LLM.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"

	"github.com/spf13/cobra"

	"github.com/xen0bit/hotbutteredbeans/internal/buildinfo"
)

// Exit codes.
const (
	exitOK       = 0
	exitFindings = 1 // a finding reached --fail-at / --fail-delta
	exitError    = 2
)

// exitError carries an exit code out of a command.
type exitCode int

func (e exitCode) Error() string { return fmt.Sprintf("exit %d", int(e)) }

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	root := rootCmd()
	err := root.ExecuteContext(ctx)
	var code exitCode
	switch {
	case err == nil:
		os.Exit(exitOK)
	case errors.As(err, &code):
		os.Exit(int(code))
	default:
		fmt.Fprintf(os.Stderr, "hbb: %v\n", err)
		os.Exit(exitError)
	}
}

func rootCmd() *cobra.Command {
	g := &globals{}
	root := &cobra.Command{
		Use:   "hbb",
		Short: "Rank code by likely security weaknesses (CWE Top 25) with a local model",
		Long: `hbb (Hot Buttered Beans) reads source code in the 240-line windows the secjev encoder
was trained on and scores each window on the 25 questions of the CWE Top 25. The
result is a reading order: the code most worth a security review first.

  hbb scan [path]                 rank a whole tree
  hbb diff                        rank what changed in the work tree
  hbb diff --staged               ... in the index (what a commit will hold)
  hbb diff main...HEAD --compare  ... on a branch, with each score's rise from the base
  hbb hook install                run on every commit
  hbb doctor                      show the model, runtime and device hbb will use

The model and ONNX Runtime come built in (full builds) or are fetched once into the
cache. It runs on the GPU when a usable NVIDIA one is found, else the CPU.`,
		Version:       buildinfo.Ver(),
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.SetVersionTemplate("hbb {{.Version}}\n")
	g.register(root)
	root.AddCommand(scanCmd(g), diffCmd(g), hookCmd(g), modelCmd(g), runtimeCmd(g), doctorCmd(g),
		configCmd(g), versionCmd(g), windowsCmd(g))
	return root
}
