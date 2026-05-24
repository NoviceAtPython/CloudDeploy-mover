# CloudDeploy v3 architecture

## Why v3 exists

The v2 deploy is a 7500-line Bash script. It works — the live VM
reached confirmed `Moonlight overlay: AV1 10-bit HDR @ 3840x2160 120
FPS` after the long chain of patches documented in
[`final-hdr-success/KNOWN_GOOD_SUNSHINE_STATE.md`](final-hdr-success/KNOWN_GOOD_SUNSHINE_STATE.md).
That run proved every piece of the technical stack (KMS capture +
patched KWin NVIDIA private HDR + Sunshine fork + AV1 NVENC) is real
and stable.

What v2 cannot give us:

* **Idempotency that we can trust.** A 7500-line Bash script with
  ad-hoc phase markers under `/var/lib/clouddeploy/` is brittle. Any
  re-run risks half-applying or double-applying steps.
* **Resumability after reboot.** v2 has a continuation service, but
  the state model is dozens of marker files. We cannot reason about
  "what state is this VM in?" without running the script and watching
  what it does.
* **Conditional logic at scale.** RTX 5090 / GB202 needs
  `nvidia-driver-580-server-open`; L4 wants `nvidia-driver-580-server`;
  RTX 4090 will accept either. Encoding that decision tree in Bash
  branching is hard to read and even harder to test.
* **Diagnostics.** When a VM ends up wedged, the operator has to
  read 7500 lines of script + `journalctl` to figure out what
  happened. There is no `clouddeployctl doctor nvidia` that prints
  a clean per-subsystem report.
* **Testing.** v2 has zero unit tests. Every change is validated by
  running it on a real GPU VM. That is slow and expensive.
* **dpkg/apt deadlocks.** The RTX 5090 deploy showed
  `power-profiles-daemon.postinst` blocked on a
  `deb-systemd-invoke start` that deadlocked behind boot jobs. v2 has
  no transaction wrapper that disables service-start during package
  phases.

v3 is a redesign with those problems as first-class concerns.

## Non-goals (explicit)

v3 is **not** a clean-room rewrite of every line. v2 stays in the
repo at the top level as the validated fallback path until v3 has
demonstrably replaced each phase end-to-end. The migration is
incremental and described in [MIGRATION.md](MIGRATION.md).

v3 does **not** regress the HDR success state. The Sunshine fork pin
(`ec7f60fb8a31042fe03a638bdafcdb3bfe096b87`), the patched KWin
NVIDIA private HDR path, and the
`SUNSHINE_FORCE_AV1_HDR10` / `SUNSHINE_SYNTHESIZE_HDR10_METADATA` env
vars all move forward verbatim. The architectural rules in
[`final-hdr-success/KNOWN_GOOD_SUNSHINE_STATE.md`](final-hdr-success/KNOWN_GOOD_SUNSHINE_STATE.md)
remain authoritative.

v3 does **not** add cluster orchestration. CloudDeploy operates on
**one** machine: the VM that will host Sunshine. Multi-host
orchestration belongs to whatever provisioner creates the VM
(Terraform, Pulumi, etc.). This is why we picked Go over Ansible
(see [ADR-0001](ADR-0001-orchestrator-language.md) and
[ADR-0002](ADR-0002-ansible-vs-native-go.md)).

## Language

Go for the orchestrator, with a small Bash bootstrap and a couple
of existing Python helpers retained where they already pull their
weight.

* `bootstrap.sh` — tiny Bash glue: installs Go (if missing), clones
  the repo, builds `clouddeployctl`, exec's it. ~100 lines, ideally
  fewer.
* `clouddeployctl` — Go binary, single static executable. Owns all
  decision logic, state, logging, and the orchestration loop.
* `helpers/write-edids.py` — kept as-is. Generates EDID binaries for
  forced virtual outputs; the existing implementation is small and
  correct. Eventually we may port this to Rust (see [Rust later
  candidates](#rust-as-a-later-candidate)), but not for v1.

Why Go: [ADR-0001](ADR-0001-orchestrator-language.md).
Why not Ansible: [ADR-0002](ADR-0002-ansible-vs-native-go.md).

## Repository layout

```
.
├── bootstrap.sh                # Bash glue
├── CloudDeploy-wayland.sh      # v2 legacy, untouched
├── CloudDeploy.ps1             # v2 legacy, untouched
├── clouddeploy-run-rootless-systemd.sh  # v2 legacy, untouched
├── cmd/
│   └── clouddeployctl/
│       └── main.go             # CLI entrypoint (Cobra)
├── internal/
│   ├── apt/                    # dpkg/apt transaction wrapper
│   ├── config/                 # YAML profile/GPU loader
│   ├── cuda/                   # cuda.mode = none|optional|required
│   ├── edid/                   # EDID gen (calls helpers/write-edids.py)
│   ├── gpu/                    # lspci-based GPU detection
│   ├── kwin/                   # KWin patch / build / install
│   ├── nvidia/                 # Driver package family selector
│   ├── reboot/                 # Continuation service / resume
│   ├── runner/                 # Subprocess wrapper + structured logs
│   ├── state/                  # state.json schema + load/save
│   ├── sunshine/               # Sunshine fork pin / build / install
│   ├── systemd/                # Systemd unit templating
│   └── validate/               # Doctor + post-deploy validators
├── config/
│   ├── profiles/
│   │   ├── hdr-4k120.yaml      # Production HDR profile
│   │   └── sdr-safe.yaml       # Safe-mode SDR
│   └── gpus/
│       ├── rtx5090.yaml
│       ├── rtx4090.yaml
│       └── l4.yaml
├── templates/
│   └── systemd/
│       ├── sunshine-headless.service.tmpl
│       ├── kwin-realvt.service.tmpl
│       ├── plasma-shell-realvt.service.tmpl
│       └── clouddeploy-continue.service.tmpl
├── helpers/
│   └── write-edids.py          # Python EDID generator (existing)
├── docs/
│   ├── ARCHITECTURE.md         # this file
│   ├── MIGRATION.md
│   ├── ADR-0001-orchestrator-language.md
│   ├── ADR-0002-ansible-vs-native-go.md
│   ├── HDR-NVIDIA-PRIVATE.md           # v2 doc, still authoritative
│   ├── SUNSHINE-HDR-NEGOTIATION.md     # v2 doc, still authoritative
│   └── final-hdr-success/
│       └── KNOWN_GOOD_SUNSHINE_STATE.md # v2 doc, still authoritative
├── tests/
│   ├── apt/
│   ├── cuda/
│   ├── nvidia/
│   └── state/
├── legacy/
│   └── README.md               # Pointer at v2 files at repo root
├── patches/
│   └── kwin-clouddeploy-nvidia-private-hdr.patch  # v2 patch, reused
├── scripts/
│   └── validate-hdr-drm-state.py       # v2 helper, reused
├── go.mod
├── go.sum
└── Makefile
```

v2 files (`CloudDeploy-wayland.sh`, `CloudDeploy.ps1`, the existing
`docs/`, `patches/`, `scripts/`) are not moved or deleted. They stay
at the top level. Operators following raw-curl deploys against
`origin/v2` continue to work without change.

## State model

A single JSON file at `/var/lib/clouddeploy/state.json` is the
canonical source of truth for "what has been done on this VM."

```jsonc
{
  "version": 3,
  "profile": "hdr-4k120",
  "started_at": "2026-05-16T12:00:00Z",
  "finished_at": null,
  "phases": {
    "ubuntu_upgrade": {
      "status": "done",
      "from": "24.04",
      "to": "25.10",
      "completed_at": "2026-05-16T12:05:00Z"
    },
    "base_packages": {
      "status": "done",
      "completed_at": "2026-05-16T12:10:00Z"
    },
    "nvidia_driver": {
      "status": "done",
      "gpu": "NVIDIA GeForce RTX 5090",
      "pci_id": "10de:2b85",
      "package_family": "server-open",
      "driver_package": "nvidia-driver-580-server-open",
      "dkms_package": "nvidia-dkms-580-server-open",
      "driver_version": "580.126.20",
      "kernel_module": "open",
      "completed_at": "2026-05-16T12:25:00Z"
    },
    "cuda": {
      "status": "skipped",
      "mode": "optional",
      "reason": "not required for KMS/NVENC/HDR path"
    },
    "kwin_patch": { "status": "pending" },
    "sunshine_build": {
      "status": "pending",
      "commit": "ec7f60fb8a31042fe03a638bdafcdb3bfe096b87"
    },
    "validation": { "status": "pending_moonlight_connect" }
  }
}
```

Phase status values:

| Status | Meaning |
| --- | --- |
| `pending` | Not started. |
| `running` | In progress (current invocation owns the lock). |
| `done` | Completed successfully. |
| `skipped` | Intentionally not run for this profile/config. |
| `failed_fatal` | Failed; deploy is dead until operator fixes it. |
| `failed_nonfatal` | Failed but the profile allows skipping. |
| `pending_moonlight_connect` | Cannot validate without a client connect. |

Phases write **only** when their work completes. A crash mid-phase
leaves the phase at `running` from the previous invocation; the next
mutating run first acquires the process lock, then rewrites stale
`running` phases to `pending` with interrupted-run recovery guidance
and retries them from scratch.

The state file is the contract:

* `clouddeployctl apply` reads it to decide which phases to skip.
* `clouddeployctl resume` reads it to decide where to pick up.
* `clouddeployctl doctor` reads it to print a clean per-subsystem
  report without re-running anything.
* External tools (CI, monitoring) can parse it.

Every write goes through a transactional helper (temp file +
fsync + rename). A lock file (`/var/lib/clouddeploy/state.lock`)
prevents concurrent `clouddeployctl` invocations.

## Logging

All output goes through `internal/runner`:

* Console: human-readable, structured ("phase=nvidia step=install
  package=nvidia-driver-580-server-open").
* `/var/log/clouddeploy/clouddeploy.log` — combined log, JSON lines.
* `/var/log/clouddeploy/<phase>.log` — per-phase plain-text log,
  including raw subprocess stdout/stderr.

Subprocess invocation goes through `runner.Exec(ctx, cmd, opts)`
which:

* Times each command.
* Streams stdout/stderr to the phase log.
* Returns exit code + duration + captured tail for the JSON log.
* Wraps long-running commands with a progress indicator (the
  "Sunshine fork build" phase is ~10 minutes on a 2-vCPU VM and v2
  goes silent during that window).

No `os/exec` calls outside of `internal/runner`. This is enforced
by a `go vet`-style lint pass in CI.

## Subsystem ownership

| Package | Owns |
| --- | --- |
| `internal/apt` | dpkg/apt transactions, policy-rc.d guard, repair-dpkg-state, lock waiting. |
| `internal/config` | Loading YAML profile + GPU config; merging into runtime config. |
| `internal/cuda` | `cuda.mode = none|optional|required`. Runfile retry-suppression. Apt-package install. Version detection. |
| `internal/edid` | Generating EDID binaries (invokes `helpers/write-edids.py`); writing them under `/lib/firmware/edid/`. |
| `internal/gpu` | Detecting GPU(s) via `lspci -nn` + driver heuristics. |
| `internal/kwin` | Pinning patched KWin; building/installing the patch; locking via apt-pinning. |
| `internal/nvidia` | Package family selector (`server` / `server-open` / `non-server` / `non-server-open`); driver install; dkms verification. |
| `internal/reboot` | Continuation systemd service; reboot scheduling; resume after boot. |
| `internal/runner` | Subprocess wrapper; structured logs. |
| `internal/state` | `state.json` load/save; phase status transitions; locking. |
| `internal/sunshine` | Fork pin commit; build; dual-install (`/usr/local/bin/sunshine{-clouddeploy,}`); setcap; `sunshine.conf` generation; HDR env wiring. |
| `internal/systemd` | Unit templating (`text/template`); reload/restart; per-unit dependencies. |
| `internal/validate` | `doctor` subcommands per subsystem; `validate hdr-stream` (journal grep for the success markers). |

Each package exposes a tiny public surface: typically a `Run(ctx,
deps) error` entry point and a `Doctor(ctx, deps) Report` query.
Internal package layout is its own concern.

## Idempotency strategy

Every phase follows the same pattern:

```go
func (p *Phase) Run(ctx context.Context, deps *Deps) error {
    if deps.State.Phase("nvidia_driver").Status == StatusDone {
        if p.alreadyCorrect(ctx) {
            return nil
        }
        // State says done but reality disagrees -> log warning + redo.
    }
    deps.State.MarkRunning("nvidia_driver")
    // ... do work ...
    deps.State.MarkDone("nvidia_driver", details)
    return nil
}
```

`alreadyCorrect` is a cheap query against the actual system (e.g.
`nvidia-smi` works + `dkms status` agrees + `dpkg -l` matches the
expected package family). It is the same query `doctor` uses. The
phase-level idempotency check is the doctor check, reused.

This is the same pattern Ansible modules use internally, with one
difference: in Go we can write arbitrary conditional logic, return
typed errors, and unit-test the decision tree without spinning up a
container.

## Profiles

`config/profiles/*.yaml` are the operator's high-level intent.

`hdr-4k120.yaml` (production HDR target):

```yaml
profile: hdr-4k120
ubuntu_version: "25.10"
display:
  resolution: 3840x2160
  refresh: 120
  hdr: true
  forced_connector: DP-1
nvidia:
  driver_major: "580"
  prefer_open_family: true   # required for Blackwell; harmless for older
cuda:
  mode: none                 # KMS+NVENC+HDR path doesn't need CUDA
sunshine:
  source: fork
  fork_repo: https://github.com/NoviceAtPython/Sunshine
  fork_branch: codex/sunshine-pairing-diagnostics
  fork_commit: ec7f60fb8a31042fe03a638bdafcdb3bfe096b87
  encoder: nvenc
  capture: kms
  force_av1_hdr10: true
  synthesize_hdr10_metadata: true
kwin:
  patched_hdr: true
  patch: patches/kwin-clouddeploy-nvidia-private-hdr.patch
```

`sdr-safe.yaml` (no HDR, no patched KWin, packaged Sunshine):

```yaml
profile: sdr-safe
ubuntu_version: "24.04"
display:
  resolution: 1920x1080
  refresh: 60
  hdr: false
nvidia:
  driver_major: "580"
  prefer_open_family: false
cuda:
  mode: none
sunshine:
  source: deb
  encoder: nvenc
  capture: kms
kwin:
  patched_hdr: false
```

`config/gpus/*.yaml` are GPU-specific overrides merged on top of the
profile after detection:

```yaml
# config/gpus/rtx5090.yaml
match:
  pci_ids: ["10de:2b85"]
  name_patterns: ["GeForce RTX 5090"]
nvidia:
  prefer_open_family: true   # GB202 requires open kernel module
  driver_major_min: "580"
  notes:
    - "Blackwell consumer GPU; OK with server-open or non-server-open."
    - "Standard NVIDIA closed kernel module rejects this PCI ID."
```

GPU config is loaded *after* GPU detection and merged into the
effective config. The merge precedence is: profile YAML <
gpu-specific YAML < runtime overrides from `clouddeployctl apply
--set`.

## Subcommand surface

```
clouddeployctl apply --profile hdr-4k120
clouddeployctl resume
clouddeployctl doctor
clouddeployctl doctor nvidia
clouddeployctl doctor cuda
clouddeployctl doctor kwin
clouddeployctl doctor sunshine
clouddeployctl validate hdr-stream [--since "10 minutes ago"]
clouddeployctl phase base-packages
clouddeployctl phase nvidia-driver
clouddeployctl phase cuda
clouddeployctl phase kwin-patch
clouddeployctl phase sunshine-build
clouddeployctl phase sunshine-config
clouddeployctl phase tailscale
clouddeployctl phase pipewire-audio
clouddeployctl phase streaming-services
clouddeployctl phase stream-validate
clouddeployctl phase optional-apps
clouddeployctl state show
clouddeployctl state reset --phase nvidia_driver  # opt-in destructive
```

* `apply` runs all phases in order. Skips phases already `done`.
* `resume` is `apply` after a reboot — same code path, the
  continuation systemd unit just calls `clouddeployctl apply` with
  the profile from `state.json`.
* `doctor` is read-only. It re-runs every check that a phase's
  idempotency guard runs, prints a per-subsystem report.
* `phase <name>` runs exactly one phase. Useful when iterating in
  development.
* `state show` is `cat state.json` with formatting.
* `state reset --phase X` is the explicit "I know what I'm doing"
  knob to force a phase to re-run.

## Testing

`go test ./...` is the entry point.

Three tiers of test:

1. **Unit tests** (in-package, `_test.go`). Fast. No subprocess. Cover:
   * NVIDIA package family selector (decision matrix per GPU /
     `dmesg` evidence / installed-package state).
   * CUDA policy: required/optional/none + same-SHA retry suppression.
   * apt policy-rc.d guard: no-existing-file / existing-file /
     stale-CloudDeploy-file paths.
   * State JSON load/save / phase status transitions.
   * Config merge (profile + GPU + runtime overrides).
   * Subprocess wrapper (using `os/exec` against a stub helper).

2. **Integration tests** (`tests/<subsystem>/`). May fork helper
   binaries. Cover dpkg-state-repair against fake fixture files.

3. **End-to-end** (out of scope for v1 — runs on real VM in CI).

Initial unit tests in the first commit:

* `internal/nvidia/family_test.go` — covers the eight selection
  scenarios from the requirements (RTX 5090 → server-open; RTX 4090 →
  server accepted; L4 → server; installed-server-open validation
  scenarios; dmesg "open kernel modules" wins).
* `internal/cuda/policy_test.go` — covers required / optional / none
  + same-SHA retry suppression.
* `internal/apt/policy_rc_d_test.go` — covers all three guard
  scenarios.
* `internal/state/state_test.go` — load/save round-trip + phase
  status transitions.

## Bootstrap flow

A new VM provisioned by Terraform / Pulumi / hand:

```bash
curl -fsSL https://raw.githubusercontent.com/NoviceAtPython/CloudDeploy-mover/v3/bootstrap.sh \
  | sudo ENABLE_HDR=1 PROFILE=hdr-4k120 bash
```

`bootstrap.sh` does ~3 things:

1. Ensure a recent Go toolchain is available (apt install golang-go
   on ≥ 24.04 ships Go 1.22+; or pull a release tarball if older).
2. `git clone --branch v3 https://github.com/NoviceAtPython/CloudDeploy-mover.git /opt/clouddeploy-mover`.
3. `cd /opt/clouddeploy-mover && go build -o /usr/local/bin/clouddeployctl ./cmd/clouddeployctl && exec /usr/local/bin/clouddeployctl apply --profile "${PROFILE:-hdr-4k120}"`.

The first run installs the continuation systemd unit. Reboots are
handled by the continuation unit calling `clouddeployctl resume`.

## Rust as a later candidate

A second phase (v3.1+) will likely move a few specific things to
Rust:

* **EDID parsing/generation.** `edid-rs` is well-maintained and
  catches malformed EDIDs that the Python helper currently
  produces by hand.
* **DRM/KMS property inspection.** `drm-rs` and `libdrm` bindings
  let us replace `drm_info -j | python3 validate-hdr-drm-state.py`
  with a native validator that prints actionable errors.
* **HDR metadata validation.** Parsing the EDID HDR Static Metadata
  Data Block (CTA-861.3) and confirming the EOTF / max-luminance
  values match what we expect.

These are leaf utilities and would compile to single binaries
invoked by Go. Not v1 scope; called out so the architecture leaves
room for them.

## What we deliberately are not using

* **Ansible.** See [ADR-0002](ADR-0002-ansible-vs-native-go.md).
  Short version: CloudDeploy is single-host, Ansible's value is
  multi-host. The complex parts of v3 are conditional logic, not
  declarative state, and conditional logic is hard to express
  cleanly in Ansible playbooks.
* **Nix / NixOS.** Too heavy a switch for an Ubuntu / NVIDIA / KWin
  / Sunshine target. The KWin patches and NVIDIA driver flow are
  Ubuntu-specific in non-trivial ways. Maybe relevant if we ever
  pivot to a NixOS-based gaming VM, but not now.
* **Container runtime.** Sunshine + KWin Wayland + KMS capture need
  bare-metal hardware access. Containerising the orchestrator is
  pointless; we already run on a dedicated VM.
* **Yet another configuration tool.** No Chef, Puppet, Salt. Same
  reason as Ansible.
