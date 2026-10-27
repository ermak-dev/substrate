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
    echo 'Usage: guard.sh STAND_DIRECTORY' >&2
    exit 1
fi
config="$1/cluster/kubeconfig"
cluster=lxc-e2e-7
expected="$(mktemp)"
trap 'rm -f "$expected"' EXIT
kind get kubeconfig --name "$cluster" > "$expected"
server_path='{.clusters[0].cluster.server}'
ca_path='{.clusters[0].cluster.certificate-authority-data}'
actual_server="$(kubectl --kubeconfig "$config" --context "kind-$cluster" config view --raw --minify -o "jsonpath=$server_path")"
expected_server="$(kubectl --kubeconfig "$expected" --context "kind-$cluster" config view --raw --minify -o "jsonpath=$server_path")"
actual_ca="$(kubectl --kubeconfig "$config" --context "kind-$cluster" config view --raw --minify -o "jsonpath=$ca_path")"
expected_ca="$(kubectl --kubeconfig "$expected" --context "kind-$cluster" config view --raw --minify -o "jsonpath=$ca_path")"
skip_tls="$(kubectl --kubeconfig "$config" --context "kind-$cluster" config view --raw --minify -o 'jsonpath={.clusters[0].cluster.insecure-skip-tls-verify}')"
if [[ -z "$expected_ca" || "$actual_server" != "$expected_server" || "$actual_ca" != "$expected_ca" || "$skip_tls" == true ]]; then
    echo 'isolated kind target mismatch: refusing Kubernetes mutations' >&2
    exit 1
fi
