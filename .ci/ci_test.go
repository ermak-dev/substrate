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

package ci

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

type job struct {
	ResourceGroup string   `yaml:"resource_group"`
	Stage         string   `yaml:"stage"`
	Tags          []string `yaml:"tags"`
	Script        []string `yaml:"script"`
	AllowFailure  bool     `yaml:"allow_failure"`
	Rules         []struct {
		If   string `yaml:"if"`
		When string `yaml:"when"`
	} `yaml:"rules"`
}

func TestPipeline(t *testing.T) {
	data, err := os.ReadFile("../.gitlab-ci.yml")
	if err != nil {
		t.Fatal(err)
	}
	var pipeline map[string]yaml.Node
	if err := yaml.Unmarshal(data, &pipeline); err != nil {
		t.Fatal(err)
	}
	var workflow struct {
		Rules []struct {
			If   string `yaml:"if"`
			When string `yaml:"when"`
		} `yaml:"rules"`
	}
	node := pipeline["workflow"]
	if err := node.Decode(&workflow); err != nil {
		t.Fatal(err)
	}
	for _, source := range []string{"push", "merge_request_event"} {
		if !slices.ContainsFunc(workflow.Rules, func(r struct {
			If   string `yaml:"if"`
			When string `yaml:"when"`
		}) bool {
			return strings.Contains(r.If, `"`+source+`"`) && r.When != "never"
		}) {
			t.Errorf("missing %s workflow", source)
		}
	}
	for _, mode := range []string{"test", "verify", "repeat"} {
		var j job
		node := pipeline["go-"+mode]
		if err := node.Decode(&j); err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(j.Tags, []string{"docker", "build"}) || !slices.Equal(j.Script, []string{".ci/run.sh " + mode}) || j.AllowFailure {
			t.Errorf("mandatory %s job: %+v", mode, j)
		}
	}
	var e2e job
	node = pipeline["lxc-e2e"]
	if err := node.Decode(&e2e); err != nil {
		t.Fatal(err)
	}
	if e2e.AllowFailure || !slices.Contains(e2e.Tags, "substrate-lxc-e2e") || !slices.Equal(e2e.Script, []string{".ci/e2e.sh"}) || len(e2e.Rules) != 1 || e2e.Rules[0].If != `$RUN_LXC_E2E == "true"` {
		t.Fatalf("unsafe e2e selection: %+v", e2e)
	}
}

func TestRunRejectsInvalidMode(t *testing.T) {
	for _, mode := range []string{"", "unknown"} {
		t.Run(mode, func(t *testing.T) {
			command := exec.CommandContext(t.Context(), "bash", "run.sh", mode)
			output, err := command.CombinedOutput()
			if err == nil || !strings.Contains(string(output), "Usage:") {
				t.Fatalf("expected usage refusal, got %v: %s", err, output)
			}
		})
	}
}

func TestE2ERequiresStand(t *testing.T) {
	command := exec.CommandContext(t.Context(), "bash", "e2e.sh")
	command.Env = append(os.Environ(), "LXC_E2E_STAND=")
	output, err := command.CombinedOutput()
	if err == nil || !strings.Contains(string(output), "LXC_E2E_STAND") {
		t.Fatalf("missing stand must fail: %v: %s", err, output)
	}
}

func TestContainerDispatch(t *testing.T) {
	for _, mode := range []string{"test", "verify", "repeat"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			log := filepath.Join(dir, "commands")
			for _, tool := range []string{"go", "git", "make"} {
				data, err := os.ReadFile("testdata/tool.sh")
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, tool), data, 0700); err != nil {
					t.Fatal(err)
				}
			}
			root := t.TempDir()
			for _, file := range []struct{ src, dest string }{{"checks.sh", ".ci/checks.sh"}, {"testdata/verify.sh", "hack/verify-all.sh"}, {"testdata/tool.sh", "hack/verify/postgresql-migrations.sh"}, {"testdata/tool.sh", "hack/run-tool.sh"}} {
				data, err := os.ReadFile(file.src)
				if err != nil {
					t.Fatal(err)
				}
				target := filepath.Join(root, file.dest)
				if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(target, data, 0700); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.MkdirAll(filepath.Join(root, "internal/plugins/gcp-secret-manager"), 0700); err != nil {
				t.Fatal(err)
			}
			command := exec.CommandContext(t.Context(), "bash", filepath.Join(root, ".ci/checks.sh"), mode)
			command.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"), "COMMAND_LOG="+log, "CI_DEFAULT_BRANCH=main", "CI_COMMIT_BRANCH=feature")
			output, err := command.CombinedOutput()
			if err != nil {
				t.Fatalf("dispatch: %v: %s", err, output)
			}
			data, err := os.ReadFile(log)
			if err != nil {
				t.Fatal(err)
			}
			text := string(data)
			switch mode {
			case "test":
				for _, want := range []string{"go test -race", "./...", "go test -race ./.ci"} {
					if !strings.Contains(text, want) {
						t.Errorf("missing %q in %s", want, text)
					}
				}
			case "verify":
				for _, want := range []string{"postgresql-migrations.sh origin/main", "make verify", "run-tool.sh golangci-lint run --config ../../../.golangci.yaml ./..."} {
					if !strings.Contains(text, want) {
						t.Errorf("missing %q: %s", want, text)
					}
				}
				if !strings.Contains(string(output), "verify-all.sh") {
					t.Errorf("verifier not invoked: %s", output)
				}
			case "repeat":
				if !strings.Contains(text, "-count=3 -shuffle=on") || !strings.Contains(text, "./...") || !strings.Contains(text, "./.ci") {
					t.Errorf("missing repeat checks: %s", text)
				}
			}
		})
	}
}

func TestDockerIsolation(t *testing.T) {
	for _, fail := range []string{"0", "7"} {
		t.Run(fail, func(t *testing.T) {
			dir := t.TempDir()
			data, err := os.ReadFile("testdata/docker.sh")
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "docker"), data, 0700); err != nil {
				t.Fatal(err)
			}
			log := filepath.Join(dir, "commands")
			root := t.TempDir()
			if err := os.MkdirAll(filepath.Join(root, ".ci"), 0700); err != nil {
				t.Fatal(err)
			}
			data, err = os.ReadFile("run.sh")
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, ".ci/run.sh"), data, 0700); err != nil {
				t.Fatal(err)
			}
			data, err = os.ReadFile("testdata/tool.sh")
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "git"), data, 0700); err != nil {
				t.Fatal(err)
			}
			command := exec.CommandContext(t.Context(), "bash", filepath.Join(root, ".ci/run.sh"), "verify")
			command.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"), "COMMAND_LOG="+log, "DOCKER_RUN_STATUS="+fail, "GITLAB_TOKEN=not-for-container", "CI_PROJECT_URL=https://git.example/project")
			output, err := command.CombinedOutput()
			if fail == "0" && err != nil {
				t.Fatalf("run: %v: %s", err, output)
			}
			if fail == "7" && (err == nil || err.(*exec.ExitError).ExitCode() != 7) {
				t.Fatalf("lost check failure: %v: %s", err, output)
			}
			data, err = os.ReadFile(log)
			if err != nil {
				t.Fatal(err)
			}
			text := string(data)
			for _, want := range []string{"--user", "--cap-drop=ALL", "--security-opt=no-new-privileges", "CI=true", "GOFLAGS=-modcacherw", "checks.sh", "image\nrm"} {
				if !strings.Contains(text, want) {
					t.Errorf("missing %q: %s", want, text)
				}
			}
			for _, forbidden := range []string{"--privileged", "docker.sock", "GITLAB_TOKEN", "--env-file", "--network=host"} {
				if strings.Contains(text, forbidden) {
					t.Errorf("unsafe %q in %s", forbidden, text)
				}
			}
		})
	}
}

func TestE2ETestsCurrentSource(t *testing.T) {
	root := t.TempDir()
	stand := t.TempDir()
	tools := t.TempDir()
	log := filepath.Join(root, "commands")
	for _, file := range []struct{ src, dest string }{
		{"e2e.sh", filepath.Join(root, ".ci/e2e.sh")},
		{"testdata/tool.sh", filepath.Join(root, "hack/lxc-e2e/guard.sh")},
		{"testdata/tool.sh", filepath.Join(root, "hack/lxc-e2e/install.sh")},
		{"testdata/tool.sh", filepath.Join(root, "hack/lxc-e2e/run.sh")},
	} {
		data, err := os.ReadFile(file.src)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(file.dest), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file.dest, data, 0700); err != nil {
			t.Fatal(err)
		}
	}
	data, err := os.ReadFile("testdata/tool.sh")
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range []string{"docker", "kind", "kubectl", "go", "ko"} {
		if err := os.WriteFile(filepath.Join(tools, tool), data, 0700); err != nil {
			t.Fatal(err)
		}
	}
	command := exec.CommandContext(t.Context(), "bash", filepath.Join(root, ".ci/e2e.sh"))
	command.Env = append(os.Environ(), "LXC_E2E_STAND="+stand, "PATH="+tools+":"+os.Getenv("PATH"), "COMMAND_LOG="+log)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("e2e dispatch: %v: %s", err, output)
	}
	data, err = os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	commands := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(commands) != 5 || !strings.HasPrefix(commands[0], "guard.sh ") || !strings.Contains(commands[1], "go build -o "+stand+"/bin/ate-setup ./cmd/ate-setup") || !strings.Contains(commands[2], "./cmd/kubectl-ate") || !strings.HasPrefix(commands[3], "install.sh ") || !strings.HasPrefix(commands[4], "run.sh ") {
		t.Fatalf("must guard, build and install current source before testing: %s", data)
	}
}

func TestNativeJobsUseLocalDocker(t *testing.T) {
	for _, mode := range []string{"test", "repeat"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			tools := t.TempDir()
			for _, file := range []struct{ src, dest string }{
				{"run.sh", filepath.Join(root, ".ci/run.sh")},
				{"testdata/native-checks.sh", filepath.Join(root, ".ci/checks.sh")},
				{"testdata/docker.sh", filepath.Join(tools, "docker")},
				{"testdata/tool.sh", filepath.Join(tools, "git")},
			} {
				data, err := os.ReadFile(file.src)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.MkdirAll(filepath.Dir(file.dest), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(file.dest, data, 0700); err != nil {
					t.Fatal(err)
				}
			}
			log := filepath.Join(root, "commands")
			command := exec.CommandContext(t.Context(), "bash", filepath.Join(root, ".ci/run.sh"), mode)
			command.Env = append(os.Environ(), "TEST_DOCKER_HOST=", "TEST_DOCKER_TLS_DIR=", "CI_PROJECT_URL=https://git.example/project", "COMMAND_LOG="+log, "DOCKER_RUN_STATUS=0", "PATH="+tools+":"+os.Getenv("PATH"), "CI_JOB_TOKEN=fixture-job-auth", "GITLAB_TOKEN=fixture-operator-auth")
			output, err := command.CombinedOutput()
			if err != nil {
				t.Fatalf("native %s: %v: %s", mode, err, output)
			}
			if !strings.Contains(string(output), "native-checks:"+mode) {
				t.Fatalf("checks did not run on the host: %s", output)
			}
			data, err := os.ReadFile(log)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(data), "docker.sock:") || strings.Contains(string(data), "--privileged") {
				t.Fatalf("SDK extraction exposes privileged access: %s", data)
			}
		})
	}
}

func TestCheckoutAuthenticationRemoved(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".ci"), 0700); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile("run.sh")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".ci/run.sh"), data, 0700); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init", root}, {"-C", root, "config", "remote.origin.url", "https://gitlab-ci-token:fixture-auth@git.example/project.git"}} {
		if output, err := exec.CommandContext(t.Context(), "git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git setup: %v: %s", err, output)
		}
	}
	tools := t.TempDir()
	data, err = os.ReadFile("testdata/docker.sh")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tools, "docker"), data, 0700); err != nil {
		t.Fatal(err)
	}
	command := exec.CommandContext(t.Context(), "bash", filepath.Join(root, ".ci/run.sh"), "verify")
	command.Env = append(os.Environ(), "CI_PROJECT_URL=https://git.example/project", "COMMAND_LOG="+filepath.Join(root, "commands"), "DOCKER_RUN_STATUS=0", "PATH="+tools+":"+os.Getenv("PATH"))
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("run: %v: %s", err, output)
	}
	data, err = os.ReadFile(filepath.Join(root, ".git/config"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "fixture-auth") {
		t.Fatal("checkout authentication remained readable to check container")
	}
}

func TestColdLintBudget(t *testing.T) {
	data, err := os.ReadFile("../.golangci.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var config struct {
		Run struct {
			Timeout string `yaml:"timeout"`
		} `yaml:"run"`
	}
	if err := yaml.Unmarshal(data, &config); err != nil {
		t.Fatal(err)
	}
	budget, err := time.ParseDuration(config.Run.Timeout)
	if err != nil {
		t.Fatal(err)
	}
	if budget < 15*time.Minute {
		t.Fatalf("cold lint budget = %s, want at least 15m for concurrent runner jobs", budget)
	}
}

func TestNativeJobsShareExclusiveRunnerResource(t *testing.T) {
	data, err := os.ReadFile("../.gitlab-ci.yml")
	if err != nil {
		t.Fatal(err)
	}
	var pipeline map[string]job
	var nodes map[string]yaml.Node
	if err := yaml.Unmarshal(data, &nodes); err != nil {
		t.Fatal(err)
	}
	pipeline = make(map[string]job)
	for _, name := range []string{"go-test", "go-repeat"} {
		var j job
		node := nodes[name]
		if err := node.Decode(&j); err != nil {
			t.Fatal(err)
		}
		pipeline[name] = j
	}
	group := pipeline["go-test"].ResourceGroup
	if group == "" || group != pipeline["go-repeat"].ResourceGroup {
		t.Fatal("native test jobs must share a resource_group to avoid fixed-port collisions")
	}
}
