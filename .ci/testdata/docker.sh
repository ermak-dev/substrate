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
printf '%s\n' "$@" >> "$COMMAND_LOG"
case "$1" in
    info) echo linux ;;
    context) echo unix:///var/run/docker.sock ;;
    run)
        if [[ " $* " == *" cp -R /usr/local/go "* ]]; then
            mkdir -p "${!#}/bin"
        fi
        exit "$DOCKER_RUN_STATUS"
        ;;
esac
