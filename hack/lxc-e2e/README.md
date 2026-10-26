# LXC API e2e stand

This lane uses an isolated kind cluster and registry. It does not install tools
or change PVE, host sysctls, firewall or CNI configuration. The kind node has its
own Docker network and is capped at 8 CPUs and 16 GiB. Existing clusters and
containers are not reused. Kubernetes and registry images are digest-pinned.

Prerequisites: Docker, Go, kind, kubectl, ko, and built `ate-setup` and
`kubectl-ate` on PATH. Tools can live under the stand directory. The installer
builds the fork's control plane and a separate LXC worker image from this source.

```sh
hack/lxc-e2e/create.sh /absolute/stand/cluster
hack/lxc-e2e/install.sh /absolute/stand
hack/lxc-e2e/run.sh /absolute/stand
```

Use a fresh stand directory. The scripts refuse existing test containers or an
existing kubeconfig. Installer and tests always name the private kubeconfig and
`kind-lxc-e2e-7` context, never the caller's default context.

The dedicated suite covers real API create/resume, router ingress, allowed and
forbidden gateway egress to the same origin, private actor network addresses,
suspend refusal without changing the live actor, CSI-backed storage versus
cold-reset rootfs, crash, worker-pod recreation and deletion without disturbing
another actor. Worker replacement uses an explicit cold restart. It does not
claim node-failure recovery or restoration of memory.

CI must select `E2E_SANDBOX_CLASS=lxc` and run the suite with `--e2e`. A selected
CI lane without that flag fails through a named predicate. Missing pinned worker
image, SandboxConfig, CSI StorageClass, cluster or control plane is an error.
Regular unit runs do not start e2e infrastructure. The gVisor/microVM fixture
rendering is tested unchanged.

A failed run retains its own cluster for diagnosis. Remove only these resources
after inspecting them: kind cluster `lxc-e2e-7`, container
`lxc-e2e-7-registry`, Docker network `lxc-e2e-7`, and the stand directory.

## Validation

On 2026-10-26, the suite passed on Linux/amd64 in the isolated stand with
`-race -count=3 -shuffle=on`. All eight lifecycle subtests passed in every
iteration. One allowed-egress check needed 63 seconds: Envoy had cached a DNS
failure for the newly created Service and retried after its default 60-second
interval. The test allows 90 seconds and still requires HTTP 200; the forbidden
request must still be policy-denied.

The full Go race suite passed, as did the new unit tests with
`-count=3 -shuffle=on`. Selecting the LXC CI lane without `--e2e` was also
verified to fail before cluster access. All 16 verifier checks passed in an
isolated staged snapshot; the dirty-tree `hack/verify-all.sh` wrapper was not
reported as passing. The gVisor/microVM fixture-rendering tests passed, but their
live Kubernetes e2e suites were not run on this stand. The macOS harness was not
executed.
