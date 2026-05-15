# NVIDIA Private HDR / PQ path — what we learned

This document captures the empirical findings from the live VM where
KWin/Plasma 6 + NVIDIA 580 + CUDA 13 on Ubuntu 25.10 / Questing finally
produced the "good" HDR-ish state. Most of the result was negative: the
standard connector HDR metadata path is **not** what makes this work.
The result is summarised so that future work can resume without
rediscovering the same dead ends.

## Stable good state

KWin/Plasma Wayland running on real VT/KMS, forced EDID connector alive,
KScreen reporting:

```
HDR: enabled
Wide Color Gamut: enabled
```

Active framebuffer reported by `drm_info` / `kscreen-doctor`:

```
ABGR2101010 10-bit
NVIDIA_BLOCK_LINEAR
```

Active NVIDIA DRM private properties on the primary plane + CRTC:

```
NV_CRTC_REGAMMA_TF       = PQ
NV_INPUT_COLORSPACE      = BT.2100 PQ
NV_PLANE_DEGAMMA_TF      = PQ
NV_HDR_STATIC_METADATA   = blob 0          ← intentionally empty
```

**`NV_HDR_STATIC_METADATA` is intentionally not set.** Setting it broke
the path in every variant we tried. Treat metadata as experimental and
not a deployment prerequisite.

## Standard connector HDR path: failed

Setting the kernel-DRM connector properties via KWin's stock code path
caused NVIDIA's atomic check to reject the modeset:

```
connector HDR_OUTPUT_METADATA blob    → driver rejected the output configuration
connector Colorspace BT2020_RGB       → driver rejected the output configuration
```

KScreen then reported HDR/WCG as **disabled** because KWin only
advertises them after a successful modeset.

## What actually unblocked HDR + WCG

The combination that produced the good state above:

* **Skip standard connector HDR metadata and connector colorspace.** Do
  not set `HDR_OUTPUT_METADATA` and do not set connector `Colorspace`
  to `BT2020_RGB`.
* **Set NVIDIA private CRTC/plane properties instead.** When HDR or
  WCG is requested by the compositor's internal state:
  * CRTC `NV_CRTC_REGAMMA_TF`        = `PQ` (otherwise `Default`)
  * Primary plane `NV_INPUT_COLORSPACE` = `BT.2100 PQ` (otherwise `None`)
  * Primary plane `NV_PLANE_DEGAMMA_TF` = `PQ` (otherwise `Default`)
  * Leave `NV_HDR_STATIC_METADATA` unset (blob 0)
* KScreen and KWin's internal HDR/WCG state still report **enabled**
  because the compositor's HDR/WCG bookkeeping is independent of the
  connector property write.

This requires a KWin patch in `src/backends/drm/drm_pipeline.cpp`
(and related DRM property handling) gated on a runtime/build flag:

```
KWIN_CLOUDDEPLOY_NVIDIA_PRIVATE_HDR=1
KWIN_CLOUDDEPLOY_NVIDIA_PRIVATE_HDR_MODESET_PLANE_PROPS=1
KWIN_CLOUDDEPLOY_NVIDIA_PRIVATE_HDR_METADATA=0           # default
```

## Integration state in CloudDeploy-mover (v2)

This is the current state of the patched-KWin integration in the script.
Update this section whenever the build/patch changes.

| Piece | Status |
|---|---|
| `KWIN_CLOUDDEPLOY_NVIDIA_PRIVATE_HDR*` env vars wired through `/etc/clouddeploy-wayland.env` | **Done** |
| `Environment=KWIN_CLOUDDEPLOY_NVIDIA_PRIVATE_HDR*=1` lines written into `kwin-realvt.service` when `KWIN_CLOUDDEPLOY_NVIDIA_PRIVATE_HDR=1` (and *not* written when 0, so `qEnvironmentVariableIsSet()` in the patched KWin returns false) | **Done** |
| Patch file at [`patches/kwin-clouddeploy-nvidia-private-hdr.patch`](../patches/kwin-clouddeploy-nvidia-private-hdr.patch) | **Done** |
| `build_install_patched_kwin`: enables deb-src, `apt-get build-dep -y kwin`, `apt source kwin`, applies the patch, `DEB_BUILD_OPTIONS="nocheck parallel=$(nproc)" dpkg-buildpackage -us -uc -b`, stops kwin/sunshine, installs the resulting `.deb`s, `apt-mark hold`s them, runs `kwin_wayland --version`, and writes `${PATCHED_KWIN_MARKER}` | **Done** |
| New `patched-kwin` phase wired between `sunshine-build` and `systemd-units` (only when `KWIN_CLOUDDEPLOY_NVIDIA_PRIVATE_HDR=1`) | **Done** |
| Early guard `require_patched_kwin_if_hdr` refuses `ENABLE_HDR=1` when the patched KWin marker is missing | **Done** |
| Post-deploy `validate_hdr_final_state` requires KScreen HDR/WCG + drm_info `NV_CRTC_REGAMMA_TF=PQ` + `NV_INPUT_COLORSPACE=BT.2100 PQ` + `NV_PLANE_DEGAMMA_TF=PQ` and fails loudly on miss | **Done** |
| KWin runtime debug toggle file paths honoured by the patched KWin | **Not in this patch.** The integrated patch is the production baseline (NVIDIA private CRTC/plane props, metadata blob 0). The `/tmp/clouddeploy-*` debug toggles listed below are for a future diagnostic patch and currently have no effect. |

**What you get when `ENABLE_HDR=1` runs to completion**

1. apt source kwin → patch applies → kwin/libkwin .debs are built and installed
2. kwin-realvt.service gets `Environment=KWIN_CLOUDDEPLOY_NVIDIA_PRIVATE_HDR=1` and `Environment=KWIN_CLOUDDEPLOY_NVIDIA_PRIVATE_HDR_MODESET_PLANE_PROPS=1`
3. When KWin runs the atomic modeset, the patched code path skips connector `HDR_OUTPUT_METADATA` and `Colorspace=BT2020_RGB` (which NVIDIA rejects), and instead writes `NV_CRTC_REGAMMA_TF=PQ` on the CRTC and `NV_INPUT_COLORSPACE=BT.2100 PQ` + `NV_PLANE_DEGAMMA_TF=PQ` on the primary plane (with `NV_HDR_STATIC_METADATA` left as blob 0)
4. `validate_hdr_final_state` confirms the documented good state via `kscreen-doctor -o` and `drm_info` and refuses to claim success otherwise.

## Debug toggles (not deployment-required)

These are file-presence runtime toggles the patched KWin honours during
experimentation. They are **not** part of the default deployment path
and exist purely to diagnose metadata behaviour:

| File path | Effect |
|---|---|
| `/tmp/clouddeploy-enable-nv-hdr-metadata`       | Attempt to set `NV_HDR_STATIC_METADATA` from layer metadata. |
| `/tmp/clouddeploy-nv-hdr-safe-metadata`         | Use hardcoded "safe" HDR10 metadata instead of EDID-derived. |
| `/tmp/clouddeploy-skip-nv-crtc-regamma`         | Skip writing `NV_CRTC_REGAMMA_TF`. |
| `/tmp/clouddeploy-nv-plane-degamma-default`     | Force plane degamma to `Default`. |
| `/tmp/clouddeploy-nv-plane-degamma-linear`      | Force plane degamma to `Linear`. |
| `/tmp/clouddeploy-nv-plane-degamma-skip`        | Skip writing `NV_PLANE_DEGAMMA_TF`. |
| `/tmp/clouddeploy-nv-input-colorspace-none`     | Force plane input colorspace to `None`. |
| `/tmp/clouddeploy-nv-input-colorspace-scrgb`    | Force plane input colorspace to `scRGB`. |

Anything that depends on these toggles is by definition not stable.

## Metadata investigation log

Everything in this section is **negative** result. Recording it so future
attempts do not retry the same dead variants.

### KWin-side attempts (all failed):

* EDID-derived metadata → `applyModeSetConfig` atomic check failed.
* Hardcoded "safe" HDR10 metadata → same atomic check failure.
* Metadata combined with `NV_INPUT_COLORSPACE=None`, `NV_CRTC_REGAMMA_TF=Default`, and various plane degamma variants → still failed.
* The blob itself was the correct length (32 bytes) and parsed cleanly
  by NVIDIA's open DRM layer.

### NVIDIA DKMS instrumentation result:

`nv_drm_plane_atomic_set_property` accepted the metadata blob:

```
len=32  expected=32
metadata_type=0
eotf=2
max_lum=1000
min_lum=1
max_cll=1000
max_fall=400
replace ret=0
```

`plane_req_config_update` parsed the blob and set NvKMS HDR metadata
with `outputTf=2`. The failure point was later, inside
`nvKms->applyModeSetConfig`, at the atomic check stage — the metadata
made it into NvKMS state but the modeset itself was rejected.

### NvKMS ablation matrix:

| Variant | Result |
|---|---|
| head-infoframe-only-colorimetry-bt2100   | fail |
| head-infoframe-only-colorimetry-default  | fail |
| head-infoframe-active-heads-only         | fail |
| clear-all-hdr-metadata                   | sometimes pass |
| layer-metadata-only-no-head-infoframe    | pass (later ablation) |
| layer-metadata-only-no-outputtf          | pass (later ablation) |

The InfoFrame mirror hack (mirroring layer metadata into the head's HDR
InfoFrame) actively made things worse and should be removed from any
future attempt.

## Best next metadata experiment

If we ever resume the metadata path:

1. **Remove the head InfoFrame mirror hack.** It correlated with failure.
2. Test **layer metadata only, no head InfoFrame**. This passed in
   ablation and is the most promising lead.
3. If that still fails, force `layer.hdrMetadata.enabled=true` but
   `outputTf=NVKMS_OUTPUT_TF_NONE`. `layer-metadata-only-no-outputtf`
   passed in ablation.
4. Validate via `NV_HDR_STATIC_METADATA != blob 0` on the active plane
   and confirm Moonlight reports HDR on the client.
5. **Do not make this part of normal deployment** until both of those
   conditions hold reliably across reboots.

Gated by:

```
CLOUDDEPLOY_EXPERIMENTAL_NVIDIA_DKMS_HDR_METADATA=1
```

The matching DKMS patches (`nvidia-drm-crtc.c` debug logging, the
InfoFrame mirror experiment, the head-only hack, the ablation probe)
were diagnostic and are not bundled. They should not be re-introduced
to the default deployment unless they actually produce a working
metadata path.

## Validation commands for the good state

After deploy, the following should hold simultaneously:

```bash
# KScreen reports HDR/WCG enabled
kscreen-doctor -o | grep -E 'HDR:|Wide Color Gamut:'

# KWin agrees
qdbus org.kde.KWin /KWin org.kde.KWin.supportInformation \
  | grep -E '^(Name:|Geometry:|Refresh Rate:|HDR|Wide Color Gamut)'

# drm_info shows the framebuffer + NVIDIA private props
drm_info | grep -E 'ABGR2101010|NVIDIA_BLOCK_LINEAR|NV_CRTC_REGAMMA_TF|NV_INPUT_COLORSPACE|NV_PLANE_DEGAMMA_TF|NV_HDR_STATIC_METADATA'

# Sunshine is up and streams via KMS/DRM/Wayland/NVENC
systemctl is-active sunshine-headless.service
curl -fsS http://127.0.0.1:47989/serverinfo | head -c 200
```

`NV_HDR_STATIC_METADATA = blob 0` is **expected** in the good state.
That is not a regression.
