# Security policy

This is a prototype. The sandbox design and its limits are described in README.md and docs/DEVLOG.md; please read them
before reporting.

## Reporting a vulnerability

Use **GitHub Security Advisories of this repository** ("Security" tab, "Report a vulnerability"). Do not open a public
issue for an undisclosed vulnerability. Please include the kernel, the core and its commit, how to reproduce, and what
the sandbox flags in `/v1/health` showed.

No other private contact channel is published yet.

## Scope

In scope: escaping the per-session sandbox from a core, reaching the parent or the Totv token/secret, path or zip handling
(`savezip.go`, `rom.go`), the control API. Out of scope: a malicious core binary you built yourself (cores are trusted
by design), and GPU driver bugs reachable by GPU cores as documented in "What a compromised core can still reach".
