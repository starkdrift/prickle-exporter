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

# Digest-pinned, not tag-pinned. This image compiles the binary that ships, so
# it is in the trusted path exactly as an Action is — and the workflows already
# pin Actions by SHA with a trailing version comment. A mutable tag here would
# have been the loosest pin in the repository sitting at its most sensitive
# point. Dependabot maintains it (.github/dependabot.yml, docker ecosystem).
#
# tag@digest rather than a bare digest: the digest is what Docker resolves, the
# tag is what a human reads, and Dependabot updates the pair. A trailing
# comment cannot carry the version the way it does in the workflows — Docker
# parses it as arguments to FROM and refuses the file.
FROM golang:1.26-alpine@sha256:0178a641fbb4858c5f1b48e34bdaabe0350a330a1b1149aabd498d0699ff5fb2 AS build
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

# cgo, dynamically linked, for the nvml target. Kept in its own stage so the
# default build never pays for it: `docker build .` produces the static image
# and never compiles this one.
# A glibc builder, not the alpine one above. cgo on alpine links against musl,
# and a musl binary does not run on the glibc distroless base the nvml target
# uses — it would build cleanly and fail at exec with a missing loader.
FROM golang:1.26-trixie@sha256:4ee9ffa999b4583ce281939cdff828763083610292f252279a0cee77473bd9a7 AS buildnvml
WORKDIR /src
COPY go.mod ./
COPY cmd/ cmd/
COPY internal/ internal/
ARG VERSION=docker
RUN CGO_ENABLED=1 go build -trimpath -tags nvml \
      -ldflags "-s -w -X main.version=${VERSION}" \
      -o /prickle-nvml ./cmd/prickle

# --- Default target: the static binary on scratch ---------------------------
FROM scratch AS static
COPY --from=build /prickle /prickle

# A fixed non-root UID rather than a name: scratch has no /etc/passwd for a
# name to resolve against. Everything the exporter reads is world-readable.
USER 65532:65532

EXPOSE 10047
ENTRYPOINT ["/prickle"]

# --- NVML target ------------------------------------------------------------
# Built with `--target nvml`. It cannot be scratch: NVML is reached by dlopen,
# which needs cgo and therefore a dynamically linked binary, which needs a libc
# to link against. distroless/base is that libc and nothing else — no shell, no
# package manager.
#
# The driver's own library is NOT baked in. libnvidia-ml.so.1 belongs to the
# host's driver and must match it, so it is mounted in at run time and found
# through LD_LIBRARY_PATH. Verified on an H100 with driver 580: mounting only
# that one file into this image, plus /dev/nvidiactl and /dev/nvidia0, gives
# `live source: nvml`. Mounting the host's whole library directory would work
# too and would also replace this image's libc with the host's — 855 files to
# solve a one-file problem.
FROM gcr.io/distroless/base-debian12:nonroot@sha256:63f52bd27b6aa6555f5d56500b70d7bb0afe51c654905be88a2c1cf967a77b1a AS nvml
COPY --from=buildnvml /prickle-nvml /prickle-nvml
ENV LD_LIBRARY_PATH=/nvidia
USER 65532:65532
EXPOSE 10047
ENTRYPOINT ["/prickle-nvml"]
