# v3 deployment readiness audit

Snapshot taken at the start of Milestone 2 work. The goal is a no-contact
deploy on an Ubuntu cloud VM with an NVIDIA GPU; the audit makes the
remaining gap explicit so the implementation work is targeted at the
right failure modes.

## 1. Current stubs

`clouddeployctl` subcommands grouped by readiness:

| Subcommand | Status |
| --- | --- |
| `doctor nvidia` | **Real** (Milestone 1.1). Reads host evidence, classifies the GPU, picks a package family, prints the decision and any error. |
| `doctor cuda` | **Real-ish** (Milestone 1.1). Prints the CUDA mode for the default profile but does not load the real profile or check for nvcc. Tightened in Milestone 2. |
| `doctor apt` | Stub. Milestone 2. |
| `doctor system` | Missing. Milestone 2. |
| `doctor kwin` | Stub. Milestone 4. |
| `doctor sunshine` | Stub. Milestone 4. |
| `validate hdr-stream` | Stub. Milestone 4. |
| `state show` | **Real**. Pretty-prints `/var/lib/clouddeploy/state.json`. |
| `state reset` | Stub. Milestone 2. |
| `apply` | Stub. Milestone 3 partial in this commit (runs only base-packages / nvidia-driver / cuda then exits with a clear partial-apply banner). |
| `resume` | Stub. Milestone 3 partial (resumes the same three phases). |
| `phase base-packages` | Stub. Milestone 3 partial in this commit. |
| `phase nvidia-driver` | Stub. Milestone 3 partial in this commit. |
| `phase cuda` | Stub. Milestone 3 partial in this commit. |
| `phase kwin-patch` | Stub. Milestone 4. |
| `phase sunshine-build` | Stub. Milestone 4. |
| `phase services` | Stub. Milestone 4. |

`internal/` packages grouped by readiness:

| Package | Status |
| --- | --- |
| `internal/nvidia` | **Real**. Family selector with apt-availability + soft/hard PreferOpenFamily; `Classify(name, pciID)` taxonomy; tests cover the v3-brief matrix. |
| `internal/cuda` | **Real** for the policy decision (`Plan(mode) PlanResult`) and same-SHA retry suppression (`RetryDecider`). Install side is Milestone 3 (this commit, minimal). |
| `internal/apt` | Half-real: policy-rc.d guard is real with tests; `Transaction(ctx, fn)` wrapper is Milestone 2 in this commit. |
| `internal/state` | Real for load/save/atomic-write. Locking + RebootNeeded + ResumeTarget added in Milestone 2 (this commit). |
| `internal/config` | Real for profile + GPU YAML loading + schema validation. |
| `internal/runner` | Stub. Implemented in Milestone 2 (this commit). |
| `internal/gpu` | Stub (Milestone 2). `internal/nvidia.GatherEvidenceFromHost` covers the NVIDIA case for now. |
| `internal/kwin` | Stub. Milestone 4. |
| `internal/sunshine` | Stub. Milestone 4. |
| `internal/edid` | Stub. Milestone 4. |
| `internal/systemd` | Stub. Milestone 4. |
| `internal/validate` | Stub. Milestone 4. |
| `internal/reboot` | Stub. Milestone 3 partial in this commit (continuation systemd unit; reboot scheduling). |

## 2. Phases required for a full `hdr-4k120` deployment

The complete chain v3 has to own end-to-end to reach the
[`Moonlight overlay: AV1 10-bit HDR`](final-hdr-success/KNOWN_GOOD_SUNSHINE_STATE.md)
success state:

| Order | Phase | Purpose |
| --- | --- | --- |
| 1 | `ubuntu_upgrade` (optional) | `do-release-upgrade` for VMs starting on an older release. Out of scope for v3.0; v2 path stays available. |
| 2 | `base_packages` | apt-installs the minimum tooling v3 needs: git, curl, ca-certificates, build-essential, pkg-config, python3, python3-venv, jq, pciutils, lsb-release, systemd. |
| 3 | `nvidia_driver` | Selects the correct package family (Milestone 1.1 logic), installs `nvidia-driver-${MAJOR}-${SUFFIX}` + matching dkms, verifies via `nvidia-smi`. May request reboot. |
| 4 | `cuda` | Obeys `cuda.mode = none / optional / required`. hdr-4k120 sets `none`. |
| 5 | `kwin_patch` (HDR profiles only) | Apt-pins the patched-KWin packages from CloudDeploy's PPA / source build, applies the NVIDIA private HDR patch, drops the marker file. |
| 6 | `sunshine_build` | Clones `NoviceAtPython/Sunshine` at the pinned commit (`464bccf1`), builds via cmake/ninja, installs to both `/usr/local/bin/sunshine{-clouddeploy,}`, setcap `cap_sys_admin,cap_net_bind_service,cap_sys_nice+ep`. |
| 7 | `sunshine_config` | Writes `sunshine.conf` (no `hdr=` / `fps=` / `resolutions=`), CSRF allowlist (localhost + 127.0.0.1 + Tailscale IP). |
| 8 | `edid` | Generates forced-EDID binary, drops it under `/lib/firmware/edid/`, edits `/etc/default/grub` to add `drm.edid_firmware=...` + NVIDIA modeset cmdline. May request reboot. |
| 9 | `systemd_units` | Renders + installs `kwin-realvt.service`, `plasma-shell-realvt.service`, `sunshine-headless.service` (with `CAP_SYS_NICE` ambient cap + HDR env vars), `clouddeploy-continue.service`. |
| 10 | `service_start` | Enables and starts the unit chain. |
| 11 | `hdr_drm_validation` | Calls `scripts/validate-hdr-drm-state.py` (or a future Rust replacement) to confirm `NV_CRTC_REGAMMA_TF=PQ` + `NV_INPUT_COLORSPACE=BT.2100 PQ` + `NV_PLANE_DEGAMMA_TF=PQ`. |
| 12 | `hdr_stream_validation` | After a Moonlight client connects: grep the Sunshine journal for `Sent HDR mode control packet to Moonlight: enabled=1`. Marked `pending_moonlight_connect` until the operator runs it. |

**This commit implements 2, 3, 4 only.** Plus `clouddeployctl resume`
covers reboot continuity for those three phases. The
[v3 roadmap](V3-ROADMAP.md) tracks 5..12 as Milestone 4.

## 3. v2 → v3 mapping for the three phases this commit lands

| v2 function in `CloudDeploy-wayland.sh` | v3 owner | Notes |
| --- | --- | --- |
| `apt_install_wait` / `apt_purge_wait` | `internal/apt.Transaction(ctx, fn)` | apt-get install/update/purge with policy-rc.d guard + dpkg-lock wait + dpkg --audit. |
| `repair_dpkg_state_if_needed` | `internal/apt.Transaction` (`RepairDpkgState` step inside the wrapper) | Runs `dpkg --configure -a` when audit reports a half-configured state. |
| `dpkg_output_has_*` helpers | `internal/apt.parseErrors` (in Milestone 2; minimal in this commit) | Parses apt/dpkg output to surface useful messages. |
| `install_target_nvidia_driver` | `internal/phase.NvidiaDriver` + `internal/nvidia.SelectFamily` | Family selection + `apt.Transaction` install + `nvidia-smi` smoke test. |
| `install_cuda_toolkit_if_requested` | `internal/phase.Cuda` (uses `internal/cuda.Plan`) | Skip / optional / required behaviour from `cuda.Plan`. Runfile retry suppression from `cuda.RetryDecider`. |
| `install_continuation_service` + `schedule_reboot_for_continuation` | `internal/reboot` (minimal in this commit) | Installs `clouddeploy-continue.service`; schedules `systemctl reboot`. The continuation unit invokes `clouddeployctl resume`. |
| `write_clouddeploy_env_file` + `clouddeploy-write-env` | n/a in v3. v3 reads config from YAML + flags, not from a sourced env file. The v2 env-file mechanism stays for v2 callers; v3 ignores it. |
| `validate_streaming_stack_ready` | `internal/phase` post-phase checks + `doctor` subcommands. Streaming-side validation is Milestone 4. |

## 4. Legacy files (Ubuntu-only pivot)

These files stay in the repo for v2 fallback but are **not** part of
the v3 deploy path and should not block v3 work:

| Path | Status | What does v3 do? |
| --- | --- | --- |
| `CloudDeploy-wayland.sh` | v2 entrypoint, **validated** for HDR success on the live VM. | v3 does not call it. v2 callers may still invoke it directly. |
| `CloudDeploy.ps1` | Windows-side helper for the v2 Windows VM provisioning path. | v3 does not target Windows VMs. `.gitattributes` marks it `linguist-vendored linguist-generated linguist-detectable=false` so it doesn't pollute language stats. |
| `main.py` | Python orchestration for the v2 Windows VM path. | v3 does not target Windows VMs. Same treatment as `CloudDeploy.ps1`. Marked retired in `docs/UBUNTU-ONLY.md`. |
| `clouddeploy-run-rootless-systemd.sh` | v2 rootless variant. | Out of v3 scope. Stays for v2 callers. |
| `scripts/validate-hdr-drm-state.py` | Used by both v2 and (eventually) v3 `internal/validate`. | Kept; not legacy. |
| `patches/kwin-clouddeploy-nvidia-private-hdr.patch` | Used by both v2 `build_install_patched_kwin` and (eventually) v3 `internal/kwin`. | Kept; not legacy. |

See [`docs/UBUNTU-ONLY.md`](UBUNTU-ONLY.md) for the explicit pivot
record.

## 5. Compatibility failure points and how this commit addresses each

Roughly in the order the deploy hits them.

### 5.1 Ubuntu release / upgrade

* **Risk**: VM provisioned on 24.04 LTS but the profile targets 25.10
  (KWin 6 + recent NVIDIA driver branch).
* **v2 owns** `maybe_upgrade_ubuntu` / `direct_apt_codename_upgrade`.
* **v3 today**: out of scope. The `base_packages` phase asserts the
  required Ubuntu version (no upgrade attempted). If the profile says
  25.10 and the host is on 24.04, the phase exits with a clear
  `failed_fatal` + a one-liner pointing at the v2 release-upgrade
  path. Release-upgrade moves to Milestone 4 / 5.

### 5.2 apt + dpkg locks and service-start postinst deadlocks

* **Risk** (validated on RTX 5090 deploy):
  `power-profiles-daemon.postinst` runs `deb-systemd-invoke start ...`
  which blocks behind systemd boot jobs that need the dpkg lock the
  postinst is holding. Classic deadlock.
* **v2 fix**: manual `policy-rc.d` exit 101 plus retry loops.
* **v3 fix in this commit**:
  * `internal/apt.InstallPolicyRcD` (already real with tests).
  * `internal/apt.Transaction(ctx, fn)` wraps every apt/dpkg
    operation with:
    * `WaitForLocks(ctx)` polling `/var/lib/dpkg/lock-frontend` +
      `/var/lib/dpkg/lock` + `/var/lib/apt/lists/lock` with a timeout.
    * `InstallPolicyRcD` for the lifetime of the transaction.
    * `dpkg --audit` + `dpkg --configure -a` pre-flight if a previous
      run was interrupted.
    * `apt-get -y -o Dpkg::Options::='--force-confnew' install ...`
      with stdout/stderr captured to `/var/log/clouddeploy/apt.log`.
  * `doctor apt` exposes the lock-holder + dpkg state for an
    operator who runs `clouddeployctl doctor apt` before triggering
    a deploy.

### 5.3 NVIDIA driver family

* **Risk** (validated on RTX 5090 deploy): v2 picked
  `nvidia-driver-580-server` (closed module) on GB202, which the
  driver rejected; v2's validator then refused the operator's manual
  `server-open` fix.
* **v3 fix in Milestone 1.1**: `internal/nvidia.SelectFamily` returns
  `(Family, reason, error)` consulting `Evidence{IsBlackwellConsumer,
  DmesgRequiresOpenKernelModule, Installed*, Available*,
  AvailabilityKnown, PreferOpenFamily, PreferServerFamily}` with the
  hard/soft open-requirement distinction.
* **v3 fix in this commit (Milestone 3)**: `internal/phase.NvidiaDriver`
  calls `SelectFamily` and installs the chosen
  `nvidia-driver-${major}-${suffix}` + matching dkms via
  `apt.Transaction`. Validates via `nvidia-smi -L`. Honours
  `--driver-major` flag and the profile's `nvidia.driver_major`.

### 5.4 Open vs closed kernel module + driver major availability

* **Risk**: A profile pinning `driver_major=580` on a release that
  ships only 535 in apt produces a confusing "Unable to locate
  package" failure.
* **v3 fix**: `internal/nvidia.GatherEvidenceFromHost(opts)` scans
  apt-cache for the specific major + sets `AvailabilityKnown=true`.
  `SelectFamily` returns `ErrNoFamilyAvailable` (with the major in
  the message) when the requested major is not installable.
  `doctor nvidia --driver-major 580` prints exactly what was
  scanned.

### 5.5 CUDA optional/required policy + same-SHA runfile thrash

* **Risk** (validated on RTX 5090 deploy): CUDA runfile downloaded
  five times, identical 4.3 GB blob, identical SHA-256, every
  `--check` failed. 25 minutes wasted.
* **v3 fix in Milestone 1.1**: `internal/cuda.RetryDecider` refuses
  to redownload the same (sha256, size) pair if it already failed
  `--check`.
* **v3 fix in this commit (Milestone 3)**: `internal/phase.Cuda`
  obeys `cuda.Plan(mode)`:
  * `none` → mark phase `skipped` immediately.
  * `optional` → attempt apt install of `cuda-toolkit-${MAJOR}` (only
    when the metapackage is available); on failure, mark
    `failed_nonfatal` and continue.
  * `required` → attempt apt install; on failure, mark
    `failed_fatal`.
* Runfile install path stays a Milestone 4 task: the hdr-4k120
  profile uses `mode=none`, so the runfile is not needed for the
  primary deploy target.

### 5.6 KWin package pinning

* **Risk**: unattended-upgrades replaces the patched KWin with the
  stock package between deploys.
* **v2 owns**: `write_patched_kwin_apt_pin`.
* **v3 today**: Milestone 4. Out of scope for this commit.

### 5.7 Sunshine fork build dependencies

* **Risk**: `cmake` / `ninja` / `libavcodec-dev` / `libpipewire-0.3-dev`
  / `libssl-dev` etc. not installed; fork build fails with confusing
  cmake messages.
* **v2 owns**: `install_sunshine_from_fork_if_requested` (apt-installs
  the full list, then clones + builds).
* **v3 today**: Milestone 4. Out of scope for this commit. The
  `base_packages` phase pre-installs the universal-bottom-tier
  prerequisites (git, build-essential, cmake) but not the
  Sunshine-specific dev packages.

### 5.8 systemd services

* **Risk**: missing `CAP_SYS_NICE` in the Sunshine unit; mis-ordered
  After/Wants between compositor and Sunshine; continuation service
  not enabled after reboot.
* **v3 today**: Milestone 4 for the compositor + Sunshine units.
  This commit lands the **continuation** unit
  (`clouddeploy-continue.service`) so reboot/resume works for the
  three phases that ARE implemented.

### 5.9 EDID / GRUB / reboot flow

* **Risk**: forced EDID needs to be in `/lib/firmware/edid/` AND
  referenced in the kernel cmdline AND survive an initramfs update
  AND the VM needs a reboot for the cmdline to take effect.
* **v3 today**: Milestone 4. Out of scope.
* **What this commit gets right**: any phase that installs a new
  kernel or NVIDIA module sets `state.RebootNeeded = true`, writes
  state atomically, and calls `internal/reboot.Schedule` to enable
  the continuation unit + `systemctl reboot`. `clouddeployctl resume`
  reads state and picks up at the next pending phase.

### 5.10 Tailscale setup

* **Risk**: `tailscale up` needs an auth key; without one the VM
  becomes unreachable from the operator's network and the deploy
  silently runs forever.
* **v3 today**: Milestone 4. Out of scope for this commit.

### 5.11 Moonlight / Sunshine pairing

* **Risk**: PIN-pairing CSRF rejection over Tailscale because
  Sunshine's CSRF allowlist doesn't include the Tailscale IP.
* **v2 fix**: `csrf_allowed_origins` includes localhost + 127.0.0.1
  + Tailscale IP.
* **v3 today**: Milestone 4 (`sunshine_config` phase).

### 5.12 HDR validation

* **DRM side** (validated in Milestone 1.1 v2 docs): `NV_CRTC_REGAMMA_TF`
  = PQ etc.
* **Stream side** (validated in v2 by `clouddeploy-validate-hdr-stream`):
  `Sent HDR mode control packet to Moonlight: enabled=1`.
* **v3 today**: Milestone 4 (`hdr_drm_validation` + `hdr_stream_validation`
  phases).

## 6. What this commit explicitly does NOT do

* Does **not** claim v3 is production-ready.
* Does **not** implement KWin patch / Sunshine build / EDID / systemd
  units / HDR validation.
* Does **not** modify v2 production files.
* Does **not** support Windows VMs (see [UBUNTU-ONLY.md](UBUNTU-ONLY.md)).
* `clouddeployctl apply` exits with a banner saying "Milestone 3
  partial apply complete; KWin/Sunshine phases not implemented yet."
  so an operator running `apply` cannot mistake a partial success
  for a full deploy.

## 7. Acceptance criteria for the next VM test

After this commit is on the VM and `bootstrap.sh` is run with
`PROFILE=hdr-4k120`:

1. `base_packages` phase installs git / curl / build-essential / etc.
   without dpkg deadlock and writes `state.phases.base_packages.status
   = done`.
2. `nvidia_driver` phase picks the right family (server-open for
   RTX 5090; server-or-installed-working for RTX 4090; L4 → server),
   installs it via apt.Transaction, runs `nvidia-smi -L`, and either:
   * marks `done` if `nvidia-smi` worked, OR
   * marks `state.RebootNeeded = true` and triggers reboot + resume.
3. `cuda` phase respects `mode = none` and marks `skipped` immediately.
4. `apply` prints the partial-apply banner and exits cleanly.
5. `clouddeployctl state show` reports the three phases plus the
   profile and any RebootNeeded marker.
6. `clouddeployctl doctor apt` prints no lock holders, no pending
   configure, policy-rc.d absent (we cleaned up).
7. `clouddeployctl doctor nvidia --driver-major 580` agrees with
   `nvidia-smi` and `dpkg -l | grep nvidia-driver`.
8. No KWin / Sunshine work was attempted.

Once those criteria pass on the live VM, Milestone 4 (KWin / Sunshine
/ EDID / systemd / HDR validation) becomes the next chunk.
