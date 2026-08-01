# SPDX-License-Identifier: Apache-2.0
#
# Two stages, and the final one is `scratch`. That is not minimalism for its
# own sake: SPEC.md §Hard constraints #1 says the standard library is the only
# dependency, and a base image with a package manager in it would put a
# hundred packages behind a binary that has none. There is nothing to patch in
# this image because there is nothing in it.
#
# Build:
#   docker build -t prickle-exporter .
#
# Run — the exporter reads the host, so the host has to be visible, read-only:
#   docker run -d --name prickle --pid=host --network=host \
#     -v /proc:/host/proc:ro -v /sys:/host/sys:ro \
#     prickle-exporter -path.rootfs=/host
#
# --network=host is what makes :10047 reachable as the node's own port, which
# is what a Prometheus node discovery expects. --pid=host is needed only for
# -collector.gpu.per-process; drop it otherwise.

FROM golang:1.26-alpine AS build
WORKDIR /src

# No go.sum to copy and no modules to download: the dependency count is zero,
# so there is no dependency layer to cache either.
COPY go.mod ./
COPY cmd/ cmd/
COPY internal/ internal/

ARG VERSION=docker
# CGO_ENABLED=0 for the same reason the release build uses it: this is the
# static artifact, and it takes the nvidia-smi path. The NVML variant cannot be
# built this way — it needs cgo and dynamic linking — so it is not offered here.
RUN CGO_ENABLED=0 go build -trimpath \
      -ldflags "-s -w -X main.version=${VERSION}" \
      -o /prickle ./cmd/prickle

FROM scratch
COPY --from=build /prickle /prickle

# A fixed non-root UID rather than a name: scratch has no /etc/passwd for a
# name to resolve against. Everything the exporter reads is world-readable.
USER 65532:65532

EXPOSE 10047
ENTRYPOINT ["/prickle"]
