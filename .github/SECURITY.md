# Security policy

## Reporting a vulnerability

Please report privately, through GitHub's **[Report a
vulnerability](https://github.com/starkdrift/prickle-exporter/security/advisories/new)**
button on the repository's Security tab. That opens a draft advisory visible
only to you and the maintainer.

Please do not open a public issue for a suspected vulnerability. Everything else
about this project is reported in the open — this is the one exception, and only
until a fix exists.

There is deliberately no email address here. A contact address in a public
repository cannot be un-published once it is in the git history, and the private
advisory route is both harder to lose and better structured: it keeps the report
attached to the repository, and it is the same mechanism used to publish the
advisory and request a CVE afterwards.

**Expect an acknowledgement within 5 business days.** This is a single-maintainer
project, so that is a realistic first response rather than a triage SLA — it
means the report has been read and you will hear what happens next, not that a
fix is ready. If 5 days pass with no reply at all, please assume the
notification went astray and file a public issue saying only that you are
waiting on a security response, with no details of the finding.

## Supported versions

**The latest release, and only the latest release.** Fixes go out in a new
version rather than as backports to an older line.

`prickle-exporter` is pre-1.0 by intent — see the Status section of the
[README](../README.md) — and there are no long-term support branches to speak
of. If you are running something older, the upgrade is the fix.

## What is likely to matter here

The exporter is read-only, listens on one port, serves one path, and makes no
outbound connection except an optional Docker socket request. So the surface
worth attention is narrower than the code size suggests:

- **Anything that reads a file the exporter should not read**, or that escapes
  the [internal/fsroot](../internal/fsroot/) prefixes.
- **Anything that writes**, anywhere. Strictly read-only access to `/proc`,
  `/sys` and cgroups is a stated guarantee, and a violation of it is a
  vulnerability even without an exploit path.
- **Leaking a privilege upward** — most of all around
  `-collector.container.pod-names`, which is run with extra file-read reach:
  group root on Kubernetes, or ambient `CAP_DAC_READ_SEARCH` under a systemd
  drop-in, which can read any file on the host. A bug that widens what reaches
  the metrics endpoint while either is granted is serious.
- **A PID, or anything else the contract forbids, appearing in output.**
  [SPEC.md](../SPEC.md) §Metrics contract treats that as a correctness
  guarantee, not a preference.
- **Parsing input from an untrusted source** — `/proc` and cgroup content is
  attacker-influenced on a multi-tenant node, and a crash or a hang in a parser
  is worth reporting.

Denial of service by scraping the endpoint very fast is *not* interesting: the
architecture serves the last completed render, so a scrape does no collection
work. Neither is anything that requires root on the host already.

Reports about the demonstration compose stack in
[packaging/compose](../packaging/compose/) are also out of scope — it runs
Grafana with anonymous admin on purpose, which the README says plainly, and it
is not something to deploy.

See [CONTRIBUTING.md](CONTRIBUTING.md) for how everything that is not a
vulnerability gets reported.
