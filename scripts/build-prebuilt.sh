#!/usr/bin/env bash
# build-prebuilt.sh — assemble a CloudDeploy prebuilt artifact bundle.
#
# Packages the already-compiled Sunshine fork binary + built web UI and the
# HDR-patched KWin .debs into a single release tarball. The sunshine-build and
# kwin-patch phases download this bundle (internal/phase/prebuilt.go) and skip
# the slow from-source compiles, turning a ~40 min deploy into ~10-20 min.
#
# RUN THIS ON THE HOST YOU BUILT THE ARTIFACTS ON. The Ubuntu version + arch of
# that host become the bundle's identity, and the compiled binary/.debs are only
# ABI-compatible with that exact release. Current HDR profiles upgrade the VM to
# 25.10 *before* these phases run, so the bundle must be built on 25.10.
#
# Layout produced (must match internal/phase/prebuilt.go expectations):
#
#   clouddeploy-prebuilt-ubuntu<ver>-<arch>.tar.gz
#   ├── manifest.json   {sunshine_commit, kwin_version, ubuntu, arch, built_utc}
#   ├── sunshine/
#   │   ├── sunshine          compiled fork binary
#   │   └── assets/           apps.json, shaders/, images, AND a built web/ UI
#   └── kwin/*.deb            kwin-common, kwin-data, kwin-dev, kwin-wayland, libkwin6
#
# Usage:
#   scripts/build-prebuilt.sh [output-dir]      # default output-dir = $PWD
#
# Env overrides (defaults match the CloudDeploy build VM layout):
#   SUNSHINE_SRC    /opt/sunshine-src              git checkout w/ build/{sunshine,assets}
#   KWIN_BUILD_DIR  /opt/clouddeploy-kwin-src/build dir holding the patched *.deb
#   SUNSHINE_WEB    /usr/share/sunshine/web         built web UI folded into assets/web
#
# Publish (separate step, no auth needed by downloaders since the repo is public):
#   gh release create prebuilt-ubuntu2510-amd64 clouddeploy-prebuilt-ubuntu2510-amd64.tar.gz \
#       --repo NoviceAtPython/CloudDeploy-mover --title "Prebuilt artifacts (Ubuntu 25.10 amd64)" \
#       --notes "Compiled Sunshine fork + HDR-patched KWin for 25.10 amd64."
#   # update an existing release asset instead:
#   gh release upload prebuilt-ubuntu2510-amd64 clouddeploy-prebuilt-ubuntu2510-amd64.tar.gz \
#       --repo NoviceAtPython/CloudDeploy-mover --clobber
set -euo pipefail

SUNSHINE_SRC="${SUNSHINE_SRC:-/opt/sunshine-src}"
KWIN_BUILD_DIR="${KWIN_BUILD_DIR:-/opt/clouddeploy-kwin-src/build}"
SUNSHINE_WEB="${SUNSHINE_WEB:-/usr/share/sunshine/web}"
OUT_DIR="${1:-$PWD}"

die() { echo "ERROR: $*" >&2; exit 1; }

# --- host identity (becomes the bundle tag) ---
[ -r /etc/os-release ] || die "/etc/os-release unreadable; run this on the build host"
# shellcheck disable=SC1091
. /etc/os-release
UBUNTU_VER="${VERSION_ID:?could not read VERSION_ID from /etc/os-release}"
ARCH="$(dpkg --print-architecture)"
TAG="prebuilt-ubuntu${UBUNTU_VER//./}-${ARCH}"
ASSET="clouddeploy-${TAG}.tar.gz"
echo ">> assembling ${ASSET}  (ubuntu ${UBUNTU_VER}, arch ${ARCH})"

# --- locate Sunshine binary + assets + commit ---
SUN_BIN="${SUNSHINE_SRC}/build/sunshine"
SUN_ASSETS="${SUNSHINE_SRC}/build/assets"
[ -x "$SUN_BIN" ]   || die "sunshine binary not found/executable at $SUN_BIN"
[ -d "$SUN_ASSETS" ] || die "sunshine assets dir not found at $SUN_ASSETS"
SUN_COMMIT="$(git -C "$SUNSHINE_SRC" rev-parse HEAD)"
echo ">> sunshine: $SUN_BIN ($(du -h "$SUN_BIN" | awk '{print $1}')), commit ${SUN_COMMIT}"

# --- locate KWin .debs + version + verify the HDR patch is compiled in ---
shopt -s nullglob
KWIN_DEBS=("$KWIN_BUILD_DIR"/*.deb)
shopt -u nullglob
[ "${#KWIN_DEBS[@]}" -gt 0 ] || die "no .deb files in $KWIN_BUILD_DIR"

KWIN_VER=""
PATCH_OK=no
for d in "${KWIN_DEBS[@]}"; do
  case "$(basename "$d")" in
    libkwin6_*)
      KWIN_VER="$(dpkg-deb -f "$d" Version)"
      t="$(mktemp -d)"; dpkg-deb -x "$d" "$t"
      if grep -rlqE "NV_INPUT_COLORSPACE|NV_CRTC_REGAMMA_TF" "$t" 2>/dev/null; then PATCH_OK=yes; fi
      rm -rf "$t"
      ;;
  esac
done
[ -n "$KWIN_VER" ] || KWIN_VER="$(dpkg-deb -f "${KWIN_DEBS[0]}" Version)"
# Drop any "epoch:" prefix (4:6.4.5-0ubuntu3 -> 6.4.5-0ubuntu3) so the manifest
# matches the deb filename convention and the substring gate in prebuilt.go's
# kwin-patch check (host dpkg version still carries the epoch).
KWIN_VER="${KWIN_VER#*:}"
[ "$PATCH_OK" = yes ] || die "HDR patch (NV_INPUT_COLORSPACE/NV_CRTC_REGAMMA_TF) not found compiled into libkwin6 — refusing to ship an unpatched bundle"
echo ">> kwin: ${#KWIN_DEBS[@]} debs, version ${KWIN_VER}, HDR patch verified in libkwin6"

# --- stage the bundle tree ---
STAGE="$(mktemp -d)"
trap 'rm -rf "$STAGE"' EXIT
mkdir -p "$STAGE/sunshine" "$STAGE/kwin"

install -m 0755 "$SUN_BIN" "$STAGE/sunshine/sunshine"
# -L (dereference) is load-bearing: build/assets/shaders is a SYMLINK into the
# source tree (src_assets/linux/assets/shaders). A plain `cp -a` would preserve
# it as a symlink, and on a prebuilt deploy (no source tree) it would dangle and
# break Sunshine's color-convert shaders. -L copies the real shader files.
cp -aL "$SUN_ASSETS" "$STAGE/sunshine/assets"
# Guard: refuse to ship a bundle that still contains any symlink under assets/
# (they would point at absolute build-host paths that do not exist on deploys).
if find "$STAGE/sunshine/assets" -type l | grep -q .; then
  echo "ERROR: dangling symlinks remain under assets/ after deref:" >&2
  find "$STAGE/sunshine/assets" -type l >&2
  die "assets contain symlinks; cp -aL did not dereference them"
fi

# Fold in the built web UI. Prebuilt deploys skip the git clone, so there is no
# src_assets/.../web for the phase to npm-build as a fallback — the bundle MUST
# carry a finished web/ directory or the Sunshine config UI would be missing.
if [ -d "$SUNSHINE_WEB" ]; then
  mkdir -p "$STAGE/sunshine/assets/web"
  cp -a "$SUNSHINE_WEB/." "$STAGE/sunshine/assets/web/"
fi
[ -f "$STAGE/sunshine/assets/web/index.html" ] \
  || die "assets/web/index.html missing after staging — no built Sunshine web UI found (set SUNSHINE_WEB to a built web dir)"

cp -a "${KWIN_DEBS[@]}" "$STAGE/kwin/"

# --- manifest ---
cat > "$STAGE/manifest.json" <<EOF
{
  "sunshine_commit": "${SUN_COMMIT}",
  "kwin_version": "${KWIN_VER}",
  "ubuntu": "${UBUNTU_VER}",
  "arch": "${ARCH}",
  "built_utc": "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
}
EOF

# --- pack (members at archive top level: manifest.json, sunshine/, kwin/) ---
mkdir -p "$OUT_DIR"
OUT="${OUT_DIR%/}/${ASSET}"
tar czf "$OUT" -C "$STAGE" manifest.json sunshine kwin

echo ">> wrote   $OUT"
echo ">> size    $(du -h "$OUT" | awk '{print $1}')"
echo ">> sha256  $(sha256sum "$OUT" | awk '{print $1}')"
echo ">> manifest:"
sed 's/^/     /' "$STAGE/manifest.json"
echo ">> contents (top):"
tar tzf "$OUT" | sed 's/^/     /' | head -30
