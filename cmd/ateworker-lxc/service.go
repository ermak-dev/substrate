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
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"go.ate.dev/substrate/internal/actorlock"
	"go.ate.dev/substrate/internal/apierror"
	"go.ate.dev/substrate/internal/ateworker/register"
	"go.ate.dev/substrate/internal/ateworkervalidation"
	"go.ate.dev/substrate/internal/nodepath"
	"go.ate.dev/substrate/internal/proto/ateworkerpb"
	"go.ate.dev/substrate/internal/resources"
	"go.ate.dev/substrate/internal/workerstats"
	"google.golang.org/protobuf/proto"
)

// runtimeBackend confines all process, mount and network operations to this worker.
type runtimeBackend interface {
	Start(context.Context, *ateworkerpb.RunWorkloadRequest) (runtimeSession, error)
	Cleanup(context.Context, string, *ateworkerpb.ActorDirs) error
	Recover(context.Context) ([]resources.ActorAttribution, error)
}

type runtimeSession interface {
	Done() <-chan struct{}
	Exit() string
	Sample() (*ateworkerpb.WorkloadStatsSample, error)
}

type crashReporter interface {
	ReportCrashed(context.Context, resources.ActorAttribution, string) error
}

type hostedActor struct {
	request         *ateworkerpb.RunWorkloadRequest
	attribution     resources.ActorAttribution
	usage           *workerstats.Activation
	session         runtimeSession
	ready, stopping bool
	// A crash report must finish before this slot can be reused.
	reporting bool
}

type workerService struct {
	ateworkerpb.UnimplementedWorkerServer
	ctx      context.Context
	backend  runtimeBackend
	reporter crashReporter
	usage    *workerstats.UsageEmitter
	epoch    register.Epoch
	locks    actorlock.Locks
	inflight *actorlock.InFlight
	mu       sync.Mutex
	actor    *hostedActor
	draining bool
}

func newWorkerService(ctx context.Context, backend runtimeBackend, reporter crashReporter, usage *workerstats.UsageEmitter) *workerService {
	return &workerService{ctx: ctx, backend: backend, reporter: reporter, usage: usage, inflight: actorlock.NewInFlight()}
}

func validateActorPaths(uid string, dirs *ateworkerpb.ActorDirs) error {
	root := filepath.Join(nodepath.ActorsDir, uid)
	if dirs.GetRootDir() != root {
		return apierror.InvalidArgument("actor root does not belong to actor %q", uid)
	}
	rel, err := filepath.Rel(root, dirs.GetOciBundleDir())
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, "../") {
		return apierror.InvalidArgument("OCI bundle directory does not belong to actor %q", uid)
	}
	return nil
}

func (s *workerService) RunWorkload(ctx context.Context, req *ateworkerpb.RunWorkloadRequest) (_ *ateworkerpb.RunWorkloadResponse, retErr error) {
	if err := ateworkervalidation.ValidateRunWorkloadRequest(ctx, req); err != nil {
		return nil, err
	}
	epoch, err := s.epoch.Require()
	if err != nil {
		return nil, err
	}
	if err := validateActorPaths(req.ActorUid, req.ActorDirs); err != nil {
		return nil, err
	}
	if req.RunscPath != "" || len(req.RuntimeAssetPaths) != 0 {
		return nil, apierror.InvalidArgument("LXC uses the worker image, not external runtime assets")
	}
	if !s.locks.Lock(ctx, req.ActorUid) {
		return nil, ctx.Err()
	}
	defer s.locks.Unlock(req.ActorUid)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	s.mu.Lock()
	if s.draining {
		s.mu.Unlock()
		return nil, apierror.Unavailable("worker is shutting down")
	}
	if s.actor != nil {
		retry := s.actor.request.ActorUid == req.ActorUid && s.actor.ready && !s.actor.stopping
		s.mu.Unlock()
		if retry {
			return &ateworkerpb.RunWorkloadResponse{WorkerEpoch: epoch}, nil
		}
		return nil, apierror.ResourceExhausted("LXC worker is full")
	}
	h := &hostedActor{request: proto.Clone(req).(*ateworkerpb.RunWorkloadRequest), attribution: workerstats.ActorAttributionFromRequest(req), usage: workerstats.NewActivation(time.Now(), false)}
	s.actor = h
	release := s.inflight.Add(req.ActorUid, "run", cancel)
	s.mu.Unlock()
	defer release()
	defer func() {
		if retErr != nil {
			cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
			defer cancel()
			if err := s.cleanup(cleanupCtx, h); err != nil {
				retErr = errors.Join(retErr, fmt.Errorf("startup cleanup: %w", err))
			}
		}
	}()
	session, err := s.backend.Start(ctx, h.request)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	h.session = session
	h.ready = true
	s.mu.Unlock()
	sample, err := s.measure(ctx, h)
	if err != nil {
		slog.WarnContext(ctx, "Initial LXC usage reading failed", slog.Any("err", err))
	}
	h.usage.Initial(sample, func() { s.emit(ctx, "initial", sample) })
	go s.watch(h, session)
	return &ateworkerpb.RunWorkloadResponse{WorkerEpoch: epoch}, nil
}

func (s *workerService) cleanup(ctx context.Context, h *hostedActor) error {
	s.mu.Lock()
	ready := h.ready
	s.mu.Unlock()
	if ready {
		if _, err := s.measure(ctx, h); err != nil {
			slog.WarnContext(ctx, "Final LXC usage reading failed", slog.Any("err", err))
		}
	}
	s.mu.Lock()
	h.stopping = true
	h.ready = false
	s.mu.Unlock()
	if err := s.backend.Cleanup(ctx, h.attribution.UID, h.request.ActorDirs); err != nil {
		return err
	}
	s.mu.Lock()
	if s.actor == h && !h.reporting {
		s.actor = nil
	}
	s.mu.Unlock()
	h.usage.Final(func(sample *ateworkerpb.WorkloadStatsSample) {
		if sample != nil {
			s.emit(ctx, "final", sample)
		}
	})
	return nil
}

func (s *workerService) TerminateWorkload(ctx context.Context, req *ateworkerpb.TerminateWorkloadRequest) (*ateworkerpb.TerminateWorkloadResponse, error) {
	if err := ateworkervalidation.ValidateTerminateWorkloadRequest(ctx, req); err != nil {
		return nil, err
	}
	if err := validateActorPaths(req.ActorUid, req.ActorDirs); err != nil {
		return nil, err
	}
	if !s.locks.Lock(ctx, req.ActorUid) {
		return nil, ctx.Err()
	}
	defer s.locks.Unlock(req.ActorUid)
	s.mu.Lock()
	h := s.actor
	s.mu.Unlock()
	if h != nil && h.attribution.UID == req.ActorUid {
		if err := s.cleanup(ctx, h); err != nil {
			return nil, err
		}
	} else if err := s.backend.Cleanup(ctx, req.ActorUid, req.ActorDirs); err != nil {
		return nil, err
	}
	return &ateworkerpb.TerminateWorkloadResponse{}, nil
}

func (s *workerService) watch(h *hostedActor, session runtimeSession) {
	select {
	case <-session.Done():
	case <-s.ctx.Done():
		return
	}
	ctx := context.WithoutCancel(s.ctx)
	if !s.locks.Lock(ctx, h.attribution.UID) {
		return
	}
	s.mu.Lock()
	ours := s.actor == h && !h.stopping && !s.draining
	if ours {
		h.reporting = true
	}
	s.mu.Unlock()
	if !ours {
		s.locks.Unlock(h.attribution.UID)
		return
	}
	release := s.inflight.Add(h.attribution.UID, "crash-report", nil)
	defer release()
	message := "LXC monitor exited: " + session.Exit()
	cleanupCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	cleanupErr := s.cleanup(cleanupCtx, h)
	cancel()
	s.locks.Unlock(h.attribution.UID)
	if cleanupErr != nil {
		s.mu.Lock()
		h.reporting = false
		s.mu.Unlock()
		slog.ErrorContext(ctx, "LXC crash cleanup failed; retaining the actor", slog.Any("err", cleanupErr))
		return
	}
	var reportErr error
	if s.reporter != nil {
		reportCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
		defer cancel()
		reportErr = s.reporter.ReportCrashed(reportCtx, h.attribution, message)
		if reportErr != nil {
			slog.ErrorContext(ctx, "LXC crash report failed; retaining the actor", slog.Any("err", reportErr))
		}
	}
	s.mu.Lock()
	h.reporting = false
	if reportErr == nil && s.actor == h {
		s.actor = nil
	}
	s.mu.Unlock()
}

func (s *workerService) recover(ctx context.Context) error {
	actors, err := s.backend.Recover(ctx)
	if err != nil {
		return err
	}
	for _, a := range actors {
		if s.reporter != nil {
			if err := s.reporter.ReportCrashed(ctx, a, "LXC worker restarted"); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *workerService) shutdown(ctx context.Context) error {
	s.mu.Lock()
	s.draining = true
	s.inflight.CancelStartups()
	s.mu.Unlock()
	if !s.inflight.WaitIdle(ctx) {
		return ctx.Err()
	}
	s.mu.Lock()
	h := s.actor
	s.mu.Unlock()
	if h == nil {
		return nil
	}
	if !s.locks.Lock(ctx, h.attribution.UID) {
		return ctx.Err()
	}
	defer s.locks.Unlock(h.attribution.UID)
	return s.cleanup(ctx, h)
}

func (s *workerService) measure(ctx context.Context, h *hostedActor) (*ateworkerpb.WorkloadStatsSample, error) {
	s.mu.Lock()
	session, ready := h.session, h.ready
	s.mu.Unlock()
	if !ready {
		return nil, apierror.FailedPrecondition("LXC actor is not ready to measure")
	}
	sample, err := h.usage.Measure(ctx, func() (*ateworkerpb.WorkloadStatsSample, map[string]uint64, error) {
		raw, err := session.Sample()
		if err != nil {
			return nil, nil, err
		}
		sample := proto.Clone(raw).(*ateworkerpb.WorkloadStatsSample)
		sample.Atespace = h.attribution.Ref.Atespace
		sample.ActorName = h.attribution.Ref.Name
		sample.ActorUid = h.attribution.UID
		sample.ActorTemplateAtespace = h.attribution.TemplateAtespace
		sample.ActorTemplateName = h.attribution.TemplateName
		sample.SandboxClass = "lxc"
		sample.Source = ateworkerpb.StatsSource_STATS_SOURCE_CGROUP
		sample.ObservedAtUnixNano = time.Now().UnixNano()
		return sample, nil, nil
	})
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	current := s.actor == h && h.ready
	s.mu.Unlock()
	if !current {
		return nil, apierror.NotFound("actor activation ended while reading usage")
	}
	h.usage.Store(sample)
	return sample, nil
}

func (s *workerService) GetWorkloadStats(ctx context.Context, req *ateworkerpb.GetWorkloadStatsRequest) (*ateworkerpb.GetWorkloadStatsResponse, error) {
	if err := ateworkervalidation.ValidateGetWorkloadStatsRequest(ctx, req); err != nil {
		return nil, err
	}
	s.mu.Lock()
	h := s.actor
	s.mu.Unlock()
	if h == nil || h.attribution.UID != req.ActorUid {
		return nil, apierror.NotFound("actor is not hosted by this worker")
	}
	sample, err := s.measure(ctx, h)
	if err != nil {
		return nil, err
	}
	return &ateworkerpb.GetWorkloadStatsResponse{Sample: sample}, nil
}

func (s *workerService) GetActiveWorkloadStats(context.Context, *ateworkerpb.GetActiveWorkloadStatsRequest) (*ateworkerpb.GetActiveWorkloadStatsResponse, error) {
	s.mu.Lock()
	h := s.actor
	s.mu.Unlock()
	response := &ateworkerpb.GetActiveWorkloadStatsResponse{}
	if h != nil {
		sample := h.usage.Latest()
		if sample == nil {
			sample = h.usage.WithEpoch(&ateworkerpb.WorkloadStatsSample{Atespace: h.attribution.Ref.Atespace, ActorName: h.attribution.Ref.Name, ActorUid: h.attribution.UID, ActorTemplateAtespace: h.attribution.TemplateAtespace, ActorTemplateName: h.attribution.TemplateName, SandboxClass: "lxc", ObservedAtUnixNano: time.Now().UnixNano()})
		}
		response.Samples = []*ateworkerpb.WorkloadStatsSample{sample}
	}
	return response, nil
}

func (s *workerService) emit(ctx context.Context, kind string, sample *ateworkerpb.WorkloadStatsSample) {
	if s.usage != nil {
		s.usage.Emit(ctx, kind, sample)
	}
}

func (s *workerService) sweepUsage(ctx context.Context) {
	s.mu.Lock()
	h := s.actor
	s.mu.Unlock()
	if h == nil || !h.usage.Sampling() {
		return
	}
	sample, err := s.measure(ctx, h)
	if err != nil {
		slog.WarnContext(ctx, "LXC usage reading failed", slog.Any("err", err))
		return
	}
	h.usage.Periodic(sample, func() { s.emit(ctx, "periodic", sample) })
}
