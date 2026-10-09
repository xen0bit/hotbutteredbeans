package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"
)

// httpProtocol is bumped when the HTTP shape changes, so a client can refuse a server
// it does not understand.
const httpProtocol = 1

// httpListen is `hbb serve --listen`: the stdio protocol over HTTP, for a model that
// runs on another machine (one with a GPU) than the program asking.
//
//	GET  /v1/info    the document stdio prints as its first line
//	POST /v1/score   one serveRequest in, one serveResponse out
//	GET  /healthz    200 once the model is loaded
//
// The server never reads a path a client names: a request carries its text, because
// the caller and this machine do not share a disk.
type httpListen struct {
	addr      string
	tokenFile string
	maxBody   int64
	tlsCert   string
	tlsKey    string
	token     string
}

func (h *httpListen) flags(cmd *cobra.Command) {
	f := cmd.Flags()
	f.StringVar(&h.addr, "listen", "", "serve HTTP on host:port (instead of --stdio)")
	f.StringVar(&h.tokenFile, "token-file", "", "file holding the bearer token clients must send (or set HBB_SERVE_TOKEN)")
	f.Int64Var(&h.maxBody, "max-body", 8<<20, "largest request body accepted, in bytes")
	f.StringVar(&h.tlsCert, "tls-cert", "", "TLS certificate file (with --tls-key)")
	f.StringVar(&h.tlsKey, "tls-key", "", "TLS key file (with --tls-cert)")
}

// check resolves the token and refuses a listener that would be open to a network
// without one. Loopback may go without: only this machine can reach it.
func (h *httpListen) check() error {
	h.token = strings.TrimSpace(os.Getenv("HBB_SERVE_TOKEN"))
	if h.tokenFile != "" {
		b, err := os.ReadFile(h.tokenFile)
		if err != nil {
			return err
		}
		h.token = strings.TrimSpace(string(b))
	}
	if (h.tlsCert == "") != (h.tlsKey == "") {
		return fmt.Errorf("--tls-cert and --tls-key go together")
	}
	if h.maxBody < 1 {
		return fmt.Errorf("--max-body must be positive")
	}
	if h.token == "" && !loopback(h.addr) {
		return fmt.Errorf("--listen %s is reachable from the network: set --token-file or HBB_SERVE_TOKEN "+
			"(only a loopback address may go without)", h.addr)
	}
	return nil
}

func loopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// handler builds the routes. score is the same function stdio calls; a mutex
// serialises it, as stdio's one-line-at-a-time reading does, since the model is one
// session on one device.
func (h *httpListen) handler(ready map[string]any, score func(context.Context, *serveRequest) serveResponse) http.Handler {
	info := map[string]any{}
	for k, v := range ready {
		info[k] = v
	}
	info["protocol"] = httpProtocol
	info["max_body"] = h.maxBody

	var mu sync.Mutex
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok\n")
	})
	mux.HandleFunc("GET /v1/info", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, info)
	})
	mux.HandleFunc("POST /v1/score", func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, h.maxBody)
		var req serveRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			var tooBig *http.MaxBytesError
			if errors.As(err, &tooBig) {
				writeJSON(w, http.StatusRequestEntityTooLarge, serveResponse{
					Error: fmt.Sprintf("request is over the %d byte limit", h.maxBody)})
				return
			}
			writeJSON(w, http.StatusBadRequest, serveResponse{Error: "bad request: " + err.Error()})
			return
		}
		if req.File != "" {
			// Not a per-file error: the client is built wrong, and every file would fail.
			resp := serveResponse{ID: req.ID, Path: req.Path,
				Error: "file is not accepted over HTTP; send the bytes as text"}
			writeJSON(w, http.StatusBadRequest, resp)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		if r.Context().Err() != nil {
			return // gone while it waited its turn
		}
		resp := score(r.Context(), &req)
		resp.ID = req.ID
		writeJSON(w, http.StatusOK, resp)
	})

	if h.token == "" {
		return mux
	}
	want := []byte(h.token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			if subtle.ConstantTimeCompare([]byte(got), want) != 1 {
				w.Header().Set("WWW-Authenticate", `Bearer realm="hbb"`)
				writeJSON(w, http.StatusUnauthorized, serveResponse{Error: "missing or wrong bearer token"})
				return
			}
		}
		mux.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

// serve listens until ctx ends. The model is already loaded, so a connection that
// succeeds is one that will be answered.
func (h *httpListen) serve(ctx context.Context, ready map[string]any,
	score func(context.Context, *serveRequest) serveResponse) error {
	ln, err := net.Listen("tcp", h.addr)
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: h.handler(ready, score), ReadHeaderTimeout: 10 * time.Second}
	fmt.Fprintf(os.Stderr, "hbb: serving %v on %s (%v)\n", ready["model"], ln.Addr(), ready["device"])
	done := make(chan struct{})
	go func() {
		defer close(done)
		<-ctx.Done()
		sc, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sc)
	}()
	if h.tlsCert != "" {
		err = srv.ServeTLS(ln, h.tlsCert, h.tlsKey)
	} else {
		err = srv.Serve(ln)
	}
	if errors.Is(err, http.ErrServerClosed) {
		<-done
		return nil
	}
	return err
}
