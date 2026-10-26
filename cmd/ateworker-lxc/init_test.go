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
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	specs "github.com/opencontainers/runtime-spec/specs-go"
)

func TestActorInitSubprocess(t *testing.T) {
	path := os.Getenv("ATE_TEST_INIT_SPEC")
	if path == "" {
		return
	}
	if err := actorInit(path); err != nil {
		os.Stderr.WriteString(err.Error())
		os.Exit(1)
	}
	os.Exit(1)
}

// The actor's env is applied only after entering its rootfs and namespaces.
func TestActorInitPreservesEnvironment(t *testing.T) {
	spec := &specs.Spec{Process: &specs.Process{Args: []string{"/usr/bin/env"}, Cwd: "/", User: specs.User{UID: uint32(os.Getuid()), GID: uint32(os.Getgid())}, Env: []string{"VALUE=with spaces\nand newline", "PATH=/usr/bin:/bin"}}}
	data, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestActorInitSubprocess$")
	cmd.Env = append(os.Environ(), "ATE_TEST_INIT_SPEC="+path, "WORKER_SECRET=not-an-actor-variable")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("actor init: %v: %s", err, out)
	}
	if !strings.Contains(string(out), "VALUE=with spaces\nand newline\n") || strings.Contains(string(out), "WORKER_SECRET") || strings.Contains(string(out), "ATE_TEST_INIT_SPEC") {
		t.Fatalf("actor env changed or worker env leaked: %q", out)
	}
}
