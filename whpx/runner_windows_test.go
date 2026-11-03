//go:build windows

package whpx

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

func TestVerifyGuestPipeACL(t *testing.T) {
	userToken := windows.GetCurrentProcessToken()
	user, err := userToken.GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	userSID := user.User.Sid.String()

	tests := []struct {
		name    string
		sddl    string
		wantErr bool
	}{
		{name: "current user and SYSTEM only", sddl: "O:" + userSID + "D:P(A;;0x001F01FF;;;" + userSID + ")(A;;0x001F01FF;;;SY)"},
		{name: "rejects world access", sddl: "O:" + userSID + "D:P(A;;0x001F01FF;;;" + userSID + ")(A;;0x001F01FF;;;WD)", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := openTestNamedPipeClient(t, tt.sddl)
			defer windows.CloseHandle(client)
			err := verifyGuestPipeACL(client)
			if (err != nil) != tt.wantErr {
				t.Fatalf("verifyGuestPipeACL() error = %v, wantErr %t", err, tt.wantErr)
			}
		})
	}
}

func TestRunnerReapsUnexpectedQEMUExit(t *testing.T) {
	qemuPath, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	runner, err := NewRunner(qemuPath)
	if err != nil {
		t.Fatal(err)
	}
	exit := make(chan error, 1)
	runner.SetOnExit(func(id string, err error) {
		if id != "runner-exit-test" {
			exit <- fmt.Errorf("unexpected QEMU instance ID %q", id)
			return
		}
		exit <- err
	})

	artifacts := make([]string, 3)
	for i, name := range []string{"kernel", "initrd", "root.qcow2"} {
		path := filepath.Join(t.TempDir(), name)
		if err := os.WriteFile(path, []byte("test"), 0o600); err != nil {
			t.Fatal(err)
		}
		artifacts[i] = path
	}
	startCtx, cancelStart := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelStart()
	if err := runner.Start(startCtx, "runner-exit-test", StartConfig{Boot: Config{
		KernelPath: artifacts[0], InitrdPath: artifacts[1], RootDiskPath: artifacts[2],
		NetworkEndpoint: "127.0.0.1:54321", CPUs: 1, MemoryMiB: 512,
	}}); err != nil {
		t.Fatalf("start child process: %v", err)
	}

	select {
	case exitErr := <-exit:
		if exitErr == nil {
			t.Fatal("unexpected QEMU process exit was reported as successful")
		}
	case <-time.After(15 * time.Second):
		t.Fatal("runner did not report the unexpected QEMU process exit")
	}

	runner.mu.Lock()
	remaining := len(runner.instances)
	runner.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("runner retained %d exited instance(s)", remaining)
	}
	closeCtx, cancelClose := context.WithTimeout(context.Background(), time.Second)
	defer cancelClose()
	if err := runner.Close(closeCtx); err != nil {
		t.Fatalf("close runner after child exit: %v", err)
	}
}

func openTestNamedPipeClient(t *testing.T, sddl string) windows.Handle {
	t.Helper()
	pipeName, err := newGuestPipeName()
	if err != nil {
		t.Fatal(err)
	}
	pipePath := `\\.\pipe\` + pipeName
	pipePathUTF16, err := windows.UTF16PtrFromString(pipePath)
	if err != nil {
		t.Fatal(err)
	}
	descriptor, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		t.Fatal(err)
	}
	attributes := windows.SecurityAttributes{
		Length:             uint32(unsafe.Sizeof(windows.SecurityAttributes{})),
		SecurityDescriptor: descriptor,
	}
	// The test constructs a real local named pipe so ACL checks exercise the
	// same Win32 handle metadata as ConnectGuest, without launching a VM.
	server, err := windows.CreateNamedPipe(pipePathUTF16,
		windows.PIPE_ACCESS_DUPLEX|windows.FILE_FLAG_FIRST_PIPE_INSTANCE,
		windows.PIPE_TYPE_BYTE|windows.PIPE_READMODE_BYTE|windows.PIPE_WAIT|windows.PIPE_REJECT_REMOTE_CLIENTS,
		1, 0, 0, 0, &attributes)
	if err != nil {
		t.Fatal(fmt.Errorf("create ACL test pipe: %w", err))
	}
	t.Cleanup(func() { _ = windows.CloseHandle(server) })
	client, err := windows.CreateFile(pipePathUTF16,
		windows.GENERIC_READ|windows.GENERIC_WRITE|windows.READ_CONTROL, 0, nil,
		windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		t.Fatal(fmt.Errorf("open ACL test pipe: %w", err))
	}
	return client
}
