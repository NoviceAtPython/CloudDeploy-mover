#!/usr/bin/env bash
#
# validate-kwin-patch.sh
#
# Local pre-flight: download the kwin source via `apt source kwin` and run
# `patch -p1 --dry-run` against patches/kwin-clouddeploy-nvidia-private-hdr.patch
# to confirm the patch applies cleanly to the version that Ubuntu currently
# ships. Run this whenever you touch the patch file so the malformed-hunk
# regression that bit the fresh VM (drm_plane.h hunk count mismatch, blank
# context lines without a leading space) cannot land on origin/v2 again.
#
# Exit codes:
#   0  patch applies cleanly (dry-run succeeded)
#   1  patch does not apply (malformed diff, bumped kwin version, etc.)
#   2  prerequisites missing (apt, dpkg-dev, source repo, etc.)
#
# Usage (from inside a CloudDeploy-mover clone on an Ubuntu host that has
# deb-src enabled for the matching kwin source package, typically 25.10/
# questing):
#
#   ./scripts/validate-kwin-patch.sh
#
# The script never modifies the kwin source tree - it only runs `patch
# --dry-run`. If the validation passes, dpkg-buildpackage on a real deploy
# host should also apply cleanly.

set -Eeuo pipefail

REPO_ROOT="$(cd "$(dirname "$(readlink -f "$0")")/.." && pwd)"
PATCH_FILE="${REPO_ROOT}/patches/kwin-clouddeploy-nvidia-private-hdr.patch"
WORK_DIR="${CLOUDDEPLOY_KWIN_VALIDATE_DIR:-/tmp/clouddeploy-kwin-validate}"
KEEP_WORK_DIR="${CLOUDDEPLOY_KWIN_VALIDATE_KEEP:-0}"

log() { printf '\n[validate-kwin-patch %s] %s\n' "$(date '+%T')" "$*"; }
die() { printf '\nERROR: %s\n' "$*" >&2; exit "${2:-1}"; }

[[ -f "${PATCH_FILE}" ]] || die "Patch not found at ${PATCH_FILE}" 2

for cmd in apt-get patch dpkg-source; do
        command -v "${cmd}" >/dev/null 2>&1 \
                || die "Missing prerequisite command: ${cmd}. Install dpkg-dev and ensure apt has deb-src enabled." 2
done

if grep -RhE '^Types:.*deb-src' /etc/apt/sources.list.d/*.sources 2>/dev/null | grep -q .; then
        :
elif grep -RhE '^[^#]*deb-src' /etc/apt/sources.list /etc/apt/sources.list.d/*.list 2>/dev/null | grep -q .; then
        :
else
        log "WARNING: no deb-src lines were detected in /etc/apt. apt source kwin will probably fail. Run as root:  sed -i -E 's/^Types: deb\$/Types: deb deb-src/' /etc/apt/sources.list.d/ubuntu.sources && apt-get update"
fi

rm -rf "${WORK_DIR}"
install -d -m 0755 "${WORK_DIR}"

log "Fetching kwin source into ${WORK_DIR} via apt source"
(
        cd "${WORK_DIR}"
        apt-get source kwin
)

SOURCE_DIR="$(find "${WORK_DIR}" -maxdepth 1 -type d -name 'kwin-*' | head -n1)"
[[ -n "${SOURCE_DIR}" ]] || die "Could not find unpacked kwin-* source under ${WORK_DIR}" 2
log "kwin source: ${SOURCE_DIR}"

log "Running patch -p1 --dry-run < ${PATCH_FILE}"
DRY_RUN_LOG="${WORK_DIR}/dry-run.log"
DRY_RUN_RC=0
(cd "${SOURCE_DIR}" && patch -p1 --dry-run < "${PATCH_FILE}") > "${DRY_RUN_LOG}" 2>&1 || DRY_RUN_RC=$?

cat "${DRY_RUN_LOG}"

if [[ "${DRY_RUN_RC}" -eq 0 ]]; then
        log "Dry-run PASSED. ${PATCH_FILE} applies cleanly to $(basename "${SOURCE_DIR}")."
        if [[ "${KEEP_WORK_DIR}" != "1" ]]; then
                rm -rf "${WORK_DIR}"
        fi
        exit 0
fi

log "Dry-run FAILED (exit ${DRY_RUN_RC}). Common causes:"
log "  * blank context lines in the patch that don't start with a single space"
log "  * stale @@ -X,Y +X,Y @@ hunk counts after editing the C++ content"
log "  * kwin source tree drifted from the KWin 6.4.x layout the patch targets"
log "  * unrelated apt source ${SOURCE_DIR##*/} bump"
log "Inspect ${DRY_RUN_LOG} and ${SOURCE_DIR}. Re-run the included Python normalizer to recompute hunk counts:"
cat <<'NORMALIZER'

  python3 - <<'PY'
  from pathlib import Path
  import re

  p = Path("patches/kwin-clouddeploy-nvidia-private-hdr.patch")
  lines = p.read_text().splitlines()
  hunk_re = re.compile(r'^@@ -(\d+)(?:,(\d+))? \+(\d+)(?:,(\d+))? @@(.*)$')

  out, i, fixed = [], 0, 0
  while i < len(lines):
      line = lines[i]
      m = hunk_re.match(line)
      if not m:
          out.append(line); i += 1; continue
      old_start, new_start, suffix = m.group(1), m.group(3), m.group(5)
      i += 1
      hunk = []
      while i < len(lines):
          nxt = lines[i]
          if nxt.startswith("diff --git ") or hunk_re.match(nxt):
              break
          if nxt == "":
              nxt = " "
          hunk.append(nxt)
          i += 1
      old_count = new_count = 0
      for hl in hunk:
          if hl.startswith("\\"):
              continue
          if hl == "":
              hl = " "
          c = hl[0]
          if c in (" ", "-"):
              old_count += 1
          if c in (" ", "+"):
              new_count += 1
      out.append(f"@@ -{old_start},{old_count} +{new_start},{new_count} @@{suffix}")
      out.extend(hunk)
      fixed += 1
  p.write_text("\n".join(out) + "\n")
  print(f"Rewrote {fixed} hunks in {p}")
  PY

NORMALIZER
exit 1
