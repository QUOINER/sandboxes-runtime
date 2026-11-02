package manifest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRepositoryManifest(t *testing.T) {
	root := filepath.Join("..", "..", "build")
	m, err := Load(filepath.Join(root, "manifest.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Verify(root); err != nil {
		t.Fatal(err)
	}
}

func TestNativeBuildScriptsUseManifestCommits(t *testing.T) {
	root := filepath.Join("..", "..")
	manifest, err := Load(filepath.Join(root, "build", "manifest.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for script, component := range map[string]string{
		"scripts/build-nerdbox-shim.sh": "nerdbox",
		"scripts/build-gvproxy.sh":      "gvisor-tap-vsock",
	} {
		commit := manifest.Components[component].Commit
		if commit == "" {
			t.Fatalf("manifest component %q has no commit", component)
		}
		data, err := os.ReadFile(filepath.Join(root, script))
		if err != nil {
			t.Fatal(err)
		}
		scriptText := string(data)
		if got := strings.Count(scriptText, "verify-pinned-source.sh"); got != 2 {
			t.Errorf("%s runs source verification %d times, want before and after copy", script, got)
		}
		if !strings.Contains(scriptText, commit) {
			t.Errorf("%s does not verify manifest commit %s", script, commit)
		}
	}
}

func FuzzParseManifest(f *testing.F) {
	f.Add([]byte("version: 1\nplatforms: [linux/arm64]\n"))
	f.Add([]byte("components: ["))
	f.Add([]byte(""))
	f.Fuzz(func(t *testing.T, data []byte) {
		_, _ = parseManifest(data)
	})
}

func TestVerifyRejectsMissingPatch(t *testing.T) {
	root := t.TempDir()
	m := Manifest{
		Version: 1,
		Components: map[string]Component{
			"containerd":       {Version: "1", Source: "x"},
			"nerdbox":          {Version: "1", Source: "x"},
			"libkrun":          {Version: "1", Source: "x"},
			"gvisor-tap-vsock": {Version: "1", Source: "x"},
		},
		Patches: map[string]Patch{
			"missing": {File: "missing.patch", SHA256: "00"},
		},
	}
	if err := m.Verify(root); err == nil {
		t.Fatal("expected missing patch error")
	}
}

func TestVerifyRejectsUnpinnedImage(t *testing.T) {
	root := t.TempDir()
	patch := filepath.Join(root, "p.patch")
	if err := os.WriteFile(patch, []byte("ok"), 0o600); err != nil {
		t.Fatal(err)
	}
	m := Manifest{
		Version: 1,
		Components: map[string]Component{
			"containerd":       {Version: "1", Source: "x"},
			"nerdbox":          {Version: "1", Source: "x"},
			"libkrun":          {Version: "1", Source: "x"},
			"gvisor-tap-vsock": {Version: "1", Source: "x"},
		},
		Patches: map[string]Patch{
			"p": {File: "p.patch", SHA256: "2689367b205c16ce32ed4200942b8b8b1e262dfc70d9bc9fbc77c49699a4f1df"},
		},
		Images: map[string]Image{
			"default": {Ref: "x", Digest: "latest"},
		},
	}
	if err := m.Verify(root); err == nil {
		t.Fatal("expected image pin error")
	}
}
