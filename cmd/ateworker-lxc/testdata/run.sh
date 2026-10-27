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
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
context="$(mktemp -d)"
trap 'rm -rf "${context:?}"' EXIT
cd "$root"
image="ateworker-lxc-integration:local"
cmd/ateworker-lxc/build.sh ateworker-lxc-integration-worker:local
# Copy only source files, excluding ignored credentials and local build artifacts.
while IFS= read -r -d '' file; do
    if [[ -f "$file" || -L "$file" ]]; then
        mkdir -p "$context/$(dirname "$file")"
        cp "$file" "$context/$file"
    fi
done < <(git ls-files --cached --others --exclude-standard -z -- go.mod go.sum api internal pkg cmd)
cp cmd/ateworker-lxc/testdata/Dockerfile "$context/Dockerfile"
docker build --platform "linux/$(go env GOARCH)" --build-arg WORKER_IMAGE=ateworker-lxc-integration-worker:local -t "$image" "$context"
docker run --rm --cgroupns=private --cpus=2 --memory=2g \
    --tmpfs /var/lib/ate:rw,exec,size=1g \
    --cap-drop=ALL \
    --cap-add=NET_ADMIN --cap-add=SYS_ADMIN --cap-add=SYS_CHROOT --cap-add=SYS_PTRACE \
    --cap-add=SETUID --cap-add=SETGID --cap-add=SETPCAP --cap-add=DAC_OVERRIDE \
    --cap-add=FOWNER --cap-add=CHOWN --cap-add=MKNOD --cap-add=NET_RAW --cap-add=SETFCAP \
    --cap-add=NET_BIND_SERVICE --cap-add=SYS_RESOURCE \
    --security-opt apparmor=unconfined --security-opt seccomp=unconfined \
    --env ATE_LXC_INTEGRATION_CONTAINER=1 \
    "$image" -test.run '^TestLXCIntegration$' -test.v -test.count=3 -test.shuffle=on -test.timeout=5m
