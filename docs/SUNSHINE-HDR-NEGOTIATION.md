# Sunshine HDR stream negotiation — open work

CloudDeploy reaches the documented "good" capture state automatically:

- KWin/Plasma 6 Wayland on real VT, DP-1 forced active, 3840×2160@120.
- KScreen reports `HDR: enabled` and `Wide Color Gamut: enabled`.
- Patched KWin writes the NVIDIA private DRM properties:
  `NV_CRTC_REGAMMA_TF=PQ`, `NV_INPUT_COLORSPACE=BT.2100 PQ`,
  `NV_PLANE_DEGAMMA_TF=PQ`, `NV_HDR_STATIC_METADATA=blob 0`.
- Sunshine binds the right KMS source:
  `STREAM_DIAG kms capture selected drm_device=/dev/dri/card1 connector=DP-1 width=3840 height=2160 pixel_format=AB30`.
- NVENC initialises and registers all three encoders
  (`h264_nvenc`, `hevc_nvenc`, `av1_nvenc`).
- Encoder capability probes report 10-bit support
  (`hevc_nvenc test: Color depth 10-bit`, `av1_nvenc test: Color depth 10-bit`).

But the actual stream that Moonlight launches still chooses **8-bit SDR**:

```
Creating encoder [av1_nvenc]
Color coding: SDR (Rec. 601)
Color depth: 8-bit
Color range: MPEG
```

and the Moonlight client log confirms:

```
AV1 actual stream bitdepth 8
```

The Sunshine config probe also reports:

```
Warning: Unrecognized configurable option [hdr]
```

so the `hdr = 1` key CloudDeploy used to write into `sunshine.conf`
**was a no-op**. As of this commit, CloudDeploy no longer writes that
line into `~/.config/sunshine/sunshine.conf` (both
`write_sunshine_config` and the `clouddeploy-reset-streaming` heredoc
have been updated to skip it with a comment pointing at this document).

That removes the misleading config but does **not** make Sunshine
honour client-requested HDR. The remaining work is in the Sunshine
fork at https://github.com/NoviceAtPython/Sunshine — see the patch
direction below.

## Patch direction (Sunshine fork, not the deploy script)

Done in the deploy script:

  1. **Removed `hdr = ${ENABLE_HDR}` from `sunshine.conf`.** Sunshine
     warns "Unrecognized configurable option [hdr]" so the line had no
     effect. Both write sites updated.

Still needed in the Sunshine fork:

  2. **Find where Sunshine parses Moonlight's launch/session HDR
     request.** Likely in `src/nvhttp.cpp` (`launch` / `resume` handlers)
     and `src/rtsp.cpp` (SDP negotiation). Moonlight's launch HTTP query
     carries an `hdr` parameter and a `videoFormat` bitmask (the
     `VIDEO_FORMAT_*` flags in `enet.h` / `stream.h`).

  3. **Find where actual stream encode parameters are chosen.** The
     codec/bitdepth/colorspace decision is in `src/video.cpp` / the
     encode session setup. The trace `Creating encoder [av1_nvenc]
     Color coding: SDR (Rec. 601) Color depth: 8-bit Color range: MPEG`
     names the exact lines that need to honour the client HDR request.

  4. **When Moonlight requests HDR and AV1 is selected, force:**
     - `encoder = av1_nvenc`
     - color depth = 10-bit
     - pixel format = `P010` / `yuv420p10le`
     - color coding = `BT.2020` / `PQ` (not SDR Rec.601)
     The capability probe already confirms `av1_nvenc test: Color depth
     10-bit`, so the encoder supports it; the gating is purely in the
     session-encode selection logic.

  5. **Fail loudly if AV1 Main10 / HDR cannot be selected.** Don't
     silently fall back to AV1 8-bit SDR. Log a fatal warning and let
     the client see the negotiation refusal.

  6. **Add diagnostic logs:**
     - `client requested HDR: yes/no`
     - `selected video format bitmask` (name + value)
     - `selected encoder`
     - `actual encode color depth`
     - `actual encode color coding`
     - `actual pixel format`

  7. **Investigate `Sent HD mode: false`.** This appears during session
     launch right before the stream falls back to AV1 8-bit. It is
     almost certainly part of the same negotiation path. Trace it back
     to its source in `src/rtsp.cpp` / `src/stream.cpp` and decide
     whether it's the toggle that flips the encoder to 8-bit.

## How CloudDeploy will apply the eventual Sunshine fork patch

Once the patch exists as a unified diff, the pattern is the same as
the KWin patch in
[`patches/kwin-clouddeploy-nvidia-private-hdr.patch`](../patches/kwin-clouddeploy-nvidia-private-hdr.patch):

  * Drop the patch into `patches/sunshine-clouddeploy-av1-hdr-enforce.patch`.
  * Run [`scripts/validate-kwin-patch.sh`](../scripts/validate-kwin-patch.sh) -style
    pre-flight (clone the Sunshine fork, `patch -p1 --dry-run`).
  * Wire `install_sunshine_from_fork_if_requested` in
    `CloudDeploy-wayland.sh` to apply the patch with `patch -p1
    --forward` after `git clone` of the fork and before `cmake`, using
    the same `require_clouddeploy_repo_asset` resolver so raw-curl
    deploys auto-fetch the file from `origin/v2`.

The deploy gating mirrors what already exists for KWin private HDR:

  * `ENABLE_HDR=1` deploys are refused if the patched Sunshine marker
    is missing.
  * `validate_streaming_stack_ready` requires the client-requested-HDR
    log line (item 6 above) and refuses 8-bit AV1 on an HDR-requested
    session.

None of that is wired up today. CloudDeploy still claims SDR success
on an `ENABLE_HDR=1` deploy even though the Moonlight stream is 8-bit
SDR. That gating will land once the Sunshine fork actually enforces
the HDR path.

## What the validator currently checks vs. doesn't

[`scripts/validate-hdr-drm-state.py`](../scripts/validate-hdr-drm-state.py)
walks `drm_info -j` for active CRTC + active primary plane and
confirms the **KWin/KMS side** is correct:
`NV_CRTC_REGAMMA_TF=PQ`, `NV_INPUT_COLORSPACE=BT.2100 PQ`,
`NV_PLANE_DEGAMMA_TF=PQ`. That side is good.

It does **not** validate the Moonlight stream. The Moonlight client
log line `AV1 actual stream bitdepth 8` is the canonical "stream HDR
failed" symptom; checking for it server-side requires the new logs
listed in item 6 above. Until those land, an end-to-end HDR check has
to be done from the Moonlight client (e.g. by reading the per-stream
log on a Windows host and confirming `bitdepth 10`).

## Quick local verify on the next deploy

After CloudDeploy reaches `validate_streaming_stack_ready` cleanly:

```bash
# 1. Sunshine no longer warns about hdr= in sunshine.conf
sudo -u user journalctl -u sunshine-headless.service --since "10 min ago" \
        | grep -F "Unrecognized configurable option" || echo "OK: no hdr= warning"

# 2. KWin/KScreen and DRM still pass
kscreen-doctor -o | grep -E 'HDR:|Wide Color Gamut:'
drm_info -j | python3 scripts/validate-hdr-drm-state.py DP-1

# 3. NVENC initialised
journalctl -u sunshine-headless.service --since "10 min ago" \
        | grep -E 'Found (H[.]264|HEVC|AV1) encoder|Nvenc initialized successfully'

# 4. The bit-depth probe (this is what's broken until the fork patch lands):
journalctl -u sunshine-headless.service --since "10 min ago" \
        | grep -E 'Color (coding|depth|range)|pixel_format|Sent HD mode'
```

Expected today: items 1-3 pass, item 4 still shows
`Color depth: 8-bit` / `Color coding: SDR (Rec. 601)` for the launched
stream. That is the residual failure this document tracks.
