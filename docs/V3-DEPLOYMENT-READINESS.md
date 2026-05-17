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
| `apply` | **Partial.** Runs `ubuntu-upgrade → base-packages → nvidia-driver → cuda → edid (opt-in)`. Exits 10 with banner; or 2 when reboot is pending (and triggers `systemctl reboot` when `--auto-reboot` is set / profile has `auto_reboot: true` (defaults to manual reboot)). Refuses unsupported Ubuntu versions unless `--allow-unsupported`, but if the host is on a v3-supported release AND `profile.deploy.auto_upgrade_ubuntu=true` AND the profile targets a different supported version, defers the exact-match gate to the ubuntu-upgrade phase. |
| `phase ubuntu-upgrade` | **Real (this commit).** v3 port of v2's `maybe_upgrade_ubuntu` + `direct_apt_codename_upgrade`. 24.04 → 25.10 path works; anti-reboot-loop guard via `state.Details["pre_version"]`. do-release-upgrade path is stubbed with a clear error (set `deploy.direct_apt_codename_upgrade=force` to skip it). |
| `resume` | **Real**. Reads state, clears RebootNeeded, replays implemented phases. Disables the continuation service when no further reboot is queued. |
| `phase base-packages` | **Real**. |
| `phase nvidia-driver` | **Real + dirty-driver cleanup planner.** |
| `phase cuda` | **Real**. Package-candidate discovery (e.g. `cuda-toolkit-13-0`, `nvidia-cuda-toolkit`) replaces the hard-coded name. |
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
| Repeated bad-CUDA-runfile retries | Mitigated by `cuda.RetryDecider`; runfile install path is M4. |
| CUDA apt package name guessed wrong | **Solved this commit** — `cuda.DiscoverCandidate` tries `cuda-toolkit-13-0`, `cuda-toolkit-12-4`, `nvidia-cuda-toolkit`, profile override. |
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

The `hdr-4k120` profile now ships with
`deploy.auto_upgrade_ubuntu: true` + `deploy.accept_non_lts: true` +
`deploy.direct_apt_codename_upgrade: auto` + `deploy.auto_reboot: false`.
That means the next VM test can start from **either**:

* a fresh Ubuntu 25.10 cloud image (no release-upgrade hop), or
* a fresh Ubuntu 24.04 cloud image — the v3 ubuntu-upgrade phase will
  reconcile to 25.10 via direct apt-source codename rewrite (the same
  path v2 uses, because `do-release-upgrade -d` refuses 24.04 → 25.10).

```bash
sudo CLOUDDEPLOY_RUN=1 PROFILE=hdr-4k120 bash bootstrap.sh
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
