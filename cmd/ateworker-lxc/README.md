# Experimental LXC worker

This worker registers class `lxc` and its runtime attributes through the existing
atelet protocol. It reports **zero actor capacity**. Run, checkpoint, restore,
and terminate are unimplemented. Workload lifecycle support is a separate change.

LXC shares the node kernel. Its broad capabilities and unconfined AppArmor and
seccomp profiles are unsuitable for hostile multi-tenancy. These permissions
apply only to `lxc`; the gVisor and microVM pod configurations are unchanged.
No KVM device, host network, or host PID namespace is needed.

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
