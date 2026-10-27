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
	"fmt"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	specs "github.com/opencontainers/runtime-spec/specs-go"
	"go.ate.dev/substrate/internal/nodepath"
)

// lxcConfig keeps actor argv and env in the OCI spec, never in host config lines.
func lxcConfig(uid, bundle, helper, executable string, spec *specs.Spec) ([]byte, error) {
	if spec.Root == nil || spec.Process == nil || len(spec.Process.Args) == 0 {
		return nil, fmt.Errorf("OCI spec requires a root and process args")
	}
	if spec.Root.Path != "rootfs" {
		return nil, fmt.Errorf("OCI root must be the bundle's rootfs")
	}
	for _, v := range []string{uid, bundle, helper, executable} {
		if strings.ContainsAny(v, "\n\r\x00") {
			return nil, fmt.Errorf("invalid LXC path")
		}
	}
	var out strings.Builder
	fmt.Fprintf(&out, "lxc.rootfs.path = dir:%s\nlxc.uts.name = actor\nlxc.namespace.share.net = %s\nlxc.cgroup.relative = 1\nlxc.apparmor.profile = unconfined\nlxc.mount.auto = proc:mixed sys:ro cgroup:rw\nlxc.autodev = 1\nlxc.pty.max = 0\nlxc.tty.max = 0\nlxc.environment = container=lxc\n", filepath.Join(bundle, "rootfs"), nodepath.ActorNetNSPath(uid))
	if spec.Root.Readonly {
		out.WriteString("lxc.rootfs.options = ro\n")
	}
	mount := func(source, destination, options string) {
		fmt.Fprintf(&out, "lxc.mount.entry = %s %s none %s 0 0\n", fstabPath(source), fstabPath(destination), options)
	}
	for _, m := range spec.Mounts {
		if strings.ContainsAny(m.Source+m.Destination, "\n\r\x00") || !path.IsAbs(m.Destination) || path.Clean(m.Destination) != m.Destination || m.Destination == "/" {
			return nil, fmt.Errorf("invalid OCI mount path")
		}
		if (m.Type == "proc" && m.Destination == "/proc") || (m.Type == "sysfs" && m.Destination == "/sys") || (m.Type == "tmpfs" && m.Destination == "/dev") {
			continue
		}
		if m.Type != "bind" || !filepath.IsAbs(m.Source) {
			return nil, fmt.Errorf("unsupported OCI mount type %q", m.Type)
		}
		options := []string{"bind"}
		for _, v := range m.Options {
			switch v {
			case "bind":
			case "ro", "rw", "nosuid", "nodev", "noexec":
				options = append(options, v)
			default:
				return nil, fmt.Errorf("unsupported OCI mount option %q", v)
			}
		}
		options = append(options, "create=dir")
		mount(m.Source, strings.TrimPrefix(m.Destination, "/"), strings.Join(options, ","))
	}
	mount(executable, helper+"/init", "bind,ro,create=file")
	mount(filepath.Join(bundle, "config.json"), helper+"/config.json", "bind,ro,create=file")
	if spec.Linux != nil && spec.Linux.Resources != nil {
		res := spec.Linux.Resources
		if res.Memory != nil && res.Memory.Limit != nil {
			fmt.Fprintf(&out, "lxc.cgroup2.memory.max = %d\n", *res.Memory.Limit)
		}
		if res.CPU != nil && res.CPU.Quota != nil && res.CPU.Period != nil {
			fmt.Fprintf(&out, "lxc.cgroup2.cpu.max = %d %d\n", *res.CPU.Quota, *res.CPU.Period)
		}
	}
	return []byte(out.String()), nil
}

func fstabPath(s string) string {
	r := strings.NewReplacer("\\", `\134`, " ", `\040`, "\t", `\011`)
	return r.Replace(s)
}

func lxcName(uid, container string) string { return uid + "-" + container }
func initArguments(helper string) []string {
	return []string{"/" + helper + "/init", "actor-init", "/" + helper + "/config.json"}
}

func limitValue(value int64) string {
	if value <= 0 {
		return "max"
	}
	return strconv.FormatInt(value, 10)
}
