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
mode="${1:-}"
case "$mode" in test|verify|repeat) ;; *) echo 'Usage: run.sh test|verify|repeat' >&2; exit 2 ;; esac
if [[ $# != 1 ]]; then
    echo 'Usage: run.sh test|verify|repeat' >&2
    exit 2
fi
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$root"
: "${CI_PROJECT_URL:?CI_PROJECT_URL is required to remove checkout authentication}"
git remote set-url origin "$CI_PROJECT_URL.git"
if [[ "$(docker info --format '{{.OSType}}')" != linux ]]; then
    echo 'A Linux Docker daemon is required.' >&2
    exit 1
fi
work="$(mktemp -d)"
name="substrate-ci-$(basename "$work")"
image="substrate-ci:$name"
built=false
cleanup() {
    local status=$?
    if $built; then
        local container
        container="$(docker container ls -aq --filter "name=^/$name$")" || status=1
        if [[ -n "$container" ]]; then
            docker container rm --force "$container" >/dev/null || status=1
        fi
        docker image rm "$image" >/dev/null || status=1
    fi
    chmod -R u+w "$work" || status=1
    rm -rf "$work" || status=1
    exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
docker build --tag "$image" --file .ci/Dockerfile .ci
built=true
if [[ "$mode" != verify ]]; then
    if [[ "$(uname -s)" != Linux ]]; then
        echo 'Native CI test jobs require a Linux shell runner.' >&2
        exit 1
    fi
    docker_host="$(docker context inspect --format '{{.Endpoints.docker.Host}}')"
    if [[ "$docker_host" != unix:///* ]]; then
        echo 'Native CI test jobs require the trusted runner local Docker socket.' >&2
        exit 1
    fi
    mkdir "$work/sdk" "$work/home" "$work/tmp"
    docker run --rm --name "$name" --user "$(id -u):$(id -g)" \
        --cap-drop=ALL --security-opt=no-new-privileges --cpus=4 --memory=8g --pids-limit=1024 \
        --volume "$work/sdk:$work/sdk" "$image" cp -R /usr/local/go "$work/sdk/go"
    env -i PATH="$work/sdk/go/bin:$PATH" HOME="$work/home" TMPDIR="$work/tmp" \
        CI=true REQUIRE_DOCKER=true GOMAXPROCS=4 GOTOOLCHAIN=local CGO_ENABLED=1 GOFLAGS=-modcacherw \
        GOROOT="$work/sdk/go" GOCACHE="$root/__ci-cache/build" GOMODCACHE="$root/__ci-cache/mod" \
        GOPATH="$root/__ci-cache/go" KUBECONFIG=/nonexistent DOCKER_HOST="$docker_host" \
        TESTCONTAINERS_DOCKER_SOCKET_OVERRIDE="${docker_host#unix://}" \
        bash .ci/checks.sh "$mode"
    exit 0
fi
docker run --rm --name "$name" --user "$(id -u):$(id -g)" \
    --cap-drop=ALL --security-opt=no-new-privileges --cpus=4 --memory=8g --pids-limit=1024 \
    --env GOFLAGS=-modcacherw --env CI_DEFAULT_BRANCH --env CI_COMMIT_BRANCH \
    --env CI_COMMIT_BEFORE_SHA --env CI_MERGE_REQUEST_DIFF_BASE_SHA \
    --env HOME=/tmp --env CI=true --env GOMAXPROCS=4 --env GOTOOLCHAIN=local --env CGO_ENABLED=1 \
    --env GOCACHE=/src/__ci-cache/build --env GOMODCACHE=/src/__ci-cache/mod \
    --env GOPATH=/src/__ci-cache/go --env KUBECONFIG=/nonexistent \
    --volume "$root:/src" --workdir /src "$image" bash .ci/checks.sh "$mode"
