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

# Globals referenced by the cleanup trap. Initialize BEFORE setting
# the trap so an early failure path (e.g. apt-get update dying with
# set -u still on) cannot crash the trap with "unbound variable".
# Live VM 2026-05-22: the previous code created askpass_dir as a
# function-local inside ensure_repo(); the trap fired AFTER that
# function returned and `${askpass_dir}` (no default) hit set -u.
askpass_dir=""

cleanup_bootstrap() {
    # Always use ${askpass_dir:-} so an unset / out-of-scope variable
    # never makes the cleanup itself fail under set -u.
    local dir="${askpass_dir:-}"
    if [[ -n "${dir}" && -d "${dir}" ]]; then
        rm -rf "${dir}" 2>/dev/null || true
    fi
}
trap cleanup_bootstrap EXIT

[[ ${EUID} -eq 0 ]] || die "Run with sudo (need root for apt + writing under /opt)."

PROFILE="${PROFILE:-hdr-4k120}"
CLOUDDEPLOY_UNATTENDED="${CLOUDDEPLOY_UNATTENDED:-0}"
CLOUDDEPLOY_AUTO_REBOOT="${CLOUDDEPLOY_AUTO_REBOOT:-0}"
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
        # IMPORTANT: askpass_dir is the GLOBAL declared at the top of
        # this script (initial value ""). Do NOT redeclare it `local`
        # here -- the cleanup trap fires on EXIT, which runs AFTER
        # this function has returned. A function-local askpass_dir
        # would be out of scope by then and `${askpass_dir}` (no
        # default) inside the trap would crash with "unbound variable"
        # under set -u. The cleanup_bootstrap function defensively
        # uses ${askpass_dir:-} too.
        local askpass
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
        # Cleanup is handled by the top-level cleanup_bootstrap trap;
        # do NOT install a second trap here (it would overwrite the
        # global one).
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
    log "v3 Milestone 5 apply"
    log ""
    log "Implemented phases (run in this order):"
    log "  apt-health, ubuntu-upgrade, base-packages, nvidia-driver,"
    log "  cuda, edid, headless-user, desktop-packages, desktop-runtime,"
    log "  kwin-session, drm-display-validate, sunshine-build,"
    log "  sunshine-config, tailscale, pipewire-audio, streaming-services,"
    log "  stream-validate, optional-apps"
    log ""
    log "If the active profile has profile.deploy.auto_upgrade_ubuntu=true"
    log "AND the host VERSION_ID differs from profile.ubuntu_version,"
    log "this run MAY perform an Ubuntu release upgrade (e.g. 24.04 ->"
    log "25.10 via direct apt-source codename rewrite). That hop installs"
    log "a new kernel + userspace and requires a reboot before the rest"
    log "of the phases run. With profile.deploy.auto_reboot=false (the"
    log "default), this script exits 2 and the operator must reboot +"
    log "rerun: 'sudo clouddeployctl resume'. Set CLOUDDEPLOY_UNATTENDED=1"
    log "or CLOUDDEPLOY_AUTO_REBOOT=1 for autopilot continuation."
    log ""
    log "For a deploy that reaches Moonlight AV1 10-bit HDR today,"
    log "keep using the v2 entrypoint:"
    log "    sudo ENABLE_HDR=1 bash ./CloudDeploy-wayland.sh"
    log ""
    log "See docs/V2-V3-PARITY.md for the formal v2 -> v3 capability"
    log "audit and docs/V3-ROADMAP.md for milestone status."
    log "============================================================"
    local apply_args=(apply --profile "${PROFILE}" --config-dir "${CLOUDDEPLOY_REPO_DIR}/config")
    if [[ "${CLOUDDEPLOY_UNATTENDED}" == "1" ]]; then
        apply_args+=(--unattended --auto-reboot)
    elif [[ "${CLOUDDEPLOY_AUTO_REBOOT}" == "1" ]]; then
        apply_args+=(--auto-reboot)
    fi
    log "Invoking clouddeployctl ${apply_args[*]}"
    
    set +e
    /usr/local/bin/clouddeployctl "${apply_args[@]}"
    local apply_ec=$?
    set -e
    
    case ${apply_ec} in
        0)
            log "Apply completed successfully (all implemented phases done)."
            ;;
        2)
            log "Apply requires a reboot to continue."
            if [[ "${CLOUDDEPLOY_UNATTENDED}" == "1" || "${CLOUDDEPLOY_AUTO_REBOOT}" == "1" ]]; then
                log "Auto-reboot was requested; the continuation service should resume after reboot."
            else
                log "Please reboot and run 'sudo clouddeployctl resume'."
            fi
            exit 2
            ;;
        *)
            die "Apply failed with exit code ${apply_ec}."
            ;;
    esac
}

prompt_secrets() {
    # Only relevant for unattended / auto-reboot paths. The whole
    # point of this prompt is "ask the operator once at the terminal
    # so the resume step (which has no terminal) can still log in to
    # Tailscale". For interactive runs the operator can set
    # TAILSCALE_AUTHKEY in their env before invoking bootstrap and
    # this function falls through silently.
    if [[ "${CLOUDDEPLOY_UNATTENDED}" != "1" && "${CLOUDDEPLOY_AUTO_REBOOT}" != "1" ]]; then
        return 0
    fi

    local secrets_path
    secrets_path="${CLOUDDEPLOY_SECRETS_ENV:-/etc/clouddeploy/secrets.env}"

    # Check whether the active profile actually wants Tailscale. The
    # selected YAML lives at <repo>/config/profiles/<PROFILE>.yaml.
    # Cheap grep keeps bootstrap dependency-free (no yq/python).
    local profile_yaml="${CLOUDDEPLOY_REPO_DIR}/config/profiles/${PROFILE}.yaml"
    local wants_tailscale=0
    if [[ -f "${profile_yaml}" ]]; then
        if grep -E '^\s*enabled:\s*(true|yes|1)\s*$' "${profile_yaml}" >/dev/null 2>&1; then
            # Crude but reliable: a single "enabled: true" line within
            # a `tailscale:` block. The full YAML semantics live in
            # internal/config; bootstrap only needs the "do we even
            # need a key?" hint.
            if awk '
                /^[A-Za-z][A-Za-z0-9_-]*:[[:space:]]*$/ { block=$1 }
                block ~ /^tailscale:/ && /^[[:space:]]+enabled:[[:space:]]*(true|yes|1)[[:space:]]*$/ { found=1 }
                END { exit found ? 0 : 1 }
            ' "${profile_yaml}"; then
                wants_tailscale=1
            fi
        fi
    fi

    local sunshine_user="${SUNSHINE_USER:-cloudgamer}"
    local sunshine_pass="${SUNSHINE_PASS:-}"
    if [[ -z "${sunshine_pass}" ]]; then
        log "Sunshine Web UI credentials are needed once so resume can run 'sunshine --creds'."
        if [[ -r /dev/tty ]]; then
            read -rp "Sunshine username [${sunshine_user}]: " entered_user < /dev/tty
            [[ -n "${entered_user:-}" ]] && sunshine_user="${entered_user}"
            read -rsp "Sunshine password (blank to skip credential setup): " sunshine_pass < /dev/tty
            echo >/dev/tty
        else
            read -rp "Sunshine username [${sunshine_user}]: " entered_user
            [[ -n "${entered_user:-}" ]] && sunshine_user="${entered_user}"
            read -rsp "Sunshine password (blank to skip credential setup): " sunshine_pass
            echo
        fi
    fi

    if [[ "${wants_tailscale}" != "1" ]]; then
        log "Profile ${PROFILE} does not enable Tailscale; skipping authkey prompt."
        write_secrets_file "${secrets_path}" "" "${sunshine_user}" "${sunshine_pass}"
        export SUNSHINE_USER="${sunshine_user}"
        export SUNSHINE_PASS="${sunshine_pass}"
        return 0
    fi

    # Operator already exported the key (e.g. from a CI runner)?
    # Persist it to secrets.env so the post-reboot resume sees it too.
    if [[ -n "${TAILSCALE_AUTHKEY:-}" ]]; then
        log "TAILSCALE_AUTHKEY found in environment; persisting secrets to ${secrets_path}."
        write_secrets_file "${secrets_path}" "${TAILSCALE_AUTHKEY}" "${sunshine_user}" "${sunshine_pass}"
        export SUNSHINE_USER="${sunshine_user}"
        export SUNSHINE_PASS="${sunshine_pass}"
        return 0
    fi

    # Interactive prompt. Use -s so the key never echoes to the TTY.
    # `< /dev/tty` (where available) forces the prompt to the
    # terminal even if stdin is a pipe (`curl ... | bash`).
    log "Profile ${PROFILE} has Tailscale enabled and unattended/auto-reboot is on."
    log "Tailscale auth key is needed once now so the post-reboot resume can log Tailscale in."
    local key=""
    if [[ -r /dev/tty ]]; then
        read -rsp "Tailscale auth key (blank to skip Tailscale): " key < /dev/tty
        echo >/dev/tty
    else
        read -rsp "Tailscale auth key (blank to skip Tailscale): " key
        echo
    fi
    if [[ -z "${key}" ]]; then
        log "No auth key entered; Tailscale phase will skip nonfatally."
        write_secrets_file "${secrets_path}" "" "${sunshine_user}" "${sunshine_pass}"
        export SUNSHINE_USER="${sunshine_user}"
        export SUNSHINE_PASS="${sunshine_pass}"
        return 0
    fi
    write_secrets_file "${secrets_path}" "${key}" "${sunshine_user}" "${sunshine_pass}"
    # Export to this process too so apply (before the first reboot)
    # can use it without re-reading the file.
    export TAILSCALE_AUTHKEY="${key}"
    export SUNSHINE_USER="${sunshine_user}"
    export SUNSHINE_PASS="${sunshine_pass}"
    # Local var goes out of scope at function return; clear belt+suspenders.
    key=""
}

# write_secrets_file installs a 0600 root:root file at $1 containing
# TAILSCALE_AUTHKEY=$2, shell-escaped. NEVER logs the value.
write_secrets_file() {
    local path="$1"
    local key="$2"
    local sunshine_user="${3:-cloudgamer}"
    local sunshine_pass="${4:-}"
    local dir
    dir="$(dirname "${path}")"
    install -d -m 0755 -o root -g root "${dir}"
    # Single-quote escape so embedded special chars don't break the
    # KEY='value' syntax. Same encoding internal/secrets/secrets.go
    # produces, so a value written by bootstrap is round-trippable
    # via secrets.Load.
    local escaped="${key//\'/\'\\\'\'}"
    local tmp
    tmp="$(mktemp "${dir}/.secrets-XXXXXX.env")"
    chmod 0600 "${tmp}"
    # Write via printf rather than echo to avoid backslash mangling.
    printf '# managed by clouddeployctl bootstrap: operator secrets for the resume continuation service.\n' > "${tmp}"
    printf '# DO NOT EDIT BY HAND. mode 0600 root:root. systemd reads this via EnvironmentFile=.\n' >> "${tmp}"
    if [[ -n "${key}" ]]; then
        printf "TAILSCALE_AUTHKEY='%s'\n" "${escaped}" >> "${tmp}"
    fi
    local escaped_user="${sunshine_user//\'/\'\\\'\'}"
    local escaped_pass="${sunshine_pass//\'/\'\\\'\'}"
    printf "SUNSHINE_USER='%s'\n" "${escaped_user}" >> "${tmp}"
    if [[ -n "${sunshine_pass}" ]]; then
        printf "SUNSHINE_PASS='%s'\n" "${escaped_pass}" >> "${tmp}"
    fi
    chown root:root "${tmp}"
    chmod 0600 "${tmp}"
    mv -f "${tmp}" "${path}"
    chmod 0600 "${path}"
    chown root:root "${path}"
    log "Wrote ${path} (0600 root:root); tailscale_key_length=$(printf '%s' "${key}" | wc -c); sunshine_user=${sunshine_user}; sunshine_pass_set=$([[ -n "${sunshine_pass}" ]] && echo yes || echo no)"
}

main() {
    ensure_go
    ensure_repo
    build_binary
    prompt_secrets
    run_apply
}

main "$@"
