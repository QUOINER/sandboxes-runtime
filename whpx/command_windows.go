//go:build windows

// Package whpx provides low-level Windows WHPX process primitives. It does not
// implement runtime.Backend or the guest, workspace, and gateway protocols.
package whpx

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Config describes the immutable inputs for one Linux guest boot.
type Config struct {
	KernelPath      string
	InitrdPath      string
	RootDiskPath    string
	NetworkEndpoint string
	GuestPipeName   string
	CPUs            uint8
	MemoryMiB       uint32
}

// BuildArgs returns a deterministic QEMU argv for an x86_64 Linux guest on a
// Windows host. The caller must still arrange a private gateway endpoint and
// an owned process lifecycle before starting QEMU.
func BuildArgs(cfg Config) ([]string, error) {
	if cfg.CPUs == 0 || cfg.MemoryMiB < 512 {
		return nil, errors.New("WHPX requires at least one CPU and 512 MiB memory")
	}
	for _, path := range []string{cfg.KernelPath, cfg.InitrdPath, cfg.RootDiskPath} {
		if err := validateArtifact(path); err != nil {
			return nil, err
		}
	}
	endpoint, err := validateLoopbackEndpoint(cfg.NetworkEndpoint)
	if err != nil {
		return nil, err
	}
	if !validPipeName(cfg.GuestPipeName) {
		return nil, errors.New("WHPX guest pipe name is invalid")
	}
	blockdev, err := json.Marshal(map[string]any{
		"driver":    "qcow2",
		"node-name": "drive0",
		"file": map[string]any{
			"driver":   "file",
			"filename": cfg.RootDiskPath,
			"aio":      "threads",
		},
	})
	if err != nil {
		return nil, fmt.Errorf("encode root disk configuration: %w", err)
	}
	return []string{
		"-nodefaults", "-no-user-config", "-no-reboot",
		"-machine", "q35,accel=whpx",
		"-cpu", "max",
		"-smp", strconv.Itoa(int(cfg.CPUs)),
		"-m", strconv.FormatUint(uint64(cfg.MemoryMiB), 10) + "M",
		"-kernel", cfg.KernelPath,
		"-initrd", cfg.InitrdPath,
		"-append", "console=ttyS0 root=/dev/vda rw panic=-1",
		"-blockdev", string(blockdev),
		"-device", "virtio-blk-pci,drive=drive0",
		"-netdev", "socket,id=net0,connect=" + endpoint,
		"-device", "virtio-net-pci,netdev=net0",
		"-device", "virtio-serial-pci,id=guest-serial",
		"-chardev", "pipe,id=guest-agent,path=" + cfg.GuestPipeName,
		"-device", "virtserialport,chardev=guest-agent,name=org.rheic.guest.0",
		"-display", "none",
		"-serial", "null",
		"-monitor", "none",
	}, nil
}

func validPipeName(name string) bool {
	if len(name) < 24 || len(name) > 128 {
		return false
	}
	for _, r := range name {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '-' && r != '_' {
			return false
		}
	}
	return true
}

func validateArtifact(path string) error {
	if !filepath.IsAbs(path) || strings.IndexByte(path, 0) >= 0 {
		return errors.New("WHPX artifact paths must be absolute")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("inspect WHPX artifact: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("WHPX artifacts must be regular non-symlink files")
	}
	return nil
}

func validateLoopbackEndpoint(endpoint string) (string, error) {
	host, portText, err := net.SplitHostPort(endpoint)
	if err != nil || host != "127.0.0.1" {
		return "", errors.New("WHPX gateway endpoint must use IPv4 loopback")
	}
	port, err := strconv.ParseUint(portText, 10, 16)
	if err != nil || port == 0 {
		return "", errors.New("WHPX gateway endpoint must use a valid TCP port")
	}
	return net.JoinHostPort(host, strconv.FormatUint(port, 10)), nil
}
