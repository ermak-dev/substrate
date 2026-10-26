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
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"

	specs "github.com/opencontainers/runtime-spec/specs-go"
	"golang.org/x/sys/unix"
)

// actorInit runs as container PID 1 and replaces itself with the OCI process.
func actorInit(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var spec specs.Spec
	if err := json.Unmarshal(data, &spec); err != nil {
		return err
	}
	p := spec.Process
	if p == nil || len(p.Args) == 0 {
		return errors.New("OCI process args are required")
	}
	os.Clearenv()
	for _, entry := range p.Env {
		key, value, ok := strings.Cut(entry, "=")
		if !ok || key == "" {
			return errors.New("invalid actor environment entry")
		}
		if err := os.Setenv(key, value); err != nil {
			return errors.New("invalid actor environment value")
		}
	}
	if os.Getenv("container") == "" {
		if err := os.Setenv("container", "lxc"); err != nil {
			return err
		}
	}
	if err := os.Chdir(p.Cwd); err != nil {
		return fmt.Errorf("setting actor working directory: %w", err)
	}
	if os.Geteuid() == 0 {
		groups := make([]int, len(p.User.AdditionalGids))
		for i, g := range p.User.AdditionalGids {
			groups[i] = int(g)
		}
		if err := unix.Setgroups(groups); err != nil {
			return err
		}
	}
	if err := unix.Setgid(int(p.User.GID)); err != nil {
		return err
	}
	if err := unix.Setuid(int(p.User.UID)); err != nil {
		return err
	}
	executable, err := exec.LookPath(p.Args[0])
	if err != nil {
		return errors.New("actor executable not found")
	}
	return unix.Exec(executable, p.Args, os.Environ())
}
