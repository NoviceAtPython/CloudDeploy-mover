#!/usr/bin/env bash
# CloudDeploy v3 bootstrap.
#
# Tiny Bash glue. Installs a Go toolchain if missing, clones (or pulls)
# this repo, builds clouddeployctl, and execs it with the requested
# profile. All real work happens inside clouddeployctl.
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/NoviceAtPython/CloudDeploy-mover/v3/bootstrap.sh \
#     | sudo PROFILE=hdr-4k120 ENABLE_HDR=1 bash
#
# Or, with the repo already on disk:
#   sudo PROFILE=hdr-4k120 ./bootstrap.sh
#
# The v2 path (CloudDeploy-wayland.sh at the repo root) is unaffected by
# this script. v2 deploys keep working by invoking the v2 entrypoint
# directly:
#   sudo ENABLE_HDR=1 bash ./CloudDeploy-wayland.sh
#
# Environment variables:
#   PROFILE                  Profile name under config/profiles/.
#                            Default: hdr-4k120.
#   ENABLE_HDR               1 / 0. Threaded through to the profile.
#   CLOUDDEPLOY_REPO_DIR     Where to clone the repo. Default
#                            /opt/clouddeploy-mover.
#   CLOUDDEPLOY_REPO_URL     Default
#                            https://github.com/NoviceAtPython/CloudDeploy-mover.git.
#   CLOUDDEPLOY_BRANCH       Default v3.
#   CLOUDDEPLOY_GO_VERSION   Default 1.22.7. Used only when apt's Go is
#                            too old.
#   CLOUDDEPLOY_RUN          1 / 0. If 0, only build clouddeployctl; do
#                            not invoke `apply`. Useful for first-time
#                            builds where the operator wants to inspect
#                            the binary first. Default 1.
#
# This script is intentionally small. Anything that needs conditionals
# beyond "is Go installed?" belongs in clouddeployctl, not here.

set -euo pipefail

log() { printf '[bootstrap] %s\n' "$*" >&2; }
die() { printf '[bootstrap] FATAL: %s\n' "$*" >&2; exit 1; }

[[ ${EUID} -eq 0 ]] || die "Run with sudo (need root for apt + writing under /opt)."

PROFILE="${PROFILE:-hdr-4k120}"
CLOUDDEPLOY_REPO_DIR="${CLOUDDEPLOY_REPO_DIR:-/opt/clouddeploy-mover}"
CLOUDDEPLOY_REPO_URL="${CLOUDDEPLOY_REPO_URL:-https://github.com/NoviceAtPython/CloudDeploy-mover.git}"
CLOUDDEPLOY_BRANCH="${CLOUDDEPLOY_BRANCH:-v3}"
CLOUDDEPLOY_GO_VERSION="${CLOUDDEPLOY_GO_VERSION:-1.22.7}"
CLOUDDEPLOY_RUN="${CLOUDDEPLOY_RUN:-1}"

ensure_go() {
    # We need Go 1.21+ for log/slog and a few other stdlib pieces.
    local installed_version
    if command -v go >/dev/null 2>&1; then
        installed_version="$(go version 2>/dev/null | awk '{print $3}' | sed 's/^go//')"
        log "Detected Go ${installed_version}"
        # Quick "is it >= 1.21" parse.
        if [[ "${installed_version}" =~ ^([0-9]+)\.([0-9]+)(\..*)?$ ]]; then
            local major="${BASH_REMATCH[1]}"
            local minor="${BASH_REMATCH[2]}"
            if (( major > 1 )) || { (( major == 1 )) && (( minor >= 21 )); }; then
                return 0
            fi
        fi
        log "Go ${installed_version} is too old; need >= 1.21"
    fi

    # Try apt first.
    if command -v apt-get >/dev/null 2>&1; then
        log "Installing golang-go via apt..."
        DEBIAN_FRONTEND=noninteractive apt-get update -qq
        if DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends golang-go ca-certificates git curl; then
            installed_version="$(go version 2>/dev/null | awk '{print $3}' | sed 's/^go//')"
            log "apt installed Go ${installed_version}"
            if [[ "${installed_version}" =~ ^([0-9]+)\.([0-9]+) ]] \
                && { (( BASH_REMATCH[1] > 1 )) || { (( BASH_REMATCH[1] == 1 )) && (( BASH_REMATCH[2] >= 21 )); }; }; then
                return 0
            fi
            log "apt's Go is too old; falling back to tarball install"
        fi
    fi

    # Tarball fallback under /usr/local/go.
    local arch
    case "$(uname -m)" in
        x86_64)  arch=amd64 ;;
        aarch64) arch=arm64 ;;
        *)       die "Unsupported architecture: $(uname -m)" ;;
    esac
    local tarball="go${CLOUDDEPLOY_GO_VERSION}.linux-${arch}.tar.gz"
    local url="https://go.dev/dl/${tarball}"
    log "Downloading ${url} ..."
    curl -fL --retry 3 -o "/tmp/${tarball}" "${url}"
    rm -rf /usr/local/go
    tar -C /usr/local -xzf "/tmp/${tarball}"
    rm -f "/tmp/${tarball}"
    install -d -m 0755 /etc/profile.d
    # Use a quoted heredoc so the ${PATH} reference stays literal in the
    # generated profile.d snippet (it expands on the *target* shell at
    # login, not here). Quoted heredoc is shellcheck-clean (SC2016).
    cat > /etc/profile.d/clouddeploy-go.sh <<'CLOUDDEPLOY_GO_PROFILE'
export PATH="/usr/local/go/bin:${PATH}"
CLOUDDEPLOY_GO_PROFILE
    chmod 0644 /etc/profile.d/clouddeploy-go.sh
    export PATH="/usr/local/go/bin:${PATH}"
    log "Installed Go $(/usr/local/go/bin/go version)"
}

ensure_repo() {
    install -d -m 0755 "$(dirname "${CLOUDDEPLOY_REPO_DIR}")"
    if [[ -d "${CLOUDDEPLOY_REPO_DIR}/.git" ]]; then
        log "Updating ${CLOUDDEPLOY_REPO_DIR} (branch ${CLOUDDEPLOY_BRANCH})"
        git -C "${CLOUDDEPLOY_REPO_DIR}" fetch --tags --prune origin "${CLOUDDEPLOY_BRANCH}"
        git -C "${CLOUDDEPLOY_REPO_DIR}" checkout -B "${CLOUDDEPLOY_BRANCH}" "origin/${CLOUDDEPLOY_BRANCH}"
        return 0
    fi
    if [[ -e "${CLOUDDEPLOY_REPO_DIR}" ]]; then
        die "${CLOUDDEPLOY_REPO_DIR} exists but is not a git checkout. Remove it or set CLOUDDEPLOY_REPO_DIR."
    fi
    log "Cloning ${CLOUDDEPLOY_REPO_URL} (branch ${CLOUDDEPLOY_BRANCH}) into ${CLOUDDEPLOY_REPO_DIR}"
    git clone --branch "${CLOUDDEPLOY_BRANCH}" "${CLOUDDEPLOY_REPO_URL}" "${CLOUDDEPLOY_REPO_DIR}"
}

build_binary() {
    log "Building clouddeployctl ..."
    (
        cd "${CLOUDDEPLOY_REPO_DIR}"
        go build -o /usr/local/bin/clouddeployctl ./cmd/clouddeployctl
    )
    log "Installed /usr/local/bin/clouddeployctl ($(/usr/local/bin/clouddeployctl version 2>/dev/null || echo "version: unknown"))"
}

run_apply() {
    if [[ "${CLOUDDEPLOY_RUN}" != "1" ]]; then
        log "CLOUDDEPLOY_RUN=0; built clouddeployctl but not running apply."
        log "Next: sudo /usr/local/bin/clouddeployctl apply --profile ${PROFILE}"
        return 0
    fi
    log "============================================================"
    log "v3 Milestone 3 partial apply"
    log ""
    log "This run will install base packages, the NVIDIA driver"
    log "family, and apply the CUDA mode policy. It will NOT yet"
    log "build/patch KWin, NOT yet build Sunshine, NOT yet generate"
    log "systemd units, and NOT yet validate HDR. Those phases land"
    log "in Milestone 4; for now they remain on the v2 path:"
    log "    sudo ENABLE_HDR=1 bash ./CloudDeploy-wayland.sh"
    log ""
    log "See docs/V3-ROADMAP.md."
    log "============================================================"
    log "Invoking clouddeployctl apply --profile ${PROFILE}"
    exec /usr/local/bin/clouddeployctl apply --profile "${PROFILE}"
}

main() {
    ensure_go
    ensure_repo
    build_binary
    run_apply
}

main "$@"
