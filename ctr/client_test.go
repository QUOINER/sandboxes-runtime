package ctr

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	runtime "github.com/rheic/sandboxes-runtime"
)

type fakeRunner struct {
	calls [][]string
	fn    func(args []string) ([]byte, error)
}

func TestStartTaskHandlesExistingTask(t *testing.T) {
	for _, tc := range []struct {
		name       string
		status     string
		wantDelete bool
		wantStart  bool
		wantErr    bool
	}{
		{name: "absent", wantStart: true},
		{name: "running", status: "RUNNING"},
		{name: "stopped", status: "STOPPED", wantDelete: true, wantStart: true},
		{name: "paused", status: "PAUSED", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner := &fakeRunner{fn: func(args []string) ([]byte, error) {
				if strings.Contains(strings.Join(args, " "), "tasks ls") {
					result := "TASK PID STATUS\n"
					if tc.status != "" {
						result += "rheic-dev 12 " + tc.status + "\n"
					}
					return []byte(result), nil
				}
				return nil, nil
			}}
			err := newTestClient(t, runner).StartTask(context.Background(), "rheic-dev", "/tmp/fifo")
			if (err != nil) != tc.wantErr {
				t.Fatalf("unexpected error: %v", err)
			}
			calls := fmt.Sprint(runner.calls)
			if strings.Contains(calls, "tasks delete -f rheic-dev") != tc.wantDelete ||
				strings.Contains(calls, "tasks start -d --fifo-dir /tmp/fifo rheic-dev") != tc.wantStart {
				t.Fatalf("unexpected calls: %s", calls)
			}
		})
	}
}

func TestStartTaskStopsOnInspectionOrDeleteFailure(t *testing.T) {
	for _, failDelete := range []bool{false, true} {
		runner := &fakeRunner{fn: func(args []string) ([]byte, error) {
			if failDelete && strings.Contains(strings.Join(args, " "), "tasks ls") {
				return []byte("TASK PID STATUS\nrheic-dev 12 STOPPED\n"), nil
			}
			return nil, errors.New("containerd unavailable")
		}}
		if err := newTestClient(t, runner).StartTask(context.Background(), "rheic-dev", ""); err == nil {
			t.Fatal("failure was ignored")
		}
		if strings.Contains(fmt.Sprint(runner.calls), "tasks start") {
			t.Fatal("started after failure")
		}
	}
}

func TestStopTaskReportsContainerdFailures(t *testing.T) {
	for _, test := range []struct {
		name string
		fn   func([]string) ([]byte, error)
	}{
		{"list", func([]string) ([]byte, error) { return nil, errors.New("containerd unavailable") }},
		{"signal", func(args []string) ([]byte, error) {
			if strings.Contains(strings.Join(args, " "), "tasks ls") {
				return []byte("TASK PID STATUS\nrheic-dev 12 RUNNING\n"), nil
			}
			return nil, errors.New("signal failed")
		}},
		{"delete", func(args []string) ([]byte, error) {
			if strings.Contains(strings.Join(args, " "), "tasks ls") {
				return []byte("TASK PID STATUS\nrheic-dev 12 STOPPED\n"), nil
			}
			return nil, errors.New("delete failed")
		}},
		{"unknown state", func([]string) ([]byte, error) {
			return []byte("TASK PID STATUS\nrheic-dev 12 PAUSED\n"), nil
		}},
		{"malformed", func([]string) ([]byte, error) {
			return []byte("TASK PID STATUS\nrheic-dev\n"), nil
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := newTestClient(t, &fakeRunner{fn: test.fn})
			if err := client.StopTask(context.Background(), "rheic-dev", time.Second); err == nil {
				t.Fatal("failure was reported as successful stop")
			}
		})
	}
}

func TestStopTaskSignalsAndDeletesStoppedTask(t *testing.T) {
	lists := 0
	runner := &fakeRunner{fn: func(args []string) ([]byte, error) {
		if strings.Contains(strings.Join(args, " "), "tasks ls") {
			lists++
			if lists == 1 {
				return []byte("TASK PID STATUS\nrheic-dev 12 RUNNING\n"), nil
			}
			return []byte("TASK PID STATUS\nrheic-dev 12 STOPPED\n"), nil
		}
		return nil, nil
	}}
	client := newTestClient(t, runner)
	if err := client.StopTask(context.Background(), "rheic-dev", time.Second); err != nil {
		t.Fatal(err)
	}
	calls := make([]string, 0, len(runner.calls))
	for _, call := range runner.calls {
		calls = append(calls, strings.Join(call, " "))
	}
	joined := strings.Join(calls, "\n")
	if !strings.Contains(joined, "tasks kill -s SIGTERM rheic-dev") || !strings.Contains(joined, "tasks delete -f rheic-dev") || strings.Contains(joined, "SIGKILL") {
		t.Fatalf("unexpected stop commands: %s", joined)
	}
}

func TestCreateDoesNotProceedWhenContainerListFails(t *testing.T) {
	runner := &fakeRunner{fn: func(args []string) ([]byte, error) {
		if strings.Contains(strings.Join(args, " "), "images ls") {
			return []byte("alpine:3.22\n"), nil
		}
		return nil, errors.New("containerd unavailable")
	}}
	client := newTestClient(t, runner)
	backend := &Backend{client: client, image: "alpine:3.22"}
	if _, err := backend.Create(context.Background(), runtime.Config{Name: "dev"}); err == nil {
		t.Fatal("container list failure was ignored")
	}
	if len(runner.calls) != 2 {
		t.Fatalf("runtime continued after failed list: %d calls", len(runner.calls))
	}
}

func TestBackendDeleteAfterTaskHasStopped(t *testing.T) {
	runner := &fakeRunner{fn: func(args []string) ([]byte, error) {
		switch {
		case strings.Contains(strings.Join(args, " "), "tasks ls"):
			return []byte("TASK PID STATUS\n"), nil
		case strings.Contains(strings.Join(args, " "), "containers ls"):
			return []byte("rheic-dev\n"), nil
		case strings.Contains(strings.Join(args, " "), "containers delete"):
			return nil, nil
		default:
			return nil, errors.New("unexpected command")
		}
	}}
	backend := &Backend{client: newTestClient(t, runner)}
	if err := backend.Delete(context.Background(), runtime.Handle{ID: "rheic-dev"}); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 3 {
		t.Fatalf("unexpected delete sequence: %v", runner.calls)
	}
}

func TestBackendDeleteFailsWhenTaskListUnavailable(t *testing.T) {
	runner := &fakeRunner{fn: func([]string) ([]byte, error) {
		return nil, errors.New("containerd unavailable")
	}}
	backend := &Backend{client: newTestClient(t, runner)}
	if err := backend.Delete(context.Background(), runtime.Handle{ID: "rheic-dev"}); err == nil {
		t.Fatal("task inspection failure ignored")
	}
	if len(runner.calls) != 1 {
		t.Fatalf("delete proceeded after inspection failure: %v", runner.calls)
	}
}

func (f *fakeRunner) CombinedOutput(_ context.Context, name string, args ...string) ([]byte, error) {
	call := append([]string{name}, args...)
	f.calls = append(f.calls, call)
	if f.fn == nil {
		return nil, nil
	}
	return f.fn(call)
}

func newTestClient(t *testing.T, runner Runner) *Client {
	t.Helper()
	client, err := New(Config{Socket: "/tmp/containerd.sock", CTR: "ctr"})
	if err != nil {
		t.Fatal(err)
	}
	client.runner = runner
	return client
}

func TestRunBuildsCommand(t *testing.T) {
	runner := &fakeRunner{}
	client := newTestClient(t, runner)
	err := client.Run(context.Background(), RunSpec{
		ContainerID: "rheic-dev",
		Name:        "dev",
		Image:       "alpine:3.22",
		CPUs:        2,
		MemoryMiB:   512,
		FIFODir:     "/tmp/fifo",
		Mounts:      []Mount{{Source: "/work", Destination: "/work"}},
		Env:         []string{"PARITY=1"},
		Privileged:  true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 1 {
		t.Fatalf("calls = %d", len(runner.calls))
	}
	args := strings.Join(runner.calls[0], " ")
	for _, want := range []string{
		"ctr --address /tmp/containerd.sock --namespace rheic run --detach",
		"--runtime io.containerd.nerdbox.v1",
		"--snapshotter erofs",
		"--fifo-dir /tmp/fifo",
		"type=bind,src=/work,dst=/work,options=rbind:rw",
		"--env PARITY=1",
		"--cap-add CAP_SYS_ADMIN",
		"--cap-add CAP_NET_ADMIN",
		"--cap-add CAP_NET_RAW",
		"alpine:3.22 rheic-dev /bin/sh -c",
	} {
		if !strings.Contains(args, want) {
			t.Fatalf("missing %q in %q", want, args)
		}
	}
}

func TestEnsureImage(t *testing.T) {
	runner := &fakeRunner{fn: func(args []string) ([]byte, error) {
		if strings.Contains(strings.Join(args, " "), "images ls") {
			return []byte("alpine:3.22\n"), nil
		}
		return nil, nil
	}}
	client := newTestClient(t, runner)
	if err := client.EnsureImage(context.Background(), "alpine:3.22"); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 1 {
		t.Fatalf("unexpected pull: %d calls", len(runner.calls))
	}

	runner.calls = nil
	runner.fn = func(args []string) ([]byte, error) {
		if strings.Contains(strings.Join(args, " "), "images ls") {
			return nil, nil
		}
		if strings.Contains(strings.Join(args, " "), "images pull") {
			return nil, nil
		}
		return nil, errors.New("unexpected")
	}
	if err := client.EnsureImage(context.Background(), "alpine:3.22"); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 2 {
		t.Fatalf("calls = %d", len(runner.calls))
	}
}

func TestBackendCreate(t *testing.T) {
	runner := &fakeRunner{fn: func(args []string) ([]byte, error) {
		joined := strings.Join(args, " ")
		if strings.Contains(joined, "images ls") || strings.Contains(joined, "containers ls") {
			return nil, nil
		}
		return nil, nil
	}}
	client := newTestClient(t, runner)
	backend := &Backend{client: client, image: "alpine:3.22", fifoDir: "/tmp/fifo"}
	handle, err := backend.Create(context.Background(), runtime.Config{
		Name:      "dev",
		Workspace: "/work",
		CPUs:      2,
		MemoryMiB: 512,
		Env:       []string{"PARITY=1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(runner.calls[len(runner.calls)-1], " "), "--env PARITY=1") {
		t.Fatalf("env missing in %v", runner.calls)
	}
	if handle.ID != "rheic-dev" || handle.FIFODir != "/tmp/fifo" {
		t.Fatalf("unexpected handle: %+v", handle)
	}
}

func TestCapabilitiesFailClosed(t *testing.T) {
	backend := &Backend{}
	caps, err := backend.Capabilities(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if caps.Network || caps.IPv6 {
		t.Fatalf("network capabilities must stay off: %+v", caps)
	}
}

func TestBackendHandle(t *testing.T) {
	backend := &Backend{fifoDir: "/tmp/fifo"}
	handle := backend.Handle("dev")
	if handle.ID != "rheic-dev" || handle.FIFODir != "/tmp/fifo" {
		t.Fatalf("unexpected handle: %+v", handle)
	}
}

type fakeNetwork struct {
	started    int
	ports      []runtime.Port
	allow      []runtime.Rule
	deny       []runtime.Rule
	stopped    int
	stopErr    error
	stopCtxErr error
}

func (f *fakeNetwork) Start(_ context.Context, _ string, ports []runtime.Port, allow, deny []runtime.Rule) (runtime.NetworkAttachment, error) {
	f.started++
	f.ports = append([]runtime.Port(nil), ports...)
	f.allow = append([]runtime.Rule(nil), allow...)
	f.deny = append([]runtime.Rule(nil), deny...)
	return runtime.NetworkAttachment{Annotations: []string{"io.containerd.nerdbox.network.0=test"}}, nil
}
func (f *fakeNetwork) UpdatePolicy(context.Context, string, []runtime.Port, []runtime.Rule, []runtime.Rule, int) error {
	return nil
}

func (f *fakeNetwork) UpdatePorts(context.Context, string, []runtime.Port) error { return nil }

func (f *fakeNetwork) Stop(ctx context.Context, _ string) error {
	f.stopped++
	f.stopCtxErr = ctx.Err()
	return f.stopErr
}

func TestCreateReportsNetworkCleanupFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runErr := errors.New("synthetic run failure")
	cleanupErr := errors.New("synthetic network cleanup failure")
	runner := &fakeRunner{fn: func(args []string) ([]byte, error) {
		if strings.Contains(strings.Join(args, " "), " run --detach") {
			cancel()
			return nil, runErr
		}
		return nil, nil
	}}
	network := &fakeNetwork{stopErr: cleanupErr}
	backend := &Backend{client: newTestClient(t, runner), image: "alpine:3.22", network: network}
	_, err := backend.Create(ctx, runtime.Config{Name: "dev"})
	if !errors.Is(err, runErr) || !errors.Is(err, cleanupErr) {
		t.Fatalf("runtime and cleanup errors not preserved: %v", err)
	}
	if network.stopped != 1 || network.stopCtxErr != nil {
		t.Fatalf("network cleanup did not use a live context: %+v", network)
	}
}

func TestStopReportsBothTaskAndNetworkFailures(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	taskErr := errors.New("synthetic task inspection failure")
	networkErr := errors.New("synthetic network stop failure")
	runner := &fakeRunner{fn: func([]string) ([]byte, error) { return nil, taskErr }}
	network := &fakeNetwork{stopErr: networkErr}
	backend := &Backend{client: newTestClient(t, runner), network: network}
	err := backend.Stop(ctx, runtime.Handle{ID: "rheic-dev"}, time.Second)
	if !errors.Is(err, taskErr) || !errors.Is(err, networkErr) {
		t.Fatalf("one of the stop failures was hidden: %v", err)
	}
	if network.stopped != 1 || network.stopCtxErr != nil {
		t.Fatalf("network cleanup did not use a live context: %+v", network)
	}
}

func TestBackendNetworkAnnotations(t *testing.T) {
	runner := &fakeRunner{fn: func(args []string) ([]byte, error) {
		if strings.Contains(strings.Join(args, " "), "tasks ls") {
			return []byte("TASK PID STATUS\n"), nil
		}
		return nil, nil
	}}
	client := newTestClient(t, runner)
	network := &fakeNetwork{}
	backend := &Backend{client: client, image: "alpine:3.22", network: network}
	handle, err := backend.Create(context.Background(), runtime.Config{
		Name: "dev", Ports: []runtime.Port{{Host: 8080, Guest: 3000, Protocol: "tcp"}},
		Allow: []runtime.Rule{{Protocol: "tcp", CIDR: "0.0.0.0/0", Port: 80}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if network.started != 1 || len(network.ports) != 1 || network.ports[0].Host != 8080 ||
		len(network.allow) != 1 || network.allow[0].Port != 80 {
		t.Fatalf("network starts=%d ports=%+v allow=%+v", network.started, network.ports, network.allow)
	}
	joined := strings.Join(runner.calls[len(runner.calls)-1], " ")
	if !strings.Contains(joined, "io.containerd.nerdbox.network.0=test") {
		t.Fatalf("missing network annotation: %s", joined)
	}
	caps, err := backend.Capabilities(context.Background())
	if err != nil || !caps.Network {
		t.Fatalf("network capability: %+v, %v", caps, err)
	}
	if err := backend.Stop(context.Background(), handle, 0); err != nil {
		t.Fatal(err)
	}
	if network.stopped != 1 {
		t.Fatalf("network stops = %d", network.stopped)
	}
}

func TestCreateRemovesContainerWhenRunFails(t *testing.T) {
	listings := 0
	runner := &fakeRunner{fn: func(args []string) ([]byte, error) {
		joined := strings.Join(args, " ")
		switch {
		case strings.Contains(joined, "run --detach"):
			return []byte("failed format"), errors.New("exit status 1")
		case strings.Contains(joined, "containers ls -q"):
			listings++
			if listings == 1 {
				return nil, nil // pre-create check: name is free
			}
			return []byte("rheic-dev\n"), nil // ctr left a partial container
		}
		return nil, nil
	}}
	backend, err := NewBackend(Config{Socket: "/tmp/containerd.sock", CTR: "ctr"}, BackendOptions{Image: "img"})
	if err != nil {
		t.Fatal(err)
	}
	backend.client.runner = runner
	if _, err := backend.Create(context.Background(), runtime.Config{Name: "dev", Image: "img"}); err == nil {
		t.Fatal("failed run returned no error")
	}
	calls := fmt.Sprint(runner.calls)
	if !strings.Contains(calls, "containers delete rheic-dev") {
		t.Fatalf("partial container was not removed: %s", calls)
	}
}

func TestCreatePassesProxyChannelAnnotation(t *testing.T) {
	runner := &fakeRunner{}
	backend, err := NewBackend(Config{Socket: "/tmp/containerd.sock", CTR: "ctr"}, BackendOptions{Image: "img"})
	if err != nil {
		t.Fatal(err)
	}
	backend.client.runner = runner
	proxySocket := filepath.Join(os.TempDir(), "dev.proxy.sock")
	if _, err := backend.Create(context.Background(), runtime.Config{Name: "dev", Image: "img", ProxyPort: 1027, ProxySocket: proxySocket}); err != nil {
		t.Fatal(err)
	}
	calls := fmt.Sprint(runner.calls)
	wantAnnotation := fmt.Sprintf("io.rheic.proxy=port=1027,socket=%s", proxySocket)
	if !strings.Contains(calls, wantAnnotation) {
		t.Fatalf("proxy annotation missing: %s", calls)
	}
}

func TestCreateMountsAndStartsProxyHelper(t *testing.T) {
	runner := &fakeRunner{}
	backend, err := NewBackend(Config{Socket: "/tmp/containerd.sock", CTR: "ctr"}, BackendOptions{Image: "img"})
	if err != nil {
		t.Fatal(err)
	}
	backend.client.runner = runner
	helper := filepath.Join(t.TempDir(), "rheic-proxy-helper")
	if err := os.WriteFile(helper, []byte("synthetic helper"), 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := runtime.Config{Name: "dev", Image: "img", ProxyPort: 1027, ProxySocket: filepath.Join(os.TempDir(), "dev.proxy.sock")}
	if _, err := backend.CreateWithProxy(context.Background(), &cfg, helper, 3128, "api.openai.com"); err != nil {
		t.Fatal(err)
	}
	calls := fmt.Sprint(runner.calls)
	for _, want := range []string{
		"type=bind,src=" + helper + ",dst=/tmp/rheic-proxy-helper,options=rbind:ro",
		"/tmp/rheic-proxy-helper -listen 127.0.0.1:3128 -cid 2 -port 1027 -service-host api.openai.com",
	} {
		if !strings.Contains(calls, want) {
			t.Fatalf("missing %q in runtime call: %s", want, calls)
		}
	}
}

func TestProxyHelperRejectsShellMetacharactersInHost(t *testing.T) {
	for _, host := range []string{"api.openai.com; touch /tmp/owned", "api.openai.com:443", " api.openai.com"} {
		if validProxyServiceHost(host) {
			t.Fatalf("accepted unsafe service host %q", host)
		}
	}
}
