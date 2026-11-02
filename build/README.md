# Runtime build pins

This directory pins the native runtime inputs used by the containerd/nerdbox
adapter. The manifest is authoritative; the patches are checked by the
`internal/manifest` test.

## Components

- containerd `2.4.2`
- nerdbox `0.2.5`, commit `28c86e8e16c62a08079531ebe99e24a7bdad3d62`
- libkrun `1.19.6`
- gvisor-tap-vsock commit `0b0819c174331f5b86d96db3ab7aad7dd126078e`
- default image `docker.io/library/alpine:3.22` at the pinned digest

## Patches

The gvproxy patch is experimental until matching integration and security
tests are recorded in the plan:

- `patches/gvproxy-egress-policy-v2.patch` — default-deny egress policy,
  domain-aware DNS authorization with a bounded resolved-IP cache, revision
  reload, audit, flow revocation, and the supervisor owner lease. The daemon
  requires the patched `-require-owner-lease` option; older gvproxy builds are
  rejected at startup. Domain-only TCP authorization additionally requires a
  bounded TLS ClientHello with a matching SNI and DNS binding. Policy DNS A/AAAA
  lookups use the configured upstream, retry truncated UDP replies over TCP to
  that upstream, follow bounded CNAME chains and expire bindings at the minimum
  observed TTL. Encrypted Client Hello on the
  domain-only path, domain-only UDP, and domain denies overlapping a direct IP
  allow are rejected. Policy decisions, audit records, and flow tracking use
  the guest-visible destination evaluated before host NAT translation.
  Live VM evidence (2026-11-01): default deny, allowed domain fetch with a
  matching SNI, direct-IP connection without a matching name denied
  (`tls-name-denied`), non-TLS on a domain-only rule denied, DNS refusal
  after rule revocation, and reload revocation of active flows. HTTP Host
  and complete DNS-rebinding and bypass guarantees are not established; the
  patch remains experimental.
- `patches/nerdbox-gpt-threshold.patch` — raises nerdbox's GPT threshold from
  8 to 128 layers so multi-layer images stay on raw virtio EROFS disks instead
  of the GPT VMDK path that libkrun 1.19.6 rejects. Build the shim with
  `scripts/build-nerdbox-shim.sh`; the hypervisor entitlement is mandatory.

Nested Docker Engine remains blocked. The former nerdbox cgroup/capability
patch was removed because it applied broad capabilities and a writable bind of
the whole guest cgroup filesystem to every workload. Do not reintroduce it without an explicit
opt-in profile, least-privilege capability analysis, and guest-only cgroup
design validated against the pinned nerdbox build.

Patch SHA-256 values are recorded in `manifest.yaml` and checked by the
manifest test.

## Build outline

When producing the Linux guest EROFS image on macOS, specify a 4096-byte
block size explicitly. The macOS `mkfs.erofs` default can use 16384-byte
blocks, which the pinned guest kernel rejects during boot:

```sh
mkfs.erofs -b4096 -zlz4 /path/to/nerdbox-rootfs.erofs /path/to/guest-rootfs
```

The guest root filesystem must contain the vminitd built from the pinned
unmodified source. Do not use the removed broad cgroup/capability patch.

```sh
# gvproxy
git clone https://github.com/containers/gvisor-tap-vsock
cd gvisor-tap-vsock
git checkout 0b0819c174331f5b86d96db3ab7aad7dd126078e
git apply /path/to/build/patches/gvproxy-egress-policy-v2.patch
go build -o gvproxy-policy-v2 ./cmd/gvproxy
```

or use `scripts/build-gvproxy.sh` with `GVPROXY_SRC` and `OUT`.

The containerd process must find the patched shim on PATH before the stock
`containerd-shim-nerdbox-v1`; Klorq does not modify an existing containerd
installation. `scripts/build-nerdbox-shim.sh` builds and signs it.

Both native build scripts verify the source checkout's exact manifest commit
and reject dirty or untracked source trees before applying patches. Go builds
use `-trimpath`, disable VCS stamping, and clear the Go build ID so independent
build directories do not affect the unsigned payload digest. The nerdbox shim
is then ad-hoc signed for local macOS execution; that signature is not a
release signature or provenance statement. The runtime adapter must consume
these artifacts from explicit paths; it must not download `@latest`
components during a reproducible build.
