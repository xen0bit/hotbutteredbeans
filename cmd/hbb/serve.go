package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"

	"github.com/spf13/cobra"

	"github.com/xen0bit/hotbutteredbeans/internal/buildinfo"
	"github.com/xen0bit/hotbutteredbeans/internal/engine"
	"github.com/xen0bit/hotbutteredbeans/internal/scan"
)

// serveRequest is one line on stdin. The window headers show Path, so it is the
// path the caller wants the model to read, not necessarily where the bytes are:
// File names a file to read, Text carries the bytes themselves.
type serveRequest struct {
	ID   json.RawMessage `json:"id"`
	Path string          `json:"path"`
	File string          `json:"file,omitempty"`
	Text *string         `json:"text,omitempty"`
	Lang string          `json:"lang,omitempty"` // default: from Path's extension
}

type serveWindow struct {
	From int                `json:"from"`
	To   int                `json:"to"`
	P    map[string]float64 `json:"p"`
}

// serveResponse answers one request, in request order. Skip is a file the model
// has no language for: not an error, and not scored.
type serveResponse struct {
	ID      json.RawMessage `json:"id"`
	Path    string          `json:"path,omitempty"`
	Lang    string          `json:"lang,omitempty"`
	Lines   int             `json:"lines,omitempty"`
	Windows []serveWindow   `json:"windows,omitempty"`
	Cached  int             `json:"cached,omitempty"`
	Tokens  int             `json:"tokens,omitempty"`
	Skip    string          `json:"skip,omitempty"`
	Error   string          `json:"error,omitempty"`
}

type serveQuestion struct {
	CWE      string   `json:"cwe"`
	Title    string   `json:"title"`
	Q        string   `json:"q"`
	Langs    []string `json:"langs,omitempty"`
	NotLangs []string `json:"not_langs,omitempty"`
}

func serveCmd(g *globals) *cobra.Command {
	var stdio bool
	var hl httpListen
	cmd := &cobra.Command{
		Use:   "serve --stdio",
		Short: "Score files for another program: JSON lines in, JSON lines out",
		Long: `Load the model once and score the files a parent process names, for tools that drive
hbb (pwrq's invoke_hbb is one). The protocol is one JSON object per line.

On startup hbb writes a single line to stdout: {"ready":true, ...} with the model, device,
window rules and every question, so the caller knows what it is talking to. Then, for each
line on stdin
  {"id":1,"path":"src/a.c","file":"/abs/src/a.c"}      or  {"id":1,"path":"a.c","text":"..."}
hbb answers one line, in order:
  {"id":1,"path":"src/a.c","lang":"c","lines":412,"windows":[{"from":1,"to":240,"p":{"cwe_79":0.01,...}}]}
A file with no language is {"skip":"..."}; a failure is {"error":"..."} and the process goes
on. Only questions asked of the file's language appear in p. Diagnostics go to stderr, never
stdout. It exits when stdin closes.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			switch {
			case stdio && hl.addr != "":
				return fmt.Errorf("pass --stdio or --listen, not both")
			case !stdio && hl.addr == "":
				return fmt.Errorf("pass --stdio (JSON lines on stdin and stdout) or --listen host:port (HTTP)")
			}
			if hl.addr != "" {
				if err := hl.check(); err != nil {
					return err
				}
			}
			s, err := g.session(cmd, "")
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			m, err := s.model(ctx)
			if err != nil {
				return err
			}
			e, err := s.open(ctx, m)
			if err != nil {
				return err
			}
			defer e.Close()
			ready := readyDoc(m.Ref.String(), e)
			if hl.addr != "" {
				return hl.serve(ctx, ready, func(ctx context.Context, req *serveRequest) serveResponse {
					return s.serveOne(ctx, e, req)
				})
			}

			out := json.NewEncoder(os.Stdout)
			out.SetEscapeHTML(false)
			if err := out.Encode(ready); err != nil {
				return err
			}

			in := bufio.NewReaderSize(os.Stdin, 1<<20)
			for {
				line, err := in.ReadBytes('\n')
				if len(line) > 0 {
					var req serveRequest
					var resp serveResponse
					if jerr := json.Unmarshal(line, &req); jerr != nil {
						resp = serveResponse{Error: "bad request: " + jerr.Error()}
					} else {
						resp = s.serveOne(ctx, e, &req)
						resp.ID = req.ID
					}
					if werr := out.Encode(resp); werr != nil {
						return werr
					}
				}
				if err == io.EOF {
					return nil
				}
				if err != nil {
					return err
				}
				if cerr := ctx.Err(); cerr != nil {
					return cerr
				}
			}
		},
	}
	cmd.Flags().BoolVar(&stdio, "stdio", false, "speak the JSON-lines protocol on stdin and stdout")
	hl.flags(cmd)
	return cmd
}

// serveOne cuts and scores one file. Windows are the model's own (the same cut as
// scan), so a caller's From and To are the chunker's and the model cannot disagree.
func (s *session) serveOne(ctx context.Context, e *engine.Engine, req *serveRequest) serveResponse {
	b := e.Bundle()
	path := filepath.ToSlash(req.Path)
	r := serveResponse{Path: path}
	if path == "" {
		r.Error = "path is required: it is what the window headers show"
		return r
	}
	var data []byte
	switch {
	case req.Text != nil:
		data = []byte(*req.Text)
	case req.File != "":
		var err error
		if data, err = os.ReadFile(req.File); err != nil {
			r.Error = err.Error()
			return r
		}
	default:
		r.Error = "give file or text"
		return r
	}
	lang := req.Lang
	if lang == "" {
		lang = b.LangOf(path)
	}
	if lang == "" {
		r.Skip = "no language for this extension"
		return r
	}
	r.Lang = lang
	var targets []scan.Target
	for _, w := range b.Window.Windows(path, data) {
		targets = append(targets, scan.Target{Path: path, Lang: lang, Window: w})
	}
	if len(targets) == 0 {
		r.Skip = "empty"
		return r
	}
	res, st, err := scan.Score(ctx, e, targets, false, nil)
	if err != nil {
		r.Error = err.Error()
		return r
	}
	r.Cached, r.Tokens = st.Cached, st.Tokens
	for _, x := range res {
		r.Windows = append(r.Windows, serveWindow{From: x.Window.From, To: x.Window.To, P: x.P})
	}
	r.Lines = r.Windows[len(r.Windows)-1].To
	return r
}

// readyDoc is what a caller learns about the model before it asks anything: on stdio
// the first line, over HTTP GET /v1/info.
func readyDoc(model string, e *engine.Engine) map[string]any {
	b := e.Bundle()
	qs := map[string]serveQuestion{}
	labels := append([]string(nil), b.Labels...)
	sort.Strings(labels)
	for _, l := range labels {
		lim := b.Limits[l]
		qs[l] = serveQuestion{CWE: scan.CWE(l), Title: scan.Title(l), Q: b.Questions[l],
			Langs: lim.Langs, NotLangs: lim.NotLangs}
	}
	return map[string]any{
		"ready": true, "version": buildinfo.Ver(), "model": model,
		"device": string(e.Device), "window": b.Window, "labels": labels, "questions": qs,
	}
}
