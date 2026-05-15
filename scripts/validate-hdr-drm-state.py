#!/usr/bin/env python3
"""Validate the NVIDIA private HDR DRM state on the active CRTC + active
primary plane.

Reads `drm_info -j` JSON on stdin. Optional argv[1] is the expected
connector name (e.g. DP-1, HDMI-A-2); used only for diagnostic context,
not for the scan itself.

Algorithm (matches the v2 review spec):

    1. Find every ACTIVE=1 CRTC in drm_info. "Active" means either the
       JSON field active is True, or the CRTC has a non-zero fb_id, or
       it carries a real mode (non-zero clock / explicit mode name).
    2. For each active CRTC, find the primary plane whose CRTC_ID points
       at that CRTC and whose FB_ID != 0.
    3. Verify on that pair:
            active CRTC          NV_CRTC_REGAMMA_TF   = PQ
            active primary plane NV_INPUT_COLORSPACE  = BT.2100 PQ
            active primary plane NV_PLANE_DEGAMMA_TF  = PQ
       NV_HDR_STATIC_METADATA = blob 0 is the documented good state and
       is accepted; anything non-empty is logged as NOTE (the metadata
       path is experimental and not required).

If any active CRTC + active primary plane pair satisfies all three NV_*
properties, exit 0. Otherwise exit non-zero with one diagnostic per
candidate pair so the failure is easy to read.

The script intentionally does NOT grep the global drm_info dump - a
property like NV_CRTC_REGAMMA_TF=PQ on an unused CRTC must not fool the
check.
"""
from __future__ import annotations

import json
import sys
from typing import Any, Optional

PRIMARY_PLANE_TYPE_INT = 1  # DRM_PLANE_TYPE_PRIMARY


def _to_int(value: Any) -> Optional[int]:
    try:
        return int(value)
    except (TypeError, ValueError):
        return None


def enum_value_name(prop: Any) -> Optional[str]:
    """Return the textual enum value (e.g. "PQ") for an enum-typed
    property, or None when the property is missing/non-enum/empty.

    drm_info -j shapes an enum property as:

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


def crtc_is_active(crtc: dict) -> bool:
    """An active CRTC has either active=true, or a non-zero fb_id, or a
    real mode (non-zero clock / explicit mode name / mode_valid=true).
    """
    if crtc.get("active") is True:
        return True
    fb_id = _to_int(crtc.get("fb_id"))
    if fb_id is not None and fb_id != 0:
        return True
    mode = crtc.get("mode")
    if isinstance(mode, dict):
        clock = _to_int(mode.get("clock"))
        if clock is not None and clock != 0:
            return True
        if mode.get("name"):
            return True
    if crtc.get("mode_valid") is True:
        return True
    # Some drm_info versions only expose ACTIVE via properties.
    active_prop = (crtc.get("properties") or {}).get("ACTIVE")
    if isinstance(active_prop, dict):
        raw = _to_int(active_prop.get("value"))
        if raw is not None and raw != 0:
            return True
    return False


def plane_is_primary(plane: dict) -> bool:
    """A plane is "primary" when:
      * its top-level "type" string is "primary", OR
      * its properties.type enum resolves to "Primary", OR
      * its properties.type raw value equals DRM_PLANE_TYPE_PRIMARY (1).
    """
    top_type = plane.get("type")
    if isinstance(top_type, str):
        return top_type.lower() == "primary"
    type_prop = (plane.get("properties") or {}).get("type")
    enum_name = enum_value_name(type_prop)
    if enum_name and enum_name.lower() == "primary":
        return True
    if isinstance(type_prop, dict):
        raw = _to_int(type_prop.get("value"))
        if raw is not None:
            return raw == PRIMARY_PLANE_TYPE_INT
    return False


def plane_has_active_fb(plane: dict) -> bool:
    fb_id = _to_int(plane.get("fb_id"))
    if fb_id is not None and fb_id != 0:
        return True
    fb_prop = (plane.get("properties") or {}).get("FB_ID")
    if isinstance(fb_prop, dict):
        raw = _to_int(fb_prop.get("value"))
        if raw is not None and raw != 0:
            return True
    return False


def plane_crtc_id(plane: dict) -> Optional[int]:
    cid = _to_int(plane.get("crtc_id"))
    if cid is not None:
        return cid
    crtc_prop = (plane.get("properties") or {}).get("CRTC_ID")
    if isinstance(crtc_prop, dict):
        return _to_int(crtc_prop.get("value"))
    return None


def find_active_primary_plane_for_crtc(
    planes: dict, crtc_id: int
) -> tuple[Optional[str], Optional[dict]]:
    """Locate the primary plane whose CRTC_ID == crtc_id AND FB_ID != 0."""
    for pid, plane in planes.items():
        if not isinstance(plane, dict):
            continue
        if not plane_is_primary(plane):
            continue
        if plane_crtc_id(plane) != crtc_id:
            continue
        if not plane_has_active_fb(plane):
            continue
        return str(pid), plane
    return None, None


def metadata_is_blob_zero(metadata: Any) -> bool:
    """blob 0 = property exists but holds no data."""
    if not isinstance(metadata, dict):
        return True  # property missing entirely == effectively blob 0
    value = _to_int(metadata.get("value"))
    data = metadata.get("data")
    blob_id_zero = value in (0, None)
    no_payload = data in (None, "", [], {})
    return blob_id_zero and no_payload


def check(target_connector: Optional[str], drm: dict) -> int:
    any_active_candidate = False
    candidate_failures: list[str] = []

    for device, dev_data in drm.items():
        if not isinstance(dev_data, dict):
            continue
        crtcs = dev_data.get("crtcs") or {}
        planes = dev_data.get("planes") or {}

        for crtc_id_str, crtc in crtcs.items():
            if not isinstance(crtc, dict):
                continue
            if not crtc_is_active(crtc):
                continue

            crtc_id_int = _to_int(crtc_id_str)
            if crtc_id_int is None:
                continue

            primary_id, primary = find_active_primary_plane_for_crtc(
                planes, crtc_id_int
            )
            if primary is None:
                candidate_failures.append(
                    f"{device}: active CRTC {crtc_id_str} has no active primary plane "
                    "(no plane with CRTC_ID matching and FB_ID != 0)"
                )
                continue

            any_active_candidate = True

            crtc_props = crtc.get("properties") or {}
            plane_props = primary.get("properties") or {}

            regamma = enum_value_name(crtc_props.get("NV_CRTC_REGAMMA_TF"))
            input_cs = enum_value_name(plane_props.get("NV_INPUT_COLORSPACE"))
            degamma = enum_value_name(plane_props.get("NV_PLANE_DEGAMMA_TF"))

            mismatches: list[str] = []
            if regamma != "PQ":
                mismatches.append(f"NV_CRTC_REGAMMA_TF={regamma!r} (expected 'PQ')")
            if input_cs not in {"BT.2100 PQ", "BT2100 PQ"}:
                mismatches.append(
                    f"NV_INPUT_COLORSPACE={input_cs!r} (expected 'BT.2100 PQ')"
                )
            if degamma != "PQ":
                mismatches.append(f"NV_PLANE_DEGAMMA_TF={degamma!r} (expected 'PQ')")

            label = (
                f"{device}: active CRTC {crtc_id_str} + active primary plane {primary_id}"
            )
            if target_connector:
                label += f" (target connector {target_connector})"

            if not mismatches:
                print(
                    f"OK: {label}: NV_CRTC_REGAMMA_TF=PQ, "
                    f"NV_INPUT_COLORSPACE={input_cs}, NV_PLANE_DEGAMMA_TF=PQ"
                )
                # Metadata state: blob 0 = good; anything else logged as NOTE.
                metadata = plane_props.get("NV_HDR_STATIC_METADATA")
                if metadata is None:
                    print(
                        f"OK: {label}: NV_HDR_STATIC_METADATA property absent on this plane "
                        "(equivalent to blob 0; metadata stays experimental)"
                    )
                elif metadata_is_blob_zero(metadata):
                    print(
                        f"OK: {label}: NV_HDR_STATIC_METADATA = blob 0 "
                        "(expected; metadata stays experimental)"
                    )
                else:
                    raw_val = _to_int(metadata.get("value")) if isinstance(metadata, dict) else None
                    print(
                        f"NOTE: {label}: NV_HDR_STATIC_METADATA is non-empty "
                        f"(value={raw_val}). Metadata path is experimental and is not required."
                    )
                return 0

            candidate_failures.append(f"{label}: " + ", ".join(mismatches))

    if not any_active_candidate:
        msg = (
            "FAIL: no ACTIVE CRTC with an active primary plane was found in drm_info -j. "
            "KWin/Plasma is probably not actually driving any output. "
            "Check kwin-realvt.service, kscreen-doctor -o, and the force-mode helper."
        )
        if candidate_failures:
            msg += "\n" + "\n".join(candidate_failures)
        print(msg, file=sys.stderr)
        return 1

    for fail in candidate_failures:
        print(f"FAIL: {fail}", file=sys.stderr)
    return 1


def main(argv: list[str]) -> int:
    if len(argv) < 1 or len(argv) > 2:
        print(
            f"usage: {argv[0]} [CONNECTOR_NAME]  (reads drm_info -j on stdin)",
            file=sys.stderr,
        )
        return 2
    target_connector = argv[1] if len(argv) == 2 else None

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
        print(
            f"FAIL: drm_info JSON root is not an object: {type(drm).__name__}",
            file=sys.stderr,
        )
        return 3

    return check(target_connector, drm)


if __name__ == "__main__":
    sys.exit(main(sys.argv))
