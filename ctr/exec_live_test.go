package ctr

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	runtime "github.com/quoiner/sandboxes-runtime"
)

// TestLiveExec requires an explicitly supplied, already running, owned VM.
// It neither creates nor removes containerd resources.
func TestLiveExec(t *testing.T) {
	socket := os.Getenv("KLORQ_TEST_CTR_SOCKET")
	id := os.Getenv("KLORQ_TEST_OWNED_CONTAINER")
	fifo := os.Getenv("KLORQ_TEST_FIFO_DIR")
	if socket == "" || id == "" || fifo == "" {
		t.Skip("set KLORQ_TEST_CTR_SOCKET, KLORQ_TEST_OWNED_CONTAINER and KLORQ_TEST_FIFO_DIR for an owned VM")
	}
	backend, err := NewBackend(Config{Socket: socket}, BackendOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var stdout, stderr bytes.Buffer
	code, err := backend.Exec(ctx, runtime.Handle{ID: id, FIFODir: fifo}, runtime.ExecRequest{
		Argv:    []string{"/bin/sh", "-c", "printf live-stdout; printf live-stderr >&2"},
		Workdir: "/", Stdout: &stdout, Stderr: &stderr,
	})
	if err != nil || code != 0 || stdout.String() != "live-stdout" || stderr.String() != "live-stderr" {
		t.Fatalf("guest command: code=%d err=%v stdout=%q stderr=%q", code, err, stdout.String(), stderr.String())
	}
	code, err = backend.Exec(ctx, runtime.Handle{ID: id, FIFODir: fifo}, runtime.ExecRequest{
		Argv: []string{"/bin/sh", "-c", "exit 7"}, Workdir: "/",
	})
	if code != 7 || err == nil {
		t.Fatalf("guest nonzero exit must remain ambiguous: code=%d err=%v", code, err)
	}
}

func TestLiveNativeExecExitStatus(t *testing.T) {
	socket := os.Getenv("KLORQ_TEST_CTR_SOCKET")
	id := os.Getenv("KLORQ_TEST_OWNED_CONTAINER")
	fifo := os.Getenv("KLORQ_TEST_FIFO_DIR")
	if socket == "" || id == "" || fifo == "" {
		t.Skip("set KLORQ_TEST_CTR_SOCKET, KLORQ_TEST_OWNED_CONTAINER and KLORQ_TEST_FIFO_DIR for an owned VM")
	}
	backend, err := NewBackend(Config{Socket: socket}, BackendOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var stdout, stderr bytes.Buffer
	code, err := backend.ExecNative(ctx, runtime.Handle{ID: id, FIFODir: fifo}, runtime.ExecRequest{
		Argv:    []string{"/bin/sh", "-c", "printf live-out; printf live-err >&2; exit 7"},
		Workdir: "/", Stdout: &stdout, Stderr: &stderr,
	})
	if err != nil || code != 7 || stdout.String() != "live-out" || stderr.String() != "live-err" {
		t.Fatalf("guest command: code=%d err=%v stdout=%q stderr=%q", code, err, stdout.String(), stderr.String())
	}
}

type liveReadyWriter struct {
	once  sync.Once
	ready chan struct{}
}

func (w *liveReadyWriter) Write(p []byte) (int, error) {
	w.once.Do(func() { close(w.ready) })
	return len(p), nil
}

func TestLiveExecCancellationStopsGuest(t *testing.T) {
	liveExecCancellationStopsGuest(t, false)
}

func TestLiveNativeExecCancellationStopsGuest(t *testing.T) {
	liveExecCancellationStopsGuest(t, true)
}

func liveExecCancellationStopsGuest(t *testing.T, native bool) {
	t.Helper()
	socket := os.Getenv("KLORQ_TEST_CTR_SOCKET")
	id := os.Getenv("KLORQ_TEST_OWNED_CONTAINER")
	fifo := os.Getenv("KLORQ_TEST_FIFO_DIR")
	if socket == "" || id == "" || fifo == "" {
		t.Skip("set KLORQ_TEST_CTR_SOCKET, KLORQ_TEST_OWNED_CONTAINER and KLORQ_TEST_FIFO_DIR for an owned VM")
	}
	backend, err := NewBackend(Config{Socket: socket}, BackendOptions{})
	if err != nil {
		t.Fatal(err)
	}
	handle := runtime.Handle{ID: id, FIFODir: fifo}
	execute := backend.Exec
	if native {
		execute = backend.ExecNative
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	guest := "/tmp/klorq-live-cancel-marker"
	_, err = execute(ctx, handle, runtime.ExecRequest{Argv: []string{"/bin/sh", "-c", `touch "$0" && test -e "$0" && rm "$0"`, guest}})
	if err != nil {
		t.Fatalf("positive control failed: %v", err)
	}
	execCtx, cancelExec := context.WithCancel(ctx)
	defer cancelExec()
	ready := &liveReadyWriter{ready: make(chan struct{})}
	done := make(chan error, 1)
	go func() {
		_, err := execute(execCtx, handle, runtime.ExecRequest{
			Argv: []string{"/bin/sh", "-c", `printf ready; sleep 2; touch "$0"`, guest}, Stdout: ready,
		})
		done <- err
	}()
	select {
	case <-ready.ready:
	case <-ctx.Done():
		t.Fatal("guest did not start")
	}
	cancelExec()
	if err := <-done; !errors.Is(err, context.Canceled) || strings.Contains(err.Error(), "terminate guest exec") {
		t.Fatalf("guest cancellation failed: %v", err)
	}
	select {
	case <-time.After(3 * time.Second):
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	_, err = execute(ctx, handle, runtime.ExecRequest{Argv: []string{"/bin/sh", "-c", `test ! -e "$0"`, guest}})
	if err != nil {
		t.Fatalf("guest command continued after cancellation: %v", err)
	}
}
