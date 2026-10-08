// Package ort is a minimal binding to the ONNX Runtime C API, loaded at run time
// without cgo (purego on Unix, LoadLibraryEx on Windows). It covers exactly what hbb
// needs: one environment, sessions from a file or from memory (with external weights
// supplied from memory), the CPU, CUDA and CoreML execution providers, and running a
// graph with one int64 input and one float32 output.
//
// The OrtApi function table is append-only across releases, so the indices below,
// taken from onnxruntime_c_api.h, hold for every library that serves API version
// apiVersion or newer.
package ort

import (
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"unsafe"
)

// apiVersion is the OrtApi version requested: 18 (ONNX Runtime 1.18) added
// AddExternalInitializersFromFilesInMemory, the newest function used here.
const apiVersion = 18

// Indices into the OrtApi function table (onnxruntime_c_api.h, struct OrtApi).
const (
	fnGetErrorCode                          = 1
	fnGetErrorMessage                       = 2
	fnCreateEnv                             = 3
	fnDisableTelemetryEvents                = 6
	fnCreateSession                         = 7
	fnCreateSessionFromArray                = 8
	fnRun                                   = 9
	fnCreateSessionOptions                  = 10
	fnSetSessionLogSeverityLevel            = 22
	fnSetIntraOpNumThreads                  = 24
	fnCreateTensorWithDataAsOrtValue        = 49
	fnGetTensorMutableData                  = 51
	fnGetTensorShapeElementCount            = 64
	fnGetTensorTypeAndShape                 = 65
	fnCreateCpuMemoryInfo                   = 69
	fnReleaseEnv                            = 92
	fnReleaseStatus                         = 93
	fnReleaseMemoryInfo                     = 94
	fnReleaseSession                        = 95
	fnReleaseValue                          = 96
	fnReleaseTensorTypeAndShapeInfo         = 99
	fnReleaseSessionOptions                 = 100
	fnGetAvailableProviders                 = 125
	fnReleaseAvailableProviders             = 126
	fnAddSessionConfigEntry                 = 130
	fnSessionOptionsAppendCUDAV2            = 204
	fnCreateCUDAProviderOptions             = 205
	fnUpdateCUDAProviderOptions             = 206
	fnReleaseCUDAProviderOptions            = 208
	fnSessionOptionsAppendExecutionProvider = 216
	fnAddExternalInitializersFromMemory     = 279
)

const (
	logWarning = 2
	logError   = 3

	tensorInt64 = 7 // ONNX_TENSOR_ELEMENT_DATA_TYPE_INT64

	allocatorArena = 1 // OrtArenaAllocator
	memTypeDefault = 0 // OrtMemTypeDefault
)

// Library is a loaded onnxruntime shared library.
type Library struct {
	Path    string
	Version string
	api     unsafe.Pointer // const OrtApi*

	mu  sync.Mutex
	env uintptr // OrtEnv*, created on first use
}

var (
	loadMu sync.Mutex
	loaded = map[string]*Library{}
)

// Load opens the onnxruntime library at path (a full path: the system's search path is
// never used, so an unrelated onnxruntime on it cannot be picked up by accident).
// Loading the same path twice returns the same Library.
func Load(path string) (*Library, error) {
	loadMu.Lock()
	defer loadMu.Unlock()
	if l, ok := loaded[path]; ok {
		return l, nil
	}
	if !strings.HasPrefix(path, "/proc/self/fd/") {
		if abs, err := filepath.Abs(path); err == nil {
			path = abs
		}
	}
	base, err := openLibrary(path) // const OrtApiBase*
	if err != nil {
		return nil, fmt.Errorf("loading %s: %w", path, err)
	}
	// struct OrtApiBase { const OrtApi* (*GetApi)(uint32_t); const char* (*GetVersionString)(void); }
	getAPI := *(*uintptr)(base)
	getVersion := *(*uintptr)(unsafe.Add(base, ptrSize))
	version := goString(call(getVersion))
	api := call(getAPI, apiVersion)
	if api == 0 {
		return nil, fmt.Errorf("%s: onnxruntime %s does not serve C API version %d (1.18 or newer is required)", path, version, apiVersion)
	}
	l := &Library{Path: path, Version: version, api: toPointer(api)}
	loaded[path] = l
	return l, nil
}

const ptrSize = unsafe.Sizeof(uintptr(0))

// fn returns the address of the OrtApi function at index i.
func (l *Library) fn(i int) uintptr { return *(*uintptr)(unsafe.Add(l.api, uintptr(i)*ptrSize)) }

// status turns an OrtStatus* into an error, releasing it.
func (l *Library) status(st uintptr) error {
	if st == 0 {
		return nil
	}
	msg := goString(call(l.fn(fnGetErrorMessage), st))
	call(l.fn(fnReleaseStatus), st)
	return errors.New(strings.TrimSpace(msg))
}

func (l *Library) do(i int, args ...uintptr) error { return l.status(call(l.fn(i), args...)) }

func (l *Library) environment() (uintptr, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.env != 0 {
		return l.env, nil
	}
	id := cString("hbb")
	var env uintptr
	if err := l.do(fnCreateEnv, logError, ptrOf(id), uintptr(unsafe.Pointer(&env))); err != nil {
		return 0, fmt.Errorf("creating the onnxruntime environment: %w", err)
	}
	runtime.KeepAlive(id)
	_ = l.do(fnDisableTelemetryEvents, env)
	l.env = env
	return env, nil
}

// Providers lists the execution providers this library was built with.
func (l *Library) Providers() ([]string, error) {
	var arr uintptr
	var n int32
	if err := l.do(fnGetAvailableProviders, uintptr(unsafe.Pointer(&arr)), uintptr(unsafe.Pointer(&n))); err != nil {
		return nil, err
	}
	out := make([]string, n)
	for i := range out {
		out[i] = goString(*(*uintptr)(unsafe.Add(toPointer(arr), uintptr(i)*ptrSize)))
	}
	_ = l.do(fnReleaseAvailableProviders, arr, uintptr(n))
	return out, nil
}

// HasProvider reports whether the library was built with the named execution provider
// (e.g. "CUDAExecutionProvider").
func (l *Library) HasProvider(name string) bool {
	ps, err := l.Providers()
	if err != nil {
		return false
	}
	for _, p := range ps {
		if p == name {
			return true
		}
	}
	return false
}

// Provider is an execution provider a session runs on.
type Provider string

const (
	CPU    Provider = "cpu"
	CUDA   Provider = "cuda"
	CoreML Provider = "coreml"
)

// SessionOptions configure a session.
type SessionOptions struct {
	Provider Provider
	Device   int // CUDA device id
	Threads  int // intra-op threads; 0: ONNX Runtime's default (the physical cores)
	Verbose  bool
}

// ModelSource is a graph to load: a file on disk, or the graph's bytes plus the external
// data files it references, keyed by the name the graph uses for them.
type ModelSource struct {
	Path     string
	Graph    []byte
	External map[string][]byte
}

// Session is one loaded graph. Run it from one goroutine at a time.
type Session struct {
	lib     *Library
	sess    uintptr
	mem     uintptr
	keep    []any // memory ONNX Runtime may still read (external weights)
	inName  []byte
	outName []byte
}

// NewSession loads a graph with one input and one output.
func (l *Library) NewSession(src ModelSource, input, output string, o SessionOptions) (*Session, error) {
	env, err := l.environment()
	if err != nil {
		return nil, err
	}
	var opts uintptr
	if err := l.do(fnCreateSessionOptions, uintptr(unsafe.Pointer(&opts))); err != nil {
		return nil, err
	}
	defer call(l.fn(fnReleaseSessionOptions), opts)
	if !o.Verbose {
		_ = l.do(fnSetSessionLogSeverityLevel, opts, logError)
	}
	if o.Threads > 0 {
		if err := l.do(fnSetIntraOpNumThreads, opts, uintptr(o.Threads)); err != nil {
			return nil, err
		}
	}
	if err := l.appendProvider(opts, o); err != nil {
		return nil, err
	}

	s := &Session{lib: l, inName: cString(input), outName: cString(output)}
	if src.Path != "" {
		p := osString(src.Path)
		err = l.do(fnCreateSession, env, ptrOf(p), opts, uintptr(unsafe.Pointer(&s.sess)))
		runtime.KeepAlive(p)
	} else {
		if len(src.External) > 0 {
			if err := l.addExternal(opts, src.External); err != nil {
				return nil, err
			}
			for _, b := range src.External {
				s.keep = append(s.keep, b)
			}
		}
		err = l.do(fnCreateSessionFromArray, env, ptrOf(src.Graph), uintptr(len(src.Graph)), opts,
			uintptr(unsafe.Pointer(&s.sess)))
		runtime.KeepAlive(src.Graph)
	}
	if err != nil {
		return nil, err
	}
	if err := l.do(fnCreateCpuMemoryInfo, allocatorArena, memTypeDefault, uintptr(unsafe.Pointer(&s.mem))); err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}

func (l *Library) appendProvider(opts uintptr, o SessionOptions) error {
	switch o.Provider {
	case "", CPU:
		return nil
	case CUDA:
		var cuda uintptr
		if err := l.do(fnCreateCUDAProviderOptions, uintptr(unsafe.Pointer(&cuda))); err != nil {
			return fmt.Errorf("CUDA: %w", err)
		}
		defer call(l.fn(fnReleaseCUDAProviderOptions), cuda)
		keys, vals := cStrings("device_id"), cStrings(fmt.Sprint(o.Device))
		if err := l.do(fnUpdateCUDAProviderOptions, cuda, ptrOf(keys.ptrs), ptrOf(vals.ptrs), 1); err != nil {
			return fmt.Errorf("CUDA: %w", err)
		}
		runtime.KeepAlive(keys)
		runtime.KeepAlive(vals)
		if err := l.do(fnSessionOptionsAppendCUDAV2, opts, cuda); err != nil {
			return fmt.Errorf("CUDA: %w", err)
		}
		return nil
	case CoreML:
		name := cString("CoreML")
		err := l.do(fnSessionOptionsAppendExecutionProvider, opts, ptrOf(name), 0, 0, 0)
		runtime.KeepAlive(name)
		if err != nil {
			return fmt.Errorf("CoreML: %w", err)
		}
		return nil
	}
	return fmt.Errorf("unknown execution provider %q", o.Provider)
}

func (l *Library) addExternal(opts uintptr, files map[string][]byte) error {
	names := make([][]byte, 0, len(files))
	bufs := make([]uintptr, 0, len(files))
	lens := make([]uintptr, 0, len(files))
	for name, b := range files {
		names = append(names, osString(name))
		bufs = append(bufs, ptrOf(b))
		lens = append(lens, uintptr(len(b)))
	}
	namePtrs := make([]uintptr, len(names))
	for i, n := range names {
		namePtrs[i] = ptrOf(n)
	}
	err := l.do(fnAddExternalInitializersFromMemory, opts, ptrOf(namePtrs), ptrOf(bufs), ptrOf(lens), uintptr(len(files)))
	runtime.KeepAlive(names)
	runtime.KeepAlive(files)
	return err
}

// Run feeds ids as an int64 tensor of shape [1, len(ids)] and returns the output's
// float32 values.
func (s *Session) Run(ids []int64) ([]float32, error) {
	l := s.lib
	shape := []int64{1, int64(len(ids))}
	var in uintptr
	if err := l.do(fnCreateTensorWithDataAsOrtValue, s.mem, ptrOf(ids), uintptr(len(ids))*8,
		ptrOf(shape), uintptr(len(shape)), tensorInt64, uintptr(unsafe.Pointer(&in))); err != nil {
		return nil, err
	}
	defer call(l.fn(fnReleaseValue), in)
	inNames := []uintptr{ptrOf(s.inName)}
	outNames := []uintptr{ptrOf(s.outName)}
	inputs := []uintptr{in}
	outputs := []uintptr{0}
	err := l.do(fnRun, s.sess, 0, ptrOf(inNames), ptrOf(inputs), 1, ptrOf(outNames), 1, ptrOf(outputs))
	runtime.KeepAlive(ids)
	runtime.KeepAlive(shape)
	runtime.KeepAlive(s)
	if err != nil {
		return nil, err
	}
	out := outputs[0]
	defer call(l.fn(fnReleaseValue), out)

	var info uintptr
	if err := l.do(fnGetTensorTypeAndShape, out, uintptr(unsafe.Pointer(&info))); err != nil {
		return nil, err
	}
	var n uintptr
	err = l.do(fnGetTensorShapeElementCount, info, uintptr(unsafe.Pointer(&n)))
	call(l.fn(fnReleaseTensorTypeAndShapeInfo), info)
	if err != nil {
		return nil, err
	}
	var data uintptr
	if err := l.do(fnGetTensorMutableData, out, uintptr(unsafe.Pointer(&data))); err != nil {
		return nil, err
	}
	return append([]float32(nil), unsafe.Slice((*float32)(toPointer(data)), n)...), nil
}

// Close frees the session.
func (s *Session) Close() {
	if s.mem != 0 {
		call(s.lib.fn(fnReleaseMemoryInfo), s.mem)
		s.mem = 0
	}
	if s.sess != 0 {
		call(s.lib.fn(fnReleaseSession), s.sess)
		s.sess = 0
	}
	s.keep = nil
}

// --- helpers ---

// toPointer converts an address returned by C into an unsafe.Pointer without the
// uintptr-to-pointer conversion vet rejects; the memory is C's, so the GC ignores it.
func toPointer(u uintptr) unsafe.Pointer { return *(*unsafe.Pointer)(unsafe.Pointer(&u)) }

// ptrOf is the address of a slice's first element (0 for an empty slice). The caller
// keeps the slice alive across the call with runtime.KeepAlive.
func ptrOf[T any](s []T) uintptr {
	if len(s) == 0 {
		return 0
	}
	return uintptr(unsafe.Pointer(&s[0]))
}

func cString(s string) []byte { return append([]byte(s), 0) }

type cStringArray struct {
	strs [][]byte
	ptrs []uintptr
}

func cStrings(ss ...string) *cStringArray {
	a := &cStringArray{}
	for _, s := range ss {
		b := cString(s)
		a.strs = append(a.strs, b)
		a.ptrs = append(a.ptrs, ptrOf(b))
	}
	return a
}

func goString(p uintptr) string {
	if p == 0 {
		return ""
	}
	var b []byte
	for i := uintptr(0); ; i++ {
		c := *(*byte)(unsafe.Add(toPointer(p), i))
		if c == 0 {
			break
		}
		b = append(b, c)
	}
	return string(b)
}
