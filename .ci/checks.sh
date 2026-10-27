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
case "$mode" in test|verify|repeat) ;; *) echo 'Usage: checks.sh test|verify|repeat' >&2; exit 2 ;; esac
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$root"
case "$mode" in
    test)
        go test -race -p 2 ./...
        go test -race ./.ci
        for module in tools/apitool tools/junittool internal/plugins/gcp-secret-manager; do
            go -C "$module" test -race ./...
        done
        GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build ./...
        ;;
    verify)
        if [[ -n "${CI_MERGE_REQUEST_DIFF_BASE_SHA:-}" ]]; then
            base="$CI_MERGE_REQUEST_DIFF_BASE_SHA"
        elif [[ "${CI_COMMIT_BRANCH:-}" == "${CI_DEFAULT_BRANCH:?CI_DEFAULT_BRANCH is required}" ]]; then
            base="${CI_COMMIT_BEFORE_SHA:?CI_COMMIT_BEFORE_SHA is required on the default branch}"
            if [[ "$base" =~ ^0+$ ]]; then
                echo 'A nonzero base commit is required to verify merged migrations.' >&2
                exit 1
            fi
        else
            base="origin/${CI_DEFAULT_BRANCH:?CI_DEFAULT_BRANCH is required}"
        fi
        git cat-file -e "$base^{commit}"
        ./hack/verify-all.sh
        ./hack/verify/postgresql-migrations.sh "$base"
        (
            cd internal/plugins/gcp-secret-manager
            make verify
            ../../../hack/run-tool.sh golangci-lint run --config ../../../.golangci.yaml ./...
        )
        ;;
    repeat)
        go test -race -p 2 -count=3 -shuffle=on ./...
        go test -race -count=3 -shuffle=on ./.ci
        go test -race -count=3 -shuffle=on ./internal/e2e/suites/lxc/health.go ./internal/e2e/suites/lxc/health_test.go
        ;;
esac
