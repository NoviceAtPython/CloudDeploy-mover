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
#   GH_TOKEN                 GitHub Personal Access Token. Required only
#                            when CLOUDDEPLOY_REPO_URL points at a
#                            private repository. Bootstrap funnels the
#                            token through a GIT_ASKPASS shim so it
#                            never appears in `ps` or `git remote -v`.
#
# Private-clone invocation (sudo strips GH_TOKEN unless you opt in
# explicitly):
#
#   sudo -E GH_TOKEN="$GH_TOKEN" PROFILE=hdr-4k120-cuda-compatible \
#       bash ./bootstrap.sh
#
# Or with an env-file owned by root:
#
#   sudo install -m 0600 /dev/null /root/clouddeploy-v3.env
#   sudo ${EDITOR:-nano} /root/clouddeploy-v3.env   # GH_TOKEN=ghp_...
#   sudo bash -c 'set -a; . /root/clouddeploy-v3.env; bash ./bootstrap.sh'
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

dpkg_health_preflight() {
    # Best-effort: detect a corrupt /var/lib/dpkg/updates journal
    # before we try to apt-get install golang-go. If `dpkg --audit`
    # errors out AND the journal directory has numeric entries,
    # quarantine those files and run `dpkg --configure -a`. This is
    # the same recipe `phase apt-health` runs later, but bootstrap
    # needs it earlier so the very first apt-get call doesn't half-
    # configure the toolchain on a broken dpkg.
    [[ -d /var/lib/dpkg/updates ]] || return 0
    if dpkg --audit >/dev/null 2>/tmp/clouddeploy-dpkg-audit.err; then
        return 0
    fi
    if ! grep -q '/var/lib/dpkg/updates' /tmp/clouddeploy-dpkg-audit.err 2>/dev/null; then
        # Audit errored for some other reason. Surface it but don't
        # quarantine the journal blindly.
        log "dpkg --audit failed; head of stderr:"
        head -n 5 /tmp/clouddeploy-dpkg-audit.err 2>/dev/null || true
        return 0
    fi
    local ts backup
    ts="$(date -u +%Y%m%d-%H%M%S)"
    backup="/var/lib/clouddeploy/backups/dpkg-updates-${ts}"
    log "dpkg journal looks corrupt; quarantining numeric files to ${backup}"
    install -d -m 0700 "$(dirname "${backup}")"
    install -d -m 0700 "${backup}"
    shopt -s nullglob
    local f moved=0
    for f in /var/lib/dpkg/updates/[0-9]*; do
        [[ -f "$f" ]] || continue
        case "$(basename "$f")" in
            *[!0-9]*) ;;  # skip non-pure-numeric
            *)
                mv "$f" "${backup}/"
                moved=$((moved + 1))
                ;;
        esac
    done
    shopt -u nullglob
    log "dpkg journal: moved ${moved} file(s) to ${backup}"
    if ! dpkg --configure -a; then
        die "dpkg --configure -a failed after quarantining journal; rerun manually and inspect ${backup}"
    fi
    if ! DEBIAN_FRONTEND=noninteractive apt-get -f install -y; then
        die "apt-get -f install failed after dpkg journal quarantine; rerun manually and inspect ${backup}"
    fi
    log "dpkg health preflight: repair complete"
}

wait_for_apt_lock() {
    # Wait up to ~3 minutes for apt-daily / unattended-upgrades /
    # someone-else's apt to release the dpkg + apt locks. We don't try
    # to stop those services here - bootstrap is best-effort and may
    # run without root for the early "is Go installed?" path - we just
    # poll the locks.
    local deadline=$((SECONDS + 180))
    local locks=(
        /var/lib/dpkg/lock-frontend
        /var/lib/dpkg/lock
        /var/lib/apt/lists/lock
        /var/cache/apt/archives/lock
    )
    while (( SECONDS < deadline )); do
        local busy=""
        for f in "${locks[@]}"; do
            if [[ -e "$f" ]] && command -v fuser >/dev/null 2>&1 \
                && fuser "$f" >/dev/null 2>&1; then
                busy="${busy} $f"
            fi
        done
        if [[ -z "$busy" ]]; then
            return 0
        fi
        log "Waiting for apt/dpkg locks:${busy}"
        sleep 5
    done
    log "WARNING: apt/dpkg locks still held after 3m; proceeding"
    return 0
}

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
        dpkg_health_preflight
        wait_for_apt_lock
        DEBIAN_FRONTEND=noninteractive apt-get update -qq
        wait_for_apt_lock
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

    # Private-clone support. The repo is currently private; sudo loses
    # the GH_TOKEN env by default, so the recommended invocation is:
    #
    #   sudo -E GH_TOKEN="$GH_TOKEN" PROFILE=... bash bootstrap.sh
    #
    # We translate GH_TOKEN into a GIT_ASKPASS shim so neither the URL
    # nor the process list ever contains the token (the URL-embedded
    # form `https://${GH_TOKEN}@github.com/...` shows up in `ps` and
    # in `git remote -v` output).
    local git_env=()
    if [[ -n "${GH_TOKEN:-}" ]]; then
        local askpass_dir askpass
        askpass_dir="$(mktemp -d /tmp/clouddeploy-askpass.XXXXXX)"
        askpass="${askpass_dir}/askpass.sh"
        cat > "${askpass}" <<'CLOUDDEPLOY_GIT_ASKPASS'
#!/usr/bin/env bash
# clouddeployctl bootstrap: feed GH_TOKEN to git via askpass.
case "${1:-}" in
    Username*) echo "x-access-token" ;;
    Password*) echo "${GH_TOKEN}" ;;
esac
CLOUDDEPLOY_GIT_ASKPASS
        chmod 0700 "${askpass}"
        git_env=(env "GH_TOKEN=${GH_TOKEN}" "GIT_ASKPASS=${askpass}" "GIT_TERMINAL_PROMPT=0")
        # Best-effort cleanup. trap may already be set; we append.
        trap 'rm -rf "${askpass_dir}" 2>/dev/null || true' EXIT
    fi

    if [[ -d "${CLOUDDEPLOY_REPO_DIR}/.git" ]]; then
        log "Updating ${CLOUDDEPLOY_REPO_DIR} (branch ${CLOUDDEPLOY_BRANCH})"
        "${git_env[@]}" git -C "${CLOUDDEPLOY_REPO_DIR}" fetch --tags --prune origin "${CLOUDDEPLOY_BRANCH}"
        "${git_env[@]}" git -C "${CLOUDDEPLOY_REPO_DIR}" checkout -B "${CLOUDDEPLOY_BRANCH}" "origin/${CLOUDDEPLOY_BRANCH}"
        return 0
    fi
    if [[ -e "${CLOUDDEPLOY_REPO_DIR}" ]]; then
        die "${CLOUDDEPLOY_REPO_DIR} exists but is not a git checkout. Remove it or set CLOUDDEPLOY_REPO_DIR."
    fi
    log "Cloning ${CLOUDDEPLOY_REPO_URL} (branch ${CLOUDDEPLOY_BRANCH}) into ${CLOUDDEPLOY_REPO_DIR}"
    "${git_env[@]}" git clone --branch "${CLOUDDEPLOY_BRANCH}" "${CLOUDDEPLOY_REPO_URL}" "${CLOUDDEPLOY_REPO_DIR}"
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
    log "v3 Milestone 4 partial apply"
    log ""
    log "Implemented phases (run in this order):"
    log "  apt-health, ubuntu-upgrade, base-packages, nvidia-driver,"
    log "  cuda, edid, headless-user, desktop-packages, desktop-runtime,"
    log "  kwin-session, drm-display-validate"
    log ""
    log "If the active profile has profile.deploy.auto_upgrade_ubuntu=true"
    log "AND the host VERSION_ID differs from profile.ubuntu_version,"
    log "this run MAY perform an Ubuntu release upgrade (e.g. 24.04 ->"
    log "25.10 via direct apt-source codename rewrite). That hop installs"
    log "a new kernel + userspace and requires a reboot before the rest"
    log "of the phases run. With profile.deploy.auto_reboot=false (the"
    log "default), this script exits 2 and the operator must reboot +"
    log "rerun: 'sudo clouddeployctl resume'."
    log ""
    log "NOT yet implemented (still v2-only):"
    log "  Sunshine fork build, Tailscale, PipeWire virtual sink,"
    log "  full Sunshine/Plasma systemd unit chain,"
    log "  HDR DRM validation, HDR stream validation."
    log "(patched-KWin NVIDIA private HDR build is real: phase kwin-patch.)"
    log ""
    log "For a deploy that reaches Moonlight AV1 10-bit HDR today,"
    log "keep using the v2 entrypoint:"
    log "    sudo ENABLE_HDR=1 bash ./CloudDeploy-wayland.sh"
    log ""
    log "See docs/V2-V3-PARITY.md for the formal v2 -> v3 capability"
    log "audit and docs/V3-ROADMAP.md for milestone status."
    log "============================================================"
    log "Invoking clouddeployctl apply --profile ${PROFILE}"
    
    set +e
    /usr/local/bin/clouddeployctl apply --profile "${PROFILE}"
    local apply_ec=$?
    set -e
    
    case ${apply_ec} in
        0)
            log "Apply completed successfully (all implemented phases done)."
            ;;
        2)
            log "Apply requires a reboot to continue."
            log "Please reboot and run 'sudo clouddeployctl resume'."
            exit 2
            ;;
        10)
            log "Partial apply complete. Unimplemented phases skipped."
            exit 10
            ;;
        *)
            die "Apply failed with exit code ${apply_ec}."
            ;;
    esac
}

main() {
    ensure_go
    ensure_repo
    build_binary
    run_apply
}

main "$@"
