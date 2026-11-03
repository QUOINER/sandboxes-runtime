package ctr

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

const taskRunning = "RUNNING"
const taskStopped = "STOPPED"

// Runner executes ctr commands. Tests can replace it with a fake.
type Runner interface {
	CombinedOutput(ctx context.Context, name string, args ...string) ([]byte, error)
}

type execRunner struct{}

func (execRunner) CombinedOutput(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...) // #nosec G204 -- ctr and its arguments are validated by this adapter
	return cmd.CombinedOutput()
}

// Client wraps a pinned containerd CLI.
type Client struct {
	cfg    Config
	runner Runner
}

// New creates a containerd CLI client.
func New(cfg Config) (*Client, error) {
	cfg, err := cfg.withDefaults()
	if err != nil {
		return nil, err
	}
	return &Client{cfg: cfg, runner: execRunner{}}, nil
}

// Config returns the effective client configuration.
func (c *Client) Config() Config { return c.cfg }

// Version checks that containerd is reachable.
func (c *Client) Version(ctx context.Context) error {
	_, err := c.run(ctx, "version")
	return err
}

// EnsureImage pulls image when it is not already present.
func (c *Client) EnsureImage(ctx context.Context, image string) error {
	if image == "" {
		return fmt.Errorf("image is required")
	}
	out, err := c.run(ctx, "images", "ls", "-q")
	if err != nil {
		return err
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) == image {
			return nil
		}
	}
	_, err = c.run(ctx, "images", "pull", "--platform", c.cfg.Platform, "--snapshotter", c.cfg.Snapshotter, image)
	if err != nil {
		return fmt.Errorf("pull %s: %w", image, err)
	}
	return nil
}

// ContainerExists reports whether a container exists.
func (c *Client) ContainerExists(ctx context.Context, id string) bool {
	found, _ := c.containerExists(ctx, id)
	return found
}

func (c *Client) containerExists(ctx context.Context, id string) (bool, error) {
	out, err := c.run(ctx, "containers", "ls", "-q")
	if err != nil {
		return false, err
	}
	return containsLine(out, id), nil
}

// TaskRunning reports whether a task is running.
func (c *Client) TaskRunning(ctx context.Context, id string) bool {
	status, _ := c.taskStatus(ctx, id)
	return status == taskRunning
}

func (c *Client) taskStatus(ctx context.Context, id string) (string, error) {
	out, err := c.run(ctx, "tasks", "ls")
	if err != nil {
		return "", err
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	header := strings.Fields(lines[0])
	if len(header) != 3 || header[0] != "TASK" || header[1] != "PID" || header[2] != "STATUS" {
		return "", fmt.Errorf("invalid task list response")
	}
	for _, line := range lines[1:] {
		fields := strings.Fields(line)
		if len(fields) != 3 {
			return "", fmt.Errorf("invalid task status response")
		}
		if fields[0] == id {
			return fields[2], nil
		}
	}
	return "", nil
}

// Mount describes a bind mount.
type Mount struct {
	Type        string
	Source      string
	Destination string
	ReadOnly    bool
}

// RunSpec describes a detached runtime task.
type RunSpec struct {
	ContainerID string
	Name        string
	Image       string
	CPUs        uint8
	MemoryMiB   uint32
	FIFODir     string
	Mounts      []Mount
	Env         []string
	Privileged  bool
	Annotations []string
	Args        []string
}

// Run creates and starts a detached container.
func (c *Client) Run(ctx context.Context, spec RunSpec) error {
	if spec.ContainerID == "" || spec.Image == "" {
		return fmt.Errorf("container id and image are required")
	}
	if spec.CPUs == 0 {
		spec.CPUs = 2
	}
	if spec.MemoryMiB == 0 {
		spec.MemoryMiB = 1024
	}
	args := []string{
		"run", "--detach",
		"--runtime", c.cfg.Runtime,
		"--snapshotter", c.cfg.Snapshotter,
		"--label", "io.rheic.sandbox=" + spec.Name,
		"--cpus", strconv.Itoa(int(spec.CPUs)),
		"--memory-limit", strconv.FormatUint(uint64(spec.MemoryMiB)*1024*1024, 10),
	}
	if spec.FIFODir != "" {
		args = append(args, "--fifo-dir", spec.FIFODir)
	}
	if spec.Privileged {
		args = append(args, "--cap-add", "CAP_SYS_ADMIN", "--cap-add", "CAP_NET_ADMIN", "--cap-add", "CAP_NET_RAW")
	}
	for _, annotation := range spec.Annotations {
		args = append(args, "--annotation", annotation)
	}
	for _, mount := range spec.Mounts {
		args = append(args, "--mount", mountSpec(mount))
	}
	for _, env := range spec.Env {
		args = append(args, "--env", env)
	}
	args = append(args, spec.Image, spec.ContainerID)
	if len(spec.Args) == 0 {
		args = append(args, "/bin/sh", "-c", "trap 'exit 0' TERM INT; while :; do sleep 3600; done")
	} else {
		args = append(args, spec.Args...)
	}
	_, err := c.run(ctx, args...)
	return err
}

// StartTask starts a stopped task.
func (c *Client) StartTask(ctx context.Context, id, fifoDir string) error {
	status, err := c.taskStatus(ctx, id)
	if err != nil {
		return fmt.Errorf("inspect task before start: %w", err)
	}
	switch status {
	case taskRunning:
		return nil
	case taskStopped:
		if err := c.DeleteTask(ctx, id); err != nil {
			return fmt.Errorf("remove stopped task before start: %w", err)
		}
	case "":
	default:
		return fmt.Errorf("cannot start task in state %q", status)
	}
	args := []string{"tasks", "start", "-d"}
	if fifoDir != "" {
		args = append(args, "--fifo-dir", fifoDir)
	}
	args = append(args, id)
	_, err = c.run(ctx, args...)
	return err
}

// StopTask terminates a task and waits for the grace period.
func (c *Client) StopTask(ctx context.Context, id string, grace time.Duration) error {
	status, err := c.taskStatus(ctx, id)
	if err != nil {
		return fmt.Errorf("inspect task: %w", err)
	}
	if status == "" {
		return nil
	}
	if status == taskStopped {
		return c.DeleteTask(ctx, id)
	}
	if status != taskRunning {
		return fmt.Errorf("cannot stop task in state %q", status)
	}
	if _, err := c.run(ctx, "tasks", "kill", "-s", "SIGTERM", id); err != nil {
		return fmt.Errorf("signal task: %w", err)
	}
	status, err = c.waitTaskStop(ctx, id, grace)
	if err != nil {
		return err
	}
	if status == taskRunning {
		if _, err := c.run(ctx, "tasks", "kill", "-s", "SIGKILL", id); err != nil {
			return fmt.Errorf("kill task: %w", err)
		}
	}
	if status == "" {
		return nil
	}
	if _, err := c.run(ctx, "tasks", "delete", "-f", id); err != nil {
		return fmt.Errorf("delete task: %w", err)
	}
	return nil
}

func (c *Client) waitTaskStop(ctx context.Context, id string, grace time.Duration) (string, error) {
	status := taskRunning
	deadline := time.Now().Add(grace)
	for time.Now().Before(deadline) {
		var err error
		status, err = c.taskStatus(ctx, id)
		if err != nil {
			return "", fmt.Errorf("inspect task: %w", err)
		}
		if status == "" || status == taskStopped {
			break
		}
		if status != taskRunning {
			return "", fmt.Errorf("cannot stop task in state %q", status)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	return status, nil
}

// DeleteTask removes a task if present.
func (c *Client) DeleteTask(ctx context.Context, id string) error {
	_, err := c.run(ctx, "tasks", "delete", "-f", id)
	return err
}

// DeleteContainer removes a container.
func (c *Client) DeleteContainer(ctx context.Context, id string) error {
	_, err := c.run(ctx, "containers", "delete", id)
	return err
}

func (c *Client) run(ctx context.Context, args ...string) (string, error) {
	full := append([]string{"--address", c.cfg.Socket, "--namespace", c.cfg.Namespace}, args...)
	out, err := c.runner.CombinedOutput(ctx, c.cfg.CTR, full...)
	if err != nil {
		return string(out), fmt.Errorf("%s %s: %w: %s", c.cfg.CTR, strings.Join(full, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

func containsLine(out, want string) bool {
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) == want {
			return true
		}
	}
	return false
}

func mountSpec(m Mount) string {
	if m.Type != "" && m.Type != "bind" {
		return fmt.Sprintf("type=%s,src=%s,dst=%s", m.Type, m.Source, m.Destination)
	}
	options := "rbind:rw"
	if m.ReadOnly {
		options = "rbind:ro"
	}
	return fmt.Sprintf("type=bind,src=%s,dst=%s,options=%s", m.Source, m.Destination, options)
}
