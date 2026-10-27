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
    echo 'Usage: install.sh STAND_DIRECTORY' >&2
    exit 1
fi
stand="$1"
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
export KUBECONFIG="$stand/cluster/kubeconfig"
export KO_DOCKER_REPO=localhost:5017
KO_DEFAULTPLATFORMS="linux/$(go env GOARCH)"
export KO_DEFAULTPLATFORMS
export GOMAXPROCS=4
cluster=lxc-e2e-7
if [[ ! -f "$KUBECONFIG" ]]; then
    echo 'Create the isolated LXC stand first' >&2
    exit 1
fi
if [[ "$(docker inspect -f '{{index .Config.Labels "io.x-k8s.kind.cluster"}}' "$cluster-control-plane")" != "$cluster" ]]; then
    echo 'Refusing to install outside the isolated LXC cluster' >&2
    exit 1
fi
bash "$root/hack/lxc-e2e/guard.sh" "$stand"
cd "$root"
ate-setup --no-dev-env --kind --kind-cluster-name "$cluster" \
    --kubeconfig "$KUBECONFIG" --context "kind-$cluster" \
    --ko-docker-repo "$KO_DOCKER_REPO" --preview ALL --rollout-timeout 5m \
    --credential-provider '{"name":"k8s.io"}' \
    --ingress-allowed-spiffe-ids spiffe://cluster.local/ns/ate-e2e-router-client/sa/router-client \
    deploy ate-system --setup-csi=hostpath
cmd/ateworker-lxc/build.sh localhost:5017/ateworker-lxc:e2e
docker push localhost:5017/ateworker-lxc:e2e
kubectl --kubeconfig "$KUBECONFIG" --context "kind-$cluster" apply -f cmd/ateworker-lxc/sandboxconfig.yaml
