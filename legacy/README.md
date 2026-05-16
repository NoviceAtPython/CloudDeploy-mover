# CloudDeploy v2 legacy artefacts

This directory is intentionally near-empty. It exists as a marker
for where v2 files **will move** once v3 reaches Milestone 5, but in
this milestone (1) v2 files remain at the repo root:

* `../CloudDeploy-wayland.sh` — primary v2 entrypoint.
* `../CloudDeploy.ps1` — Windows-side helper.
* `../clouddeploy-run-rootless-systemd.sh` — rootless variant.
* `../README.md` — top-level README, with v2 instructions intact.
* `../docs/HDR-NVIDIA-PRIVATE.md` — authoritative KWin DRM-side spec.
* `../docs/SUNSHINE-HDR-NEGOTIATION.md` — authoritative Sunshine
  fix-history doc.
* `../docs/final-hdr-success/KNOWN_GOOD_SUNSHINE_STATE.md` —
  authoritative success-state spec.
* `../patches/kwin-clouddeploy-nvidia-private-hdr.patch` — KWin
  source patch.
* `../scripts/validate-hdr-drm-state.py` — DRM-side validator
  invoked by both v2 and (eventually) v3 `internal/validate`.

Do not delete or modify the files above without working through
`docs/MIGRATION.md` first.

## When this directory gets populated

Milestone 5 (see `docs/MIGRATION.md`): once `clouddeployctl apply`
reaches the HDR success state end-to-end without invoking
`CloudDeploy-wayland.sh`, the v2 entrypoint moves here as
`legacy/CloudDeploy-wayland.sh` and the top-level `bootstrap.sh`
becomes the only supported deploy command.

## Why keep v2 at all

* Bug bisection. If a v3 module ever regresses behaviour that v2
  shipped correctly, the rollback command is `git checkout v2` +
  `bash CloudDeploy-wayland.sh`. That has to remain a working
  recovery path.
* Diagnostic. v2's structure (one big script, all logic visible)
  is sometimes the easier reference for "what does CloudDeploy do
  here?" than v3's per-package layout.
* History. The Moonlight `AV1 10-bit HDR` success state was
  achieved with v2's `CloudDeploy-wayland.sh` at commit
  `7d850e9`. That artifact is the proof of life for the
  technical stack; the repo should keep it readable.
