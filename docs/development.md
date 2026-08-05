# Development

Building, testing, and the one command that is the whole pre-commit gate.

[← README](../README.md)

One command is the whole pre-commit checklist, and CI runs the same script:

```sh
./ci/check.sh
```

It runs `gofmt -l`, `go vet`, `go test -race ./...`, compiles and vets the
`-tags nvml` build (which no other step sees), then gates on: an empty
`go.sum` and no `require` block, an SPDX header in every `.go` file, `promtool
check metrics` on every golden file, and greps for denied names and abbreviated
metric prefixes.

The test run is under the race detector because the sampler swaps a fully
rendered buffer under a mutex while `net/http` serves the previous one from
another goroutine — that non-blocking swap is the architecture's load-bearing
claim, and nothing else in the checklist can falsify it. The detector needs a C
toolchain, which is the one thing `ci/check.sh` wants that the release build
(`CGO_ENABLED=0`, static) deliberately does not: the gate tests the source, the
release builds the artifact.

`promtool` is required, not optional. Get the same pinned version CI uses:

```sh
./ci/install-promtool.sh          # checksum-verified, into ./bin
export PATH="$PWD/bin:$PATH"
```

> **Known gap:** `ci/denied-names.txt` is currently empty, so that gate reports
> `VACUOUS` and protects nothing until the names discarded during naming are
> filled in.

`ci/check.sh` is hermetic and stays that way. The one check that needs the
network lives on its own:

```sh
./ci/check-port-registration.sh
```

It confirms port 10047 is still registered to this project on the [Prometheus
default-port wiki](https://github.com/prometheus/prometheus/wiki/Default-port-allocations)
— a page anyone can edit, in someone else's repository, that sends no
notification if our row is reassigned or dropped. The port is read out of
SPEC.md rather than hardcoded, so the check can't silently drift from the spec.
It exits `0` registered, `2` missing or reassigned, and `1` for *couldn't tell*
(no network, page moved, payload that isn't the allocation table) — so a flaky
fetch never reads as a lost registration. Run it on a schedule, not before a
commit.

### Fixtures

Every parser is developed against a captured fixture tree under `testdata/`,
laid out mirroring real paths so [internal/fsroot](../internal/fsroot/) points
straight at it. Exposition output is checked against golden files.

**File formats and path shapes are never invented.** If no fixture exists for a
case, stop and capture one with
[scripts/capture-fixtures.sh](../scripts/capture-fixtures.sh) — it dumps the exact
`/proc`, `/sys`, cgroup, Docker-API and GPU vendor-tool output a real host
exposes. Capture hosts are usually rented by the hour, so the script has `check`
and `prep` subcommands to make sure the interesting state is live *before* you
capture and destroy the box. Read [scripts/README.md](../scripts/README.md) before
renting anything.

Synthetic fixtures are allowed only where hardware access is pending, and must
be marked synthetic in a README beside them.

NVML is the one path that cannot be fixture-tested — it's a C library call, not
a file read. Unit tests exercise a fake `nvidiaSource`; its captured
`nvidia-smi` fixtures remain the reference for what the NVML path must report.

### Layout

```
cmd/prickle/          main, flags, diagnose
internal/collector/   the Collector interface
  host/               Phase 1: /proc parsers + fixtures + golden file
internal/exposition/  hand-written Prometheus text format
internal/fsroot/      the /proc, /sys, cgroup prefixes every path goes through
internal/sampler/     poll loop, buffer swap, self-metrics, http.Handler
ci/check.sh           the pre-commit gate
scripts/              dev-run.sh, capture-fixtures.sh, capture-dashboard.sh
assets/               logo and mark, light and dark; dashboard captures
```


## Building from source

Requires Go 1.26. There is nothing to fetch — the module has no dependencies.

```sh
git clone https://github.com/starkdrift/prickle-exporter
cd prickle-exporter
CGO_ENABLED=0 go build -o prickle ./cmd/prickle
./prickle
```

Then scrape it:

```sh
curl -s localhost:10047/metrics | head
```

Port **10047** is fixed by [SPEC.md §Identity](../SPEC.md#identity).
`-web.listen-address` exists for when something else on your workstation
already holds it — don't change it in anything that ships.

Stamp a version into the binary with:

```sh
go build -ldflags "-X main.version=$(git describe --tags --always)" -o prickle ./cmd/prickle
```

There is a second build, `-tags nvml`, which is cgo and dynamically linked so
it can `dlopen` the driver's NVML library. [SPEC.md §Distribution](../SPEC.md#distribution)
has the reasoning; `ci/check.sh` compiles and vets it, so a change that breaks
it fails the gate rather than the release.


## Running from source

[scripts/dev-run.sh](../scripts/dev-run.sh) wraps the common loops with
dev-friendly defaults — debug logging, a 2s sample interval, no root needed:

```sh
./scripts/dev-run.sh              # serve on :10047 until Ctrl-C
./scripts/dev-run.sh fixture      # same, but read a captured fixture tree
./scripts/dev-run.sh diagnose     # what this host can and cannot be read from
./scripts/dev-run.sh scrape       # start, scrape once, print, promtool, stop
```

See [scripts/README.md](../scripts/README.md) for the details, including why
`fixture` mode still reports *your* filesystems.


## Releases and versioning

Everything is under `internal/`, so there is no importable Go API. SemVer here
applies to the two surfaces that actually exist: the **metrics contract** and
the **command line**. [SPEC.md §Versioning](../SPEC.md#versioning) is the full
policy; the short version:

- **major** — a metric renamed or removed, a label key added to or removed from
  an existing series, a flag removed. (Adding a label key is a major, not a
  minor: it breaks every rule that aggregates without `by`.)
- **minor** — a new metric family, collector, flag, or label value.
- **patch** — a wrong value corrected, a parser fix, docs.

Pre-1.0 the minor tracks the roadmap phase, so the version says what is
implemented: `0.1.0` host, `0.2.0` containers, `0.3.0` GPU. **`1.0.0` means the
metrics contract is frozen.** Changes are recorded by hand in
[CHANGELOG.md](../CHANGELOG.md).

Git tags are the only source of truth for the version — there is no VERSION
file. Releases carry SLSA build provenance:

```sh
sha256sum -c SHA256SUMS
gh attestation verify prickle_v0.1.0_linux_amd64.tar.gz --repo starkdrift/prickle-exporter
```


## Contributing

Read [SPEC.md](../SPEC.md) first, in full — it is short, and it settles most of the
questions a patch would otherwise raise. [CLAUDE.md](../CLAUDE.md) is the fast index
plus the traps that are easy to trip on. Then make `./ci/check.sh` pass.

