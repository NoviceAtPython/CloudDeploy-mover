# ADR-0001: Orchestrator language choice (Go)

| Field | Value |
| --- | --- |
| Status | Accepted |
| Date | 2026-05-16 |
| Decision driver | Iván Sánchez (project owner) — "professional, modular, idempotent, testable deployment, not one giant Bash script" |
| Authors | CloudDeploy v3 design |
| Supersedes | n/a (this is the first ADR) |
| Superseded by | n/a |

## Context

CloudDeploy v2 is a single 7500-line `CloudDeploy-wayland.sh` plus
about 200 lines of supporting Python (`scripts/write-edids.py`,
`scripts/validate-hdr-drm-state.py`). The Bash script is the
orchestrator: it makes apt-install decisions, installs the NVIDIA
driver, builds Sunshine from source, generates systemd units, runs
validators, and schedules reboots.

That worked end-to-end (Moonlight overlay reads `AV1 10-bit HDR` on
the live VM as of `7d850e9` on the `v2` branch) but it surfaced
problems we cannot solve with more Bash:

* No unit tests. Every change is validated by spinning a real GPU
  VM, ~10 minutes per iteration.
* No structured state. Phase markers are individual files under
  `/var/lib/clouddeploy/`. State queries require running the script.
* Conditional logic is hard to read. NVIDIA package family selection
  (`server` / `server-open` / `non-server` / `non-server-open`) plus
  CUDA mode (`none` / `optional` / `required`) plus retry suppression
  are ~400 lines of nested Bash `case` / `if [[`.
* dpkg/apt deadlocks (RTX 5090 deploy: `power-profiles-daemon.postinst`
  blocked on `deb-systemd-invoke start` behind boot jobs) need a real
  transaction wrapper. Bash can express it but the resulting code is
  fragile.

We need a language that:

1. Compiles to a single binary we can ship to a fresh Ubuntu VM.
2. Has first-class subprocess handling (we shell out to apt, dpkg,
   git, cmake, ninja, setcap, nvidia-smi, drm_info, journalctl).
3. Has structured types we can serialise to / from `state.json`.
4. Has a real test framework so the decision tree gets coverage.
5. Has YAML/JSON parsing in the standard library or a tiny
   well-known dep.
6. Is read-able by humans who already work on infrastructure
   tooling.
7. Has fast compile so iterating in development is not painful.

## Candidates considered

### Go

Pros:

* Single static binary. `GOOS=linux GOARCH=amd64 go build` produces
  a self-contained `clouddeployctl`. No runtime to install on the
  target VM beyond glibc.
* Standard library covers everything we need: `os/exec`,
  `encoding/json`, `text/template`, `flag`/`cobra`, `log/slog`,
  `context` for cancellation/timeouts.
* `gopkg.in/yaml.v3` is a thin, stable YAML dep.
* `testing` package + table-driven tests are idiomatic. `go test
  ./...` is the entire test surface.
* `os/exec.Cmd` with `Context` is exactly what we want for the
  subprocess wrapper. Stream stdout/stderr, capture exit code, kill
  on timeout, all in two lines.
* Cross-compilable from a developer's macOS laptop to the Ubuntu
  target. Cobra is the de-facto Go CLI framework (kubectl, hugo, gh).
* Existing infrastructure-tooling ecosystem (Terraform, Hashicorp,
  Kubernetes ecosystem) speaks Go. Hiring & code review pool is
  large.
* Fast compile: a 5000-line Go project builds in seconds.

Cons:

* More code than Bash for the trivial "run a shell command" case
  (about 6 lines of Go vs 1 line of Bash). Mitigated by the
  `internal/runner` wrapper.
* No `set -euo pipefail` analogue; we have to be deliberate about
  error propagation. Considered a feature, not a bug.

### Rust

Pros:

* Same single-binary advantages as Go, plus stronger type system
  and memory safety.
* Excellent for the low-level work we already want for v3.1+:
  EDID parsing (`edid-rs`), DRM property inspection (`drm-rs`),
  HDR metadata validation.
* Faster runtime than Go (irrelevant here — the bottleneck is apt /
  cmake, not the orchestrator).

Cons:

* Slower compile times hurt during iteration.
* Steeper learning curve for the next contributor. The next person
  who touches CloudDeploy is more likely to be comfortable with Go
  than with Rust lifetimes/borrowing.
* The orchestrator does not benefit from Rust's strengths. Most of
  what `clouddeployctl` does is "shell out, parse output, write
  config files." Go's standard library covers that with less code.
* Adding Cobra-equivalent (clap) is fine but yet another moving
  piece.

Conclusion on Rust: **Reserved for the leaf utilities in v3.1+.**
The orchestrator does not have a problem Rust uniquely solves.

### Python

Pros:

* Already in the project (`write-edids.py`,
  `validate-hdr-drm-state.py`).
* Ubiquitous on Ubuntu.
* Quick to write.
* Good for scripting glue.

Cons:

* No single-binary deploy. We have to either install
  `python3-pyyaml`, `python3-jinja2`, etc. on the target (apt
  dependency hell), or vendor with `pyinstaller` (defeats the
  purpose).
* No real static typing. mypy + pydantic helps but it is opt-in;
  the code review burden is higher.
* Test ergonomics are worse than Go's table-driven tests.
* Subprocess handling is verbose (`subprocess.run(...)` with quoting
  + capture is more code than `exec.Cmd`).
* Performance is fine for our scale but the standard library
  ergonomics (e.g. context cancellation) are weaker than Go's.

Conclusion on Python: **Keep the existing two small helpers; do not
make the orchestrator Python.**

### Bash (status quo)

Already discussed. Cons dominate.

### Nix / NixOS

Pros:

* Declarative system configuration. The package family / kernel
  module decision tree becomes a single Nix expression.
* Reproducibility.

Cons:

* Wholesale migration from Ubuntu to NixOS, which is out of scope.
* Existing patches (KWin NVIDIA private HDR, Sunshine fork) target
  Ubuntu-specific build flows. Re-doing them in nixpkgs is a much
  bigger project than CloudDeploy v3.
* Operator learning curve.

Conclusion on Nix: **Out of scope. Revisit if the project ever pivots
to a NixOS-based gaming VM.**

### TypeScript / Deno

Pros:

* Single binary via Deno's compile target.
* Good ecosystem for CLIs.

Cons:

* JS/TS is rarely the right pick for an infrastructure tool that
  shells out to system binaries. Less idiomatic than Go.
* Smaller ecosystem of infra tooling.

Conclusion on TS/Deno: **Rejected.**

## Decision

**Go** is the v3 orchestrator language.

* `cmd/clouddeployctl/` is the only `main` package.
* `internal/<subsystem>/` holds all logic.
* The CLI framework is Cobra (`github.com/spf13/cobra`).
* YAML is `gopkg.in/yaml.v3`.
* Logging is `log/slog` from the standard library.
* Subprocess invocation goes through `internal/runner` — never
  `os/exec` directly outside that package.

The existing two Python helpers (`scripts/validate-hdr-drm-state.py`,
the planned `helpers/write-edids.py`) stay in Python until a Rust
replacement makes sense. They are invoked as subprocesses from Go.

## Consequences

* Operators need Go on the build host. `bootstrap.sh` installs it
  via `apt install golang-go` on Ubuntu 24.04+, or downloads the
  official Linux tarball on older releases.
* Target VMs **do not** need Go installed. `clouddeployctl` is a
  static binary that ships with the deploy artifact.
* All decision logic that v2 expressed in Bash conditionals is
  re-expressed in Go functions. Those functions are unit-testable
  without spinning a real VM.
* The first v3 commit lands the project skeleton plus the three
  highest-value tested modules (`internal/nvidia`,
  `internal/cuda`, `internal/apt`). The remaining modules are
  stubs until subsequent milestones.

## Open questions

* Should `clouddeployctl` be versioned independently from the
  deploy profile schema? Probably yes — bump the binary independently
  of the config YAML schema, with explicit migration on
  state-file version mismatch. Tracked separately.
