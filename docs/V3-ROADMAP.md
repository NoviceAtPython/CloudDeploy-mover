# v3 roadmap

The plan from the architecture brief, with the current state marked.
Each milestone has a checklist of deliverables; tick them off as
commits land.

The v2 entry point (`CloudDeploy-wayland.sh` at the repo root) stays
the supported deploy path until Milestone 5. v3 should never claim
to replace v2 for a given phase until that phase is validated
end-to-end on a real VM.

## Milestone 1 — architecture, skeleton, first tests ✅ DONE (`2fd6879`)

Goal: lay out the v3 design and harden the highest-blast-radius
decision logic with unit tests, without changing any v2 behaviour.

- [x] v3 branch off v2 at `7d850e9`.
- [x] `docs/ARCHITECTURE.md`.
- [x] `docs/MIGRATION.md`.
- [x] `docs/ADR-0001-orchestrator-language.md` (chose Go).
- [x] `docs/ADR-0002-ansible-vs-native-go.md` (rejected Ansible).
- [x] `bootstrap.sh` glue (~150 lines).
- [x] `cmd/clouddeployctl/main.go` (Cobra CLI).
- [x] `internal/state/` schema + atomic save + tests.
- [x] `internal/nvidia/` family selector + tests (v1).
- [x] `internal/cuda/` mode policy + retry decider + tests.
- [x] `internal/apt/` policy-rc.d guard + tests.
- [x] `internal/config/` profile loader skeleton.
- [x] Stub packages for `runner`, `gpu`, `kwin`, `sunshine`, `edid`,
      `systemd`, `validate`, `reboot`.
- [x] `config/profiles/{hdr-4k120,sdr-safe}.yaml`.
- [x] `config/gpus/{rtx5090,rtx4090,l4}.yaml`.
- [x] `legacy/README.md` pointer doc.
- [x] v2 files untouched.

## Milestone 1.1 — CI, language stats, GPU coverage ⬅ THIS COMMIT

Goal: a foundation that is broader, cleaner, and harder to regress
before live deploy phases land.

- [x] `go.sum` committed (hashes pulled from sum.golang.org).
- [x] `.github/workflows/v3-go.yml` running `gofmt -s -l`,
      `go mod tidy --check`, `go vet`, `go test -race`, `go build`,
      `bash -n bootstrap.sh`, `shellcheck bootstrap.sh`.
- [x] `.gitattributes` Linguist overrides so the v2 monolith is
      marked vendored and Go / bootstrap.sh dominate language stats.
- [x] `internal/nvidia.Evidence` extended with `Available{Server,
      ServerOpen,NonServer,NonServerOpen}` and `Has*` helpers.
- [x] `SelectFamily` returns `(Family, reason, error)` and consults
      apt availability, with `ErrNoOpenAvailable` /
      `ErrNoFamilyAvailable` sentinels.
- [x] `internal/nvidia/database.go` taxonomy: `Category` / `Kind`
      enums; name-regex classifier for every Blackwell / Ada /
      Ampere / Turing / Volta / Pascal SKU we expect to see; PCI-ID
      table for the SKUs CloudDeploy has fingerprinted.
- [x] `Kind.SupportsAV1Encode` / `SupportsHDRStreaming` so doctor
      can refuse the HDR profile on a Pascal / V100 host with a
      clear reason.
- [x] New tests covering the nine v3-brief scenarios + database
      classification + helper behaviour.
- [x] `config/gpus/` expanded to 17 overlay files spanning every
      category in the brief (consumer, professional, datacenter).
- [x] `internal/config.LoadAllProfiles` / `LoadAllGPUs` +
      `ValidateProfile` / `ValidateGPU` + tests that load every
      committed YAML and assert the HDR / SDR profile invariants.
- [x] `docs/GPU-COMPATIBILITY.md` matrix.
- [x] `docs/V3-ROADMAP.md` (this file).
- [x] README v3 banner clarified to say v3 does not deploy yet.

## Milestone 2 — doctor + runner + apt transaction

Goal: a fresh VM can be inspected via `clouddeployctl doctor`
without any destructive action.

- [ ] `internal/runner.Exec(ctx, cmd, opts)` with structured logs
      under `/var/log/clouddeploy/<phase>.log`.
- [ ] `internal/apt.Transaction(ctx, fn)` owning lock-wait +
      policy-rc.d guard + dpkg-state repair.
- [ ] `clouddeployctl doctor nvidia` upgraded to print:
      classification, evidence, selected family, dkms status,
      `nvidia-smi -L`, `/proc/driver/nvidia/version`.
- [ ] `clouddeployctl doctor cuda` reports nvcc / libcudart presence
      + runfile `--check` result + RetryDecider history.
- [ ] `clouddeployctl doctor apt` reports dpkg lock holders, pending
      postinst, policy-rc.d presence, suspected service-start
      deadlocks.
- [ ] `clouddeployctl doctor kwin` and `doctor sunshine` skeletons
      with TODO links to Milestone 4.
- [ ] Validation: `doctor` agrees with `nvidia-smi`, `dkms status`,
      `dpkg -l`, `journalctl` on at least one RTX 4090 VM and one
      RTX 5090 VM.

## Milestone 3 — bootstrap-driven apply + v2 delegation

Goal: a fresh VM goes from clean Ubuntu to Moonlight HDR via
`bootstrap.sh`, with v3 owning the early phases and v2 still owning
the harder ones.

- [ ] `bootstrap.sh` actually invokes `clouddeployctl apply
      --profile hdr-4k120` on a fresh VM.
- [ ] `clouddeployctl apply` runs v3-implemented phases (base
      packages with policy-rc.d guard, NVIDIA family selection +
      install, CUDA mode policy).
- [ ] For phases v3 has not yet ported (Sunshine build,
      services, validation), `apply` shells out to
      `CloudDeploy-wayland.sh` via the existing env-file flow,
      recording per-phase status in `state.json`.
- [ ] `internal/reboot` owns the continuation systemd unit + the
      reboot scheduling. `clouddeployctl resume` is the resume
      entrypoint.
- [ ] Rollback documented: `clouddeployctl state reset --phase X`
      + falling back to `bash CloudDeploy-wayland.sh`.
- [ ] Validation: end-to-end fresh-VM deploy to HDR success state
      on RTX 4090 and RTX 5090.

## Milestone 4 — port the hard phases

Goal: every phase that currently delegates to
`CloudDeploy-wayland.sh` has a v3 implementation behind a config
flag.

- [ ] `internal/sunshine` — pin commit, clone, build, dual-install
      `/usr/local/bin/sunshine{-clouddeploy,}`, setcap
      `cap_sys_admin,cap_net_bind_service,cap_sys_nice+ep`, generate
      `sunshine.conf` with the right CSRF allowlist, generate
      `sunshine-headless.service` with the HDR env vars.
- [x] `internal/kwin` / `phase kwin-patch` — validate the configured
      patch, build/install patched KWin source packages, hold the
      installed packages, and write the marker file.
- [ ] `internal/edid` — generate forced-mode EDID, write under
      `/lib/firmware/edid/`, wire into kernel cmdline.
- [ ] `internal/systemd` — render templates under
      `templates/systemd/` with the live profile + GPU evidence.
- [ ] `internal/validate.HDRStream` — port
      `clouddeploy-validate-hdr-stream`'s journal grep.
- [ ] `internal/validate.HdrDrmState` — call
      `scripts/validate-hdr-drm-state.py` (or a Rust replacement if
      that landed first).
- [ ] Each phase has a `doctor <phase>` subcommand and its own tests.

## Milestone 5 — v3 default, v2 archived

Goal: v3 is the supported deploy path.

- [ ] `bootstrap.sh` defaults to all-v3 phases.
- [ ] `CloudDeploy-wayland.sh` moves to
      `legacy/CloudDeploy-wayland.sh`.
- [ ] `clouddeploy-run-rootless-systemd.sh` and `CloudDeploy.ps1`
      move to `legacy/` too.
- [ ] Top-level README updated to recommend the v3 entry path.
- [ ] `v2` branch on origin remains alive indefinitely as the
      bisect / rollback target.

## Optional later work (v3.x)

These are not blockers for v3 reaching production. Tracked here so
they don't slip through the cracks.

- [ ] Port `helpers/write-edids.py` to Rust (`edid-rs`).
- [ ] Port `scripts/validate-hdr-drm-state.py` to Rust (`drm-rs`).
- [ ] Add a `clouddeployctl gpu db` subcommand that prints the
      classification table from
      [`internal/nvidia/database.go`](../internal/nvidia/database.go)
      for documentation generation.
- [ ] Multi-host mode (Ansible inventory). Out of scope today; track
      the revisit trigger in
      [ADR-0002](ADR-0002-ansible-vs-native-go.md).
