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
	"testing"

	"github.com/spf13/pflag"
	"go.ate.dev/substrate/internal/nodepath"
	"go.ate.dev/substrate/internal/proto/ateworkerpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// The registration-only worker must never admit actors before lifecycle support.
func TestRegistrationConfig(t *testing.T) {
	fs := pflag.NewFlagSet("lxc", pflag.ContinueOnError)
	o := newOptions(fs)
	err := fs.Parse([]string{"--pod-uid=worker-pod", "--pod-ips=10.0.0.1,fd00::1", "--atunnel-credential-bundle=worker.pem", "--atunnel-trust-bundle=trust.pem", "--atunnel-broker-identity=broker", "--atunnel-client-identity=router", "--atunnel-listen-address=:443", "--atunnel-connect-listen-address=:8443", "--atunnel-egress-listen-address=:15001", "--atunnel-egress-trust-bundle=egress.pem", "--dns-egress-policy=enforce-in-atunnel"})
	if err != nil {
		t.Fatal(err)
	}
	cfg := o.reportConfig()
	if cfg.Actors != 0 || cfg.SandboxClass != "lxc" {
		t.Fatalf("unsafe registration: %+v", cfg)
	}
	if cfg.RuntimesFile != nodepath.WorkerSandboxRuntimesPath("worker-pod") || cfg.SocketPath != nodepath.WorkerSupportSocket {
		t.Fatalf("registration paths: %+v", cfg)
	}
	if len(cfg.PodIPs) != 2 || cfg.PodIPs[0] != "10.0.0.1" || cfg.PodIPs[1] != "fd00::1" {
		t.Fatalf("IPs = %v", cfg.PodIPs)
	}
	if cfg.CredentialBundlePath != "worker.pem" || cfg.TrustBundlePath != "trust.pem" || cfg.AteletSPIFFEID != "broker" {
		t.Fatalf("registration identity: %+v", cfg)
	}
}

func TestRegistrationOnlyWorker(t *testing.T) {
	worker := &registrationWorker{}
	if _, err := worker.RunWorkload(context.Background(), &ateworkerpb.RunWorkloadRequest{}); status.Code(err) != codes.Unimplemented {
		t.Fatalf("run error = %v", err)
	}
	if _, err := worker.CheckpointWorkload(context.Background(), &ateworkerpb.CheckpointWorkloadRequest{}); status.Code(err) != codes.Unimplemented {
		t.Fatalf("checkpoint error = %v", err)
	}
	if _, err := worker.RestoreWorkload(context.Background(), &ateworkerpb.RestoreWorkloadRequest{}); status.Code(err) != codes.Unimplemented {
		t.Fatalf("restore error = %v", err)
	}
	stats, err := worker.GetActiveWorkloadStats(context.Background(), &ateworkerpb.GetActiveWorkloadStatsRequest{})
	if err != nil || len(stats.GetSamples()) != 0 {
		t.Fatalf("empty worker stats = %v, %v", stats, err)
	}
}
