# v3 deployment readiness audit

Refreshed after the v2 → v3 parity audit
([V2-V3-PARITY.md](V2-V3-PARITY.md)). The goal remains a no-contact
deploy on an Ubuntu cloud VM with an NVIDIA GPU; this doc keeps the
gap between "v3 today" and "no-contact full deploy" honest.

## TL;DR — can v3 do a no-contact deploy today?

**No.** v3 is NOT equivalent to v2. The full P0 / P1 / P2 audit is in
[V2-V3-PARITY.md](V2-V3-PARITY.md). v3 today reaches:

* **(this commit) Ubuntu release upgrade**: 24.04 → 25.10 via direct
  apt codename rewrite, behind `profile.deploy.auto_upgrade_ubuntu`.
* `base_packages` installed via `apt.Transaction` (deadlock-proof).
* NVIDIA driver family selected + installed via `nvidia.SelectFamily`,
  with dirty-driver cleanup.
* CUDA mode policy honoured (none / optional / required).
* EDID/GRUB cmdline generation (opt-in via `display.forced_connector`).
* Reboot needed → continuation systemd unit installed; auto-reboot
  via `profile.deploy.auto_reboot`.
* `collect-logs` packages /var/log/clouddeploy + state + systemctl +
  journal + dmesg + apt logs into a tarball.

It still has beta gaps:

* Microphone input has not been validated.
* Cloud-init lifecycle waiting is limited; dpkg/apt locks are handled, but
  images that are still actively provisioning may need a retry.
* Some cloud hosts expose broken GPU PCIe/MSI topology; `nvidia-smi` can work
  while NVENC work submission hangs.
* The validated end-to-end path is narrow: Vast.ai RTX 4090-class Ada GPU,
  4K/120, HDR, AV1-capable Moonlight client.

The current recommended v3 profile is:

```bash
PROFILE=hdr-4k120-cuda-auto-unattended-optional
```

## 1. Current stubs

`clouddeployctl` subcommands grouped by readiness as of this commit:

| Subcommand | Status |
| --- | --- |
| `doctor nvidia` | **Real**. Family selector + classification + suggested next action. |
| `doctor cuda` | **Real**. Reads profile, prints mode + nvcc presence + Sunshine CUDA-module verdict + runfile retry policy. |
| `doctor apt` | **Real**. policy-rc.d state + lock holders + `dpkg --audit`. |
| `doctor system` | **Real + gate**. OS release / kernel / state file / phase summary. Refuses unsupported Ubuntu versions unless `--allow-unsupported`. |
| `doctor kwin` | **Real (Milestone 4A).** |
| `doctor sunshine` | **Real (Milestone 5).** Reports fork/build binary, runtime assets, caps, build HEAD, and Sunshine service hints. |
| `validate hdr-stream` | **Pointer (Milestone 5).** Directs operators to `phase stream-validate`; deep Moonlight HDR packet validation still depends on an active stream attempt. |
| `state show` | **Real**. |
| `state reset --phase X` | **Real**. |
| `apply` | **Milestone 5 attempt.** Runs `apt-health → ubuntu-upgrade → base-packages → nvidia-driver → cuda → edid → headless-user → desktop-packages → desktop-runtime → kwin-patch → kwin-session → drm-display-validate → sunshine-build → sunshine-config → tailscale → pipewire-audio → streaming-services → stream-validate → optional-apps`. Reboots via continuation service when needed; `--unattended` / `--auto-reboot` can drive the run with no manual reboot/resume steps. |
| `phase ubuntu-upgrade` | **Real (this commit).** v3 port of v2's `maybe_upgrade_ubuntu` + `direct_apt_codename_upgrade`. 24.04 → 25.10 path works; anti-reboot-loop guard via `state.Details["pre_version"]`. do-release-upgrade path is stubbed with a clear error (set `deploy.direct_apt_codename_upgrade=force` to skip it). |
| `resume` | **Real**. Reads state, clears RebootNeeded, rewrites stale `running` phases to `pending` after acquiring the process lock, replays implemented phases, and disables the continuation service when no further reboot is queued. |
| `phase base-packages` | **Real**. |
| `phase nvidia-driver` | **Real + dirty-driver cleanup planner.** |
| `phase cuda` | **Real (policy-driven, cross-distro repo ladder, source-aware layout).** Honors `cuda.method` + `cuda.selection_policy` + `cuda.expected_major` / `min_major` / `prefer_major` / `prefer_newest` + `cuda.allow_cross_distro_cuda_repo` + `cuda.cuda_repo_distro_candidates` + `cuda.allow_ubuntu_archive_fallback` + `cuda.compile_smoke_test`. Apt path walks the configured cross-distro CUDA repo ladder (`auto-host` expands to the host slug, e.g. `ubuntu2510` on 25.10; subsequent entries like `ubuntu2404` are tried only when `allow_cross_distro_cuda_repo=true`). The first reachable slug wins; v3 then bootstraps `cuda-keyring_1.1-1` for THAT slug — this is how 25.10 hosts get CUDA 13 via NVIDIA's `ubuntu2404` repo. Only when every NVIDIA candidate is unreachable AND `allow_ubuntu_archive_fallback=true` does the phase fall through to Ubuntu's archive `nvidia-cuda-toolkit`. Source-aware layout verification: NVIDIA-repo / runfile installs must produce `/usr/local/cuda/{bin/nvcc,include,lib64}`; Ubuntu archive installs validated against `/usr/bin/nvcc` + (`/usr/include/cuda_runtime.h` OR `/usr/lib/cuda/include`) + (`/usr/lib/x86_64-linux-gnu/libcudart.so*` OR `/usr/lib/cuda/lib64`). CUDA probes are bounded (`nvcc --version`, `nvidia-smi`, smoke compile, smoke run); required profiles fail clearly, while optional/auto profiles mark CUDA degraded and continue to EDID/KWin/Sunshine. State.Details: `host_ubuntu_version`, `host_codename`, `cuda_repo_distro_tried`, `selected_repo_distro`, `cross_distro_cuda_repo`, `archive_fallback`, `fallback_reason`, `source`, `selected_package`, `selected_major`, `driver_preferred_major`, `required_major`, `min_major`, `layout_kind` / `layout_nvcc` / `layout_headers` / `layout_libs`, `compile_smoke_test*`, `runtime_smoke_test*`, `cuda_status`, `cuda_degraded_reason`, smoke timings/tails. |
| `phase headless-user` | **Real (Milestone 4A).** Creates / verifies the deploy account (default `cloudgamer`), ensures supplementary groups (`video`, `render`, `input`, `audio`, `systemd-journal` by default), runs `loginctl enable-linger` so `/run/user/<uid>` persists, and best-effort chowns the home dir. State.Details: `user`, `uid`, `gid`, `home`, `shell`, `groups`, `linger_enabled`, `created_or_existing`. |
| `phase desktop-packages` | **Real (Milestone 4A).** apt-installs the minimum Wayland/KDE/PipeWire stack via apt.Transaction: `kwin-wayland`, `plasma-workspace`, `plasma-desktop`, `kscreen`, `dbus-user-session`, `xdg-desktop-portal[-kde]`, `pipewire[-pulse]`, `wireplumber`, `qt6-wayland`, `wayland-utils`, `vulkan-tools`, `mesa-utils`, `drm-info`. |
| `phase desktop-runtime` | **Real (Milestone 4A).** Verifies preconditions: `/dev/dri/card*` + `/dev/dri/renderD*` exist; `/sys/module/nvidia_drm/parameters/modeset == "Y"`; `/proc/cmdline` contains `nvidia-drm.modeset=1`, `nvidia-drm.fbdev=1`, `video=<connector>:e`, and `drm.edid_firmware=<connector>:edid/<file>`; the headless user is in BOTH `video` and `render`. Any failure -> fatal before kwin-session. State.Details: `dri_cards`, `render_nodes`, `nvidia_drm_modeset`, `cmdline`, `cmdline_missing`, `cmdline_ok`, `user_groups_now`, `user_device_access_ok`. |
| `phase kwin-patch` | **Real (Milestone 4B).** If `kwin.patched_hdr=false`, marks skipped. If enabled, validates `patches/kwin-clouddeploy-nvidia-private-hdr.patch` against the active Ubuntu `kwin` source, builds patched KWin binary packages with `dpkg-buildpackage`, installs/holds them, and writes `/var/lib/clouddeploy/kwin-patch.json` with patch/source/install metadata. Fails closed for HDR profiles unless `allow_packaged_fallback=true` and `require_patch=false`. |
| `phase kwin-session` | **Real (Milestone 4A/4B).** Renders + installs the selected real-VT KWin/Plasma service, `systemctl daemon-reload + enable + start`, waits for `/run/user/<uid>/wayland-0` to appear and remain stable, requires `systemctl is-active` + MainPID, and scans recent journal lines for KWin DRM/logind fatal signatures. When `kwin.patched_hdr=true` and `kwin_patch` is done, injects `KWIN_CLOUDDEPLOY_NVIDIA_PRIVATE_HDR=1` and `KWIN_CLOUDDEPLOY_NVIDIA_PRIVATE_HDR_MODESET_PLANE_PROPS=1` into the service environment. |
| `phase drm-display-validate` | **Real (Milestone 5).** Runs `kscreen-doctor -o` as the headless user with the right Wayland/XDG/D-Bus env. Parses KScreen 6.4.x mode syntax, asserts the forced connector exists + is enabled + advertises/selects the configured mode (`<W>x<H>@<refresh>`). For HDR profiles, hard-fails unless KScreen reports `HDR: enabled` and `Wide Color Gamut: enabled`. State.Details: `connector`, `enabled`, `selected_mode`, `expected_mode`, `modes`, `hdr_enabled`, `wcg_enabled`. |
| `doctor kwin` | **Real (Milestone 4A).** Reports selected compositor backend, service name/path, systemd state, `/run/user/<uid>` and socket state, loginctl/user groups, DRI nodes, journal tail, and recovery commands. |
| `doctor kwin-patch` | **Real (Milestone 4B).** Reports patched-HDR profile settings, patch path/hash, marker metadata, installed packages, held packages, fallback reason, and the next command to rerun validation/build. |
| `phase apt-health` | **Real.** Runs FIRST in the apply chain. (1) preserves emergency sudo by writing `/etc/sudoers.d/90-clouddeploy-<SUDO_USER>` (validated with `visudo -cf`, installed via temp+atomic rename, mode 0440) so a clobbered `/etc/sudoers` mid-upgrade does not lock the operator out; (2) runs `dpkg --audit`, and on the `/var/lib/dpkg/updates/<N>` parse-error signature quarantines the journal files into `/var/lib/clouddeploy/backups/dpkg-updates-<ts>/` before running `dpkg --configure -a` + `apt-get -f install`; (3) detects mid-upgrade state (host VERSION_ID disagrees with the codename in `ubuntu.sources`) and records `mid_upgrade: true` + a `mid_upgrade_reason` in state.Details. Any unrecoverable repair failure fails the phase **before** ubuntu-upgrade touches sources. |
| `phase ubuntu-upgrade` | **Real, resume-friendly, target-resolver-aware.** Records a `stage` marker (`started → third-party-sources-disabled → sources-rewritten → dist-upgrade-started → dist-upgrade-complete → reboot-required`). Anti-loop guard fires only when stage indicates the previous run completed dist-upgrade AND `current==pre_version`. `rewriteAptCodename` is idempotent. Before mutating sources the phase stops `apt-daily.timer`, `apt-daily-upgrade.timer`, `unattended-upgrades.service` and waits up to 3 minutes for dpkg/apt locks. **New (this commit):** OS target resolver. When `deploy.ubuntu_candidates` is non-empty, the phase walks the candidate list newest-first via `ubuntu.ResolveOSTarget(...)` and records `ubuntu_selection_policy`, `ubuntu_candidates_configured`, `ubuntu_candidates_tried`, `selected_ubuntu_version`, `selected_ubuntu_codename`, `rejected_ubuntu_candidates`, `prefer_lts` in state.Details. The resolver gates on known-codename + v3-supported list + non-LTS knob + an optional `OSPackageProbeFn`. Policies: `latest-compatible` (default), `exact`, `min-version`, `any`. |
| `doctor lock` | **Real (this commit).** Read-only probe of `/var/lib/clouddeploy/state.lock`: reports the holder PID, whether it's still alive, and recovery instructions for stale locks. Use after a "lock is held by another clouddeployctl process" error. |
| `phase edid` | **Real (opt-in).** Invokes `helpers/write-edids.py`, writes `/lib/firmware/edid/<name>.bin`, drops a `/etc/default/grub.d/99-clouddeploy.cfg`, runs `update-initramfs -u` + `update-grub`, sets `RebootNeeded`. Only runs when `display.forced_connector` is set in the profile. |
| `phase kwin-patch` | **Real.** Patch validation + source package build/install + marker/idempotency. |
| `phase sunshine-build` | **Real (Milestone 5).** Installs build deps, clones/checks out the pinned Sunshine fork commit, builds the `sunshine` target, installs `/usr/local/bin/sunshine-clouddeploy`, sets capabilities, and installs runtime assets under `/usr/local/assets`. |
| `phase sunshine-config` | **Real (Milestone 5).** Generates KMS/NVENC `sunshine.conf` without known-invalid keys and with local/Tailscale CSRF allowlist support. |
| `phase tailscale` | **Real (Milestone 5, optional).** Installs/starts Tailscale and runs `tailscale up --authkey ... --ssh` only when an auth key is present. |
| `phase pipewire-audio` | **Real (Milestone 5).** Installs PipeWire/WirePlumber/Pulse tools and validates user-session audio visibility. |
| `phase streaming-services` | **Real (Milestone 5).** Installs `sunshine-headless.service`, reset helper, and watchdog timer; uses direct `kwin-realvt.service` as compositor dependency. |
| `phase stream-validate` | **Real (Milestone 5).** Requires Sunshine active and `/serverinfo` reachable, records listener/log markers, and enters `pending_moonlight_connect` until a Moonlight stream attempt produces KMS/NVENC markers. |
| `phase optional-apps` | **Real (Milestone 5, nonfatal).** Optional profile-gated app installer that runs last. |
| `collect-logs` | **Real**. Bundles `/var/log/clouddeploy/`, `state.json`, `systemctl status` of relevant units, `journalctl` snippets, `dmesg` NVIDIA lines, `dpkg.log` / `apt/history.log` tails into `/tmp/clouddeploy-logs-<ts>.tar.gz`. Secret redaction applies to env. |

`internal/` packages grouped by readiness:

| Package | Status |
| --- | --- |
| `internal/runner` | **Real**. Exec + redaction + log tee + DryRun. |
| `internal/apt` | **Real**. Transaction + policy-rc.d + lock wait + dpkg audit. |
| `internal/state` | **Real**. Atomic save + lock file + RebootNeeded/ResumeTarget + LastError + Reset. |
| `internal/nvidia` | **Real**. Selector + classification + dirty-state cleanup planner + post-install validators. |
| `internal/cuda` | **Real**. Policy + RetryDecider + package-candidate discovery. |
| `internal/config` | **Real**. Profile + GPU + validation. |
| `internal/phase` | **Real for the Milestone 5 apply chain through `optional_apps`.** |
| `internal/reboot` | **Real (minimal).** Continuation systemd unit + Install/Schedule/Disable. |
| `internal/ubuntu` | **Real.** `/etc/os-release` reader + supported-version gate. |
| `internal/edid` | **Real (helper-script wrapper).** Calls `helpers/write-edids.py` + drops EDID + edits grub. |
| `internal/gpu` | Stub. Milestone 4. |
| `internal/kwin` | **Real marker/hash helper.** The build/install orchestration lives in `internal/phase/kwin_patch.go`. |
| `internal/sunshine` | Stub. Milestone 4. |
| `internal/systemd` | Stub. Milestone 4 (runtime services beyond the continuation unit). |
| `internal/validate` | Stub. Milestone 4. |

## 2. Fresh-VM phases now in the Milestone 5 attempt

| Phase | Owner | Notes |
| --- | --- | --- |
| `ubuntu_upgrade` | **v3 real (this commit)** | Direct apt codename rewrite 24.04 → 25.10. Honors `profile.deploy.{auto_upgrade_ubuntu,accept_non_lts,direct_apt_codename_upgrade}`. Reboot-aware. |
| `base_packages` | **v3 real** | Idempotent. |
| `nvidia_driver` | **v3 real** | Reboot-aware. |
| `cuda` | **v3 real** | mode=none default. |
| `edid` | **v3 real (opt-in)** | Reboot-aware. |
| `kwin_patch` | **v3 real** | Validates patch, builds/install patched KWin source packages, holds installed packages, writes marker. |
| `sunshine_build` | **v3 real (Milestone 5)** | Pin commit `8f02b1ce`, build fork, install `/usr/local/bin/sunshine-clouddeploy`, set caps, install assets. |
| `sunshine_config` | **v3 real (Milestone 5)** | KMS/NVENC config without known-invalid keys + CSRF allowlist. |
| `tailscale` | **v3 real (Milestone 5, optional)** | `tailscale up` only when `TAILSCALE_AUTHKEY` is present. |
| `pipewire_audio` | **v3 real (Milestone 5)** | PipeWire/WirePlumber install + user-session audio visibility check. |
| `streaming_services` | **v3 real (Milestone 5)** | direct `kwin-realvt.service` + `sunshine-headless.service` + reset/watchdog helpers. |
| `stream_validate` | **v3 real (Milestone 5)** | Sunshine active + `/serverinfo` reachable; KMS/NVENC journal markers become done after a Moonlight stream attempt. |
| `optional_apps` | **v3 real (Milestone 5, nonfatal)** | Profile-gated, last phase. |
| `moonlight_pairing` | Remaining manual/helper gap | No state-file pairing injection; PIN/API helper still separate from the apply chain. |

## 3. Remaining blockers to no-contact deploy

In rough order of how the deploy hits them:

1. **Mic input is unvalidated.** PipeWire creates the experimental
   `clouddeploy-mic` path, but it has not been proved with a real Moonlight
   headset/microphone session.
2. **Moonlight pairing is still manual.** Sunshine is reachable, but there is
   no state-file pairing injection or first-class `clouddeployctl pair` flow.
3. **Cloud host topology can still fail below the guest.** A VM can pass
   `nvidia-smi` but hang on NVENC if the provider exposes broken PCIe/MSI
   interrupt topology.
4. **HDR validation still depends on a real client attempt.** The
   `stream_validate` phase records Sunshine/server markers, but full proof
   still requires a Moonlight session.
5. **Fresh-host coverage is narrow.** The known-good path is the tested
   Vast.ai RTX 4090/Ada VM. More hosts and client devices need coverage.

## 4. Exit codes

`clouddeployctl apply` and `resume` now use distinct exit codes so
`bootstrap.sh` and external CI / automation can branch correctly:

| Code | Meaning |
| --- | --- |
| `0` | All phases in the selected profile completed or intentionally skipped; no reboot pending. |
| `1` | Real failure. Apply aborted; state records `failed_fatal` / `LastError` for the failing phase. |
| `2` | Reboot scheduled or required. State has `RebootNeeded=true` and `ResumeTarget=<phase>`. Continuation service is enabled. With `--auto-reboot` / `profile.deploy.auto_reboot: true`, `systemctl reboot` has been invoked. |
| `64` | Refused to run: unsupported Ubuntu version (use `--allow-unsupported` to override). |

`bootstrap.sh` reads the exit code and prints a one-line summary so
the operator sees clearly which case applied.

## 5. Compatibility failure points — status

| Failure mode | Status |
| --- | --- |
| apt/dpkg deadlock (postinst service-start) | **Solved** — `apt.Transaction` policy-rc.d guard. |
| NVIDIA family selection (RTX 5090 needs server-open) | **Solved** — `nvidia.SelectFamily` Milestone 1.1. |
| Driver-major availability | **Solved** — `nvidia.GatherEvidenceFromHost(opts)` scoped scan. |
| Open vs closed kernel module | **Solved** — hard/soft requirement split. |
| Multiple installed driver families | **Solved this commit** — `nvidia.PlanCleanup` + `--repair-driver-family` gate. |
| Repeated bad-CUDA-runfile retries | **Solved** — `cuda.RetryDecider` wired into the v3 runfile install path; refuses to retry same (sha256, size) that already failed `--check`. As of 2026-05-18, the CUDA 13.0.2 toolkit-only runfile on the production NVIDIA mirror is reproducibly corrupt (`--check` rejects its own embedded MD5), so `hdr-4k120-cuda` ships with `cuda.method=apt` and no `runfile_url`. A `--check` failure now logs `RunfileCheckCorruptHint` and includes it in the fatal error / optional-skip details. |
| No NVIDIA CUDA apt repo for Ubuntu 25.10 | **Worked around** (2026-05-18). NVIDIA does not publish `https://developer.download.nvidia.com/compute/cuda/repos/ubuntu2510/...` and Ubuntu 25.10's archive ships `nvidia-cuda-toolkit` at CUDA 12.4 (intentionally filtered out by `cuda.CandidateLadder` when `RequiredMajor=13`). The combined result: `hdr-4k120-cuda` (which targets 25.10) is currently unsatisfiable and fails fatal with a clear "no CUDA apt repo available" message. The new `config/profiles/hdr-4k120-cuda-ubuntu2404.yaml` pins to Ubuntu 24.04 LTS (`auto_upgrade_ubuntu=false`) where NVIDIA's `ubuntu2404` CUDA apt repo is known to exist, giving v3 a working CUDA-required test target today. |
| CUDA apt package name guessed wrong | **Solved this commit** — `cuda.DiscoverCandidate` tries `cuda-toolkit-13-N`, `cuda-toolkit`, `nvidia-cuda-toolkit` (filtered out when required-major=13), profile override. |
| CUDA install accidentally clobbers driver | **Solved this commit** — apt path refuses `cuda-drivers` / `cuda-drivers-*`; runfile path uses `--toolkit --override` (never `--driver`). |
| Reboot/resume ambiguity | **Solved this commit** — `internal/reboot` installs continuation systemd unit; `apply` returns exit 2 when reboot is pending; `--auto-reboot` triggers it; `resume` clears state + disables the unit. |
| Unsupported Ubuntu version | **Mitigated this commit** — `internal/ubuntu` gate; `apply` refuses unless `--allow-unsupported`. |
| Wrong installed family + broken nvidia-smi | **Mitigated this commit** — `nvidia.PlanCleanup` proposes purge of conflicting closed packages when open is required and the closed install is broken. Tests cover the planner. |
| dmesg "requires NVIDIA open kernel modules" | **Solved** — detected by `nvidia.GatherEvidenceFromHost`; flagged in `doctor nvidia`; used by `SelectFamily`. |
| KWin pinning | M4. |
| Sunshine build dependencies + dual-install + setcap | M4. |
| systemd runtime units | M4 (continuation unit is real). |
| EDID / GRUB / initramfs / reboot for cmdline pickup | **Real (opt-in phase) this commit.** |
| Tailscale setup | M4. |
| Moonlight pairing CSRF allowlist | M4. |
| HDR DRM validation | M4. |
| HDR stream control-packet validation | M4. |

## 6. Docs/comments-vs-behaviour audit

Findings from sweeping the codebase for misleading text:

| Where | Before | After (this commit) |
| --- | --- | --- |
| `cmd/clouddeployctl/main.go` apply help text | "schedules a reboot via clouddeploy-continue.service" but body printed "operator should run sudo reboot" | Rewritten to match new behaviour: continuation service is installed; with `--auto-reboot`, `systemctl reboot` is invoked; without it, exit 2 is returned and the operator runs `sudo reboot`. |
| `docs/V3-ROADMAP.md` Milestone 3 | "Implement runner, apt transaction wrapper, doctor improvements, and phase nvidia/cuda." Stops there. | Section updated to reflect Milestone 3 partial **shipped** + Milestone 3.5 (this commit) + the Milestone 4 critical path slices the user has agreed to. |
| README v3 line | "Milestone 3 partial — Ubuntu-only" | Updated to mention auto-reboot and EDID/GRUB phase availability. |

## 7. Acceptance criteria for the next VM test

Seven test profiles cover the supported paths:

| Profile | Ubuntu target | CUDA selection | Notes |
| --- | --- | --- | --- |
| `hdr-4k120` | 25.10 (pinned) | `mode=none` | Default. Tests ubuntu-upgrade → driver → edid; cuda is skipped. |
| `hdr-4k120-auto` (this commit) | resolver `[26.04, 25.10, 24.04]` | `mode=none` | OS-resolver variant of `hdr-4k120`. The resolver rejects 26.04 today (not in v3 supported list) and selects 25.10; when 26.04 is validated this profile auto-picks it with no edit. |
| `hdr-4k120-cuda` | 25.10 (pinned) | strict: `exact-major=13`, cross-distro CUDA ladder `[auto-host, ubuntu2404]`, no archive fallback | Strict CUDA-13. On 25.10 the apt path probes `ubuntu2510` (missing today) then `ubuntu2404` (reachable) and installs `cuda-toolkit-13-N`. |
| `hdr-4k120-cuda-ubuntu2404` | 24.04 (pinned) | strict: `exact-major=13`, native-only CUDA, no archive fallback | LTS strict variant. |
| `hdr-4k120-cuda-compatible` | 25.10 (pinned) | broad: `latest-compatible`, `prefer_major=13`, `min_major=12`, cross-distro CUDA ladder, archive fallback ok | Broad-compat. Cross-distro CUDA 13 beats archive 12.4. |
| `hdr-4k120-cuda-auto` (this commit) | resolver `[26.04, 25.10, 24.04]` | broad optional CUDA attempt: `latest-compatible`, `prefer_major=13`, cross-distro CUDA ladder, archive fallback ok | Composes OS resolver + CUDA cross-distro resolver. The "deploy this, figure out OS + CUDA" profile; CUDA smoke degradation is nonfatal so the NVENC streaming stack can still validate. |
| `hdr-4k120-cuda-native` | 25.10 (pinned) | strict: `exact-major=13`, **no cross-distro CUDA**, no archive | Diagnostic. |

`hdr-4k120` ships with
`deploy.auto_upgrade_ubuntu: true` + `deploy.accept_non_lts: true` +
`deploy.direct_apt_codename_upgrade: auto` + `deploy.auto_reboot: false`,
so the next VM test of the streaming-side path can start from
**either**:

* a fresh Ubuntu 25.10 cloud image (no release-upgrade hop), or
* a fresh Ubuntu 24.04 cloud image — the v3 ubuntu-upgrade phase will
  reconcile to 25.10 via direct apt-source codename rewrite (the same
  path v2 uses, because `do-release-upgrade -d` refuses 24.04 → 25.10).

```bash
# Streaming-side path (no CUDA):
sudo CLOUDDEPLOY_RUN=1 PROFILE=hdr-4k120 bash bootstrap.sh

# CUDA-required path on Ubuntu 24.04 LTS:
sudo CLOUDDEPLOY_RUN=1 PROFILE=hdr-4k120-cuda-ubuntu2404 bash bootstrap.sh
```

Expected behaviour:

1. `doctor system` confirms a v3-supported Ubuntu version (25.10 or
   24.04). If the host is on 24.04 and the profile targets 25.10,
   apply defers the exact-match gate to the ubuntu-upgrade phase.
2. **If the host is on 24.04:** `phase ubuntu-upgrade` disables
   third-party NVIDIA/CUDA apt sources, purges any stale NVIDIA/CUDA
   packages, rewrites codenames in `/etc/apt/sources.list.d/ubuntu.sources`
   + `/etc/apt/sources.list` from `noble` → `questing`, runs
   `apt-get update` + `apt-get -y dist-upgrade`, sets RebootNeeded.
   **Apply exits 2 here (auto_reboot=false default); the operator
   reboots manually and SSHes back in to `sudo clouddeployctl resume`.**
3. After the upgrade reboot, the host reports VERSION_ID=25.10; the
   ubuntu-upgrade anti-loop guard confirms forward progress and marks
   the phase done. Resume continues with the rest.
4. `phase base-packages` installs the minimum tooling without dpkg
   deadlock.
5. `phase nvidia-driver` selects the right family (server-open for
   RTX 5090; either for RTX 4090; server for L4), installs it, runs
   `nvidia-smi` smoke test. If the kernel module did not load
   without a reboot, sets RebootNeeded → apply exits 2 again.
6. After this second reboot, resume runs; `nvidia-smi` works.
7. `phase cuda` skips for `cuda.mode=none`.
8. `phase edid` generates the 4K120-HDR EDID, writes GRUB cmdline,
   updates initramfs + grub, sets RebootNeeded → apply exits 2 a
   third time. After reboot, resume confirms EDID is reflected on
   `DP-1` and disables the continuation unit.
9. Milestone 5 continues into patched KWin real-VT validation,
   Sunshine fork build/config, optional Tailscale, PipeWire, runtime
   services, and stream validation. `stream_validate` may report
   `pending_moonlight_connect` until a Moonlight client attempt creates
   the KMS/NVENC journal markers.

The next paid fresh-VM run is expected to prove whether this Milestone 5
pipeline can reach Moonlight overlay `AV1 10-bit HDR` without manual
reboot/resume steps. Until that run passes, v2 remains the production
fallback.

If you need to send the run somewhere for inspection:

```bash
sudo clouddeployctl collect-logs --output /tmp/clouddeploy.tar.gz
```

bundles every relevant log, state file, and systemctl status into a
single tarball.
