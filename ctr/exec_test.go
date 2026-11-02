package ctr

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	runtime "github.com/QUOINER/sandboxes-runtime"
)

func TestExecPassesArgumentsWithoutShell(t *testing.T) {
	dir := t.TempDir()
	program := filepath.Join(dir, "fake-ctr")
	if err := os.WriteFile(program, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\"\nprintf 'synthetic stderr' >&2\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	backend, err := NewBackend(Config{Socket: "/tmp/test.sock", CTR: program}, BackendOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code, err := backend.Exec(context.Background(), runtime.Handle{ID: "klorq-example", FIFODir: "/tmp/fifo"}, runtime.ExecRequest{
		Argv: []string{"printf", "$(touch /tmp/never-created-by-klorq)"}, Workdir: "/workspace", Stdout: &stdout, Stderr: &stderr,
	})
	if err != nil || code != 0 {
		t.Fatalf("exec = %d, %v", code, err)
	}
	for _, want := range []string{"tasks\nexec\n", "--fifo-dir\n/tmp/fifo\n", "--cwd\n/workspace\n", "klorq-example\nprintf\n$(touch /tmp/never-created-by-klorq)\n"} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("missing %q in %q", want, stdout.String())
		}
	}
	if stderr.String() != "synthetic stderr" {
		t.Fatalf("unexpected stderr: %q", stderr.String())
	}
}

func TestExecRejectsInvalidInput(t *testing.T) {
	backend, err := NewBackend(Config{Socket: "/tmp/test.sock", CTR: "/does/not/exist"}, BackendOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		handle runtime.Handle
		req    runtime.ExecRequest
	}{
		{runtime.Handle{ID: "-other"}, runtime.ExecRequest{Argv: []string{"true"}}},
		{runtime.Handle{ID: "klorq-x"}, runtime.ExecRequest{}},
		{runtime.Handle{ID: "klorq-x"}, runtime.ExecRequest{Argv: []string{"true"}, Workdir: "relative"}},
		{runtime.Handle{ID: "klorq-x"}, runtime.ExecRequest{Argv: []string{"true", "bad\x00arg"}}},
		{runtime.Handle{ID: "klorq-x"}, runtime.ExecRequest{Argv: []string{"true"}, Env: []string{"A=1"}}},
		{runtime.Handle{ID: "klorq-x"}, runtime.ExecRequest{Argv: []string{"true"}, User: "0:0"}},
		{runtime.Handle{ID: "klorq-x"}, runtime.ExecRequest{Argv: []string{"true"}, User: "root"}},
	} {
		if _, err := backend.Exec(context.Background(), tc.handle, tc.req); err == nil || strings.Contains(err.Error(), "/does/not/exist") {
			t.Fatalf("invalid request was not rejected before starting ctr: %v", err)
		}
	}
}

func TestExecReportsExitAndCancellation(t *testing.T) {
	dir := t.TempDir()
	program := filepath.Join(dir, "fake-ctr")
	if err := os.WriteFile(program, []byte("#!/bin/sh\nexit 7\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	backend, err := NewBackend(Config{Socket: "/tmp/test.sock", CTR: program}, BackendOptions{})
	if err != nil {
		t.Fatal(err)
	}
	handle, req := runtime.Handle{ID: "klorq-example"}, runtime.ExecRequest{Argv: []string{"false"}}
	code, err := backend.Exec(context.Background(), handle, req)
	if code != 7 || err == nil {
		t.Fatalf("ambiguous ctr exit = %d, %v", code, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := backend.Exec(ctx, handle, req); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled exec = %v", err)
	}
}

func TestExecCancellationTargetsGuestProcess(t *testing.T) {
	for _, failKill := range []bool{false, true} {
		t.Run(fmt.Sprint(failKill), func(t *testing.T) {
			dir := t.TempDir()
			program := filepath.Join(dir, "fake-ctr")
			ready, killLog := filepath.Join(dir, "ready"), filepath.Join(dir, "kill")
			t.Setenv("KLORQ_TEST_READY", ready)
			t.Setenv("KLORQ_TEST_KILL_LOG", killLog)
			t.Setenv("KLORQ_TEST_KILL_FAIL", fmt.Sprint(failKill))
			script := "#!/bin/sh\ncase \"$*\" in\n*'tasks kill'*) printf '%s' \"$*\" > \"$KLORQ_TEST_KILL_LOG\"; [ \"$KLORQ_TEST_KILL_FAIL\" = false ]; exit $? ;;\n*) touch \"$KLORQ_TEST_READY\"; exec sleep 30 ;;\nesac\n"
			if err := os.WriteFile(program, []byte(script), 0o700); err != nil {
				t.Fatal(err)
			}
			backend, err := NewBackend(Config{Socket: "/tmp/test.sock", CTR: program}, BackendOptions{})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				_, err := backend.Exec(ctx, runtime.Handle{ID: "klorq-owned"}, runtime.ExecRequest{Argv: []string{"sleep"}})
				done <- err
			}()
			deadline := time.Now().Add(5 * time.Second)
			for {
				if _, err := os.Stat(ready); err == nil {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("exec process did not start")
				}
				time.Sleep(10 * time.Millisecond)
			}
			cancel()
			err = <-done
			if !errors.Is(err, context.Canceled) || (strings.Contains(err.Error(), "terminate guest exec") != failKill) {
				t.Fatalf("unexpected cancellation result: %v", err)
			}
			data, err := os.ReadFile(killLog)
			if err != nil || !strings.Contains(string(data), "tasks kill --signal SIGKILL --exec-id klorq-exec-") || !strings.Contains(string(data), "klorq-owned") {
				t.Fatalf("guest kill not directed to exec: %q, %v", data, err)
			}
		})
	}
}

func TestIsNumericUser(t *testing.T) {
	for _, tc := range []struct {
		user string
		want bool
	}{
		{"0", true}, {"1000:1001", true}, {"root", false}, {"root:0", false}, {"root:wheel", false}, {"", false},
	} {
		if got := isNumericUser(tc.user); got != tc.want {
			t.Fatalf("isNumericUser(%q)=%v want %v", tc.user, got, tc.want)
		}
	}
}

func TestClassifyStartError(t *testing.T) {
	for _, tc := range []struct {
		message string
		code    int
		ok      bool
	}{
		{"OCI runtime exec failed: executable file `missing` not found in $PATH: No such file or directory", 127, true},
		{"OCI runtime exec failed: the path `/etc/passwd` exists but it is not executable: Operation not permitted", 127, true},
		{"synthetic unrelated start failure", 0, false},
	} {
		code, _, ok := classifyStartError(errors.New(tc.message))
		if code != tc.code || ok != tc.ok {
			t.Fatalf("classifyStartError(%q)=%d,%v want %d,%v", tc.message, code, ok, tc.code, tc.ok)
		}
	}
}

func TestStartErrorCodeWritesToStdoutContract(t *testing.T) {
	var output bytes.Buffer
	code, handled := startErrorCode(&output, errors.New("OCI runtime exec failed: executable file `missing` not found in $PATH"))
	if !handled || code != 127 || !strings.Contains(output.String(), "not found in $PATH") {
		t.Fatalf("code=%d handled=%v output=%q", code, handled, output.String())
	}
}
