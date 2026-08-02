# Contributing

**This repository does not accept pull requests.** One opened from a fork is
closed automatically by [a workflow](workflows/close-prs.yml), without being
read. That is a blanket policy rather than a verdict on any particular change —
if yours was closed, it was not rejected on its merits, because nobody looked at
its merits.

Everything else about the project is open, and the parts that are open are meant
seriously.

## What is welcome

- **Bug reports.** A metric with the wrong value, a parser that misreads a
  layout, a host where the exporter reports nothing — these are worth an issue,
  and they get read. Say which distribution, which cgroup hierarchy and which
  container runtime; `prickle diagnose` prints most of it for you.
- **Feature discussion.** Ask for a metric, a collector, or a flag by opening an
  issue that describes the question you cannot answer today. A proposal argued
  in an issue can change [SPEC.md](../SPEC.md), which is the only way anything
  gets built here. A patch cannot.
- **Security reports.** Privately, through the Security tab rather than a public
  issue — [SECURITY.md](SECURITY.md) has the route and what is in scope.

Auditing is welcome too, and is much of why the repository is public: the
zero-dependency and read-only claims in the README are the sort of thing that
should be checkable by someone who does not trust the person making them.

## Why

`prickle-exporter` is written by one maintainer against a frozen contract —
[SPEC.md](../SPEC.md) is decided first and the code is made to match, so a patch
that arrives contract-first has nothing to attach to. Its two load-bearing
guarantees are properties of the whole tree rather than of any one diff: zero
third-party dependencies, and strictly read-only access to `/proc`, `/sys` and
cgroups. [ci/check.sh](../ci/check.sh) checks them over the tree as a whole, and
they are much easier to keep true when every line has one origin. Single-author
copyright provenance keeps the licensing story equally short.

None of that is a statement about the quality of outside work. It is a statement
about how much of this project's value comes from its constraints holding
without exception, and how cheaply a single maintainer can promise that.
