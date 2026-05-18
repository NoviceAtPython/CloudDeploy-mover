# v2 → v3 parity audit

This is the formal capability audit of `CloudDeploy-wayland.sh` (v2)
against the current v3 Go orchestrator. It is the gating document for
the v3 rollout: v3 cannot claim "production parity" until every P0
row is implemented and every P1 row is at least partial.

Snapshot taken at v3 head **`dc2a042`** (Merge Milestone 3.5 / early
Milestone 4 work into `v3`).

## Status legend

| Status | Meaning |
| --- | --- |
| `implemented` | A real Go phase / package / CLI command does the work. |
| `partial` | Some of the v2 behaviour is in v3, but pieces are still missing or behind a flag. |
| `missing` | Not implemented in v3. The deploy will fall back to v2 or fail. |
| `intentionally deprecated` | v2 supported this but v3 has decided not to (e.g. Windows-VM path). Stays in v2 for v2 callers. |

## Priority legend

| Priority | Meaning |
| --- | --- |
| `P0 bootstrap blocker` | v3 cannot produce a working deploy from a fresh Ubuntu image without this. |
| `P1 full deploy` | Needed for the `AV1 10-bit HDR` success state. |
| `P2 convenience / diagnostic` | Nice to have; not required for the success state. |
| `deprecated` | Out of v3 scope. |

---

## P0 — bootstrap blockers

| v2 capability | Why it exists | v3 status | v3 owner | Notes |
| --- | --- | --- | --- | --- |
| Root + env-file + secrets loading (`write_clouddeploy_env_file`, `clouddeploy-write-env`) | v2 sources `/etc/clouddeploy-wayland.env` for `SUNSHINE_PASS` / `TAILSCALE_AUTHKEY` / etc. | partial | `cmd/clouddeployctl` profile YAML + env | v3 reads profile from `config/profiles/<name>.yaml`. Secrets (`SUNSHINE_PASS`, `TAILSCALE_AUTHKEY`) land later with the Tailscale / Sunshine phases. |
| Process lock (`acquire_clouddeploy_lock`) | Prevents two simultaneous deploys. | implemented | `internal/state.Acquire` | PID-file lock with stale-PID stealing. |
| Headless user creation + groups + sudo + linger (`normalize_clouddeploy_users`, `ensure_headless_user_admin_access`) | v2 creates `user`, adds to `video render input tty sudo`, enables linger. | missing | TBD `internal/phase/users.go` | Required before KWin / Sunshine phases land (those need a non-root user). |
| Ubuntu version + LTS classifier (`ubuntu_version_id`, `ubuntu_version_is_lts`, `ubuntu_codename_for_version`) | Drives the release-upgrade decision. | **implemented (this commit expands)** | `internal/ubuntu` | `internal/ubuntu.Gate` already does the version + supported-list check; this commit adds the codename mapping + LTS classifier used by the upgrade phase. |
| Non-LTS acceptance guard (`CLOUDDEPLOY_ACCEPT_NON_LTS`) | Refuses non-LTS targets (`25.10`, `26.10`) unless the operator opts in. | **implemented (this commit)** | `internal/ubuntu` + `deploy.accept_non_lts` profile knob | New `internal/phase/UbuntuUpgrade` honours `profile.deploy.accept_non_lts`. |
| Direct apt codename rewrite (`should_use_direct_apt_codename_upgrade`, `direct_apt_codename_upgrade`) | `do-release-upgrade -d` refuses 24.04 LTS → 25.10 ("Upgrades to the development release are only available from the latest supported release"). v2 rewrites `noble` → `questing` in apt sources directly. | **implemented (this commit)** | `internal/ubuntu.PlanCodenameRewrite` + `internal/phase/UbuntuUpgrade` | Driven by `deploy.direct_apt_codename_upgrade` (`auto` / `force` / `off`). Auto = yes for 24.04 → 25.10. |
| Optional Ubuntu release upgrade (`maybe_upgrade_ubuntu`) | Top-level orchestrator. Reads target, plans hop, runs upgrade, reboots, validates progress. | **implemented (this commit)** | `internal/phase/UbuntuUpgrade` | Driven by `deploy.auto_upgrade_ubuntu`. Default `false`. |
| Anti-reboot-loop guard | v2 keeps a progress file with the pre-upgrade version. After reboot, if `VERSION_ID` didn't move, it dies instead of looping. | **implemented (this commit)** | `internal/phase/UbuntuUpgrade` | Uses `state.Get("ubuntu_upgrade").Details["pre_version"]`. |
| Continuation systemd service (`install_continuation_service`, `schedule_reboot_for_continuation`) | Resumes the deploy after reboot. | implemented | `internal/reboot.Service` | M3.5 deliverable. |
| Cloud-init wait (`wait_for_cloud_init`) | Prevents apt fights between cloud-init and CloudDeploy on first boot. | missing | TBD `internal/apt` extension | apt-transaction wrapper would block on cloud-init's apt lock anyway; explicit wait is friendlier diagnostics. |
| apt lock wait (`wait_for_apt`) | Polls `/var/lib/dpkg/lock-frontend` etc. before apt-get. | implemented | `internal/apt.Transaction` (`waitForLocks`). | |
| apt progress heartbeat (`start_apt_progress_reporter` / `stop_apt_progress_reporter`) | v2 prints periodic "still alive" lines during long apt operations so the operator doesn't think it hung. | missing | `internal/runner` extension | Nice-to-have; partial mitigation: per-command log file + structured stdout. |
| Corrupt dpkg-updates queue (`move_corrupt_dpkg_updates_aside`) | Moves `/var/lib/dpkg/updates/*` aside when dpkg --audit reports parse errors. | missing | `internal/apt.RepairDpkgUpdates` | Sees real cloud VMs after dirty reboots. |
| snapd postinst failure purge (`purge_snapd_after_postinst_failure`) | v2 purges `snapd` when its postinst blocks apt and CloudDeploy doesn't need Snap. | missing | `internal/apt.PurgeSnapdIfBlocking` | snapd is in default Ubuntu cloud images and breaks roughly every other release. Worth porting. |
| libblockdev bad-state recovery (`repair_libblockdev_bad_state`) | Workaround for `libblockdev-mdraid3` ending up in a "very bad inconsistent state". | missing | `internal/apt.RepairLibblockdev` | Lower frequency than snapd but real on Ubuntu 25.10. |
| `dpkg --configure -a` + `apt -f install` retry (`repair_dpkg_state_if_needed`) | Umbrella that drives the three above. | partial | `internal/apt.RepairIfNeeded` | v3 runs `dpkg --audit` + `dpkg --configure -a` + `apt-get -f install`, but does not yet branch on the parse-error / snapd / libblockdev signals. |
| Base packages (`apt_install_wait` + base list) | Minimum tooling install. | implemented | `internal/phase.BasePackages` | |
| NVIDIA driver family selection (`select_nvidia_driver_package_family`) | Picks server / server-open / non-server / non-server-open. | implemented | `internal/nvidia.SelectFamily` + `internal/phase.NvidiaDriver` | The whole reason v3 exists. Open-module hard requirement is honoured. |
| NVIDIA driver install (`install_target_nvidia_driver`) | apt-installs the chosen family + DKMS. | implemented | `internal/phase.NvidiaDriver` | Uses `apt.Transaction`. |
| Dirty NVIDIA driver cleanup (`cleanup_conflicting_nvidia_driver_packages`) | Purges old conflicting NVIDIA / CUDA packages before install. | implemented | `internal/nvidia.PlanCleanup` + `internal/phase.NvidiaDriver` | --repair-driver-family flag. |
| Provider GPU init failure diagnostics (`provider_gpu_failure_message`, `detect_provider_gpu_init_failure`) | Parses dmesg for `NVRM` `RmInitAdapter failed`, surfaces a clear error. | partial | `internal/nvidia.EvaluatePostInstall` | Detects "requires open kernel modules" already. `RmInitAdapter` lines not parsed yet. |
| NVIDIA driver post-install validation (`validate_nvidia_driver_acceptance`) | Runs `nvidia-smi -L`, checks DKMS, decides "reboot" vs "fatal". | implemented | `internal/nvidia.EvaluatePostInstall` + `internal/phase.NvidiaDriver` | |
| CUDA policy (`install_cuda_toolkit_if_requested`) | `mode = none / optional / required`, `method = auto / apt / runfile / none`. | implemented | `internal/cuda.Plan` + `internal/cuda.ParseMethod` + `internal/phase.Cuda` | `hdr-4k120` keeps `mode=none` (KMS+NVENC HDR does not need CUDA); `hdr-4k120-cuda` exercises `mode=required` end-to-end. |
| CUDA apt-repo bootstrap (`detect_cuda_repo_distro` + `ensure_cuda_ubuntu_repo`) | Downloads + installs `cuda-keyring_1.1-1_all.deb`, runs `apt-get update`. | implemented | `internal/cuda.RepoDistroForUbuntuVersion` + `internal/cuda.KeyringURL` + `internal/cuda.DetectRepoDistro` + `internal/phase.Cuda.ensureCudaAptRepo` | HEAD-probes the keyring URL before adopting the distro slug. |
| CUDA apt candidate ladder (`CUDA_TOOLKIT_PACKAGE` + `apt-cache policy`) | Prefer `cuda-toolkit-13-N` then `cuda-toolkit`, then Ubuntu's `nvidia-cuda-toolkit`. | implemented | `internal/cuda.CandidateLadder` + `internal/cuda.DiscoverCandidate` | When `RequiredMajor=13`, Ubuntu's `nvidia-cuda-toolkit` is filtered out (it currently installs CUDA 12.x). |
| CUDA toolkit-only install (apt: never `cuda` / `cuda-drivers`; runfile: `--toolkit --override`) | Keeps the cuda phase from clobbering the driver phase's installed family. | implemented | `internal/phase.Cuda` apt-path refuses `cuda-drivers*`; runfile path passes `--silent --toolkit --override --tmpdir=...`. | |
| CUDA runfile install (`install_cuda_toolkit_from_runfile`) | Stages under `/var/tmp/clouddeploy-cuda`, curl-retries with size + sha256 logging, `--check` before install. | implemented | `internal/phase.Cuda.runRunfilePath` + `internal/cuda.RunfileTmpDir` + `internal/cuda.MinRunfileBytes` + `internal/cuda.ResolveRunfileMaxAttempts` | Honors `cuda.runfile_url`, `cuda.runfile_sha256`, `cuda.runfile_max_attempts`, `CLOUDDEPLOY_CUDA_RUNFILE_MAX_ATTEMPTS` env. |
| CUDA runfile retry-suppression (`install_cuda_toolkit_from_runfile`) | Refuses to redownload the same SHA after a failed `--check`. | implemented | `internal/cuda.RetryDecider` wired into `internal/phase.Cuda.runRunfilePath` | The original v2 regression (5 identical 4.3 GiB downloads, all failing `--check`) is now caught after attempt 1. |
| CUDA post-install verification (`cuda_toolkit_ready`, `verify_cuda_toolkit_version_matches`, `verify_cuda_toolkit_or_fail`) | Checks `nvcc --version`, `/usr/local/cuda/{bin/nvcc,include,lib64}`, refuses CUDA 12 when CUDA 13 required. | implemented | `internal/cuda.ParseNvccRelease` + `internal/cuda.ExpectedMajor` + `internal/cuda.MajorMatches` + `internal/phase.Cuda.verifyAndFinish` | Writes `/etc/profile.d/clouddeploy-cuda.sh` so login shells pick up `/usr/local/cuda/{bin,lib64}`. |
| EDID generation + GRUB cmdline (`update_grub_clouddeploy`, `update_initramfs_clouddeploy`, `scripts/write-edids.py`) | Generates 4K120 HDR EDID, writes GRUB cmdline, updates initramfs / grub, reboots. | implemented | `internal/edid` + `internal/phase.Edid` | M3.5. |

## P1 — needed for full deploy

| v2 capability | Why it exists | v3 status | v3 owner | Notes |
| --- | --- | --- | --- | --- |
| KDE / Plasma / KWin package install (`kde_plasma_package_list`) | Installs Plasma 6 + KWin Wayland + plasma-shell. | missing | TBD `internal/phase/kde.go` | Depends on `base-packages` and `nvidia-driver`. |
| Plasma 6 availability checks (`installed_plasma_version`, `installed_kwin_version`) | v2 refuses if Plasma 6 isn't available on the running release; this is the link to the ubuntu-upgrade phase. | missing | TBD `internal/kde` | Pairs with `kde` phase. |
| KWin private HDR patch build / install (`build_install_patched_kwin`) | Applies `patches/kwin-clouddeploy-nvidia-private-hdr.patch`, builds, installs. | missing | TBD `internal/kwin` | The single highest-impact missing piece for HDR. |
| KWin apt pin / hold / marker (`write_patched_kwin_apt_pin`) | Prevents `unattended-upgrades` from replacing the patched build. | missing | TBD `internal/kwin` | |
| Sunshine fork clone / build / pin (`install_sunshine_from_fork_if_requested`) | Pins `464bccf1`, builds, dual-installs, setcap. | missing | TBD `internal/sunshine` | Second-highest impact. |
| Sunshine setcap (cap_sys_admin + cap_net_bind_service + cap_sys_nice) | Without it, NVENC scheduling breaks at 4K120. | missing | TBD `internal/sunshine` | |
| Sunshine runtime assets (`/usr/local/assets/`) | Web UI + apps.json. | missing | TBD `internal/sunshine` | |
| sunshine.conf generation (no `hdr=` / `fps=` / `resolutions=` invalid keys) | v2 lesson: those keys make Sunshine warn and confuse newcomers. | missing | TBD `internal/sunshine.WriteConfig` | |
| Sunshine CSRF allowlist + Tailscale IP (`csrf_allowed_origins`) | Pairing-PIN endpoint refuses non-allowlisted origins. | missing | TBD `internal/sunshine` | |
| KWin / Plasma / Sunshine systemd units (`install_clouddeploy_systemd_units`) | The runtime services that drive the streaming stack. | missing | TBD `internal/systemd` | |
| Tailscale install + auth + connect (`validate_tailscale_authkey`) | Required to reach the VM from Moonlight. | missing | TBD `internal/phase/tailscale.go` | Needs `TAILSCALE_AUTHKEY` secret. |
| PipeWire virtual sink (`install_clouddeploy_pipewire_virtual_sink`) | Sunshine audio capture needs a sink in the VM (no real HDA). | missing | TBD `internal/phase/audio.go` | |
| Force-KWin-mode helper (`clouddeploy-force-kwin-mode.sh` writer) | Periodically asserts `kscreen-doctor output.DP-1.mode.3840x2160@120`. | missing | TBD `internal/kwin` | |
| Reset-streaming helper (`clouddeploy-reset-streaming`) | One-button "restart the streaming stack" for the operator. | missing | TBD `internal/systemd` | |
| Watchdog service / timer (`clouddeploy-watch-streaming.timer`) | Restarts Sunshine if it falls over. | missing | TBD `internal/systemd` | |
| Final streaming validation (`validate_streaming_stack_ready`) | Walks the journal + DRM state + `nvidia-smi` + `kscreen-doctor` and confirms everything. | missing | TBD `internal/validate` | |
| HDR DRM validation (`validate_hdr_final_state` + `scripts/validate-hdr-drm-state.py`) | Confirms `NV_CRTC_REGAMMA_TF=PQ` + `NV_INPUT_COLORSPACE=BT.2100 PQ`. | partial | wraps existing Python helper | Helper exists; v3 wrapper / phase to invoke it does not. |
| HDR stream packet validation (`/usr/local/sbin/clouddeploy-validate-hdr-stream`) | Greps Sunshine journal for `Sent HDR mode control packet to Moonlight: enabled=1`. | missing | TBD `internal/validate` | |

## P2 — convenience / diagnostic

| v2 capability | Status | Notes |
| --- | --- | --- |
| Optional apps (Steam, Heroic, Lutris, Bottles, Prism, ProtonUp-Qt, Chrome) (`install_optional_apps_nonfatal`) | missing | M4+. Pure quality-of-life; not on the success-criteria path. |
| Final summary / checklist (`print_final_validation_summary`, `print_known_good_checklist`) | missing | M4. |
| `clouddeploy-run` helper | n/a | v3 has `bootstrap.sh` + direct `clouddeployctl` invocations. |
| `clouddeploy-write-env` helper | partial | Profile YAML covers the configurable part; secrets land later. |
| Extended diagnostics (`clouddeploy_failure_diagnostics`, `print_*_diagnostics`) | partial | `doctor system / apt / nvidia / cuda` covers the read-only side. `collect-logs` bundles the rest into a tarball. M3.5. |

## Deprecated (intentionally out of v3 scope)

| v2 capability | Why deprecated |
| --- | --- |
| Windows VM path (`CloudDeploy.ps1`, `main.py`) | v3 is Ubuntu-only. See `docs/UBUNTU-ONLY.md`. |
| Rootless variant (`clouddeploy-run-rootless-systemd.sh`) | Out of v3 scope; v2 callers still have it. |
| EDID descriptor helpers inside the Bash script | Moved to `scripts/write-edids.py` (which v3 invokes). |

## Apply-order parity

v2 calls happen in this order inside `CloudDeploy-wayland.sh`'s main
flow (simplified):

```
maybe_upgrade_ubuntu                 # P0
repair_dpkg_state_if_needed          # P0
apt_install_wait base-packages       # P0
install_target_nvidia_driver         # P0
install_cuda_toolkit_if_requested    # P0
build_install_patched_kwin           # P1
install_sunshine_from_fork_if_requested  # P1
install_clouddeploy_systemd_units    # P1
install_clouddeploy_pipewire_virtual_sink # P1
write_sunshine_config                # P1
known_good_clean_reset_streaming_stack # P1
validate_streaming_stack_ready        # P1
validate_hdr_final_state              # P1
finalize_success                      # P2
```

v3 apply at the end of this commit:

```
phase ubuntu-upgrade   # NEW this commit, P0
phase base-packages    # implemented
phase nvidia-driver    # implemented
phase cuda             # implemented
phase edid             # implemented
                       # ↓ remaining P1 phases not yet ported
phase kwin-patch       # NOT IMPLEMENTED
phase sunshine-build   # NOT IMPLEMENTED
phase services         # NOT IMPLEMENTED
phase validate         # NOT IMPLEMENTED
```

## Acceptance criteria for "v3 = v2"

v3 cannot claim parity until:

1. Every P0 row is `implemented`.
2. Every P1 row is at least `partial` and behind a profile flag.
3. The live VM walks from a fresh Ubuntu 24.04 image to Moonlight
   `AV1 10-bit HDR` via `bootstrap.sh` with no operator intervention
   (auto-upgrade + auto-reboot enabled in the profile).
4. v2 stays untouched and remains a 1-line fallback for the cases
   v3 hasn't covered yet.

This commit closes the **ubuntu-upgrade** row. The remaining P0
gaps (headless user, cloud-init wait, snapd / libblockdev / corrupt-
updates dpkg branches) and every P1 row remain open. See
[V3-ROADMAP.md](V3-ROADMAP.md) for the milestone-by-milestone plan.
