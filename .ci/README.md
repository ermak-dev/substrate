# GitLab CI

Push and merge request pipelines run three required jobs. A branch with an open
merge request uses its MR pipeline instead of a duplicate push pipeline.

- `go-test`: root-module race tests, CI contract tests, tests in the three nested
  modules exercised upstream, and a Linux/arm64 build.
- `go-verify`: the unchanged `hack/verify-all.sh`, merged-migration immutability
  checks and the nested credential-provider plugin's verify/lint. The migration
  baseline is the MR base, the previous default-branch commit, or origin/main for
  feature-branch pushes. Missing or zero default-branch baselines fail.
- `go-repeat`: root-module unit tests and CI tests with race detection and
  `-count=3 -shuffle=on`. The LXC health-request unit test is also run explicitly
  because its suite's TestMain does not run without infrastructure.

## Trusted shell runner

On 2026-10-26 runner `builder` (ID 9) was online with tags `docker,build`, GitLab
Runner 19.3.1 and the shell executor. Windows and macOS runners are not selected.
The executor ignores GitLab's `image:` field, so the wrapper uses Docker
explicitly. No runner settings were changed.

Test/repeat jobs run Go directly on the Linux shell runner using its ordinary
local Docker daemon for PostgreSQL testcontainers. **Docker access is effectively
root access to that host. This runner is for trusted fork code, not unreviewed
external merge requests.** The wrapper does not claim to isolate the test code
from the host, and it does not require a separate TLS daemon or privileged DinD.
The two native jobs share a resource_group because functional-test fixtures use
fixed host listener ports; the verifier may run alongside either one.

The wrapper builds `.ci/Dockerfile`, whose Go and Weaver images are digest-pinned.
It copies the Go SDK from that image into a fresh private job directory, then
runs the test commands on the host. No host Go installation or system package
changes are needed. GCC, Git and a working local Unix-socket Docker context are
prerequisites. Testcontainers also needs access to its image registries.

Tests receive an explicit environment with a temporary HOME/TMPDIR, the local
Docker endpoint, Go cache locations and CI fail-closed flags. Operator/deploy
variables and GitLab job tokens are not forwarded. The SDK and temporary home
are removed after the job. Go concurrency is limited to GOMAXPROCS=4 and -p 2;
native tests and their Docker containers do not have an additional per-job
memory sandbox. The trusted runner must have capacity for them.

Verifier tools stay in a non-root Docker container with all capabilities dropped
and no-new-privileges. It has a 4-CPU, 8-GiB and 1024-PID limit. It mounts the
checkout, not the host Docker socket or home. The origin URL is rewritten to the
token-free CI_PROJECT_URL before mounting the checkout, and GitLab Runner is
asked not to persist authentication in checkout URLs. Its environment is
explicitly selected. Verifiers require a clean Git checkout and fail on missing
tools, failed downloads or generated-file drift.

Go caches use ignored `__ci-cache/` paths. GOFLAGS=-modcacherw lets the runner
remove module-cache directories during checkout cleanup. The pipeline does not
publish caches or credentials as artifacts. Outbound access to Go modules,
package repositories, release assets and Python indexes is required; no check
falls back to -short or skips unavailable PostgreSQL tests.

From a normal Linux clone, local equivalents are:

```sh
export CI_PROJECT_URL=https://git.ermak.dev/ermak/substrate
export CI_DEFAULT_BRANCH=main
.ci/run.sh test
.ci/run.sh verify
.ci/run.sh repeat
```

The wrapper rewrites that clone's origin URL to CI_PROJECT_URL.git, not global
Git configuration. GitLab supplies the metadata automatically. Linked worktrees
point outside the verifier's mounted checkout; use a normal clone to exercise
the complete wrapper. CI contract tests run directly with
`go test -race ./.ci -count=3 -shuffle=on`.

## Infrastructure e2e

The ordinary jobs do not claim to run root-gated OS tests, Rust dataplane tests
or live Kubernetes e2e. Those need separately authorized infrastructure.

Set RUN_LXC_E2E=true to require the lxc-e2e job. It is not allow_failure and does
not silently skip. It uses the exclusive substrate-lxc-e2e runner tag and
resource group. No runner with that tag was available during implementation;
enabling the job without one leaves it pending, not passed. Runner registration
or tag changes require operator approval.

The dedicated runner must be trusted, Linux, and able to run real LXC containers
in an isolated kind node. Set LXC_E2E_STAND to an absolute stand path created by
hack/lxc-e2e/create.sh. Provide Go (the version in go.mod), Docker, kind, kubectl
and ko on PATH or in the stand's bin/. Do not use a working application cluster
or share its Docker network with existing workloads. See
hack/lxc-e2e/README.md for provisioning and resource limits.

The job validates the cluster endpoint and CA before mutations. It builds
ate-setup and kubectl-ate from the current source, reinstalls that revision's
control plane and LXC worker, then runs the real API suite three times with race
detection. Missing prerequisites fail. It does not create a cluster, change host
settings, load kernel modules, change firewall/CNI/PVE, or clean an existing
stand. Never upload its kubeconfig or credentials as CI artifacts.

The LXC CI suite fails if selected without --e2e; the runner script supplies the
flag. Cold restart does not restore memory. gVisor/microVM live e2e require their
own infrastructure and are not reported as exercised by this configuration.

## Validation

On 2026-10-26 both `.ci/run.sh test` and `.ci/run.sh repeat` passed through SSH
on builder in a private normal checkout, including the fixture fixes from #11
and #12. The first job ran the full root-module race suite, nested-module tests
and arm64 build; the second ran the full root-module count=3/shuffle suite plus
CI and LXC health checks. Job-owned containers were gone afterwards, and readonly
envtest-cache cleanup completed without errors.

CI contract tests passed with race/count=3/shuffle. All 16 verifier checks passed
on the integrated staged snapshot; plugin verify/lint and migration checks were
also exercised. The literal verify-all.sh wrapper still requires a clean commit,
so snapshot equivalence is not reported as a successful dirty-tree wrapper run.
GitLab lint and a default-branch pipeline dry run passed before publication.
Results of actual GitLab pipeline runs are recorded in fork issue #8. LXC
infrastructure e2e was not rerun as part of this CI task.
