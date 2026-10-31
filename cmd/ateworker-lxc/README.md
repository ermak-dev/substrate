# Experimental LXC worker

This worker registers class `lxc` and its runtime attributes through the existing
atelet protocol. It runs one actor per worker, with one LXC container per OCI
application container. Checkpoint and restore return `Unimplemented`; suspend
must be rejected by the control plane before it changes a live LXC actor.

The worker composes the OCI bundle rootfs with `imagecache`, joins the actor's
private veth network, and activates ingress after the configured wakeup probes.
A small PID 1 helper applies OCI argv, environment, cwd and user inside the
container, then uses `exec` to replace itself. Systemd remains PID 1. Actor
environment values never enter the host LXC configuration or host process env.

Termination stops LXC, kills remaining actor-cgroup processes, closes atunnel
listeners and namespaces, unmounts bundle overlays, and removes runtime state.
A failed cleanup retains the worker's slot. Startup cleans recorded actors
before registration and reports them crashed. Unexpected monitor exit also
cleans the actor and reports its UID through WorkerSupport.

Stats read the actor's cgroup, including its LXC monitors and all containers.
Samples carry actor and template attribution and a per-activation epoch. Initial,
periodic and final usage records use the existing `ate.actor.usage_sampled`
stream.

LXC shares the node kernel. Its broad capabilities and unconfined AppArmor and
seccomp profiles are unsuitable for hostile multi-tenancy. These permissions
apply only to `lxc`; the gVisor and microVM pod configurations are unchanged.
No KVM device, host network, or host PID namespace is needed. LXC does not
apply the OCI capability allowlist: nested systemd and Docker need the broader
LXC capability set. The worker's explicit pod capabilities still bound it.

## Nested Docker and kind

Trusted pools can opt into additional worker capabilities with the WorkerPool
metadata label `ate.dev/lxc-nesting: enabled`. Set it in `metadata.labels`, not
`spec.template.labels`, where the `ate.dev` domain is reserved. Removing the
label restores the ordinary LXC profile through a worker Deployment rollout.
Changing this label rolls the pool's workers; plan for actor eviction.

The profile adds only `FSETID`, `KILL`, `AUDIT_WRITE`, and `SYSLOG` to the ordinary
LXC capability set. The first three complete Docker's default container
capabilities in the worker's bounding set. `SYSLOG` is required to read the
shared kernel log. The profile also requests `ate.dev/kmsg`, which atelet
advertises where `/dev/kmsg` exists. Kubelet maps that device read-only and
adds its device-cgroup allow rule; a capability or hostPath mount alone cannot
grant device access. A node without the resource cannot schedule this profile.
It does not add `IPC_LOCK` or permit loading kernel modules.
The pod remains unprivileged, with the same namespaces, AppArmor and seccomp
settings. The controller passes `--actor-pids-limit=4096` for this profile;
ordinary LXC workers keep the default actor cgroup limit of 512 tasks. The worker
accepts limits from 1 through 4096. CPU and memory limits are unchanged.
The nesting profile requires a rebuilt worker image that accepts this flag.
Update the pool's worker image before enabling the profile with a new controller.
Ordinary LXC pools and other sandbox classes receive no new flag.

Any value other than `enabled`, including an empty value, or use on a non-LXC
pool fails reconciliation before changing the Deployment. Remove the label to
disable the profile. Admission does not validate this metadata label.

The profile does not apply OCI process capability allowlists or configure
actor-local devpts and Docker storage. Those remain separate runtime and
workbench concerns.

## Build

From a checkout with Go and Docker, run:

```sh
cmd/ateworker-lxc/build.sh ateworker-lxc:local
```

The script builds for the host architecture on Linux or macOS. The image
contains LXC and its userspace dependencies. It is separate from the ko-built
release images and is not deployed by the standard installer. The base image
is pinned by digest. The build does not push an image or change a cluster.

Before deployment, publish the image and set `WorkerPool.spec.workerImage` to
its registry digest. Select `lxc` in `spec.sandboxClasses` and reference the
`lxc-image` SandboxConfig in this directory. No class default is installed.

## Runtime metadata

`sandboxconfig.yaml` lists supported architectures with empty asset maps. LXC
runs from the worker image and uses the node kernel, so atelet must not fetch a
runsc binary, guest kernel, or firmware. Admission rejects nonempty asset maps.
The runtime attributes contain the architecture and the empty bundle's hash.
They do not identify the installed LXC package version or promise snapshot
compatibility. The worker image digest identifies its userspace.

Suspend must be protected by the checkpoint precondition before enabling this
class. LXC templates must use `GOLDEN_SNAPSHOT_MODE_DISABLED` and select the LXC
pool through worker selectors. There is no memory checkpoint or restore path.

## Verification

Unit tests run with the repository suite. The physical backend also has isolated
Docker tests:

```sh
cmd/ateworker-lxc/testdata/run.sh
```

The script builds the race-enabled test binary in a pinned Linux Go image, so
it does not require a Linux C cross-compiler on macOS. It uses a private cgroup
namespace and the LXC capability set, without
privileged mode or host mounts. Actor files use container-local tmpfs because
Docker's overlay root cannot be used as an overlay upperdir. The integration
build tag is `lxc_integration`; selecting it outside this harness fails closed.
The tests run systemd and verify terminate, crash reporting, partial startup,
recovery, overlay rootfs and a read-only volume with `-count=3 -shuffle=on`.
They do not exercise Kubernetes registration, PVE placement or the full gateway
path. Those require runtime e2e coverage.
