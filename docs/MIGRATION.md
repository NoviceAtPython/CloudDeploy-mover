# CloudDeploy v2 → v3 migration plan

## Ground rules

1. **v2 stays usable until v3 has demonstrably replaced each phase
   end-to-end on the live VM.** `CloudDeploy-wayland.sh` at the repo
   root is the validated fallback path. Deletion is the very last
   step, not the first.
2. **The HDR success state is the floor, not the ceiling.** Every
   v3 milestone has to either reach or exceed the v2-known-good HDR
   state from
   [`final-hdr-success/KNOWN_GOOD_SUNSHINE_STATE.md`](final-hdr-success/KNOWN_GOOD_SUNSHINE_STATE.md).
   A v3 milestone that ships before it can reproduce
   `Moonlight overlay: AV1 10-bit HDR` is a regression.
3. **No phase moves to v3 without unit tests for its decision logic.**
   The phases that currently make v2 fragile (NVIDIA package family
   selection, CUDA retry policy, apt/dpkg deadlock recovery) are the
   ones we want covered by tests before we trust v3 to run them.

## Milestones

### Milestone 1 — Skeleton + docs + first tests (this commit)

Deliverables:

* New `v3` git branch off `v2`.
* `docs/ARCHITECTURE.md`, `docs/MIGRATION.md`,
  `docs/ADR-0001-orchestrator-language.md`,
  `docs/ADR-0002-ansible-vs-native-go.md`.
* Compileable Go skeleton: `go.mod`, `cmd/clouddeployctl/main.go`,
  `internal/<package>/` stubs.
* **Real implementations** for the three modules whose logic was
  most painful in v2:
  * `internal/nvidia` — package family selector with full decision
    matrix tests.
  * `internal/cuda` — `mode = none|optional|required` policy with
    same-SHA retry suppression tests.
  * `internal/apt` — policy-rc.d guard with all three precondition
    scenarios tested.
* Initial unit tests under each of those packages (using `go test
  ./...`).
* `bootstrap.sh` that installs Go, clones the repo, builds the
  binary, runs `clouddeployctl --help` (no destructive actions yet).
* Config profiles + GPU configs: `config/profiles/{hdr-4k120,sdr-safe}.yaml`,
  `config/gpus/{rtx5090,rtx4090,l4}.yaml`.
* Systemd template scaffolds under `templates/systemd/`.
* `legacy/README.md` documenting that v2 files at the repo root are
  the supported path until v3 catches up.

Acceptance:

* `go build ./...` succeeds.
* `go test ./...` passes the new unit tests.
* v2 files (`CloudDeploy-wayland.sh`, `docs/HDR-NVIDIA-PRIVATE.md`,
  `docs/SUNSHINE-HDR-NEGOTIATION.md`, `docs/final-hdr-success/`,
  `scripts/validate-hdr-drm-state.py`, `patches/`) are not modified
  in this commit.

Non-goals:

* Not running on a real VM yet.
* No replacement of any v2 deploy phase.
* No `clouddeployctl apply` end-to-end.

### Milestone 2 — Doctor + the three modules wired in

Deliverables:

* `clouddeployctl doctor nvidia` works against a real VM. It prints
  GPU model, PCI ID, dmesg evidence, installed-package family,
  driver version, dkms status. Read-only.
* `clouddeployctl doctor cuda` reports CUDA mode, whether nvcc /
  libcudart is present, and whether the runfile (if present) passes
  `--check`.
* `clouddeployctl doctor apt` reports dpkg state (locked? broken?
  any pending postinst?), policy-rc.d presence, and pending
  service-start jobs that might deadlock a package install.
* `internal/apt.Transaction(ctx, deps, func(tx *Tx) error)` —
  installs policy-rc.d, runs the callback, restores any pre-existing
  policy-rc.d, removes ours, surfaces dpkg errors.
* `internal/nvidia.SelectFamily(ctx, deps, gpu) Family` — used by
  doctor, not yet by an `apply` phase.
* `internal/cuda.Plan(ctx, deps, mode) Plan` — same, used by doctor.

Acceptance:

* Operator can SSH to the live VM and run `clouddeployctl doctor`
  to get a clean report without any destructive action.
* `doctor` agrees with `nvidia-smi`, `dkms status`, `dpkg -l`,
  `journalctl -u <unit>` on at least one RTX 5090 VM and one
  RTX 4090 VM.

### Milestone 3 — Bootstrap + delegating to v2

Deliverables:

* `bootstrap.sh` actually invokes `clouddeployctl apply --profile
  hdr-4k120` on a fresh VM.
* `clouddeployctl apply` runs the v3-implemented phases for the
  ones we have ported (base packages with policy-rc.d guard,
  doctor as a pre-flight). For phases we have not yet ported (KWin
  patch, Sunshine build, EDID, services, validation), it shells
  out to `CloudDeploy-wayland.sh` with the existing env-file flow,
  recording per-phase status in `state.json`.
* State file (`/var/lib/clouddeploy/state.json`) is the truth.
* `clouddeployctl resume` works across reboots (continuation
  systemd unit).

Acceptance:

* A fresh RTX 4090 / RTX 5090 VM goes from clean Ubuntu 24.04 →
  Moonlight `AV1 10-bit HDR` end-to-end via `bootstrap.sh`, using
  v3 for the phases listed and falling back to v2 for the rest.
* Manual rollback path (`clouddeployctl state reset --phase X` and
  fall back to `CloudDeploy-wayland.sh`) works.

### Milestone 4 — Port the hard phases

In rough order of complexity:

1. **`internal/sunshine`** — pin commit, clone, build, dual-install,
   setcap, write `sunshine.conf`, generate `sunshine-headless.service`
   with the HDR env vars. Replaces `install_sunshine_from_fork_if_requested`
   from v2.
2. **`internal/kwin`** — apt-pin patched KWin packages, apply the
   patch, build, install, drop marker. Replaces `build_install_patched_kwin`.
3. **`internal/edid`** — generate forced-mode EDID, write under
   `/lib/firmware/edid/`, wire into kernel cmdline (`drm.edid_firmware=...`).
4. **`internal/systemd`** — generate all unit files (compositor,
   shell, Sunshine, watch, continuation) from templates with the
   real values.
5. **`internal/reboot`** — schedule reboots; continuation unit is
   the only piece that ever calls `systemctl reboot`.
6. **`internal/validate`** — port the v2 streaming validators,
   add `clouddeployctl validate hdr-stream`.

Each phase ports independently and is gated behind a config flag so
the operator can pick "use v3 for sunshine, fall back to v2 for kwin"
during the transition.

Acceptance:

* Each ported phase has tests for its decision logic.
* Each ported phase has a `doctor` subcommand.
* End-to-end deploys with all-v3 phases reach the HDR success state.

### Milestone 5 — v3 default, v2 legacy

* `bootstrap.sh` defaults to all v3 phases.
* v2 files move to `legacy/` (script + the v2-style README pointer).
  The repo root no longer has `CloudDeploy-wayland.sh`.
* `legacy/CloudDeploy-wayland.sh` is preserved verbatim. Operators
  with old `curl | bash` snippets pointing at the raw script keep
  working if they pin to `origin/v2`.
* Docs:
  * `docs/HDR-NVIDIA-PRIVATE.md`, `docs/SUNSHINE-HDR-NEGOTIATION.md`,
    `docs/final-hdr-success/KNOWN_GOOD_SUNSHINE_STATE.md` remain
    authoritative for the design rationale; they describe what
    `internal/kwin` and `internal/sunshine` implement.
* `v2` branch stays alive on origin indefinitely.

## Phase porting checklist

Each phase port follows the same template:

1. Identify the v2 function and its inputs/outputs.
2. Write the Go module: types, public surface, idempotency check.
3. Write unit tests for the decision logic.
4. Add a `doctor <phase>` subcommand that runs the idempotency
   check and prints a per-subsystem report.
5. Wire it into `clouddeployctl apply` behind a config flag.
6. Validate on a real VM in both states (`config flag off` → v2
   path still used; `config flag on` → v3 path used; both reach the
   HDR success state).
7. Document the v2 → v3 mapping in this file's "Phase mapping" table
   below.

## Phase mapping (v2 → v3)

| v2 function in `CloudDeploy-wayland.sh` | v3 owner | Milestone |
| --- | --- | --- |
| `maybe_upgrade_ubuntu` + `direct_apt_codename_upgrade` | `internal/apt` (release-upgrade subcommand) | M4 |
| `apt_install_wait` / `apt_purge_wait` | `internal/apt.Transaction` | M2 |
| `repair_dpkg_state_if_needed` | `internal/apt.RepairDpkgState` | M2 |
| `install_target_nvidia_driver` | `internal/nvidia.Install` | M4 |
| `install_cuda_toolkit_if_requested` + `install_cuda_toolkit_from_runfile` | `internal/cuda.Install` | M4 |
| `build_install_patched_kwin` + `write_patched_kwin_apt_pin` | `internal/kwin.Install` | M4 |
| `install_sunshine_from_fork_if_requested` | `internal/sunshine.Install` | M4 |
| `write_sunshine_config` | `internal/sunshine.WriteConfig` | M4 |
| `install_clouddeploy_systemd_units` | `internal/systemd.Apply` | M4 |
| `install_clouddeploy_helpers` (the helper-script writer) | `internal/systemd.Helpers` + `cmd/clouddeployctl/cmd_doctor.go` etc. | M4 |
| `known_good_clean_reset_streaming_stack` | `clouddeployctl phase reset-streaming` | M4 |
| `validate_hdr_final_state` | `internal/validate.HdrDrmState` | M4 |
| `validate_streaming_stack_ready` | `internal/validate.StreamingStack` + per-subsystem `doctor` | M2-M4 |
| Continuation service / `schedule_reboot_for_continuation` | `internal/reboot.Continue` | M3 |

The two existing v2 Python helpers carry over unchanged:

| v2 Python helper | Status in v3 |
| --- | --- |
| `scripts/validate-hdr-drm-state.py` | `internal/validate.HdrDrmState` calls it. Stays in `scripts/` until a Rust replacement lands. |
| (would-be `scripts/write-edids.py`) | Becomes `helpers/write-edids.py`, invoked by `internal/edid.Write`. |

## Rollback

`v2` is the rollback target at every milestone.

Per-VM rollback:

```bash
# Stop v3.
sudo systemctl stop clouddeploy-continue.service 2>/dev/null || true

# Wipe v3 state. v2 markers under /var/lib/clouddeploy/ are kept.
sudo rm -f /var/lib/clouddeploy/state.json

# Switch to v2 branch on the repo checkout.
cd /opt/clouddeploy-mover
sudo git fetch origin v2
sudo git checkout v2

# Re-run v2.
sudo ENABLE_HDR=1 bash ./CloudDeploy-wayland.sh
```

Repository rollback (worst case):

* `git revert` the v3 merge commit on `origin/main` if v3 was merged
  back. v2 branch is untouched throughout this migration so the
  branch tip is the rollback target.

## Risk register

| Risk | Mitigation |
| --- | --- |
| `clouddeployctl` regresses HDR success state | Every phase port keeps the v2 fallback available behind a config flag until the v3 version is validated end-to-end on a real VM. |
| State JSON corruption | Transactional writes (temp + fsync + rename). Lock file. `clouddeployctl state show` is read-only. |
| Operator runs `apply` and `phase X` concurrently | Lock file. Second invocation refuses with a clear error. |
| Go toolchain unavailable on older Ubuntu | `bootstrap.sh` falls back to downloading the official tarball if the apt package is too old. |
| Sunshine fork commit moves | `SUNSHINE_FORK_COMMIT` pin in profile YAML. `internal/sunshine.Install` errors loudly if the pin is unreachable. |
| Ansible advocates push back | [ADR-0002](ADR-0002-ansible-vs-native-go.md) documents the reasoning. Open to revisiting if v3 grows past CloudDeploy's single-host scope. |
