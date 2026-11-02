package ctr

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"

	runtime "github.com/quoiner/sandboxes-runtime"
)

// Exec runs a non-interactive command in an existing VM. It does not use a
// shell or expose the command's output in errors or logs.
func (b *Backend) Exec(ctx context.Context, h runtime.Handle, req runtime.ExecRequest) (int, error) {
	if err := validateNativeExec(h, req); err != nil {
		return 0, err
	}
	if len(req.Env) > 0 || req.User != "" {
		return 0, errors.New("ctr exec does not support environment or user overrides")
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return 0, fmt.Errorf("create exec identifier: %w", err)
	}
	execID := "klorq-exec-" + hex.EncodeToString(nonce[:])
	args := []string{"--address", b.client.cfg.Socket, "--namespace", b.client.cfg.Namespace,
		"tasks", "exec", "--exec-id", execID}
	if h.FIFODir != "" {
		args = append(args, "--fifo-dir", h.FIFODir)
	}
	if req.Workdir != "" {
		args = append(args, "--cwd", req.Workdir)
	}
	args = append(args, h.ID)
	args = append(args, req.Argv...)
	return b.runExec(ctx, args, h.ID, execID, req.Stdout, req.Stderr)
}

func validExecHandle(id string) bool {
	if !strings.HasPrefix(id, "klorq-") || len(id) <= len("klorq-") || len(id) > 69 {
		return false
	}
	for _, c := range id[len("klorq-"):] {
		if c < 'a' || c > 'z' {
			if c < 'A' || c > 'Z' {
				if (c < '0' || c > '9') && c != '-' && c != '_' && c != '.' {
					return false
				}
			}
		}
	}
	return true
}

func (b *Backend) runExec(ctx context.Context, args []string, containerID, execID string, stdout, stderr io.Writer) (int, error) {
	cmd := exec.CommandContext(ctx, b.client.cfg.CTR, args...) // #nosec G204 -- executable is operator-configured, arguments are passed without a shell
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	err := cmd.Run()
	if err == nil {
		return 0, nil
	}
	if ctx.Err() != nil {
		if cmd.Process == nil {
			return 0, ctx.Err()
		}
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		_, cleanupErr := b.client.run(cleanupCtx, "tasks", "kill", "--signal", "SIGKILL", "--exec-id", execID, containerID)
		if cleanupErr != nil {
			return 0, errors.Join(ctx.Err(), fmt.Errorf("terminate guest exec: %w", cleanupErr))
		}
		return 0, ctx.Err()
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode(), fmt.Errorf("runtime exec failed: %w", err)
	}
	return 0, fmt.Errorf("start runtime exec: %w", err)
}
