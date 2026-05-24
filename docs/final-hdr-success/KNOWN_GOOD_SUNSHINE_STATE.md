# Known-good Sunshine HDR success state

This is the exact end-to-end Sunshine state that produced
**`AV1 10-bit HDR`** on the Moonlight overlay against
`3840x2160 @ 120 Hz`, AV1 NVENC, P010, BT.2020 + SMPTE2084 PQ on the
CloudDeploy live VM (Ubuntu 25.10, NVIDIA RTX 4090, driver 580.x,
patched KDE Plasma / KWin Wayland).

Document this aggressively because Moonlight's HDR/SDR badge depends on
multiple independent signals being correct simultaneously; one of them
moving silently is enough to break HDR with no other symptom.

## Sunshine fork pin

| Field | Value |
| --- | --- |
| Repo | `https://github.com/NoviceAtPython/Sunshine` |
| Branch | `codex/sunshine-pairing-diagnostics` |
| Commit (pinned) | `8f02b1ce455ce4e7efa1b4031bb23764f2809cdb` |
| Optional permanent target | `clouddeploy-av1-hdr-success` (suggested; not auto-created) |

CloudDeploy v3 hard-resets the Sunshine working tree to the profile
`sunshine.fork_commit` after `git fetch`/`git checkout` of the branch.
The current v3 profiles pin:

```bash
sunshine.fork_commit: 8f02b1ce455ce4e7efa1b4031bb23764f2809cdb
```

The older `CloudDeploy-wayland.sh` v2 script may still reference the
historical `464bccf1` pin. Bump any pin only after you have re-verified the
success criteria below end-to-end.

## Required env vars (Sunshine side)

CloudDeploy bakes both of these into `sunshine-headless.service` via
`Environment=` lines, defaulting to `${ENABLE_HDR}`:

| Var | Effect inside the fork |
| --- | --- |
| `SUNSHINE_FORCE_AV1_HDR10=1` | In `probe_encoders()`: bumps `active_av1_mode` 2→3 (and `active_hevc_mode`) when the encoder probe confirms `DYNAMIC_RANGE` support, so `/serverinfo`'s `ServerCodecModeSupport` flips on `SCM_AV1_MAIN10`. In `rtsp::cmd_announce()`: forces `config.monitor.dynamicRange=1` when the client RTSP `dynamicRangeMode` arrived as `0`. |
| `SUNSHINE_SYNTHESIZE_HDR10_METADATA=1` | In `kmsgrab::display_t::get_hdr_metadata()` + the 3 `video.cpp` consumer sites: when the standard DRM `HDR_OUTPUT_METADATA` blob is `0` (it always is on the NVIDIA forced-headless private HDR path), synthesises BT.2020 / D65 / 1000-nit / MaxCLL=1000 / MaxFALL=400 defaults so the Sunshine control packet ships `enabled=1`. |

Both env vars also end up in `/etc/clouddeploy-wayland.env` for
`clouddeploy-reset-streaming` and operator shells.

## Required CloudDeploy KWin/DRM state (display side)

`docs/HDR-NVIDIA-PRIVATE.md` is the authoritative spec. Summary:

* Patched KWin from `patches/kwin-clouddeploy-nvidia-private-hdr.patch`.
* Standard connector `HDR_OUTPUT_METADATA` blob: `0` (intentionally).
* Standard connector `Colorspace`: stock (`Default`, intentionally).
* CRTC `NV_CRTC_REGAMMA_TF`: `PQ`.
* Active primary plane `NV_INPUT_COLORSPACE`: `BT.2100 PQ`.
* Active primary plane `NV_PLANE_DEGAMMA_TF`: `PQ`.
* `NV_HDR_STATIC_METADATA`: `blob 0` (intentionally).

## Required Sunshine binary state

* `/usr/local/bin/sunshine-clouddeploy`: source-built from
  `SUNSHINE_FORK_COMMIT`, mode `0755`,
  `setcap cap_sys_admin,cap_net_bind_service,cap_sys_nice+ep`.
* `/usr/local/bin/sunshine`: identical copy with identical caps. This
  shadows any stale packaged `/usr/bin/sunshine` so manual `sunshine`
  invocations and the web UI's "Open Sunshine" both pick the fork
  build, avoiding the recurring `libminiupnpc` / `libicu` ABI-skew
  failure mode.

## Required systemd service state

`sunshine-headless.service` must have:

```
NoNewPrivileges=no
AmbientCapabilities=CAP_SYS_ADMIN CAP_NET_BIND_SERVICE CAP_SYS_NICE
CapabilityBoundingSet=CAP_SYS_ADMIN CAP_NET_BIND_SERVICE CAP_SYS_NICE
Environment=SUNSHINE_FORCE_AV1_HDR10=1
Environment=SUNSHINE_SYNTHESIZE_HDR10_METADATA=1
```

Without `NoNewPrivileges=no`, file caps are stripped and Sunshine falls
back to `/dev/dri/card0` `connector=Virtual-1 1024x768` on a multi-GPU
VM, logging `Failed to gain CAP_SYS_ADMIN` + `missing_fb_handle`.

## Required `sunshine.conf` state

The minimal correct conf — CloudDeploy writes exactly this, no more:

```ini
min_log_level = debug
encoder = nvenc
capture = kms
adapter_name = /dev/dri/card1
hevc_mode = 0
av1_mode = 2
stream_audio = disabled
address_family = ipv4
ping_timeout = 60000
csrf_allowed_origins = https://localhost:47990,https://127.0.0.1:47990,https://<TAILSCALE_IP>:47990
```

**Do not** write `hdr =`, `fps =`, or `resolutions =`. Sunshine refuses
all three with `Warning: Unrecognized configurable option`. fps and
resolution come from KWin's EDID + the active mode line.

`av1_mode = 2` is fine here because `SUNSHINE_FORCE_AV1_HDR10=1`
overrides it to `3` at runtime in the fork.

## Proof log markers (Sunshine journal, after Moonlight connect)

The streaming-side HDR overlay flips on once Moonlight connects and
the new session starts. Run
`/usr/local/sbin/clouddeploy-validate-hdr-stream` to grep for all of:

```
is_hdr: NVIDIA private HDR via NV_INPUT_COLORSPACE=BT.2100 PQ on plane <N>
HDR metadata fallback: standard DRM HDR_OUTPUT_METADATA blob is 0 on
  NVIDIA private HDR path; synthesizing HDR10 static metadata defaults
  (BT.2020 primaries, D65 white point, max=1000 nits, min=50/10000 nits,
  MaxCLL=1000, MaxFALL=400)
Client video request: codec=AV1 (videoFormat=2) dynamicRangeMode=1 ...
        session.enable_hdr=yes active_av1_mode=3
Encode selection: codec=AV1 (videoFormat=2) client_dynamicRange=1
        is_hdr_display=yes selected_colorspace=HDR (Rec. 2020 + SMPTE 2084 PQ)
        selected_bit_depth=10-bit selected_pix_fmt=p010
        chromaSamplingType=0
Creating encoder [av1_nvenc]
Color coding: HDR (Rec. 2020 + SMPTE 2084 PQ)
Color depth: 10-bit
NvEnc color-config: codec=AV1 primaries=9 transfer=16 matrix=9
        full_range=0 bit_depth=10 yuv444=no
HDR control message (sync session): enabled=1 with SYNTHESIZED HDR10
        defaults (display reported no metadata; ...)
Sent HDR mode control packet to Moonlight: enabled=1
        maxDisplayLuminance=1000 nits minDisplayLuminance=50/10000 nits
        MaxCLL=1000 MaxFALL=400 primaries[R]=(35400,14600)
        whitePoint=(15635,16450)
```

The **last line** is the canonical "Moonlight saw HDR" proof. If it
shows `enabled=1` and the overlay still says SDR, the problem is in
the Moonlight client (overlay caching across reconnects, decoder
negotiation), not in Sunshine.

## Moonlight overlay success string

Look at the Moonlight in-game overlay top bar. It should show, in order:

* Resolution + framerate (e.g. `3840 x 2160 @ 120 FPS`)
* Codec + bit depth + dynamic range: **`AV1 10-bit HDR`**

If it shows `AV1 10-bit SDR` after a real client reconnect, the
control packet is shipping `enabled=0`. Re-run
`clouddeploy-validate-hdr-stream` and look for the negative markers
section.

## Negative markers (must NOT appear)

The validator also flags these. Any single occurrence means HDR is off
even if the positive markers above also matched on stale earlier runs:

* `Color coding: SDR (Rec. 601)` or `Color coding: SDR (Rec. 709)`
* `Sent HDR mode control packet to Moonlight: enabled=0`
* Legacy: `Sent HDR mode: false`, `Sent HDR mode: 0`

## Do not regress

Specifically:

1. Do not require display-side `HDR_OUTPUT_METADATA`. The CloudDeploy
   NVIDIA forced-headless path keeps it `blob 0` deliberately.
2. Do not re-enable the standard connector `Colorspace` property. Same
   reason.
3. Do not bring back `hdr = ${ENABLE_HDR}` in `sunshine.conf`. It is
   not a Sunshine option; it just spams `Unrecognized configurable
   option [hdr]`.
4. Do not relax `setcap` to remove `cap_sys_nice`. The known-good
   live-VM install had it; preserve the exact state.
5. Do not change the synthesised HDR10 defaults to zeros. Some
   decoders short-circuit "MaxCLL=0/MaxFALL=0" to SDR-ish tone mapping
   regardless of the `enabled=1` flag.
