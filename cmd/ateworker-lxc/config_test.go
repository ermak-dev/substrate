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
	"os"
	"path/filepath"
	"strings"
	"testing"

	specs "github.com/opencontainers/runtime-spec/specs-go"
	"go.ate.dev/substrate/internal/nodepath"
)

func TestLXCConfig(t *testing.T) {
	spec := &specs.Spec{Root: &specs.Root{Path: "rootfs"}, Process: &specs.Process{Args: []string{"/sbin/init", "with spaces"}, Env: []string{"TOKEN=private\nvalue", "LD_PRELOAD=actor-library"}}, Mounts: []specs.Mount{{Type: "bind", Source: "/volumes/with space", Destination: "/data/with space", Options: []string{"bind", "ro"}}}}
	data, err := lxcConfig(actorUID, "/bundle", ".ate-runtime-test", "/usr/local/bin/ateworker-lxc", spec)
	if err != nil {
		t.Fatal(err)
	}
	config := string(data)
	for _, want := range []string{"lxc.rootfs.path = dir:/bundle/rootfs", "lxc.namespace.share.net = " + nodepath.ActorNetNSPath(actorUID), "lxc.cgroup.relative = 1", "lxc.apparmor.profile = unconfined", `/volumes/with\040space data/with\040space none bind,ro,create=dir`, ".ate-runtime-test/init", ".ate-runtime-test/config.json"} {
		if !strings.Contains(config, want) {
			t.Errorf("config missing %q:\n%s", want, config)
		}
	}
	if strings.Contains(config, "TOKEN") || strings.Contains(config, "LD_PRELOAD") || strings.Contains(config, "with spaces") {
		t.Fatal("actor argv/env was embedded into the host LXC config")
	}
}

func TestLXCConfigRejectsUnsafeMounts(t *testing.T) {
	for _, mount := range []specs.Mount{
		{Type: "bind", Source: "/volume\nlxc.include = /bad", Destination: "/data"},
		{Type: "bind", Source: "/volume", Destination: "/../escape"},
		{Type: "bind", Source: "/volume", Destination: "/"},
		{Type: "unknown", Source: "x", Destination: "/data"},
	} {
		spec := &specs.Spec{Root: &specs.Root{Path: "rootfs"}, Process: &specs.Process{Args: []string{"/sbin/init"}}, Mounts: []specs.Mount{mount}}
		if _, err := lxcConfig(actorUID, "/bundle", ".ate-test", "/worker", spec); err == nil {
			t.Errorf("accepted mount: %+v", mount)
		}
	}
}

func TestRecoverRejectsMalformedRecord(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, actorUID), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, actorUID, "actor.json"), []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	backend := newLXCBackend(root, "/worker", nil, nil)
	if _, err := backend.Recover(t.Context()); err == nil {
		t.Fatal("malformed recovery state ignored")
	}
}

func TestRecoverIncompleteRecord(t *testing.T) {
	for _, temporary := range []bool{false, true} {
		t.Run(map[bool]string{false: "empty directory", true: "uncommitted temporary record"}[temporary], func(t *testing.T) {
			root := t.TempDir()
			dir := filepath.Join(root, actorUID)
			if err := os.Mkdir(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			if temporary {
				if err := os.WriteFile(filepath.Join(dir, ".actor-interrupted"), []byte("partial json"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			backend := newLXCBackend(root, "/worker", nil, nil)
			actors, err := backend.Recover(t.Context())
			if err != nil || len(actors) != 0 {
				t.Fatalf("uncommitted startup: %v, %v", actors, err)
			}
			if _, err := os.Stat(dir); !os.IsNotExist(err) {
				t.Fatalf("incomplete state remains: %v", err)
			}
		})
	}
}
