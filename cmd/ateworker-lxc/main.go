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

// Command ateworker-lxc registers an experimental shared-kernel worker.
// It runs one actor per worker without memory checkpoints.
package main

import (
	"context"
	"errors"
	"fmt"
	"go.ate.dev/substrate/internal/actorlog"
	"go.ate.dev/substrate/internal/ateworker/actornet"
	"go.ate.dev/substrate/internal/ateworker/cgroup"
	"go.ate.dev/substrate/internal/workerstats"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"cloud.google.com/go/compute/metadata"
	"github.com/spf13/pflag"
	"go.ate.dev/substrate/internal/ateinterceptors"
	"go.ate.dev/substrate/internal/ateworker/pid1"
	"go.ate.dev/substrate/internal/ateworker/register"
	"go.ate.dev/substrate/internal/atunnel"
	"go.ate.dev/substrate/internal/nodepath"
	"go.ate.dev/substrate/internal/otlprelay"
	"go.ate.dev/substrate/internal/preview"
	"go.ate.dev/substrate/internal/proto/ateworkerpb"
	"go.ate.dev/substrate/internal/resources"
	"go.ate.dev/substrate/internal/serverboot"
	"go.ate.dev/substrate/internal/version"
	"go.ate.dev/substrate/pkg/sandboxclass"
	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc"
)

type options struct {
	podUID, podIPs, logLevel, readinessAddress, relaySocket string
	showVersion                                             bool
	preview                                                 []string
	tunnel                                                  *atunnel.Config
}

func newOptions(fs *pflag.FlagSet) *options {
	o := &options{tunnel: atunnel.RegisterFlags(fs)}
	fs.StringVar(&o.podUID, "pod-uid", "", "The UID of the current pod")
	fs.StringVar(&o.podIPs, "pod-ips", "", "The comma-separated IPs of the current pod")
	fs.StringVar(&o.logLevel, "log-level", "info", "Minimum log level: debug, info, warn, or error.")
	fs.BoolVar(&o.showVersion, "version", false, "Print version and exit.")
	fs.StringSliceVar(&o.preview, "preview", nil, "Preview gates to enable.")
	fs.StringVar(&o.readinessAddress, "readiness-listen-address", "0.0.0.0:8080", "HTTP health and readiness listen address")
	fs.StringVar(&o.relaySocket, "otlp-relay-socket", nodepath.AteletOTLPSocketPath(), "Unix socket of atelet's OTLP relay")
	return o
}

func (o *options) reportConfig() register.ReportConfig {
	return register.ReportConfig{
		SocketPath:           nodepath.WorkerSupportSocket,
		CredentialBundlePath: o.tunnel.CredentialBundle,
		TrustBundlePath:      o.tunnel.TrustBundle,
		AteletSPIFFEID:       o.tunnel.BrokerIdentity,
		Actors:               1,
		PodIPs:               register.ParsePodIPs(o.podIPs),
		SandboxClass:         sandboxclass.LXC,
		RuntimesFile:         nodepath.WorkerSandboxRuntimesPath(o.podUID),
	}
}

func main() {
	if len(os.Args) == 3 && os.Args[1] == "actor-init" {
		if err := actorInit(os.Args[2]); err != nil {
			fmt.Fprintln(os.Stderr, "actor init failed:", err)
			os.Exit(1)
		}
		return
	}
	pid1.RunIfPID1()
	o := newOptions(pflag.CommandLine)
	pflag.Parse()
	if o.showVersion {
		fmt.Println(version.String())
		return
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()
	if err := run(ctx, o); err != nil && !errors.Is(err, context.Canceled) {
		slog.ErrorContext(ctx, "LXC worker failed", slog.Any("err", err))
		os.Exit(1)
	}
}

func run(ctx context.Context, o *options) error {
	logWriter := actorlog.NewSyncedWriter(os.Stdout)
	serverboot.InitLoggerWithWriter(logWriter)
	if err := serverboot.SetLogLevel(o.logLevel); err != nil {
		return err
	}
	if err := preview.Init(o.preview...); err != nil {
		return err
	}
	if err := resources.ValidateWorkerPodUID(o.podUID); err != nil {
		return err
	}
	cfg := o.reportConfig()
	if len(cfg.PodIPs) == 0 {
		return errors.New("--pod-ips is required")
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	const serviceName = "ateworker-lxc"
	relay, err := otlprelay.Dial(ctx, o.relaySocket)
	if err != nil {
		slog.ErrorContext(ctx, "Failed to connect to the OTLP relay; using the configured OTLP endpoint", slog.Any("err", err))
	}
	if relay != nil {
		defer relay.Close()
	}
	tp, err := serverboot.InitTracing(ctx, serverboot.TracingOptions{ServiceName: serviceName, Sampling: serverboot.ResolveTraceSampling(ctx, serverboot.ParentRatioSampling(serverboot.ControlPlaneTraceRatio)), ExporterConn: relay, RelayCapable: true})
	if err != nil {
		return err
	}
	defer serverboot.ShutdownProvider("TracerProvider", tp.Shutdown)
	mp, err := serverboot.InitMetricsPushOnlyVia(ctx, serviceName, relay)
	if err != nil {
		return err
	}
	defer serverboot.ShutdownProvider("MeterProvider", mp.Shutdown)
	lp, err := serverboot.InitLogging(ctx, serverboot.LoggingOptions{ServiceName: serviceName, Exporter: serverboot.ResolveLogsExporter(ctx), ExporterConn: relay, RelayCapable: true})
	if err != nil {
		return err
	}
	if lp != nil {
		defer serverboot.ShutdownProvider("LoggerProvider", lp.Shutdown)
	}

	if err := cgroup.Delegate(ctx); err != nil {
		return err
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	tunnel, err := atunnel.Start(ctx, *o.tunnel, actornet.ActorHTTPUpstream)
	if err != nil {
		return err
	}
	reporter, err := register.NewCrashReporter(cfg)
	if err != nil {
		return err
	}
	actorLogger := actorlog.NewActorLogger(logWriter, metadata.OnGCE())
	backend := newLXCBackend(filepath.Join(nodepath.WorkerPath(o.podUID), "lxc"), executable, actorLogger, tunnel)
	usageStdout := workerstats.NewStdoutHandler(logWriter)
	defer usageStdout.Close()
	usage := workerstats.NewUsageEmitter(lp, usageStdout, workerstats.Pool{Namespace: os.Getenv("POD_NAMESPACE"), Name: os.Getenv("WORKER_POOL_NAME")})
	worker := newWorkerService(ctx, backend, reporter, usage)
	if err := worker.recover(ctx); err != nil {
		return fmt.Errorf("recovering LXC workers: %w", err)
	}
	if err := os.MkdirAll(nodepath.WorkerPath(o.podUID), 0o700); err != nil {
		return err
	}
	defer workerstats.StartSampler(ctx, time.Minute, worker.sweepUsage)()
	socket := nodepath.WorkerSocketPath(o.podUID)
	if err := os.Remove(socket); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	listener, err := net.Listen("unix", socket)
	if err != nil {
		return err
	}
	defer func() {
		if err := listener.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			slog.ErrorContext(ctx, "Failed to close worker listener", slog.Any("err", err))
		}
		if err := os.Remove(socket); err != nil && !errors.Is(err, os.ErrNotExist) {
			slog.ErrorContext(ctx, "Failed to remove worker socket", slog.Any("err", err))
		}
	}()
	server := grpc.NewServer(grpc.StatsHandler(otelgrpc.NewServerHandler()), grpc.UnaryInterceptor(ateinterceptors.InternalServerUnaryInterceptor))
	ateworkerpb.RegisterWorkerServer(server, worker)
	readiness := &serverboot.Readiness{}
	readiness.AddCheck("registered", worker.epoch.Registered)
	group, ctx := errgroup.WithContext(ctx)
	group.Go(func() error { return register.Report(ctx, cfg, &worker.epoch) })
	group.Go(func() error { return server.Serve(listener) })
	group.Go(func() error {
		<-ctx.Done()
		readiness.MarkNotReady()
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		err := worker.shutdown(shutdownCtx)
		server.GracefulStop()
		return err
	})
	go serverboot.StartHealthServer(ctx, o.readinessAddress, readiness)
	slog.InfoContext(ctx, "LXC worker serving", slog.String("socket", socket))
	return group.Wait()
}
