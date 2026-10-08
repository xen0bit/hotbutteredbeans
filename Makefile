# Hot Buttered Beans (hbb)
#
#   make               a full build for this machine: model and ONNX Runtime built in
#   make slim          a small build that fetches both on first use (what `go install` gives)
#   make full GOOS=windows GOARCH=arm64   cross-compile (no cgo, so any target from anywhere)
#
# The model is chosen at build time by these variables (or the same environment
# variables); left empty, hbb.lock.json's pin is used:
#
#   HBB_MODEL_REPO      Hugging Face repository  (billytesterman/secjev-encoder)
#   HBB_MODEL_REVISION  a commit (a branch or tag is resolved and printed)
#   HBB_MODEL_VARIANT   q8 (593 MB) or fp32 (1.4 GB)
#
# e.g. make full HBB_MODEL_REVISION=8544e6faf9b7c88f2b061e0a986d0a54f40c78a5 HBB_MODEL_VARIANT=fp32

HBB_MODEL_REPO     ?=
HBB_MODEL_REVISION ?=
HBB_MODEL_VARIANT  ?=
export HBB_MODEL_REPO HBB_MODEL_REVISION HBB_MODEL_VARIANT

GO      ?= go
GOOS    ?= $(shell $(GO) env GOOS)
GOARCH  ?= $(shell $(GO) env GOARCH)
EXE     := $(if $(filter windows,$(GOOS)),.exe,)
OUT     ?= bin/hbb$(EXE)
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse HEAD 2>/dev/null)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

BI := github.com/xen0bit/hotbutteredbeans/internal/buildinfo
LDFLAGS := -s -w -X $(BI).Version=$(VERSION) -X $(BI).Commit=$(COMMIT) -X $(BI).Date=$(DATE) \
	-X '$(BI).ModelRepo=$(HBB_MODEL_REPO)' -X '$(BI).ModelRevision=$(HBB_MODEL_REVISION)' \
	-X '$(BI).ModelVariant=$(HBB_MODEL_VARIANT)'

# Tools run on this machine, whatever GOOS/GOARCH the build targets.
HOSTGO := GOOS= GOARCH= CGO_ENABLED=0 $(GO)
BUILD  := GOOS=$(GOOS) GOARCH=$(GOARCH) CGO_ENABLED=0 $(GO) build -trimpath -ldflags "$(LDFLAGS)"

.PHONY: all full slim model runtime lock test test-model vet snapshot snapshot-docker install clean help

all: full

full: model runtime ## build with the model and ONNX Runtime embedded
	$(BUILD) -tags hbb_embed -o $(OUT) ./cmd/hbb
	@ls -lh $(OUT)

slim: ## build without them (fetched into the cache on first use)
	$(BUILD) -o $(OUT) ./cmd/hbb
	@ls -lh $(OUT)

install: ## go install the slim build
	CGO_ENABLED=0 $(GO) install -trimpath -ldflags "$(LDFLAGS)" ./cmd/hbb

model: ## fetch the model (verified) into internal/embedded/model
	$(HOSTGO) run ./internal/tools/assets model

runtime: ## fetch ONNX Runtime for GOOS/GOARCH into internal/embedded/runtime
	$(HOSTGO) run ./internal/tools/assets runtime -platform $(GOOS)/$(GOARCH)

lock: ## re-pin hbb.lock.json (model files, ONNX Runtime archives, CUDA wheels)
	$(HOSTGO) run ./internal/tools/assets lock

vet:
	$(GO) vet ./...
	@test -z "$$(gofmt -l .)" || { gofmt -l .; echo "gofmt needed"; exit 1; }

test: ## unit and conformance tests (no model needed)
	CGO_ENABLED=0 $(GO) test ./...

test-model: slim ## also the tokenizer and the logits against Python (fetches the model)
	HBB_TEST_BUNDLE="$$($(OUT) model path)" HBB_TEST_ORT_LIB="$$($(OUT) runtime path)" CGO_ENABLED=0 $(GO) test -count=1 ./...

snapshot: ## every release archive, locally (goreleaser, no publishing; images: snapshot-docker)
	goreleaser release --snapshot --clean --skip=docker

snapshot-docker: ## also the images (needs arm64 emulation: docker run --privileged --rm tonistiigi/binfmt --install arm64)
	goreleaser release --snapshot --clean

clean:
	rm -rf bin dist internal/embedded/model internal/embedded/runtime

help:
	@grep -E '^[a-z-]+:.*## ' $(MAKEFILE_LIST) | sed 's/:.*## /\t/'
