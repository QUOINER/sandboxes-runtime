package ctr

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	runtime "github.com/rheic/sandboxes-runtime"
)

// BackendOptions configures the runtime backend.
type BackendOptions struct {
	Image   string
	FIFODir string
	Network runtime.Network
}

// Backend implements the runtime.Backend contract with ctr.
type Backend struct {
	client  *Client
	image   string
	fifoDir string
	network runtime.Network
}

// CreateWithProxy configures the optional guest helper while preserving the
// stable runtime.Backend contract for consumers pinned to older releases.
func (b *Backend) CreateWithProxy(ctx context.Context, cfg *runtime.Config, helperPath string, guestPort uint16, serviceHost string) (runtime.Handle, error) {
	cfg.ProxyHelperPath = helperPath
	cfg.ProxyGuestPort = guestPort
	cfg.ProxyServiceHost = serviceHost
	return b.Create(ctx, *cfg)
}

var _ runtime.Backend = (*Backend)(nil)

// NewBackend creates a containerd-backed runtime.
func NewBackend(cfg Config, opts BackendOptions) (*Backend, error) {
	client, err := New(cfg)
	if err != nil {
		return nil, err
	}
	return &Backend{client: client, image: opts.Image, fifoDir: opts.FIFODir, network: opts.Network}, nil
}

// Capabilities reports the current adapter capabilities.
func (b *Backend) Capabilities(context.Context) (runtime.Capabilities, error) {
	return runtime.Capabilities{
		Workspace: true,
		Network:   b.network != nil,
		Snapshots: false,
		GPU:       false,
		IPv6:      false,
	}, nil
}

// Create creates and starts a detached nerdbox task.
//
//nolint:gocritic // The value signature is required by runtime.Backend.
func (b *Backend) Create(ctx context.Context, cfg runtime.Config) (runtime.Handle, error) {
	image := cfg.Image
	if image == "" {
		image = b.image
	}
	fifoDir := cfg.FIFODir
	if fifoDir == "" {
		fifoDir = b.fifoDir
	}
	if err := b.client.EnsureImage(ctx, image); err != nil {
		return runtime.Handle{}, err
	}
	id := ContainerID(cfg.Name)
	exists, err := b.client.containerExists(ctx, id)
	if err != nil {
		return runtime.Handle{}, fmt.Errorf("inspect container: %w", err)
	}
	if exists {
		return runtime.Handle{}, fmt.Errorf("container %s already exists", id)
	}
	var attachment runtime.NetworkAttachment
	if b.network != nil {
		var err error
		attachment, err = b.network.Start(ctx, cfg.Name, cfg.Ports, cfg.Allow, cfg.Deny)
		if err != nil {
			return runtime.Handle{}, err
		}
	}
	spec := RunSpec{
		ContainerID: id,
		Name:        cfg.Name,
		Image:       image,
		CPUs:        cfg.CPUs,
		MemoryMiB:   cfg.MemoryMiB,
		FIFODir:     fifoDir,
		Env:         append([]string(nil), cfg.Env...),
		Privileged:  cfg.Privileged,
		Annotations: append([]string(nil), attachment.Annotations...),
	}
	if err := applyProxyConfig(&spec, &cfg); err != nil {
		return runtime.Handle{}, err
	}
	for _, volume := range cfg.BlockVolumes {
		spec.Mounts = append(spec.Mounts, Mount{Type: "ext4", Source: volume.Source, Destination: volume.Destination})
	}
	workspaces := cfg.Workspaces
	if len(workspaces) == 0 && cfg.Workspace != "" {
		workspaces = []runtime.Workspace{{Path: cfg.Workspace}}
	}
	for _, workspace := range workspaces {
		spec.Mounts = append(spec.Mounts, Mount{Source: workspace.Path, Destination: workspace.Path, ReadOnly: workspace.ReadOnly})
	}
	if err := b.client.Run(ctx, spec); err != nil {
		cleanupErr := b.cleanupFailedCreate(ctx, id)
		if b.network != nil {
			cleanupErr = errors.Join(cleanupErr, b.stopNetwork(ctx, cfg.Name))
		}
		return runtime.Handle{}, errors.Join(err, cleanupErr)
	}
	return runtime.Handle{ID: id, FIFODir: fifoDir}, nil
}

func applyProxyConfig(spec *RunSpec, cfg *runtime.Config) error {
	if err := applyProxyChannel(spec, cfg); err != nil {
		return err
	}
	return applyProxyHelper(spec, cfg)
}

func applyProxyHelper(spec *RunSpec, cfg *runtime.Config) error {
	if cfg.ProxyHelperPath == "" {
		if cfg.ProxyGuestPort != 0 || cfg.ProxyServiceHost != "" {
			return errors.New("proxy helper options require a helper binary path")
		}
		return nil
	}
	if cfg.ProxyPort < 1027 || cfg.ProxySocket == "" || !filepath.IsAbs(cfg.ProxyHelperPath) || strings.ContainsAny(cfg.ProxyHelperPath, ",=\x00\r\n") || cfg.ProxyGuestPort < 1024 || !validProxyServiceHost(cfg.ProxyServiceHost) {
		return errors.New("proxy helper requires a valid channel, guest port and service host")
	}
	spec.Mounts = append(spec.Mounts, Mount{
		Source:      cfg.ProxyHelperPath,
		Destination: "/tmp/rheic-proxy-helper",
		ReadOnly:    true,
	})
	spec.Args = []string{
		"/bin/sh", "-c",
		fmt.Sprintf("set -eu; /tmp/rheic-proxy-helper -listen 127.0.0.1:%d -cid 2 -port %d -service-host %s & helper=$!; trap 'kill \"$helper\" 2>/dev/null || true; wait \"$helper\" 2>/dev/null || true' TERM INT; while kill -0 \"$helper\" 2>/dev/null; do sleep 1; done; wait \"$helper\"", cfg.ProxyGuestPort, cfg.ProxyPort, cfg.ProxyServiceHost),
	}
	return nil
}

func validProxyServiceHost(host string) bool {
	if host == "" || strings.HasPrefix(host, ".") || strings.HasSuffix(host, ".") {
		return false
	}
	for _, char := range host {
		if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') ||
			(char >= '0' && char <= '9') || char == '.' || char == '-' {
			continue
		}
		return false
	}
	return true
}

// cleanupFailedCreate removes a container that ctr created before its run
// failed. Without it a partial container keeps the sandbox name reserved and
// every retry fails with "container already exists".
func (b *Backend) cleanupFailedCreate(ctx context.Context, id string) error {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	exists, err := b.client.containerExists(cleanupCtx, id)
	if err != nil || !exists {
		return err
	}
	// A failed run may leave a task behind; ignore its absence.
	_ = b.client.DeleteTask(cleanupCtx, id)
	if err := b.client.DeleteContainer(cleanupCtx, id); err != nil {
		return fmt.Errorf("remove container after failed create: %w", err)
	}
	return nil
}

func applyProxyChannel(spec *RunSpec, cfg *runtime.Config) error {
	annotation, err := proxyChannelAnnotation(cfg)
	if err != nil {
		return err
	}
	if annotation != "" {
		spec.Annotations = append(spec.Annotations, annotation)
	}
	return nil
}

// proxyChannelAnnotation renders the shim annotation for a guest-dialed
// vsock channel or returns an empty string when the channel is disabled.
func proxyChannelAnnotation(cfg *runtime.Config) (string, error) {
	if cfg.ProxyPort == 0 && cfg.ProxySocket == "" {
		return "", nil
	}
	if cfg.ProxyPort < 1027 || cfg.ProxyPort > 65535 || !filepath.IsAbs(cfg.ProxySocket) || len(cfg.ProxySocket) >= 104 || strings.ContainsAny(cfg.ProxySocket, ",=\x00\r\n") {
		return "", errors.New("proxy channel requires a port >= 1027 and a socket path")
	}
	return fmt.Sprintf("io.rheic.proxy=port=%d,socket=%s", cfg.ProxyPort, cfg.ProxySocket), nil
}

// Handle returns the runtime handle for a sandbox name.
func (b *Backend) Handle(name string) runtime.Handle {
	return runtime.Handle{ID: ContainerID(name), FIFODir: b.fifoDir}
}

// Start starts a stopped task.
func (b *Backend) Start(ctx context.Context, h runtime.Handle) error {
	if b.network != nil {
		if _, err := b.network.Start(ctx, strings.TrimPrefix(h.ID, "rheic-"), h.Ports, h.Allow, h.Deny); err != nil {
			return err
		}
	}
	if err := b.client.StartTask(ctx, h.ID, h.FIFODir); err != nil {
		if b.network != nil {
			return errors.Join(err, b.stopNetwork(ctx, strings.TrimPrefix(h.ID, "rheic-")))
		}
		return err
	}
	return nil
}

func (b *Backend) stopNetwork(ctx context.Context, name string) error {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err := b.network.Stop(cleanupCtx, name); err != nil {
		return fmt.Errorf("stop sandbox network: %w", err)
	}
	return nil
}

// Stop terminates a task.
func (b *Backend) UpdatePolicy(ctx context.Context, h runtime.Handle, allow, deny []runtime.Rule, revision int) error {
	if b.network == nil {
		return errors.New("runtime has no network policy engine")
	}
	return b.network.UpdatePolicy(ctx, strings.TrimPrefix(h.ID, "rheic-"), h.Ports, allow, deny, revision)
}

func (b *Backend) UpdatePorts(ctx context.Context, h runtime.Handle, ports []runtime.Port) error {
	if b.network == nil {
		return errors.New("runtime has no network port engine")
	}
	return b.network.UpdatePorts(ctx, strings.TrimPrefix(h.ID, "rheic-"), ports)
}

func (b *Backend) Stop(ctx context.Context, h runtime.Handle, grace time.Duration) error {
	if grace <= 0 {
		grace = 5 * time.Second
	}
	err := b.client.StopTask(ctx, h.ID, grace)
	if b.network != nil {
		err = errors.Join(err, b.stopNetwork(ctx, strings.TrimPrefix(h.ID, "rheic-")))
	}
	return err
}

// Delete removes the task and container.
func (b *Backend) Delete(ctx context.Context, h runtime.Handle) error {
	status, err := b.client.taskStatus(ctx, h.ID)
	if err == nil && status != "" {
		err = b.client.DeleteTask(ctx, h.ID)
	}
	if err == nil {
		var exists bool
		exists, err = b.client.containerExists(ctx, h.ID)
		if err == nil && exists {
			err = b.client.DeleteContainer(ctx, h.ID)
		}
	}
	if b.network != nil {
		err = errors.Join(err, b.stopNetwork(ctx, strings.TrimPrefix(h.ID, "rheic-")))
	}
	return err
}

// ContainerID returns the containerd container id for a sandbox name.
func ContainerID(name string) string { return "rheic-" + name }
