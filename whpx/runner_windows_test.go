//go:build windows

package whpx

import (
	"fmt"
	"testing"
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
