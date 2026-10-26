#!/usr/bin/env bash

# Copyright 2026 Google LLC
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#      http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

set -euo pipefail
if [[ $# != 1 ]]; then
    echo 'Usage: run.sh STAND_DIRECTORY' >&2
    exit 1
fi
stand="$1"
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
export KUBECONFIG="$stand/cluster/kubeconfig"
export KO_DOCKER_REPO=localhost:5017
KO_DEFAULTPLATFORMS="linux/$(go env GOARCH)"
export KO_DEFAULTPLATFORMS
export E2E_SANDBOX_CLASS=lxc
bash "$root/hack/lxc-e2e/guard.sh" "$stand"
cd "$root"
digest="$(go run github.com/google/go-containerregistry/cmd/crane@latest digest localhost:5017/ateworker-lxc:e2e)"
export E2E_LXC_WORKER_IMAGE="localhost:5017/ateworker-lxc@$digest"
GOMAXPROCS=4 go test -race ./internal/e2e/suites/lxc -v -count=3 -shuffle=on -timeout=30m \
    -args --e2e --kube-config "$KUBECONFIG" --kube-context kind-lxc-e2e-7 \
    --preview ALL --storage-class csi-hostpath-sc
