//go:build windows

package whpx

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuildArgsRequiresValidResourcesAndLoopback(t *testing.T) {
	artifacts := make([]string, 3)
	for i, name := range []string{"kernel", "initrd", "root.qcow2"} {
		path := filepath.Join(t.TempDir(), name)
		if err := os.WriteFile(path, []byte("test"), 0o600); err != nil {
			t.Fatal(err)
		}
		artifacts[i] = path
	}

	base := Config{
		KernelPath: artifacts[0], InitrdPath: artifacts[1], RootDiskPath: artifacts[2],
		NetworkEndpoint: "127.0.0.1:54321", GuestPipeName: "rheic-guest-pipe-0123456789",
		CPUs: 2, MemoryMiB: 1024,
	}
	valid, err := BuildArgs(base)
	if err != nil {
		t.Fatalf("BuildArgs(valid) error = %v", err)
	}
	if !containsPair(valid, "-machine", "q35,accel=whpx") {
		t.Fatalf("QEMU machine does not require WHPX: %v", valid)
	}
	if !containsPair(valid, "-netdev", "socket,id=net0,connect=127.0.0.1:54321") {
		t.Fatalf("QEMU network is not pinned to the loopback gateway: %v", valid)
	}
	if !containsPair(valid, "-chardev", "pipe,id=guest-agent,path="+base.GuestPipeName) {
		t.Fatalf("QEMU guest channel is missing: %v", valid)
	}

	tests := []struct {
		name   string
		mutate func(*Config)
	}{
		{name: "zero CPUs", mutate: func(c *Config) { c.CPUs = 0 }},
		{name: "too little memory", mutate: func(c *Config) { c.MemoryMiB = 511 }},
		{name: "missing kernel", mutate: func(c *Config) { c.KernelPath = filepath.Join(t.TempDir(), "missing") }},
		{name: "relative initrd", mutate: func(c *Config) { c.InitrdPath = "initrd" }},
		{name: "directory root disk", mutate: func(c *Config) { c.RootDiskPath = t.TempDir() }},
		{name: "wildcard network", mutate: func(c *Config) { c.NetworkEndpoint = "0.0.0.0:54321" }},
		{name: "invalid network port", mutate: func(c *Config) { c.NetworkEndpoint = "127.0.0.1:0" }},
		{name: "pipe traversal", mutate: func(c *Config) { c.GuestPipeName = `rheic-guest-..\pipe\target` }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := base
			tt.mutate(&cfg)
			if args, err := BuildArgs(cfg); err == nil {
				t.Fatalf("BuildArgs(%s) unexpectedly succeeded: %v", tt.name, args)
			}
		})
	}
}

func TestBuildArgsEncodesRootDiskPath(t *testing.T) {
	artifacts := make([]string, 3)
	for i := range artifacts {
		path := filepath.Join(t.TempDir(), "artifact")
		if err := os.WriteFile(path, []byte("test"), 0o600); err != nil {
			t.Fatal(err)
		}
		artifacts[i] = path
	}
	cfg := Config{
		KernelPath: artifacts[0], InitrdPath: artifacts[1], RootDiskPath: artifacts[2],
		NetworkEndpoint: "127.0.0.1:54321", GuestPipeName: "rheic-guest-pipe-0123456789",
		CPUs: 1, MemoryMiB: 512,
	}
	args, err := BuildArgs(cfg)
	if err != nil {
		t.Fatal(err)
	}
	blockdev := valueAfter(t, args, "-blockdev")
	var decoded map[string]any
	if err := json.Unmarshal([]byte(blockdev), &decoded); err != nil {
		t.Fatalf("blockdev is not valid JSON: %v", err)
	}
	file, ok := decoded["file"].(map[string]any)
	if !ok || file["filename"] != cfg.RootDiskPath {
		t.Fatalf("blockdev file = %#v, want exact root disk path %q", decoded["file"], cfg.RootDiskPath)
	}
	if strings.Contains(strings.Join(args, " "), "hostfwd=") {
		t.Fatal("unexpected user-mode networking or implicit host forwarding")
	}
	if got := valueAfter(t, args, "-machine"); got != "q35,accel=whpx" {
		t.Fatalf("machine = %q, want WHPX", got)
	}
}

func containsPair(args []string, key, value string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == key && args[i+1] == value {
			return true
		}
	}
	return false
}

func valueAfter(t *testing.T, args []string, key string) string {
	t.Helper()
	for i := 0; i+1 < len(args); i++ {
		if args[i] == key {
			return args[i+1]
		}
	}
	t.Fatalf("argument %q is missing from %v", key, args)
	return ""
}
