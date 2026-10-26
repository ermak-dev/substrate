//go:build linux

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
	"errors"
	"go.ate.dev/substrate/internal/resources"
	"go.ate.dev/substrate/internal/workerstats"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.ate.dev/substrate/internal/apierror"
	"go.ate.dev/substrate/internal/nodepath"
	"go.ate.dev/substrate/internal/proto/ateworkerpb"
	"google.golang.org/grpc/codes"
)

const actorUID = "01234567-89ab-cdef-0123-456789abcdef"
const otherUID = "11234567-89ab-cdef-0123-456789abcdef"

func runRequest(uid string) *ateworkerpb.RunWorkloadRequest {
	root := filepath.Join(nodepath.ActorsDir, uid)
	return &ateworkerpb.RunWorkloadRequest{Atespace: "team-a", ActorName: "actor-1", ActorUid: uid,
		ActorTemplateAtespace: "team-a", ActorTemplateName: "template-1",
		Spec:      &ateworkerpb.WorkloadSpec{Containers: []*ateworkerpb.Container{{Name: "main"}}},
		ActorDirs: &ateworkerpb.ActorDirs{RootDir: root, OciBundleDir: root + "/bundles", CheckpointDir: root + "/checkpoint", RestoreDir: root + "/restore", DurableDirVolumeMountsDir: root + "/durable", SystemInfoVolumeRootsDir: root + "/sysinfo", ExternalVolumeMountsDir: root + "/external", SandboxAssetsDir: nodepath.ActorSandboxAssetsDir(uid)},
	}
}

// An actor must not start before the worker has acquired its epoch.
func TestRunRequiresRegistration(t *testing.T) {
	worker := &workerService{}
	_, err := worker.RunWorkload(context.Background(), runRequest(actorUID))
	if apierror.Code(err) != codes.Unavailable {
		t.Fatalf("RunWorkload error = %v, want Unavailable", err)
	}
}

type fakeSession struct {
	reads atomic.Int32
	done  chan struct{}
}

func (f *fakeSession) Done() <-chan struct{} { return f.done }
func (f *fakeSession) Exit() string          { return "monitor exited" }
func (f *fakeSession) Sample() (*ateworkerpb.WorkloadStatsSample, error) {
	f.reads.Add(1)
	return &ateworkerpb.WorkloadStatsSample{Source: ateworkerpb.StatsSource_STATS_SOURCE_CGROUP, MemoryCurrentBytes: 100, MemoryWorkingSetBytes: 80, CpuUsageUsec: 23}, nil
}

type fakeBackend struct {
	mu                               sync.Mutex
	session                          *fakeSession
	startErr, cleanupErr, recoverErr error
	starts, cleanups                 int
	entered, release                 chan struct{}
	recovered                        []resources.ActorAttribution
}

func newFakeBackend() *fakeBackend {
	return &fakeBackend{session: &fakeSession{done: make(chan struct{})}}
}
func (f *fakeBackend) Start(ctx context.Context, req *ateworkerpb.RunWorkloadRequest) (runtimeSession, error) {
	f.mu.Lock()
	f.starts++
	entered, release, session, err := f.entered, f.release, f.session, f.startErr
	f.mu.Unlock()
	if entered != nil {
		close(entered)
	}
	if release != nil {
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return session, err
}
func (f *fakeBackend) Cleanup(context.Context, string, *ateworkerpb.ActorDirs) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cleanups++
	return f.cleanupErr
}
func (f *fakeBackend) Recover(context.Context) ([]resources.ActorAttribution, error) {
	return f.recovered, f.recoverErr
}

type crashRecord struct {
	actor   resources.ActorAttribution
	message string
}
type fakeCrashReporter struct {
	reported chan crashRecord
	release  <-chan struct{}
}

func (f *fakeCrashReporter) ReportCrashed(_ context.Context, a resources.ActorAttribution, msg string) error {
	f.reported <- crashRecord{a, msg}
	if f.release != nil {
		<-f.release
	}
	return nil
}

func terminateRequest(uid string) *ateworkerpb.TerminateWorkloadRequest {
	r := runRequest(uid)
	return &ateworkerpb.TerminateWorkloadRequest{Atespace: r.Atespace, ActorName: r.ActorName, ActorUid: uid, ActorDirs: r.ActorDirs, Spec: r.Spec}
}

func TestLifecycle(t *testing.T) {
	for _, tc := range []struct {
		name                 string
		startErr, cleanupErr error
	}{
		{"running", nil, nil},
		{"partial startup failure", errors.New("partial boot"), nil},
		{"startup and cleanup failure", errors.New("partial boot"), errors.New("mount busy")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			driver := newFakeBackend()
			driver.startErr = tc.startErr
			driver.cleanupErr = tc.cleanupErr
			worker := newWorkerService(t.Context(), driver, nil, nil)
			worker.epoch.Store(7)
			response, err := worker.RunWorkload(t.Context(), runRequest(actorUID))
			if tc.startErr == nil {
				if err != nil || response.GetWorkerEpoch() != 7 {
					t.Fatalf("run = %v, %v", response, err)
				}
				if _, err := worker.RunWorkload(t.Context(), runRequest(actorUID)); err != nil {
					t.Fatalf("retry: %v", err)
				}
				if driver.starts != 1 {
					t.Fatalf("retry started %d runtimes", driver.starts)
				}
				if _, err := worker.RunWorkload(t.Context(), runRequest(otherUID)); apierror.Code(err) != codes.ResourceExhausted {
					t.Fatalf("second actor: %v", err)
				}
				for range 2 {
					if _, err := worker.TerminateWorkload(t.Context(), terminateRequest(actorUID)); err != nil {
						t.Fatal(err)
					}
				}
			} else {
				if err == nil {
					t.Fatal("partial failure was accepted")
				}
				if driver.cleanups != 1 {
					t.Fatalf("cleanup calls = %d, want 1", driver.cleanups)
				}
				if tc.cleanupErr != nil {
					if _, err := worker.RunWorkload(t.Context(), runRequest(otherUID)); apierror.Code(err) != codes.ResourceExhausted {
						t.Fatalf("dirty runtime reused: %v", err)
					}
				}
				driver.mu.Lock()
				driver.startErr = nil
				driver.cleanupErr = nil
				driver.mu.Unlock()
				if _, err := worker.TerminateWorkload(t.Context(), terminateRequest(actorUID)); err != nil {
					t.Fatal(err)
				}
			}
			got, err := worker.GetActiveWorkloadStats(t.Context(), &ateworkerpb.GetActiveWorkloadStatsRequest{})
			if err != nil || len(got.GetSamples()) != 0 {
				t.Fatalf("inactive stats = %v, %v", got, err)
			}
			if _, err := worker.RunWorkload(t.Context(), runRequest(otherUID)); err != nil {
				t.Fatalf("capacity not released: %v", err)
			}
		})
	}
}

func TestTerminateCleanupFailureRetainsCapacity(t *testing.T) {
	driver := newFakeBackend()
	worker := newWorkerService(t.Context(), driver, nil, nil)
	worker.epoch.Store(1)
	if _, err := worker.RunWorkload(t.Context(), runRequest(actorUID)); err != nil {
		t.Fatal(err)
	}
	driver.mu.Lock()
	driver.cleanupErr = errors.New("busy mount")
	driver.mu.Unlock()
	if _, err := worker.TerminateWorkload(t.Context(), terminateRequest(actorUID)); err == nil {
		t.Fatal("cleanup failure ignored")
	}
	if _, err := worker.RunWorkload(t.Context(), runRequest(otherUID)); apierror.Code(err) != codes.ResourceExhausted {
		t.Fatalf("capacity reused: %v", err)
	}
	driver.mu.Lock()
	driver.cleanupErr = nil
	driver.mu.Unlock()
	if _, err := worker.TerminateWorkload(t.Context(), terminateRequest(actorUID)); err != nil {
		t.Fatal(err)
	}
}

func TestStatsAndCrashAttribution(t *testing.T) {
	driver := newFakeBackend()
	reporter := &fakeCrashReporter{reported: make(chan crashRecord, 1)}
	worker := newWorkerService(t.Context(), driver, reporter, nil)
	worker.epoch.Store(5)
	if _, err := worker.RunWorkload(t.Context(), runRequest(actorUID)); err != nil {
		t.Fatal(err)
	}
	result, err := worker.GetWorkloadStats(t.Context(), &ateworkerpb.GetWorkloadStatsRequest{ActorUid: actorUID})
	if err != nil {
		t.Fatal(err)
	}
	sample := result.GetSample()
	if sample.GetActorUid() != actorUID || sample.GetActorName() != "actor-1" || sample.GetAtespace() != "team-a" || sample.GetActorTemplateName() != "template-1" || sample.GetSandboxClass() != "lxc" || sample.GetEpochUnixNano() == 0 || sample.GetMemoryCurrentBytes() != 100 || sample.GetCpuUsageUsec() != 23 {
		t.Fatalf("wrong attribution or usage: %v", sample)
	}
	if _, err := worker.GetWorkloadStats(t.Context(), &ateworkerpb.GetWorkloadStatsRequest{ActorUid: otherUID}); apierror.Code(err) != codes.NotFound {
		t.Fatalf("foreign actor stats: %v", err)
	}
	close(driver.session.done)
	select {
	case crash := <-reporter.reported:
		if crash.actor.UID != actorUID || crash.message == "" {
			t.Fatalf("crash = %+v", crash)
		}
	case <-t.Context().Done():
		t.Fatal("crash not reported")
	}
	if !worker.inflight.WaitIdle(t.Context()) {
		t.Fatal("crash watcher did not complete")
	}
	driver.mu.Lock()
	cleanups := driver.cleanups
	driver.mu.Unlock()
	if cleanups != 1 {
		t.Fatalf("crash cleanup calls = %d", cleanups)
	}
	got, _ := worker.GetActiveWorkloadStats(t.Context(), &ateworkerpb.GetActiveWorkloadStatsRequest{})
	if len(got.GetSamples()) != 0 {
		t.Fatalf("dead actor still hosted: %v", got)
	}
}

func TestShutdownCancelsStartup(t *testing.T) {
	driver := newFakeBackend()
	driver.entered = make(chan struct{})
	driver.release = make(chan struct{})
	worker := newWorkerService(t.Context(), driver, nil, nil)
	worker.epoch.Store(1)
	result := make(chan error, 1)
	go func() { _, err := worker.RunWorkload(t.Context(), runRequest(actorUID)); result <- err }()
	<-driver.entered
	stats, err := worker.GetActiveWorkloadStats(t.Context(), &ateworkerpb.GetActiveWorkloadStatsRequest{})
	if err != nil || len(stats.GetSamples()) != 1 || stats.GetSamples()[0].GetSource() != ateworkerpb.StatsSource_STATS_SOURCE_UNSPECIFIED {
		t.Fatalf("startup stats = %v, %v", stats, err)
	}
	if err := worker.shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := <-result; err == nil {
		t.Fatal("startup not canceled")
	}
	if _, err := worker.RunWorkload(t.Context(), runRequest(otherUID)); apierror.Code(err) != codes.Unavailable {
		t.Fatalf("shutdown accepted actor: %v", err)
	}
}

func TestRecoveryRefusesCapacityOnCleanupFailure(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "clean", true: "cleanup failed"}[fail], func(t *testing.T) {
			driver := newFakeBackend()
			if fail {
				driver.recoverErr = errors.New("orphan still alive")
			}
			worker := newWorkerService(t.Context(), driver, nil, nil)
			err := worker.recover(t.Context())
			if (err != nil) != fail {
				t.Fatalf("recover = %v", err)
			}
		})
	}
}

func TestUnknownTerminateDoesNotKillCurrentActor(t *testing.T) {
	driver := newFakeBackend()
	worker := newWorkerService(t.Context(), driver, nil, nil)
	worker.epoch.Store(1)
	if _, err := worker.RunWorkload(t.Context(), runRequest(actorUID)); err != nil {
		t.Fatal(err)
	}
	if _, err := worker.TerminateWorkload(t.Context(), terminateRequest(otherUID)); err != nil {
		t.Fatal(err)
	}
	stats, err := worker.GetWorkloadStats(t.Context(), &ateworkerpb.GetWorkloadStatsRequest{ActorUid: actorUID})
	if err != nil || stats.GetSample().GetActorUid() != actorUID {
		t.Fatalf("foreign terminate removed active actor: %v, %v", stats, err)
	}
}

func TestTerminateTakesFinalReading(t *testing.T) {
	driver := newFakeBackend()
	worker := newWorkerService(t.Context(), driver, nil, nil)
	worker.epoch.Store(1)
	if _, err := worker.RunWorkload(t.Context(), runRequest(actorUID)); err != nil {
		t.Fatal(err)
	}
	before := driver.session.reads.Load()
	if _, err := worker.TerminateWorkload(t.Context(), terminateRequest(actorUID)); err != nil {
		t.Fatal(err)
	}
	if got := driver.session.reads.Load(); got != before+1 {
		t.Fatalf("readings = %d, want %d including final", got, before+1)
	}
}

// A failed teardown must not let the control plane run a second copy elsewhere.
func TestCrashCleanupFailureDoesNotReleaseActor(t *testing.T) {
	driver := newFakeBackend()
	driver.cleanupErr = errors.New("process cleanup failed")
	reporter := &fakeCrashReporter{reported: make(chan crashRecord, 1)}
	worker := newWorkerService(t.Context(), driver, reporter, nil)
	req := runRequest(actorUID)
	h := &hostedActor{request: req, attribution: workerstats.ActorAttributionFromRequest(req), usage: workerstats.NewActivation(time.Now(), false), session: driver.session, ready: true}
	worker.actor = h
	close(driver.session.done)
	worker.watch(h, driver.session)
	if len(reporter.reported) != 0 {
		t.Fatal("crash reported before cleanup succeeded")
	}
	if worker.actor != h {
		t.Fatal("dirty actor capacity released")
	}
}

func TestCrashReportRetainsSlotUntilResolved(t *testing.T) {
	driver := newFakeBackend()
	hold := make(chan struct{})
	reporter := &fakeCrashReporter{reported: make(chan crashRecord, 1), release: hold}
	worker := newWorkerService(t.Context(), driver, reporter, nil)
	worker.epoch.Store(3)
	req := runRequest(actorUID)
	h := &hostedActor{request: req, attribution: workerstats.ActorAttributionFromRequest(req), usage: workerstats.NewActivation(time.Now(), false), session: driver.session, ready: true}
	worker.actor = h
	close(driver.session.done)
	done := make(chan struct{})
	go func() { worker.watch(h, driver.session); close(done) }()
	defer func() {
		close(hold)
		<-done
		worker.mu.Lock()
		defer worker.mu.Unlock()
		if worker.actor != nil {
			t.Error("resolved report retained slot")
		}
	}()
	<-reporter.reported
	for _, uid := range []string{actorUID, otherUID} {
		if _, err := worker.RunWorkload(t.Context(), runRequest(uid)); apierror.Code(err) != codes.ResourceExhausted {
			t.Errorf("actor %s reused pending-report slot: %v", uid, err)
		}
	}
	if _, err := worker.TerminateWorkload(t.Context(), terminateRequest(actorUID)); err != nil {
		t.Fatal(err)
	}
	if _, err := worker.RunWorkload(t.Context(), runRequest(actorUID)); apierror.Code(err) != codes.ResourceExhausted {
		t.Errorf("terminate released pending-report slot: %v", err)
	}
}
