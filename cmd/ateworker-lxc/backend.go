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
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	specs "github.com/opencontainers/runtime-spec/specs-go"
	"go.ate.dev/substrate/internal/actorlog"
	"go.ate.dev/substrate/internal/ateworker/actornet"
	"go.ate.dev/substrate/internal/ateworker/cgroup"
	"go.ate.dev/substrate/internal/ateworker/cgroupstats"
	"go.ate.dev/substrate/internal/ateworker/child"
	"go.ate.dev/substrate/internal/atunnel"
	"go.ate.dev/substrate/internal/imagecache"
	"go.ate.dev/substrate/internal/netns"
	"go.ate.dev/substrate/internal/nodepath"
	"go.ate.dev/substrate/internal/proto/ateworkerpb"
	"go.ate.dev/substrate/internal/resources"
	"go.ate.dev/substrate/internal/wakeupprobe"
	"go.ate.dev/substrate/internal/workerstats"
	"k8s.io/apimachinery/pkg/util/wait"
)

type containerRecord struct {
	Name   string
	Helper string
}
type actorRecord struct {
	Attribution resources.ActorAttribution
	Dirs        *ateworkerpb.ActorDirs
	Containers  []containerRecord
}

type lxcBackend struct {
	root, executable string
	logger           *actorlog.ActorLogger
	tunnel           *atunnel.Tunnel
	mu               sync.Mutex
	sessions         map[string]*lxcSession
}

type lxcSession struct {
	uid       string
	done      chan struct{}
	once      sync.Once
	mu        sync.Mutex
	exit      string
	processes []*child.Process
	logs      []io.WriteCloser
	network   *actornet.SandboxSession
}

func newLXCBackend(root, executable string, logger *actorlog.ActorLogger, tunnel *atunnel.Tunnel) *lxcBackend {
	return &lxcBackend{root: root, executable: executable, logger: logger, tunnel: tunnel, sessions: map[string]*lxcSession{}}
}
func (s *lxcSession) Done() <-chan struct{} { return s.done }
func (s *lxcSession) Exit() string          { s.mu.Lock(); defer s.mu.Unlock(); return s.exit }
func (s *lxcSession) Sample() (*ateworkerpb.WorkloadStatsSample, error) {
	sample, err := cgroupstats.Read(filepath.Join(cgroup.Root, s.uid))
	if err != nil {
		return nil, err
	}
	return &ateworkerpb.WorkloadStatsSample{Source: ateworkerpb.StatsSource_STATS_SOURCE_CGROUP, MemoryCurrentBytes: sample.MemoryCurrentBytes, MemoryPeakBytes: sample.MemoryPeakBytes, MemoryWorkingSetBytes: sample.MemoryWorkingSetBytes, CpuUsageUsec: sample.CPUUsageUsec}, nil
}

func (b *lxcBackend) Start(ctx context.Context, req *ateworkerpb.RunWorkloadRequest) (_ runtimeSession, retErr error) {
	uid := req.ActorUid
	record := actorRecord{Attribution: workerstats.ActorAttributionFromRequest(req), Dirs: req.ActorDirs}
	for _, c := range req.Spec.Containers {
		record.Containers = append(record.Containers, containerRecord{Name: c.Name, Helper: ".ate-lxc-" + rand.Text()})
	}
	if err := b.persist(record); err != nil {
		return nil, err
	}
	session := &lxcSession{uid: uid, done: make(chan struct{})}
	b.mu.Lock()
	b.sessions[uid] = session
	b.mu.Unlock()
	egress, err := b.tunnel.PrepareEgress(ctx, record.Attribution, req.EgressGateway)
	if err != nil {
		return nil, err
	}
	network, err := actornet.ServeSandbox(ctx, actornet.SandboxNetworkConfig{ActorUID: uid, Veth: true, EgressPort: b.tunnel.EgressPort, DNSPolicy: b.tunnel.ActorEgressPolicy(record.Attribution)}, b.tunnel.Egress, b.tunnel.DNSRelay)
	if err != nil {
		return nil, err
	}
	session.network = network
	if err := b.tunnel.ActivateEgress(record.Attribution, egress); err != nil {
		return nil, err
	}
	leaf, err := cgroup.OpenActorLeaf(uid, req.GetResources().GetLimits().GetCpuMillicores())
	if err != nil {
		return nil, err
	}
	defer func() { retErr = errors.Join(retErr, leaf.Close()) }()
	for name, value := range map[string]string{"memory.max": limitValue(req.GetResources().GetLimits().GetMemoryBytes()), "pids.max": "512"} {
		if err := writeCgroupValue(filepath.Join(cgroup.Root, uid, name), value); err != nil {
			return nil, err
		}
	}
	for _, container := range record.Containers {
		bundle := filepath.Join(req.ActorDirs.OciBundleDir, container.Name)
		if err := imagecache.SetupBundleRootfs(bundle); err != nil {
			return nil, err
		}
		rootfs := filepath.Join(bundle, "rootfs")
		if err := actornet.WriteRootfsEtcFiles(rootfs, "actor"); err != nil {
			return nil, err
		}
		data, err := os.ReadFile(filepath.Join(bundle, "config.json"))
		if err != nil {
			return nil, err
		}
		var spec specs.Spec
		if err := json.Unmarshal(data, &spec); err != nil {
			return nil, err
		}
		config, err := lxcConfig(uid, bundle, container.Helper, b.executable, &spec)
		if err != nil {
			return nil, err
		}
		if err := os.Mkdir(filepath.Join(rootfs, container.Helper), 0o700); err != nil {
			return nil, err
		}
		name := lxcName(uid, container.Name)
		configDir := filepath.Join(b.root, uid, "lxc", name)
		if err := os.MkdirAll(configDir, 0o700); err != nil {
			return nil, err
		}
		if err := os.WriteFile(filepath.Join(configDir, "config"), config, 0o600); err != nil {
			return nil, err
		}
		log, err := b.logger.StartJSONLogPipe(record.Attribution, container.Name)
		if err != nil {
			return nil, err
		}
		session.logs = append(session.logs, log)
		args := []string{"-P", filepath.Join(b.root, uid, "lxc"), "-n", name, "-F", "--"}
		args = append(args, initArguments(container.Helper)...)
		cmd := exec.Command("lxc-start", args...)
		cmd.SysProcAttr = leaf.SysProcAttr()
		cmd.Stdout = log
		cmd.Stderr = log
		process, err := child.Start(cmd)
		if err != nil {
			return nil, err
		}
		session.processes = append(session.processes, process)
		go func(p *child.Process) {
			<-p.Done()
			session.once.Do(func() { session.mu.Lock(); session.exit = p.Exit(); session.mu.Unlock(); close(session.done) })
		}(process)
		err = wait.PollUntilContextTimeout(ctx, 50*time.Millisecond, 30*time.Second, true, func(ctx context.Context) (bool, error) {
			select {
			case <-process.Done():
				return false, fmt.Errorf("LXC monitor exited during startup: %s", process.Exit())
			default:
			}
			state, err := b.state(ctx, uid, container.Name)
			if err != nil {
				return false, err
			}
			return state == "RUNNING", nil
		})
		if err != nil {
			return nil, err
		}
	}
	if err := wakeupprobe.WaitAll(ctx, req.Spec.Containers, actornet.ActorVethIP, wakeupprobe.DialFunc(network.Dialer())); err != nil {
		return nil, err
	}
	select {
	case <-session.done:
		return nil, fmt.Errorf("LXC monitor exited during startup: %s", session.Exit())
	default:
	}
	if err := b.tunnel.ActivateIngress(record.Attribution, network.Dialer()); err != nil {
		return nil, err
	}
	b.logger.EmitLifecycleLog(ctx, "Actor started", record.Attribution)
	return session, nil
}

func (b *lxcBackend) state(ctx context.Context, uid, container string) (string, error) {
	out, err := exec.CommandContext(ctx, "lxc-info", "-P", filepath.Join(b.root, uid, "lxc"), "-n", lxcName(uid, container), "-sH").Output()
	if err != nil {
		return "", fmt.Errorf("querying LXC state: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

func (b *lxcBackend) Cleanup(ctx context.Context, uid string, dirs *ateworkerpb.ActorDirs) error {
	if err := validateActorPaths(uid, dirs); err != nil {
		return err
	}
	b.mu.Lock()
	session := b.sessions[uid]
	b.mu.Unlock()
	record, err := b.readRecord(uid)
	if errors.Is(err, os.ErrNotExist) {
		if session != nil {
			return errors.New("active LXC actor has no committed record")
		}
		return b.removeIncompleteState(uid)
	}
	if err != nil {
		return err
	}
	if err := b.tunnel.Deactivate(ctx, record.Attribution); err != nil {
		return err
	}
	var cleanupErr error
	for _, container := range record.Containers {
		config := filepath.Join(b.root, uid, "lxc", lxcName(uid, container.Name), "config")
		if _, err := os.Stat(config); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			cleanupErr = errors.Join(cleanupErr, err)
			continue
		}
		state, err := b.state(ctx, uid, container.Name)
		if err != nil {
			cleanupErr = errors.Join(cleanupErr, err)
			continue
		}
		if state != "STOPPED" {
			stopCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
			err := exec.CommandContext(stopCtx, "lxc-stop", "-P", filepath.Join(b.root, uid, "lxc"), "-n", lxcName(uid, container.Name), "-t", "10").Run()
			cancel()
			if err != nil {
				slog.WarnContext(ctx, "Graceful LXC stop failed; killing the actor cgroup", slog.Any("err", err))
			}
		}
	}
	if err := cgroup.KillActorLeaf(ctx, uid); err != nil {
		cleanupErr = errors.Join(cleanupErr, err)
	}
	// KillActorLeaf can be a no-op on an old kernel. Never recycle a populated leaf.
	events, err := os.ReadFile(filepath.Join(cgroup.Root, uid, "cgroup.events"))
	if err == nil && !strings.Contains(string(events), "populated 0") {
		cleanupErr = errors.Join(cleanupErr, errors.New("actor cgroup is still populated"))
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		cleanupErr = errors.Join(cleanupErr, err)
	}
	if cleanupErr != nil {
		return cleanupErr
	}
	if session != nil {
		for _, p := range session.processes {
			select {
			case <-p.Done():
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		for _, log := range session.logs {
			if err := log.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
				return err
			}
		}
	}
	for _, container := range record.Containers {
		helper := filepath.Join(record.Dirs.OciBundleDir, container.Name, "rootfs", container.Helper)
		if err := os.RemoveAll(helper); err != nil {
			return err
		}
	}
	if err := imagecache.UnmountAllUnder(record.Dirs.OciBundleDir); err != nil {
		return err
	}
	if session != nil && session.network != nil {
		if err := session.network.Close(ctx); err != nil {
			return err
		}
	} else {
		for _, name := range []string{nodepath.ActorNetNSName(uid), actornet.SandboxGatewayNetNSName(uid)} {
			if err := netns.RemoveNamed(name); err != nil {
				return err
			}
		}
	}
	if err := removeCgroupTree(filepath.Join(cgroup.Root, uid)); err != nil {
		return err
	}
	if err := os.RemoveAll(filepath.Join(b.root, uid)); err != nil {
		return err
	}
	b.mu.Lock()
	delete(b.sessions, uid)
	b.mu.Unlock()
	return nil
}

func writeCgroupValue(path, value string) error {
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	_, err = f.WriteString(value)
	return errors.Join(err, f.Close())
}

func removeCgroupTree(path string) error {
	entries, err := os.ReadDir(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() {
			if err := removeCgroupTree(filepath.Join(path, entry.Name())); err != nil {
				return err
			}
		}
	}
	return os.Remove(path)
}

func (b *lxcBackend) persist(record actorRecord) error {
	dir := filepath.Join(b.root, record.Attribution.UID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".actor-")
	if err != nil {
		return err
	}
	defer func() {
		if err := os.Remove(f.Name()); err != nil && !errors.Is(err, os.ErrNotExist) {
			slog.Error("Removing temporary LXC record failed", slog.Any("err", err))
		}
	}()
	_, writeErr := f.Write(data)
	syncErr := f.Sync()
	closeErr := f.Close()
	if err := errors.Join(writeErr, syncErr, closeErr); err != nil {
		return err
	}
	return os.Rename(f.Name(), filepath.Join(dir, "actor.json"))
}

func (b *lxcBackend) readRecord(uid string) (*actorRecord, error) {
	f, err := os.Open(filepath.Join(b.root, uid, "actor.json"))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var record actorRecord
	decoder := json.NewDecoder(f)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&record); err != nil {
		return nil, err
	}
	if record.Attribution.UID != uid {
		return nil, errors.New("LXC recovery actor UID mismatch")
	}
	if err := validateActorPaths(uid, record.Dirs); err != nil {
		return nil, err
	}
	for _, container := range record.Containers {
		if container.Name == "" || strings.ContainsAny(container.Name, "/\\\n\r\x00") || container.Name == "." || container.Name == ".." {
			return nil, errors.New("invalid LXC container name")
		}
		if !strings.HasPrefix(container.Helper, ".ate-lxc-") || filepath.Base(container.Helper) != container.Helper {
			return nil, errors.New("invalid LXC helper path")
		}
	}
	return &record, nil
}

func (b *lxcBackend) Recover(ctx context.Context) ([]resources.ActorAttribution, error) {
	entries, err := os.ReadDir(b.root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var actors []resources.ActorAttribution
	var failures error
	for _, entry := range entries {
		if !entry.IsDir() {
			return nil, errors.New("unexpected entry in LXC state directory")
		}
		uid := entry.Name()
		if err := resources.ValidateWorkerPodUID(uid); err != nil {
			return nil, err
		}
		record, err := b.readRecord(uid)
		if errors.Is(err, os.ErrNotExist) {
			failures = errors.Join(failures, b.removeIncompleteState(uid))
			continue
		}
		if err != nil {
			failures = errors.Join(failures, err)
			continue
		}
		if err := b.Cleanup(ctx, uid, record.Dirs); err != nil {
			failures = errors.Join(failures, err)
			continue
		}
		actors = append(actors, record.Attribution)
	}
	return actors, failures
}

// An actor directory precedes its committed record, but never runtime allocation.
func (b *lxcBackend) removeIncompleteState(uid string) error {
	dir := filepath.Join(b.root, uid)
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !entry.Type().IsRegular() || !strings.HasPrefix(entry.Name(), ".actor-") {
			return errors.New("unrecognized LXC state without a committed actor record")
		}
	}
	for _, entry := range entries {
		if err := os.Remove(filepath.Join(dir, entry.Name())); err != nil {
			return err
		}
	}
	return os.Remove(dir)
}
