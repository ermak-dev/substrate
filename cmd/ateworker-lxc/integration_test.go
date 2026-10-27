//go:build linux && lxc_integration

// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.ate.dev/substrate/internal/actorlog"
	"go.ate.dev/substrate/internal/ateworker/actornet"
	"go.ate.dev/substrate/internal/ateworker/cgroup"
	"go.ate.dev/substrate/internal/ateworker/pid1"
	"go.ate.dev/substrate/internal/atunnel"
	"go.ate.dev/substrate/internal/egresspolicy"
	"go.ate.dev/substrate/internal/imagecache"
	"go.ate.dev/substrate/internal/installdefaults"
	"go.ate.dev/substrate/internal/localca"
	"go.ate.dev/substrate/internal/nodepath"
	"go.ate.dev/substrate/internal/proto/ateworkerpb"
	"go.ate.dev/substrate/internal/substratex509"
	"k8s.io/apimachinery/pkg/util/wait"
)

func TestMain(m *testing.M) {
	pid1.RunIfPID1()
	if integrationContainer() && os.Getenv("ATE_LXC_RECOVERY_CHILD") != "1" {
		if err := cgroup.Delegate(context.Background()); err != nil {
			os.Stderr.WriteString(err.Error())
			os.Exit(1)
		}
	}
	os.Exit(m.Run())
}

func integrationContainer() bool { return os.Getenv("ATE_LXC_INTEGRATION_CONTAINER") == "1" }

func integrationTunnel(t *testing.T) *atunnel.Tunnel {
	t.Helper()
	ca, err := localca.GenerateCA("lxc-integration", localca.KeyTypeECDSAP256, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(2), NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}}
	if err := substratex509.AddPodIdentityToCertificate(&substratex509.PodIdentity{Namespace: "test", ServiceAccountName: "worker", ServiceAccountUID: "test-sa", PodName: "worker", PodUID: "test-pod", NodeName: "test-node", NodeUID: "test-node-uid"}, template); err != nil {
		t.Fatal(err)
	}
	der, err := x509.CreateCertificate(rand.Reader, template, ca.RootCertificate, &key.PublicKey, ca.SigningKey)
	if err != nil {
		t.Fatal(err)
	}
	private, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	bundle := filepath.Join(dir, "worker.pem")
	trust := filepath.Join(dir, "trust.pem")
	material := append(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: private})...)
	if err := os.WriteFile(bundle, material, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(trust, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.RootCertificate.Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	tunnel, err := atunnel.Start(t.Context(), atunnel.Config{ListenAddress: "127.0.0.1:0", ConnectListenAddress: "127.0.0.1:0", CredentialBundle: bundle, TrustBundle: trust, ClientIdentity: installdefaults.SPIFFEID("test", "router"), BrokerIdentity: installdefaults.SPIFFEID("test", "atelet"), EgressListenAddress: "0.0.0.0:15001", EgressTrustBundle: trust, DNSEgressPolicy: egresspolicy.DNSEgressPolicyEnforceInAtunnel}, actornet.ActorHTTPUpstream)
	if err != nil {
		t.Fatal(err)
	}
	return tunnel
}

func integrationRequest(t *testing.T, uid string) *ateworkerpb.RunWorkloadRequest {
	t.Helper()
	req := runRequest(uid)
	t.Cleanup(func() {
		if err := os.RemoveAll(req.ActorDirs.RootDir); err != nil {
			t.Error(err)
		}
	})
	bundle := filepath.Join(req.ActorDirs.OciBundleDir, "main")
	if err := os.MkdirAll(bundle, 0o700); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.CommandContext(t.Context(), "cp", "-a", "/opt/lxc-test-guest", filepath.Join(bundle, "rootfs")).CombinedOutput(); err != nil {
		t.Fatalf("copy rootfs: %v: %s", err, out)
	}
	req.Spec.Containers[0].ContainerSpec = &ateworkerpb.ContainerSpec{Args: []string{"/sbin/init"}, Env: []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin"}}
	req.Resources = &ateworkerpb.ResourceRequirements{Limits: &ateworkerpb.Resources{CpuMillicores: 1000, MemoryBytes: 512 << 20}}
	return req
}

func TestLXCIntegration(t *testing.T) {
	if !integrationContainer() {
		t.Fatal("use testdata/run.sh: this test requires an isolated private-cgroup container")
	}
	for _, scenario := range []string{"terminate", "crash", "partial-start", "recover", "overlay"} {
		t.Run(scenario, func(t *testing.T) {
			if scenario == "recover" {
				testPhysicalRecovery(t)
				return
			}

			req := integrationRequest(t, actorUID)
			if scenario == "overlay" {
				bundle := filepath.Join(req.ActorDirs.OciBundleDir, "main")
				layer := filepath.Join(req.ActorDirs.RootDir, "layer")
				if err := os.MkdirAll(layer, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(filepath.Join(bundle, "rootfs"), filepath.Join(layer, "fs")); err != nil {
					t.Fatal(err)
				}
				if err := imagecache.WriteSpec(bundle, &imagecache.OverlaySpec{Layers: []string{layer}, ExtraDirs: []string{"/data"}}); err != nil {
					t.Fatal(err)
				}
				volume := filepath.Join(req.ActorDirs.SystemInfoVolumeRootsDir, "data")
				if err := os.MkdirAll(volume, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(volume, "marker"), []byte("actor-volume"), 0o600); err != nil {
					t.Fatal(err)
				}
				req.Spec.Containers[0].SystemInfoVolumeMounts = []*ateworkerpb.VolumeMount{{Name: "data", MountPath: "/data"}}
			}

			tunnel := integrationTunnel(t)
			logger := actorlog.NewActorLogger(actorlog.NewSyncedWriter(os.Stdout), false)
			root := filepath.Join(nodepath.WorkerPath("integration"), "lxc")
			backend := newLXCBackend(root, "/usr/local/bin/ateworker-lxc", logger, tunnel)
			reporter := &fakeCrashReporter{reported: make(chan crashRecord, 1)}
			lifetime, cancel := context.WithCancel(t.Context())
			defer cancel()
			worker := newWorkerService(lifetime, backend, reporter, nil)
			t.Cleanup(func() {
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				if err := worker.shutdown(ctx); err != nil {
					t.Error(err)
				}
			})
			worker.epoch.Store(9)
			if scenario == "partial-start" {
				req.Spec.Containers[0].ContainerSpec.Args = []string{"/missing-executable"}
				req.Spec.Containers[0].WakeupProbe = &ateworkerpb.WakeupProbe{HttpGet: &ateworkerpb.HttpGetAction{Path: "/", Port: 8080}, TimeoutSeconds: 2}
				_, err := worker.RunWorkload(t.Context(), req)
				if err == nil {
					t.Fatal("bad executable accepted")
				}
			} else {
				if _, err := worker.RunWorkload(t.Context(), req); err != nil {
					t.Fatal(err)
				}
				err := wait.PollUntilContextTimeout(t.Context(), 100*time.Millisecond, 30*time.Second, true, func(ctx context.Context) (bool, error) {
					err := exec.CommandContext(ctx, "lxc-attach", "-P", filepath.Join(root, actorUID, "lxc"), "-n", lxcName(actorUID, "main"), "--", "/usr/bin/test", "-S", "/run/systemd/private").Run()
					return err == nil, nil
				})
				if err != nil {
					t.Fatal(err)
				}
				stats, err := worker.GetWorkloadStats(t.Context(), &ateworkerpb.GetWorkloadStatsRequest{ActorUid: actorUID})
				if err != nil || stats.GetSample().GetActorUid() != actorUID || stats.GetSample().GetMemoryCurrentBytes() == 0 {
					t.Fatalf("physical stats: %v, %v", stats, err)
				}
				switch scenario {
				case "overlay":
					base := []string{"-P", filepath.Join(root, actorUID, "lxc"), "-n", lxcName(actorUID, "main"), "--"}
					out, err := exec.CommandContext(t.Context(), "lxc-attach", append(base, "/bin/cat", "/data/marker")...).CombinedOutput()
					if err != nil || !strings.Contains(string(out), "actor-volume") {
						t.Fatalf("volume: %q, %v", out, err)
					}
					if err := exec.CommandContext(t.Context(), "lxc-attach", append(base, "/usr/bin/touch", "/data/forbidden")...).Run(); err == nil {
						t.Fatal("read-only volume was writable")
					}
					if _, err := worker.TerminateWorkload(t.Context(), terminateRequest(actorUID)); err != nil {
						t.Fatal(err)
					}
				case "terminate":
					for range 2 {
						if _, err := worker.TerminateWorkload(t.Context(), terminateRequest(actorUID)); err != nil {
							t.Fatal(err)
						}
					}
				case "crash":
					if out, err := exec.CommandContext(t.Context(), "lxc-stop", "-P", filepath.Join(root, actorUID, "lxc"), "-n", lxcName(actorUID, "main"), "-k").CombinedOutput(); err != nil {
						t.Fatalf("kill LXC: %v: %s", err, out)
					}
					select {
					case report := <-reporter.reported:
						if report.actor.UID != actorUID {
							t.Fatal("wrong crash actor")
						}
					case <-t.Context().Done():
						t.Fatal("crash report missing")
					}

				}
			}
			for _, path := range []string{filepath.Join(root, actorUID), filepath.Join(cgroup.Root, actorUID), nodepath.ActorNetNSPath(actorUID), filepath.Join("/run/netns", actornet.SandboxGatewayNetNSName(actorUID))} {
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Errorf("resource remains at %s: %v", path, err)
				}
			}
			mounts, err := os.ReadFile("/proc/self/mountinfo")
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(mounts), req.ActorDirs.OciBundleDir+"/") {
				t.Error("actor bundle mount remains")
			}
			if err := os.RemoveAll(req.ActorDirs.RootDir); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// Exiting without cleanup leaves the actual OS resources for the next backend.
func TestLXCRecoveryChild(t *testing.T) {
	if !integrationContainer() || os.Getenv("ATE_LXC_RECOVERY_CHILD") != "1" {
		t.Fatal("recovery child requires the private integration harness")
	}
	req := integrationRequest(t, actorUID)
	tunnel := integrationTunnel(t)
	logger := actorlog.NewActorLogger(actorlog.NewSyncedWriter(os.Stdout), false)
	backend := newLXCBackend(filepath.Join(nodepath.WorkerPath("integration"), "lxc"), "/usr/local/bin/ateworker-lxc", logger, tunnel)
	worker := newWorkerService(t.Context(), backend, nil, nil)
	worker.epoch.Store(9)
	if _, err := worker.RunWorkload(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	os.Exit(0)
}

func testPhysicalRecovery(t *testing.T) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "old-worker.log")
	log, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestLXCRecoveryChild$", "-test.timeout=1m")
	cmd.Env = append(os.Environ(), "ATE_LXC_RECOVERY_CHILD=1")
	cmd.Stdout = log
	cmd.Stderr = log
	runErr := cmd.Run()
	closeErr := log.Close()
	if runErr != nil || closeErr != nil {
		data, _ := os.ReadFile(path)
		t.Fatalf("old worker: %v, %v: %s", runErr, closeErr, data)
	}
	root := filepath.Join(nodepath.WorkerPath("integration"), "lxc")
	tunnel := integrationTunnel(t)
	backend := newLXCBackend(root, "/usr/local/bin/ateworker-lxc", actorlog.NewActorLogger(actorlog.NewSyncedWriter(os.Stdout), false), tunnel)
	actors, err := backend.Recover(t.Context())
	if err != nil || len(actors) != 1 || actors[0].UID != actorUID {
		t.Fatalf("recover: %v, %v", actors, err)
	}
	for _, path := range []string{filepath.Join(root, actorUID), filepath.Join(cgroup.Root, actorUID), nodepath.ActorNetNSPath(actorUID), filepath.Join("/run/netns", actornet.SandboxGatewayNetNSName(actorUID))} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("resource remains after worker exit: %s: %v", path, err)
		}
	}
	mounts, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		t.Fatal(err)
	}
	actorRoot := filepath.Join(nodepath.ActorsDir, actorUID)
	if strings.Contains(string(mounts), actorRoot+"/") {
		t.Error("actor mount remains after recovery")
	}
	if err := os.RemoveAll(actorRoot); err != nil {
		t.Fatal(err)
	}
}
