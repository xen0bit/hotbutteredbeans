package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoad(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "a", "b")
	os.MkdirAll(sub, 0o755)
	os.WriteFile(filepath.Join(root, ".hbb.yaml"), []byte("device: cpu\nmax_file_size: 2MiB\nfail_at: 0.9\ncwe: {only: [CWE-89]}\n"), 0o644)
	t.Setenv("HBB_CONFIG", "")
	t.Setenv("HBB_THREADS", "3")
	c, err := Load(sub, root)
	if err != nil {
		t.Fatal(err)
	}
	if c.Device != "cpu" || c.MaxFileSize != 2<<20 || c.FailAt != 0.9 || c.Threads != 3 || len(c.CWE.Only) != 1 {
		t.Errorf("%+v", c)
	}
	t.Setenv("HBB_DEVICE", "gpu")
	if c, _ = Load(sub, root); c.Device != "gpu" {
		t.Errorf("the environment overrides the file: %q", c.Device)
	}
	os.WriteFile(filepath.Join(root, ".hbb.yaml"), []byte("devise: cpu\n"), 0o644)
	if _, err := Load(sub, root); err == nil {
		t.Error("an unknown key should be an error")
	}
}

func TestTemplateIsValid(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, ".hbb.yaml"), []byte(Template), 0o644)
	t.Setenv("HBB_CONFIG", "")
	if _, err := Load(dir, dir); err != nil {
		t.Fatal(err)
	}
}
