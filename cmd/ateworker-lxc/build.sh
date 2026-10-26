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
    echo 'Usage: build.sh LOCAL_IMAGE_NAME' >&2
    exit 1
fi
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
context="$(mktemp -d)"
trap 'rm -rf "${context:?}"' EXIT
cd "$root"
arch="$(go env GOARCH)"
CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -o "$context/ateworker-lxc" ./cmd/ateworker-lxc
cp cmd/ateworker-lxc/Dockerfile "$context/Dockerfile"
docker build --platform "linux/$arch" -t "$1" "$context"
