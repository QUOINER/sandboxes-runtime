//go:build windows

package whpx

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Runner owns QEMU processes in kill-on-close Windows Job Objects. QEMU is
// created suspended, assigned to its job, and resumed only after assignment so
// daemon failure cannot leave a running guest outside the owned process tree.
type Runner struct {
	qemuPath  string
	mu        sync.Mutex
	instances map[string]*instance
	closed    bool
	onExit    func(id string, err error)
}

type instance struct {
	process   windows.Handle
	job       windows.Handle
	pipeName  string
	connectMu sync.Mutex
	pipeMu    sync.Mutex
	pipeConn  *guestPipeConn
	ready     chan struct{}
	done      chan error
	stopping  bool
	stopMu    sync.Mutex
}

// StartConfig describes the boot inputs for one guest.
type StartConfig struct {
	Boot Config
}

// NewRunner creates a QEMU process owner for the configured executable.
func NewRunner(qemuPath string) (*Runner, error) {
	if !filepath.IsAbs(qemuPath) {
		return nil, errors.New("QEMU path must be absolute")
	}
	info, err := os.Lstat(qemuPath)
	if err != nil {
		return nil, fmt.Errorf("inspect QEMU executable: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("QEMU executable must be a regular non-symlink file")
	}
	return &Runner{qemuPath: qemuPath, instances: make(map[string]*instance)}, nil
}

// SetOnExit installs the callback for unexpected QEMU exits.
func (r *Runner) SetOnExit(callback func(id string, err error)) {
	r.mu.Lock()
	r.onExit = callback
	r.mu.Unlock()
}

// Start launches a QEMU WHPX guest and tracks its process until exit.
func (r *Runner) Start(ctx context.Context, id string, cfg StartConfig) error {
	if !validInstanceID(id) {
		return errors.New("invalid WHPX instance ID")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	pipeName, err := newGuestPipeName()
	if err != nil {
		return err
	}
	cfg.Boot.GuestPipeName = pipeName
	args, err := BuildArgs(cfg.Boot)
	if err != nil {
		return err
	}
	commandLine := windows.ComposeCommandLine(append([]string{r.qemuPath}, args...))
	commandLineUTF16, err := windows.UTF16FromString(commandLine)
	if err != nil {
		return fmt.Errorf("encode QEMU command line: %w", err)
	}
	applicationName, err := windows.UTF16PtrFromString(r.qemuPath)
	if err != nil {
		return fmt.Errorf("encode QEMU path: %w", err)
	}
	currentDir, err := windows.UTF16PtrFromString(filepath.Dir(r.qemuPath))
	if err != nil {
		return fmt.Errorf("encode QEMU working directory: %w", err)
	}
	environment, err := qemuEnvironmentBlock()
	if err != nil {
		return err
	}

	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return errors.New("WHPX runner is closed")
	}
	if _, exists := r.instances[id]; exists {
		r.mu.Unlock()
		return errors.New("WHPX instance already exists")
	}
	current := &instance{ready: make(chan struct{}), done: make(chan error, 1), pipeName: pipeName}
	r.instances[id] = current
	r.mu.Unlock()

	job, err := newKillOnCloseJob()
	if err != nil {
		return r.failStart(id, current, err)
	}
	startup := windows.StartupInfo{Cb: uint32(unsafe.Sizeof(windows.StartupInfo{}))}
	var proc windows.ProcessInformation
	creationFlags := uint32(windows.CREATE_SUSPENDED | windows.CREATE_UNICODE_ENVIRONMENT | windows.CREATE_NO_WINDOW)
	if err := windows.CreateProcess(applicationName, &commandLineUTF16[0], nil, nil, false,
		creationFlags, &environment[0], currentDir, &startup, &proc); err != nil {
		_ = windows.CloseHandle(job)
		return r.failStart(id, current, fmt.Errorf("create suspended QEMU process: %w", err))
	}
	if err := windows.AssignProcessToJobObject(job, proc.Process); err != nil {
		_ = windows.TerminateProcess(proc.Process, 1)
		_, _ = windows.WaitForSingleObject(proc.Process, windows.INFINITE)
		_ = windows.CloseHandle(proc.Thread)
		_ = windows.CloseHandle(proc.Process)
		_ = windows.CloseHandle(job)
		return r.failStart(id, current, fmt.Errorf("assign suspended QEMU process to kill-on-close job: %w", err))
	}
	if _, err := windows.ResumeThread(proc.Thread); err != nil {
		_ = windows.TerminateJobObject(job, 1)
		_, _ = windows.WaitForSingleObject(proc.Process, windows.INFINITE)
		_ = windows.CloseHandle(proc.Thread)
		_ = windows.CloseHandle(proc.Process)
		_ = windows.CloseHandle(job)
		return r.failStart(id, current, fmt.Errorf("resume QEMU process: %w", err))
	}
	_ = windows.CloseHandle(proc.Thread)

	r.mu.Lock()
	current.process = proc.Process
	current.job = job
	close(current.ready)
	r.mu.Unlock()
	go r.wait(id, current)
	if err := ctx.Err(); err != nil {
		return errors.Join(err, r.Stop(context.Background(), id))
	}
	return nil
}

// ConnectGuest opens the QEMU virtio-serial named pipe and requires the
// protected per-user ACL configured by the private-pipe QEMU build. Only one
// host connection may be active for an instance at a time.
func (r *Runner) ConnectGuest(ctx context.Context, id string) (net.Conn, error) {
	r.mu.Lock()
	current := r.instances[id]
	r.mu.Unlock()
	if current == nil {
		return nil, errors.New("WHPX instance is not running")
	}
	select {
	case <-current.ready:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	current.connectMu.Lock()
	defer current.connectMu.Unlock()
	if current.process == 0 || current.pipeName == "" {
		return nil, errors.New("WHPX guest transport is unavailable")
	}
	current.pipeMu.Lock()
	connected := current.pipeConn != nil
	current.pipeMu.Unlock()
	if connected {
		return nil, errors.New("WHPX guest transport is already connected")
	}
	pipePath := `\\.\pipe\` + current.pipeName
	var file *os.File
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		handle, err := windows.CreateFile(windows.StringToUTF16Ptr(pipePath),
			windows.GENERIC_READ|windows.GENERIC_WRITE|windows.READ_CONTROL, 0, nil, windows.OPEN_EXISTING,
			windows.FILE_ATTRIBUTE_NORMAL, 0)
		if err == nil {
			if err := verifyGuestPipeACL(handle); err != nil {
				_ = windows.CloseHandle(handle)
				return nil, fmt.Errorf("WHPX guest pipe security check failed: %w", err)
			}
			file = os.NewFile(uintptr(handle), pipePath)
			break
		}
		if !errors.Is(err, windows.ERROR_FILE_NOT_FOUND) && !errors.Is(err, windows.ERROR_PIPE_BUSY) {
			return nil, fmt.Errorf("connect to WHPX guest pipe: %w", err)
		}
		select {
		case <-current.done:
			return nil, errors.New("QEMU exited before the guest transport became ready")
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
	host, bridge := net.Pipe()
	conn := &guestPipeConn{Conn: host, file: file, bridge: bridge}
	current.pipeMu.Lock()
	if current.pipeConn != nil {
		current.pipeMu.Unlock()
		_ = conn.Close()
		return nil, errors.New("WHPX guest transport is already connected")
	}
	current.pipeConn = conn
	current.pipeMu.Unlock()
	go conn.pump(current)
	return conn, nil
}

func newGuestPipeName() (string, error) {
	var nonce [32]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", fmt.Errorf("generate WHPX guest pipe name: %w", err)
	}
	return "lickren-guest-" + hex.EncodeToString(nonce[:]), nil
}

func verifyGuestPipeACL(handle windows.Handle) error {
	token := windows.GetCurrentProcessToken()
	user, err := token.GetTokenUser()
	if err != nil {
		return fmt.Errorf("read current user SID: %w", err)
	}
	sid := user.User.Sid.String()
	descriptor, err := windows.GetSecurityInfo(handle, windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return fmt.Errorf("read named pipe security descriptor: %w", err)
	}
	owner, _, err := descriptor.Owner()
	if err != nil || owner.String() != sid {
		return errors.New("named pipe owner is not the current user")
	}
	control, _, err := descriptor.Control()
	if err != nil || control&windows.SE_DACL_PROTECTED == 0 {
		return errors.New("named pipe DACL is not protected")
	}
	dacl, defaulted, err := descriptor.DACL()
	if err != nil || defaulted || dacl == nil || dacl.AceCount != 2 {
		return errors.New("named pipe DACL must grant access only to the current user and SYSTEM")
	}
	seenUser, seenSystem := false, false
	for index := uint32(0); index < uint32(dacl.AceCount); index++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, index, &ace); err != nil || ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE ||
			ace.Header.AceFlags != 0 || ace.Mask != 0x001f01ff {
			return errors.New("named pipe DACL contains a non-standard access rule")
		}
		principal := (*windows.SID)(unsafe.Pointer(&ace.SidStart)).String()
		switch principal {
		case sid:
			seenUser = true
		case "S-1-5-18":
			seenSystem = true
		default:
			return errors.New("named pipe DACL grants access to another principal")
		}
	}
	if !seenUser || !seenSystem {
		return errors.New("named pipe DACL is missing the current user or SYSTEM")
	}
	return nil
}

type guestPipeConn struct {
	net.Conn
	file     *os.File
	bridge   net.Conn
	once     sync.Once
	closeErr error
}

func (c *guestPipeConn) pump(current *instance) {
	finished := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(c.bridge, c.file); finished <- struct{}{} }()
	go func() { _, _ = io.Copy(c.file, c.bridge); finished <- struct{}{} }()
	<-finished
	_ = c.Close()
	current.pipeMu.Lock()
	if current.pipeConn == c {
		current.pipeConn = nil
	}
	current.pipeMu.Unlock()
}

func (c *guestPipeConn) Close() error {
	c.once.Do(func() {
		c.closeErr = errors.Join(c.Conn.Close(), c.bridge.Close(), c.file.Close())
	})
	return c.closeErr
}

func (r *Runner) wait(id string, current *instance) {
	waitResult, waitErr := windows.WaitForSingleObject(current.process, windows.INFINITE)
	var exitErr error
	if waitErr != nil {
		exitErr = fmt.Errorf("wait for QEMU process: %w", waitErr)
	} else if waitResult != windows.WAIT_OBJECT_0 {
		exitErr = fmt.Errorf("wait for QEMU process returned unexpected result 0x%x", waitResult)
	} else {
		var code uint32
		if err := windows.GetExitCodeProcess(current.process, &code); err != nil {
			exitErr = fmt.Errorf("read QEMU exit status: %w", err)
		} else {
			exitErr = fmt.Errorf("QEMU exited unexpectedly with status %d", code)
		}
	}
	r.mu.Lock()
	if current.stopping {
		exitErr = nil
	}
	onExit := r.onExit
	if r.instances[id] == current {
		delete(r.instances, id)
	}
	r.mu.Unlock()
	current.pipeMu.Lock()
	if current.pipeConn != nil {
		_ = current.pipeConn.Close()
		current.pipeConn = nil
	}
	current.pipeMu.Unlock()
	jobErr := windows.CloseHandle(current.job)
	processErr := windows.CloseHandle(current.process)
	current.done <- errors.Join(exitErr, jobErr, processErr)
	close(current.done)
	if exitErr != nil && onExit != nil {
		onExit(id, exitErr)
	}
}

// Stop terminates the owned process tree and waits for the QEMU process.
func (r *Runner) Stop(ctx context.Context, id string) error {
	r.mu.Lock()
	current := r.instances[id]
	r.mu.Unlock()
	if current == nil {
		return nil
	}
	select {
	case err := <-current.done:
		return err
	default:
	}
	select {
	case <-current.ready:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case err := <-current.done:
		return err
	default:
	}
	current.stopMu.Lock()
	defer current.stopMu.Unlock()
	select {
	case err := <-current.done:
		return err
	default:
	}
	r.mu.Lock()
	current.stopping = true
	r.mu.Unlock()
	if err := windows.TerminateJobObject(current.job, 1); err != nil {
		select {
		case waitErr := <-current.done:
			return waitErr
		default:
			return fmt.Errorf("terminate WHPX job: %w", err)
		}
	}
	select {
	case err := <-current.done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Close terminates all owned VM process trees and prevents future starts.
func (r *Runner) Close(ctx context.Context) error {
	r.mu.Lock()
	r.closed = true
	ids := make([]string, 0, len(r.instances))
	for id := range r.instances {
		ids = append(ids, id)
	}
	r.mu.Unlock()
	var result error
	for _, id := range ids {
		result = errors.Join(result, r.Stop(ctx, id))
	}
	return result
}

func (r *Runner) failStart(id string, current *instance, cause error) error {
	r.mu.Lock()
	if r.instances[id] == current {
		delete(r.instances, id)
	}
	r.mu.Unlock()
	close(current.ready)
	current.done <- cause
	close(current.done)
	return cause
}

func newKillOnCloseJob() (windows.Handle, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return 0, fmt.Errorf("create Windows Job Object: %w", err)
	}
	limits := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	limits.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&limits)), uint32(unsafe.Sizeof(limits))); err != nil {
		_ = windows.CloseHandle(job)
		return 0, fmt.Errorf("set kill-on-close job policy: %w", err)
	}
	return job, nil
}

func validInstanceID(id string) bool {
	if id == "" || len(id) > 128 || strings.TrimSpace(id) != id {
		return false
	}
	for _, r := range id {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			continue
		}
		return false
	}
	return true
}

func qemuEnvironmentBlock() ([]uint16, error) {
	systemRoot, ok := os.LookupEnv("SystemRoot")
	if !ok || systemRoot == "" {
		return nil, errors.New("Windows SystemRoot environment is unavailable")
	}
	securityDescriptor, err := qemuPipeSecurityDescriptor()
	if err != nil {
		return nil, err
	}
	values := []string{"SystemRoot=" + systemRoot, "LICKREN_PIPE_SDDL=" + securityDescriptor}
	for _, key := range []string{"WINDIR", "TEMP", "TMP"} {
		if value, ok := os.LookupEnv(key); ok && value != "" {
			values = append(values, key+"="+value)
		}
	}
	sort.Slice(values, func(i, j int) bool { return strings.ToUpper(values[i]) < strings.ToUpper(values[j]) })
	block := strings.Join(values, "\x00") + "\x00\x00"
	return utf16.Encode([]rune(block)), nil
}

func qemuPipeSecurityDescriptor() (string, error) {
	token := windows.GetCurrentProcessToken()
	user, err := token.GetTokenUser()
	if err != nil {
		return "", fmt.Errorf("read current user SID for QEMU pipe ACL: %w", err)
	}
	sid := user.User.Sid.String()
	return "O:" + sid + "D:P(A;;0x001F01FF;;;" + sid + ")(A;;0x001F01FF;;;SY)", nil
}
