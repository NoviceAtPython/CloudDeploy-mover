from pathlib import Path

OUTDIR = Path('/lib/firmware/edid')
OUTDIR.mkdir(parents=True, exist_ok=True)


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


def base_block(name: str) -> bytes:
    b = bytearray(128)
    b[0:8] = b'\x00\xff\xff\xff\xff\xff\xff\x00'
    b[8:10] = mfg_id('CDP')
    b[10:12] = (0x0420).to_bytes(2, 'little')
    b[12:16] = (0x00000001).to_bytes(4, 'little')
    b[16] = 1
    b[17] = 0
    b[18] = 1
    b[19] = 4
    b[20] = 0xA5
    b[21] = 0x3C
    b[22] = 0x22
    b[23] = 0x78
    b[24] = 0x0A
    for i in range(25, 35):
        b[i] = 0
    b[35:38] = b'\x00\x00\x00'
    for i in range(38, 54, 2):
        b[i:i + 2] = b'\x01\x01'
    b[54:72] = detailed_timing(148500, 1920, 280, 1080, 45, 88, 44, 4, 5, 600, 340)
    b[72:90] = descriptor(0xFC, name)
    b[90:108] = range_limits(48, 144, 30, 255)
    b[108:126] = descriptor(0xFE, 'CloudDeploy EDID')
    b[126] = 1
    return bytes(checksum(b))


def cta_block(vics, hdr=False) -> bytes:
    data = bytearray()
    data.extend([0x40 | len(vics), *vics])
    if hdr:
        data.extend([0x73, 0x05, 0xC0, 0x00])
        data.extend([0x76, 0x06, 0x0D, 0x01, 100, 80, 1])

    ext = bytearray(128)
    ext[0] = 0x02
    ext[1] = 0x03
    ext[2] = 4 + len(data)
    ext[3] = 0x00
    ext[4:4 + len(data)] = data
    return bytes(checksum(ext))


def write_profile(filename: str, name: str, vics, hdr=False):
    blob = base_block(name) + cta_block(vics=vics, hdr=hdr)
    (OUTDIR / filename).write_bytes(blob)


write_profile('virtual-1080p-sdr.bin', 'CloudDeploy 1080p', [16], hdr=False)
write_profile('virtual-4k60-sdr.bin', 'CloudDeploy 4K60', [97], hdr=False)
write_profile('virtual-4k120-sdr.bin', 'CloudDeploy 4K120', [118, 97], hdr=False)
write_profile('virtual-4k120-hdr.bin', 'CloudDeploy 4K120 HDR', [118, 97], hdr=True)
