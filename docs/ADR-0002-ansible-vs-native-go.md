# ADR-0002: Ansible vs native Go orchestration

| Field | Value |
| --- | --- |
| Status | Accepted |
| Date | 2026-05-16 |
| Authors | CloudDeploy v3 design |
| Related | [ADR-0001](ADR-0001-orchestrator-language.md) |

## Context

The v3 architecture brief explicitly invited consideration of Ansible
in three shapes:

* **A.** Go orchestrator only.
* **B.** Go orchestrator + Ansible roles for declarative bits
  (package install, file rendering, systemd unit management).
* **C.** Ansible primary + Go helpers for the conditional logic.
* **D.** Other hybrid.

CloudDeploy v2 is single-host Bash. The v3 brief was clear about not
wanting either "one giant Bash script" or "one giant unreadable YAML
maze."

## What Ansible would actually buy us

Honest accounting of Ansible's strengths against v3's needs:

| Strength | Relevance to CloudDeploy v3 |
| --- | --- |
| Idempotent built-in modules for `apt`, `file`, `copy`, `template`, `systemd`, `lineinfile` | Real. About 40% of v2's lines are doing the equivalent. |
| Inventory model for multi-host fleets | **Zero** — CloudDeploy operates on the VM where it runs. |
| Push model with parallel SSH | **Zero** — same reason. |
| Galaxy ecosystem of reusable roles | Marginal. There are NVIDIA Ansible roles but they target server CUDA installs, not the KWin-Wayland-headless-AV1-HDR path. |
| `--check` / `--diff` for dry-run | Helpful, but we have to write the same thing in Go (`clouddeployctl doctor`). |
| Declarative YAML | Useful when the work is mostly "install package X, render template Y." Hostile when the work is "look at lspci, dmesg, dpkg, nvidia-smi, decide which of four package families to install." |

## What Ansible would cost us

The hard parts of v3 are exactly the things Ansible is bad at:

### 1. The NVIDIA package family selector

The decision input:

* `lspci -nn` parse → GPU model + PCI ID.
* `dmesg | grep -i nvrm` → driver evidence ("requires use of NVIDIA
  open kernel modules" lines).
* `apt-cache policy nvidia-driver-${MAJOR}-server-open` → is the
  open package available on the configured apt sources?
* `dpkg -l | grep nvidia-driver-` → what is currently installed?
* `dkms status` → which modules are built for the running kernel?
* `nvidia-smi` exit code + version line.
* `/proc/driver/nvidia/version` (if present) → exact loaded module.

The output:

* One of `server`, `server-open`, `non-server`, `non-server-open`
  with a reason string.
* Decision to leave the existing install in place OR
  uninstall+reinstall+rebuild-DKMS.

Implementing this in Ansible playbooks means:

* A `command` task that runs `lspci -nn`, registers the output.
* Six more `command` tasks for `dmesg | grep`, `apt-cache policy`,
  `dpkg -l`, `dkms status`, `nvidia-smi`, `cat /proc/driver/...`.
* A `when:` cascade that evaluates the decision tree in Jinja2.
* A `set_fact:` to record the chosen family.

The Jinja2 decision-tree expression is genuinely awful to read and
genuinely impossible to unit-test. The same logic in Go:

```go
func SelectFamily(ctx context.Context, ev Evidence) (Family, string, error) {
    if ev.DmesgRequiresOpenKernelModule {
        return FamilyServerOpen, "dmesg: NVIDIA driver requires open kernel modules", nil
    }
    if ev.IsBlackwellConsumer {
        return FamilyServerOpen, "Blackwell consumer GPU; closed kernel module unsupported", nil
    }
    // ... etc.
}
```

is shorter, type-checked, and unit-testable with table-driven tests
that take ~3ms each. This is the single biggest reason we are not
using Ansible.

### 2. CUDA retry suppression

The v2 deploy hit the failure mode where the CUDA runfile was
downloaded **five times**, every download produced an identical 4.3 GB
artifact with identical SHA-256, and every `--check` run failed with
an internal MD5 mismatch. v2's retry loop made the deploy take an
extra ~25 minutes per attempt.

The right behaviour is "if the download SHA matches a previous attempt
that already failed `--check`, do not redownload — fail-fast or
fall back to apt." Encoding that in Ansible means:

* A `get_url` with `checksum:` (Ansible handles redownload-on-mismatch
  but does not handle the post-download `--check` failure case).
* A `command` task running the runfile `--check`.
* A `block` / `rescue` / `always` to handle failure, plus
  `failed_when:` and `changed_when:` overrides.
* A `set_fact:` to track previous attempts.
* A `meta: end_play` if we are giving up.

In Go this is a 30-line function with a small state struct that
remembers `(sha256, size, check_result)` across attempts and returns
"do not redownload" if the same artifact already failed `--check`.

### 3. dpkg deadlock survival

`power-profiles-daemon.postinst` runs `deb-systemd-invoke start
power-profiles-daemon.service` during install. On a freshly booted
VM with `systemd` still finishing boot jobs, that call hangs because
the service-start operation queues behind boot jobs, and the
postinst's parent dpkg holds the dpkg lock the boot jobs need.

The fix is to install a temporary `/usr/sbin/policy-rc.d` that
returns exit code 101 (= "do not start any services") for the
duration of the apt transaction, preserving any pre-existing
`policy-rc.d`. After the transaction, the temp file is removed and
the original (if any) is restored.

Ansible's `apt:` module does **not** install a policy-rc.d. You have
to write:

* A `copy:` to write `/usr/sbin/policy-rc.d` with mode 0755 and
  content `#!/bin/sh\nexit 101\n`.
* A `stat:` to detect pre-existing policy-rc.d.
* A `block:` / `rescue:` / `always:` to ensure cleanup.
* A `command:` to chmod the file.

…and you still have to write the "did one already exist? back it up
and restore it" logic, because Ansible's `copy:` will refuse to
overwrite without `force: yes`, which destroys the backup.

In Go:

```go
func (apt *APT) Transaction(ctx context.Context, fn func(*Tx) error) error {
    guard, err := apt.installPolicyRcD(ctx)
    if err != nil { return err }
    defer guard.Restore(ctx)
    // ... do work ...
}
```

`Guard.Restore` knows whether it created the file or only relabelled
an existing one. Cleanly testable.

### 4. Idempotency via the doctor reuse pattern

The v3 design reuses the per-subsystem `doctor` checks as
idempotency guards inside the phase's `Run` method. That is the same
pattern Ansible modules use internally (`check_mode`), but Ansible
exposes it indirectly — you cannot easily run a single Ansible
module's check from the CLI in the way we want operators to run
`clouddeployctl doctor nvidia`. We would end up writing wrapper
playbooks that just call the module with `-C` and parse the result.

In Go, "the idempotency check is the doctor check" is a single
function reused from two callsites.

## Hybrid considered (Option B): Go + Ansible roles

The most appealing hybrid is "Go owns conditional logic and state,
Ansible owns declarative bits."

Concrete proposal: keep the package family / CUDA / state stuff in
Go; have `clouddeployctl` invoke `ansible-playbook` for "install this
list of apt packages, render this systemd unit, restart these
services."

Problems:

1. **Runtime dependency.** Ansible needs Python 3 + `ansible-core`
   on the target VM. We then have to either preinstall it via
   bootstrap (yet another moving piece) or install it inside the
   first apt transaction (chicken-and-egg with the package transaction
   we want it to run).
2. **Diagnostics across the boundary.** When an Ansible task fails,
   the Go process gets back an exit code and a JSON-ish log. To
   surface a useful message, Go has to parse Ansible's output. The
   `--check` / `--diff` model is great in isolation; round-tripping
   it through subprocess JSON is not.
3. **Two test surfaces.** `go test ./...` for Go modules, Molecule
   for Ansible roles, with a CI matrix that runs both. Doubles the
   test infrastructure for marginal benefit.
4. **Idempotency double-bookkeeping.** Ansible modules are
   idempotent in their own way; Go's state.json is the truth. When
   the two disagree (Ansible says "ok=changed=0" but state.json says
   "running"), debugging becomes a guessing game.
5. **The declarative bits are tiny anyway.** Rendering a systemd
   unit is `text/template` + write file. Installing apt packages is
   the `internal/apt.Transaction` wrapper we have to write regardless
   to handle the deadlock issue. Restarting a service is one
   `systemctl restart` call. Ansible adds overhead, not abstraction,
   on top of those.

## Hybrid considered (Option C): Ansible primary

Same problems as B, amplified. Conditional logic moves to Jinja2 /
`when:`, where it is unreadable and untestable. Rejected without
serious analysis — already covered above.

## Decision

**Option A: Go orchestrator only.**

* All decision logic in Go.
* All subprocess invocation in `internal/runner`.
* All declarative work (apt install, file render, systemd reload) in
  Go using the standard library and a thin set of internal helpers.
* No Ansible.
* Existing Python helpers (`write-edids.py`,
  `validate-hdr-drm-state.py`) remain Python and are invoked from Go.

## Revisit triggers

Reconsider this ADR if any of these become true:

* CloudDeploy expands to provisioning **multiple** VMs as a single
  declarative fleet. Ansible's inventory + push model would then
  add real value.
* The "render systemd unit + restart service" code in `internal/systemd`
  grows past ~500 lines and starts duplicating Ansible's `systemd_unit`
  module. At that point a thin Ansible bridge for unit management
  may be worth it.
* A contributor with deep Ansible expertise volunteers to maintain
  an Ansible-roles port. Even then, the question is whether their
  effort is better spent fixing Go's gaps directly.

Until any of those triggers fire, Ansible is out.

## Consequences

* Faster iteration. Go tests run in seconds, no Ansible runner spin-up.
* Smaller deploy artifact. The target VM gets a single static
  binary + a small set of YAML/template files; no Python+Ansible
  install.
* Single source of truth for state (`state.json`) instead of
  splitting between Ansible's facts and our own state file.
* More code to write than option B. Mitigated by the fact that the
  work CloudDeploy actually does is conditional logic, which Ansible
  was bad at anyway, and the few truly declarative bits are
  one-screen Go functions.
