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

It does **not** yet:

* Create the headless user / sudo / loginctl-linger setup.
* Block on cloud-init's apt lock (we wait on dpkg locks but not
  on cloud-init's lifecycle).
* Repair the snapd / libblockdev / corrupt-`/var/lib/dpkg/updates`
  failure modes the v2 path handles.
* Install KDE / Plasma / KWin.
* Patch / build / install the patched-KWin NVIDIA private HDR path.
* Build the Sunshine fork at `464bccf1`.
* Generate the KWin / Plasma / Sunshine systemd units.
* Wire Tailscale.
* Install the PipeWire virtual sink.
* Run the streaming or HDR validators.

Until those land, the only command that reaches Moonlight
`AV1 10-bit HDR` end-to-end remains the v2 entrypoint:

```bash
sudo ENABLE_HDR=1 bash ./CloudDeploy-wayland.sh
```

## 1. Current stubs

`clouddeployctl` subcommands grouped by readiness as of this commit:

| Subcommand | Status |
| --- | --- |
| `doctor nvidia` | **Real**. Family selector + classification + suggested next action. |
| `doctor cuda` | **Real**. Reads profile, prints mode + nvcc presence + Sunshine CUDA-module verdict + runfile retry policy. |
| `doctor apt` | **Real**. policy-rc.d state + lock holders + `dpkg --audit`. |
| `doctor system` | **Real + gate**. OS release / kernel / state file / phase summary. Refuses unsupported Ubuntu versions unless `--allow-unsupported`. |
| `doctor kwin` | Stub. Milestone 4. |
| `doctor sunshine` | Stub. Milestone 4. |
| `validate hdr-stream` | Stub. Milestone 4. |
| `state show` | **Real**. |
| `state reset --phase X` | **Real**. |
| `apply` | **Partial (Milestone 4A landed).** Runs `apt-health → ubuntu-upgrade → base-packages → nvidia-driver → cuda → edid (opt-in) → headless-user → desktop-packages → desktop-runtime → kwin-session → drm-display-validate`. Exits 10 with banner; or 2 when reboot is pending (and triggers `systemctl reboot` when `--auto-reboot` is set / profile has `auto_reboot: true` (defaults to manual reboot)). Refuses unsupported Ubuntu versions unless `--allow-unsupported`, but if the host is on a v3-supported release AND `profile.deploy.auto_upgrade_ubuntu=true` AND the profile targets a different supported version, defers the exact-match gate to the ubuntu-upgrade phase. |
| `phase ubuntu-upgrade` | **Real (this commit).** v3 port of v2's `maybe_upgrade_ubuntu` + `direct_apt_codename_upgrade`. 24.04 → 25.10 path works; anti-reboot-loop guard via `state.Details["pre_version"]`. do-release-upgrade path is stubbed with a clear error (set `deploy.direct_apt_codename_upgrade=force` to skip it). |
| `resume` | **Real**. Reads state, clears RebootNeeded, rewrites stale `running` phases to `pending` after acquiring the process lock, replays implemented phases, and disables the continuation service when no further reboot is queued. |
| `phase base-packages` | **Real**. |
| `phase nvidia-driver` | **Real + dirty-driver cleanup planner.** |
| `phase cuda` | **Real (policy-driven, cross-distro repo ladder, source-aware layout).** Honors `cuda.method` + `cuda.selection_policy` + `cuda.expected_major` / `min_major` / `prefer_major` / `prefer_newest` + `cuda.allow_cross_distro_cuda_repo` + `cuda.cuda_repo_distro_candidates` + `cuda.allow_ubuntu_archive_fallback` + `cuda.compile_smoke_test`. Apt path walks the configured cross-distro CUDA repo ladder (`auto-host` expands to the host slug, e.g. `ubuntu2510` on 25.10; subsequent entries like `ubuntu2404` are tried only when `allow_cross_distro_cuda_repo=true`). The first reachable slug wins; v3 then bootstraps `cuda-keyring_1.1-1` for THAT slug — this is how 25.10 hosts get CUDA 13 via NVIDIA's `ubuntu2404` repo. Only when every NVIDIA candidate is unreachable AND `allow_ubuntu_archive_fallback=true` does the phase fall through to Ubuntu's archive `nvidia-cuda-toolkit`. Source-aware layout verification: NVIDIA-repo / runfile installs must produce `/usr/local/cuda/{bin/nvcc,include,lib64}`; Ubuntu archive installs validated against `/usr/bin/nvcc` + (`/usr/include/cuda_runtime.h` OR `/usr/lib/cuda/include`) + (`/usr/lib/x86_64-linux-gnu/libcudart.so*` OR `/usr/lib/cuda/lib64`). State.Details: `host_ubuntu_version`, `host_codename`, `cuda_repo_distro_tried`, `selected_repo_distro`, `cross_distro_cuda_repo`, `archive_fallback`, `fallback_reason`, `source`, `selected_package`, `selected_major`, `driver_preferred_major`, `required_major`, `min_major`, `layout_kind` / `layout_nvcc` / `layout_headers` / `layout_libs`, `compile_smoke_test*`, `runtime_smoke_test*`. |
| `phase headless-user` | **Real (Milestone 4A).** Creates / verifies the deploy account (default `cloudgamer`), ensures supplementary groups (`video`, `render`, `input`, `audio`, `systemd-journal` by default), runs `loginctl enable-linger` so `/run/user/<uid>` persists, and best-effort chowns the home dir. State.Details: `user`, `uid`, `gid`, `home`, `shell`, `groups`, `linger_enabled`, `created_or_existing`. |
| `phase desktop-packages` | **Real (Milestone 4A).** apt-installs the minimum Wayland/KDE/PipeWire stack via apt.Transaction: `kwin-wayland`, `plasma-workspace`, `plasma-desktop`, `kscreen`, `dbus-user-session`, `xdg-desktop-portal[-kde]`, `pipewire[-pulse]`, `wireplumber`, `qt6-wayland`, `wayland-utils`, `vulkan-tools`, `mesa-utils`, `drm-info`. |
| `phase desktop-runtime` | **Real (Milestone 4A).** Verifies preconditions: `/dev/dri/card*` + `/dev/dri/renderD*` exist; `/sys/module/nvidia_drm/parameters/modeset == "Y"`; `/proc/cmdline` contains `nvidia-drm.modeset=1`, `nvidia-drm.fbdev=1`, `video=<connector>:e`, and `drm.edid_firmware=<connector>:edid/<file>`; the headless user is in BOTH `video` and `render`. Any failure -> fatal before kwin-session. State.Details: `dri_cards`, `render_nodes`, `nvidia_drm_modeset`, `cmdline`, `cmdline_missing`, `cmdline_ok`, `user_groups_now`, `user_device_access_ok`. |
| `phase kwin-session` | **Real (Milestone 4A).** Renders + installs `/etc/systemd/system/clouddeploy-kwin-wayland.service` (template under `phase.RenderUnitText`), `systemctl daemon-reload + enable + start`, then polls `/run/user/<uid>/wayland-0` for up to 30s. The unit runs `dbus-run-session -- kwin_wayland --drm --no-lockscreen` as the headless user with `XDG_RUNTIME_DIR=/run/user/<uid>` + `WAYLAND_DISPLAY=wayland-0` + `GBM_BACKEND=nvidia-drm` + `KWIN_DRM_USE_EGL_STREAMS=0`. State.Details: `user`, `uid`, `unit_path`, `systemctl_enabled`, `systemctl_started`, `wayland_socket_path`, `wayland_socket_ok`. |
| `phase drm-display-validate` | **Real (Milestone 4A).** Runs `kscreen-doctor -o` as the headless user with the right Wayland/XDG/D-Bus env. Parses the output, asserts the profile's forced connector exists + is enabled + advertises the configured mode (`<W>x<H>@<refresh>`). Fails fatal otherwise. HDR-side validation is recorded if visible but does NOT hard-fail (Milestone 4B). State.Details: `connector`, `enabled`, `selected_mode`, `expected_mode`, `modes`, `wayland_socket_ok`, `kscreen_doctor_ok`. |
| `doctor kwin` | **Real (Milestone 4A).** Reports unit path + install state, `systemctl is-active`, `/run/user/<uid>/wayland-0` presence, and a 20-line `journalctl -u clouddeploy-kwin-wayland.service` tail. |
| `phase apt-health` | **Real.** Runs FIRST in the apply chain. (1) preserves emergency sudo by writing `/etc/sudoers.d/90-clouddeploy-<SUDO_USER>` (validated with `visudo -cf`, installed via temp+atomic rename, mode 0440) so a clobbered `/etc/sudoers` mid-upgrade does not lock the operator out; (2) runs `dpkg --audit`, and on the `/var/lib/dpkg/updates/<N>` parse-error signature quarantines the journal files into `/var/lib/clouddeploy/backups/dpkg-updates-<ts>/` before running `dpkg --configure -a` + `apt-get -f install`; (3) detects mid-upgrade state (host VERSION_ID disagrees with the codename in `ubuntu.sources`) and records `mid_upgrade: true` + a `mid_upgrade_reason` in state.Details. Any unrecoverable repair failure fails the phase **before** ubuntu-upgrade touches sources. |
| `phase ubuntu-upgrade` | **Real, resume-friendly, target-resolver-aware.** Records a `stage` marker (`started → third-party-sources-disabled → sources-rewritten → dist-upgrade-started → dist-upgrade-complete → reboot-required`). Anti-loop guard fires only when stage indicates the previous run completed dist-upgrade AND `current==pre_version`. `rewriteAptCodename` is idempotent. Before mutating sources the phase stops `apt-daily.timer`, `apt-daily-upgrade.timer`, `unattended-upgrades.service` and waits up to 3 minutes for dpkg/apt locks. **New (this commit):** OS target resolver. When `deploy.ubuntu_candidates` is non-empty, the phase walks the candidate list newest-first via `ubuntu.ResolveOSTarget(...)` and records `ubuntu_selection_policy`, `ubuntu_candidates_configured`, `ubuntu_candidates_tried`, `selected_ubuntu_version`, `selected_ubuntu_codename`, `rejected_ubuntu_candidates`, `prefer_lts` in state.Details. The resolver gates on known-codename + v3-supported list + non-LTS knob + an optional `OSPackageProbeFn`. Policies: `latest-compatible` (default), `exact`, `min-version`, `any`. |
| `doctor lock` | **Real (this commit).** Read-only probe of `/var/lib/clouddeploy/state.lock`: reports the holder PID, whether it's still alive, and recovery instructions for stale locks. Use after a "lock is held by another clouddeployctl process" error. |
| `phase edid` | **Real (opt-in).** Invokes `helpers/write-edids.py`, writes `/lib/firmware/edid/<name>.bin`, drops a `/etc/default/grub.d/99-clouddeploy.cfg`, runs `update-initramfs -u` + `update-grub`, sets `RebootNeeded`. Only runs when `display.forced_connector` is set in the profile. |
| `phase kwin-patch` | Stub. Milestone 4. |
| `phase sunshine-build` | Stub. Milestone 4. |
| `phase services` | Stub. Milestone 4. |
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
| `internal/phase` | **Real for base-packages / nvidia-driver / cuda / edid.** |
| `internal/reboot` | **Real (minimal).** Continuation systemd unit + Install/Schedule/Disable. |
| `internal/ubuntu` | **Real.** `/etc/os-release` reader + supported-version gate. |
| `internal/edid` | **Real (helper-script wrapper).** Calls `helpers/write-edids.py` + drops EDID + edits grub. |
| `internal/gpu` | Stub. Milestone 4. |
| `internal/kwin` | Stub. Milestone 4. |
| `internal/sunshine` | Stub. Milestone 4. |
| `internal/systemd` | Stub. Milestone 4 (runtime services beyond the continuation unit). |
| `internal/validate` | Stub. Milestone 4. |

## 2. Phases still missing from a full hdr-4k120 deploy

| Phase | Owner | Notes |
| --- | --- | --- |
| `ubuntu_upgrade` | **v3 real (this commit)** | Direct apt codename rewrite 24.04 → 25.10. Honors `profile.deploy.{auto_upgrade_ubuntu,accept_non_lts,direct_apt_codename_upgrade}`. Reboot-aware. |
| `base_packages` | **v3 real** | Idempotent. |
| `nvidia_driver` | **v3 real** | Reboot-aware. |
| `cuda` | **v3 real** | mode=none default. |
| `edid` | **v3 real (opt-in)** | Reboot-aware. |
| `kwin_patch` | Milestone 4 | Apt-pin patched KWin + build + install + marker file. |
| `sunshine_build` | Milestone 4 | Pin commit `464bccf1`, dual-install, setcap, config. |
| `sunshine_config` | Milestone 4 | sunshine.conf without invalid keys + CSRF allowlist. |
| `systemd_units` | Milestone 4 | kwin-realvt + plasma-shell-realvt + sunshine-headless (with `SUNSHINE_FORCE_AV1_HDR10=1` + `SUNSHINE_SYNTHESIZE_HDR10_METADATA=1`). |
| `service_start` | Milestone 4 | Enable + start the unit chain. |
| `hdr_drm_validation` | Milestone 4 | `scripts/validate-hdr-drm-state.py` wrapped. |
| `hdr_stream_validation` | Milestone 4 | Journal grep for `Sent HDR mode control packet to Moonlight: enabled=1`. |
| `tailscale_setup` | Milestone 4 | `tailscale up` with `TAILSCALE_AUTHKEY` from env or root-only env-file. |
| `moonlight_pairing` | Milestone 4 | Sunshine PIN-pairing helper + CSRF allowlist verification. |

## 3. Remaining blockers to no-contact deploy

In rough order of how the deploy hits them:

1. **EDID/GRUB reboot sequencing — partially solved.** The EDID phase
   now generates the binary and updates GRUB; combined with the
   continuation service the operator no longer has to babysit the
   reboot. Validated for the `hdr-4k120` profile only.
2. **KWin patched build is missing.** Without it, KScreen does not
   report `HDR: enabled` + `Wide Color Gamut: enabled`, and the
   NVIDIA private DRM properties are never set. **Highest-impact
   missing piece**.
3. **Sunshine fork build is missing.** Without the pinned
   `464bccf1` build, the HDR control packet synthesis fix is not in
   place. Even with patched KWin, Moonlight would still see SDR.
4. **systemd unit generation is missing.** Without the right unit
   files (CAP_SYS_NICE + HDR env vars + GBM_BACKEND=nvidia-drm),
   Sunshine cannot start correctly.
5. **HDR validation is missing.** Without the post-deploy validator,
   we cannot programmatically prove the deploy reached the success
   state.
6. **Tailscale is missing.** Without it, the VM is unreachable from
   the operator's network and the deploy "succeeds" but no client
   can actually connect.

## 4. Exit codes (Milestone 3.5)

`clouddeployctl apply` and `resume` now use distinct exit codes so
`bootstrap.sh` and external CI / automation can branch correctly:

| Code | Meaning |
| --- | --- |
| `0` | All implemented phases done; no reboot pending. (Not "full deploy" — see the partial-apply banner.) |
| `1` | Real failure. Apply aborted; state records `failed_fatal` / `LastError` for the failing phase. |
| `2` | Reboot scheduled or required. State has `RebootNeeded=true` and `ResumeTarget=<phase>`. Continuation service is enabled. With `--auto-reboot` / `profile.deploy.auto_reboot: true`, `systemctl reboot` has been invoked. |
| `10` | Partial-apply banner printed: implemented phases completed cleanly, but KWin/Sunshine/services/HDR-validation phases are not yet implemented. The deploy is intentionally incomplete. |
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
| `hdr-4k120-cuda-auto` (this commit) | resolver `[26.04, 25.10, 24.04]` | broad: `latest-compatible`, `prefer_major=13`, cross-distro CUDA ladder, archive fallback ok | Composes OS resolver + CUDA cross-distro resolver. The "deploy this, figure out OS + CUDA" profile. |
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
9. Final exit code is **10** (partial-apply banner) — KWin / Sunshine
   / Tailscale / PipeWire / services / HDR validation are not
   implemented; **this is NOT a full streaming deploy**. For Moonlight
   `AV1 10-bit HDR` today, use the v2 entrypoint.

After the rest of Milestone 4 lands, the goal is final exit code **0**
with Moonlight overlay reading `AV1 10-bit HDR`.

If you need to send the run somewhere for inspection:

```bash
sudo clouddeployctl collect-logs --output /tmp/clouddeploy.tar.gz
```

bundles every relevant log, state file, and systemctl status into a
single tarball.
