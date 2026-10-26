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

package lxc

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"go.ate.dev/substrate/api/ateapi/v1"
	"go.ate.dev/substrate/internal/ateclient"
	"go.ate.dev/substrate/internal/e2e"
	"go.ate.dev/substrate/internal/portforward"
	"go.ate.dev/substrate/internal/resources"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
)

// This lane exercises real LXC actors and refuses to substitute another runtime.
func TestLXCActorLifecycle(t *testing.T) {
	if !e2e.IsLXC() {
		t.Fatal("select E2E_SANDBOX_CLASS=lxc for this dedicated suite")
	}
	ctx := t.Context()
	clients := e2e.GetClients()
	if _, err := clients.K8s.StorageV1().StorageClasses().Get(ctx, e2e.StorageClass, metav1.GetOptions{}); err != nil {
		t.Fatalf("persistent-volume prerequisite %q: %v", e2e.StorageClass, err)
	}
	atespace, templates := e2e.DeploySubstrateFixture(t, ctx, clients, e2e.SubstrateFixtureManifests{Pool: "internal/e2e/fixtures/probe/probe.yaml.tmpl", Template: "internal/e2e/fixtures/probe/probe-lxc-template.yaml.tmpl"}, "ate-snapshots", "lifecycle", nil, e2e.WithTemplateValue("STORAGE_CLASS", e2e.StorageClass))
	router, err := e2e.NewRouterClient(t, ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer router.Close()
	origin := e2e.DeployServerPod(t, ctx, e2e.ServerPod{Name: "lxc-origin", ImportPath: "go.ate.dev/substrate/internal/e2e/fixtures/testserver", Args: []string{"http"}, Port: 80, TargetPort: 8080})
	allowed := "lxc-origin." + origin.Namespace + ".svc.cluster.local"
	actor := createActor(t, ctx, atespace, "first", templates[0], allowed)
	other := createActor(t, ctx, atespace, "second", templates[0], allowed)
	ref := resources.ActorRefFromActor(actor)
	otherRef := resources.ActorRefFromActor(other)
	if actor.GetStatus().GetWorkerAssignment().GetSandboxRuntime().GetSandboxClass() != "lxc" {
		t.Fatalf("wrong assigned runtime: %v", actor.GetStatus().GetWorkerAssignment())
	}
	if actor.GetStatus().GetWorkerAssignment().GetWorker().GetName() == other.GetStatus().GetWorkerAssignment().GetWorker().GetName() {
		t.Fatal("two actors placed on one-slot worker")
	}

	t.Run("ingress-and-identity", func(t *testing.T) {
		got := probe(t, ctx, router, ref, "/whoami")
		if got["uid"] != actor.GetMetadata().GetUid() {
			t.Fatalf("identity: %v", got)
		}
	})
	t.Run("gateway-allow-and-deny", func(t *testing.T) {
		var result map[string]string
		// Envoy retries failed DNS lookups after 60s by default.
		err := wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 90*time.Second, true, func(ctx context.Context) (bool, error) {
			result = probe(t, ctx, router, ref, "/fetch?roots=system&url="+url.QueryEscape("http://"+allowed+"/healthz"))
			return result["status"] == "200", nil
		})
		if err != nil {
			t.Fatalf("allowed origin through gateway: %v: %v", err, result)
		}
		result = probe(t, ctx, router, ref, "/fetch?roots=system&url="+url.QueryEscape("http://"+origin.Address()+"/healthz"))
		if !e2e.CurrentAtenetDataplane().IsEgressPolicyDenied(parseStatus(result["status"]), result["body"]) {
			t.Fatalf("same origin by forbidden IP was not policy-denied: %v", result)
		}
	})
	t.Run("network-namespaces", func(t *testing.T) {
		assignment := other.GetStatus().GetWorkerAssignment()
		config, err := ateclient.LoadKubeConfig(e2e.KubeConfig, e2e.KubeContext)
		if err != nil {
			t.Fatal(err)
		}
		port, stop, err := portforward.PodPortForward(ctx, config, clients.K8s, assignment.GetWorkerNamespace(), assignment.GetWorkerPod(), 8080)
		if err != nil {
			t.Fatal(err)
		}
		defer stop()
		if err := checkWorkerHealth(ctx, fmt.Sprintf("http://127.0.0.1:%d/healthz", port)); err != nil {
			t.Fatal(err)
		}
		blocked := probe(t, ctx, router, ref, "/fetch?roots=system&url="+url.QueryEscape("http://"+assignment.GetWorkerPodIps()[0]+":8080/healthz"))
		if blocked["status"] == "200" || (blocked["error"] == "" && !e2e.CurrentAtenetDataplane().IsEgressPolicyDenied(parseStatus(blocked["status"]), blocked["body"])) {
			t.Fatalf("actor reached neighbor worker control endpoint: %v", blocked)
		}

		got := probe(t, ctx, router, ref, "/fetch?roots=system&url="+url.QueryEscape("http://169.254.17.2/whoami"))
		var identity map[string]string
		if got["status"] != "200" || json.Unmarshal([]byte(got["body"]), &identity) != nil || identity["uid"] != actor.GetMetadata().GetUid() {
			t.Fatalf("actor-local address leaked another actor: %v", got)
		}
		got = probe(t, ctx, router, otherRef, "/fetch?roots=system&url="+url.QueryEscape("http://169.254.17.2/whoami"))
		if got["status"] != "200" || json.Unmarshal([]byte(got["body"]), &identity) != nil || identity["uid"] != other.GetMetadata().GetUid() {
			t.Fatalf("second actor namespace: %v", got)
		}
	})
	t.Run("suspend-preserves-live-state", func(t *testing.T) {
		requireWrite(t, ctx, router, ref, "/tmp/live-marker")
		requireWrite(t, ctx, router, ref, "/data/suspend-marker")
		memory, err := router.PostJSON(ctx, ref, "/memory", nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := memory.Body.Close(); err != nil {
			t.Fatal(err)
		}
		if memory.StatusCode != http.StatusOK {
			t.Fatalf("memory marker: %d", memory.StatusCode)
		}
		memoryBefore := probe(t, ctx, router, ref, "/memory")["value"]
		if memoryBefore == "0" || memoryBefore == "" {
			t.Fatal("in-memory marker was not set")
		}

		for _, mode := range []ateapi.DurabilityMode{ateapi.DurabilityMode_DURABILITY_MODE_SYNCHRONOUS, ateapi.DurabilityMode_DURABILITY_MODE_ASYNCHRONOUS} {
			before, err := clients.SubstrateAPI.GetActor(ctx, &ateapi.GetActorRequest{Actor: ref.ToObjectRef()})
			if err != nil {
				t.Fatal(err)
			}
			_, err = clients.SubstrateAPI.SuspendActor(ctx, &ateapi.SuspendActorRequest{Actor: ref.ToObjectRef(), Options: &ateapi.SuspendOptions{Mode: mode}})
			if status.Code(err) != codes.FailedPrecondition {
				t.Fatalf("suspend %s: %v", mode, err)
			}
			after, err := clients.SubstrateAPI.GetActor(ctx, &ateapi.GetActorRequest{Actor: ref.ToObjectRef()})
			if err != nil {
				t.Fatal(err)
			}
			if !proto.Equal(before.GetStatus(), after.GetStatus()) {
				t.Fatalf("unsupported suspend changed status: %v -> %v", before.GetStatus(), after.GetStatus())
			}
			requireContent(t, ctx, router, ref, "/tmp/live-marker", "written by probe")
			requireContent(t, ctx, router, ref, "/data/suspend-marker", "written by probe")
			if got := probe(t, ctx, router, ref, "/memory")["value"]; got != memoryBefore {
				t.Fatalf("unsupported suspend changed memory: %q -> %q", memoryBefore, got)
			}

		}
	})
	t.Run("persistent-volume-versus-rootfs", func(t *testing.T) {
		requireWrite(t, ctx, router, ref, "/data/persistent-marker")
		requireWrite(t, ctx, router, ref, "/tmp/ephemeral-marker")
		if _, err := clients.SubstrateAPI.RevertActor(ctx, &ateapi.RevertActorRequest{Actor: ref.ToObjectRef()}); err != nil {
			t.Fatal(err)
		}
		actor = resumeActor(t, ctx, ref.ToObjectRef())
		requireContent(t, ctx, router, ref, "/data/persistent-marker", "written by probe")
		if got := probe(t, ctx, router, ref, "/readfile?path=/tmp/ephemeral-marker"); got["error"] == "" {
			t.Fatalf("cold reset retained temporary rootfs: %v", got)
		}
	})
	t.Run("crash-and-cold-restart", func(t *testing.T) {
		assignment := actor.GetStatus().GetWorkerAssignment()
		e2e.KillActorSandbox(ctx, t, clients, assignment.GetWorkerNamespace(), assignment.GetWorkerPod(), actor.GetMetadata().GetUid())
		waitActorState(t, ctx, ref, ateapi.ActorState_ACTOR_STATE_CRASHED)
		if _, err := clients.SubstrateAPI.RevertActor(ctx, &ateapi.RevertActorRequest{Actor: ref.ToObjectRef()}); err != nil {
			t.Fatal(err)
		}
		actor = resumeActor(t, ctx, ref.ToObjectRef())
		requireContent(t, ctx, router, ref, "/data/persistent-marker", "written by probe")
	})
	t.Run("worker-process-restart", func(t *testing.T) {
		assignment := actor.GetStatus().GetWorkerAssignment()
		pod, err := clients.K8s.CoreV1().Pods(assignment.GetWorkerNamespace()).Get(ctx, assignment.GetWorkerPod(), metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if string(pod.UID) != assignment.GetWorkerPodUid() {
			t.Fatal("worker pod UID changed before restart")
		}
		oldUID := pod.UID
		// Recreate only this test's worker pod. Recovery is a cold restart, not RAM restore.
		if err := clients.K8s.CoreV1().Pods(pod.Namespace).Delete(ctx, pod.Name, metav1.DeleteOptions{}); err != nil {
			t.Fatal(err)
		}
		waitActorState(t, ctx, ref, ateapi.ActorState_ACTOR_STATE_CRASHED)
		if _, err := clients.SubstrateAPI.RevertActor(ctx, &ateapi.RevertActorRequest{Actor: ref.ToObjectRef()}); err != nil {
			t.Fatal(err)
		}
		actor = resumeActor(t, ctx, ref.ToObjectRef())
		if actor.GetStatus().GetWorkerAssignment().GetWorkerPodUid() == string(oldUID) {
			t.Fatal("actor returned to deleted worker pod")
		}
		requireContent(t, ctx, router, ref, "/data/persistent-marker", "written by probe")
	})
	t.Run("delete-does-not-touch-neighbor", func(t *testing.T) {
		if _, err := clients.SubstrateAPI.DeleteActor(ctx, &ateapi.DeleteActorRequest{Actor: ref.ToObjectRef()}); err != nil {
			t.Fatal(err)
		}
		if _, err := clients.SubstrateAPI.GetActor(ctx, &ateapi.GetActorRequest{Actor: ref.ToObjectRef()}); status.Code(err) != codes.NotFound {
			t.Fatalf("deleted actor still exists: %v", err)
		}
		if got := probe(t, ctx, router, otherRef, "/whoami"); got["uid"] != other.GetMetadata().GetUid() {
			t.Fatalf("delete damaged neighbor: %v", got)
		}
	})
}

func createActor(t *testing.T, ctx context.Context, atespace, name string, template *ateapi.ActorTemplate, allowed string) *ateapi.Actor {
	t.Helper()
	clients := e2e.GetClients()
	ref := &ateapi.ObjectRef{Atespace: atespace, Name: name}
	if _, err := clients.SubstrateAPI.CreateActor(ctx, &ateapi.CreateActorRequest{Actor: &ateapi.Actor{Metadata: &ateapi.ResourceMetadata{Atespace: atespace, Name: name}, ActorTemplate: e2e.TemplateRef(template)}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if _, err := clients.SubstrateAPI.DeleteActor(cleanup, &ateapi.DeleteActorRequest{Actor: ref}); err != nil && status.Code(err) != codes.NotFound {
			t.Error(err)
		}
	})
	e2e.EnsureEgressPolicy(t, ctx, clients, ref, e2e.EgressAllowHTTP(allowed))
	return resumeActor(t, ctx, ref)
}
func resumeActor(t *testing.T, ctx context.Context, ref *ateapi.ObjectRef) *ateapi.Actor {
	t.Helper()
	response, err := e2e.ResumeActorAwaitCapacity(t, ctx, e2e.GetClients(), &ateapi.ResumeActorRequest{Actor: ref})
	if err != nil {
		t.Fatal(err)
	}
	return response.GetActor()
}
func probe(t *testing.T, ctx context.Context, router *e2e.RouterClient, ref resources.ActorRef, path string) map[string]string {
	t.Helper()
	var data []byte
	err := wait.PollUntilContextTimeout(ctx, 100*time.Millisecond, 30*time.Second, true, func(ctx context.Context) (bool, error) {
		response, err := router.Get(ctx, ref, path)
		if err != nil {
			return false, nil
		}
		defer response.Body.Close()
		data, err = io.ReadAll(response.Body)
		if err != nil {
			return false, err
		}
		return response.StatusCode == http.StatusOK, nil
	})
	if err != nil {
		t.Fatalf("probe %s: %v: %s", path, err, data)
	}
	result := map[string]string{}
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatalf("probe response %s: %v", data, err)
	}
	return result
}
func requireWrite(t *testing.T, ctx context.Context, router *e2e.RouterClient, ref resources.ActorRef, path string) {
	t.Helper()
	if result := probe(t, ctx, router, ref, "/writefile?path="+url.QueryEscape(path)); result["ok"] != "true" {
		t.Fatalf("write %s: %v", path, result)
	}
}
func requireContent(t *testing.T, ctx context.Context, router *e2e.RouterClient, ref resources.ActorRef, path, want string) {
	t.Helper()
	if result := probe(t, ctx, router, ref, "/readfile?path="+url.QueryEscape(path)); result["error"] != "" || result["content"] != want {
		t.Fatalf("read %s: %v", path, result)
	}
}
func waitActorState(t *testing.T, ctx context.Context, ref resources.ActorRef, want ateapi.ActorState) {
	t.Helper()
	var actor *ateapi.Actor
	err := wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 90*time.Second, true, func(ctx context.Context) (bool, error) {
		var err error
		actor, err = e2e.GetClients().SubstrateAPI.GetActor(ctx, &ateapi.GetActorRequest{Actor: ref.ToObjectRef()})
		return err == nil && actor.GetStatus().GetState() == want, err
	})
	if err != nil {
		t.Fatalf("actor state wanted %s: %v: %v", want, actor, err)
	}
}
func parseStatus(s string) int {
	var code int
	_, _ = fmt.Sscanf(strings.TrimSpace(s), "%d", &code)
	return code
}
