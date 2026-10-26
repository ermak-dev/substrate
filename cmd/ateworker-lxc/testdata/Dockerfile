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

ARG WORKER_IMAGE
FROM golang:1.27.3-bookworm@sha256:972b4110ba0f47539a975c31e52c64765e1ff19f2d74469ae4a595c6676e80a5 AS tests
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY api ./api
COPY internal ./internal
COPY pkg ./pkg
COPY cmd ./cmd
RUN CGO_ENABLED=1 go test -race -c -tags=lxc_integration -o /lxc.test ./cmd/ateworker-lxc
FROM debian:13-slim@sha256:62bd423aeee912639ca8ee681c2b762cb3e25137e0a92b20be455898a9f034ad AS guest
RUN apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
    systemd systemd-sysv dbus && rm -rf /var/lib/apt/lists/*
FROM ${WORKER_IMAGE}
COPY --from=guest / /opt/lxc-test-guest/
COPY --from=tests /lxc.test /usr/local/bin/lxc.test
ENTRYPOINT ["/usr/local/bin/lxc.test"]
