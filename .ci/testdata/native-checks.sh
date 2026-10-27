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
[[ -d "$GOROOT/bin" ]]
[[ "$CI" == true ]]
[[ "$DOCKER_HOST" == unix:///var/run/docker.sock ]]
[[ "$GOFLAGS" == -modcacherw ]]
[[ -z "${GITLAB_TOKEN:-}" && -z "${CI_JOB_TOKEN:-}" ]]
printf 'native-checks:%s\n' "$1"
mkdir "$HOME/read-only"
touch "$HOME/read-only/cached-binary"
chmod 500 "$HOME/read-only"
