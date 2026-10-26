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
    echo 'Usage: test-guard.sh VALID_STAND_DIRECTORY' >&2
    exit 1
fi
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
stand="$(mktemp -d)"
trap 'rm -rf "${stand:?}"' EXIT
mkdir "$stand/cluster" "$stand/bin"
ln -s "$root/hack/lxc-e2e/testdata/deny-install.sh" "$stand/bin/ate-setup"
for mode in server ca skip-tls; do
    cp "$1/cluster/kubeconfig" "$stand/cluster/kubeconfig"
    case "$mode" in
        server) kubectl --kubeconfig "$stand/cluster/kubeconfig" config set-cluster kind-lxc-e2e-7 --server=https://127.0.0.1:1 >/dev/null ;;
        ca) kubectl --kubeconfig "$stand/cluster/kubeconfig" config unset clusters.kind-lxc-e2e-7.certificate-authority-data >/dev/null ;;
        skip-tls) kubectl --kubeconfig "$stand/cluster/kubeconfig" config set-cluster kind-lxc-e2e-7 --insecure-skip-tls-verify=true >/dev/null ;;
    esac
    set +e
    PATH="$stand/bin:$PATH" bash "$root/hack/lxc-e2e/install.sh" "$stand" >"$stand/result" 2>&1
    result=$?
    set -e
    cat "$stand/result"
    if [[ "$result" == 0 || "$result" == 77 ]]; then
        echo "Wrong kubeconfig $mode was not rejected before mutation" >&2
        exit 1
    fi
    grep -q 'isolated kind target mismatch' "$stand/result"
done
