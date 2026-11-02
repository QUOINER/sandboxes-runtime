package manifest

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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
		for _, flag := range []string{"-trimpath", "-buildvcs=false", "-ldflags=-buildid="} {
			if !strings.Contains(scriptText, flag) {
				t.Errorf("%s is missing reproducible Go build flag %q", script, flag)
			}
		}
		if !strings.Contains(scriptText, commit) {
			t.Errorf("%s does not verify manifest commit %s", script, commit)
		}
	}
}

func TestPinnedSourceRejectsIgnoredFiles(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("native source preflight is a POSIX shell script")
	}
	root := t.TempDir()
	runGit := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, output)
		}
		return strings.TrimSpace(string(output))
	}
	runGit("init", "-q")
	runGit("config", "user.name", "runtime-test")
	runGit("config", "user.email", "runtime-test@example.invalid")
	if err := os.WriteFile(filepath.Join(root, ".gitignore"), []byte("generated.go\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "tracked.txt"), []byte("pinned source\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit("add", ".gitignore", "tracked.txt")
	runGit("commit", "-m", "test pinned source")
	commit := runGit("rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(root, "generated.go"), []byte("package injected\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join("..", "..", "scripts", "verify-pinned-source.sh")
	cmd := exec.Command(script, root, commit)
	if output, err := cmd.CombinedOutput(); err == nil {
		t.Fatalf("preflight accepted an ignored source file: %s", output)
	}
}

func TestPinnedSourceAcceptsCleanCheckout(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("native source preflight is a POSIX shell script")
	}
	root, commit := initPinnedSourceRepo(t)
	if output := runPinnedSourceVerifier(t, root, commit, nil); output != nil {
		t.Fatalf("preflight rejected a clean pinned checkout: %s", output)
	}
}

func TestPinnedSourceRejectsGitIndexFlags(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("native source preflight is a POSIX shell script")
	}
	for _, flag := range []string{"--skip-worktree", "--assume-unchanged"} {
		t.Run(flag, func(t *testing.T) {
			root, commit := initPinnedSourceRepo(t)
			gitTestCommand(t, root, "update-index", flag, "tracked.go")
			if err := os.WriteFile(filepath.Join(root, "tracked.go"), []byte("package main\nvar changed = true\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if output := runPinnedSourceVerifier(t, root, commit, nil); output == nil {
				t.Fatal("preflight accepted modified source hidden by an index flag")
			}
		})
	}
}

func TestPinnedSourceBindsGitWorktreeToCopiedDirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("native source preflight is a POSIX shell script")
	}
	root, commit := initPinnedSourceRepo(t)
	checkout := filepath.Join(t.TempDir(), "checkout")
	gitTestCommand(t, filepath.Dir(root), "clone", "-q", root, checkout)
	gitTestCommand(t, checkout, "config", "core.worktree", root)
	if err := os.WriteFile(filepath.Join(checkout, "injected.go"), []byte("package main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if output := runPinnedSourceVerifier(t, checkout, commit, nil); output == nil {
		t.Fatal("preflight followed core.worktree instead of checking the copied checkout")
	}
}

func TestPinnedSourceIgnoresInheritedGitWorktree(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("native source preflight is a POSIX shell script")
	}
	root, commit := initPinnedSourceRepo(t)
	other := filepath.Join(t.TempDir(), "other")
	gitTestCommand(t, filepath.Dir(root), "clone", "-q", root, other)
	if err := os.WriteFile(filepath.Join(root, "injected.go"), []byte("package main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env := []string{"GIT_DIR=" + filepath.Join(other, ".git"), "GIT_WORK_TREE=" + other}
	if output := runPinnedSourceVerifier(t, root, commit, env); output == nil {
		t.Fatal("preflight followed inherited Git environment instead of checking the source directory")
	}
}

func TestPinnedSourceRejectsConfiguredGitFilters(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("native source preflight is a POSIX shell script")
	}
	root, commit := initPinnedSourceRepo(t)
	gitTestCommand(t, root, "config", "filter.synthetic.clean", "cat")
	if output := runPinnedSourceVerifier(t, root, commit, nil); output == nil {
		t.Fatal("preflight accepted a checkout with an unpinned Git filter")
	}
}

func initPinnedSourceRepo(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
	gitTestCommand(t, root, "init", "-q")
	gitTestCommand(t, root, "config", "user.name", "runtime-test")
	gitTestCommand(t, root, "config", "user.email", "runtime-test@example.invalid")
	if err := os.WriteFile(filepath.Join(root, "tracked.go"), []byte("package main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTestCommand(t, root, "add", "tracked.go")
	gitTestCommand(t, root, "commit", "-m", "test pinned source")
	return root, gitTestCommand(t, root, "rev-parse", "HEAD")
}

func gitTestCommand(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, output)
	}
	return strings.TrimSpace(string(output))
}

func runPinnedSourceVerifier(t *testing.T, root, commit string, gitEnv []string) []byte {
	t.Helper()
	script := filepath.Join("..", "..", "scripts", "verify-pinned-source.sh")
	cmd := exec.Command(script, root, commit)
	if gitEnv != nil {
		for _, entry := range os.Environ() {
			if !strings.HasPrefix(entry, "GIT_") {
				cmd.Env = append(cmd.Env, entry)
			}
		}
		cmd.Env = append(cmd.Env, gitEnv...)
	}
	output, err := cmd.CombinedOutput()
	if err == nil {
		return nil
	}
	return output
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
