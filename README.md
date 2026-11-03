# sandboxes-runtime

## Import path migration

The canonical Go module path is now `github.com/lickren/sandboxes-runtime` (previously `github.com/quoiner/sandboxes-runtime`). Update imports and `go.mod` requirements when moving to a release that includes this change. The old module path is not kept as a compatibility alias.

`runtime.Config` supports the additive per-VM service-proxy channel. A proxy
helper is mounted read-only at `/tmp/lickren-proxy-helper` and started by an
`/bin/sh` init wrapper; the helper accepts only the configured upstream host and
talks to the host broker over the shim's vsock bridge. The helper path must be
a static Linux ARM64 binary supplied by the daemon. Runtime users that leave
`ProxyHelperPath` empty retain the existing command path.

Status: pre-alpha.

Runtime adapters for containerd/nerdbox, VM lifecycle, workspace mounts, OCI
images, templates, and kits. Implements ports defined by sandboxes-core.

## Boundaries

- does not expose daemon or SDK types;
- does not make network policy decisions;
- host Docker socket is never mounted.

## Runtime pins

`build/manifest.yaml` pins containerd, nerdbox, libkrun, gvisor-tap-vsock,
patches, and the default image digest. Validate the checked-in inputs with:

```sh
make runtime-verify
```

## containerd adapter

`ctr` is the first lifecycle adapter. It uses a pinned containerd CLI and
implements the `runtime.Backend` contract: image pull, detached run, task
start/stop/delete, and workspace bind mounts.

Current limits are explicit:

- `Capabilities.Network` is `false` unless a `runtime.Network` implementation is supplied;
- network annotations are wired through `runtime.Network`; the daemon supplies the gateway manager;
- non-interactive and interactive exec use separate adapter capabilities;
  native execution requires cancellation-aware output and interactive input
  requires an owned, closeable stream;
- local guest user/group name resolution depends on guest tools and is not
  a general NSS implementation; full signal and detach-key behavior is open;
- no snapshots or GPU;
- live VM validation is platform-specific and does not establish cross-platform
  parity; fault and independent security reviews remain open.

## Windows WHPX work in progress

`whpx.BuildArgs` builds a restricted QEMU argv for a Linux kernel/initrd/root
disk, forces `accel=whpx`, and permits the QEMU socket-network backend to
connect only to IPv4 loopback. `whpx.Runner` starts and tracks QEMU in
kill-on-close Windows Job Objects. These APIs are not wired into daemon
composition and do not implement `runtime.Backend`. Windows host support
remains unavailable until the project supplies and integrates owned disk/image
lifecycle, the guest control and workspace protocols, a private gateway API/
lease transport, and fail-closed cleanup. Do not use this low-level runtime
layer as evidence of a bootable or isolated sandbox.
