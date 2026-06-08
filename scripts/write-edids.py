#!/usr/bin/env python3
"""Generate CloudDeploy forced-EDID blobs under /lib/firmware/edid.

CloudDeploy drives a *virtual* display: the VM has no real monitor, so the
NVIDIA driver + KWin learn what modes the "panel" supports from a forced
EDID injected via the kernel cmdline (drm.edid_firmware=DP-1:edid/<file>).

This script emits two **universal multi-mode** EDIDs --

    virtual-universal-sdr.bin   720p / 1080p / 1200p / 1440p / 4K @ 60+120, SDR
    virtual-universal-hdr.bin   same modes, plus CTA HDR static-metadata (PQ/HLG)

-- so that a single deploy can run any supported resolution (chosen at deploy
time via the profile + clouddeploy-force-kwin-mode.sh) and the in-VM KDE
display settings actually show real options.

It also still emits the legacy single-mode SKUs (virtual-4k120-hdr.bin etc.)
for backward compatibility with older profiles/cmdlines during the transition.

Mode-advertisement strategy (EDID is byte-budget constrained):
  * CTA-861 VICs for modes that have one: 720p60(4), 1080p60(16),
    1080p120(63), 4K60(97), 4K120(118).
  * Standard-timing entries for 1080p60 / 1200p60 / 720p60.
  * Detailed-timing descriptors (CVT reduced-blanking) for the PC modes that
    have NO CTA VIC: 1440p60, 1440p120, 1200p60, 1200p120, 720p120.
  * 4K120's pixel clock (~1.19 GHz CEA / ~1.10 GHz CVT-RB) EXCEEDS the EDID
    detailed-timing 16-bit pixel-clock field (max 655.35 MHz), so 4K120 can
    ONLY be advertised as CTA VIC 118 -- never a detailed timing.

The script self-validates: after writing each blob it re-parses the bytes and
asserts every target mode is discoverable (as VIC, standard timing, or DTD).

Run on the VM as:   sudo python3 write-edids.py
Off-box self-test:  CLOUDDEPLOY_EDID_OUTDIR=/tmp/edids python3 write-edids.py
"""

import math
import os
import re
import shutil
import subprocess
import sys
from pathlib import Path

OUTDIR = Path(os.environ.get("CLOUDDEPLOY_EDID_OUTDIR", "/lib/firmware/edid"))

# (width, height, refresh) the universal EDIDs must advertise.
TARGET_MODES = [
    (1280, 720, 60), (1280, 720, 120),
    (1920, 1080, 60), (1920, 1080, 120),
    (1920, 1200, 60), (1920, 1200, 120),
    (2560, 1440, 60), (2560, 1440, 120),
    (3840, 2160, 60), (3840, 2160, 120),
    # High-refresh extras. Only modes whose CVT-RB pixel clock fits the EDID
    # detailed-timing field (<= 655 MHz) are expressible here: 1080p144 (~325),
    # 1080p240 (~533), 1440p144 (~591). 1440p240 (~985) and 4K>120 exceed it
    # and 1440p/non-CTA resolutions have no VIC. MUST stay in sync with the
    # extraModes map in internal/edid/edid.go.
    (1920, 1080, 144), (1920, 1080, 240),
    (2560, 1440, 144),
]

# CTA VIC -> (w, h, refresh) for the SVDs we emit + the self-check decoder.
VIC_MODES = {
    4: (1280, 720, 60),
    16: (1920, 1080, 60),
    63: (1920, 1080, 120),
    97: (3840, 2160, 60),
    118: (3840, 2160, 120),
}


# -----------------------------------------------------------------------------
# low-level EDID helpers (mfg_id / checksum / detailed_timing / descriptor /
# range_limits are byte-for-byte the proven v2 helpers)
# -----------------------------------------------------------------------------

def mfg_id(code: str) -> bytes:
    code = code.upper()
    value = ((ord(code[0]) - 64) << 10) | ((ord(code[1]) - 64) << 5) | (ord(code[2]) - 64)
    return value.to_bytes(2, 'big')


def checksum(block: bytearray) -> bytearray:
    block[127] = (-sum(block[:127])) & 0xFF
    return block


def detailed_timing(pixel_clock_khz: int, hact: int, hblank: int, vact: int, vblank: int,
                    hsync_off: int, hsync_width: int, vsync_off: int, vsync_width: int,
                    hsize_mm: int, vsize_mm: int) -> bytes:
    pclk = pixel_clock_khz // 10
    if pclk > 0xFFFF:
        raise ValueError(
            f"detailed timing pixel clock {pixel_clock_khz} kHz exceeds the "
            f"EDID 16-bit field (max 655350 kHz); advertise this mode as a CTA VIC instead")
    d = bytearray(18)
    d[0:2] = pclk.to_bytes(2, 'little')
    d[2] = hact & 0xFF
    d[3] = hblank & 0xFF
    d[4] = ((hact >> 8) & 0xF) << 4 | ((hblank >> 8) & 0xF)
    d[5] = vact & 0xFF
    d[6] = vblank & 0xFF
    d[7] = ((vact >> 8) & 0xF) << 4 | ((vblank >> 8) & 0xF)
    d[8] = hsync_off & 0xFF
    d[9] = hsync_width & 0xFF
    d[10] = ((vsync_off & 0xF) << 4) | (vsync_width & 0xF)
    d[11] = (((hsync_off >> 8) & 0x3) << 6) | (((hsync_width >> 8) & 0x3) << 4) | (((vsync_off >> 4) & 0x3) << 2) | ((vsync_width >> 4) & 0x3)
    d[12] = hsize_mm & 0xFF
    d[13] = vsize_mm & 0xFF
    d[14] = ((hsize_mm >> 8) & 0xF) << 4 | ((vsize_mm >> 8) & 0xF)
    d[15] = 0
    d[16] = 0
    d[17] = 0x1A
    return bytes(d)


def descriptor(tag: int, text: str) -> bytes:
    d = bytearray(18)
    d[0:3] = b'\x00\x00\x00'
    d[3] = tag
    d[4] = 0x00
    payload = text.encode('ascii', 'ignore')[:13]
    d[5:5 + len(payload)] = payload
    if len(payload) < 13:
        d[5 + len(payload)] = 0x0A
    return bytes(d)


def range_limits(vmin: int, vmax: int, hmin: int, hmax: int) -> bytes:
    d = bytearray(18)
    d[0:3] = b'\x00\x00\x00'
    d[3] = 0xFD
    d[4] = 0x00
    d[5] = vmin
    d[6] = vmax
    d[7] = hmin
    d[8] = hmax
    d[9] = 0xFF
    return bytes(d)


# -----------------------------------------------------------------------------
# CVT reduced-blanking (v1) timing generator -- matches libxcvt / the `cvt -r`
# tool. Verified: 2560x1440@60 -> 241.50 MHz, 2560x1440@120 -> 497.75 MHz.
# -----------------------------------------------------------------------------

def cvt_rb(hdisp: int, vdisp: int, refresh: float) -> dict:
    CLOCK_STEP = 0.25      # MHz
    RB_MIN_VBLANK = 460.0  # us
    RB_HBLANK = 160        # pixels
    RB_HSYNC = 32          # pixels
    RB_VFPORCH = 3         # lines
    MIN_VBPORCH = 6        # lines

    ratio = hdisp / vdisp
    if abs(ratio - 4 / 3) < 0.05:
        vsync = 4
    elif abs(ratio - 16 / 9) < 0.05:
        vsync = 5
    elif abs(ratio - 16 / 10) < 0.05:
        vsync = 6
    elif abs(ratio - 5 / 4) < 0.05:
        vsync = 7
    else:
        vsync = 10

    h_period_est = (1_000_000.0 / refresh - RB_MIN_VBLANK) / vdisp
    vbi_lines = math.floor(RB_MIN_VBLANK / h_period_est) + 1
    rb_min_vbi = RB_VFPORCH + vsync + MIN_VBPORCH
    act_vbi = max(vbi_lines, rb_min_vbi)

    total_v_lines = act_vbi + vdisp
    total_pixels = hdisp + RB_HBLANK
    freq = refresh * total_v_lines * total_pixels / 1_000_000.0  # MHz
    freq = math.floor(freq / CLOCK_STEP) * CLOCK_STEP
    pclk_khz = int(round(freq * 1000))

    return dict(
        pclk_khz=pclk_khz,
        hblank=RB_HBLANK,
        vblank=act_vbi,
        hsync_off=(RB_HBLANK // 2 - RB_HSYNC),  # 48
        hsync_width=RB_HSYNC,                   # 32
        vsync_off=RB_VFPORCH,                   # 3
        vsync_width=vsync,
    )


def size_mm(w: int, h: int) -> tuple:
    # Nominal ~600 mm wide panel; height scaled by the mode aspect. Cosmetic.
    return (600, int(round(600 * h / w)))


def dtd_for(w: int, h: int, refresh: int) -> bytes:
    t = cvt_rb(w, h, refresh)
    hs_mm, vs_mm = size_mm(w, h)
    return detailed_timing(
        t['pclk_khz'], w, t['hblank'], h, t['vblank'],
        t['hsync_off'], t['hsync_width'], t['vsync_off'], t['vsync_width'],
        hs_mm, vs_mm)


# Proven CEA 4K60 detailed timing (594 MHz) reused as the preferred timing.
DTD_4K60 = detailed_timing(594000, 3840, 560, 2160, 90, 176, 88, 8, 10, 600, 340)
DTD_1080P60 = detailed_timing(148500, 1920, 280, 1080, 45, 88, 44, 4, 5, 600, 340)


def std_timing(w: int, h: int, refresh: int) -> bytes:
    aspect_bits = {(16, 10): 0, (4, 3): 1, (5, 4): 2, (16, 9): 3}
    g = math.gcd(w, h)
    key = (w // g, h // g)
    if key == (8, 5):
        key = (16, 10)
    ab = aspect_bits.get(key)
    if ab is None or not (256 <= w <= 2288):
        raise ValueError(f"{w}x{h} cannot be encoded as an EDID standard timing")
    b0 = (w // 8) - 31
    b1 = (ab << 6) | ((refresh - 60) & 0x3F)
    return bytes([b0, b1])


# -----------------------------------------------------------------------------
# block assembly
# -----------------------------------------------------------------------------

def base_block(name: str, preferred_dtd: bytes, extra_dtd: bytes, hdr: bool, num_extensions: int = 1) -> bytes:
    b = bytearray(128)
    b[0:8] = b'\x00\xff\xff\xff\xff\xff\xff\x00'
    b[8:10] = mfg_id('CDP')
    b[10:12] = (0x0420).to_bytes(2, 'little')
    b[12:16] = (0x00000001).to_bytes(4, 'little')
    b[16] = 0            # week unused
    b[17] = 35           # year 2025 (value + 1990)
    b[18] = 1
    b[19] = 4            # EDID 1.4
    # Digital input, DisplayPort (0x5). 10 bpc for HDR, 8 bpc for SDR.
    b[20] = 0xB5 if hdr else 0xA5
    b[21] = 0x3C         # max horizontal image size (cm)
    b[22] = 0x22         # max vertical image size (cm)
    b[23] = 0x78         # gamma 2.2
    b[24] = 0x0A         # feature: RGB+YCrCb444, preferred timing is native
    # chromaticity 25-34 left zero (matches the proven v2 EDID)
    # established timings: 640x480@60, 800x600@60, 1024x768@60
    b[35] = 0x21
    b[36] = 0x08
    b[37] = 0x00
    # standard timings (8 slots): 1080p60, 1200p60, 720p60, rest unused.
    st = std_timing(1920, 1080, 60) + std_timing(1920, 1200, 60) + std_timing(1280, 720, 60)
    b[38:38 + len(st)] = st
    for i in range(38 + len(st), 54):
        b[i] = 0x01
    # four 18-byte descriptors
    b[54:72] = preferred_dtd
    b[72:90] = range_limits(48, 144, 30, 255)
    b[90:108] = descriptor(0xFC, name)
    b[108:126] = extra_dtd
    b[126] = num_extensions  # number of extension blocks that follow
    return bytes(checksum(b))


def cta_block(vics: list, dtds: list, hdr: bool) -> bytes:
    data = bytearray()
    # Video Data Block (type 2): SVDs. Omitted when there are no VICs (the
    # second extension block carries only high-refresh detailed timings).
    if vics:
        data.extend([0x40 | len(vics), *[v & 0xFF for v in vics]])
    if hdr:
        # Extended Colorimetry Data Block (ext tag 0x05): BT.2020 RGB/YCC/cYCC.
        data.extend([0xE3, 0x05, 0xE0, 0x00])
        # Extended HDR Static Metadata Data Block (ext tag 0x06):
        # EOTF bits = SDR + Traditional-HDR + PQ/ST2084 + HLG; SM type 1.
        data.extend([0xE6, 0x06, 0x0F, 0x01, 100, 80, 1])
    dtd_offset = 4 + len(data)
    if dtd_offset + 18 * len(dtds) > 127:
        raise ValueError("CTA extension block overflow: too many DTDs for the data blocks present")
    ext = bytearray(128)
    ext[0] = 0x02                 # CTA-861 extension tag
    ext[1] = 0x03                 # revision 3
    ext[2] = dtd_offset           # byte offset to first DTD (0 = none)
    ext[3] = len(dtds) & 0x0F     # number of native DTDs, no YCbCr/audio flags
    ext[4:4 + len(data)] = data
    pos = dtd_offset
    for d in dtds:
        ext[pos:pos + 18] = d
        pos += 18
    return bytes(checksum(ext))


def assemble_universal(hdr: bool) -> bytes:
    name = 'CloudDeploy HDR' if hdr else 'CloudDeploy SDR'
    base = base_block(
        name,
        preferred_dtd=DTD_4K60,
        extra_dtd=dtd_for(2560, 1440, 120),
        hdr=hdr,
        num_extensions=2,
    )
    vics = [118, 97, 63, 16, 4]
    dtds = [
        dtd_for(2560, 1440, 60),
        dtd_for(1920, 1200, 120),
        dtd_for(1920, 1200, 60),
        dtd_for(1280, 720, 120),
    ]
    # High-refresh DTDs go in a SECOND CTA extension block: the first block is
    # near its 127-byte DTD budget (especially with the HDR data blocks), so
    # the extras (1080p144/240, 1440p144) overflow it. These resolutions have
    # no CTA VIC, so they must be detailed timings, and 1440p240 / 4K>120 are
    # intentionally excluded (their pixel clock exceeds the EDID timing field).
    hi_refresh_dtds = [
        dtd_for(1920, 1080, 144),
        dtd_for(1920, 1080, 240),
        dtd_for(2560, 1440, 144),
    ]
    return base + cta_block(vics, dtds, hdr) + cta_block([], hi_refresh_dtds, False)


def assemble_legacy(name: str, vics: list, preferred_dtd: bytes, hdr: bool) -> bytes:
    base = base_block(name, preferred_dtd=preferred_dtd, extra_dtd=descriptor(0xFE, 'CloudDeploy'), hdr=hdr)
    return base + cta_block(vics, [], hdr)


# -----------------------------------------------------------------------------
# self-validation: re-parse the blob and confirm every target mode is present
# -----------------------------------------------------------------------------

def _decode_dtd(d: bytes):
    pclk = (d[1] << 8 | d[0]) * 10_000  # Hz
    if pclk == 0:
        return None
    hact = d[2] | ((d[4] >> 4) << 8)
    hblank = d[3] | ((d[4] & 0xF) << 8)
    vact = d[5] | ((d[7] >> 4) << 8)
    vblank = d[6] | ((d[7] & 0xF) << 8)
    htot, vtot = hact + hblank, vact + vblank
    if htot == 0 or vtot == 0:
        return None
    return (hact, vact, round(pclk / (htot * vtot)))


def _decode_std(b0: int, b1: int):
    if (b0, b1) == (0x01, 0x01) or b0 == 0:
        return None
    w = (b0 + 31) * 8
    ar = (b1 >> 6) & 0x3
    h = {0: w * 10 // 16, 1: w * 3 // 4, 2: w * 4 // 5, 3: w * 9 // 16}[ar]
    return (w, h, (b1 & 0x3F) + 60)


def discovered_modes(blob: bytes) -> set:
    found = set()
    # base detailed timings
    for off in (54, 72, 90, 108):
        d = blob[off:off + 18]
        if d[0] or d[1]:
            dd = _decode_dtd(d)
            if dd:
                found.add(dd)
    # base standard timings
    for off in range(38, 54, 2):
        ss = _decode_std(blob[off], blob[off + 1])
        if ss:
            found.add(ss)
    # every CTA extension block (the universal EDID uses two)
    for n in range(blob[126]):
        base = 128 * (1 + n)
        if base + 128 > len(blob) or blob[base] != 0x02:
            continue
        ext = blob[base:base + 128]
        dtd_off = ext[2]
        i = 4
        while 0 < i < dtd_off and i < 127:
            tag = ext[i]
            typ, ln = tag >> 5, tag & 0x1F
            if typ == 2:  # video data block -> SVDs
                for v in ext[i + 1:i + 1 + ln]:
                    vic = v & 0x7F
                    if vic in VIC_MODES:
                        found.add(VIC_MODES[vic])
            i += 1 + ln
        p = dtd_off
        while dtd_off and p + 18 <= 127 and (ext[p] or ext[p + 1]):
            dd = _decode_dtd(ext[p:p + 18])
            if dd:
                found.add(dd)
            p += 18
    return found


def self_check(blob: bytes, label: str):
    num_ext = blob[126]
    expected = 128 * (1 + num_ext)
    if len(blob) != expected:
        raise SystemExit(f"{label}: expected {expected}-byte blob ({1 + num_ext} blocks), got {len(blob)}")
    for i in range(1 + num_ext):
        block = blob[128 * i:128 * (i + 1)]
        if (sum(block) & 0xFF) != 0:
            raise SystemExit(f"{label}: block {i} checksum invalid")
    found = discovered_modes(blob)

    def present(w, h, r):
        return any(fw == w and fh == h and abs(fr - r) <= 2 for (fw, fh, fr) in found)

    missing = [(w, h, r) for (w, h, r) in TARGET_MODES if not present(w, h, r)]
    if missing:
        pretty = ", ".join(f"{w}x{h}@{r}" for (w, h, r) in sorted(found))
        raise SystemExit(
            f"{label}: missing target modes {missing}; discovered only: {pretty}")
    print(f"{label}: OK ({len(found)} modes; all {len(TARGET_MODES)} targets advertised)")


# -----------------------------------------------------------------------------
# optional edid-decode cross-check (only when the tool is installed, i.e. VM)
# -----------------------------------------------------------------------------

def edid_decode_hdr_check(path: Path):
    if not shutil.which('edid-decode'):
        print(f"edid-decode not found; HDR metadata cross-check skipped for {path}")
        return
    decoded = subprocess.run(
        ['edid-decode', str(path)], check=False,
        stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True).stdout
    pattern = re.compile(r'HDR|EOTF|PQ|HLG|BT[.]2020|Static Metadata|SMPTE ST 2084', re.IGNORECASE)
    matches = [ln for ln in decoded.splitlines() if pattern.search(ln)]
    if not matches:
        raise SystemExit(f"{path} did not decode with HDR/BT.2020 CTA metadata")
    print(f"{path}: edid-decode HDR metadata present")


def write_blob(filename: str, blob: bytes):
    (OUTDIR / filename).write_bytes(blob)


def main():
    OUTDIR.mkdir(parents=True, exist_ok=True)

    universal_sdr = assemble_universal(hdr=False)
    universal_hdr = assemble_universal(hdr=True)
    self_check(universal_sdr, "virtual-universal-sdr.bin")
    self_check(universal_hdr, "virtual-universal-hdr.bin")
    write_blob('virtual-universal-sdr.bin', universal_sdr)
    write_blob('virtual-universal-hdr.bin', universal_hdr)

    # Legacy single-mode SKUs (kept for backward compatibility with older
    # profiles + cmdlines during the multi-resolution transition).
    write_blob('virtual-1080p-sdr.bin', assemble_legacy('CloudDeploy 1080p', [16], DTD_1080P60, hdr=False))
    write_blob('virtual-4k60-sdr.bin', assemble_legacy('CloudDeploy 4K60', [97], DTD_4K60, hdr=False))
    write_blob('virtual-4k120-sdr.bin', assemble_legacy('CloudDeploy 4K120', [118, 97], DTD_4K60, hdr=False))
    write_blob('virtual-4k120-hdr.bin', assemble_legacy('CloudDeploy 4K120 HDR', [118, 97], DTD_4K60, hdr=True))

    edid_decode_hdr_check(OUTDIR / 'virtual-universal-hdr.bin')
    print(f"wrote EDID blobs to {OUTDIR}")


if __name__ == '__main__':
    sys.exit(main())
