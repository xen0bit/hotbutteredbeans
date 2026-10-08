package hook

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstallChainsAndRestores(t *testing.T) {
	dir := t.TempDir()
	orig := "#!/bin/sh\necho mine\n"
	os.WriteFile(filepath.Join(dir, "pre-commit"), []byte(orig), 0o755)
	if _, err := Install(dir, "pre-commit", "/opt/hbb", false); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "pre-commit.local")); string(b) != orig {
		t.Fatal("the existing hook was not kept")
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "pre-commit")); !strings.Contains(string(b), Marker) {
		t.Fatal("hbb's hook not written")
	}
	if _, err := Install(dir, "pre-commit", "/opt/hbb", false); err != nil { // idempotent
		t.Fatal(err)
	}
	if _, err := Uninstall(dir, "pre-commit"); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "pre-commit")); string(b) != orig {
		t.Fatal("the original hook was not restored")
	}
	if _, err := Uninstall(dir, "pre-commit"); err == nil {
		t.Fatal("uninstall removed a hook that is not hbb's")
	}
}

func TestScriptsParse(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	for kind := range Kinds {
		p := filepath.Join(t.TempDir(), kind)
		os.WriteFile(p, []byte(Script(kind, "/path/with 'quote'/hbb")), 0o755)
		if out, err := exec.Command(sh, "-n", p).CombinedOutput(); err != nil {
			t.Errorf("%s: %v\n%s", kind, err, out)
		}
	}
}
