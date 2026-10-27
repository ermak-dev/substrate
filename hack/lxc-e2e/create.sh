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
    echo 'Usage: create.sh EMPTY_STAND_DIRECTORY' >&2
    exit 1
fi
stand="$1"
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cluster=lxc-e2e-7
registry=lxc-e2e-7-registry
if docker inspect "$cluster-control-plane" >/dev/null 2>&1 || docker inspect "$registry" >/dev/null 2>&1; then
    echo 'Refusing to reuse existing LXC test containers' >&2
    exit 1
fi
if [[ -e "$stand/kubeconfig" ]]; then
    echo 'Refusing to overwrite an existing kubeconfig' >&2
    exit 1
fi
mkdir -p "$stand"
docker network create --label created-by=substrate-lxc-e2e "$cluster"
docker run -d --name "$registry" --network "$cluster" --network-alias kind-registry \
    --label created-by=substrate-lxc-e2e -p 127.0.0.1:5017:5000 \
    registry:3@sha256:01cd312e67026f06f5d89c3e133a4a9adc7cb9eeb9b1194e0b2954dfd770efd2
KIND_EXPERIMENTAL_DOCKER_NETWORK="$cluster" kind create cluster \
    --name "$cluster" --kubeconfig "$stand/kubeconfig" --config "$root/hack/lxc-e2e/kind.yaml" \
    --image kindest/node:v1.37.0@sha256:a1ed56cfb0e7b93589bdf97c8cd566405a265939e3620fc4f5de89adff580ae5
# Limit only the new test node, leaving other containers and the host unchanged.
docker update --cpus=8 --memory=16g --memory-swap=16g "$cluster-control-plane"
registry_dir=/etc/containerd/certs.d/localhost:5017
docker exec "$cluster-control-plane" mkdir -p "$registry_dir"
printf '[host."http://%s:5000"]\n' "$registry" | docker exec -i "$cluster-control-plane" cp /dev/stdin "$registry_dir/hosts.toml"
kubectl --kubeconfig "$stand/kubeconfig" --context "kind-$cluster" get nodes
