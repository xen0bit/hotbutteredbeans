# hbb: [Hot Buttered Beans](https://en.wikipedia.org/wiki/Hunt_the_thimble)

hbb ranks source code by how likely it is to hold one of the
[CWE Top 25](https://cwe.mitre.org/top25/) weaknesses, with a small encoder model that runs
locally: [secjev-encoder](https://huggingface.co/billytesterman/secjev-encoder), LFM2.5-350M
fine-tuned on code before and after real CVE and GHSA fixes. One forward pass reads a
window of up to 240 lines and answers 25 questions such as *"Is a SQL query built here by
concatenating or interpolating data, rather than by binding parameters?"*

It is a **reading order, not a verdict**: it tells you (or an LLM, or a reviewer) where to
look first. Its strongest signal is a change that raises a score: it was trained to score
code before a security fix above the same code after it.

```console
$ hbb diff main...feature --compare --top 4 --content-lines 5
app.py:1-16  changed 15
  0.694 +0.12 CWE-476  NULL pointer dereference
  0.879 +0.05 CWE-502  Unsafe deserialization
  0.970 +0.04 CWE-78   OS command injection
  0.978 +0.03 CWE-434  Unrestricted file upload
  CWE-476: Is a pointer or reference dereferenced here right after an operation that can return null (an allocation, lookup, parse or optional result) without a check for null?
     12 | def user():
     13 |     name = request.args.get("name", "")
     14 |     cur = db().cursor()
    >15 |     cur.execute("SELECT id, email FROM users WHERE name = '" + name + "'")
     16 |     return {"rows": cur.fetchall()}
      ... 11 more lines in the window

hbb: 16 more findings at p >= 0.50 (--top -1 lists all)
```

(A real run on a 16-line Flask handler, where a bound parameter became string
concatenation. Short web handlers score high on most questions both before and after
a change: here CWE-89 went from 0.98 to 0.99. That is why the window, not the label, is
the unit to read.)

- [Install](#install) · [Use](#use) · [GPU and CPU](#gpu-and-cpu) · [Settings](#settings) · [Output](#output)
- [Pipelines](#pipelines): [git hooks](#git-hooks) · [CI and LLM review](#ci-and-llm-review) · [Docker](#docker)
- [Building](#building) · [Where things live](#where-things-live) · [How it works](#how-it-works) · [Licenses](#licenses)

## Install

Each release has two archives per platform (linux amd64/arm64, macOS arm64, Windows
amd64/arm64), from the same code:

| archive | size | model and ONNX Runtime |
|---|---|---|
| `hbb-full_<version>_<os>_<arch>` | ~610 MB | built in: works offline, nothing to fetch |
| `hbb_<version>_<os>_<arch>` | ~11 MB | fetched once into the cache on first use, pinned by sha256 |

```sh
# with Go 1.26+: the slim build
go install github.com/xen0bit/hotbutteredbeans/cmd/hbb@latest

# the container (full build, with git)
docker run --rm -v "$PWD:/src" ghcr.io/xen0bit/hbb diff --staged

# check what it will use
hbb doctor
```

Nothing needs cgo or a C toolchain: hbb loads ONNX Runtime at run time.

## Use

```sh
hbb scan                              # rank every window of the repository (or folder)
hbb scan src/api --cwe CWE-89,CWE-78  # some paths, some questions
hbb diff                              # what changed in the work tree (incl. untracked files)
hbb diff --staged                     # what the next commit holds
hbb diff main...HEAD --compare        # a branch, with each score's rise against main
hbb diff origin/main...HEAD -- src/   # limited to paths
cat x.go | hbb scan --stdin --name pkg/x.go
```

**How a diff is cut.** hbb scores the model's own windows (every 240 lines from line 1,
halved when a window is too long), never windows re-centred on a change, because the model
has only seen that shape. `diff` scores the windows that contain changed lines, so a
one-line change costs one window. `--compare` also scores the base version's windows that
the same hunks touched and reports the rise (`+0.42`), and ranks by it.

**Speed.** A window of ~2,000 tokens takes ~0.05 s on a recent NVIDIA GPU and ~1.3 s on a
32-thread desktop CPU (longer with fewer cores).
Scores are cached by window content, so re-running after a small edit only scores the
windows that changed. A whole repository on a CPU takes minutes; `diff` is the everyday path.

**Exit codes.** `0` done (findings or not), `1` a finding reached `fail_at` / `fail_delta`,
`2` an error. hbb never fails on findings unless you set a threshold.

| command | |
|---|---|
| `hbb scan [path...]` | rank whole files |
| `hbb diff [range] [-- path...]` | rank changed windows: `--staged`, `A`, `A..B`, `A...B`, `--pre-push` |
| `hbb hook install\|uninstall` | git hooks (below) |
| `hbb doctor` | the model, runtime and device it will use, why, and a test window |
| `hbb model fetch\|info\|verify\|path\|prune` | the model: fetch it, list its 25 questions, re-hash it |
| `hbb runtime fetch [--gpu]\|path` | ONNX Runtime, and the GPU stack |
| `hbb config init\|show` | write a commented `.hbb.yaml`; print the settings in force |
| `hbb version`, `hbb completion` | |

## GPU and CPU

`--device auto` (the default) runs on an NVIDIA GPU when one is usable, and on the CPU
otherwise, with a one-line reason. "Usable" means, on linux/amd64 and windows/amd64:

1. an NVIDIA driver new enough for CUDA 13 (580 or newer);
2. the CUDA 13 and cuDNN 9 libraries, found in `$HBB_CUDA_PATH`, hbb's cache (below), the
   CUDA toolkit (`$CUDA_PATH`, `/usr/local/cuda`), the `nvidia-*` pip wheels of the active
   virtualenv or conda environment, or the system's library folders;
3. the GPU build of ONNX Runtime, which hbb downloads (225 MB, pinned) the first time 1
   and 2 hold. Git hooks never download it; they use the CPU until it is there.

The libraries are not installed with most drivers. `hbb runtime fetch --gpu` downloads
NVIDIA's redistributable CUDA runtime, cuBLAS, cuRAND and cuDNN (~1.1 GB, from their PyPI
wheels, pinned by sha256) together with the GPU build of ONNX Runtime into the cache.

Force a device with any of:

```sh
hbb scan --device cpu        # flag
HBB_DEVICE=cpu hbb scan      # environment
echo 'device: cpu' >> .hbb.yaml
```

`gpu` fails instead of falling back. `coreml` uses Apple's Core ML on macOS (it is opt-in:
the 8-bit graph's operators mostly run on the CPU there anyway). `hbb doctor` shows each
decision.

## Settings

Defaults, then `.hbb.yaml` (in the repository root or a parent folder of where hbb runs,
or `$HBB_CONFIG`), then environment variables, then flags. `hbb config init` writes a
commented file; `hbb config show` prints what is in force. See
[examples/hbb.yaml](examples/hbb.yaml).

| setting | flag | environment | default |
|---|---|---|---|
| `device` | `--device` | `HBB_DEVICE` | `auto` (`gpu`, `cpu`, `coreml`) |
| `threads` | `--threads` | `HBB_THREADS` | ONNX Runtime's (physical cores) |
| `model.repo` / `.revision` / `.variant` | `--model-repo` ... | `HBB_MODEL_REPO`, `HBB_MODEL_REVISION`, `HBB_MODEL_VARIANT` | the build's pin; `q8` |
| `model.dir` | `--model-dir` | `HBB_MODEL_DIR` | (a local bundle folder: nothing is fetched) |
| `ort_lib` | `--ort-lib` | `HBB_ORT_LIB`, `ONNXRUNTIME_LIB` | resolved (below) |
| `offline` | `--offline` | `HBB_OFFLINE` | `false` |
| `exclude`, `include` | `--exclude`, `--include` | | `.gitignore`-style globs |
| `default_excludes` | `--no-default-excludes` | | `vendor/`, `third_party/`, minified JS |
| `gitignore` | `--no-gitignore` | | `true` in a repository |
| `skip_generated` | `--include-generated` | | `true` ("Code generated ... DO NOT EDIT", `@generated`) |
| `max_file_size` | `--max-file-size` | | `1MiB` |
| `cwe.only`, `cwe.skip` | `--cwe`, `--skip-cwe` | | all 25 |
| `scan.top`, `scan.min_p` | `--top`, `--min-p` | | 30, 0 |
| `diff.top`, `diff.min_p`, `diff.compare` | `--top`, `--min-p`, `--compare` | | 20, 0.5, false |
| `fail_at`, `fail_delta` | `--fail-at`, `--fail-delta` | `HBB_FAIL_AT`, `HBB_FAIL_DELTA` | 0 (never fail) |
| `format` | `-f` | `HBB_FORMAT` | `text` |
| `content_lines` | `--content-lines` | | 10 (0 none, -1 all) |
| `baseline` | `--baseline` | `HBB_BASELINE` | (none) |
| `hook.fetch`, `hook.compare` | | `HBB_HOOK_FETCH` | false |
| | `--cache-dir` | `HBB_CACHE_DIR` | the user cache folder + `/hbb` |

The questions depend on the language: the memory-safety ones (CWE-119, 125, 416, 787) are
asked of C and C++ only, CWE-190 not of memory-safe languages. `hbb model info` lists them.

**Baselines.** `hbb scan --write-baseline .hbb-baseline.json` records today's findings;
with `baseline: .hbb-baseline.json` they are left out from then on. A finding is
identified by its path, question and the window's content, so it comes back as soon as
that code changes.

## Output

`-f/--format` picks one; `-o` writes it to a file; `--also FORMAT=PATH` (repeatable) writes
more from the same run.

| format | for |
|---|---|
| `text` | a terminal: windows ranked by their best finding, changed lines marked `>` |
| `json`, `jsonl` | tools: the whole report, or one finding per line |
| `sarif` | GitHub code scanning, GitLab, IDEs: SARIF 2.1.0, one rule per CWE |
| `github` | GitHub Actions annotations (`::warning file=...`) on the changed lines |
| `markdown` | a pull request comment or a job summary |
| `llm` | context for an LLM: what the scores mean, then each flagged window once with its findings, numbered code and changed lines; `--max-chars` caps the code |

`--windows-out FILE` also writes every window's scores (JSON lines), as secjev's
`scan.py --jsonl` did.

## Pipelines

### Git hooks

```sh
hbb hook install              # pre-commit: ranks the staged changes
hbb hook install pre-push     # pre-push: ranks the commits being pushed
hbb hook uninstall
```

A hook reports to the terminal and lets the commit through. Set `fail_at` or `fail_delta`
in `.hbb.yaml` to block instead. A hook never blocks on its own failure (no model yet, a
broken runtime): it says so and lets the commit through. It never downloads either, unless
`hook.fetch: true`; `hook install` fetches the model up front. An existing hook is kept as
`<hook>.local` and runs first. Skip once with `HBB_SKIP=1 git commit` or `--no-verify`.

With the [pre-commit](https://pre-commit.com) framework, see
[examples/pre-commit-config.yaml](examples/pre-commit-config.yaml) (`id: hbb` builds it
from source, `id: hbb-system` uses the one on PATH).

### CI and LLM review

`hbb diff origin/main...HEAD --compare` ranks what a pull request changed. One run can
write every report:

```sh
hbb diff "origin/$BASE...HEAD" --compare \
  --format llm --max-chars 60000 -o hbb-context.xml \
  --also sarif=hbb.sarif --also markdown=summary.md --also github=/dev/stdout
```

The `llm` report is built to be handed to a model: it explains what the scores mean and
asks for a judgement backed by line numbers, groups findings by window so code appears
once, marks changed lines, and gives each finding's score before and after the change. A
workflow that feeds it to Claude for an inline review, and uploads SARIF:
[examples/github-llm-review.yml](examples/github-llm-review.yml). In CI, check out with
`fetch-depth: 0` so the merge base exists, and cache `~/.cache/hbb` (the model and the
scores) between runs.

### Driving hbb from another program

`hbb serve --stdio` loads the model once and scores the files a parent names, one JSON
object per line in and out. It prints a `{"ready":true,...}` line first (model, device,
window rules, every question with its language limits), then answers each
`{"id":1,"path":"src/a.c","file":"/abs/src/a.c"}` (or `"text":"..."`) with that file's windows
and their per-question probabilities, in order. A file with no language is a `skip`, a
failure an `error`, and the process carries on. pwrq's `invoke_hbb` uses it.

### Docker

`ghcr.io/xen0bit/hbb` (linux amd64 and arm64) is the full build on Debian slim with git:
nothing is downloaded at run time, and any user id can run it.

```sh
docker run --rm -v "$PWD:/src" ghcr.io/xen0bit/hbb scan --top 20
docker run --rm -v "$PWD:/src" ghcr.io/xen0bit/hbb diff origin/main...HEAD --compare -f sarif > hbb.sarif
```

For GitLab, see [examples/gitlab-ci.yml](examples/gitlab-ci.yml).

## Building

```sh
make            # full build for this machine: fetches the model and ONNX Runtime, embeds them -> bin/hbb
make slim       # without them (what go install gives)
make full GOOS=windows GOARCH=arm64     # any target from anywhere (no cgo)
make test       # unit and conformance tests, no model
make test-model # also the tokenizer and logits against Python's (fetches the model)
make snapshot   # every release artifact, locally (goreleaser)
make lock       # re-pin hbb.lock.json
```

**Choosing the model at build time.** `HBB_MODEL_REPO`, `HBB_MODEL_REVISION` and
`HBB_MODEL_VARIANT`, as make variables or in the environment, set the model a build
embeds (full) or fetches (slim). They default to [hbb.lock.json](hbb.lock.json)'s pin.

```sh
make full HBB_MODEL_VARIANT=fp32                          # the 1.4 GB fp32 graph
make full HBB_MODEL_REPO=you/your-encoder HBB_MODEL_REVISION=<commit>
```

Pin a commit: a branch is resolved and the commit printed. For the pinned model, every
file is checked against the sha256 in `hbb.lock.json`; for another, against the Hub's
own hashes. The release workflow reads the same variables from repository variables.

A model is any export of secjev's `encoder/export.py` (a bundle folder with `bundle.json`,
`tokenizer.json` and the graphs); `--model-dir` runs one from disk without fetching.

## Where things live

hbb finds each piece in the first place that has it:

| | order |
|---|---|
| model | `--model-dir` · built in (full builds) · the cache · Hugging Face |
| ONNX Runtime | `--ort-lib` · the GPU build in the cache (GPU) · built in · the cache · next to `hbb` · GitHub release · the system's library folders |
| CUDA libraries | `$HBB_CUDA_PATH` · the cache · CUDA toolkit · active virtualenv/conda · system |

The cache (`hbb doctor` prints it; `$HBB_CACHE_DIR` moves it):

```
~/.cache/hbb/                 (~/Library/Caches/hbb, %LocalAppData%\hbb)
  models/<repo>/<commit>/     bundle.json, tokenizer.json, the graph and weights, manifest.json
  runtime/onnxruntime-<version>-<os>-<arch>-{cpu,cuda,embedded}/
  cuda/<os>-<arch>/           NVIDIA libraries (hbb runtime fetch --gpu)
  scores/<model>-<device>.bin cached scores, by window content
```

Every download is verified by sha256 before use, resumes after an interruption, and is
safe to run from several processes at once. For an air-gapped machine, use a full build,
or copy a cache folder and set `--offline`. `hbb model prune` frees old models.

## How it works

1. **Files.** In a repository, the files git tracks plus untracked ones `.gitignore`
   allows; the language comes from the extension (28 languages), as in training.
2. **Windows.** Each file is cut into the training windows: a header
   (`// ==== path:from-to of total ====`), numbered lines cut at 400 characters, 240
   lines per tile, halved until under 33,084 characters. Invalid UTF-8 decodes as Python
   does.
3. **Tokens.** A pure-Go port of the model's byte-level BPE, id for id with Hugging Face
   `tokenizers` (testdata/conformance).
4. **Model.** ONNX Runtime, loaded at run time without cgo (`internal/ort`), runs the
   8-bit graph on one window at a time; `sigmoid(logit / temperature)` gives a calibrated
   P(yes) for each question asked of the language.
5. **Ranking.** (window, question) pairs by p, or by the rise against the base.

Windows, token ids and scores match secjev's Python reference exactly: CI checks the
conformance fixtures on Linux, macOS and Windows, and `hbb scan` agrees with secjev's Go
scanner to 0.0 in probability.

**What to expect.** Read [the model card](https://huggingface.co/billytesterman/secjev-encoder).
On its validation split, code just before a CVE fix ranks above ordinary code with an
AUROC of 0.94, and scores above its own fixed version 66% of the time; about 0.75% of
scores on ordinary code reach 0.5. On repositories it never saw, secjev measured a lower
AUROC for its round-2 model (0.78), so expect the ranking to be weaker on unfamiliar code. Short web handlers score high on
many questions at once: compare windows on one question, and use `--compare` on changes.

## Licenses

The model is a derivative of LFM2.5-Encoder-350M under the
[LFM Open License v1.0](licenses/LFM-Open-License-1.0.txt): free to use and redistribute,
but commercial use is licensed only to organisations under $10M annual revenue. Full
builds and the container include it, and ONNX Runtime ([MIT](licenses/onnxruntime-LICENSE.txt),
[third-party notices](licenses/onnxruntime-ThirdPartyNotices.txt)); `hbb runtime fetch
--gpu` downloads NVIDIA's CUDA libraries under NVIDIA's license.
