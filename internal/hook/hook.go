// Package hook installs hbb as a git hook. An existing hook that is not hbb's is kept:
// it is moved aside to <hook>.local and run first, so installing hbb never disables a
// check a repository already had.
package hook

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Marker identifies a hook hbb wrote.
const Marker = "# hbb-managed hook"

// Kinds are the hooks hbb can install, with the command each runs.
var Kinds = map[string]string{
	"pre-commit": "diff --staged --hook",
	"pre-push":   "diff --pre-push --hook",
}

// Script is the hook's text. exe is hbb's path at install time, used when hbb is not on
// the PATH of whatever runs git (an editor, a GUI client).
func Script(kind, exe string) string {
	args := Kinds[kind]
	stdin := ""
	chain := `[ -x "$0.local" ] && { "$0.local" "$@" || exit $?; }`
	if kind == "pre-push" {
		// git passes the pushed refs on stdin: keep them for both hooks.
		stdin = "input=$(cat)\n"
		chain = `[ -x "$0.local" ] && { printf '%s\n' "$input" | "$0.local" "$@" || exit $?; }`
	}
	run := fmt.Sprintf(`"$HBB" %s`, args)
	if kind == "pre-push" {
		run = fmt.Sprintf(`printf '%%s\n' "$input" | "$HBB" %s`, args)
	} else {
		run = "exec " + run
	}
	return fmt.Sprintf(`#!/bin/sh
%s (hbb hook install). Skip it once with HBB_SKIP=1 or git's --no-verify.
# A hook that was here before is kept as %s.local and runs first.
%s%s
[ "${HBB_SKIP:-0}" = "1" ] && exit 0
HBB=hbb
command -v hbb >/dev/null 2>&1 || HBB=%s
if ! command -v "$HBB" >/dev/null 2>&1; then
  echo "hbb: not found; skipping the security triage (install hbb or run hbb hook uninstall)" >&2
  exit 0
fi
%s
`, Marker, kind, stdin, chain, shellQuote(exe), run)
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// Install writes a hook into hooksDir. It reports what it did.
func Install(hooksDir, kind, exe string, force bool) (string, error) {
	if _, ok := Kinds[kind]; !ok {
		return "", fmt.Errorf("unknown hook %q (pre-commit, pre-push)", kind)
	}
	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		return "", err
	}
	p := filepath.Join(hooksDir, kind)
	note := "installed " + p
	if old, err := os.ReadFile(p); err == nil {
		switch {
		case bytes.Contains(old, []byte(Marker)):
			note = "updated " + p
		case force:
			if err := os.Rename(p, p+".hbb-backup"); err != nil {
				return "", err
			}
			note = fmt.Sprintf("replaced %s (the old hook is %s.hbb-backup)", p, p)
		default:
			if _, err := os.Stat(p + ".local"); err == nil {
				return "", fmt.Errorf("%s exists and so does %s.local; move one of them, or use --force", p, p)
			}
			if err := os.Rename(p, p+".local"); err != nil {
				return "", err
			}
			if err := os.Chmod(p+".local", 0o755); err != nil {
				return "", err
			}
			note = fmt.Sprintf("installed %s (the existing hook moved to %s.local and still runs first)", p, kind)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	return note, os.WriteFile(p, []byte(Script(kind, exe)), 0o755)
}

// Uninstall removes hbb's hook, restoring one it had moved aside.
func Uninstall(hooksDir, kind string) (string, error) {
	p := filepath.Join(hooksDir, kind)
	old, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return kind + ": no hook installed", nil
	}
	if err != nil {
		return "", err
	}
	if !bytes.Contains(old, []byte(Marker)) {
		return "", fmt.Errorf("%s is not hbb's hook; leaving it alone", p)
	}
	if err := os.Remove(p); err != nil {
		return "", err
	}
	if _, err := os.Stat(p + ".local"); err == nil {
		if err := os.Rename(p+".local", p); err != nil {
			return "", err
		}
		return fmt.Sprintf("removed hbb's %s hook and restored the previous one", kind), nil
	}
	return "removed " + p, nil
}
