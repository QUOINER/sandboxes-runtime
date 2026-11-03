// Package runtime defines the VM backend surface.
package runtime

import (
	"context"
	"io"
	"time"
)

// Capabilities describes backend support.
type Capabilities struct {
	Workspace bool
	Network   bool
	Snapshots bool
	GPU       bool
	IPv6      bool
}

// Rule is a CIDR/protocol/port egress rule. ID is the stable policy rule
// identifier copied into the gateway audit log; it is never used for matching.
type Rule struct {
	ID       string
	Protocol string
	CIDR     string
	Port     uint16
	Domain   string
}

// Port maps a host loopback address/port to a guest port.
type Port struct {
	Host     uint16
	Guest    uint16
	Protocol string
	HostIP   string
}

// Workspace is a host path mounted at the same absolute path in the guest.
type Workspace struct {
	Path     string
	ReadOnly bool
}

// BlockVolume is a preformatted ext4 image attached as a virtio block device.
type BlockVolume struct {
	Source      string
	Destination string
}

// Config describes a VM create request.
type Config struct {
	Name         string
	Image        string
	Workspace    string
	Workspaces   []Workspace
	BlockVolumes []BlockVolume
	Ports        []Port
	Allow        []Rule
	Deny         []Rule
	CPUs         uint8
	MemoryMiB    uint32
	FIFODir      string
	Env          []string
	Privileged   bool
	// ProxyPort and ProxySocket request a guest-dialed vsock channel bridged
	// to a host unix socket (see ADR-0002). Zero disables it.
	ProxyPort   uint32
	ProxySocket string
	// ProxyHelperPath mounts the trusted static helper read-only and enables
	// the guest init wrapper when non-empty.
	ProxyHelperPath string
	// ProxyGuestPort is the helper's loopback HTTP port.
	ProxyGuestPort uint16
	// ProxyServiceHost is the fixed upstream host the helper may address.
	ProxyServiceHost string
}

// Handle identifies a runtime instance.
type Handle struct {
	ID      string
	FIFODir string
	Ports   []Port
	Allow   []Rule
	Deny    []Rule
}

// NetworkAttachment is the host network data required by the runtime.
type NetworkAttachment struct {
	SocketPath  string
	MAC         string
	DeviceIP    string
	GatewayIP   string
	Annotations []string
}

// Network starts and stops the host-side gateway for a sandbox.
type Network interface {
	Start(ctx context.Context, sandbox string, ports []Port, allow, deny []Rule) (NetworkAttachment, error)
	UpdatePolicy(ctx context.Context, sandbox string, ports []Port, allow, deny []Rule, revision int) error
	UpdatePorts(ctx context.Context, sandbox string, ports []Port) error
	Stop(ctx context.Context, sandbox string) error
}

// PolicyUpdater is implemented by runtimes that can replace a live egress policy.
type PolicyUpdater interface {
	UpdatePolicy(ctx context.Context, handle Handle, allow, deny []Rule, revision int) error
}

// PortUpdater is implemented by runtimes that can change live published ports.
type PortUpdater interface {
	UpdatePorts(ctx context.Context, handle Handle, ports []Port) error
}

// Backend is implemented by the containerd/nerdbox adapter.
type Backend interface {
	Capabilities(ctx context.Context) (Capabilities, error)
	Create(ctx context.Context, cfg Config) (Handle, error)
	Start(ctx context.Context, h Handle) error
	Stop(ctx context.Context, h Handle, grace time.Duration) error
	Delete(ctx context.Context, h Handle) error
	Handle(name string) Handle
}

// ExecRequest describes a non-interactive command in an existing VM.
// The working directory is a guest path. Env contains explicit KEY=VALUE
// overrides; host environment variables are never copied. User accepts numeric
// IDs or supported local guest names; empty keeps the process default.
// Output is streamed to the supplied writers; nil writers discard output.
// Interactive sessions require a separate transport and are not supported by
// this interface.
type ExecRequest struct {
	Argv    []string
	Workdir string
	Env     []string
	User    string
	Stdout  io.Writer
	Stderr  io.Writer
}

// Executor is an optional runtime capability for non-interactive guest commands.
// A backend may return both an exit code and an error when it cannot distinguish
// a guest exit from a transport failure.
type Executor interface {
	Exec(ctx context.Context, h Handle, req ExecRequest) (int, error)
}

// NativeExecutor is implemented by runtimes that can run a guest process with
// an explicit user/environment through the native containerd API.
type NativeExecutor interface {
	ExecNative(ctx context.Context, h Handle, req ExecRequest) (int, error)
}

// ImageInspector exposes image configuration labels for Docker profile detection.
type ImageInspector interface {
	ImageLabels(ctx context.Context, image string) (map[string]string, error)
}

// ContextWriter writes output while honoring cancellation of ctx. Implementers
// must return promptly when ctx is canceled, even if the consumer stops reading.
type ContextWriter interface {
	WriteContext(ctx context.Context, p []byte) (int, error)
}

// CancelableExecutor requires output sinks that implement ContextWriter.
// It may reject other writers without executing the guest command.
type CancelableExecutor interface {
	ExecCancelable(ctx context.Context, h Handle, req ExecRequest) (int, error)
}

// TerminalSize specifies guest terminal dimensions.
type TerminalSize struct{ Rows, Cols uint32 }

// InteractiveExecRequest describes a bounded, cancelable guest session.
// Stdin must be closeable so cancellation can unblock a pending read.
type InteractiveExecRequest struct {
	ExecRequest
	Stdin  io.ReadCloser
	TTY    bool
	Size   TerminalSize
	Resize <-chan TerminalSize
}

// InteractiveExecutor is an optional runtime capability.
type InteractiveExecutor interface {
	ExecInteractive(ctx context.Context, h Handle, req InteractiveExecRequest) (int, error)
}
