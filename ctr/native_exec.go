package ctr

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"path"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/pkg/cio"
	specs "github.com/opencontainers/runtime-spec/specs-go"

	runtime "github.com/lickren/sandboxes-runtime"
)

// ExecCancelable uses the native client only with cancellation-aware sinks.
// Validation happens before any guest process is created.
func (b *Backend) ExecCancelable(ctx context.Context, h runtime.Handle, req runtime.ExecRequest) (int, error) {
	stdout, err := cancelableOutput(ctx, req.Stdout)
	if err != nil {
		return 0, fmt.Errorf("stdout: %w", err)
	}
	stderr, err := cancelableOutput(ctx, req.Stderr)
	if err != nil {
		return 0, fmt.Errorf("stderr: %w", err)
	}
	req.Stdout, req.Stderr = stdout, stderr
	return b.ExecNative(ctx, h, req)
}

type nativeWriteFunc func([]byte) (int, error)

func (f nativeWriteFunc) Write(p []byte) (int, error) { return f(p) }

func cancelableOutput(ctx context.Context, output io.Writer) (io.Writer, error) {
	if output == nil {
		return io.Discard, nil
	}
	writer, ok := output.(runtime.ContextWriter)
	if !ok {
		return nil, errors.New("output writer does not support cancellation")
	}
	return nativeWriteFunc(func(p []byte) (int, error) {
		if ctx.Err() != nil {
			return len(p), nil //nolint:nilerr // Drain discarded output after cancellation to unblock the guest transport.
		}
		n, err := writer.WriteContext(ctx, p)
		if ctx.Err() != nil {
			return len(p), nil //nolint:nilerr // Cancellation intentionally switches the sink to discard.
		}
		return n, err
	}), nil
}

// ExecNative is an experimental non-interactive containerd API executor.
// It is not the default Backend.Exec until its lifecycle is validated.
func (b *Backend) ExecNative(ctx context.Context, h runtime.Handle, req runtime.ExecRequest) (code int, retErr error) {
	return b.execNative(ctx, h, runtime.InteractiveExecRequest{ExecRequest: req})
}

func (b *Backend) execNative(ctx context.Context, h runtime.Handle, interactive runtime.InteractiveExecRequest) (code int, retErr error) {
	req := interactive.ExecRequest
	if err := validateNativeExec(h, req); err != nil {
		return 0, err
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return 0, fmt.Errorf("create exec identifier: %w", err)
	}
	client, err := containerd.New(b.client.cfg.Socket, containerd.WithDefaultNamespace(b.client.cfg.Namespace))
	if err != nil {
		return 0, fmt.Errorf("connect containerd: %w", err)
	}
	defer func() { retErr = errors.Join(retErr, client.Close()) }()
	container, err := client.LoadContainer(ctx, h.ID)
	if err != nil {
		return 0, fmt.Errorf("load guest container: %w", err)
	}
	spec, err := container.Spec(ctx)
	if err != nil {
		return 0, fmt.Errorf("load guest process specification: %w", err)
	}
	if spec.Process == nil {
		return 0, errors.New("guest process specification is missing")
	}
	if err := b.configureProcess(ctx, h, spec.Process, interactive); err != nil {
		return 0, err
	}
	task, err := container.Task(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("load guest task: %w", err)
	}
	stdout := trackNativeOutput(req.Stdout)
	stderr := trackNativeOutput(req.Stderr)
	creator, inputDone := interactiveCreator(h, interactive, stdout, stderr)
	process, err := task.Exec(ctx, "klorq-exec-"+hex.EncodeToString(nonce[:]), spec.Process, creator)
	if err != nil {
		return 0, fmt.Errorf("create guest process: %w", err)
	}
	defer func() { retErr = errors.Join(retErr, cleanupInteractive(ctx, process, interactive.Stdin)) }()
	status, err := process.Wait(ctx)
	if err != nil {
		return 0, fmt.Errorf("wait guest process: %w", err)
	}
	if err := process.Start(ctx); err != nil {
		if code, handled := startErrorCode(req.Stdout, err); handled {
			return code, nil
		}
		return 0, fmt.Errorf("start guest process: %w", err)
	}
	controlCtx, stopControl := context.WithCancel(ctx)
	controlDone := make(chan error, 1)
	go runInteractiveControl(controlCtx, stopControl, process, inputDone, interactive.Resize, controlDone)
	defer func() { stopControl(); retErr = errors.Join(retErr, <-controlDone) }()
	return waitNativeExec(controlCtx, status, process.IO(), stdout, stderr)
}

func startErrorCode(output io.Writer, err error) (int, bool) {
	code, message, ok := classifyStartError(err)
	if !ok {
		return 0, false
	}
	if output != nil {
		_, _ = io.WriteString(output, message+"\n")
	}
	return code, true
}

func classifyStartError(err error) (int, string, bool) {
	message := err.Error()
	switch {
	case strings.Contains(message, "not found in $PATH"):
		return 127, message, true
	case strings.Contains(message, "exists but it is not executable"):
		return 127, message, true
	default:
		return 0, "", false
	}
}

func waitNativeExec(ctx context.Context, status <-chan containerd.ExitStatus, output cio.IO, stdout, stderr *nativeOutput) (int, error) {
	select {
	case result, ok := <-status:
		if !ok {
			return 0, errors.New("guest process exit status stream closed")
		}
		exitCode, _, err := result.Result()
		if err != nil {
			return 0, fmt.Errorf("wait guest process: %w", err)
		}
		if err := waitNativeOutputGrace(ctx, output, nativeExecDrainGrace); err != nil {
			if !errors.Is(err, errDrainGraceExpired) {
				return 0, err
			}
		}
		if err := errors.Join(stdout.Err(), stderr.Err()); err != nil {
			return 0, fmt.Errorf("write guest process output: %w", err)
		}
		return int(exitCode), nil
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}

// nativeExecDrainGrace bounds how long an exited guest process's output
// pipes may keep draining. A guest helper that outlives the main process
// (for example BusyBox wget's ssl_client when the server closes TCP without
// a TLS close_notify) would otherwise hold the FIFOs open indefinitely and
// hang the exec stream even though the main process already exited.
var nativeExecDrainGrace = 2 * time.Second

var errDrainGraceExpired = errors.New("output drain grace expired")

// waitNativeOutputGrace waits for containerd's FIFO copy goroutines to stop
// after the guest process exited. Cancellation closes the FIFO reads; when
// the grace expires the FIFOs are closed and the remaining pipe holders
// (orphaned guest helpers) are detached so the exec stream can finish. The
// copies themselves are not awaited on grace expiry: a fifo.Read blocked
// while a guest helper holds the write end is not interrupted by closing the
// FIFO on all platforms, so the copies finish later when the shim closes the
// FIFOs at sandbox stop or delete.
func waitNativeOutputGrace(ctx context.Context, output cio.IO, grace time.Duration) error {
	done := make(chan struct{})
	go func() {
		output.Wait()
		close(done)
	}()
	if grace <= 0 {
		grace = time.Second
	}
	timer := time.NewTimer(grace)
	defer timer.Stop()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		output.Cancel()
		<-done
		return ctx.Err()
	case <-timer.C:
		output.Cancel()
		return errDrainGraceExpired
	}
}

type nativeOutput struct {
	destination io.Writer
	mu          sync.Mutex
	err         error
}

func trackNativeOutput(destination io.Writer) *nativeOutput {
	return &nativeOutput{destination: destination}
}

func (o *nativeOutput) writer() io.Writer {
	if o.destination == nil {
		return nil
	}
	return o
}

func (o *nativeOutput) Write(p []byte) (int, error) {
	n, err := o.destination.Write(p)
	if err == nil && n != len(p) {
		err = io.ErrShortWrite
	}
	if err != nil {
		o.mu.Lock()
		o.err = errors.Join(o.err, err)
		o.mu.Unlock()
	}
	return n, err
}

func (o *nativeOutput) Err() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.err
}

func validateNativeExec(h runtime.Handle, req runtime.ExecRequest) error {
	if !validExecHandle(h.ID) || len(req.Argv) == 0 || req.Argv[0] == "" {
		return errors.New("invalid runtime handle or empty command")
	}
	if req.Workdir != "" && (!path.IsAbs(req.Workdir) || strings.ContainsRune(req.Workdir, '\x00')) {
		return errors.New("guest working directory must be absolute")
	}
	for _, arg := range req.Argv {
		if strings.ContainsRune(arg, '\x00') {
			return errors.New("command argument contains NUL")
		}
	}
	if err := validateExecEnv(req.Env); err != nil {
		return err
	}
	return validateUserSpec(req.User)
}

const (
	maxExecEnvEntries = 256
	maxExecEnvEntry   = 64 * 1024
	maxExecEnvTotal   = 256 * 1024
)

func validateExecEnv(env []string) error {
	if len(env) > maxExecEnvEntries {
		return errors.New("too many environment overrides")
	}
	total := 0
	for _, entry := range env {
		if len(entry) > maxExecEnvEntry {
			return errors.New("environment override is too large")
		}
		total += len(entry)
		if total > maxExecEnvTotal {
			return errors.New("environment overrides are too large")
		}
		key, _, ok := strings.Cut(entry, "=")
		if !ok || !validEnvName(key) {
			return errors.New("environment override must be KEY=VALUE with a valid name")
		}
		if strings.ContainsRune(entry, '\x00') {
			return errors.New("environment override contains NUL")
		}
	}
	return nil
}

func validEnvName(name string) bool {
	if name == "" || len(name) > 255 || !envNameStart(name[0]) {
		return false
	}
	for i := 1; i < len(name); i++ {
		if !envNameChar(name[i]) {
			return false
		}
	}
	return true
}

func envNameStart(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func envNameChar(c byte) bool {
	return envNameStart(c) || (c >= '0' && c <= '9')
}

func mergeExecEnv(base, overrides []string) []string {
	result := make([]string, 0, len(base)+len(overrides))
	positions := make(map[string]int, len(base)+len(overrides))
	add := func(entry string) {
		key, _, _ := strings.Cut(entry, "=")
		if index, ok := positions[key]; ok {
			result[index] = entry
			return
		}
		positions[key] = len(result)
		result = append(result, entry)
	}
	for _, entry := range base {
		add(entry)
	}
	for _, entry := range overrides {
		add(entry)
	}
	return result
}

func validateUserSpec(user string) error {
	if user == "" {
		return nil
	}
	if len(user) > 64 || strings.ContainsRune(user, '\x00') {
		return errors.New("user must be uid[:gid], name or name:group")
	}
	parts := strings.Split(user, ":")
	if len(parts) > 2 || !validUserPart(parts[0]) {
		return errors.New("user must be uid[:gid], name or name:group")
	}
	if len(parts) == 2 && !validUserPart(parts[1]) {
		return errors.New("user group must be gid or group name")
	}
	return nil
}

func validUserPart(part string) bool {
	if part == "" {
		return false
	}
	if _, err := strconv.ParseUint(part, 10, 32); err == nil {
		return true
	}
	for i := 0; i < len(part); i++ {
		c := part[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == '_' || (i > 0 && ((c >= '0' && c <= '9') || c == '-' || c == '.')) {
			continue
		}
		return false
	}
	return true
}

func parseExecUser(user string) (uid, gid uint32, hasGID bool, err error) {
	if user == "" {
		return 0, 0, false, nil
	}
	if len(user) > 64 || strings.ContainsRune(user, '\x00') {
		return 0, 0, false, errors.New("user must be a numeric uid[:gid]")
	}
	parts := strings.Split(user, ":")
	if len(parts) > 2 || parts[0] == "" {
		return 0, 0, false, errors.New("user must be a numeric uid[:gid]")
	}
	parsedUID, err := strconv.ParseUint(parts[0], 10, 32)
	if err != nil {
		return 0, 0, false, errors.New("user must be a numeric uid[:gid]; user names are not resolved yet")
	}
	uid = uint32(parsedUID)
	if len(parts) == 1 {
		return uid, 0, false, nil
	}
	if parts[1] == "" {
		return 0, 0, false, errors.New("user group must be a numeric gid")
	}
	parsedGID, err := strconv.ParseUint(parts[1], 10, 32)
	if err != nil {
		return 0, 0, false, errors.New("user group must be a numeric gid")
	}
	return uid, uint32(parsedGID), true, nil
}

func (b *Backend) configureProcess(ctx context.Context, h runtime.Handle, process *specs.Process, interactive runtime.InteractiveExecRequest) error {
	req := interactive.ExecRequest
	process.Args = append([]string(nil), req.Argv...)
	process.Terminal = interactive.TTY
	setInitialTerminalSize(process, interactive)
	if req.Workdir != "" {
		process.Cwd = req.Workdir
	}
	return b.applyExecOverrides(ctx, h, process, req)
}

func (b *Backend) applyExecOverrides(ctx context.Context, h runtime.Handle, process *specs.Process, req runtime.ExecRequest) error {
	process.Env = mergeExecEnv(process.Env, req.Env)
	if req.User == "" {
		return nil
	}
	if isNumericUser(req.User) {
		return applyExecUser(process, req.User)
	}
	uid, gid, err := b.resolveGuestUser(ctx, h, req.User)
	if err != nil {
		return err
	}
	process.User.UID = uid
	process.User.GID = gid
	return nil
}

func isNumericUser(user string) bool {
	first := user
	if index := strings.IndexByte(user, ':'); index >= 0 {
		first = user[:index]
	}
	if first == "" {
		return false
	}
	_, err := strconv.ParseUint(first, 10, 32)
	return err == nil
}

func (b *Backend) resolveGuestUser(ctx context.Context, h runtime.Handle, spec string) (uint32, uint32, error) {
	parts, err := splitUserSpec(spec)
	if err != nil {
		return 0, 0, err
	}
	uid, gid, err := b.lookupGuestUser(ctx, h, parts[0])
	if err != nil {
		return 0, 0, err
	}
	if len(parts) == 1 {
		return uid, gid, nil
	}
	if parts[1] == "" {
		return 0, 0, errors.New("user group must not be empty")
	}
	if parsed, err := strconv.ParseUint(parts[1], 10, 32); err == nil {
		return uid, uint32(parsed), nil
	}
	groupGID, err := b.lookupGuestGroup(ctx, h, parts[1])
	if err != nil {
		return 0, 0, err
	}
	return uid, groupGID, nil
}

func splitUserSpec(spec string) ([]string, error) {
	if len(spec) > 64 || strings.ContainsRune(spec, '\x00') {
		return nil, errors.New("user must be uid[:gid], name or name:group")
	}
	parts := strings.Split(spec, ":")
	if len(parts) > 2 || parts[0] == "" {
		return nil, errors.New("user must be uid[:gid], name or name:group")
	}
	return parts, nil
}

func (b *Backend) lookupGuestUser(ctx context.Context, h runtime.Handle, name string) (uint32, uint32, error) {
	output, code, err := b.probeGuest(ctx, h, `id -u "$1" && id -g "$1"`, name)
	if err != nil {
		return 0, 0, fmt.Errorf("resolve user %q: %w", name, err)
	}
	if code != 0 {
		return 0, 0, fmt.Errorf("resolve user %q: guest user does not exist", name)
	}
	lines := strings.Fields(output)
	if len(lines) != 2 {
		return 0, 0, fmt.Errorf("resolve user %q: unexpected id output", name)
	}
	uid, err := strconv.ParseUint(lines[0], 10, 32)
	if err != nil {
		return 0, 0, fmt.Errorf("resolve user %q: invalid uid", name)
	}
	gid, err := strconv.ParseUint(lines[1], 10, 32)
	if err != nil {
		return 0, 0, fmt.Errorf("resolve user %q: invalid gid", name)
	}
	return uint32(uid), uint32(gid), nil
}

func (b *Backend) lookupGuestGroup(ctx context.Context, h runtime.Handle, name string) (uint32, error) {
	output, code, err := b.probeGuest(ctx, h, `awk -F: -v n="$1" '$1==n {print $3}' /etc/group`, name)
	if err != nil {
		return 0, fmt.Errorf("resolve group %q: %w", name, err)
	}
	if code != 0 || strings.TrimSpace(output) == "" {
		return 0, fmt.Errorf("resolve group %q: guest group does not exist", name)
	}
	gid, err := strconv.ParseUint(strings.TrimSpace(output), 10, 32)
	if err != nil {
		return 0, fmt.Errorf("resolve group %q: invalid gid", name)
	}
	return uint32(gid), nil
}

func (b *Backend) probeGuest(ctx context.Context, h runtime.Handle, script, argument string) (string, int, error) {
	var stdout, stderr strings.Builder
	code, err := b.ExecNative(ctx, h, runtime.ExecRequest{
		Argv:   []string{"/bin/sh", "-c", script, "klorq-user", argument},
		Stdout: &stdout,
		Stderr: &stderr,
	})
	if err != nil {
		return "", 0, err
	}
	return stdout.String(), code, nil
}

func applyExecUser(process *specs.Process, user string) error {
	if user == "" {
		return nil
	}
	uid, gid, hasGID, err := parseExecUser(user)
	if err != nil {
		return err
	}
	process.User.UID = uid
	if hasGID {
		process.User.GID = gid
	}
	return nil
}

func cleanupNativeProcess(parent context.Context, process containerd.Process) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), 10*time.Second)
	defer cancel()
	status, err := process.Status(ctx)
	if err != nil {
		return fmt.Errorf("inspect guest exec cleanup: %w", err)
	}
	if status.Status == containerd.Running {
		if err := process.Kill(ctx, syscall.SIGKILL); err != nil {
			return fmt.Errorf("terminate guest exec: %w", err)
		}
		for status.Status == containerd.Running {
			select {
			case <-ctx.Done():
				return fmt.Errorf("wait guest exec cleanup: %w", ctx.Err())
			case <-time.After(50 * time.Millisecond):
			}
			status, err = process.Status(ctx)
			if err != nil {
				return fmt.Errorf("inspect guest exec cleanup: %w", err)
			}
		}
	}
	if _, err := process.Delete(ctx); err != nil {
		return fmt.Errorf("delete guest exec: %w", err)
	}
	return nil
}

// ExecInteractive requires cancellation-aware output and owned closeable stdin.
func (b *Backend) ExecInteractive(ctx context.Context, h runtime.Handle, req runtime.InteractiveExecRequest) (int, error) {
	stdout, err := cancelableOutput(ctx, req.Stdout)
	if err != nil {
		return 0, fmt.Errorf("stdout: %w", err)
	}
	stderr, err := cancelableOutput(ctx, req.Stderr)
	if err != nil {
		return 0, fmt.Errorf("stderr: %w", err)
	}
	req.Stdout, req.Stderr = stdout, stderr
	if req.TTY && ((req.Size.Rows == 0) != (req.Size.Cols == 0) || req.Size.Rows > 65535 || req.Size.Cols > 65535) {
		return 0, errors.New("invalid terminal size")
	}
	return b.execNative(ctx, h, req)
}

type sessionInput struct {
	reader io.Reader
	done   chan struct{}
	once   sync.Once
}

func (r *sessionInput) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	if err != nil {
		r.once.Do(func() { close(r.done) })
	}
	return n, err
}
func controlInteractive(ctx context.Context, process containerd.Process, inputDone <-chan struct{}, resize <-chan runtime.TerminalSize) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-inputDone:
			inputDone = nil
			if err := process.CloseIO(ctx, containerd.WithStdinCloser); err != nil && ctx.Err() == nil {
				return fmt.Errorf("close guest stdin: %w", err)
			}
		case size, ok := <-resize:
			if !ok {
				resize = nil
				continue
			}
			if size.Rows == 0 || size.Cols == 0 || size.Rows > 65535 || size.Cols > 65535 {
				return errors.New("invalid terminal size")
			}
			if err := process.Resize(ctx, size.Cols, size.Rows); err != nil && ctx.Err() == nil {
				return fmt.Errorf("resize guest terminal: %w", err)
			}
		}
	}
}

func setInitialTerminalSize(process *specs.Process, req runtime.InteractiveExecRequest) {
	if req.TTY {
		process.ConsoleSize = &specs.Box{Height: uint(req.Size.Rows), Width: uint(req.Size.Cols)}
	}
}

func interactiveCreator(h runtime.Handle, req runtime.InteractiveExecRequest, stdout, stderr *nativeOutput) (cio.Creator, <-chan struct{}) {
	var input io.Reader
	var inputDone chan struct{}
	if req.Stdin != nil {
		inputDone = make(chan struct{})
		input = &sessionInput{reader: req.Stdin, done: inputDone}
	}
	stderrWriter := stderr.writer()
	if req.TTY {
		stderrWriter = nil
	}
	opts := []cio.Opt{cio.WithStreams(input, stdout.writer(), stderrWriter), cio.WithFIFODir(h.FIFODir)}
	if req.TTY {
		opts = append(opts, cio.WithTerminal)
	}
	return cio.NewCreator(opts...), inputDone
}

func cleanupInteractive(ctx context.Context, process containerd.Process, input io.ReadCloser) error {
	var closeErr error
	if input != nil {
		_ = input.Close()
		closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		closeErr = process.CloseIO(closeCtx, containerd.WithStdinCloser)
		cancel()
	}
	return errors.Join(closeErr, cleanupNativeProcess(ctx, process))
}

func runInteractiveControl(ctx context.Context, cancel context.CancelFunc, process containerd.Process, inputDone <-chan struct{}, resize <-chan runtime.TerminalSize, done chan<- error) {
	err := controlInteractive(ctx, process, inputDone, resize)
	if err != nil {
		cancel()
	}
	done <- err
}
