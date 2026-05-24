# Sunshine HDR stream negotiation — solved

## Current state

**Moonlight overlay reads `AV1 10-bit HDR`** on the live VM at:
`3840x2160 @ 120 Hz`, AV1 NVENC, P010, BT.2020 + SMPTE2084 PQ.

The fix lives in three places, all reproducible from this repo:

1. The patched KWin NVIDIA private HDR path (DRM-side).
   See [`docs/HDR-NVIDIA-PRIVATE.md`](HDR-NVIDIA-PRIVATE.md) and
   [`patches/kwin-clouddeploy-nvidia-private-hdr.patch`](../patches/kwin-clouddeploy-nvidia-private-hdr.patch).
2. The Sunshine fork at `NoviceAtPython/Sunshine`, branch
   `codex/sunshine-pairing-diagnostics`, pinned in CloudDeploy to commit
   **`8f02b1ce455ce4e7efa1b4031bb23764f2809cdb`** (see
   [`docs/final-hdr-success/KNOWN_GOOD_SUNSHINE_STATE.md`](final-hdr-success/KNOWN_GOOD_SUNSHINE_STATE.md)
   for the full pin spec).
3. Two runtime env vars CloudDeploy now auto-sets in
   `sunshine-headless.service` whenever `ENABLE_HDR=1`:
   - `SUNSHINE_FORCE_AV1_HDR10=1`
   - `SUNSHINE_SYNTHESIZE_HDR10_METADATA=1`

## Root cause (for posterity)

Moonlight's HDR/SDR overlay badge does **not** read from the encoded
bitstream colour primaries. It reads from a Sunshine control packet
(`control_hdr_mode_t` in `src/stream.cpp`). The relevant field is
`enabled` — when it ships `0`, Moonlight labels the stream SDR even
when the bitstream itself is BT.2020 + SMPTE2084.

Before the fork patch, Sunshine only set `enabled = true` if the
display backend reported `HDR_OUTPUT_METADATA` mastering metadata via
the standard DRM connector property. On the CloudDeploy NVIDIA
forced-headless virtual-output path that blob is intentionally
`blob 0`:

- Stock connector HDR path (`HDR_OUTPUT_METADATA` non-zero +
  connector `Colorspace`): NVIDIA's atomic check rejected the
  configuration ("the driver rejected the output configuration").
- The patched-KWin known-good state uses the NVIDIA private CRTC/plane
  properties instead: `NV_CRTC_REGAMMA_TF=PQ`,
  `NV_INPUT_COLORSPACE=BT.2100 PQ`, `NV_PLANE_DEGAMMA_TF=PQ`,
  `NV_HDR_STATIC_METADATA=blob 0`. Scanout is genuine HDR PQ; the
  standard metadata blob stays empty by design.

The Sunshine fork's pinned commit `8f02b1ce`:

1. Adds an NVIDIA-private-HDR detector in `src/platform/linux/kmsgrab.cpp`
   that walks the active CRTC + primary plane DRM props and accepts
   `NV_CRTC_REGAMMA_TF==PQ` and/or `NV_INPUT_COLORSPACE==BT.2100 PQ` as
   evidence of HDR scanout.
2. Synthesises HDR10 mastering metadata defaults (BT.2020 primaries,
   D65 white point, 1000-nit peak, 0.005-nit min, MaxCLL=1000,
   MaxFALL=400) when `SUNSHINE_SYNTHESIZE_HDR10_METADATA=1` and the
   DRM blob is 0.
3. Forces `active_av1_mode=3` (advertises `SCM_AV1_MAIN10` in
   `/serverinfo`) when `SUNSHINE_FORCE_AV1_HDR10=1` and the encoder
   probe says AV1 Main10 is supported.
4. Sends `control_hdr_mode_t.enabled=1` with the synthesised metadata
   on every session start.

The resulting log line (server-side ground truth that Moonlight saw
the HDR packet):

```
Sent HDR mode control packet to Moonlight: enabled=1
        maxDisplayLuminance=1000 nits minDisplayLuminance=50/10000 nits
        MaxCLL=1000 MaxFALL=400 primaries[R]=(35400,14600)
        whitePoint=(15635,16450)
```

## What CloudDeploy now does automatically

* `install_sunshine_from_fork_if_requested` clones the fork, hard-resets
  to `SUNSHINE_FORK_COMMIT`, and builds. Set `SUNSHINE_FORK_COMMIT=""`
  to follow the branch tip instead. Hash and rationale in
  [`docs/final-hdr-success/KNOWN_GOOD_SUNSHINE_STATE.md`](final-hdr-success/KNOWN_GOOD_SUNSHINE_STATE.md).
* The built binary is installed twice: at the CloudDeploy-internal path
  `/usr/local/bin/sunshine-clouddeploy` and at the generic
  `/usr/local/bin/sunshine`. Both get
  `setcap cap_sys_admin,cap_net_bind_service,cap_sys_nice+ep` to match
  the known-good live-VM state.
* `sunshine-headless.service` ships `Environment=SUNSHINE_FORCE_AV1_HDR10`
  and `Environment=SUNSHINE_SYNTHESIZE_HDR10_METADATA`, both defaulting
  to `${ENABLE_HDR}`. The service unit also lists `CAP_SYS_NICE` in
  `AmbientCapabilities`/`CapabilityBoundingSet`.
* `sunshine.conf` no longer writes `hdr =`, `fps =`, or
  `resolutions =`. Sunshine refused all three with
  `Warning: Unrecognized configurable option`. fps/resolution come from
  KWin's EDID + the active mode line, not from `sunshine.conf`.
* `csrf_allowed_origins` is set to
  `https://localhost:47990,https://127.0.0.1:47990,https://${TAILSCALE_IP}:47990`.
  Older configs only listed the Tailscale origin, which made
  PIN-pairing fail when the operator used the local browser or an
  SSH-tunnelled `https://127.0.0.1:47990`.

## Verifying success after deploy

After `clouddeploy-run` succeeds:

```bash
# KWin/KMS-side HDR good state (display)
kscreen-doctor -o | grep -E 'HDR:|Wide Color Gamut:'
drm_info -j | python3 /opt/clouddeploy-mover/scripts/validate-hdr-drm-state.py DP-1

# Sunshine-side env vars baked into the unit
systemctl cat sunshine-headless.service \
        | grep -E 'SUNSHINE_FORCE_AV1_HDR10|SUNSHINE_SYNTHESIZE_HDR10_METADATA|CAP_SYS_NICE'

# Sunshine fork pinned commit
git -C /opt/sunshine-src rev-parse HEAD
# Expected: 8f02b1ce455ce4e7efa1b4031bb23764f2809cdb

# Capabilities on both binaries
getcap /usr/local/bin/sunshine-clouddeploy
getcap /usr/local/bin/sunshine
# Both should report: cap_net_bind_service,cap_sys_nice,cap_sys_admin+ep
```

Then connect Moonlight, start a stream, and confirm:

```bash
sudo /usr/local/sbin/clouddeploy-validate-hdr-stream
```

Expected `[OK]` markers:

* `is_hdr: NVIDIA private HDR via NV_INPUT_COLORSPACE=BT.2100 PQ` /
  `NV_CRTC_REGAMMA_TF=PQ`
* `Encode selection: ... selected_colorspace=HDR ... selected_bit_depth=10-bit ... selected_pix_fmt=p010`
* `HDR metadata fallback: standard DRM HDR_OUTPUT_METADATA blob is 0 ... synthesizing HDR10 static metadata defaults`
* `NvEnc color-config: codec=AV1 primaries=9 transfer=16 matrix=9 full_range=0 bit_depth=10 yuv444=no`
* `Sent HDR mode control packet to Moonlight: enabled=1`

Moonlight's overlay should read `AV1 10-bit HDR`.

## What to do if `clouddeploy-validate-hdr-stream` is missing markers

1. **`enabled=0` line present** — the Sunshine fork didn't synthesise.
   Check that the binary was actually built from
   `8f02b1ce455ce4e7efa1b4031bb23764f2809cdb` (or newer with the same
   synthesis logic):
   `git -C /opt/sunshine-src rev-parse HEAD`.
   Then verify the env vars reached the running process:
   `tr '\0' '\n' < /proc/$(pgrep -x sunshine | head -n1)/environ \
       | grep -E 'SUNSHINE_FORCE_AV1_HDR10|SUNSHINE_SYNTHESIZE_HDR10_METADATA'`.
2. **`NVIDIA private HDR via ...` missing** — KWin didn't drive the
   private NVIDIA DRM properties. Check
   `KWIN_CLOUDDEPLOY_NVIDIA_PRIVATE_HDR=1` in
   `/etc/clouddeploy-wayland.env`, the patched-KWin marker
   (`/var/lib/clouddeploy/patched-kwin-installed`), and
   `kscreen-doctor -o | grep -E 'HDR:|Wide Color Gamut:'`.
3. **`Color coding: SDR (Rec. 601)` present** — Sunshine chose the SDR
   colorspace despite client HDR request. Confirm
   `client_dynamicRange=1` in the new `Client video request:` log line;
   if it's 0, Moonlight is asking for SDR (most often because
   `SUNSHINE_FORCE_AV1_HDR10` did not reach `serverinfo` and Moonlight
   didn't see `SCM_AV1_MAIN10`).

## Display-side metadata: DO NOT re-enable

The CloudDeploy KWin patch deliberately:

* skips `HDR_OUTPUT_METADATA` (the standard connector blob);
* skips the connector `Colorspace` property;
* leaves `NV_HDR_STATIC_METADATA` as `blob 0`.

Every variant that tried to write those properties failed on the
NVIDIA forced-headless virtual output (driver rejection, HDR went off,
or Sunshine's `is_hdr()` was the wrong end of the chain anyway). The
fix is Sunshine-side stream metadata synthesis, not display-side DRM
metadata. See [`docs/HDR-NVIDIA-PRIVATE.md`](HDR-NVIDIA-PRIVATE.md).
