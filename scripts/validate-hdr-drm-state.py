#!/usr/bin/env python3
"""Validate the NVIDIA private HDR DRM state on the active CRTC + active
primary plane driving a given connector.

Reads `drm_info -j` JSON on stdin. Argv[1] is the connector name (e.g. DP-1,
HDMI-A-2). The script:

  1. Locates the connector by name across all devices in the JSON.
  2. Follows connector -> encoder -> CRTC to find the *active* CRTC that
     is actually driving that connector. An active CRTC has a non-zero
     fb_id (or, as a fallback, a non-empty mode).
  3. Locates the primary plane assigned to that CRTC (matching crtc_id
     and type == "primary"). The primary plane must itself be active
     (non-zero fb_id).
  4. Verifies the expected NVIDIA private DRM properties:
        active CRTC:           NV_CRTC_REGAMMA_TF   = PQ
        active primary plane:  NV_INPUT_COLORSPACE  = BT.2100 PQ
                               NV_PLANE_DEGAMMA_TF  = PQ
     NV_HDR_STATIC_METADATA is *not* required - blob 0 is the documented
     good state and any non-empty metadata is noted but not a failure.

Exits 0 only when every required property checks out on the *active*
CRTC and *active* primary plane. Anything else exits non-zero and prints
the specific mismatch to stderr.

This is intentionally narrow: it does not grep the global drm_info dump.
A property like NV_CRTC_REGAMMA_TF=PQ on an unused CRTC must not be
allowed to fool the check.
"""
from __future__ import annotations

import json
import sys
from typing import Any, Iterable, Optional, Tuple

# DRM_MODE_CONNECTOR_* enum -> short type prefix used in connector names.
# This matches Linux drm/drm_mode.h ordering and the canonical names used
# in /sys/class/drm/cardN-<NAME>.
NUMERIC_CONNECTOR_TYPE: dict[int, str] = {
    0: "Unknown",
    1: "VGA",
    2: "DVI-I",
    3: "DVI-D",
    4: "DVI-A",
    5: "Composite",
    6: "SVIDEO",
    7: "LVDS",
    8: "Component",
    9: "DIN",
    10: "DP",
    11: "HDMI-A",
    12: "HDMI-B",
    13: "TV",
    14: "eDP",
    15: "Virtual",
    16: "DSI",
    17: "DPI",
    18: "Writeback",
    19: "SPI",
    20: "USB",
}

PRIMARY_PLANE_TYPE_INT = 1  # DRM_PLANE_TYPE_PRIMARY


def _to_int(value: Any) -> Optional[int]:
    try:
        return int(value)
    except (TypeError, ValueError):
        return None


def enum_value_name(prop: Any) -> Optional[str]:
    """Return the textual enum value (e.g. "PQ") for an enum-typed
    property, or None when the property is missing/non-enum/empty.

    drm_info -j shapes the property roughly as::

        {
          "spec": {"type": "enum", "values": [{"name": "PQ", "value": 2}, ...]},
          "value": 2
        }
    """
    if not isinstance(prop, dict):
        return None
    raw_value = prop.get("value")
    if raw_value is None:
        return None
    raw_int = _to_int(raw_value)
    spec = prop.get("spec") or {}
    values = spec.get("values") or spec.get("enums") or []
    for entry in values:
        if not isinstance(entry, dict):
            continue
        entry_value = _to_int(entry.get("value"))
        if entry_value is not None and entry_value == raw_int:
            name = entry.get("name")
            if isinstance(name, str):
                return name
    # Some drm_info versions emit the enum name directly under "value".
    if isinstance(raw_value, str):
        return raw_value
    return None


def connector_name(conn_id: str, conn_data: dict) -> str:
    """Best-effort reconstruct the connector name (e.g. DP-1, HDMI-A-2).

    Different drm_info versions stash the name in slightly different
    places; cover the variants we have seen.
    """
    direct = conn_data.get("name") or conn_data.get("connector_name")
    if isinstance(direct, str) and direct:
        return direct

    type_field = conn_data.get("type")
    type_id = conn_data.get("type_id") or conn_data.get("connector_type_id")
    type_name: Optional[str] = None
    if isinstance(type_field, str) and type_field:
        type_name = type_field
        # Some drm_info builds emit "DisplayPort" rather than "DP".
        if type_name.lower() == "displayport":
            type_name = "DP"
    elif isinstance(type_field, int):
        type_name = NUMERIC_CONNECTOR_TYPE.get(type_field)
    elif isinstance(type_field, dict):
        # Sometimes type is {"value": 10, "name": "DisplayPort"}.
        inner = type_field.get("name") or type_field.get("value")
        if isinstance(inner, str):
            type_name = inner if inner.lower() != "displayport" else "DP"
        elif isinstance(inner, int):
            type_name = NUMERIC_CONNECTOR_TYPE.get(inner)

    if type_name and type_id is not None:
        return f"{type_name}-{type_id}"
    if type_name:
        return f"{type_name}-{conn_id}"
    return f"connector-{conn_id}"


def crtc_is_active(crtc: dict) -> bool:
    """An active CRTC is one that's driving a scanout: fb_id != 0, or
    has a mode object that looks like a real mode (non-zero clock)."""
    fb_id = _to_int(crtc.get("fb_id"))
    if fb_id is not None and fb_id != 0:
        return True
    mode = crtc.get("mode")
    if isinstance(mode, dict):
        clock = _to_int(mode.get("clock"))
        if clock is not None and clock != 0:
            return True
        # Some drm_info versions embed a "name" string when the mode is set.
        if mode.get("name"):
            return True
    # mode_valid flag exists in some versions.
    if crtc.get("mode_valid") is True:
        return True
    return False


def plane_is_primary(plane: dict) -> bool:
    """Return True iff the plane is the primary plane.

    drm_info -j sometimes exposes the plane type as a top-level "type"
    string ("primary"/"overlay"/"cursor"), sometimes as a property in
    plane.properties.type, sometimes as a numeric value (1 = primary)."""
    top_type = plane.get("type")
    if isinstance(top_type, str):
        return top_type.lower() == "primary"
    type_prop = (plane.get("properties") or {}).get("type")
    enum_name = enum_value_name(type_prop)
    if enum_name:
        return enum_name.lower() == "primary"
    if isinstance(type_prop, dict):
        raw = _to_int(type_prop.get("value"))
        if raw is not None:
            return raw == PRIMARY_PLANE_TYPE_INT
    return False


def plane_is_active(plane: dict) -> bool:
    fb_id = _to_int(plane.get("fb_id"))
    if fb_id is not None and fb_id != 0:
        return True
    # Some drm_info shapes nest fb under properties.
    fb_prop = (plane.get("properties") or {}).get("FB_ID")
    if isinstance(fb_prop, dict):
        raw = _to_int(fb_prop.get("value"))
        if raw is not None and raw != 0:
            return True
    return False


def find_active_crtc_for_connector(device: dict, conn_id: str, conn: dict
                                   ) -> Tuple[Optional[str], Optional[dict], list[str]]:
    """Follow connector -> encoder -> CRTC and return (crtc_id, crtc_obj,
    diagnostics). diagnostics is a list of human-readable strings about
    why a candidate was rejected."""
    diagnostics: list[str] = []
    encoders = device.get("encoders") or {}
    crtcs = device.get("crtcs") or {}

    encoder_id = conn.get("encoder_id")
    if not encoder_id:
        diagnostics.append(
            f"connector {conn_id} ({conn.get('status', 'unknown')}) has no encoder_id"
        )
        return None, None, diagnostics

    enc = encoders.get(str(encoder_id)) or encoders.get(encoder_id) or {}
    crtc_id = enc.get("crtc_id")
    if not crtc_id:
        diagnostics.append(
            f"encoder {encoder_id} for connector {conn_id} has no crtc_id"
        )
        return None, None, diagnostics

    crtc = crtcs.get(str(crtc_id)) or crtcs.get(crtc_id) or {}
    if not crtc:
        diagnostics.append(
            f"CRTC {crtc_id} (target of encoder {encoder_id}) not found in drm_info"
        )
        return None, None, diagnostics

    if not crtc_is_active(crtc):
        diagnostics.append(
            f"CRTC {crtc_id} (target of encoder {encoder_id}) is not active "
            f"(fb_id={crtc.get('fb_id')}, mode={crtc.get('mode')})"
        )
        return None, None, diagnostics

    return str(crtc_id), crtc, diagnostics


def find_active_primary_plane(device: dict, crtc_id: str
                              ) -> Tuple[Optional[str], Optional[dict], list[str]]:
    diagnostics: list[str] = []
    planes = device.get("planes") or {}
    target_crtc_int = _to_int(crtc_id)

    for pid, pdata in planes.items():
        if not isinstance(pdata, dict):
            continue
        plane_crtc = _to_int(pdata.get("crtc_id"))
        if plane_crtc is None or plane_crtc != target_crtc_int:
            continue
        if not plane_is_primary(pdata):
            continue
        if not plane_is_active(pdata):
            diagnostics.append(
                f"primary plane {pid} on CRTC {crtc_id} is not active (fb_id={pdata.get('fb_id')})"
            )
            continue
        return str(pid), pdata, diagnostics

    diagnostics.append(f"no active primary plane found assigned to CRTC {crtc_id}")
    return None, None, diagnostics


def check(target_connector: str, drm: dict) -> int:
    rc = 0
    matched_device = False

    for device, dev_data in drm.items():
        if not isinstance(dev_data, dict):
            continue
        connectors = dev_data.get("connectors") or {}

        target_conn_id: Optional[str] = None
        target_conn: Optional[dict] = None
        for cid, cdata in connectors.items():
            if not isinstance(cdata, dict):
                continue
            name = connector_name(cid, cdata)
            if name == target_connector:
                target_conn_id = str(cid)
                target_conn = cdata
                break

        if target_conn is None:
            continue

        matched_device = True

        crtc_id, crtc, diags = find_active_crtc_for_connector(
            dev_data, target_conn_id, target_conn
        )
        if crtc is None:
            for d in diags:
                print(f"FAIL[{device}]: {d}", file=sys.stderr)
            return 1

        crtc_props = crtc.get("properties") or {}
        regamma = enum_value_name(crtc_props.get("NV_CRTC_REGAMMA_TF"))
        if regamma != "PQ":
            print(
                f"FAIL[{device}]: active CRTC {crtc_id} (driving {target_connector}) "
                f"NV_CRTC_REGAMMA_TF={regamma!r} (expected 'PQ')",
                file=sys.stderr,
            )
            rc = 1
        else:
            print(f"OK[{device}]: active CRTC {crtc_id} NV_CRTC_REGAMMA_TF=PQ")

        plane_id, plane, plane_diags = find_active_primary_plane(dev_data, crtc_id)
        if plane is None:
            for d in plane_diags:
                print(f"FAIL[{device}]: {d}", file=sys.stderr)
            return 1

        plane_props = plane.get("properties") or {}

        # NVIDIA names BT.2100 either with or without the dot depending on
        # nvidia-drm version. Accept both spellings.
        input_cs = enum_value_name(plane_props.get("NV_INPUT_COLORSPACE"))
        if input_cs not in {"BT.2100 PQ", "BT2100 PQ"}:
            print(
                f"FAIL[{device}]: active primary plane {plane_id} "
                f"NV_INPUT_COLORSPACE={input_cs!r} (expected 'BT.2100 PQ')",
                file=sys.stderr,
            )
            rc = 1
        else:
            print(f"OK[{device}]: active primary plane {plane_id} NV_INPUT_COLORSPACE={input_cs}")

        degamma = enum_value_name(plane_props.get("NV_PLANE_DEGAMMA_TF"))
        if degamma != "PQ":
            print(
                f"FAIL[{device}]: active primary plane {plane_id} "
                f"NV_PLANE_DEGAMMA_TF={degamma!r} (expected 'PQ')",
                file=sys.stderr,
            )
            rc = 1
        else:
            print(f"OK[{device}]: active primary plane {plane_id} NV_PLANE_DEGAMMA_TF=PQ")

        # NV_HDR_STATIC_METADATA: blob 0 is the documented good state.
        # Non-zero is noted but not a failure (metadata path is experimental).
        metadata = plane_props.get("NV_HDR_STATIC_METADATA")
        if isinstance(metadata, dict):
            value = _to_int(metadata.get("value"))
            data = metadata.get("data")
            if (value in (0, None)) and (data in (None, "", [], {})):
                print(
                    f"OK[{device}]: active primary plane {plane_id} "
                    "NV_HDR_STATIC_METADATA=blob 0 (expected; metadata stays experimental)"
                )
            else:
                print(
                    f"NOTE[{device}]: active primary plane {plane_id} "
                    f"NV_HDR_STATIC_METADATA is non-empty (value={value}, data is non-null). "
                    "Metadata path is experimental and is not required to be set."
                )

        return rc

    if not matched_device:
        print(
            f"FAIL: no connector named {target_connector!r} was found across any DRM device in drm_info",
            file=sys.stderr,
        )
        return 1

    return rc


def main(argv: list[str]) -> int:
    if len(argv) != 2:
        print(f"usage: {argv[0]} CONNECTOR_NAME (reads drm_info -j on stdin)", file=sys.stderr)
        return 2
    target_connector = argv[1]

    try:
        raw = sys.stdin.read()
    except OSError as e:
        print(f"FAIL: could not read stdin: {e}", file=sys.stderr)
        return 3
    if not raw.strip():
        print("FAIL: drm_info -j produced no output on stdin", file=sys.stderr)
        return 3

    try:
        drm = json.loads(raw)
    except json.JSONDecodeError as e:
        print(f"FAIL: could not parse drm_info JSON: {e}", file=sys.stderr)
        return 3

    if not isinstance(drm, dict):
        print(f"FAIL: drm_info JSON root is not an object: {type(drm).__name__}", file=sys.stderr)
        return 3

    return check(target_connector, drm)


if __name__ == "__main__":
    sys.exit(main(sys.argv))
