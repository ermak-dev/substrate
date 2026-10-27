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
: "${LXC_E2E_STAND:?LXC_E2E_STAND must name an installed isolated LXC stand}"
if [[ "$LXC_E2E_STAND" != /* ]]; then
    echo 'LXC_E2E_STAND must be an absolute path.' >&2
    exit 1
fi
export PATH="$LXC_E2E_STAND/bin:$PATH"
for tool in docker kind kubectl go ko; do
    command -v "$tool" >/dev/null || { echo "Missing infrastructure e2e prerequisite: $tool" >&2; exit 1; }
done
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
bash "$root/hack/lxc-e2e/guard.sh" "$LXC_E2E_STAND"
cd "$root"
mkdir -p "$LXC_E2E_STAND/bin"
go build -o "$LXC_E2E_STAND/bin/ate-setup" ./cmd/ate-setup
go build -o "$LXC_E2E_STAND/bin/kubectl-ate" ./cmd/kubectl-ate
bash "$root/hack/lxc-e2e/install.sh" "$LXC_E2E_STAND"
exec bash "$root/hack/lxc-e2e/run.sh" "$LXC_E2E_STAND"
