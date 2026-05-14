#!/bin/bash

if LC_ALL=C grep -q $'\r' "$0" 2>/dev/null; then
        echo "ERROR: CloudDeploy-wayland.sh appears to contain CRLF line endings." >&2
        echo "Repair it before running:" >&2
        echo "  sed -i 's/\\r$//' CloudDeploy-wayland.sh" >&2
        echo "  chmod +x CloudDeploy-wayland.sh" >&2
        exit 2
fi

set -Eeuo pipefail
export DEBIAN_FRONTEND=noninteractive

APT_DPKG_OPTIONS=(
        -o Dpkg::Options::=--force-confdef
        -o Dpkg::Options::=--force-confold
)

clouddeploy_default_real_user() {
        local candidate

        candidate="${SUDO_USER:-}"
        if [[ -n "${candidate}" && "${candidate}" != "root" ]] \
                && [[ "$(id -u "${candidate}" 2>/dev/null || echo 0)" -ge 1000 ]]; then
                printf '%s\n' "${candidate}"
                return 0
        fi

        if [[ "$(id -u user 2>/dev/null || echo 0)" -ge 1000 ]]; then
                printf '%s\n' "user"
                return 0
        fi

        candidate="$(getent passwd | awk -F: '$3 >= 1000 && $1 != "nobody" && $7 !~ /(nologin|false)$/ { print $1; exit }')"
        if [[ -n "${candidate}" ]]; then
                printf '%s\n' "${candidate}"
                return 0
        fi

        printf '%s\n' "user"
}

ALLOW_ROOT_SESSION="${ALLOW_ROOT_SESSION:-0}"
DEFAULT_USER="${DEFAULT_USER:-$(clouddeploy_default_real_user)}"
if [[ "${DEFAULT_USER}" == "root" && "${ALLOW_ROOT_SESSION}" != "1" ]]; then
        DEFAULT_USER="$(clouddeploy_default_real_user)"
fi
HEADLESS_USER="${HEADLESS_USER:-$DEFAULT_USER}"
SUNSHINE_USER="${SUNSHINE_USER:-$HEADLESS_USER}"
SUNSHINE_PASS="${SUNSHINE_PASS:-}"
TAILSCALE_AUTHKEY="${TAILSCALE_AUTHKEY:-}"
SUNSHINE_DEB_URL="${SUNSHINE_DEB_URL:-https://github.com/LizardByte/Sunshine/releases/download/v2025.924.154138/sunshine-ubuntu-24.04-amd64.deb}"
TARGET_NVIDIA_DRIVER_MAJOR="${TARGET_NVIDIA_DRIVER_MAJOR:-580}"
INSTALL_CUDA_TOOLKIT="${INSTALL_CUDA_TOOLKIT:-1}"
CUDA_TOOLKIT_PACKAGE="${CUDA_TOOLKIT_PACKAGE:-cuda-toolkit}"
CUDA_INSTALL_METHOD="${CUDA_INSTALL_METHOD:-auto}"
REQUIRE_CUDA_TOOLKIT="${REQUIRE_CUDA_TOOLKIT:-${INSTALL_CUDA_TOOLKIT}}"
CUDA_RUNFILE_VERSION="${CUDA_RUNFILE_VERSION:-13.0.2}"
CUDA_RUNFILE_DRIVER_VERSION="${CUDA_RUNFILE_DRIVER_VERSION:-580.95.05}"
CUDA_RUNFILE_URL="${CUDA_RUNFILE_URL:-https://developer.download.nvidia.com/compute/cuda/${CUDA_RUNFILE_VERSION}/local_installers/cuda_${CUDA_RUNFILE_VERSION}_${CUDA_RUNFILE_DRIVER_VERSION}_linux.run}"
FORCE_DRIVER_UPGRADE="${FORCE_DRIVER_UPGRADE:-1}"
# Safe/stable deploys use the packaged .deb by default. Fresh VMs may still
# need SUNSHINE_SOURCE_MODE=fork until the CloudDeploy pairing/stream fixes are upstreamed.
SUNSHINE_SOURCE_MODE="${SUNSHINE_SOURCE_MODE:-fork}"
SUNSHINE_FORK_REPO="${SUNSHINE_FORK_REPO:-https://github.com/NoviceAtPython/Sunshine.git}"
SUNSHINE_DIAGNOSTIC_FORK_BRANCH="${SUNSHINE_DIAGNOSTIC_FORK_BRANCH:-codex/sunshine-pairing-diagnostics}"
SUNSHINE_CLEAN_FORK_BRANCH="${SUNSHINE_CLEAN_FORK_BRANCH:-clouddeploy-clean-pairing-stream-fix}"
SUNSHINE_FORK_BRANCH="${SUNSHINE_FORK_BRANCH:-$SUNSHINE_DIAGNOSTIC_FORK_BRANCH}"
SUNSHINE_BUILD_DIR="${SUNSHINE_BUILD_DIR:-/opt/sunshine-src}"
SUNSHINE_BUILD_JOBS="${SUNSHINE_BUILD_JOBS:-2}"
SUNSHINE_INSTALL_BIN="${SUNSHINE_INSTALL_BIN:-/usr/local/bin/sunshine-clouddeploy}"
# Sunshine's optional CUDA/NvFBC module fails to compile against CUDA 13 headers
# combined with newer glibc on Ubuntu 25.10 / 26.04 (rsqrt/rsqrtf conflict).
# The KMS/DRM/Wayland/NVENC streaming path does not need it, so auto-disable it
# on those releases. Set to "on" to force it on, or "off" to always disable.
SUNSHINE_ENABLE_CUDA_MODULE="${SUNSHINE_ENABLE_CUDA_MODULE:-auto}"
FORCED_CONNECTOR="${FORCED_CONNECTOR:-DP-1}"
TARGET_WIDTH="${TARGET_WIDTH:-3840}"
TARGET_HEIGHT="${TARGET_HEIGHT:-2160}"
TARGET_FPS="${TARGET_FPS:-120}"
ENABLE_HDR="${ENABLE_HDR:-0}"
EDID_PROFILE="${EDID_PROFILE:-auto}"
ENABLE_PLASMA6="${ENABLE_PLASMA6:-0}"
STREAM_MODE="${STREAM_MODE:-plasma}"
if [[ "${ENABLE_PLASMA6}" == "1" && "${STREAM_MODE}" == "plasma" ]]; then
        STREAM_MODE="plasma6"
fi
SESSION_BACKEND="${SESSION_BACKEND:-$STREAM_MODE}"
PLASMA_LAUNCH_MODE="${PLASMA_LAUNCH_MODE:-startplasma}"
KWIN_VTNR="${KWIN_VTNR:-7}"
KWIN_WAYLAND_DISPLAY="${KWIN_WAYLAND_DISPLAY:-wayland-0}"
WESTON_WAYLAND_DISPLAY="${WESTON_WAYLAND_DISPLAY:-wayland-cd}"
WESTON_MODE="${WESTON_MODE:-${TARGET_WIDTH}x${TARGET_HEIGHT}@${TARGET_FPS}}"
SUNSHINE_ENCODER="${SUNSHINE_ENCODER:-nvenc}"
SUNSHINE_DRM_DEVICE="${SUNSHINE_DRM_DEVICE:-auto}"
SUNSHINE_AV1_MODE="${SUNSHINE_AV1_MODE:-2}"
SUNSHINE_HEVC_MODE="${SUNSHINE_HEVC_MODE:-0}"
SENTINEL="/opt/clouddeploy-wayland.installed"
SCRIPT_VERSION="21-plasma6-hdr-experiment"
REBOOT_MARKER="/opt/clouddeploy-wayland.needs-reboot"
REBOOT_REASON_FILE="/opt/clouddeploy-wayland.reboot-reason"
GRUB_OVERRIDE_FILE="/etc/default/grub.d/99-clouddeploy-edid.cfg"
CLOUDDEPLOY_ENV_FILE="/etc/clouddeploy-wayland.env"

INSTALL_OPTIONAL_APPS="${INSTALL_OPTIONAL_APPS:-1}"
ENABLE_USER_NOPASSWD_SUDO="${ENABLE_USER_NOPASSWD_SUDO:-1}"
CLOUDDEPLOY_STATE_DIR="${CLOUDDEPLOY_STATE_DIR:-/opt/clouddeploy/state}"
CURRENT_PHASE="startup"
CUDA_REPO_ENABLED=0
CUDA_REPO_DISTRO=""
CUDA_TOOLKIT_SOURCE="not selected"
NVIDIA_DRIVER_SOURCE="not selected"
NVIDIA_DRIVER_PACKAGE_FAMILY="unknown"
NVIDIA_DRIVER_PACKAGE=""
NVIDIA_DKMS_PACKAGE=""

# =========================
# Helpers
# =========================
log() {
        printf '\n[%s] %s\n' "$(date '+%F %T')" "$*"
}

die() {
        echo "ERROR: $*" >&2
        if declare -F clouddeploy_failure_diagnostics >/dev/null 2>&1; then
                clouddeploy_failure_diagnostics "$*" || true
        fi
        if [[ "${CLOUDDEPLOY_CONTINUE:-0}" == "1" ]]; then
                systemctl disable clouddeploy-wayland-continue.service >/dev/null 2>&1 || true
                systemctl reset-failed clouddeploy-wayland-continue.service >/dev/null 2>&1 || true
                rm -f "${REBOOT_MARKER}" "${REBOOT_REASON_FILE}" 2>/dev/null || true
                rm -f "${CLOUDDEPLOY_STATE_DIR}/reboot-needed" 2>/dev/null || true
        fi
        exit 1
}

clouddeploy_failure_trap() {
        local line="$1"
        local rc="$2"

        echo "Failed at line ${line} during phase '${CURRENT_PHASE:-unknown}'" >&2
        if declare -F clouddeploy_failure_diagnostics >/dev/null 2>&1; then
                clouddeploy_failure_diagnostics "line ${line}" || true
        fi
        if [[ "${CLOUDDEPLOY_CONTINUE:-0}" == "1" ]]; then
                systemctl disable clouddeploy-wayland-continue.service >/dev/null 2>&1 || true
                systemctl reset-failed clouddeploy-wayland-continue.service >/dev/null 2>&1 || true
                rm -f "${REBOOT_MARKER}" "${REBOOT_REASON_FILE}" 2>/dev/null || true
                rm -f "${CLOUDDEPLOY_STATE_DIR}/reboot-needed" 2>/dev/null || true
        fi
        exit "${rc}"
}

trap 'clouddeploy_failure_trap "$LINENO" "$?"' ERR

set_phase() {
        CURRENT_PHASE="$1"
        log "Phase: ${CURRENT_PHASE}"
}

mark_phase_done() {
        local marker="$1"

        install -d -m 0755 "${CLOUDDEPLOY_STATE_DIR}"
        touch "${CLOUDDEPLOY_STATE_DIR}/${marker}"
}

require_root() {
        [[ "${EUID}" -eq 0 ]] || die "Run this script as root."
}

user_home() {
        getent passwd "$1" | cut -d: -f6
}

user_is_root_identity() {
        local user="$1"

        [[ "${user}" == "root" ]] && return 0
        [[ "$(id -u "${user}" 2>/dev/null || echo -1)" == "0" ]]
}

normalize_clouddeploy_users() {
        local fallback

        fallback="$(clouddeploy_default_real_user)"

        if user_is_root_identity "${HEADLESS_USER}"; then
                if [[ "${ALLOW_ROOT_SESSION}" == "1" ]]; then
                        log "ALLOW_ROOT_SESSION=1; allowing HEADLESS_USER=${HEADLESS_USER}"
                else
                        log "HEADLESS_USER resolved to root in this context; using real user '${fallback}' instead"
                        HEADLESS_USER="${fallback}"
                fi
        fi

        if user_is_root_identity "${SUNSHINE_USER}"; then
                if [[ "${ALLOW_ROOT_SESSION}" == "1" ]]; then
                        log "ALLOW_ROOT_SESSION=1; allowing SUNSHINE_USER=${SUNSHINE_USER}"
                else
                        log "SUNSHINE_USER resolved to root in this context; using HEADLESS_USER='${HEADLESS_USER}' instead"
                        SUNSHINE_USER="${HEADLESS_USER}"
                fi
        fi

        case "${STREAM_MODE}" in
                plasma|plasma6|kwin|realvt)
                        if user_is_root_identity "${HEADLESS_USER}" && [[ "${ALLOW_ROOT_SESSION}" != "1" ]]; then
                                die "STREAM_MODE=${STREAM_MODE} must not run KWin/Plasma as root. Set HEADLESS_USER to a real UID>=1000 user."
                        fi
                        ;;
        esac
}

ensure_headless_user_context() {
        normalize_clouddeploy_users

        if ! id "${HEADLESS_USER}" >/dev/null 2>&1; then
                if [[ "${HEADLESS_USER}" == "root" && "${ALLOW_ROOT_SESSION}" != "1" ]]; then
                        die "Refusing to create/use root as HEADLESS_USER"
                fi
                useradd -m -s /bin/bash "${HEADLESS_USER}"
        fi

        HEADLESS_UID="$(id -u "${HEADLESS_USER}")"
        if [[ "${HEADLESS_UID}" == "0" && "${ALLOW_ROOT_SESSION}" != "1" ]]; then
                die "HEADLESS_USER=${HEADLESS_USER} has UID 0; refusing to run Wayland/KWin session as root"
        fi

        HOME_DIR="$(user_home "${HEADLESS_USER}")"
        [[ -n "${HOME_DIR}" ]] || die "Could not determine home directory for ${HEADLESS_USER}"
        RUNTIME_DIR="/run/user/${HEADLESS_UID}"
        if [[ "${RUNTIME_DIR}" == "/run/user/0" && "${ALLOW_ROOT_SESSION}" != "1" ]]; then
                die "Refusing RUNTIME_DIR=/run/user/0 for non-root CloudDeploy session"
        fi
        KWIN_DISPLAY="${KWIN_WAYLAND_DISPLAY}"
        COMPOSITOR_SERVICE="$(service_for_mode)"
}

detect_nvidia_busid() {
        local busid=""
        if command -v nvidia-xconfig >/dev/null 2>&1; then
                busid="$(nvidia-xconfig --query-gpu-info 2>/dev/null | awk -F': ' '/PCI BusID/ {print $2; exit}')"
        fi

        if [[ -z "${busid}" ]]; then
                local raw
                raw="$(lspci -Dnnd 10de: | awk 'NR==1{print $1}')"
                [[ -n "${raw}" ]] || return 1

                local bus dev func
                IFS=':.' read -r _ bus dev func <<<"${raw}"
                busid="PCI:$((16#${bus})):$((16#${dev})):$((16#${func}))"
        fi

        printf '%s\n' "${busid}"
}

detect_nvidia_render_node() {
        local raw link
        raw="$(nvidia-smi --query-gpu=pci.bus_id --format=csv,noheader | head -n1 | tr '[:upper:]' '[:lower:]')"
        [[ -n "${raw}" ]] || return 1
        link="/dev/dri/by-path/pci-${raw}-render"
        [[ -e "${link}" ]] || return 1
        readlink -f "${link}"
}

detect_nvidia_drm_card() {
        local card vendor
        for card in /sys/class/drm/card[0-9]; do
                [[ -e "${card}/device/vendor" ]] || continue
                vendor="$(cat "${card}/device/vendor" 2>/dev/null || true)"
                if [[ "${vendor}" == "0x10de" ]]; then
                        echo "/dev/dri/$(basename "${card}")"
                        return 0
                fi
        done
        return 1
}

nvidia_modules_present_for_running_kernel() {
        local kernel
        kernel="$(uname -r)"
        [[ -d "/lib/modules/${kernel}" ]] || return 1
        find "/lib/modules/${kernel}" -type f \
                \( -name 'nvidia*.ko' -o -name 'nvidia*.ko.xz' -o -name 'nvidia*.ko.zst' \) \
                2>/dev/null | grep -q .
}

safe_kernel_cleanup_candidate() {
        local candidate="$1"
        local running
        local running_abi
        local running_generic_base

        running="$(uname -r)"
        running_abi="${running%-*}"
        running_generic_base="${running%-generic}"

        [[ -n "${candidate:-}" ]] || return 1

        case "${candidate}" in
                "${running}"|"${running_abi}"|"${running_generic_base}")
                        return 1
                        ;;
        esac

        [[ "${candidate}" != *"/"* ]] || return 1
        [[ "${candidate}" != "." ]] || return 1
        [[ "${candidate}" != ".." ]] || return 1
        [[ "${candidate}" =~ ^[A-Za-z0-9._:+-]+$ ]] || return 1
        return 0
}

write_phase2_edids() {
        install -d -m 0755 /lib/firmware/edid

        local script_dir edid_script
        script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

        edid_script="${script_dir}/scripts/write-edids.py"
        if [[ ! -f "${edid_script}" ]]; then
                edid_script="${script_dir}/write-edids.py"
        fi

        [[ -f "${edid_script}" ]] || die "Could not find write-edids.py in scripts/ or repo root"

        python3 "${edid_script}"
}

select_phase2_edid_file() {
        case "${EDID_PROFILE}" in
                auto)
                        if [[ "${ENABLE_HDR}" == "1" ]]; then
                                echo "virtual-4k120-hdr.bin"
                        else
                                echo "virtual-4k120-sdr.bin"
                        fi
                        ;;
                1080p-sdr)
                        echo "virtual-1080p-sdr.bin"
                        ;;
                4k60-sdr)
                        echo "virtual-4k60-sdr.bin"
                        ;;
                4k120-sdr)
                        echo "virtual-4k120-sdr.bin"
                        ;;
                4k120-hdr)
                        echo "virtual-4k120-hdr.bin"
                        ;;
                *)
                        die "Unsupported EDID_PROFILE: ${EDID_PROFILE}"
                        ;;
        esac
}

service_for_mode() {
        case "${STREAM_MODE}" in
                plasma|plasma6|kwin|realvt)
                        echo "kwin-realvt.service"
                        ;;
                weston)
                        echo "weston-kms-session.service"
                        ;;
                gamescope)
                        die "STREAM_MODE=gamescope is reserved for the later game/HDR path."
                        ;;
                *)
                        die "Unsupported STREAM_MODE='${STREAM_MODE}'. Supported now: plasma, plasma6, kwin, weston."
                        ;;
        esac
}

plasma6_mode_active() {
        [[ "${STREAM_MODE}" == "plasma6" || "${ENABLE_PLASMA6}" == "1" ]]
}

package_candidate_version() {
        local pkg="$1"
        local policy_out

        policy_out="$(apt-cache policy "${pkg}" 2>/dev/null || true)"
        printf '%s\n' "${policy_out}" \
                | awk -F': ' '/^[[:space:]]*Candidate:/ { print $2; exit }' \
                || true
}

ubuntu_version_id() {
        local version_id=""
        if [[ -r /etc/os-release ]]; then
                # shellcheck disable=SC1091
                . /etc/os-release
                version_id="${VERSION_ID:-}"
        fi
        printf '%s\n' "${version_id}"
}

ubuntu_codename() {
        local codename=""
        if [[ -r /etc/os-release ]]; then
                # shellcheck disable=SC1091
                . /etc/os-release
                codename="${VERSION_CODENAME:-${UBUNTU_CODENAME:-}}"
        fi
        printf '%s\n' "${codename}"
}

ubuntu_os_summary() {
        local pretty="" version codename
        if [[ -r /etc/os-release ]]; then
                # shellcheck disable=SC1091
                . /etc/os-release
                pretty="${PRETTY_NAME:-}"
        fi
        version="$(ubuntu_version_id)"
        codename="$(ubuntu_codename)"
        printf '%s\n' "${pretty:-ubuntu ${version:-unknown} ${codename:-unknown}}"
}

version_major() {
        local version="$1"
        version="${version#*:}"
        printf '%s\n' "${version}" | sed -nE 's/^[^0-9]*([0-9]+).*/\1/p'
}

installed_plasma_version() {
        if command -v plasmashell >/dev/null 2>&1; then
                plasmashell --version 2>/dev/null | head -n1 || true
        elif dpkg-query -W -f='plasma-workspace ${Version}\n' plasma-workspace 2>/dev/null; then
                return 0
        fi
}

installed_kwin_version() {
        if command -v kwin_wayland >/dev/null 2>&1; then
                kwin_wayland --version 2>/dev/null | head -n1 || true
        elif dpkg-query -W -f='kwin-wayland ${Version}\n' kwin-wayland 2>/dev/null; then
                return 0
        fi
}

plasma6_packages_available_from_native_repos() {
        local pkg candidate major
        for pkg in plasma-workspace kwin-wayland plasma-desktop; do
                candidate="$(package_candidate_version "${pkg}")"
                [[ -n "${candidate}" && "${candidate}" != "(none)" ]] || return 1
                major="$(version_major "${candidate}")"
                [[ "${major}" == "6" ]] || return 1
        done
}

require_plasma6_available_from_native_repos() {
        local pkg candidate

        plasma6_mode_active || return 0

        log "STREAM_MODE=plasma6 / ENABLE_PLASMA6=1 requested; checking native distro Plasma 6 availability"
        if plasma6_packages_available_from_native_repos; then
                for pkg in plasma-workspace kwin-wayland plasma-desktop; do
                        candidate="$(package_candidate_version "${pkg}")"
                        log "Native Plasma 6 candidate: ${pkg}=${candidate}"
                done
                candidate="$(package_candidate_version plasma-workspace-wayland)"
                if [[ -n "${candidate}" && "${candidate}" != "(none)" ]]; then
                        log "Optional Plasma Wayland package available: plasma-workspace-wayland=${candidate}"
                else
                        log "Optional Plasma Wayland package absent: plasma-workspace-wayland; skipping on this distro"
                fi
                return 0
        fi

        echo "Plasma 6/KWin 6 package candidates from current apt repositories:" >&2
        for pkg in plasma-workspace kwin-wayland plasma-desktop plasma-workspace-wayland; do
                candidate="$(package_candidate_version "${pkg}")"
                echo "  ${pkg}: ${candidate:-missing}" >&2
        done
        die "STREAM_MODE=plasma6 requires Plasma 6/KWin 6 packages from the current distro repositories. They are unavailable on this base image; use a newer base image/distro. CloudDeploy will not add KDE Neon repos to Ubuntu 24.04."
}

kde_plasma_package_list() {
        local pkg candidate
        local required_packages=(
                plasma-workspace
                kwin-wayland
                plasma-desktop
                kscreen
                weston
                xwayland
                seatd
                xdg-desktop-portal
                xdg-desktop-portal-kde
        )
        local optional_packages=(
                plasma-workspace-wayland
                qdbus-qt5
                qdbus6
                qt6-tools-dev-tools
                qttools5-dev-tools
                kde-spectacle
        )

        for pkg in "${required_packages[@]}"; do
                printf '%s\n' "${pkg}"
        done

        for pkg in "${optional_packages[@]}"; do
                candidate="$(package_candidate_version "${pkg}")"
                if [[ -n "${candidate}" && "${candidate}" != "(none)" ]]; then
                        printf 'Optional KDE/Plasma package available: %s=%s\n' "${pkg}" "${candidate}" >&2
                        printf '%s\n' "${pkg}"
                else
                        printf 'Optional KDE/Plasma package absent; skipping: %s\n' "${pkg}" >&2
                fi
        done
}

validate_plasma6_runtime_commands() {
        local missing=()
        local cmd

        plasma6_mode_active || return 0

        for cmd in kwin_wayland plasmashell startplasma-wayland kscreen-doctor; do
                command -v "${cmd}" >/dev/null 2>&1 || missing+=("${cmd}")
        done

        if ! find_qdbus_bin >/dev/null 2>&1; then
                missing+=("qdbus/qdbus-qt5/qdbus6")
        fi

        if (( ${#missing[@]} > 0 )); then
                echo "Plasma/KWin command diagnostics:" >&2
                for cmd in kwin_wayland plasmashell startplasma-wayland kscreen-doctor qdbus qdbus-qt5 qdbus6 /usr/lib/qt5/bin/qdbus /usr/lib/qt6/bin/qdbus; do
                        if command -v "${cmd}" >/dev/null 2>&1; then
                                echo "  ${cmd}: $(command -v "${cmd}")" >&2
                        elif [[ -x "${cmd}" ]]; then
                                echo "  ${cmd}: present" >&2
                        else
                                echo "  ${cmd}: missing" >&2
                        fi
                done
                die "STREAM_MODE=plasma6 is missing required Plasma runtime command(s): ${missing[*]}"
        fi

        log "Plasma 6 runtime commands found: kwin_wayland, plasmashell, startplasma-wayland, kscreen-doctor, $(find_qdbus_bin)"
}

write_clouddeploy_env_file() {
        ensure_headless_user_context
        install -m 0600 /dev/null "${CLOUDDEPLOY_ENV_FILE}"

        {
                printf 'HEADLESS_USER=%q\n' "${HEADLESS_USER}"
                printf 'SUNSHINE_USER=%q\n' "${SUNSHINE_USER}"
                printf 'SUNSHINE_PASS=%q\n' "${SUNSHINE_PASS}"
                printf 'TAILSCALE_AUTHKEY=%q\n' "${TAILSCALE_AUTHKEY}"
                printf 'SUNSHINE_DEB_URL=%q\n' "${SUNSHINE_DEB_URL}"
                printf 'FORCED_CONNECTOR=%q\n' "${FORCED_CONNECTOR}"
                printf 'TARGET_WIDTH=%q\n' "${TARGET_WIDTH}"
                printf 'TARGET_HEIGHT=%q\n' "${TARGET_HEIGHT}"
                printf 'TARGET_FPS=%q\n' "${TARGET_FPS}"
                printf 'ENABLE_HDR=%q\n' "${ENABLE_HDR}"
                printf 'ENABLE_PLASMA6=%q\n' "${ENABLE_PLASMA6}"
                printf 'EDID_PROFILE=%q\n' "${EDID_PROFILE}"
                printf 'KWIN_VTNR=%q\n' "${KWIN_VTNR}"
                printf 'KWIN_WAYLAND_DISPLAY=%q\n' "${KWIN_WAYLAND_DISPLAY}"
                printf 'WESTON_WAYLAND_DISPLAY=%q\n' "${WESTON_WAYLAND_DISPLAY}"
                printf 'WESTON_MODE=%q\n' "${WESTON_MODE}"
                printf 'SUNSHINE_ENCODER=%q\n' "${SUNSHINE_ENCODER}"
                printf 'SUNSHINE_DRM_DEVICE=%q\n' "${SUNSHINE_DRM_DEVICE}"
                printf 'SESSION_BACKEND=%q\n' "${SESSION_BACKEND}"
                printf 'STREAM_MODE=%q\n' "${STREAM_MODE}"
                printf 'PLASMA_LAUNCH_MODE=%q\n' "${PLASMA_LAUNCH_MODE}"
                printf 'SUNSHINE_AV1_MODE=%q\n' "${SUNSHINE_AV1_MODE}"
                printf 'SUNSHINE_HEVC_MODE=%q\n' "${SUNSHINE_HEVC_MODE}"
                printf 'RUNTIME_DIR=%q\n' "${RUNTIME_DIR}"
                printf 'INSTALL_OPTIONAL_APPS=%q\n' "${INSTALL_OPTIONAL_APPS}"
                printf 'TARGET_NVIDIA_DRIVER_MAJOR=%q\n' "${TARGET_NVIDIA_DRIVER_MAJOR}"
                printf 'INSTALL_CUDA_TOOLKIT=%q\n' "${INSTALL_CUDA_TOOLKIT}"
                printf 'CUDA_TOOLKIT_PACKAGE=%q\n' "${CUDA_TOOLKIT_PACKAGE}"
                printf 'CUDA_INSTALL_METHOD=%q\n' "${CUDA_INSTALL_METHOD}"
                printf 'REQUIRE_CUDA_TOOLKIT=%q\n' "${REQUIRE_CUDA_TOOLKIT}"
                printf 'CUDA_RUNFILE_VERSION=%q\n' "${CUDA_RUNFILE_VERSION}"
                printf 'CUDA_RUNFILE_DRIVER_VERSION=%q\n' "${CUDA_RUNFILE_DRIVER_VERSION}"
                printf 'CUDA_RUNFILE_URL=%q\n' "${CUDA_RUNFILE_URL}"
                printf 'FORCE_DRIVER_UPGRADE=%q\n' "${FORCE_DRIVER_UPGRADE}"
                printf 'SUNSHINE_SOURCE_MODE=%q\n' "${SUNSHINE_SOURCE_MODE}"
                printf 'SUNSHINE_FORK_REPO=%q\n' "${SUNSHINE_FORK_REPO}"
                printf 'SUNSHINE_DIAGNOSTIC_FORK_BRANCH=%q\n' "${SUNSHINE_DIAGNOSTIC_FORK_BRANCH}"
                printf 'SUNSHINE_CLEAN_FORK_BRANCH=%q\n' "${SUNSHINE_CLEAN_FORK_BRANCH}"
                printf 'SUNSHINE_FORK_BRANCH=%q\n' "${SUNSHINE_FORK_BRANCH}"
                printf 'SUNSHINE_BUILD_DIR=%q\n' "${SUNSHINE_BUILD_DIR}"
                printf 'SUNSHINE_BUILD_JOBS=%q\n' "${SUNSHINE_BUILD_JOBS}"
                printf 'SUNSHINE_INSTALL_BIN=%q\n' "${SUNSHINE_INSTALL_BIN}"
                printf 'SUNSHINE_ENABLE_CUDA_MODULE=%q\n' "${SUNSHINE_ENABLE_CUDA_MODULE}"
                printf 'ENABLE_USER_NOPASSWD_SUDO=%q\n' "${ENABLE_USER_NOPASSWD_SUDO}"
                printf 'ALLOW_ROOT_SESSION=%q\n' "${ALLOW_ROOT_SESSION}"
        } > "${CLOUDDEPLOY_ENV_FILE}"

        chmod 0600 "${CLOUDDEPLOY_ENV_FILE}"
}

validate_tailscale_authkey() {
        local lower_key

        [[ -n "${TAILSCALE_AUTHKEY}" ]] || return 0

        if [[ "${TAILSCALE_AUTHKEY}" =~ [[:space:]] ]]; then
                die "TAILSCALE_AUTHKEY must be a single-line key. Generate a fresh key; do not paste line breaks."
        fi

        lower_key="$(printf '%s' "${TAILSCALE_AUTHKEY}" | tr '[:upper:]' '[:lower:]')"
        case "${lower_key}" in
                your-*|*placeholder*|tailscale-key|changeme|change-me|example|example-key)
                        die "TAILSCALE_AUTHKEY is placeholder text. Generate a fresh single-line key; do not paste line breaks."
                        ;;
        esac
}

ensure_headless_user_admin_access() {
        if [[ "${ENABLE_USER_NOPASSWD_SUDO}" != "1" ]]; then
                log "ENABLE_USER_NOPASSWD_SUDO=0; not installing nopasswd sudoers recovery file"
                return 0
        fi

        log "Ensuring ${HEADLESS_USER} has sudo recovery access"
        usermod -aG sudo "${HEADLESS_USER}" || true
        install -d -m 0755 /etc/sudoers.d
        printf '%s ALL=(ALL) NOPASSWD:ALL\n' "${HEADLESS_USER}" > /etc/sudoers.d/90-clouddeploy-user
        chmod 0440 /etc/sudoers.d/90-clouddeploy-user
        if command -v visudo >/dev/null 2>&1; then
                visudo -cf /etc/sudoers.d/90-clouddeploy-user >/dev/null
        fi
}

print_continuation_state() {
        echo "=== CloudDeploy continuation state ==="
        echo "CLOUDDEPLOY_CONTINUE=${CLOUDDEPLOY_CONTINUE:-0}"
        echo "CLOUDDEPLOY_CONTINUE_REASON=${CLOUDDEPLOY_CONTINUE_REASON:-none}"
        echo "Reboot marker: ${REBOOT_MARKER} $([[ -f "${REBOOT_MARKER}" ]] && echo present || echo absent)"
        echo "Reboot reason: $(cat "${REBOOT_REASON_FILE}" 2>/dev/null || echo none)"
        systemctl is-enabled clouddeploy-wayland-continue.service 2>/dev/null || true
        systemctl is-active clouddeploy-wayland-continue.service 2>/dev/null || true
}

cleanup_stale_continuation_state_for_manual_rerun() {
        if [[ "${CLOUDDEPLOY_CONTINUE:-0}" == "1" ]]; then
                print_continuation_state
                return 0
        fi

        if [[ -f "${REBOOT_MARKER}" || -f "${REBOOT_REASON_FILE}" ]] \
                || systemctl is-enabled clouddeploy-wayland-continue.service >/dev/null 2>&1; then
                log "Detected stale CloudDeploy continuation state during manual/root rerun; disabling old continuation service"
                print_continuation_state
                systemctl disable clouddeploy-wayland-continue.service >/dev/null 2>&1 || true
                systemctl reset-failed clouddeploy-wayland-continue.service >/dev/null 2>&1 || true
                rm -f "${REBOOT_MARKER}" "${REBOOT_REASON_FILE}" || true
        fi
}

clouddeploy_failure_diagnostics() {
        local reason="${1:-unknown}"

        {
                echo
                echo "=== CloudDeploy failure diagnostics ==="
                echo "Reason: ${reason}"
                echo "Failed phase: ${CURRENT_PHASE:-unknown}"
                echo "Continuation marker exists: $([[ -f "${REBOOT_MARKER}" ]] && echo yes || echo no)"
                echo "Continuation reason: $(cat "${REBOOT_REASON_FILE}" 2>/dev/null || echo none)"
                echo
                echo "Useful commands:"
                echo "  systemctl status clouddeploy-manual-rerun.service --no-pager -l"
                echo "  journalctl -u clouddeploy-manual-rerun.service -n 300 --no-pager -l"
                echo "  dpkg --audit"
                echo "  nvidia-smi || true"
                echo "  dkms status || true"
                echo "  systemctl cat kwin-realvt.service || true"
                echo
                echo "=== dpkg --audit ==="
                dpkg --audit 2>/dev/null || true
                echo
                echo "=== nvidia-smi ==="
                nvidia-smi 2>/dev/null || true
                echo
                echo "=== dkms status ==="
                dkms status 2>/dev/null || true
                echo
                echo "=== kwin-realvt.service exists ==="
                systemctl cat kwin-realvt.service 2>/dev/null | sed -n '1,80p' || true
        } >&2
}

install_continuation_service() {
        local script_path
        script_path="$(readlink -f "$0")"

        cat > /usr/local/sbin/clouddeploy-wayland-continue.sh <<EOF
#!/usr/bin/env bash
set -euo pipefail

[[ -f "${REBOOT_MARKER}" ]] || exit 0
reason="\$(cat "${REBOOT_REASON_FILE}" 2>/dev/null || echo unknown)"
CLOUDDEPLOY_CONTINUE=1 CLOUDDEPLOY_CONTINUE_REASON="\${reason}" /bin/bash "${script_path}"
EOF
        chmod 0755 /usr/local/sbin/clouddeploy-wayland-continue.sh

        cat > /etc/systemd/system/clouddeploy-wayland-continue.service <<EOF
[Unit]
Description=Continue CloudDeploy Wayland after reboot
After=network-online.target
Wants=network-online.target

[Service]
Type=oneshot
EnvironmentFile=-${CLOUDDEPLOY_ENV_FILE}
ExecStart=/usr/local/sbin/clouddeploy-wayland-continue.sh

[Install]
WantedBy=multi-user.target
EOF

        systemctl daemon-reload
        systemctl enable clouddeploy-wayland-continue.service
}

schedule_reboot_for_continuation() {
        local reason="$1"
        local message="$2"

        ensure_headless_user_context
        ensure_headless_user_admin_access
        write_clouddeploy_env_file
        install_continuation_service
        echo "${reason}" > "${REBOOT_REASON_FILE}"
        touch "${REBOOT_MARKER}"
        install -d -m 0755 "${CLOUDDEPLOY_STATE_DIR}"
        touch "${CLOUDDEPLOY_STATE_DIR}/reboot-needed"
        log "${message}"
        reboot
        exit 0
}

strip_clouddeploy_args_from_cmdline() {
        local cmdline="$1"
        local token
        local -a tokens
        local -a kept

        read -r -a tokens <<<"${cmdline}"
        for token in "${tokens[@]}"; do
                case "${token}" in
                        drm.edid_firmware=*edid/virtual-*.bin|video=${FORCED_CONNECTOR}:e|video=DP-2:d|nvidia-drm.modeset=*|nvidia-drm.fbdev=*)
                                ;;
                        *)
                                kept+=("${token}")
                                ;;
                esac
        done

        printf '%s\n' "${kept[*]}"
}

sanitize_grub_default_cmdline() {
        local current cleaned
        [[ -f /etc/default/grub ]] || return 0

        current="$(sed -n 's/^GRUB_CMDLINE_LINUX_DEFAULT="\(.*\)"$/\1/p' /etc/default/grub | head -n1 || true)"
        [[ -n "${current}" ]] || return 0

        cleaned="$(strip_clouddeploy_args_from_cmdline "${current}")"
        sed -i "s|^GRUB_CMDLINE_LINUX_DEFAULT=.*|GRUB_CMDLINE_LINUX_DEFAULT=\"${cleaned}\"|" /etc/default/grub
}

write_phase2_grub_override() {
        local arg_edid="$1"
        local arg_video="$2"
        local arg_video_disable="$3"
        local arg_modeset="$4"
        local arg_fbdev="$5"

        install -d -m 0755 /etc/default/grub.d

        cat > "${GRUB_OVERRIDE_FILE}" <<EOF
GRUB_CMDLINE_LINUX_DEFAULT="${arg_edid} ${arg_video} ${arg_video_disable} ${arg_modeset} ${arg_fbdev}"
EOF
}

cmdline_has_required_phase2_args() {
        local edid_file="$1"
        local arg_edid="drm.edid_firmware=${FORCED_CONNECTOR}:edid/${edid_file}"
        local arg_video="video=${FORCED_CONNECTOR}:e"
        local arg_video_disable="video=DP-2:d"
        local arg_modeset="nvidia-drm.modeset=1"
        local arg_fbdev="nvidia-drm.fbdev=1"

        grep -qF "${arg_edid}" /proc/cmdline \
                && grep -qF "${arg_video}" /proc/cmdline \
                && grep -qF "${arg_video_disable}" /proc/cmdline \
                && grep -qF "${arg_modeset}" /proc/cmdline \
                && grep -qF "${arg_fbdev}" /proc/cmdline
}

print_edid_diagnostics() {
        local edid_file="$1"

        echo "=== EDID diagnostics for ${FORCED_CONNECTOR} ==="
        dmesg | grep -Ei "edid|firmware|${FORCED_CONNECTOR}|nvidia" || true

        if command -v edid-decode >/dev/null 2>&1; then
                echo "=== edid-decode /lib/firmware/edid/${edid_file} ==="
                edid-decode "/lib/firmware/edid/${edid_file}" || true

                for edid_path in /sys/class/drm/card*-${FORCED_CONNECTOR}/edid; do
                        [[ -f "${edid_path}" ]] || continue
                        echo "=== edid-decode ${edid_path} ==="
                        edid-decode "${edid_path}" || true
                done
        fi
}

connector_forced_connected() {
        local status_path
        for status_path in /sys/class/drm/card*-${FORCED_CONNECTOR}/status; do
                [[ -f "${status_path}" ]] || continue
                if grep -qx 'connected' "${status_path}"; then
                        return 0
                fi
        done
        return 1
}

connector_has_mode() {
        local wanted_mode="$1"
        local mode_path

        for mode_path in /sys/class/drm/card*-${FORCED_CONNECTOR}/modes; do
                [[ -f "${mode_path}" ]] || continue
                if grep -qx "${wanted_mode}" "${mode_path}"; then
                        return 0
                fi
        done
        return 1
}

validate_phase2_display_state() {
        local edid_file="$1"
        local target_mode="${TARGET_WIDTH}x${TARGET_HEIGHT}"

        cmdline_has_required_phase2_args "${edid_file}" \
                || die "Kernel cmdline missing required EDID/DRM args"

        if ! connector_forced_connected; then
                echo "${FORCED_CONNECTOR} is not connected after reboot validation"
                print_edid_diagnostics "${edid_file}"
                die "Forced connector ${FORCED_CONNECTOR} did not report connected"
        fi

        if connector_has_mode "${target_mode}"; then
                return 0
        fi

        echo "${FORCED_CONNECTOR} is connected but ${target_mode} is not exposed"
        if connector_has_mode "1920x1080"; then
                echo "Detected fallback 1920x1080 mode only; dumping EDID diagnostics"
        fi
        print_edid_diagnostics "${edid_file}"
        die "Required mode ${target_mode} not exposed on ${FORCED_CONNECTOR}"
}

repair_initramfs_tools_config() {
        local conf="/etc/initramfs-tools/initramfs.conf"
        local compress modules

        install -d -m 0755 /etc/initramfs-tools
        touch "${conf}"

        compress="$(awk -F= '/^[[:space:]]*COMPRESS[[:space:]]*=/ {gsub(/[[:space:]]/, "", $2); print $2; exit}' "${conf}" 2>/dev/null || true)"
        if [[ -z "${compress}" ]]; then
                log "Repairing initramfs-tools config: setting COMPRESS=gzip"
                if grep -Eq '^[[:space:]]*COMPRESS[[:space:]]*=' "${conf}"; then
                        sed -i -E 's/^[[:space:]]*COMPRESS[[:space:]]*=.*/COMPRESS=gzip/' "${conf}"
                else
                        printf '\nCOMPRESS=gzip\n' >> "${conf}"
                fi
        fi

        modules="$(awk -F= '/^[[:space:]]*MODULES[[:space:]]*=/ {gsub(/[[:space:]]/, "", $2); print $2; exit}' "${conf}" 2>/dev/null || true)"
        case "${modules}" in
                ""|.|none|dep|most|netboot|list)
                        if [[ -z "${modules}" || "${modules}" == "." ]]; then
                                log "Repairing initramfs-tools config: setting MODULES=most"
                                if grep -Eq '^[[:space:]]*MODULES[[:space:]]*=' "${conf}"; then
                                        sed -i -E 's/^[[:space:]]*MODULES[[:space:]]*=.*/MODULES=most/' "${conf}"
                                else
                                        printf 'MODULES=most\n' >> "${conf}"
                                fi
                        fi
                        ;;
                *)
                        log "Repairing unsupported initramfs-tools MODULES=${modules}; setting MODULES=most"
                        sed -i -E 's/^[[:space:]]*MODULES[[:space:]]*=.*/MODULES=most/' "${conf}"
                        ;;
        esac
}

update_initramfs_clouddeploy() {
        repair_dpkg_state_if_needed
        repair_initramfs_tools_config
        DEBIAN_FRONTEND=noninteractive update-initramfs "$@"
}

update_grub_clouddeploy() {
        repair_dpkg_state_if_needed
        update-grub
}

ensure_phase2_kernel_args() {
        local edid_file="$1"
        local arg_edid="drm.edid_firmware=${FORCED_CONNECTOR}:edid/${edid_file}"
        local arg_video="video=${FORCED_CONNECTOR}:e"
        local arg_video_disable="video=DP-2:d"
        local arg_modeset="nvidia-drm.modeset=1"
        local arg_fbdev="nvidia-drm.fbdev=1"

        if grep -qF "${arg_edid}" /proc/cmdline \
                && grep -qF "${arg_video}" /proc/cmdline \
                && grep -qF "${arg_video_disable}" /proc/cmdline \
                && grep -qF "${arg_modeset}" /proc/cmdline \
                && grep -qF "${arg_fbdev}" /proc/cmdline; then
                log "Kernel cmdline already contains required EDID and DRM args"
                return 0
        fi

        if [[ "${CLOUDDEPLOY_CONTINUE:-0}" == "1" ]]; then
                die "Kernel cmdline is still missing required EDID/DRM args after reboot"
        fi

        log "Applying GRUB drop-in kernel args for ${FORCED_CONNECTOR} using ${edid_file}"
        write_phase2_grub_override "${arg_edid}" "${arg_video}" "${arg_video_disable}" "${arg_modeset}" "${arg_fbdev}"
        repair_dpkg_state_if_needed
        update_initramfs_clouddeploy -u
        update_grub_clouddeploy

        ensure_headless_user_context
        ensure_headless_user_admin_access
        write_clouddeploy_env_file
        install_continuation_service
        echo "edid-kernel-args" > "${REBOOT_REASON_FILE}"
        touch "${REBOOT_MARKER}"
        install -d -m 0755 "${CLOUDDEPLOY_STATE_DIR}"
        touch "${CLOUDDEPLOY_STATE_DIR}/reboot-needed"

        log "Rebooting to apply EDID and DRM kernel arguments"
        reboot
        exit 0
}

nvidia_driver_ready() {
        local version
        command -v nvidia-smi >/dev/null 2>&1 || return 1
        version="$(nvidia-smi --query-gpu=driver_version --format=csv,noheader 2>/dev/null | head -n1 || true)"
        [[ "${version}" =~ ^[0-9]+([.][0-9]+)+$ ]]
}

current_nvidia_driver_version() {
        local version
        if command -v nvidia-smi >/dev/null 2>&1; then
                version="$(nvidia-smi --query-gpu=driver_version --format=csv,noheader 2>/dev/null | head -n1 || true)"
                if [[ "${version}" =~ ^[0-9]+([.][0-9]+)+$ ]]; then
                        printf '%s\n' "${version}"
                fi
        fi
}

current_nvidia_driver_major() {
        local version
        version="$(current_nvidia_driver_version || true)"
        [[ -n "${version}" ]] || return 0
        printf '%s\n' "${version%%.*}"
}

cuda_version_line() {
        if [[ -x /usr/local/cuda/bin/nvcc ]]; then
                /usr/local/cuda/bin/nvcc --version 2>/dev/null | grep -E 'release|Cuda compilation tools' | tail -n1 || true
        elif command -v nvcc >/dev/null 2>&1; then
                nvcc --version 2>/dev/null | grep -E 'release|Cuda compilation tools' | tail -n1 || true
        fi
}

tailscale_ipv4() {
        command -v tailscale >/dev/null 2>&1 || return 0
        tailscale ip -4 2>/dev/null | head -n1 || true
}

http_status_code() {
        local url="$1"
        local code

        code="$(curl -ksS -o /dev/null -w '%{http_code}' --connect-timeout 3 --max-time 8 "${url}" 2>/dev/null || true)"
        if [[ "${code}" =~ ^[0-9][0-9][0-9]$ ]]; then
                printf '%s\n' "${code}"
        else
                printf '000\n'
        fi
}

sunshine_web_status_is_reachable() {
        case "$1" in
                200|301|302|401|403)
                        return 0
                        ;;
                *)
                        return 1
                        ;;
        esac
}

sunshine_serverinfo_status_is_reachable() {
        case "$1" in
                200|301|302)
                        return 0
                        ;;
                *)
                        return 1
                        ;;
        esac
}

force_mode_helper_count() {
        ps -eo args 2>/dev/null | awk '/clouddeploy-force-kwin-mode[.]sh/ { count++ } END { print count + 0 }'
}

find_qdbus_bin() {
        local candidate
        for candidate in qdbus qdbus-qt5 /usr/lib/qt5/bin/qdbus qdbus6 /usr/lib/qt6/bin/qdbus; do
                if command -v "${candidate}" >/dev/null 2>&1; then
                        command -v "${candidate}"
                        return 0
                elif [[ -x "${candidate}" ]]; then
                        printf '%s\n' "${candidate}"
                        return 0
                fi
        done
        return 1
}

kwin_support_information() {
        local qdbus_bin
        qdbus_bin="$(find_qdbus_bin || true)"
        [[ -n "${qdbus_bin}" ]] || return 1

        run_as_user "${HEADLESS_USER}" env \
                HOME="${HOME_DIR}" \
                XDG_RUNTIME_DIR="${RUNTIME_DIR}" \
                WAYLAND_DISPLAY="${KWIN_DISPLAY}" \
                DBUS_SESSION_BUS_ADDRESS="unix:path=${RUNTIME_DIR}/bus" \
                QT_QPA_PLATFORM=wayland \
                XDG_CURRENT_DESKTOP=KDE \
                XDG_SESSION_TYPE=wayland \
                "${qdbus_bin}" org.kde.KWin /KWin org.kde.KWin.supportInformation 2>/dev/null || true
}

kwin_connector_block_from_support_info() {
        local support_info="$1"
        printf '%s\n' "${support_info}" \
                | awk -v connector="${FORCED_CONNECTOR}" '
                        /^Name:/ {
                                if (in_block) exit
                                in_block = ($0 ~ ("Name:[[:space:]]*" connector "$"))
                        }
                        in_block { print }
                '
}

kwin_mode_line_from_support_info() {
        local support_info="$1"
        local block geometry refresh

        block="$(kwin_connector_block_from_support_info "${support_info}")"
        geometry="$(printf '%s\n' "${block}" | grep -E 'Geometry:' | tail -n1 || true)"
        refresh="$(printf '%s\n' "${block}" | grep -E 'Refresh Rate:' | tail -n1 || true)"

        if [[ -n "${geometry}" || -n "${refresh}" ]]; then
                printf '%s %s\n' "${geometry}" "${refresh}" | sed 's/[[:space:]][[:space:]]*/ /g'
        fi
}

kwin_support_reports_target_mode() {
        local support_info="$1"
        local block geometry refresh

        block="$(kwin_connector_block_from_support_info "${support_info}")"
        geometry="$(printf '%s\n' "${block}" | grep -E 'Geometry:' | tail -n1 || true)"
        refresh="$(printf '%s\n' "${block}" | sed -nE 's/.*Refresh Rate:[[:space:]]*([0-9.]+).*/\1/p' | tail -n1)"

        [[ "${geometry}" == *"Geometry: 0,0,${TARGET_WIDTH}x${TARGET_HEIGHT}"* \
                || "${geometry}" == *"Geometry: 0,0 ${TARGET_WIDTH}x${TARGET_HEIGHT}"* ]] || return 1
        [[ "${refresh}" =~ ^(119|120) ]] || return 1
}

kscreen_connector_summary() {
        command -v kscreen-doctor >/dev/null 2>&1 || return 0

        run_as_user "${HEADLESS_USER}" env \
                HOME="${HOME_DIR}" \
                XDG_RUNTIME_DIR="${RUNTIME_DIR}" \
                WAYLAND_DISPLAY="${KWIN_DISPLAY}" \
                DBUS_SESSION_BUS_ADDRESS="unix:path=${RUNTIME_DIR}/bus" \
                QT_QPA_PLATFORM=wayland \
                XDG_CURRENT_DESKTOP=KDE \
                XDG_SESSION_TYPE=wayland \
                kscreen-doctor -o 2>/dev/null \
                | grep -Ei "${FORCED_CONNECTOR}|Geometry:|Scale:|Refresh Rate:|${TARGET_WIDTH}x${TARGET_HEIGHT}.*(119|120).*[*]" \
                || true
}

live_edid_hdr_markers() {
        local edid_path

        command -v edid-decode >/dev/null 2>&1 || return 0

        for edid_path in /sys/class/drm/card*-"${FORCED_CONNECTOR}"/edid; do
                [[ -s "${edid_path}" ]] || continue
                edid-decode "${edid_path}" 2>/dev/null \
                        | grep -Ei 'HDR|EOTF|PQ|HLG|BT[.]2020|Static Metadata|SMPTE ST 2084' \
                        || true
        done | head -n 40
}

wait_for_kwin_target_mode() {
        local support_info

        for _ in $(seq 1 60); do
                support_info="$(kwin_support_information || true)"
                if kwin_support_reports_target_mode "${support_info}"; then
                        KNOWN_WESTON_MODE_LINE="$(kwin_mode_line_from_support_info "${support_info}")"
                        return 0
                fi
                sleep 1
        done

        return 1
}

apt_package_available() {
        local pkg="$1"
        local candidate

        candidate="$(package_candidate_version "${pkg}")"
        [[ -n "${candidate}" && "${candidate}" != "(none)" ]]
}

cuda_repo_distro_for_ubuntu_version() {
        local version="$1"

        case "${version}" in
                22.04) printf '%s\n' "ubuntu2204" ;;
                24.04) printf '%s\n' "ubuntu2404" ;;
                25.10) printf '%s\n' "ubuntu2510" ;;
                26.04) printf '%s\n' "ubuntu2604" ;;
                *) printf '%s\n' "ubuntu${version//./}" ;;
        esac
}

cuda_keyring_url_for_distro() {
        local distro="$1"
        printf 'https://developer.download.nvidia.com/compute/cuda/repos/%s/x86_64/cuda-keyring_1.1-1_all.deb\n' "${distro}"
}

cuda_repo_available_for_distro() {
        local distro="$1"
        local url

        [[ -n "${distro}" ]] || return 1
        url="$(cuda_keyring_url_for_distro "${distro}")"
        wget --spider -q --timeout=10 --tries=2 "${url}" >/dev/null 2>&1
}

detect_cuda_repo_distro() {
        local os_id="" version_id="" distro

        if [[ -r /etc/os-release ]]; then
                # shellcheck disable=SC1091
                . /etc/os-release
                os_id="${ID:-}"
                version_id="${VERSION_ID:-}"
        fi

        [[ "${os_id}" == "ubuntu" && -n "${version_id}" ]] || return 1

        distro="$(cuda_repo_distro_for_ubuntu_version "${version_id}")"
        if cuda_repo_available_for_distro "${distro}"; then
                printf '%s\n' "${distro}"
                return 0
        fi

        return 1
}

ensure_cuda_ubuntu_repo() {
        local os_id="" version_id="" distro tmpdeb url
        if [[ -r /etc/os-release ]]; then
                # shellcheck disable=SC1091
                . /etc/os-release
                os_id="${ID:-}"
                version_id="${VERSION_ID:-}"
        fi

        [[ "${os_id}" == "ubuntu" ]] \
                || die "CUDA/NVIDIA apt repository setup supports Ubuntu only; detected ${os_id:-unknown} ${version_id:-unknown}"

        distro="$(detect_cuda_repo_distro || true)"
        if [[ -z "${distro}" ]]; then
                CUDA_REPO_ENABLED=0
                CUDA_REPO_DISTRO=""
                NVIDIA_DRIVER_SOURCE="native Ubuntu"
                log "No official NVIDIA CUDA apt repo detected for $(ubuntu_os_summary); using native Ubuntu NVIDIA packages and CUDA runfile fallback if needed."
                return 0
        fi

        CUDA_REPO_ENABLED=1
        CUDA_REPO_DISTRO="${distro}"
        NVIDIA_DRIVER_SOURCE="CUDA repo"

        if dpkg-query -W -f='${Status}' cuda-keyring 2>/dev/null | grep -q 'install ok installed'; then
                log "CUDA apt keyring already installed for detected repo ${CUDA_REPO_DISTRO}"
                apt_update_retry
                return 0
        fi

        log "Installing NVIDIA CUDA apt keyring for ${CUDA_REPO_DISTRO}"
        tmpdeb="$(mktemp /tmp/cuda-keyring.XXXXXX.deb)"
        url="$(cuda_keyring_url_for_distro "${CUDA_REPO_DISTRO}")"
        wget -O "${tmpdeb}" "${url}"
        wait_for_apt
        dpkg -i "${tmpdeb}"
        rm -f "${tmpdeb}"
        apt_update_retry
}

cleanup_conflicting_nvidia_driver_packages() {
        local pkg branch
        local -a installed_pkgs purge_pkgs
        local -a known_branches

        known_branches=(535 550 560 565 570 575 580)

        log "Cleaning up conflicting non-target NVIDIA/CUDA driver packages"
        systemctl stop sunshine-headless.service plasma-realvt.service kwin-realvt.service plasma-shell-realvt.service weston-kms-session.service 2>/dev/null || true
        apt-mark unhold 'cuda*' 'nvidia*' 'libnvidia*' >/dev/null 2>&1 || true

        mapfile -t installed_pkgs < <(dpkg-query -W -f='${binary:Package}\n' 2>/dev/null | sort -u)
        purge_pkgs=()

        for pkg in "${installed_pkgs[@]}"; do
                case "${pkg}" in
                        cuda-drivers|cuda-drivers-*)
                                purge_pkgs+=("${pkg}")
                                continue
                                ;;
                esac

                case "${pkg}" in
                        nvidia-*|libnvidia-*|linux-modules-nvidia-*|linux-objects-nvidia-*|linux-signatures-nvidia-*|xserver-xorg-video-nvidia-*)
                                for branch in "${known_branches[@]}"; do
                                        [[ "${branch}" == "${TARGET_NVIDIA_DRIVER_MAJOR}" ]] && continue
                                        if [[ "${pkg}" =~ (^|[-_])${branch}($|[-_.]) ]]; then
                                                purge_pkgs+=("${pkg}")
                                                break
                                        fi
                                done
                                ;;
                esac
        done

        if [[ "${#purge_pkgs[@]}" -gt 0 ]]; then
                log "Purging conflicting NVIDIA/CUDA driver packages: ${purge_pkgs[*]}"
                wait_for_apt
                DEBIAN_FRONTEND=noninteractive apt-get "${APT_DPKG_OPTIONS[@]}" purge -y "${purge_pkgs[@]}" \
                        || log "NVIDIA conflict purge had errors; continuing to target driver install"
        else
                log "No obvious non-target NVIDIA driver branch packages found"
        fi

        wait_for_apt
        apt-get autoremove --purge -y || log "apt autoremove after NVIDIA cleanup failed; continuing"
        wait_for_apt
        apt-get update -o Acquire::Retries=6 -o Acquire::http::Timeout=20 || log "apt update after NVIDIA cleanup failed; continuing"
}

diagnose_nvidia_init_failure() {
        echo
        echo "=== NVIDIA init diagnostics ==="
        echo "=== nvidia-smi ==="
        nvidia-smi || true
        echo
        echo "=== uname -r ==="
        uname -r || true
        echo
        echo "=== dkms status | grep -i nvidia ==="
        dkms status 2>/dev/null | grep -i nvidia || true
        echo
        echo "=== lsmod | grep -i nvidia ==="
        lsmod 2>/dev/null | grep -i nvidia || true
        echo
        echo "=== /dev/nvidia* ==="
        ls -l /dev/nvidia* 2>/dev/null || true
        echo
        echo "=== /proc/driver/nvidia/version ==="
        cat /proc/driver/nvidia/version 2>/dev/null || true
        echo
        echo "=== /proc/driver/nvidia/gpus ==="
        ls -R /proc/driver/nvidia/gpus 2>/dev/null || true
        echo
        echo "=== lspci NVIDIA/display devices ==="
        lspci -nnk 2>/dev/null | grep -A5 -Ei 'nvidia|vga|3d|display' || true
        echo
        echo "=== dmesg NVIDIA/provider markers ==="
        dmesg -T 2>/dev/null \
                | grep -Ei 'nvidia|NVRM|GSP|Xid|RmInit|NvKmsKapiDevice|nouveau|secure|mok|vfio|drm' \
                | tail -n 240 || true
}

nvidia_provider_init_failure_seen() {
        dmesg -T 2>/dev/null | grep -Eiq 'RmInitAdapter|Xid.*62|Failed to allocate NvKmsKapiDevice'
}

fail_if_nvidia_provider_init_failure_seen() {
        if nvidia_provider_init_failure_seen; then
                die "NVIDIA driver packages installed, but the provider GPU allocation failed to initialize. This is likely a bad/dirty cloud GPU passthrough allocation, not a Plasma/Sunshine/CUDA problem. Fully power-cycle this VM from the provider panel or create a new VM/GPU allocation."
        fi
}

installed_dpkg_package() {
        dpkg-query -W -f='${binary:Package}\n' "$1" >/dev/null 2>&1
}

dpkg_package_configured_ii() {
        local status
        status="$(dpkg-query -W -f='${db:Status-Abbrev}' "$1" 2>/dev/null || true)"
        [[ "${status}" == "ii " || "${status}" == "ii" ]]
}

print_nvidia_driver_diagnostics() {
        echo
        echo "=== NVIDIA package/DKMS diagnostics ==="
        echo "=== dkms status ==="
        dkms status 2>/dev/null || true
        echo
        echo "=== dpkg NVIDIA/kernel package states ==="
        dpkg -l 2>/dev/null \
                | awk '/nvidia-dkms|nvidia-driver|linux-image|linux-headers|linux-modules/ { print }' \
                || true
        diagnose_nvidia_init_failure
}

provider_gpu_failure_message() {
        printf '%s\n' "Provider GPU initialization failure: NVIDIA GPU is present and bound to nvidia, but NVRM reports Xid 62 / RmInitAdapter failed. This is not a CloudDeploy/KDE/Sunshine issue. Power-cycle the VM from the provider panel or recreate the instance on a different host/datacenter."
}

detect_provider_gpu_init_failure() {
        local smi_out lspci_block

        lspci_block="$(lspci -nnk 2>/dev/null | grep -A5 -Ei 'nvidia|vga|3d|display' || true)"
        if ! printf '%s\n' "${lspci_block}" | grep -Eiq 'nvidia'; then
                print_nvidia_driver_diagnostics
                die "No NVIDIA GPU is visible on PCI. This is a provider allocation issue."
        fi

        smi_out="$(nvidia-smi 2>&1)" && return 0 || true
        if printf '%s\n' "${smi_out}" | grep -Eiq 'No devices were found' \
                && printf '%s\n' "${lspci_block}" | grep -Eiq 'Kernel driver in use:[[:space:]]*nvidia' \
                && dmesg -T 2>/dev/null | grep -Eiq 'Xid.*62|RmInitAdapter failed|rm_init_adapter failed'; then
                print_nvidia_driver_diagnostics
                die "$(provider_gpu_failure_message)"
        fi
}

nvidia_smi_driver_library_mismatch() {
        local smi_out
        smi_out="$(nvidia-smi 2>&1)" && return 1 || true
        printf '%s\n' "${smi_out}" | grep -Eiq 'Driver/library version mismatch'
}

nvidia_dkms_installed_for_current_kernel() {
        local current_kernel
        current_kernel="$(uname -r)"
        dkms status 2>/dev/null | grep -Eiq "nvidia(-srv)?/.*${current_kernel}.*installed|nvidia.*${current_kernel}.*installed"
}

nvidia_server_dkms_installed_for_current_kernel() {
        local current_kernel
        current_kernel="$(uname -r)"
        dkms status 2>/dev/null | grep -Eiq "nvidia-srv/.*${current_kernel}.*installed|nvidia.*${current_kernel}.*installed"
}

nvidia_current_boot_rm_failure_seen() {
        dmesg -T 2>/dev/null | grep -Eiq 'Xid.*62|RmInitAdapter failed|rm_init_adapter failed'
}

nvidia_target_dkms_package() {
        local candidate
        for candidate in \
                "nvidia-dkms-${TARGET_NVIDIA_DRIVER_MAJOR}-server" \
                "nvidia-dkms-${TARGET_NVIDIA_DRIVER_MAJOR}"
        do
                if installed_dpkg_package "${candidate}"; then
                        printf '%s\n' "${candidate}"
                        return 0
                fi
        done

        for candidate in \
                "nvidia-dkms-${TARGET_NVIDIA_DRIVER_MAJOR}-server" \
                "nvidia-dkms-${TARGET_NVIDIA_DRIVER_MAJOR}"
        do
                if apt_package_available "${candidate}"; then
                        printf '%s\n' "${candidate}"
                        return 0
                fi
        done
        printf '%s\n' "nvidia-dkms-${TARGET_NVIDIA_DRIVER_MAJOR}-server"
}

nvidia_target_driver_package() {
        local candidate
        for candidate in \
                "nvidia-driver-${TARGET_NVIDIA_DRIVER_MAJOR}-server" \
                "nvidia-driver-${TARGET_NVIDIA_DRIVER_MAJOR}" \
                "nvidia-open-${TARGET_NVIDIA_DRIVER_MAJOR}"
        do
                if installed_dpkg_package "${candidate}"; then
                        printf '%s\n' "${candidate}"
                        return 0
                fi
        done

        for candidate in \
                "nvidia-driver-${TARGET_NVIDIA_DRIVER_MAJOR}-server" \
                "nvidia-driver-${TARGET_NVIDIA_DRIVER_MAJOR}" \
                "nvidia-open-${TARGET_NVIDIA_DRIVER_MAJOR}"
        do
                if apt_package_available "${candidate}"; then
                        printf '%s\n' "${candidate}"
                        return 0
                fi
        done
        printf '%s\n' "nvidia-driver-${TARGET_NVIDIA_DRIVER_MAJOR}-server"
}

print_nvidia_target_package_policy() {
        local pkg

        echo "=== apt-cache policy for NVIDIA target ${TARGET_NVIDIA_DRIVER_MAJOR} packages ===" >&2
        for pkg in \
                "nvidia-driver-${TARGET_NVIDIA_DRIVER_MAJOR}-server" \
                "nvidia-dkms-${TARGET_NVIDIA_DRIVER_MAJOR}-server" \
                "nvidia-utils-${TARGET_NVIDIA_DRIVER_MAJOR}-server" \
                "libnvidia-encode-${TARGET_NVIDIA_DRIVER_MAJOR}-server" \
                "libnvidia-fbc1-${TARGET_NVIDIA_DRIVER_MAJOR}-server" \
                "nvidia-driver-${TARGET_NVIDIA_DRIVER_MAJOR}" \
                "nvidia-dkms-${TARGET_NVIDIA_DRIVER_MAJOR}" \
                "nvidia-utils-${TARGET_NVIDIA_DRIVER_MAJOR}" \
                "libnvidia-encode-${TARGET_NVIDIA_DRIVER_MAJOR}" \
                "libnvidia-fbc1-${TARGET_NVIDIA_DRIVER_MAJOR}" \
                "nvidia-open-${TARGET_NVIDIA_DRIVER_MAJOR}"
        do
                echo "--- ${pkg} ---" >&2
                apt-cache policy "${pkg}" >&2 2>/dev/null || true
        done
}

select_nvidia_driver_package_family() {
        local family suffix driver_pkg dkms_pkg

        for family in server non-server; do
                if [[ "${family}" == "server" ]]; then
                        suffix="${TARGET_NVIDIA_DRIVER_MAJOR}-server"
                else
                        suffix="${TARGET_NVIDIA_DRIVER_MAJOR}"
                fi

                driver_pkg="nvidia-driver-${suffix}"
                dkms_pkg="nvidia-dkms-${suffix}"
                if apt_package_available "${driver_pkg}" && apt_package_available "${dkms_pkg}"; then
                        NVIDIA_DRIVER_PACKAGE_FAMILY="${family}"
                        NVIDIA_DRIVER_PACKAGE="${driver_pkg}"
                        NVIDIA_DKMS_PACKAGE="${dkms_pkg}"
                        if [[ "${CUDA_REPO_ENABLED}" == "1" ]]; then
                                NVIDIA_DRIVER_SOURCE="CUDA repo"
                        else
                                NVIDIA_DRIVER_SOURCE="native Ubuntu"
                        fi
                        log "Selected NVIDIA driver source: ${NVIDIA_DRIVER_SOURCE}"
                        log "Selected NVIDIA driver package family: ${NVIDIA_DRIVER_PACKAGE_FAMILY}"
                        log "Selected NVIDIA driver packages: ${NVIDIA_DRIVER_PACKAGE}, ${NVIDIA_DKMS_PACKAGE}"
                        return 0
                fi
        done

        print_nvidia_target_package_policy
        die "Could not find a consistent NVIDIA ${TARGET_NVIDIA_DRIVER_MAJOR} server or non-server package family in apt for $(ubuntu_os_summary)"
}

installed_nvidia_driver_package_family() {
        if dpkg_package_configured_ii "nvidia-driver-${TARGET_NVIDIA_DRIVER_MAJOR}-server"; then
                printf '%s\n' "server"
        elif dpkg_package_configured_ii "nvidia-driver-${TARGET_NVIDIA_DRIVER_MAJOR}"; then
                printf '%s\n' "non-server"
        elif dpkg_package_configured_ii "nvidia-open-${TARGET_NVIDIA_DRIVER_MAJOR}"; then
                printf '%s\n' "open"
        else
                printf '%s\n' "unknown"
        fi
}

collect_non_current_kernel_versions() {
        local current_kernel="$1"
        local pkg path kernel
        local -A seen

        shopt -s nullglob
        for path in /lib/modules/* /boot/vmlinuz-*; do
                [[ -e "${path}" ]] || continue
                kernel="$(basename "${path}")"
                kernel="${kernel#vmlinuz-}"
                safe_kernel_cleanup_candidate "${kernel}" || continue
                [[ "${kernel}" =~ ^[0-9].* ]] || continue
                seen["${kernel}"]=1
        done
        shopt -u nullglob

        while read -r pkg; do
                case "${pkg}" in
                        linux-image-[0-9]*|linux-modules-[0-9]*|linux-modules-extra-[0-9]*|linux-tools-[0-9]*)
                                kernel="${pkg#linux-image-}"
                                kernel="${kernel#linux-modules-}"
                                kernel="${kernel#linux-modules-extra-}"
                                kernel="${kernel#linux-tools-}"
                                safe_kernel_cleanup_candidate "${kernel}" || continue
                                [[ "${kernel}" =~ ^[0-9].* ]] || continue
                                seen["${kernel}"]=1
                                ;;
                        linux-headers-[0-9]*)
                                kernel="${pkg#linux-headers-}"
                                [[ "${kernel}" == "${current_kernel}" || "${kernel}" == "${current_kernel%-*}" ]] && continue
                                safe_kernel_cleanup_candidate "${kernel}" || continue
                                [[ "${kernel}" =~ ^[0-9].* ]] || continue
                                seen["${kernel}"]=1
                                ;;
                esac
        done < <(dpkg-query -W -f='${binary:Package}\n' 2>/dev/null || true)

        if [[ "${#seen[@]}" -gt 0 ]]; then
                printf '%s\n' "${!seen[@]}" | sort -V
        fi
}

prepare_single_kernel_for_nvidia_dkms() {
        local current_kernel current_base other_kernel other_base pkg modules_dir
        local -a other_kernels filtered_other_kernels purge_candidates purge_pkgs
        local -A seen_pkg seen_base

        current_kernel="$(uname -r)"
        current_base="${current_kernel%-generic}"
        [[ -n "${current_base:-}" ]] || die "Could not derive current kernel base from uname -r=${current_kernel}"
        log "Preparing single running kernel for NVIDIA DKMS: ${current_kernel}"

        wait_for_apt
        repair_dpkg_state_if_needed
        DEBIAN_FRONTEND=noninteractive apt-get "${APT_DPKG_OPTIONS[@]}" install -y build-essential dkms pkg-config "linux-headers-${current_kernel}" \
                || log "Kernel/DKMS prerequisite install reported errors; continuing with stale-kernel cleanup and dpkg reconfigure"

        mapfile -t other_kernels < <(collect_non_current_kernel_versions "${current_kernel}")
        filtered_other_kernels=()
        for other_kernel in "${other_kernels[@]}"; do
                if safe_kernel_cleanup_candidate "${other_kernel}"; then
                        filtered_other_kernels+=("${other_kernel}")
                fi
        done
        other_kernels=("${filtered_other_kernels[@]}")
        if [[ "${#other_kernels[@]}" -gt 0 ]]; then
                log "Non-running kernels to remove before NVIDIA DKMS: ${other_kernels[*]}"
        else
                log "No stale non-current kernel detected; skipping kernel cleanup"
                return 0
        fi

        purge_candidates=(linux-virtual linux-image-virtual linux-headers-virtual linux-headers-generic)
        for other_kernel in "${other_kernels[@]}"; do
                if ! safe_kernel_cleanup_candidate "${other_kernel}"; then
                        [[ -n "${other_kernel:-}" ]] || log "No stale non-current kernel detected; skipping kernel cleanup"
                        continue
                fi
                other_base="${other_kernel%-*}"
                [[ -n "${other_base:-}" ]] || continue
                purge_candidates+=(
                        "linux-image-${other_kernel}"
                        "linux-modules-${other_kernel}"
                        "linux-modules-extra-${other_kernel}"
                        "linux-headers-${other_kernel}"
                        "linux-tools-${other_kernel}"
                )

                [[ -n "${other_base:-}" ]] || continue
                if [[ "${other_base}" != "${current_base}" && -z "${seen_base["${other_base}"]:-}" ]]; then
                        purge_candidates+=(
                                "linux-headers-${other_base}"
                                "linux-tools-${other_base}"
                        )
                        [[ -n "${other_base:-}" ]] && seen_base["${other_base}"]=1
                fi
        done

        purge_pkgs=()
        for pkg in "${purge_candidates[@]}"; do
                [[ -n "${pkg:-}" ]] || continue
                if [[ "${pkg}" == *"${current_kernel}"* || "${pkg}" == "linux-headers-${current_base}" ]]; then
                        die "Refusing to purge package '${pkg}' because it matches running kernel ${current_kernel}"
                fi
                [[ -n "${seen_pkg["${pkg}"]:-}" ]] && continue
                seen_pkg["${pkg}"]=1
                if installed_dpkg_package "${pkg}"; then
                        purge_pkgs+=("${pkg}")
                fi
        done

        if [[ "${#purge_pkgs[@]}" -gt 0 ]]; then
                log "Purging non-current/provider kernel packages before NVIDIA DKMS: ${purge_pkgs[*]}"
                for pkg in "${purge_pkgs[@]}"; do
                        if [[ "${pkg}" == *"${current_kernel}"* || "${pkg}" == "linux-headers-${current_base}" ]]; then
                                die "Refusing to purge package '${pkg}' because it matches running kernel ${current_kernel}"
                        fi
                done
                wait_for_apt
                DEBIAN_FRONTEND=noninteractive apt-get "${APT_DPKG_OPTIONS[@]}" purge -y "${purge_pkgs[@]}" || log "Kernel package purge reported errors; continuing with cleanup/reconfigure"
        else
                log "No stale non-current kernel packages to purge before NVIDIA DKMS"
        fi

        for other_kernel in "${other_kernels[@]}"; do
                if ! safe_kernel_cleanup_candidate "${other_kernel}"; then
                        [[ -n "${other_kernel:-}" ]] || log "No stale non-current kernel detected; skipping kernel cleanup"
                        continue
                fi
                modules_dir="/lib/modules/${other_kernel}"
                [[ "${modules_dir}" == "/lib/modules/${other_kernel}" ]] \
                        || die "Refusing unsafe stale kernel modules path: ${modules_dir}"
                [[ "${modules_dir}" != "/lib/modules/${current_kernel}" ]] \
                        || die "Refusing to remove running kernel modules directory /lib/modules/${current_kernel}"
                log "Removing stale non-current kernel leftovers for ${other_kernel}"
                rm -rf -- "${modules_dir}" || true
                rm -f -- "/boot/vmlinuz-${other_kernel}" \
                        "/boot/initrd.img-${other_kernel}" \
                        "/boot/System.map-${other_kernel}" \
                        "/boot/config-${other_kernel}" || true
        done

        rm -f "/var/crash/nvidia-kernel-source-${TARGET_NVIDIA_DRIVER_MAJOR}-server.0.crash" || true

        repair_initramfs_tools_config
        DEBIAN_FRONTEND=noninteractive dpkg --configure -a || true
        DEBIAN_FRONTEND=noninteractive apt-get "${APT_DPKG_OPTIONS[@]}" -f install -y || true
        DEBIAN_FRONTEND=noninteractive dpkg --configure -a || true
        [[ -d "/lib/modules/${current_kernel}" ]] \
                || die "Running kernel modules directory disappeared during stale-kernel cleanup: /lib/modules/${current_kernel}"
        update_initramfs_clouddeploy -u -k "${current_kernel}"
        update_grub_clouddeploy
}

nvidia_install_output_has_dkms_kernel_failure() {
        local output="$1"
        local current_kernel make_log failed_kernel

        if printf '%s\n' "${output}" \
                | grep -Eiq "nvidia-dkms-${TARGET_NVIDIA_DRIVER_MAJOR}|nvidia-driver-${TARGET_NVIDIA_DRIVER_MAJOR}|DKMS.*failed|No rule to make target ['\`]?modules|bad return status"; then
                return 0
        fi

        current_kernel="$(uname -r)"
        while IFS= read -r make_log; do
                if grep -Eiq "No rule to make target ['\`]?modules|bad return status|Error [0-9]+" "${make_log}" 2>/dev/null; then
                        failed_kernel="$(printf '%s\n' "${make_log}" | sed -nE 's#.*kernel-([^/]+)/.*#\1#p; s#.*build/([^/]+)/.*#\1#p' | head -n1)"
                        if [[ -z "${failed_kernel}" || "${failed_kernel}" != "${current_kernel}" ]]; then
                                log "Detected NVIDIA DKMS failure marker in ${make_log}"
                                return 0
                        fi
                fi
        done < <(find /var/lib/dkms -type f -name make.log -path '*nvidia*' 2>/dev/null || true)

        return 1
}

require_nvidia_recovery_package_state() {
        local current_kernel dkms_pkg driver_pkg
        current_kernel="$(uname -r)"
        dkms_pkg="${NVIDIA_DKMS_PACKAGE:-$(nvidia_target_dkms_package)}"
        driver_pkg="${NVIDIA_DRIVER_PACKAGE:-$(nvidia_target_driver_package)}"

        if ! dpkg_package_configured_ii "${dkms_pkg}" \
                || ! dpkg_package_configured_ii "${driver_pkg}"; then
                print_nvidia_driver_diagnostics
                die "NVIDIA recovery did not leave ${dkms_pkg} and ${driver_pkg} configured as ii"
        fi

        if ! nvidia_dkms_installed_for_current_kernel; then
                print_nvidia_driver_diagnostics
                die "NVIDIA recovery did not install NVIDIA DKMS for ${current_kernel}"
        fi
}

require_nvidia_running_kernel_modules_intact() {
        local current_kernel dkms_out

        current_kernel="$(uname -r)"
        if [[ ! -d "/lib/modules/${current_kernel}" ]]; then
                print_nvidia_driver_diagnostics
                die "Running kernel modules directory is missing: /lib/modules/${current_kernel}"
        fi

        if ! nvidia_modules_present_for_running_kernel; then
                print_nvidia_driver_diagnostics
                die "No nvidia*.ko module was found under /lib/modules/${current_kernel}; refusing to continue/reboot with broken DKMS state"
        fi

        dkms_out="$(dkms status 2>/dev/null || true)"
        if printf '%s\n' "${dkms_out}" | grep -Fq "Built modules are missing"; then
                print_nvidia_driver_diagnostics
                die "DKMS reports built NVIDIA modules are missing for the running kernel"
        fi
}

recover_nvidia_dkms_after_kernel_failure() {
        log "NVIDIA DKMS/package configuration failed; pruning stale provider kernels and retrying configuration"
        prepare_single_kernel_for_nvidia_dkms
        DEBIAN_FRONTEND=noninteractive dpkg --configure -a
        DEBIAN_FRONTEND=noninteractive apt-get "${APT_DPKG_OPTIONS[@]}" -f install -y
        DEBIAN_FRONTEND=noninteractive dpkg --configure -a
        require_nvidia_recovery_package_state
        require_nvidia_running_kernel_modules_intact
}

validate_nvidia_driver_acceptance() {
        local driver_pkg="$1"
        local dkms_pkg="$2"

        detect_provider_gpu_init_failure

        if ! dpkg_package_configured_ii "${dkms_pkg}" || ! dpkg_package_configured_ii "${driver_pkg}"; then
                print_nvidia_driver_diagnostics
                die "NVIDIA target packages are not fully configured as ii: ${dkms_pkg}, ${driver_pkg}"
        fi

        if ! nvidia_dkms_installed_for_current_kernel; then
                print_nvidia_driver_diagnostics
                die "DKMS does not show an installed NVIDIA module for running kernel $(uname -r)"
        fi

        require_nvidia_running_kernel_modules_intact

        if nvidia_smi_driver_library_mismatch; then
                print_nvidia_driver_diagnostics
                if [[ "${CLOUDDEPLOY_CONTINUE_REASON:-}" == "nvidia-driver" ]]; then
                        die "nvidia-smi still reports driver/library version mismatch after continuation reboot"
                fi
                schedule_reboot_for_continuation "nvidia-driver" "NVIDIA driver/userland version mismatch detected; rebooting to load the matching kernel module"
        fi

        if ! nvidia_driver_ready; then
                print_nvidia_driver_diagnostics
                detect_provider_gpu_init_failure
                die "NVIDIA driver packages and DKMS are installed, but nvidia-smi is not working"
        fi

        if nvidia_current_boot_rm_failure_seen; then
                print_nvidia_driver_diagnostics
                die "NVIDIA current boot contains Xid 62 / RmInitAdapter failure markers"
        fi

        detect_provider_gpu_init_failure
}

install_target_nvidia_driver() {
        local current_version current_major install_out install_rc dkms_pkg
        local old_driver_packages_installed=0
        current_version="$(current_nvidia_driver_version || true)"
        current_major="$(current_nvidia_driver_major || true)"

        if [[ "${current_major}" == "${TARGET_NVIDIA_DRIVER_MAJOR}" ]] && nvidia_driver_ready; then
                log "NVIDIA driver ${current_version} already matches target major ${TARGET_NVIDIA_DRIVER_MAJOR}"
                detect_provider_gpu_init_failure
                validate_nvidia_driver_acceptance "$(nvidia_target_driver_package)" "$(nvidia_target_dkms_package)"
                return 0
        fi

        if nvidia_driver_ready && [[ "${FORCE_DRIVER_UPGRADE}" != "1" ]]; then
                log "NVIDIA driver ${current_version:-unknown} is working; FORCE_DRIVER_UPGRADE=0 so not forcing target ${TARGET_NVIDIA_DRIVER_MAJOR}"
                detect_provider_gpu_init_failure
                return 0
        fi

        if [[ "${FORCE_DRIVER_UPGRADE}" != "1" ]] && command -v nvidia-smi >/dev/null 2>&1; then
                log "FORCE_DRIVER_UPGRADE=0 and nvidia-smi exists but is not healthy; refusing NVIDIA package/kernel cleanup or reinstall"
                diagnose_nvidia_init_failure
                detect_provider_gpu_init_failure
                die "NVIDIA driver is not healthy, but FORCE_DRIVER_UPGRADE=0 prevents automated repair. Set FORCE_DRIVER_UPGRADE=1 to allow driver/kernel cleanup, or repair the provider/GPU state manually."
        fi

        log "Installing NVIDIA driver target major ${TARGET_NVIDIA_DRIVER_MAJOR}"
        systemctl stop sunshine-headless.service plasma-realvt.service kwin-realvt.service plasma-shell-realvt.service weston-kms-session.service 2>/dev/null || true

        repair_dpkg_state_if_needed
        prepare_single_kernel_for_nvidia_dkms
        if [[ "$(ubuntu_version_id)" == "24.04" ]]; then
                ensure_cuda_ubuntu_repo
        else
                NVIDIA_DRIVER_SOURCE="native Ubuntu"
                log "Ubuntu $(ubuntu_version_id) detected; preferring native Ubuntu NVIDIA driver packages and deferring any CUDA repo setup until toolkit install"
        fi

        if dpkg-query -W -f='${binary:Package}\n' 2>/dev/null \
                | grep -Eq '^(cuda-drivers|cuda-drivers-|nvidia-|libnvidia-|linux-modules-nvidia-|linux-objects-nvidia-|linux-signatures-nvidia-)'; then
                old_driver_packages_installed=1
        fi

        if [[ -z "${current_major}" || "${current_major}" != "${TARGET_NVIDIA_DRIVER_MAJOR}" ]] \
                || { ! nvidia_driver_ready && [[ "${old_driver_packages_installed}" == "1" ]]; }; then
                cleanup_conflicting_nvidia_driver_packages
        fi

        select_nvidia_driver_package_family

        local driver_pkg="${NVIDIA_DRIVER_PACKAGE}"
        local pkg_suffix="${TARGET_NVIDIA_DRIVER_MAJOR}"
        local candidate

        local -a install_pkgs
        if [[ "${NVIDIA_DRIVER_PACKAGE_FAMILY}" == "server" ]]; then
                pkg_suffix="${TARGET_NVIDIA_DRIVER_MAJOR}-server"
        else
                pkg_suffix="${TARGET_NVIDIA_DRIVER_MAJOR}"
        fi

        install_pkgs=("${driver_pkg}" "${NVIDIA_DKMS_PACKAGE}")
        for candidate in \
                "nvidia-utils-${pkg_suffix}" \
                "libnvidia-encode-${pkg_suffix}" \
                "libnvidia-fbc1-${pkg_suffix}"
        do
                if apt_package_available "${candidate}"; then
                        install_pkgs+=("${candidate}")
                fi
        done

        log "Installing NVIDIA packages: ${install_pkgs[*]}"
        wait_for_apt
        repair_dpkg_state_if_needed
        install_out="$(DEBIAN_FRONTEND=noninteractive apt-get "${APT_DPKG_OPTIONS[@]}" install -y "${install_pkgs[@]}" 2>&1)" && install_rc=0 || install_rc=$?
        printf '%s\n' "${install_out}"
        if [[ "${install_rc}" -ne 0 ]]; then
                if nvidia_install_output_has_dkms_kernel_failure "${install_out}"; then
                        recover_nvidia_dkms_after_kernel_failure
                else
                        die "Failed to install NVIDIA packages: ${install_pkgs[*]}"
                fi
        fi

        dkms_pkg="${NVIDIA_DKMS_PACKAGE:-$(nvidia_target_dkms_package)}"
        require_nvidia_running_kernel_modules_intact

        modprobe nvidia 2>/dev/null || true
        modprobe nvidia_modeset 2>/dev/null || true
        modprobe nvidia_drm 2>/dev/null || true

        for _ in $(seq 1 15); do
                current_version="$(current_nvidia_driver_version || true)"
                current_major="$(current_nvidia_driver_major || true)"
                if [[ "${current_major}" == "${TARGET_NVIDIA_DRIVER_MAJOR}" ]] && nvidia_driver_ready; then
                        log "NVIDIA driver ${current_version} is loaded and matches target major ${TARGET_NVIDIA_DRIVER_MAJOR}"
                        return 0
                fi
                echo "Waiting for NVIDIA driver ${TARGET_NVIDIA_DRIVER_MAJOR} to become active..."
                sleep 3
        done

        if ! nvidia_driver_ready; then
                diagnose_nvidia_init_failure
                detect_provider_gpu_init_failure
                if nvidia_smi_driver_library_mismatch; then
                        if [[ "${CLOUDDEPLOY_CONTINUE_REASON:-}" == "nvidia-driver" ]]; then
                                die "nvidia-smi still reports driver/library version mismatch after continuation reboot"
                        fi
                        if dpkg_package_configured_ii "${dkms_pkg}" \
                                && dpkg_package_configured_ii "${driver_pkg}" \
                                && nvidia_dkms_installed_for_current_kernel; then
                                schedule_reboot_for_continuation "nvidia-driver" "NVIDIA driver/userland version mismatch detected; rebooting to load the matching kernel module"
                        fi
                fi
        fi

        current_version="$(current_nvidia_driver_version || true)"
        current_major="$(current_nvidia_driver_major || true)"
        if [[ "${current_major}" != "${TARGET_NVIDIA_DRIVER_MAJOR}" ]]; then
                if nvidia_provider_init_failure_seen; then
                        diagnose_nvidia_init_failure
                        fail_if_nvidia_provider_init_failure_seen
                fi
                if [[ "${CLOUDDEPLOY_CONTINUE_REASON:-}" == "nvidia-driver" ]]; then
                        die "NVIDIA driver major is still ${current_major:-missing} after continuation reboot; expected ${TARGET_NVIDIA_DRIVER_MAJOR}"
                fi
                schedule_reboot_for_continuation "nvidia-driver" "NVIDIA driver target ${TARGET_NVIDIA_DRIVER_MAJOR} installed but loaded driver is ${current_version:-missing}; rebooting and continuing automatically"
        fi

        validate_nvidia_driver_acceptance "${driver_pkg}" "${dkms_pkg}"
        log "NVIDIA driver acceptance gate passed for ${TARGET_NVIDIA_DRIVER_MAJOR} on kernel $(uname -r)"
}

cuda_toolkit_ready() {
        local nvcc_bin=""

        if [[ -x /usr/local/cuda/bin/nvcc ]]; then
                nvcc_bin="/usr/local/cuda/bin/nvcc"
        elif command -v nvcc >/dev/null 2>&1; then
                nvcc_bin="$(command -v nvcc)"
        fi

        [[ -n "${nvcc_bin}" ]] || return 1
        [[ -d /usr/local/cuda/include ]] || return 1
        [[ -d /usr/local/cuda/lib64 ]] || return 1
}

expected_cuda_major() {
        # Derive the expected nvcc release major (e.g. "13") from either
        # CUDA_TOOLKIT_PACKAGE (cuda-toolkit-13-0 -> 13) or CUDA_RUNFILE_VERSION
        # (13.0.2 -> 13). Returns empty when neither pins a specific version.
        local pkg="${CUDA_TOOLKIT_PACKAGE:-}"
        if [[ "${pkg}" =~ ^cuda-toolkit-([0-9]+)(-[0-9]+)?$ ]]; then
                printf '%s\n' "${BASH_REMATCH[1]}"
                return 0
        fi
        if [[ -n "${CUDA_RUNFILE_VERSION:-}" ]]; then
                printf '%s\n' "${CUDA_RUNFILE_VERSION%%.*}"
                return 0
        fi
        printf '\n'
}

verify_cuda_toolkit_version_matches() {
        local expected_major
        expected_major="$(expected_cuda_major)"
        [[ -n "${expected_major}" ]] || return 0
        [[ -x /usr/local/cuda/bin/nvcc ]] || return 0

        local actual_major
        actual_major="$(/usr/local/cuda/bin/nvcc --version 2>/dev/null \
                | sed -nE 's/.*release ([0-9]+)\.[0-9]+.*/\1/p' | head -n1)"
        if [[ -z "${actual_major}" ]]; then
                if [[ "${REQUIRE_CUDA_TOOLKIT}" == "1" ]]; then
                        die "CUDA toolkit version check: could not parse nvcc release from /usr/local/cuda/bin/nvcc --version"
                fi
                log "WARNING: CUDA toolkit version check: could not parse nvcc release; continuing because REQUIRE_CUDA_TOOLKIT=${REQUIRE_CUDA_TOOLKIT}"
                return 0
        fi

        if [[ "${actual_major}" != "${expected_major}" ]]; then
                if [[ "${REQUIRE_CUDA_TOOLKIT}" == "1" ]]; then
                        die "CUDA toolkit version mismatch: expected major ${expected_major}.x (CUDA_TOOLKIT_PACKAGE=${CUDA_TOOLKIT_PACKAGE}, CUDA_RUNFILE_VERSION=${CUDA_RUNFILE_VERSION}), but nvcc reports release ${actual_major}.x. Refusing to accept Ubuntu's nvidia-cuda-toolkit (12.4) when CUDA ${expected_major} is required."
                fi
                log "WARNING: CUDA toolkit version mismatch: expected ${expected_major}.x, got ${actual_major}.x; continuing because REQUIRE_CUDA_TOOLKIT=${REQUIRE_CUDA_TOOLKIT}"
        else
                log "CUDA toolkit version check OK: nvcc release ${actual_major}.x matches expected ${expected_major}.x"
        fi
}

verify_cuda_toolkit_or_fail() {
        if cuda_toolkit_ready; then
                export PATH="/usr/local/cuda/bin:${PATH}"
                export LD_LIBRARY_PATH="/usr/local/cuda/lib64:${LD_LIBRARY_PATH:-}"
                if [[ -x /usr/local/cuda/bin/nvcc ]]; then
                        /usr/local/cuda/bin/nvcc --version || true
                else
                        nvcc --version || true
                fi
                verify_cuda_toolkit_version_matches
                return 0
        fi

        if [[ "${REQUIRE_CUDA_TOOLKIT}" == "1" ]]; then
                die "CUDA toolkit was requested but nvcc, /usr/local/cuda/include, or /usr/local/cuda/lib64 is missing"
        fi

        log "CUDA toolkit verification failed, but REQUIRE_CUDA_TOOLKIT=${REQUIRE_CUDA_TOOLKIT}; continuing"
        return 0
}

install_cuda_toolkit_from_runfile() {
        local runfile runfile_size

        CUDA_TOOLKIT_SOURCE="runfile"
        log "Installing CUDA toolkit using toolkit-only NVIDIA runfile"
        log "CUDA runfile URL: ${CUDA_RUNFILE_URL}"

        runfile="$(mktemp /tmp/cuda-toolkit.XXXXXX.run)"
        if ! wget -O "${runfile}" "${CUDA_RUNFILE_URL}"; then
                rm -f "${runfile}"
                if [[ "${REQUIRE_CUDA_TOOLKIT}" == "1" ]]; then
                        die "CUDA toolkit runfile download failed from ${CUDA_RUNFILE_URL}"
                fi
                log "CUDA toolkit runfile download failed; continuing because REQUIRE_CUDA_TOOLKIT=${REQUIRE_CUDA_TOOLKIT}"
                return 0
        fi

        # The real CUDA 13 toolkit-only runfile is ~4 GiB. Anything dramatically
        # smaller is almost certainly an HTML error page (404, redirect, captive
        # portal) saved as the target file. Refuse to execute it.
        runfile_size="$(stat -c '%s' "${runfile}" 2>/dev/null || echo 0)"
        if (( runfile_size < 1073741824 )); then
                log "CUDA runfile from ${CUDA_RUNFILE_URL} is suspiciously small (${runfile_size} bytes); refusing to execute it."
                rm -f "${runfile}"
                if [[ "${REQUIRE_CUDA_TOOLKIT}" == "1" ]]; then
                        die "CUDA runfile integrity check failed: file is only ${runfile_size} bytes (expected >1 GiB)"
                fi
                log "Skipping runfile install because REQUIRE_CUDA_TOOLKIT=${REQUIRE_CUDA_TOOLKIT}"
                return 0
        fi

        chmod 0755 "${runfile}"

        # NVIDIA runfiles support --check to validate the embedded MD5 sum.
        if ! sh "${runfile}" --check >/tmp/cuda-runfile-check.log 2>&1; then
                log "CUDA runfile --check failed; tail of /tmp/cuda-runfile-check.log:"
                tail -n 20 /tmp/cuda-runfile-check.log 2>/dev/null || true
                rm -f "${runfile}"
                if [[ "${REQUIRE_CUDA_TOOLKIT}" == "1" ]]; then
                        die "CUDA runfile integrity check (--check) failed for ${CUDA_RUNFILE_URL}"
                fi
                log "Skipping runfile install because REQUIRE_CUDA_TOOLKIT=${REQUIRE_CUDA_TOOLKIT}"
                return 0
        fi

        if ! sh "${runfile}" --silent --toolkit --override; then
                rm -f "${runfile}"
                if [[ "${REQUIRE_CUDA_TOOLKIT}" == "1" ]]; then
                        die "CUDA toolkit-only runfile install failed from ${CUDA_RUNFILE_URL}"
                fi
                log "CUDA toolkit-only runfile install failed; continuing because REQUIRE_CUDA_TOOLKIT=${REQUIRE_CUDA_TOOLKIT}"
                return 0
        fi
        rm -f "${runfile}"

        verify_cuda_toolkit_or_fail
}

install_cuda_toolkit_if_requested() {
        local method="${CUDA_INSTALL_METHOD}"

        if [[ "${INSTALL_CUDA_TOOLKIT}" == "0" || "${method}" == "none" ]]; then
                log "CUDA toolkit install disabled: INSTALL_CUDA_TOOLKIT=${INSTALL_CUDA_TOOLKIT}, CUDA_INSTALL_METHOD=${method}"
                return 0
        fi

        if cuda_toolkit_ready; then
                CUDA_TOOLKIT_SOURCE="already installed"
                log "CUDA toolkit already present under /usr/local/cuda"
                verify_cuda_toolkit_or_fail
                return 0
        fi

        case "${method}" in
                auto)
                        if [[ -n "$(detect_cuda_repo_distro || true)" ]]; then
                                method="apt"
                        else
                                method="runfile"
                        fi
                        ;;
                apt|runfile)
                        ;;
                *)
                        die "Unsupported CUDA_INSTALL_METHOD='${CUDA_INSTALL_METHOD}'. Supported: auto, apt, runfile, none."
                        ;;
        esac

        case "${method}" in
                apt)
                        ensure_cuda_ubuntu_repo
                        if [[ "${CUDA_REPO_ENABLED}" != "1" ]]; then
                                if [[ "${REQUIRE_CUDA_TOOLKIT}" == "1" ]]; then
                                        die "CUDA_INSTALL_METHOD=apt requested, but no official CUDA apt repo is available for $(ubuntu_os_summary)"
                                fi
                                log "CUDA apt repo unavailable; continuing because REQUIRE_CUDA_TOOLKIT=${REQUIRE_CUDA_TOOLKIT}"
                                return 0
                        fi
                        CUDA_TOOLKIT_SOURCE="apt repo ${CUDA_REPO_DISTRO}"
                        log "Installing CUDA toolkit package from ${CUDA_REPO_DISTRO}: ${CUDA_TOOLKIT_PACKAGE}"
                        apt_install_wait "${CUDA_TOOLKIT_PACKAGE}"
                        verify_cuda_toolkit_or_fail
                        ;;
                runfile)
                        install_cuda_toolkit_from_runfile
                        ;;
        esac
}

install_sunshine_deb() {
        if command -v sunshine >/dev/null 2>&1; then
                log "Packaged Sunshine already installed at $(command -v sunshine)"
                return 0
        fi

        log "Installing packaged Sunshine from ${SUNSHINE_DEB_URL}"
        local tmpdeb
        tmpdeb="$(mktemp /tmp/sunshine.XXXXXX.deb)"
        wget -O "${tmpdeb}" "${SUNSHINE_DEB_URL}"
        wait_for_apt
        dpkg -i "${tmpdeb}" || DEBIAN_FRONTEND=noninteractive apt-get "${APT_DPKG_OPTIONS[@]}" -f install -y
        rm -f "${tmpdeb}"
}

sunshine_runtime_bin() {
        if [[ "${SUNSHINE_SOURCE_MODE}" == "fork" ]]; then
                printf '%s\n' "${SUNSHINE_INSTALL_BIN}"
        elif command -v sunshine >/dev/null 2>&1; then
                command -v sunshine
        else
                printf '%s\n' "/usr/bin/sunshine"
        fi
}

resolve_sunshine_fork_branch() {
        local requested_branch="${SUNSHINE_FORK_BRANCH}"

        if [[ "${requested_branch}" == "${SUNSHINE_DIAGNOSTIC_FORK_BRANCH}" ]] \
                && git ls-remote --exit-code --heads "${SUNSHINE_FORK_REPO}" "${SUNSHINE_CLEAN_FORK_BRANCH}" >/dev/null 2>&1; then
                printf '%s\n' "${SUNSHINE_CLEAN_FORK_BRANCH}"
                return 0
        fi

        printf '%s\n' "${requested_branch}"
}

git_safe_directory_add() {
        local dir="$1"
        local resolved_dir

        command -v git >/dev/null 2>&1 || return 0
        [[ -n "${dir:-}" ]] || return 0

        resolved_dir="$(readlink -f "${dir}" 2>/dev/null || printf '%s\n' "${dir}")"
        [[ -n "${resolved_dir:-}" ]] || return 0

        if ! git config --global --get-all safe.directory 2>/dev/null | grep -Fxq "${resolved_dir}"; then
                git config --global --add safe.directory "${resolved_dir}" || true
        fi
}

configure_git_safe_directories() {
        local script_dir

        command -v git >/dev/null 2>&1 || return 0
        script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
        git_safe_directory_add "${script_dir}"
        git_safe_directory_add "${SUNSHINE_BUILD_DIR}"
        git_safe_directory_add "/home/user/CloudDeploy-mover"
}

install_clouddeploy_systemd_units() {
        local runtime_bin

        [[ -n "${HOME_DIR:-}" ]] || die "HOME_DIR is not set before installing systemd units"
        [[ -n "${HEADLESS_UID:-}" ]] || HEADLESS_UID="$(id -u "${HEADLESS_USER}")"
        [[ -n "${RUNTIME_DIR:-}" ]] || RUNTIME_DIR="/run/user/${HEADLESS_UID}"
        [[ -n "${KWIN_DISPLAY:-}" ]] || KWIN_DISPLAY="${KWIN_WAYLAND_DISPLAY}"
        [[ -n "${COMPOSITOR_SERVICE:-}" ]] || COMPOSITOR_SERVICE="$(service_for_mode)"
        runtime_bin="${SUNSHINE_RUNTIME_BIN:-$(sunshine_runtime_bin 2>/dev/null || true)}"
        runtime_bin="${runtime_bin:-/usr/bin/sunshine}"

        log "Installing CloudDeploy systemd units idempotently"
        install_continuation_service

        cat > /etc/systemd/system/kwin-realvt.service <<EOF
[Unit]
Description=KWin Wayland DRM session on real VT${KWIN_VTNR}
After=systemd-logind.service systemd-user-sessions.service network-online.target
Wants=network-online.target
Conflicts=display-manager.service getty@tty${KWIN_VTNR}.service plasma-realvt.service weston-kms-session.service

[Service]
Type=simple
User=${HEADLESS_USER}
Group=${HEADLESS_USER}
SupplementaryGroups=video render input
PAMName=login
WorkingDirectory=${HOME_DIR}
TTYPath=/dev/tty${KWIN_VTNR}
StandardInput=tty
StandardOutput=journal
StandardError=journal
TTYReset=yes
TTYVHangup=no
TTYVTDisallocate=no
UtmpIdentifier=tty${KWIN_VTNR}
UtmpMode=user
TimeoutStartSec=45
Environment=KWIN_DRM_DEVICES=${SUNSHINE_DRM_DEVICE}
Environment=KWIN_DRM_NO_DIRECT_SCANOUT=1
Environment=KWIN_FORCE_SW_CURSOR=1
Environment=KWIN_USE_OVERLAYS=0
Environment=GBM_BACKEND=nvidia-drm
Environment=__GLX_VENDOR_LIBRARY_NAME=nvidia
PermissionsStartOnly=true
ExecStartPre=-/usr/bin/systemctl stop getty@tty${KWIN_VTNR}.service
ExecStartPre=-/usr/bin/systemctl start user@${HEADLESS_UID}.service
ExecStartPre=/usr/bin/mkdir -p ${RUNTIME_DIR}
ExecStartPre=/usr/bin/chown ${HEADLESS_USER}:${HEADLESS_USER} ${RUNTIME_DIR}
ExecStartPre=/usr/bin/chmod 700 ${RUNTIME_DIR}
ExecStartPre=/usr/bin/chvt ${KWIN_VTNR}
ExecStartPre=/usr/bin/bash -lc 'for i in \$(seq 1 30); do nvidia-smi >/dev/null 2>&1 && exit 0; sleep 2; done; exit 1'
ExecStart=${HOME_DIR}/.local/bin/start-kwin-realvt.sh
ExecStartPost=
Restart=no

[Install]
WantedBy=multi-user.target
EOF

        cat > /etc/systemd/system/plasma-shell-realvt.service <<EOF
[Unit]
Description=Plasma shell on direct KWin Wayland VT session
After=kwin-realvt.service
Requires=kwin-realvt.service
PartOf=kwin-realvt.service

[Service]
User=${HEADLESS_USER}
Group=${HEADLESS_USER}
SupplementaryGroups=video render input
WorkingDirectory=${HOME_DIR}
Environment=HOME=${HOME_DIR}
Environment=USER=${HEADLESS_USER}
Environment=LOGNAME=${HEADLESS_USER}
Environment=XDG_RUNTIME_DIR=${RUNTIME_DIR}
Environment=WAYLAND_DISPLAY=${KWIN_DISPLAY}
Environment=DBUS_SESSION_BUS_ADDRESS=unix:path=${RUNTIME_DIR}/bus
ExecStart=${HOME_DIR}/.local/bin/start-plasmashell-realvt.sh
Restart=on-failure
RestartSec=5
StandardOutput=journal
StandardError=journal

[Install]
WantedBy=multi-user.target
EOF

        cat > /etc/systemd/system/sunshine-headless.service <<EOF
[Unit]
Description=Sunshine on CloudDeploy NVIDIA Wayland KMS
After=${COMPOSITOR_SERVICE} network-online.target tailscaled.service
Wants=network-online.target tailscaled.service ${COMPOSITOR_SERVICE}

[Service]
User=${HEADLESS_USER}
Group=${HEADLESS_USER}
SupplementaryGroups=video render input
WorkingDirectory=${HOME_DIR}
Environment=HOME=${HOME_DIR}
Environment=USER=${HEADLESS_USER}
Environment=LOGNAME=${HEADLESS_USER}
Environment=XDG_RUNTIME_DIR=${RUNTIME_DIR}
Environment=WAYLAND_DISPLAY=${KWIN_DISPLAY}
Environment=DBUS_SESSION_BUS_ADDRESS=unix:path=${RUNTIME_DIR}/bus
Environment=SUNSHINE_STREAM_DIAG_REUSE_AUDIO_PEER=1
Environment=SUNSHINE_STREAM_DIAG_VIDEO_PEER_MODE=rtsp-client-port
Environment=SUNSHINE_STREAM_DIAG_IGNORE_CONTROL_TIMEOUT=1
Environment=SUNSHINE_STREAM_DIAG_FORCE_ANNOUNCE_SUCCESS=1
Environment=SUNSHINE_STREAM_DIAG_FORCE_ANNOUNCE_SUCCESS_IMMEDIATE=1
ExecStartPre=/usr/local/bin/clouddeploy-wait-sunshine-session.sh
ExecStart=${runtime_bin} ${HOME_DIR}/.config/sunshine/sunshine.conf
Restart=on-failure
RestartSec=5
StandardOutput=journal
StandardError=journal

[Install]
WantedBy=multi-user.target
EOF

        systemctl daemon-reload || true
        mark_phase_done "systemd-units-installed.done"
}

resolve_sunshine_cuda_module() {
        # Returns "on" or "off" based on SUNSHINE_ENABLE_CUDA_MODULE and Ubuntu release.
        # auto: enable on Ubuntu 24.04 and earlier; disable on 25.10 / 26.04 because
        # CUDA 13 headers conflict with newer glibc (rsqrt/rsqrtf), which breaks
        # Sunshine's optional CUDA/NvFBC module during nvcc compiler detection.
        local mode="${SUNSHINE_ENABLE_CUDA_MODULE:-auto}"
        case "${mode}" in
                on|off)
                        printf '%s\n' "${mode}"
                        return 0
                        ;;
                auto)
                        ;;
                *)
                        log "WARNING: Unknown SUNSHINE_ENABLE_CUDA_MODULE='${mode}'; treating as auto"
                        ;;
        esac

        local ver
        ver="$(ubuntu_version_id)"
        case "${ver}" in
                25.10|26.04|26.10)
                        printf 'off\n'
                        ;;
                *)
                        printf 'on\n'
                        ;;
        esac
}

install_sunshine_from_fork_if_requested() {
        case "${SUNSHINE_SOURCE_MODE}" in
                deb)
                        install_sunshine_deb
                        ;;
                fork)
                        install_sunshine_deb
                        apt_install_wait \
                                git cmake ninja-build build-essential pkg-config python3 nodejs npm \
                                libvulkan-dev vulkan-tools vulkan-validationlayers glslang-tools glslc \
                                libpipewire-0.3-dev doxygen graphviz \
                                libssl-dev libcurl4-openssl-dev libcap-dev libdrm-dev libevdev-dev libgbm-dev \
                                libminiupnpc-dev libnotify-dev libnuma-dev libopus-dev libpulse-dev libva-dev libvdpau-dev \
                                libwayland-dev libx11-dev libxcb1-dev libxcb-shm0-dev libxcb-xfixes0-dev libxfixes-dev \
                                libxrandr-dev libxtst-dev libsystemd-dev libudev-dev libayatana-appindicator3-dev \
                                libavcodec-dev libavdevice-dev libavfilter-dev libavformat-dev libavutil-dev libswscale-dev libswresample-dev \
                                libboost-filesystem-dev libboost-log-dev libboost-program-options-dev libboost-system-dev libboost-thread-dev

                        configure_git_safe_directories

                        local resolved_branch
                        resolved_branch="$(resolve_sunshine_fork_branch)"
                        SUNSHINE_FORK_BRANCH="${resolved_branch}"

                        log "Installing Sunshine from custom fork ${SUNSHINE_FORK_REPO} branch ${SUNSHINE_FORK_BRANCH}"
                        if [[ "${SUNSHINE_FORK_BRANCH}" == "${SUNSHINE_DIAGNOSTIC_FORK_BRANCH}" ]]; then
                                log "Using heavy diagnostic Sunshine branch; switch to ${SUNSHINE_CLEAN_FORK_BRANCH} when it exists upstream."
                        else
                                log "Using clean CloudDeploy Sunshine branch ${SUNSHINE_FORK_BRANCH}."
                        fi
                        log "SUNSHINE_SOURCE_MODE=fork is custom/diagnostic until these fixes are upstreamed."

                        install -d -m 0755 "$(dirname "${SUNSHINE_BUILD_DIR}")"
                        if [[ -d "${SUNSHINE_BUILD_DIR}/.git" ]]; then
                                git -C "${SUNSHINE_BUILD_DIR}" fetch origin "${SUNSHINE_FORK_BRANCH}"
                                git -C "${SUNSHINE_BUILD_DIR}" checkout -B "${SUNSHINE_FORK_BRANCH}" "origin/${SUNSHINE_FORK_BRANCH}"
                        elif [[ -e "${SUNSHINE_BUILD_DIR}" ]]; then
                                die "${SUNSHINE_BUILD_DIR} exists but is not a git checkout"
                        else
                                git clone --recursive --branch "${SUNSHINE_FORK_BRANCH}" "${SUNSHINE_FORK_REPO}" "${SUNSHINE_BUILD_DIR}"
                        fi

                        configure_git_safe_directories
                        [[ -d "${SUNSHINE_BUILD_DIR}/.git" ]] || die "${SUNSHINE_BUILD_DIR} is missing .git after checkout"
                        git -C "${SUNSHINE_BUILD_DIR}" rev-parse --show-toplevel >/dev/null
                        git -C "${SUNSHINE_BUILD_DIR}" submodule update --init --recursive
                        log "Applying Ubuntu 24.04 Doxygen compatibility patch for Sunshine fork build"
                        local doxyconfig_file
                        for doxyconfig_file in \
                                "${SUNSHINE_BUILD_DIR}/third-party/doxyconfig/CMakeLists.txt" \
                                "${SUNSHINE_BUILD_DIR}/third-party/libdisplaydevice/third-party/doxyconfig/CMakeLists.txt" \
                                "${SUNSHINE_BUILD_DIR}/third-party/tray/third-party/doxyconfig/CMakeLists.txt"
                        do
                                if [[ -f "${doxyconfig_file}" ]]; then
                                        sed -i 's/find_package(Doxygen 1[.]10 REQUIRED dot)/find_package(Doxygen 1.9 REQUIRED dot)/g' "${doxyconfig_file}"
                                fi
                        done

                        local sunshine_cuda_module
                        sunshine_cuda_module="$(resolve_sunshine_cuda_module)"
                        log "Sunshine fork build: SUNSHINE_ENABLE_CUDA_MODULE=${SUNSHINE_ENABLE_CUDA_MODULE} (resolved=${sunshine_cuda_module}) on $(ubuntu_os_summary)"

                        local -a cmake_cuda_args
                        cmake_cuda_args=()
                        if [[ -d /usr/local/cuda ]]; then
                                export CUDA_HOME=/usr/local/cuda
                                export CUDA_PATH=/usr/local/cuda
                                export CUDAToolkit_ROOT=/usr/local/cuda
                                export PATH="/usr/local/cuda/bin:${PATH}"
                                export LD_LIBRARY_PATH="/usr/local/cuda/lib64:${LD_LIBRARY_PATH:-}"
                                cmake_cuda_args+=("-DCUDAToolkit_ROOT=/usr/local/cuda" "-DCUDA_TOOLKIT_ROOT_DIR=/usr/local/cuda")
                                if [[ -x /usr/local/cuda/bin/nvcc ]]; then
                                        local nvcc_release
                                        nvcc_release="$(/usr/local/cuda/bin/nvcc --version 2>/dev/null | sed -nE 's/.*release ([0-9.]+).*/\1/p' | head -n1)"
                                        log "Sunshine fork build: nvcc release ${nvcc_release:-unknown} from /usr/local/cuda/bin/nvcc"
                                fi
                        else
                                log "Sunshine fork build: /usr/local/cuda is missing; CUDA env exports skipped"
                        fi

                        local -a cmake_sunshine_cuda_args
                        case "${sunshine_cuda_module}" in
                                off)
                                        cmake_sunshine_cuda_args=("-DSUNSHINE_ENABLE_CUDA=OFF" "-DCUDA_FAIL_ON_MISSING=OFF")
                                        log "Sunshine fork build: CUDA/NvFBC module disabled. DRM/KMS/Wayland + NVENC streaming path remains enabled."
                                        ;;
                                on)
                                        cmake_sunshine_cuda_args=("-DSUNSHINE_ENABLE_CUDA=ON" "-DCUDA_FAIL_ON_MISSING=ON")
                                        log "Sunshine fork build: CUDA/NvFBC module forced on; configure will fail if nvcc detection or compile fails."
                                        ;;
                                *)
                                        cmake_sunshine_cuda_args=()
                                        ;;
                        esac

                        local sunshine_build_subdir="${SUNSHINE_BUILD_DIR}/build"
                        local cuda_mode_marker="${sunshine_build_subdir}/.clouddeploy-cuda-mode"
                        local cuda_configure_pending="${sunshine_build_subdir}/.clouddeploy-configure-pending"
                        local previous_cuda_mode=""
                        if [[ -f "${cuda_mode_marker}" ]]; then
                                previous_cuda_mode="$(<"${cuda_mode_marker}")"
                        fi
                        if [[ -d "${sunshine_build_subdir}" ]] \
                                && { [[ -f "${cuda_configure_pending}" ]] \
                                        || { [[ -n "${previous_cuda_mode}" ]] && [[ "${previous_cuda_mode}" != "${sunshine_cuda_module}" ]]; }; }; then
                                if [[ -f "${cuda_configure_pending}" ]]; then
                                        log "Sunshine fork build: previous configure did not complete; wiping ${sunshine_build_subdir}"
                                else
                                        log "Sunshine fork build: CUDA module changed (${previous_cuda_mode} -> ${sunshine_cuda_module}); wiping ${sunshine_build_subdir}"
                                fi
                                rm -rf "${sunshine_build_subdir}"
                        fi

                        configure_git_safe_directories
                        install -d -m 0755 "${sunshine_build_subdir}"
                        : > "${cuda_configure_pending}"
                        (
                                cd "${SUNSHINE_BUILD_DIR}"
                                cmake -S . -B build -G Ninja \
                                        -DCMAKE_BUILD_TYPE=Release \
                                        -DBUILD_TESTS=OFF \
                                        -DSUNSHINE_BUILD_TESTS=OFF \
                                        "${cmake_sunshine_cuda_args[@]}" \
                                        "${cmake_cuda_args[@]}"
                        )
                        rm -f "${cuda_configure_pending}"
                        printf '%s\n' "${sunshine_cuda_module}" > "${cuda_mode_marker}"
                        log "Sunshine fork build: configure complete (CUDA module=${sunshine_cuda_module}); DRM/KMS/Wayland/NVENC streaming path enabled."
                        (
                                cd "${SUNSHINE_BUILD_DIR}"
                                cmake --build build --target sunshine -j "${SUNSHINE_BUILD_JOBS}"
                        )

                        local built_bin
                        built_bin="$(find "${SUNSHINE_BUILD_DIR}/build" -type f -name sunshine -perm -111 2>/dev/null | head -n1)"
                        [[ -x "${built_bin}" ]] || die "Could not find built Sunshine binary in ${SUNSHINE_BUILD_DIR}/build"

                        install -m 0755 "${built_bin}" "${SUNSHINE_INSTALL_BIN}"
                        if command -v setcap >/dev/null 2>&1; then
                                setcap cap_sys_admin,cap_sys_nice+ep "${SUNSHINE_INSTALL_BIN}" || true
                        fi

                        [[ -d "${SUNSHINE_BUILD_DIR}/build/assets" ]] || die "Sunshine fork build assets missing: ${SUNSHINE_BUILD_DIR}/build/assets"
                        install -d -m 0755 /usr/local/assets
                        cp -a "${SUNSHINE_BUILD_DIR}/build/assets/." /usr/local/assets/
                        install -d -m 0755 /usr/local/assets/web
                        if [[ -d /usr/share/sunshine/web ]]; then
                                cp -a /usr/share/sunshine/web/. /usr/local/assets/web/
                        else
                                log "WARNING: Packaged Sunshine web UI source missing: /usr/share/sunshine/web"
                        fi
                        chown -R root:root /usr/local/assets
                        [[ -f /usr/local/assets/apps.json ]] || die "Sunshine runtime asset missing after install: /usr/local/assets/apps.json"
                        [[ -f /usr/local/assets/web/index.html ]] || die "Sunshine web UI asset missing after install: /usr/local/assets/web/index.html"
                        find /usr/local/assets/web -type f \( -name '*.js' -o -name '*.css' \) 2>/dev/null | grep -q . \
                                || die "Sunshine web UI assets missing built JS/CSS under /usr/local/assets/web"
                        ;;
                *)
                        die "Unsupported SUNSHINE_SOURCE_MODE='${SUNSHINE_SOURCE_MODE}'. Supported now: deb, fork."
                        ;;
        esac
}

find_nvidia_vulkan_icd() {
        find /usr/share/vulkan/icd.d -name '*nvidia*_icd.json' 2>/dev/null | head -n1 || true
}

find_nvidia_egl_vendor_json() {
        grep -Rls 'libEGL_nvidia[.]so' /usr/share/glvnd/egl_vendor.d/*.json 2>/dev/null | head -n1 || true
}

library_available_to_loader() {
        local lib="$1"
        ldconfig -p 2>/dev/null | grep -Fq "${lib}" \
                || find /usr -type f -name "${lib}" 2>/dev/null | grep -q .
}

write_egl_external_platform_json() {
        local path="$1"
        local library="$2"

        cat > "${path}" <<EOF
{
    "file_format_version": "1.0.0",
    "ICD": {
        "library_path": "${library}"
    }
}
EOF
}

install_optional_apt_package_if_available() {
        local pkg="$1"

        if apt-cache show "${pkg}" >/dev/null 2>&1; then
                log "Installing optional package if needed: ${pkg}"
                DEBIAN_FRONTEND=noninteractive apt-get "${APT_DPKG_OPTIONS[@]}" install -y "${pkg}" \
                        || log "WARNING: Optional package install failed: ${pkg}"
        else
                log "Optional package is not available from configured apt sources: ${pkg}"
        fi
}

libnvidia_egl_gbm_already_provided_by_gl_server() {
        # On Ubuntu 25.10 the libnvidia-gl-${TARGET_NVIDIA_DRIVER_MAJOR}-server package
        # ships /usr/lib/x86_64-linux-gnu/libnvidia-egl-gbm.so.1.x.y itself. Installing
        # the standalone libnvidia-egl-gbm1 package then fails with a dpkg file-overwrite
        # conflict. Detect that case and skip the standalone install.
        local pkg
        for pkg in $(dpkg-query -W -f='${db:Status-Abbrev} ${binary:Package}\n' 'libnvidia-gl-*-server' 2>/dev/null \
                | awk '$1 == "ii" { print $2 }'); do
                if dpkg -L "${pkg}" 2>/dev/null | grep -qE '/libnvidia-egl-gbm[.]so'; then
                        log "libnvidia-egl-gbm.so is already provided by ${pkg}; skipping libnvidia-egl-gbm1 install to avoid dpkg file conflict."
                        return 0
                fi
        done
        return 1
}

ensure_nvidia_egl_helper_packages() {
        install_optional_apt_package_if_available "libnvidia-egl-wayland1"
        if libnvidia_egl_gbm_already_provided_by_gl_server; then
                return 0
        fi
        install_optional_apt_package_if_available "libnvidia-egl-gbm1"
}

ensure_nvidia_egl_vendor_json() {
        install -d -m 0755 /usr/share/glvnd/egl_vendor.d

        if ! library_available_to_loader "libEGL_nvidia.so.0"; then
                die "libEGL_nvidia.so.0 is missing even after NVIDIA GL package install"
        fi

        if ! grep -Rqs 'libEGL_nvidia[.]so' /usr/share/glvnd/egl_vendor.d/*.json 2>/dev/null; then
                log "NVIDIA EGL vendor JSON missing; writing /usr/share/glvnd/egl_vendor.d/10_nvidia.json"

                cat > /usr/share/glvnd/egl_vendor.d/10_nvidia.json <<'EOF'
{
    "file_format_version" : "1.0.0",
    "ICD" : {
        "library_path" : "libEGL_nvidia.so.0"
    }
}
EOF

                chmod 0644 /usr/share/glvnd/egl_vendor.d/10_nvidia.json
                ldconfig
        fi
}

ensure_nvidia_egl_vulkan_runtime_config() {
        local nvidia_egl_json nvidia_icd

        log "Ensuring NVIDIA EGL external platform and Vulkan runtime configuration for Sunshine"
        ensure_nvidia_egl_helper_packages
        ensure_nvidia_egl_vendor_json
        install -d -m 0755 /usr/share/egl/egl_external_platform.d

        if library_available_to_loader "libnvidia-egl-gbm.so.1"; then
                write_egl_external_platform_json \
                        /usr/share/egl/egl_external_platform.d/15_nvidia_gbm.json \
                        "libnvidia-egl-gbm.so.1"
        else
                log "WARNING: libnvidia-egl-gbm.so.1 was not found; GBM external platform JSON was not written"
        fi

        if library_available_to_loader "libnvidia-egl-wayland.so.1"; then
                write_egl_external_platform_json \
                        /usr/share/egl/egl_external_platform.d/10_nvidia_wayland.json \
                        "libnvidia-egl-wayland.so.1"
        else
                log "WARNING: libnvidia-egl-wayland.so.1 was not found; Wayland external platform JSON was not written"
        fi

        nvidia_egl_json="$(find_nvidia_egl_vendor_json)"
        [[ -n "${nvidia_egl_json}" ]] || die "Could not find or create NVIDIA EGL vendor JSON under /usr/share/glvnd/egl_vendor.d"
        library_available_to_loader "libEGL_nvidia.so.0" \
                || die "libEGL_nvidia.so.0 is missing from the dynamic loader cache/search path"
        library_available_to_loader "libnvidia-egl-wayland.so.1" \
                || log "WARNING: libnvidia-egl-wayland.so.1 is missing from the dynamic loader cache/search path"
        library_available_to_loader "libnvidia-egl-gbm.so.1" \
                || log "WARNING: libnvidia-egl-gbm.so.1 is missing from the dynamic loader cache/search path"

        nvidia_icd="$(find_nvidia_vulkan_icd)"
        [[ -n "${nvidia_icd}" ]] || die "Could not find NVIDIA Vulkan ICD JSON under /usr/share/vulkan/icd.d"

        install -d -m 0755 /etc/systemd/system/sunshine-headless.service.d
        cat > /etc/systemd/system/sunshine-headless.service.d/20-nvidia-vulkan-egl.conf <<EOF
[Service]
Environment=GBM_BACKEND=nvidia-drm
Environment=EGL_PLATFORM=gbm
Environment=__EGL_VENDOR_LIBRARY_FILENAMES=${nvidia_egl_json}
Environment=__EGL_EXTERNAL_PLATFORM_CONFIG_DIRS=/usr/share/egl/egl_external_platform.d
Environment=__GLX_VENDOR_LIBRARY_NAME=nvidia
Environment=VK_ICD_FILENAMES=${nvidia_icd}
Environment=VK_DRIVER_FILES=${nvidia_icd}
Environment=LIBGL_ALWAYS_SOFTWARE=0
Environment=CUDA_VISIBLE_DEVICES=0
EOF
}

run_as_user() {
        local user="$1"
        shift
        runuser -u "${user}" -- "$@"
}

wait_for_cloud_init() {
        local timeout_seconds

        if command -v cloud-init >/dev/null 2>&1; then
                install -d -m 0755 "${CLOUDDEPLOY_STATE_DIR}" 2>/dev/null || true
                if [[ -f "${CLOUDDEPLOY_STATE_DIR}/cloud-init-broken" ]]; then
                        echo "cloud-init was previously marked broken on this image; skipping cloud-init wait."
                        return 0
                fi

                timeout_seconds=180
                if [[ "${CLOUDDEPLOY_CONTINUE:-0}" == "1" ]]; then
                        timeout_seconds="${CLOUDDEPLOY_CONTINUE_CLOUD_INIT_TIMEOUT:-20}"
                fi

                echo "Waiting for cloud-init for up to ${timeout_seconds}s..."
                if timeout "${timeout_seconds}" cloud-init status --wait; then
                        return 0
                fi

                echo "cloud-init is broken or timed out on this image; continuing because package/network checks will run next."
                touch "${CLOUDDEPLOY_STATE_DIR}/cloud-init-broken" 2>/dev/null || true
        fi
}

wait_for_apt() {
        local waited=0
        while fuser /var/lib/dpkg/lock-frontend \
                    /var/lib/dpkg/lock \
                    /var/lib/apt/lists/lock \
                    /var/cache/apt/archives/lock >/dev/null 2>&1

        do
                echo "Waiting for apt/dpkg lock..."
                sleep 5
                waited=$((waited + 5))

                if [ "${waited}" -ge 600 ]; then
                        echo "Lock holders after 10 minutes:"
                        fuser -v /var/lib/dpkg/lock-frontend \
                                /var/lib/dpkg/lock \
                                /var/lib/apt/lists/lock \
                                /var/cache/apt/archives/lock 2>/dev/null || true
                        ps -ef | grep -E 'apt|dpkg|packagekit|cloud-init' | grep -v grep || true
                        die "Timed out while waiting for apt/dpkg locks"
                fi
        done
}

dpkg_output_has_updates_parse_error() {
        local output="$1"
        printf '%s\n' "${output}" \
                | grep -Eiq "parsing file ['\"]?/var/lib/dpkg/updates/[0-9]+|/var/lib/dpkg/updates/[0-9]+.*(end of file|field name|parse)"
}

dpkg_output_has_snapd_postinst_error() {
        local output="$1"
        if printf '%s\n' "${output}" | grep -Eiq 'snapd'; then
                printf '%s\n' "${output}" \
                        | grep -Eiq 'error processing package snapd|post-installation script|setcap|must be a regular [(]non-symlink[)] file'
                return $?
        fi

        return 1
}

dpkg_output_has_libblockdev_bad_state() {
        local output="$1"
        printf '%s\n' "${output}" | grep -Eiq 'very bad inconsistent state|libblockdev-mdraid3'
}

move_corrupt_dpkg_updates_aside() {
        local backup_dir="/root/dpkg-bad-updates-backup"
        local stamp update_file moved=0

        log "Detected corrupt dpkg updates queue; moving /var/lib/dpkg/updates/* aside"
        install -d -m 0700 "${backup_dir}"
        stamp="$(date '+%Y%m%d-%H%M%S')"

        shopt -s nullglob
        for update_file in /var/lib/dpkg/updates/*; do
                [[ -e "${update_file}" ]] || continue
                mv "${update_file}" "${backup_dir}/${stamp}-$(basename "${update_file}")" || true
                moved=1
        done
        shopt -u nullglob

        if [[ "${moved}" == "1" ]]; then
                log "Moved corrupt dpkg update files to ${backup_dir}"
        else
                log "No dpkg update queue files were present to move"
        fi
}

purge_snapd_after_postinst_failure() {
        log "Detected snapd postinst failure blocking dpkg; purging snapd because CloudDeploy does not require Snap"

        systemctl stop snapd.service snapd.socket snapd.seeded.service >/dev/null 2>&1 || true
        systemctl disable snapd.service snapd.socket snapd.seeded.service >/dev/null 2>&1 || true
        dpkg --purge --force-all snapd || true
}

repair_libblockdev_bad_state() {
        local audit_out configure_out fix_out
        local download_rc=0
        local -a blockdev_pkgs

        export DEBIAN_FRONTEND=noninteractive
        blockdev_pkgs=(
                libblockdev-mdraid3
                libblockdev-nvme3
                libblockdev-part3
                libblockdev-swap3
                libblockdev3
        )

        log "Detected libblockdev package in a very bad inconsistent state; reinstalling libblockdev stack"
        repair_initramfs_tools_config || true
        wait_for_apt
        apt-get update || true
        if DEBIAN_FRONTEND=noninteractive apt-get "${APT_DPKG_OPTIONS[@]}" install --reinstall -y "${blockdev_pkgs[@]}"; then
                audit_out="$(dpkg --audit 2>&1 || true)"
                printf '%s\n' "${audit_out}"
                configure_out="$(dpkg --configure -a 2>&1)" || true
                printf '%s\n' "${configure_out}"
                fix_out="$(apt-get "${APT_DPKG_OPTIONS[@]}" -f install -y 2>&1)" || true
                printf '%s\n' "${fix_out}"
                return 0
        fi

        log "apt reinstall did not repair libblockdev-mdraid3; downloading and forcing package reinstall"
        (
                cd /tmp
                rm -f libblockdev-mdraid3_*.deb
                apt-get download libblockdev-mdraid3
                dpkg -i --force-all ./libblockdev-mdraid3_*.deb
        ) || download_rc=$?

        if [[ "${download_rc}" -ne 0 ]]; then
                log "Manual libblockdev-mdraid3 download/install failed with rc=${download_rc}; apt -f will still attempt repair"
        fi

        fix_out="$(apt-get "${APT_DPKG_OPTIONS[@]}" -f install -y 2>&1)" || true
        printf '%s\n' "${fix_out}"
        configure_out="$(dpkg --configure -a 2>&1)" || true
        printf '%s\n' "${configure_out}"
        audit_out="$(dpkg --audit 2>&1 || true)"
        printf '%s\n' "${audit_out}"
        configure_out="$(dpkg --configure -a 2>&1)" || true
        printf '%s\n' "${configure_out}"
        fix_out="$(apt-get "${APT_DPKG_OPTIONS[@]}" -f install -y 2>&1)" || true
        printf '%s\n' "${fix_out}"
}

repair_dpkg_state_if_needed() {
        if [[ "${CLOUDDEPLOY_DPKG_REPAIR_ACTIVE:-0}" == "1" ]]; then
                return 0
        fi

        CLOUDDEPLOY_DPKG_REPAIR_ACTIVE=1
        export CLOUDDEPLOY_DPKG_REPAIR_ACTIVE

        wait_for_apt
        repair_initramfs_tools_config

        local audit_out audit_rc configure_out configure_rc fix_out fix_rc

        audit_out="$(dpkg --audit 2>&1)" && audit_rc=0 || audit_rc=$?
        if [[ "${audit_rc}" -ne 0 ]] && dpkg_output_has_updates_parse_error "${audit_out}"; then
                printf '%s\n' "${audit_out}"
                move_corrupt_dpkg_updates_aside
        fi

        configure_out="$(DEBIAN_FRONTEND=noninteractive dpkg --configure -a 2>&1)" && configure_rc=0 || configure_rc=$?
        if [[ "${configure_rc}" -eq 0 ]]; then
                CLOUDDEPLOY_DPKG_REPAIR_ACTIVE=0
                export CLOUDDEPLOY_DPKG_REPAIR_ACTIVE
                return 0
        fi

        printf '%s\n' "${configure_out}"

        if dpkg_output_has_libblockdev_bad_state "${configure_out}"; then
                repair_libblockdev_bad_state
                configure_out="$(DEBIAN_FRONTEND=noninteractive dpkg --configure -a 2>&1)" && configure_rc=0 || configure_rc=$?
                printf '%s\n' "${configure_out}"
                if [[ "${configure_rc}" -eq 0 ]]; then
                        CLOUDDEPLOY_DPKG_REPAIR_ACTIVE=0
                        export CLOUDDEPLOY_DPKG_REPAIR_ACTIVE
                        return 0
                fi
        fi

        if dpkg_output_has_updates_parse_error "${configure_out}"; then
                move_corrupt_dpkg_updates_aside
                configure_out="$(DEBIAN_FRONTEND=noninteractive dpkg --configure -a 2>&1)" && configure_rc=0 || configure_rc=$?
                if [[ "${configure_rc}" -eq 0 ]]; then
                        CLOUDDEPLOY_DPKG_REPAIR_ACTIVE=0
                        export CLOUDDEPLOY_DPKG_REPAIR_ACTIVE
                        return 0
                fi

                printf '%s\n' "${configure_out}"
        fi

        if dpkg_output_has_snapd_postinst_error "${configure_out}"; then
                purge_snapd_after_postinst_failure

                fix_out="$(DEBIAN_FRONTEND=noninteractive apt-get "${APT_DPKG_OPTIONS[@]}" -f install -y 2>&1)" && fix_rc=0 || fix_rc=$?
                printf '%s\n' "${fix_out}"
                if [[ "${fix_rc}" -ne 0 ]]; then
                        log "apt-get -f install failed during snapd repair; rerunning dpkg --configure -a before deciding"
                fi

                configure_out="$(DEBIAN_FRONTEND=noninteractive dpkg --configure -a 2>&1)" && configure_rc=0 || configure_rc=$?
                printf '%s\n' "${configure_out}"
                if [[ "${configure_rc}" -eq 0 ]]; then
                        CLOUDDEPLOY_DPKG_REPAIR_ACTIVE=0
                        export CLOUDDEPLOY_DPKG_REPAIR_ACTIVE
                        return 0
                fi
        fi

        fix_out="$(DEBIAN_FRONTEND=noninteractive apt-get "${APT_DPKG_OPTIONS[@]}" -f install -y 2>&1)" && fix_rc=0 || fix_rc=$?
        printf '%s\n' "${fix_out}"
        if dpkg_output_has_libblockdev_bad_state "${fix_out}"; then
                repair_libblockdev_bad_state
                configure_out="$(DEBIAN_FRONTEND=noninteractive dpkg --configure -a 2>&1)" && configure_rc=0 || configure_rc=$?
                printf '%s\n' "${configure_out}"
                if [[ "${configure_rc}" -eq 0 ]]; then
                        CLOUDDEPLOY_DPKG_REPAIR_ACTIVE=0
                        export CLOUDDEPLOY_DPKG_REPAIR_ACTIVE
                        return 0
                fi
        fi

        if [[ "${fix_rc}" -eq 0 ]]; then
                configure_out="$(DEBIAN_FRONTEND=noninteractive dpkg --configure -a 2>&1)" && configure_rc=0 || configure_rc=$?
                printf '%s\n' "${configure_out}"
                if [[ "${configure_rc}" -eq 0 ]]; then
                        CLOUDDEPLOY_DPKG_REPAIR_ACTIVE=0
                        export CLOUDDEPLOY_DPKG_REPAIR_ACTIVE
                        return 0
                fi
        fi

        if nvidia_install_output_has_dkms_kernel_failure "${fix_out}"; then
                log "apt-get -f install is blocked by NVIDIA DKMS configuration; deferring to NVIDIA stale-kernel recovery"
                CLOUDDEPLOY_DPKG_REPAIR_ACTIVE=0
                export CLOUDDEPLOY_DPKG_REPAIR_ACTIVE
                return 0
        fi

        if nvidia_install_output_has_dkms_kernel_failure "${configure_out}"; then
                log "dpkg is blocked by NVIDIA DKMS configuration; deferring to NVIDIA stale-kernel recovery"
                CLOUDDEPLOY_DPKG_REPAIR_ACTIVE=0
                export CLOUDDEPLOY_DPKG_REPAIR_ACTIVE
                return 0
        fi

        CLOUDDEPLOY_DPKG_REPAIR_ACTIVE=0
        export CLOUDDEPLOY_DPKG_REPAIR_ACTIVE
        die "dpkg remains broken after conservative repair; see diagnostics above"
}

apt_update_retry() {
        wait_for_cloud_init
        wait_for_apt

        local update_out update_rc
        update_out="$(DEBIAN_FRONTEND=noninteractive apt-get update -o Acquire::Retries=6 -o Acquire::http::Timeout=20 2>&1)" && update_rc=0 || update_rc=$?
        printf '%s\n' "${update_out}"

        if [[ "${update_rc}" -eq 0 ]]; then
                return 0
        fi

        if printf '%s\n' "${update_out}" | grep -Eiq 'dpkg was interrupted|dpkg --configure -a'; then
                log "apt-get update reported interrupted dpkg state; attempting conservative dpkg repair"
                repair_dpkg_state_if_needed
                update_out="$(DEBIAN_FRONTEND=noninteractive apt-get update -o Acquire::Retries=6 -o Acquire::http::Timeout=20 2>&1)" && update_rc=0 || update_rc=$?
                printf '%s\n' "${update_out}"
        fi

        return "${update_rc}"
}

apt_install_wait() {
        local install_out install_rc

        wait_for_apt
        repair_dpkg_state_if_needed
        install_out="$(DEBIAN_FRONTEND=noninteractive apt-get "${APT_DPKG_OPTIONS[@]}" install -y "$@" 2>&1)" && install_rc=0 || install_rc=$?
        printf '%s\n' "${install_out}"
        if [[ "${install_rc}" -ne 0 ]]; then
                log "apt-get install failed; attempting dpkg repair and one retry"
                repair_dpkg_state_if_needed
                install_out="$(DEBIAN_FRONTEND=noninteractive apt-get "${APT_DPKG_OPTIONS[@]}" install -y "$@" 2>&1)" && install_rc=0 || install_rc=$?
                printf '%s\n' "${install_out}"
        fi
        return "${install_rc}"
}

apt_purge_wait() {
        wait_for_apt
        repair_dpkg_state_if_needed
        DEBIAN_FRONTEND=noninteractive apt-get "${APT_DPKG_OPTIONS[@]}" purge -y "$@"
}

write_sunshine_config() {
        local ts_ip
        install -d -m 0755 -o "${HEADLESS_USER}" -g "${HEADLESS_USER}" "${HOME_DIR}/.config/sunshine"
        ts_ip="$(tailscale_ipv4 || true)"

cat > "${HOME_DIR}/.config/sunshine/sunshine.conf" <<EOF
min_log_level = debug
encoder = ${SUNSHINE_ENCODER}
capture = kms
adapter_name = ${SUNSHINE_DRM_DEVICE}
hevc_mode = ${SUNSHINE_HEVC_MODE}
av1_mode = ${SUNSHINE_AV1_MODE}
hdr = ${ENABLE_HDR}
fps = [60, ${TARGET_FPS}]
resolutions = [1920x1080, 2560x1440, ${TARGET_WIDTH}x${TARGET_HEIGHT}]
stream_audio = disabled
address_family = ipv4
ping_timeout = 60000
EOF
        if [[ -n "${ts_ip}" ]]; then
                printf 'csrf_allowed_origins = https://%s:47990\n' "${ts_ip}" >> "${HOME_DIR}/.config/sunshine/sunshine.conf"
        fi
        chown "${HEADLESS_USER}:${HEADLESS_USER}" "${HOME_DIR}/.config/sunshine/sunshine.conf"
}

install_clouddeploy_helpers() {
        cat > /usr/local/sbin/clouddeploy-run <<'EOF'
#!/usr/bin/env bash
set -euo pipefail

ENV_FILE="/etc/clouddeploy-wayland.env"
OVERRIDE_SUNSHINE_SOURCE_MODE="${SUNSHINE_SOURCE_MODE-}"
OVERRIDE_SUNSHINE_PASS="${SUNSHINE_PASS-}"
OVERRIDE_TAILSCALE_AUTHKEY="${TAILSCALE_AUTHKEY-}"
OVERRIDE_SUNSHINE_BUILD_JOBS="${SUNSHINE_BUILD_JOBS-}"
OVERRIDE_INSTALL_OPTIONAL_APPS="${INSTALL_OPTIONAL_APPS-}"

if [[ -f "${ENV_FILE}" ]]; then
        set -a
        # shellcheck disable=SC1090
        source "${ENV_FILE}"
        set +a
fi

if [[ -n "${OVERRIDE_SUNSHINE_SOURCE_MODE}" ]]; then
        export SUNSHINE_SOURCE_MODE="${OVERRIDE_SUNSHINE_SOURCE_MODE}"
fi
if [[ -n "${OVERRIDE_SUNSHINE_PASS}" ]]; then
        export SUNSHINE_PASS="${OVERRIDE_SUNSHINE_PASS}"
fi
if [[ -n "${OVERRIDE_TAILSCALE_AUTHKEY}" ]]; then
        export TAILSCALE_AUTHKEY="${OVERRIDE_TAILSCALE_AUTHKEY}"
fi
if [[ -n "${OVERRIDE_SUNSHINE_BUILD_JOBS}" ]]; then
        export SUNSHINE_BUILD_JOBS="${OVERRIDE_SUNSHINE_BUILD_JOBS}"
fi
if [[ -n "${OVERRIDE_INSTALL_OPTIONAL_APPS}" ]]; then
        export INSTALL_OPTIONAL_APPS="${OVERRIDE_INSTALL_OPTIONAL_APPS}"
fi

REPO_DIR="${CLOUDDEPLOY_REPO_DIR:-/home/user/CloudDeploy-mover}"

cd "${REPO_DIR}"
export SUNSHINE_SOURCE_MODE="${SUNSHINE_SOURCE_MODE-}"
export SUNSHINE_PASS="${SUNSHINE_PASS-}"
export TAILSCALE_AUTHKEY="${TAILSCALE_AUTHKEY-}"
export SUNSHINE_BUILD_JOBS="${SUNSHINE_BUILD_JOBS-}"
export INSTALL_OPTIONAL_APPS="${INSTALL_OPTIONAL_APPS-}"
exec bash ./CloudDeploy-wayland.sh
EOF
        chmod 0755 /usr/local/sbin/clouddeploy-run

        cat > /usr/local/sbin/clouddeploy-write-env <<'EOF'
#!/usr/bin/env bash
set -euo pipefail

ENV_FILE="/etc/clouddeploy-wayland.env"

[[ ${EUID} -eq 0 ]] || { echo "Run with sudo." >&2; exit 1; }

prompt_default() {
        local name="$1"
        local default="$2"
        local value

        read -r -p "${name} [${default}]: " value
        printf '%s\n' "${value:-${default}}"
}

HEADLESS_USER="$(prompt_default HEADLESS_USER "${HEADLESS_USER:-user}")"
SUNSHINE_USER="$(prompt_default SUNSHINE_USER "${SUNSHINE_USER:-${HEADLESS_USER}}")"
if [[ "${HEADLESS_USER}" == "root" ]]; then
        echo "HEADLESS_USER=root is refused by default; use a real UID>=1000 user." >&2
        exit 1
fi
if [[ "${SUNSHINE_USER}" == "root" ]]; then
        SUNSHINE_USER="${HEADLESS_USER}"
fi
read -s -r -p "SUNSHINE_PASS: " SUNSHINE_PASS
echo
read -s -r -p "TAILSCALE_AUTHKEY (blank to skip): " TAILSCALE_AUTHKEY
echo

if [[ -z "${SUNSHINE_PASS}" ]]; then
        echo "SUNSHINE_PASS is required." >&2
        exit 1
fi
if [[ -n "${TAILSCALE_AUTHKEY}" && "${TAILSCALE_AUTHKEY}" =~ [[:space:]] ]]; then
        echo "TAILSCALE_AUTHKEY must be a single-line key. Generate a fresh key; do not paste line breaks." >&2
        exit 1
fi

install -m 0600 /dev/null "${ENV_FILE}"
{
        printf 'HEADLESS_USER=%q\n' "${HEADLESS_USER}"
        printf 'SUNSHINE_USER=%q\n' "${SUNSHINE_USER}"
        printf 'SUNSHINE_PASS=%q\n' "${SUNSHINE_PASS}"
        printf 'TAILSCALE_AUTHKEY=%q\n' "${TAILSCALE_AUTHKEY}"
        printf 'SUNSHINE_DEB_URL=%q\n' "https://github.com/LizardByte/Sunshine/releases/download/v2025.924.154138/sunshine-ubuntu-24.04-amd64.deb"
        printf 'FORCED_CONNECTOR=%q\n' "DP-1"
        printf 'TARGET_WIDTH=%q\n' "3840"
        printf 'TARGET_HEIGHT=%q\n' "2160"
        printf 'TARGET_FPS=%q\n' "120"
        printf 'ENABLE_HDR=%q\n' "0"
        printf 'ENABLE_PLASMA6=%q\n' "0"
        printf 'EDID_PROFILE=%q\n' "auto"
        printf 'KWIN_VTNR=%q\n' "7"
        printf 'KWIN_WAYLAND_DISPLAY=%q\n' "wayland-0"
        printf 'WESTON_WAYLAND_DISPLAY=%q\n' "wayland-cd"
        printf 'WESTON_MODE=%q\n' "3840x2160@120"
        printf 'SUNSHINE_ENCODER=%q\n' "nvenc"
        printf 'SUNSHINE_DRM_DEVICE=%q\n' "auto"
        printf 'SESSION_BACKEND=%q\n' "plasma"
        printf 'STREAM_MODE=%q\n' "plasma"
        printf 'PLASMA_LAUNCH_MODE=%q\n' "startplasma"
        printf 'INSTALL_OPTIONAL_APPS=%q\n' "0"
        printf 'SUNSHINE_AV1_MODE=%q\n' "2"
        printf 'SUNSHINE_HEVC_MODE=%q\n' "0"
        printf 'TARGET_NVIDIA_DRIVER_MAJOR=%q\n' "580"
        printf 'INSTALL_CUDA_TOOLKIT=%q\n' "1"
        printf 'CUDA_TOOLKIT_PACKAGE=%q\n' "cuda-toolkit"
        printf 'CUDA_INSTALL_METHOD=%q\n' "auto"
        printf 'REQUIRE_CUDA_TOOLKIT=%q\n' "1"
        printf 'CUDA_RUNFILE_VERSION=%q\n' "13.0.2"
        printf 'CUDA_RUNFILE_DRIVER_VERSION=%q\n' "580.95.05"
        printf 'CUDA_RUNFILE_URL=%q\n' "https://developer.download.nvidia.com/compute/cuda/13.0.2/local_installers/cuda_13.0.2_580.95.05_linux.run"
        printf 'FORCE_DRIVER_UPGRADE=%q\n' "1"
        printf 'SUNSHINE_SOURCE_MODE=%q\n' "fork"
        printf 'SUNSHINE_FORK_REPO=%q\n' "https://github.com/NoviceAtPython/Sunshine.git"
        printf 'SUNSHINE_DIAGNOSTIC_FORK_BRANCH=%q\n' "codex/sunshine-pairing-diagnostics"
        printf 'SUNSHINE_CLEAN_FORK_BRANCH=%q\n' "clouddeploy-clean-pairing-stream-fix"
        printf 'SUNSHINE_FORK_BRANCH=%q\n' "codex/sunshine-pairing-diagnostics"
        printf 'SUNSHINE_BUILD_DIR=%q\n' "/opt/sunshine-src"
        printf 'SUNSHINE_BUILD_JOBS=%q\n' "2"
        printf 'SUNSHINE_INSTALL_BIN=%q\n' "/usr/local/bin/sunshine-clouddeploy"
        printf 'SUNSHINE_ENABLE_CUDA_MODULE=%q\n' "auto"
        printf 'ENABLE_USER_NOPASSWD_SUDO=%q\n' "1"
        printf 'ALLOW_ROOT_SESSION=%q\n' "0"
} > "${ENV_FILE}"
chmod 0600 "${ENV_FILE}"
echo "Wrote ${ENV_FILE} with mode 0600."
EOF
        chmod 0755 /usr/local/sbin/clouddeploy-write-env

        cat > /usr/local/sbin/clouddeploy-reset-streaming <<'EOF'
#!/usr/bin/env bash
set -euo pipefail

ENV_FILE="/etc/clouddeploy-wayland.env"
if [[ -f "${ENV_FILE}" ]]; then
        set -a
        # shellcheck disable=SC1090
        source "${ENV_FILE}"
        set +a
fi

HEADLESS_USER="${HEADLESS_USER:-user}"
ALLOW_ROOT_SESSION="${ALLOW_ROOT_SESSION:-0}"
if [[ "${HEADLESS_USER}" == "root" && "${ALLOW_ROOT_SESSION}" != "1" ]]; then
        if [[ "$(id -u user 2>/dev/null || echo 0)" -ge 1000 ]]; then
                HEADLESS_USER="user"
        else
                HEADLESS_USER="$(getent passwd | awk -F: '$3 >= 1000 && $1 != "nobody" && $7 !~ /(nologin|false)$/ { print $1; exit }')"
        fi
        [[ -n "${HEADLESS_USER}" ]] || { echo "HEADLESS_USER resolved to root and no real UID>=1000 user was found" >&2; exit 1; }
fi
STREAM_MODE="${STREAM_MODE:-plasma}"
ENABLE_PLASMA6="${ENABLE_PLASMA6:-0}"
if [[ "${ENABLE_PLASMA6}" == "1" && "${STREAM_MODE}" == "plasma" ]]; then
        STREAM_MODE="plasma6"
fi
PLASMA_LAUNCH_MODE="${PLASMA_LAUNCH_MODE:-startplasma}"
FORCED_CONNECTOR="${FORCED_CONNECTOR:-DP-1}"
TARGET_WIDTH="${TARGET_WIDTH:-3840}"
TARGET_HEIGHT="${TARGET_HEIGHT:-2160}"
TARGET_FPS="${TARGET_FPS:-120}"
ENABLE_HDR="${ENABLE_HDR:-0}"
KWIN_WAYLAND_DISPLAY="${KWIN_WAYLAND_DISPLAY:-wayland-0}"
WESTON_WAYLAND_DISPLAY="${WESTON_WAYLAND_DISPLAY:-wayland-cd}"
SUNSHINE_ENCODER="${SUNSHINE_ENCODER:-nvenc}"
SUNSHINE_DRM_DEVICE="${SUNSHINE_DRM_DEVICE:-auto}"
SUNSHINE_HEVC_MODE="${SUNSHINE_HEVC_MODE:-0}"
SUNSHINE_AV1_MODE="${SUNSHINE_AV1_MODE:-2}"
HOME_DIR="$(getent passwd "${HEADLESS_USER}" | cut -d: -f6 || true)"
HOME_DIR="${HOME_DIR:-/home/${HEADLESS_USER}}"
HEADLESS_UID="$(id -u "${HEADLESS_USER}")"
RUNTIME_DIR="/run/user/${HEADLESS_UID}"
KWIN_DISPLAY="${KWIN_WAYLAND_DISPLAY}"

detect_nvidia_drm_card() {
        local card vendor

        for card in /sys/class/drm/card[0-9]; do
                [[ -e "${card}/device/vendor" ]] || continue
                vendor="$(cat "${card}/device/vendor" 2>/dev/null || true)"
                if [[ "${vendor}" == "0x10de" ]]; then
                        printf '/dev/dri/%s\n' "$(basename "${card}")"
                        return 0
                fi
        done

        return 1
}

if [[ "${SUNSHINE_DRM_DEVICE}" == "auto" ]]; then
        SUNSHINE_DRM_DEVICE="$(detect_nvidia_drm_card || true)"
fi

if [[ -z "${SUNSHINE_DRM_DEVICE}" || "${SUNSHINE_DRM_DEVICE}" == "auto" ]]; then
        echo "Could not detect NVIDIA DRM card for Sunshine KMS capture." >&2
        exit 1
fi

        case "${STREAM_MODE}" in
        plasma|plasma6|kwin|realvt)
                COMPOSITOR_SERVICE="kwin-realvt.service"
                COMPOSITOR_LOG_UNIT="kwin-realvt.service"
                ;;
        weston)
                COMPOSITOR_SERVICE="weston-kms-session.service"
                COMPOSITOR_LOG_UNIT="weston-kms-session.service"
                ;;
        gamescope)
                echo "STREAM_MODE=gamescope is reserved for the future HDR game path and is not enabled yet." >&2
                exit 1
                ;;
        *)
                echo "Unsupported STREAM_MODE='${STREAM_MODE}'. Supported now: plasma, plasma6, kwin, weston." >&2
                exit 1
                ;;
esac

echo "Performing exact known-good clean reset before final validation"

KWIN_HEALTHY=0
if [[ "${COMPOSITOR_SERVICE}" == "kwin-realvt.service" ]] \
        && systemctl is-active --quiet kwin-realvt.service \
        && [[ -S "${RUNTIME_DIR}/${KWIN_DISPLAY}" ]] \
        && pgrep -u "${HEADLESS_USER}" -x kwin_wayland >/dev/null 2>&1; then
        KWIN_HEALTHY=1
        echo "kwin-realvt.service already active; not restarting real-VT KWin"
fi

if [[ "${KWIN_HEALTHY}" == "1" ]]; then
        systemctl stop sunshine-headless.service sunshine-direct.service sunshine-manual.service sunshine-wayland-nodbus.service plasma-realvt.service plasma-shell-realvt.service weston-kms-session.service plasma-kms-session.service gamescope-session.service 2>/dev/null || true
        pkill -9 -u "${HEADLESS_USER}" -f 'plasmashell|kactivitymanagerd|plasma_session|plasma_waitforname|ksmserver|ksplashqml|startplasma-wayland|kdeinit5|klauncher|kded|sunshine|weston' 2>/dev/null || true
        rm -f /tmp/runtime-"${HEADLESS_USER}"/wayland-* 2>/dev/null || true
else
        systemctl stop sunshine-headless.service sunshine-direct.service sunshine-manual.service sunshine-wayland-nodbus.service plasma-realvt.service kwin-realvt.service plasma-shell-realvt.service weston-kms-session.service plasma-kms-session.service gamescope-session.service 2>/dev/null || true
        pkill -9 -u "${HEADLESS_USER}" -f 'kwin_wayland|kwin_wayland_wrapper|plasmashell|kactivitymanagerd|plasma_session|plasma_waitforname|ksmserver|ksplashqml|startplasma-wayland|kdeinit5|klauncher|kded|sunshine|weston|Xwayland' 2>/dev/null || true
        rm -f "${RUNTIME_DIR}"/wayland-* /tmp/runtime-"${HEADLESS_USER}"/wayland-* 2>/dev/null || true
fi

install -d -m 0755 -o "${HEADLESS_USER}" -g "${HEADLESS_USER}" "${HOME_DIR}/.config/sunshine"
TS_IP=""
if command -v tailscale >/dev/null 2>&1; then
        TS_IP="$(tailscale ip -4 2>/dev/null | head -n1 || true)"
fi
cat > "${HOME_DIR}/.config/sunshine/sunshine.conf" <<CONF
min_log_level = debug
encoder = ${SUNSHINE_ENCODER}
capture = kms
adapter_name = ${SUNSHINE_DRM_DEVICE}
hevc_mode = ${SUNSHINE_HEVC_MODE}
av1_mode = ${SUNSHINE_AV1_MODE}
hdr = ${ENABLE_HDR}
fps = [60, ${TARGET_FPS}]
resolutions = [1920x1080, 2560x1440, ${TARGET_WIDTH}x${TARGET_HEIGHT}]
stream_audio = disabled
address_family = ipv4
ping_timeout = 60000
CONF
if [[ -n "${TS_IP}" ]]; then
        printf 'csrf_allowed_origins = https://%s:47990\n' "${TS_IP}" >> "${HOME_DIR}/.config/sunshine/sunshine.conf"
fi
chown "${HEADLESS_USER}:${HEADLESS_USER}" "${HOME_DIR}/.config/sunshine/sunshine.conf"

systemctl reset-failed plasma-realvt.service kwin-realvt.service plasma-shell-realvt.service weston-kms-session.service sunshine-headless.service || true

if [[ "${KWIN_HEALTHY}" == "1" ]]; then
        echo "Skipping compositor start because real-VT KWin is already healthy"
else
        systemctl start "${COMPOSITOR_SERVICE}"
fi

if [[ "${COMPOSITOR_SERVICE}" == "kwin-realvt.service" ]]; then
        sleep 8
        /usr/local/bin/clouddeploy-force-kwin-mode.sh || echo "WARNING: clouddeploy-force-kwin-mode failed; final validation will decide success."
        systemctl restart plasma-shell-realvt.service || true
else
        sleep 5
fi

journalctl -u "${COMPOSITOR_LOG_UNIT}" -n 160 --no-pager \
  | grep -Ei "Output ${FORCED_CONNECTOR}|${TARGET_WIDTH}x${TARGET_HEIGHT}|current|EGL vendor|kwin|plasmashell|plasma_session|kded|startplasma|Wayland|fatal|error|--drm|--xwayland" || true

systemctl restart sunshine-headless.service
sleep 6

journalctl -u sunshine-headless.service -n 180 --no-pager \
  | grep -Ei 'Desktop resolution|Monitor 0|Found monitor|Screencasting|/dev/dri|Creating encoder|Nvenc initialized|Found H.264|Found HEVC|Found AV1|sample_all_black|EGL|GL: renderer|llvmpipe|Error|Fatal' || true
EOF
        chmod 0755 /usr/local/sbin/clouddeploy-reset-streaming

        cat > /etc/systemd/system/clouddeploy-reset-streaming.service <<'EOF'
[Unit]
Description=Reset CloudDeploy compositor/Sunshine streaming stack

[Service]
Type=oneshot
EnvironmentFile=-/etc/clouddeploy-wayland.env
ExecStart=/usr/local/sbin/clouddeploy-reset-streaming
EOF

        cat > /usr/local/sbin/clouddeploy-watch-streaming <<'EOF'
#!/usr/bin/env bash
set -euo pipefail

ENV_FILE="/etc/clouddeploy-wayland.env"
LOCK_FILE="/run/clouddeploy-reset-streaming.lock"
LAST_FILE="/run/clouddeploy-reset-streaming.last"

if [[ -f "${ENV_FILE}" ]]; then
        set -a
        # shellcheck disable=SC1090
        source "${ENV_FILE}"
        set +a
fi

exec 9>"${LOCK_FILE}"
flock -n 9 || exit 0

now="$(date +%s)"
if [[ -f "${LAST_FILE}" ]]; then
        last="$(cat "${LAST_FILE}" 2>/dev/null || echo 0)"
        if [[ "${last}" =~ ^[0-9]+$ ]] && (( now - last < 180 )); then
                exit 0
        fi
fi

recent_log="$(journalctl -u sunshine-headless.service --since '45 seconds ago' --no-pager 2>/dev/null || true)"
if printf '%s\n' "${recent_log}" | grep -Eiq "Couldn't find monitor \\[0\\]|Unable to find display or encoder during startup|Couldn't find any working encoder matching|Fatal: Please ensure your manually chosen GPU and monitor are connected and powered on"; then
        printf '%s\n' "${now}" > "${LAST_FILE}"
        systemctl start clouddeploy-reset-streaming.service
fi
EOF
        chmod 0755 /usr/local/sbin/clouddeploy-watch-streaming

        cat > /etc/systemd/system/clouddeploy-watch-streaming.service <<'EOF'
[Unit]
Description=Watch CloudDeploy Sunshine logs for recoverable encoder errors

[Service]
Type=oneshot
EnvironmentFile=-/etc/clouddeploy-wayland.env
ExecStart=/usr/local/sbin/clouddeploy-watch-streaming
EOF

        cat > /etc/systemd/system/clouddeploy-watch-streaming.timer <<'EOF'
[Unit]
Description=Run CloudDeploy Sunshine recovery watchdog

[Timer]
OnBootSec=60
OnUnitActiveSec=30
Unit=clouddeploy-watch-streaming.service

[Install]
WantedBy=timers.target
EOF

        cat > /usr/local/sbin/clouddeploy-diagnose-kms <<'EOF'
#!/usr/bin/env bash
set -euo pipefail

ENV_FILE="/etc/clouddeploy-wayland.env"
if [[ -f "${ENV_FILE}" ]]; then
        set -a
        # shellcheck disable=SC1090
        source "${ENV_FILE}"
        set +a
fi

HEADLESS_USER="${HEADLESS_USER:-user}"
KWIN_WAYLAND_DISPLAY="${KWIN_WAYLAND_DISPLAY:-wayland-0}"
SUNSHINE_DRM_DEVICE="${SUNSHINE_DRM_DEVICE:-auto}"
HOME_DIR="$(getent passwd "${HEADLESS_USER}" | cut -d: -f6 || true)"
HOME_DIR="${HOME_DIR:-/home/${HEADLESS_USER}}"
HEADLESS_UID="$(id -u "${HEADLESS_USER}")"
RUNTIME_DIR="/run/user/${HEADLESS_UID}"
KWIN_DISPLAY="${KWIN_WAYLAND_DISPLAY}"

detect_nvidia_drm_card() {
        local card vendor
        for card in /sys/class/drm/card[0-9]; do
                [[ -e "${card}/device/vendor" ]] || continue
                vendor="$(cat "${card}/device/vendor" 2>/dev/null || true)"
                if [[ "${vendor}" == "0x10de" ]]; then
                        printf '/dev/dri/%s\n' "$(basename "${card}")"
                        return 0
                fi
        done
        return 1
}

if [[ "${SUNSHINE_DRM_DEVICE}" == "auto" ]]; then
        SUNSHINE_DRM_DEVICE="$(detect_nvidia_drm_card || true)"
fi

echo "=== nvidia_drm modeset ==="
cat /sys/module/nvidia_drm/parameters/modeset 2>/dev/null || true

echo
echo "=== Sunshine cap_sys_admin ==="
if command -v sunshine >/dev/null 2>&1; then
        sunshine_bin="$(readlink -f "$(command -v sunshine)")"
        getcap "${sunshine_bin}" || true
else
        echo "sunshine not found"
fi

echo
echo "=== KWin environment ==="
KPID="$(pgrep -n -u "${HEADLESS_USER}" kwin_wayland || true)"
if [[ -n "${KPID}" ]]; then
        echo "KWin PID: ${KPID}"
        tr '\0' '\n' < "/proc/${KPID}/environ" \
                | grep -E 'KWIN_DRM_DEVICES|KWIN_DRM_NO_DIRECT_SCANOUT|KWIN_FORCE_SW_CURSOR|KWIN_USE_OVERLAYS|GBM_BACKEND|GLX' || true
else
        echo "kwin_wayland is not running for ${HEADLESS_USER}"
fi

echo
echo "=== KScreen output ==="
runuser -u "${HEADLESS_USER}" -- env \
        HOME="${HOME_DIR}" \
        XDG_RUNTIME_DIR="${RUNTIME_DIR}" \
        WAYLAND_DISPLAY="${KWIN_DISPLAY}" \
        DBUS_SESSION_BUS_ADDRESS="unix:path=${RUNTIME_DIR}/bus" \
        QT_QPA_PLATFORM=wayland \
        kscreen-doctor -o || true

echo
echo "=== KWin supportInformation ==="
qdbus_bin=""
for candidate in qdbus qdbus-qt5 /usr/lib/qt5/bin/qdbus qdbus6 /usr/lib/qt6/bin/qdbus; do
        if command -v "${candidate}" >/dev/null 2>&1; then
                qdbus_bin="$(command -v "${candidate}")"
                break
        elif [[ -x "${candidate}" ]]; then
                qdbus_bin="${candidate}"
                break
        fi
done
if [[ -n "${qdbus_bin}" ]]; then
        runuser -u "${HEADLESS_USER}" -- env \
                HOME="${HOME_DIR}" \
                XDG_RUNTIME_DIR="${RUNTIME_DIR}" \
                WAYLAND_DISPLAY="${KWIN_DISPLAY}" \
                DBUS_SESSION_BUS_ADDRESS="unix:path=${RUNTIME_DIR}/bus" \
                QT_QPA_PLATFORM=wayland \
                XDG_CURRENT_DESKTOP=KDE \
                XDG_SESSION_TYPE=wayland \
                "${qdbus_bin}" org.kde.KWin /KWin org.kde.KWin.supportInformation 2>/dev/null \
                        | awk -v connector="${FORCED_CONNECTOR:-DP-1}" '
                                /^Name:/ {
                                        if (in_block) exit
                                        in_block = ($0 ~ ("Name:[[:space:]]*" connector "$"))
                                }
                                in_block && /Name:|Geometry:|Refresh Rate:/ { print }
                        ' || true
else
        echo "qdbus not found"
fi

echo
echo "=== Launching a visible KDE app briefly ==="
if command -v systemsettings >/dev/null 2>&1; then
        runuser -u "${HEADLESS_USER}" -- env \
                HOME="${HOME_DIR}" \
                XDG_RUNTIME_DIR="${RUNTIME_DIR}" \
                WAYLAND_DISPLAY="${KWIN_DISPLAY}" \
                DBUS_SESSION_BUS_ADDRESS="unix:path=${RUNTIME_DIR}/bus" \
                QT_QPA_PLATFORM=wayland \
                systemsettings >/tmp/clouddeploy-systemsettings.log 2>&1 &
        app_pid="$!"
        sleep 4
        kill "${app_pid}" >/dev/null 2>&1 || true
        echo "systemsettings log: /tmp/clouddeploy-systemsettings.log"
else
        echo "systemsettings not found"
fi

rm -f /tmp/wayland-grim.png /tmp/wayland-spectacle.png /tmp/kmsgrab.png

echo
echo "=== Wayland screenshot via grim ==="
runuser -u "${HEADLESS_USER}" -- env \
        HOME="${HOME_DIR}" \
        XDG_RUNTIME_DIR="${RUNTIME_DIR}" \
        WAYLAND_DISPLAY="${KWIN_DISPLAY}" \
        DBUS_SESSION_BUS_ADDRESS="unix:path=${RUNTIME_DIR}/bus" \
        grim /tmp/wayland-grim.png || true

if [[ ! -s /tmp/wayland-grim.png ]] && command -v spectacle >/dev/null 2>&1; then
        echo
        echo "=== Wayland screenshot fallback via Spectacle ==="
        runuser -u "${HEADLESS_USER}" -- env \
                HOME="${HOME_DIR}" \
                XDG_RUNTIME_DIR="${RUNTIME_DIR}" \
                WAYLAND_DISPLAY="${KWIN_DISPLAY}" \
                DBUS_SESSION_BUS_ADDRESS="unix:path=${RUNTIME_DIR}/bus" \
                QT_QPA_PLATFORM=wayland \
                spectacle -b -n -o /tmp/wayland-spectacle.png || true
fi

echo
echo "=== Raw KMS screenshot via ffmpeg kmsgrab (${SUNSHINE_DRM_DEVICE}) ==="
ffmpeg -y -loglevel warning \
        -f kmsgrab \
        -device "${SUNSHINE_DRM_DEVICE}" \
        -i - \
        -frames:v 1 \
        -vf 'hwdownload,format=bgr0' \
        /tmp/kmsgrab.png </dev/null || true

echo
echo "=== Image identification ==="
if command -v identify >/dev/null 2>&1; then
        identify /tmp/wayland-grim.png 2>/dev/null || echo "/tmp/wayland-grim.png missing or unreadable"
        identify /tmp/wayland-spectacle.png 2>/dev/null || echo "/tmp/wayland-spectacle.png missing or unreadable"
        identify /tmp/kmsgrab.png 2>/dev/null || echo "/tmp/kmsgrab.png missing or unreadable"
else
        ls -lh /tmp/wayland-grim.png /tmp/wayland-spectacle.png /tmp/kmsgrab.png 2>/dev/null || true
fi

echo
echo "=== Interpretation ==="
echo "grim good + kmsgrab black = KWin/DRM plane issue"
echo "grim good + kmsgrab good + Moonlight black = Sunshine KMS backend issue"
echo "grim black = KWin/Plasma rendering issue"
EOF
        chmod 0755 /usr/local/sbin/clouddeploy-diagnose-kms

        cat > /usr/local/sbin/clouddeploy-pair-pin <<'EOF'
#!/usr/bin/env bash
set -euo pipefail

ENV_FILE="/etc/clouddeploy-wayland.env"
if [[ -f "${ENV_FILE}" ]]; then
        set -a
        # shellcheck disable=SC1090
        source "${ENV_FILE}"
        set +a
fi

SUNSHINE_USER="${SUNSHINE_USER:-user}"

read -s -r -p "Sunshine web password: " SP
echo
read -r -p "Moonlight PIN: " PIN
read -r -p "Client name [roth]: " CLIENT_NAME
CLIENT_NAME="${CLIENT_NAME:-roth}"

if command -v jq >/dev/null 2>&1; then
        payload="$(jq -cn --arg pin "${PIN}" --arg name "${CLIENT_NAME}" '{pin: $pin, name: $name}')"
else
        payload="{\"pin\":\"${PIN}\",\"name\":\"${CLIENT_NAME}\"}"
fi

curl -ks -u "${SUNSHINE_USER}:${SP}" \
        -H 'Content-Type: application/json' \
        -d "${payload}" \
        https://127.0.0.1:47990/api/pin

echo
echo "=== Sunshine clients ==="
curl -ks -u "${SUNSHINE_USER}:${SP}" https://127.0.0.1:47990/api/clients/list || true
echo
echo "Pairing helper used Sunshine's API only; it did not inject sunshine_state.json."
EOF
        chmod 0755 /usr/local/sbin/clouddeploy-pair-pin

        systemctl daemon-reload
}

KNOWN_WESTON_MODE_LINE=""
KNOWN_SUNSHINE_RESOLUTION_LINE=""
KNOWN_SUNSHINE_MONITOR_LINE=""
KNOWN_SUNSHINE_KMS_LINE=""
KNOWN_SUNSHINE_NVENC_LINE=""
KNOWN_SUNSHINE_H264_LINE=""
KNOWN_SUNSHINE_HEVC_LINE=""
KNOWN_SUNSHINE_AV1_LINE=""
KNOWN_SUNSHINE_SAMPLE_LINE=""
KNOWN_SUNSHINE_EGL_LINE=""
KNOWN_SUNSHINE_GL_LINE=""
KNOWN_SUNSHINE_FAILURE_LINE=""
KNOWN_SUNSHINE_PIXEL_FORMAT_LINE=""
KNOWN_SUNSHINE_COLOR_DEPTH_LINE=""
KNOWN_SUNSHINE_HDR_FORMAT_LINE=""
LAST_WESTON_LOG=""
LAST_SUNSHINE_LOG=""
LAST_SUNSHINE_START_SINCE=""

sunshine_journal_since() {
        local since="${1:-}"

        if [[ -n "${since}" ]]; then
                journalctl -u sunshine-headless.service --since "${since}" -n 260 --no-pager 2>/dev/null || true
        else
                journalctl -u sunshine-headless.service -n 260 --no-pager 2>/dev/null || true
        fi
}

weston_current_mode_line_from_log() {
        local weston_log="$1"
        local target_mode="${TARGET_WIDTH}x${TARGET_HEIGHT}"

        printf '%s\n' "${weston_log}" \
                | grep -Ei "${target_mode}@(119|120)([.][0-9]+)?, current|${target_mode}.*current" \
                | tail -n1 || true
}

refresh_streaming_log_markers() {
        local sunshine_since="${1:-}"

        case "${STREAM_MODE}" in
                plasma|plasma6)
                        LAST_WESTON_LOG="$(journalctl -u kwin-realvt.service -n 260 --no-pager 2>/dev/null || true)"
                        local support_info
                        support_info="$(kwin_support_information || true)"
                        if pgrep -u "${HEADLESS_USER}" -x kwin_wayland >/dev/null 2>&1 \
                                && pgrep -u "${HEADLESS_USER}" -f 'kactivitymanagerd' >/dev/null 2>&1 \
                                && pgrep -u "${HEADLESS_USER}" -x plasmashell >/dev/null 2>&1 \
                                && [[ -S "${RUNTIME_DIR}/${KWIN_DISPLAY}" ]] \
                                && kwin_support_reports_target_mode "${support_info}"; then
                                KNOWN_WESTON_MODE_LINE="$(kwin_mode_line_from_support_info "${support_info}")"
                        else
                                KNOWN_WESTON_MODE_LINE=""
                        fi
                        ;;
                kwin|realvt)
                        LAST_WESTON_LOG="$(journalctl -u kwin-realvt.service -n 260 --no-pager 2>/dev/null || true)"
                        local support_info
                        support_info="$(kwin_support_information || true)"
                        if pgrep -u "${HEADLESS_USER}" -x kwin_wayland >/dev/null 2>&1 \
                                && [[ -S "${RUNTIME_DIR}/${KWIN_DISPLAY}" ]] \
                                && kwin_support_reports_target_mode "${support_info}"; then
                                KNOWN_WESTON_MODE_LINE="$(kwin_mode_line_from_support_info "${support_info}")"
                        else
                                KNOWN_WESTON_MODE_LINE=""
                        fi
                        ;;
                weston)
                        LAST_WESTON_LOG="$(journalctl -u weston-kms-session.service -n 260 --no-pager 2>/dev/null || true)"
                        KNOWN_WESTON_MODE_LINE="$(weston_current_mode_line_from_log "${LAST_WESTON_LOG}" || true)"
                        ;;
                *)
                        LAST_WESTON_LOG=""
                        KNOWN_WESTON_MODE_LINE=""
                        ;;
        esac

        LAST_SUNSHINE_LOG="$(sunshine_journal_since "${sunshine_since}")"

        KNOWN_SUNSHINE_RESOLUTION_LINE="$(printf '%s\n' "${LAST_SUNSHINE_LOG}" \
                | grep -Ei "Desktop resolution: ${TARGET_WIDTH}x${TARGET_HEIGHT}|Resolution: ${TARGET_WIDTH}x${TARGET_HEIGHT}|Logical size: ${TARGET_WIDTH}x${TARGET_HEIGHT}|width=${TARGET_WIDTH}.*height=${TARGET_HEIGHT}|${TARGET_WIDTH}x${TARGET_HEIGHT}" \
                | tail -n1 || true)"
        KNOWN_SUNSHINE_MONITOR_LINE="$(printf '%s\n' "${LAST_SUNSHINE_LOG}" \
                | grep -Ei "Monitor 0 is ${FORCED_CONNECTOR}|Name: ${FORCED_CONNECTOR}|connector=${FORCED_CONNECTOR}|Found monitor:|${FORCED_CONNECTOR}.*primary plane" \
                | tail -n1 || true)"
        KNOWN_SUNSHINE_KMS_LINE="$(printf '%s\n' "${LAST_SUNSHINE_LOG}" \
                | grep -Ei "Found monitor for DRM screencasting|Screencasting with KMS|STREAM_DIAG.*kms plane selected|kms plane selected|drm_device=.*connector=${FORCED_CONNECTOR}" \
                | tail -n1 || true)"
        KNOWN_SUNSHINE_NVENC_LINE="$(printf '%s\n' "${LAST_SUNSHINE_LOG}" \
                | grep -Ei 'Nvenc initialized successfully|Found H[.]264 encoder: h264_nvenc|Found HEVC encoder: hevc_nvenc|Found AV1 encoder: av1_nvenc|h264_nvenc|hevc_nvenc|av1_nvenc|Creating encoder.*nvenc|encoder.*nvenc' \
                | tail -n1 || true)"
        KNOWN_SUNSHINE_H264_LINE="$(printf '%s\n' "${LAST_SUNSHINE_LOG}" \
                | grep -Ei 'Found H[.]264 encoder: h264_nvenc|h264_nvenc' \
                | tail -n1 || true)"
        KNOWN_SUNSHINE_HEVC_LINE="$(printf '%s\n' "${LAST_SUNSHINE_LOG}" \
                | grep -Ei 'Found HEVC encoder: hevc_nvenc|hevc_nvenc' \
                | tail -n1 || true)"
        KNOWN_SUNSHINE_AV1_LINE="$(printf '%s\n' "${LAST_SUNSHINE_LOG}" \
                | grep -Ei 'Found AV1 encoder: av1_nvenc|av1_nvenc' \
                | tail -n1 || true)"
        KNOWN_SUNSHINE_SAMPLE_LINE="$(printf '%s\n' "${LAST_SUNSHINE_LOG}" \
                | grep -Ei 'sample_all_black=false|all_black=false|sample_nonblack=([1-9][0-9]*)|nonblack=([1-9][0-9]*)|sample_avg_rgb=([1-9][0-9]*|[0-9]+,[1-9][0-9]*|[0-9]+,[0-9]+,[1-9][0-9]*)' \
                | tail -n1 || true)"
        KNOWN_SUNSHINE_EGL_LINE="$(printf '%s\n' "${LAST_SUNSHINE_LOG}" \
                | grep -Ei 'EGL.*NVIDIA|EGL vendor.*NVIDIA' \
                | tail -n1 || true)"
        KNOWN_SUNSHINE_GL_LINE="$(printf '%s\n' "${LAST_SUNSHINE_LOG}" \
                | grep -Ei 'GL: renderer:.*NVIDIA|GL renderer.*NVIDIA|OpenGL renderer.*NVIDIA|renderer: NVIDIA GeForce' \
                | tail -n1 || true)"
        KNOWN_SUNSHINE_PIXEL_FORMAT_LINE="$(printf '%s\n' "${LAST_SUNSHINE_LOG}" \
                | grep -Ei 'pixel_format=|pixel format|DRM_FORMAT|fourcc=|format=(XR24|AR24|AB30|XB30|P010|P012|XB4H|AR30|XR30)' \
                | tail -n1 || true)"
        KNOWN_SUNSHINE_COLOR_DEPTH_LINE="$(printf '%s\n' "${LAST_SUNSHINE_LOG}" \
                | grep -Ei 'Color depth:|10-bit|Main10|P010|P012|hevc_nvenc|av1_nvenc' \
                | tail -n1 || true)"
        KNOWN_SUNSHINE_HDR_FORMAT_LINE="$(printf '%s\n' "${LAST_SUNSHINE_LOG}" \
                | grep -Ei 'pixel_format=(AB30|XB30|P010|P012|XB4H|AR30|XR30)|format=(AB30|XB30|P010|P012|XB4H|AR30|XR30)|DRM_FORMAT_(ARGB2101010|XRGB2101010|P010|P012)' \
                | tail -n1 || true)"
        KNOWN_SUNSHINE_FAILURE_LINE="$(printf '%s\n' "${LAST_SUNSHINE_LOG}" \
                | grep -Ei 'llvmpipe|Couldn'\''t open EGL display|Couldn'\''t initialize EGL display|Encoder \[nvenc\] failed|Couldn'\''t find any working encoder|Fatal: Unable to find display or encoder|Missing file: /usr/local/assets/web/index[.]html' \
                | tail -n1 || true)"
}

streaming_log_markers_ready() {
        [[ -n "${KNOWN_WESTON_MODE_LINE}" ]] \
                && [[ -n "${KNOWN_SUNSHINE_RESOLUTION_LINE}" ]] \
                && [[ -n "${KNOWN_SUNSHINE_MONITOR_LINE}" ]] \
                && [[ -n "${KNOWN_SUNSHINE_KMS_LINE}" ]] \
                && [[ -n "${KNOWN_SUNSHINE_NVENC_LINE}" ]] \
                && [[ -n "${KNOWN_SUNSHINE_SAMPLE_LINE}" ]] \
                && [[ -z "${KNOWN_SUNSHINE_FAILURE_LINE}" ]]
}

sunshine_started_with_zero_resolution() {
        printf '%s\n' "${LAST_SUNSHINE_LOG}" | grep -Fq "Desktop resolution: 0x0"
}

print_streaming_diagnostics() {
        echo "=== CloudDeploy selected user/runtime ==="
        echo "HEADLESS_USER=${HEADLESS_USER}"
        echo "HEADLESS_UID=${HEADLESS_UID:-unknown}"
        echo "HOME_DIR=${HOME_DIR:-unknown}"
        echo "RUNTIME_DIR=${RUNTIME_DIR:-unknown}"
        echo "clouddeploy-force-kwin-mode.sh count=$(force_mode_helper_count)"
        echo
        echo "=== KWin Wayland socket ==="
        ls -lah "${RUNTIME_DIR}/${KWIN_DISPLAY}" "${RUNTIME_DIR}/${KWIN_DISPLAY}.lock" 2>/dev/null || true
        echo
        echo "=== Weston fallback socket ==="
        ls -lah /tmp/runtime-"${HEADLESS_USER}"/"${WESTON_WAYLAND_DISPLAY}" /tmp/runtime-"${HEADLESS_USER}"/"${WESTON_WAYLAND_DISPLAY}.lock" 2>/dev/null || true
        echo
        echo "=== Compositor processes ==="
        pgrep -a -u "${HEADLESS_USER}" -x weston || true
        pgrep -a -u "${HEADLESS_USER}" -x kwin_wayland || true
        pgrep -a -u "${HEADLESS_USER}" -x Xwayland || true
        pgrep -a -u "${HEADLESS_USER}" -f 'kactivitymanagerd' || true
        pgrep -a -u "${HEADLESS_USER}" -x plasmashell || true
        pgrep -a -u "${HEADLESS_USER}" -f 'ksmserver|kded5|kded6|plasma_session|startplasma-wayland' || true
        echo
        echo "=== KWin supportInformation DP marker ==="
        kwin_support_information | awk -v connector="${FORCED_CONNECTOR}" '
                /^Name:/ {
                        if (in_block) exit
                        in_block = ($0 ~ ("Name:[[:space:]]*" connector "$"))
                }
                in_block && /Name:|Geometry:|Refresh Rate:/ { print }
        ' || true
        echo
        echo "=== systemctl status plasma-realvt.service ==="
        systemctl --no-pager --full status plasma-realvt.service | sed -n '1,14p' || true
        echo
        echo "=== systemctl status kwin-realvt.service ==="
        systemctl --no-pager --full status kwin-realvt.service | sed -n '1,14p' || true
        echo
        echo "=== systemctl status plasma-shell-realvt.service ==="
        systemctl --no-pager --full status plasma-shell-realvt.service | sed -n '1,14p' || true
        echo
        echo "=== systemctl status weston-kms-session.service ==="
        systemctl --no-pager --full status weston-kms-session.service | sed -n '1,14p' || true
        echo
        echo "=== systemctl status sunshine-headless.service ==="
        systemctl --no-pager --full status sunshine-headless.service | sed -n '1,14p' || true
        echo
        echo "=== Plasma real-VT journal (last 160) ==="
        journalctl -u plasma-realvt.service -n 160 --no-pager || true
        echo
        echo "=== KWin fallback journal (last 160) ==="
        journalctl -u kwin-realvt.service -n 160 --no-pager || true
        echo
        echo "=== Weston journal (last 160) ==="
        journalctl -u weston-kms-session.service -n 160 --no-pager || true
        echo
        echo "=== Sunshine journal (last 160) ==="
        journalctl -u sunshine-headless.service -n 160 --no-pager || true
}

print_server_validation_diagnostics() {
        echo "=== NVIDIA ==="
        nvidia-smi || true
        echo
        echo "=== Connector status (${FORCED_CONNECTOR}) ==="
        cat /sys/class/drm/card*-${FORCED_CONNECTOR}/status 2>/dev/null || true
        echo
        echo "=== Connector modes (${FORCED_CONNECTOR}) ==="
        cat /sys/class/drm/card*-${FORCED_CONNECTOR}/modes 2>/dev/null || true
        echo
        print_streaming_diagnostics
}

wait_for_weston_ready() {
        local socket_path="/tmp/runtime-${HEADLESS_USER}/${WESTON_WAYLAND_DISPLAY}"
        local weston_output_enabled=""

        for _ in $(seq 1 90); do
                LAST_WESTON_LOG="$(journalctl -u weston-kms-session.service -n 260 --no-pager 2>/dev/null || true)"
                KNOWN_WESTON_MODE_LINE="$(weston_current_mode_line_from_log "${LAST_WESTON_LOG}" || true)"
                weston_output_enabled="$(printf '%s\n' "${LAST_WESTON_LOG}" \
                        | grep -F "Output '${FORCED_CONNECTOR}' enabled" \
                        | tail -n1 || true)"

                if [[ -S "${socket_path}" ]] \
                        && pgrep -u "${HEADLESS_USER}" -x weston >/dev/null 2>&1; then
                        if [[ -n "${KNOWN_WESTON_MODE_LINE}" ]] \
                                || [[ -n "${weston_output_enabled}" ]] \
                                || { connector_forced_connected && connector_has_mode "${TARGET_WIDTH}x${TARGET_HEIGHT}"; }; then
                                return 0
                        fi
                fi

                sleep 1
        done

        return 1
}

wait_for_streaming_log_markers() {
        for _ in $(seq 1 45); do
                refresh_streaming_log_markers
                if streaming_log_markers_ready; then
                        return 0
                fi

                sleep 2
        done

        return 1
}

wait_for_sunshine_post_start_markers() {
        local since="${1:-}"

        for _ in $(seq 1 45); do
                refresh_streaming_log_markers "${since}"

                if streaming_log_markers_ready; then
                        return 0
                fi

                if sunshine_started_with_zero_resolution; then
                        return 2
                fi

                sleep 2
        done

        return 1
}

known_good_clean_reset_streaming_stack() {
        log "Running clouddeploy-reset-streaming before final validation"
        write_clouddeploy_env_file
        env \
                HEADLESS_USER="${HEADLESS_USER}" \
                STREAM_MODE="${STREAM_MODE}" \
                ENABLE_PLASMA6="${ENABLE_PLASMA6}" \
                FORCED_CONNECTOR="${FORCED_CONNECTOR}" \
                TARGET_WIDTH="${TARGET_WIDTH}" \
                TARGET_HEIGHT="${TARGET_HEIGHT}" \
                TARGET_FPS="${TARGET_FPS}" \
                ENABLE_HDR="${ENABLE_HDR}" \
                KWIN_WAYLAND_DISPLAY="${KWIN_WAYLAND_DISPLAY}" \
                WESTON_WAYLAND_DISPLAY="${WESTON_WAYLAND_DISPLAY}" \
                SUNSHINE_ENCODER="${SUNSHINE_ENCODER}" \
                SUNSHINE_DRM_DEVICE="${SUNSHINE_DRM_DEVICE}" \
                SUNSHINE_HEVC_MODE="${SUNSHINE_HEVC_MODE}" \
                SUNSHINE_AV1_MODE="${SUNSHINE_AV1_MODE}" \
                /usr/local/sbin/clouddeploy-reset-streaming
}

validate_streaming_stack_ready() {
        local target_mode="${TARGET_WIDTH}x${TARGET_HEIGHT}"
        local compositor_service
        local edid_file ts_ip
        local web_code local_serverinfo_code tailscale_serverinfo_code
        local force_count

        if ! nvidia_driver_ready; then
                print_server_validation_diagnostics
                die "NVIDIA driver check failed: nvidia-smi is not healthy"
        fi
        if ! nvidia_dkms_installed_for_current_kernel; then
                print_server_validation_diagnostics
                die "NVIDIA DKMS module is not installed for running kernel $(uname -r)"
        fi
        if [[ "${INSTALL_CUDA_TOOLKIT}" == "1" ]] \
                && ! { [[ -x /usr/local/cuda/bin/nvcc ]] || command -v nvcc >/dev/null 2>&1; }; then
                print_server_validation_diagnostics
                die "CUDA toolkit was requested, but nvcc is not available"
        fi
        detect_provider_gpu_init_failure
        if nvidia_smi_driver_library_mismatch; then
                print_server_validation_diagnostics
                die "nvidia-smi reports Driver/library version mismatch"
        fi
        edid_file="${SELECTED_EDID_FILE:-$(select_phase2_edid_file || true)}"
        if [[ -n "${edid_file}" ]] && ! cmdline_has_required_phase2_args "${edid_file}"; then
                print_server_validation_diagnostics
                die "Kernel cmdline is missing required EDID/NVIDIA DRM args"
        fi
        if ! connector_forced_connected; then
                print_server_validation_diagnostics
                die "${FORCED_CONNECTOR} is not connected"
        fi
        if ! connector_has_mode "${target_mode}"; then
                print_server_validation_diagnostics
                die "${target_mode} is not exposed on ${FORCED_CONNECTOR}"
        fi
        case "${STREAM_MODE}" in
                plasma|plasma6|kwin|realvt) compositor_service="kwin-realvt.service" ;;
                weston) compositor_service="weston-kms-session.service" ;;
                *) compositor_service="$(service_for_mode)" ;;
        esac

        if ! systemctl is-active --quiet "${compositor_service}"; then
                print_server_validation_diagnostics
                die "${compositor_service} failed to start"
        fi
        if ! systemctl is-active --quiet sunshine-headless.service; then
                print_server_validation_diagnostics
                die "sunshine-headless.service failed to start"
        fi

        force_count="$(force_mode_helper_count)"
        if [[ "${force_count}" =~ ^[0-9]+$ ]] && (( force_count > 1 )); then
                print_server_validation_diagnostics
                die "More than one clouddeploy-force-kwin-mode.sh helper is running (${force_count})"
        fi

        if [[ "${STREAM_MODE}" == "plasma" || "${STREAM_MODE}" == "plasma6" || "${STREAM_MODE}" == "kwin" || "${STREAM_MODE}" == "realvt" ]]; then
                pgrep -u "${HEADLESS_USER}" -x kwin_wayland >/dev/null 2>&1 || {
                        print_server_validation_diagnostics
                        die "kwin_wayland is not running"
                }
                pgrep -u "${HEADLESS_USER}" -x Xwayland >/dev/null 2>&1 || {
                        print_server_validation_diagnostics
                        die "Xwayland is not running under KWin"
                }
                pgrep -u "${HEADLESS_USER}" -f 'kactivitymanagerd' >/dev/null 2>&1 || {
                        print_server_validation_diagnostics
                        die "kactivitymanagerd is not running"
                }
                pgrep -u "${HEADLESS_USER}" -x plasmashell >/dev/null 2>&1 || {
                        print_server_validation_diagnostics
                        die "plasmashell is not running"
                }
                if [[ "${SUNSHINE_SOURCE_MODE}" == "fork" ]]; then
                        pgrep -u "${HEADLESS_USER}" -f 'sunshine-clouddeploy' >/dev/null 2>&1 || {
                                print_server_validation_diagnostics
                                die "sunshine-clouddeploy is not running"
                        }
                fi
        fi

        if [[ "${SUNSHINE_SOURCE_MODE}" == "fork" ]]; then
                [[ -f /usr/local/assets/apps.json ]] || die "Missing Sunshine runtime asset: /usr/local/assets/apps.json"
                [[ -f /usr/local/assets/web/index.html ]] || die "Missing file: /usr/local/assets/web/index.html"
                find /usr/local/assets/web -type f \( -name '*.js' -o -name '*.css' \) 2>/dev/null | grep -q . \
                        || die "Sunshine web UI assets are missing built JS/CSS under /usr/local/assets/web"
        fi

        local_serverinfo_code="$(http_status_code "http://127.0.0.1:47989/serverinfo")"
        log "Sunshine Moonlight serverinfo local HTTP status: ${local_serverinfo_code}"
        if ! sunshine_serverinfo_status_is_reachable "${local_serverinfo_code}"; then
                print_server_validation_diagnostics
                die "Sunshine Moonlight serverinfo is not reachable at http://127.0.0.1:47989/serverinfo (HTTP ${local_serverinfo_code})"
        fi

        ts_ip="$(tailscale_ipv4 || true)"
        if [[ -n "${ts_ip}" ]]; then
                web_code="$(http_status_code "https://${ts_ip}:47990")"
                log "Sunshine web UI over Tailscale HTTP status: ${web_code}"
                if [[ "${web_code}" == "000" ]]; then
                        print_server_validation_diagnostics
                        die "Sunshine web UI connection failed at https://${ts_ip}:47990 (HTTP ${web_code})"
                elif ! sunshine_web_status_is_reachable "${web_code}"; then
                        log "WARNING: Sunshine web UI responded at https://${ts_ip}:47990 with unexpected HTTP ${web_code}; treating the web socket as reachable."
                fi

                tailscale_serverinfo_code="$(http_status_code "http://${ts_ip}:47989/serverinfo")"
                log "Sunshine Moonlight serverinfo over Tailscale HTTP status: ${tailscale_serverinfo_code}"
                if ! sunshine_serverinfo_status_is_reachable "${tailscale_serverinfo_code}"; then
                        print_server_validation_diagnostics
                        die "Sunshine Moonlight serverinfo is not reachable at http://${ts_ip}:47989/serverinfo (HTTP ${tailscale_serverinfo_code})"
                fi
        fi

        if ! wait_for_streaming_log_markers; then
                print_server_validation_diagnostics
                die "Did not observe expected compositor / Sunshine KMS+NVENC log markers"
        fi

        if [[ -n "${KNOWN_SUNSHINE_FAILURE_LINE}" ]]; then
                print_server_validation_diagnostics
                die "Sunshine failure marker observed: ${KNOWN_SUNSHINE_FAILURE_LINE}"
        fi
}

print_driver_cuda_sunshine_summary() {
        local actual_driver packaged_sunshine cuda_version runtime_kind
        local driver_family driver_source cuda_source

        actual_driver="$(current_nvidia_driver_version || true)"
        packaged_sunshine="$(command -v sunshine 2>/dev/null || true)"
        cuda_version="$(cuda_version_line || true)"
        driver_family="${NVIDIA_DRIVER_PACKAGE_FAMILY}"
        [[ "${driver_family}" != "unknown" ]] || driver_family="$(installed_nvidia_driver_package_family)"
        driver_source="${NVIDIA_DRIVER_SOURCE}"
        if [[ "${driver_source}" == "not selected" ]]; then
                if dpkg-query -W -f='${Status}' cuda-keyring 2>/dev/null | grep -q 'install ok installed'; then
                        driver_source="CUDA repo"
                else
                        driver_source="native Ubuntu"
                fi
        fi
        cuda_source="${CUDA_TOOLKIT_SOURCE}"
        if [[ "${SUNSHINE_SOURCE_MODE}" == "fork" ]]; then
                runtime_kind="clouddeploy fork binary"
        else
                runtime_kind="packaged .deb Sunshine"
        fi

        echo "Ubuntu version: $(ubuntu_os_summary)"
        echo "NVIDIA driver target: ${TARGET_NVIDIA_DRIVER_MAJOR}"
        echo "NVIDIA driver actual: ${actual_driver:-unknown}"
        echo "NVIDIA driver source: ${driver_source}"
        echo "NVIDIA driver package family: ${driver_family}"
        echo "CUDA toolkit requested: ${INSTALL_CUDA_TOOLKIT}"
        echo "CUDA toolkit install method: ${CUDA_INSTALL_METHOD}"
        echo "CUDA toolkit source: ${cuda_source}"
        echo "CUDA version: ${cuda_version:-not detected}"
        echo "Sunshine source mode: ${SUNSHINE_SOURCE_MODE}"
        echo "Sunshine runtime type: ${runtime_kind}"
        echo "Sunshine binary: ${packaged_sunshine:-not installed}"
        if [[ "${SUNSHINE_SOURCE_MODE}" == "fork" ]]; then
                echo "Sunshine fork branch: ${SUNSHINE_FORK_BRANCH}"
                echo "Sunshine fork binary: ${SUNSHINE_INSTALL_BIN}"
                echo "Sunshine runtime binary: ${SUNSHINE_INSTALL_BIN}"
        else
                echo "Sunshine fork note: fresh VMs may still need SUNSHINE_SOURCE_MODE=fork until CloudDeploy pairing/stream fixes are upstreamed."
                echo "Sunshine clean branch target: ${SUNSHINE_CLEAN_FORK_BRANCH} (used automatically once it exists when fork mode is requested)."
        fi
}

print_final_validation_summary() {
        local target_mode="${TARGET_WIDTH}x${TARGET_HEIGHT}"
        local edid_file cmdline_args ts_ip web_status local_serverinfo_status tailscale_serverinfo_status
        local web_code local_serverinfo_code tailscale_serverinfo_code force_count
        local plasma_version kwin_version plasma6_active kscreen_summary hdr_markers

        edid_file="${SELECTED_EDID_FILE:-$(select_phase2_edid_file || true)}"
        cmdline_args="$(tr ' ' '\n' </proc/cmdline 2>/dev/null \
                | grep -E "drm[.]edid_firmware=${FORCED_CONNECTOR}:edid/${edid_file}|video=${FORCED_CONNECTOR}:e|video=DP-2:d|nvidia-drm[.]modeset=1|nvidia-drm[.]fbdev=1" \
                | tr '\n' ' ' || true)"
        ts_ip="$(tailscale_ipv4 || true)"
        web_status="not checked"
        local_serverinfo_code="$(http_status_code "http://127.0.0.1:47989/serverinfo")"
        local_serverinfo_status="HTTP ${local_serverinfo_code}"
        if [[ -n "${ts_ip}" ]]; then
                web_code="$(http_status_code "https://${ts_ip}:47990")"
                if sunshine_web_status_is_reachable "${web_code}"; then
                        web_status="reachable at https://${ts_ip}:47990 (HTTP ${web_code})"
                elif [[ "${web_code}" == "000" ]]; then
                        web_status="connection failed at https://${ts_ip}:47990 (HTTP ${web_code})"
                else
                        web_status="responded with unexpected HTTP ${web_code} at https://${ts_ip}:47990"
                fi
                tailscale_serverinfo_code="$(http_status_code "http://${ts_ip}:47989/serverinfo")"
                tailscale_serverinfo_status="HTTP ${tailscale_serverinfo_code}"
        else
                tailscale_serverinfo_status="not checked"
        fi

        force_count="$(force_mode_helper_count)"
        plasma_version="$(installed_plasma_version || true)"
        kwin_version="$(installed_kwin_version || true)"
        if plasma6_mode_active; then
                plasma6_active="yes"
        else
                plasma6_active="no"
        fi
        kscreen_summary="$(kscreen_connector_summary || true)"
        hdr_markers="$(live_edid_hdr_markers || true)"
        echo "Selected HEADLESS_USER: ${HEADLESS_USER}"
        echo "Selected HEADLESS_UID: ${HEADLESS_UID:-unknown}"
        echo "Selected HOME_DIR: ${HOME_DIR:-unknown}"
        echo "Selected RUNTIME_DIR: ${RUNTIME_DIR:-unknown}"
        echo "clouddeploy-force-kwin-mode.sh process count: ${force_count}"
        echo "Plasma version: ${plasma_version:-not detected}"
        echo "KWin version: ${kwin_version:-not detected}"
        echo "Plasma 6 experimental mode active: ${plasma6_active}"
        echo "Service kwin-realvt: $(systemctl is-active kwin-realvt.service 2>/dev/null || echo unknown)"
        echo "Service plasma-shell-realvt: $(systemctl is-active plasma-shell-realvt.service 2>/dev/null || echo unknown)"
        echo "Service sunshine-headless: $(systemctl is-active sunshine-headless.service 2>/dev/null || echo unknown)"
        echo "EDID file active: ${edid_file:-unknown}"
        echo "Kernel cmdline EDID/NVIDIA args: ${cmdline_args:-missing expected args}"
        echo "KWin reported geometry/refresh: ${KNOWN_WESTON_MODE_LINE:-not observed}"
        echo "KScreen ${FORCED_CONNECTOR} mode/scale: ${kscreen_summary:-not observed}"
        echo "Live EDID HDR markers:"
        if [[ -n "${hdr_markers}" ]]; then
                printf '%s\n' "${hdr_markers}"
        else
                echo "not observed"
        fi
        echo "Process kwin_wayland: $(pgrep -a -u "${HEADLESS_USER}" -x kwin_wayland | head -n1 || echo not observed)"
        echo "Process Xwayland: $(pgrep -a -u "${HEADLESS_USER}" -x Xwayland | head -n1 || echo not observed)"
        echo "Process kactivitymanagerd: $(pgrep -a -u "${HEADLESS_USER}" -f 'kactivitymanagerd' | head -n1 || echo not observed)"
        echo "Process plasmashell: $(pgrep -a -u "${HEADLESS_USER}" -x plasmashell | head -n1 || echo not observed)"
        echo "Process sunshine-clouddeploy: $(pgrep -a -u "${HEADLESS_USER}" -f 'sunshine-clouddeploy' | head -n1 || echo not observed)"
        echo "Sunshine desktop resolution: ${KNOWN_SUNSHINE_RESOLUTION_LINE:-not observed}"
        echo "Sunshine KMS monitor found: ${KNOWN_SUNSHINE_KMS_LINE:-not observed}"
        echo "NVENC initialized: ${KNOWN_SUNSHINE_NVENC_LINE:-not observed}"
        echo "Sunshine non-black KMS sample: ${KNOWN_SUNSHINE_SAMPLE_LINE:-not observed}"
        echo "Sunshine EGL NVIDIA marker: ${KNOWN_SUNSHINE_EGL_LINE:-not observed}"
        echo "Sunshine GL NVIDIA marker: ${KNOWN_SUNSHINE_GL_LINE:-not observed}"
        echo "Sunshine KMS framebuffer pixel format: ${KNOWN_SUNSHINE_PIXEL_FORMAT_LINE:-not observed}"
        echo "Sunshine encoder/color depth: ${KNOWN_SUNSHINE_COLOR_DEPTH_LINE:-not observed}"
        if [[ -n "${KNOWN_SUNSHINE_HDR_FORMAT_LINE}" ]]; then
                echo "HDR-capable KMS framebuffer observed: ${KNOWN_SUNSHINE_HDR_FORMAT_LINE}"
        elif [[ -n "${hdr_markers}" ]] \
                && printf '%s\n' "${KNOWN_SUNSHINE_COLOR_DEPTH_LINE}" | grep -Eiq '10-bit|Main10|P010|P012' \
                && printf '%s\n' "${KNOWN_SUNSHINE_PIXEL_FORMAT_LINE}" | grep -Eiq 'pixel_format=AR24|format=AR24|AR24'; then
                echo "HDR EDID and 10-bit encoder available, but compositor framebuffer is still AR24/SDR."
        fi
        echo "Sunshine H.264 encoder: ${KNOWN_SUNSHINE_H264_LINE:-not observed}"
        echo "Sunshine HEVC encoder: ${KNOWN_SUNSHINE_HEVC_LINE:-not observed}"
        echo "Sunshine AV1 encoder: ${KNOWN_SUNSHINE_AV1_LINE:-not observed}"
        echo "Sunshine web UI over Tailscale: ${web_status}"
        echo "Sunshine Moonlight serverinfo local: ${local_serverinfo_status}"
        echo "Sunshine Moonlight serverinfo over Tailscale: ${tailscale_serverinfo_status}"
        echo "Moonlight target: ${target_mode}, ${TARGET_FPS} FPS, HDR $(if [[ "${ENABLE_HDR}" == "1" ]]; then echo on; else echo off; fi), AV1 preferred"
}

print_known_good_checklist() {
        local target_mode="${TARGET_WIDTH}x${TARGET_HEIGHT}"

        echo
        echo "CloudDeploy 4K120 SDR status:"
        echo "[OK] NVIDIA driver working"
        echo "[OK] ${FORCED_CONNECTOR} forced connected"
        echo "[OK] ${target_mode} mode exposed"
        echo "[OK] ${STREAM_MODE} compositor active"
        echo "[OK] KWin ${FORCED_CONNECTOR} ${target_mode}@120-ish (${KNOWN_WESTON_MODE_LINE})"
        echo "[OK] Sunshine active"
        echo "[OK] Sunshine Desktop resolution ${target_mode} (${KNOWN_SUNSHINE_RESOLUTION_LINE})"
        echo "[OK] Sunshine Monitor 0 is ${FORCED_CONNECTOR} (${KNOWN_SUNSHINE_MONITOR_LINE})"
        echo "[OK] Sunshine KMS capture found monitor (${KNOWN_SUNSHINE_KMS_LINE})"
        echo "[OK] NVENC initialized (${KNOWN_SUNSHINE_NVENC_LINE})"
        echo "[OK] Sunshine KMS sample non-black (${KNOWN_SUNSHINE_SAMPLE_LINE})"
        echo "[OK] Sunshine EGL/GL NVIDIA (${KNOWN_SUNSHINE_EGL_LINE}; ${KNOWN_SUNSHINE_GL_LINE})"
        echo "[OK] Sunshine NVENC encoders (${KNOWN_SUNSHINE_H264_LINE}; ${KNOWN_SUNSHINE_HEVC_LINE}; ${KNOWN_SUNSHINE_AV1_LINE})"
        echo "Codec profile: AV1-enabled SDR profile"
        echo "Moonlight: ${target_mode}, ${TARGET_FPS} FPS, HDR off, AV1 preferred"
}

install_optional_apps_nonfatal() {
        local tmpchrome
        local steam_status="skipped"
        local heroic_status="skipped"
        local lutris_status="skipped"
        local bottles_status="skipped"
        local prism_status="skipped"
        local protonup_status="skipped"
        local chrome_status="skipped"

        if [[ "${INSTALL_OPTIONAL_APPS}" != "1" ]]; then
                echo "Optional game/app installs requested: ${INSTALL_OPTIONAL_APPS}"
                echo "Steam installer: ${steam_status}"
                echo "Heroic: ${heroic_status}"
                echo "Lutris: ${lutris_status}"
                echo "Bottles: ${bottles_status}"
                echo "PrismLauncher: ${prism_status}"
                echo "ProtonUp-Qt: ${protonup_status}"
                echo "Chrome: ${chrome_status}"
                return 0
        fi

        log "Installing optional desktop apps (non-fatal)"

        if ! dpkg --print-foreign-architectures | grep -q i386; then
                wait_for_apt
                dpkg --add-architecture i386 || log "Could not add i386 architecture; continuing"
        fi

        apt_update_retry || log "Apt update failed before optional app installs; continuing"

        wait_for_apt
        DEBIAN_FRONTEND=noninteractive apt-get "${APT_DPKG_OPTIONS[@]}" install -y flatpak wine64 winetricks \
                || log "Optional non-Steam apt packages failed; continuing"
        if DEBIAN_FRONTEND=noninteractive apt-get "${APT_DPKG_OPTIONS[@]}" install -y steam-installer; then
                steam_status="installed"
        else
                steam_status="failed"
                log "Steam installer apt package failed; continuing"
        fi

        if command -v flatpak >/dev/null 2>&1; then
                flatpak remote-add --if-not-exists flathub https://flathub.org/repo/flathub.flatpakrepo \
                        || log "Flathub remote add failed; continuing"
                if flatpak install -y flathub com.heroicgameslauncher.hgl; then heroic_status="installed"; else heroic_status="failed"; log "Heroic Flatpak install failed; continuing"; fi
                if flatpak install -y flathub net.lutris.Lutris; then lutris_status="installed"; else lutris_status="failed"; log "Lutris Flatpak install failed; continuing"; fi
                if flatpak install -y flathub com.usebottles.bottles; then bottles_status="installed"; else bottles_status="failed"; log "Bottles Flatpak install failed; continuing"; fi
                if flatpak install -y flathub org.prismlauncher.PrismLauncher; then prism_status="installed"; else prism_status="failed"; log "PrismLauncher Flatpak install failed; continuing"; fi
                if flatpak install -y flathub net.davidotek.pupgui2; then protonup_status="installed"; else protonup_status="failed"; log "ProtonUp-Qt Flatpak install failed; continuing"; fi
        else
                log "Flatpak is unavailable; skipping Flatpak app installs"
        fi

        tmpchrome="/tmp/google-chrome-stable_current_amd64.deb"
        if wget -O "${tmpchrome}" https://dl.google.com/linux/direct/google-chrome-stable_current_amd64.deb; then
                wait_for_apt
                if DEBIAN_FRONTEND=noninteractive apt-get "${APT_DPKG_OPTIONS[@]}" install -y "${tmpchrome}"; then
                        chrome_status="installed"
                else
                        chrome_status="failed"
                        log "Google Chrome install failed; continuing"
                fi
                rm -f "${tmpchrome}"
        else
                chrome_status="failed"
                log "Google Chrome download failed; continuing"
        fi

        echo
        echo "Optional app install summary:"
        echo "Steam installer: ${steam_status}"
        echo "Heroic: ${heroic_status}"
        echo "Lutris: ${lutris_status}"
        echo "Bottles: ${bottles_status}"
        echo "PrismLauncher: ${prism_status}"
        echo "ProtonUp-Qt: ${protonup_status}"
        echo "Chrome: ${chrome_status}"
}

# =========================
# Start
# =========================
require_root
ensure_headless_user_context
ensure_headless_user_admin_access

cleanup_stale_continuation_state_for_manual_rerun
validate_tailscale_authkey
if [[ "${CLOUDDEPLOY_CONTINUE:-0}" == "1" ]]; then
        set_phase "continuation-dpkg-repair"
        repair_dpkg_state_if_needed
fi

if [[ -z "${SUNSHINE_PASS}" ]]; then
        log "SUNSHINE_PASS was not provided; Sunshine credentials will be left unchanged/default."
fi

if id "${HEADLESS_USER}" >/dev/null 2>&1; then
        HOME_DIR="$(user_home "${HEADLESS_USER}")"
        HEADLESS_UID="$(id -u "${HEADLESS_USER}")"
        RUNTIME_DIR="/run/user/${HEADLESS_UID}"
        KWIN_DISPLAY="${KWIN_WAYLAND_DISPLAY}"
        COMPOSITOR_SERVICE="$(service_for_mode)"
else
        HOME_DIR=""
fi

if [[ -f "$SENTINEL" ]] && [[ "$(cat "$SENTINEL")" == "$SCRIPT_VERSION" ]]; then
        log "CloudDeploy-wayland has already run on this machine for version $SCRIPT_VERSION. Restarting in the known-good order and validating..."
        [[ -n "${HOME_DIR}" ]] || die "Could not determine home directory for ${HEADLESS_USER}"
        if [[ "${SUNSHINE_DRM_DEVICE}" == "auto" ]]; then
                SUNSHINE_DRM_DEVICE="$(detect_nvidia_drm_card || true)"
        fi
        [[ -n "${SUNSHINE_DRM_DEVICE}" ]] && [[ "${SUNSHINE_DRM_DEVICE}" != "auto" ]] \
                || die "Could not detect NVIDIA DRM card node"

        COMPOSITOR_SERVICE="$(service_for_mode)"
        write_clouddeploy_env_file
        ensure_nvidia_egl_vulkan_runtime_config
        install_clouddeploy_systemd_units

        systemctl daemon-reload || true
        systemctl disable plasma-realvt.service kwin-realvt.service plasma-shell-realvt.service weston-kms-session.service sunshine-headless.service >/dev/null 2>&1 || true
        if systemctl list-unit-files | grep -q '^tailscaled'; then
                systemctl enable tailscaled || true
                systemctl restart tailscaled || true
        fi

        install_clouddeploy_helpers
        if [[ "${COMPOSITOR_SERVICE}" == "kwin-realvt.service" ]]; then
                systemctl enable kwin-realvt.service plasma-shell-realvt.service sunshine-headless.service || true
        else
                systemctl enable weston-kms-session.service sunshine-headless.service || true
        fi
        if command -v tailscale >/dev/null 2>&1; then
                if ! tailscale status >/dev/null 2>&1; then
                        if [[ -n "${TAILSCALE_AUTHKEY}" ]]; then
                                echo "Tailscale is installed but not connected. Attempting to connect..."
                                tailscale up --authkey="${TAILSCALE_AUTHKEY}" --ssh || true
                        else
                                echo "Tailscale is installed but not connected, and no auth key is set. Please set TAILSCALE_AUTHKEY and run 'tailscale up' manually."
                        fi
                fi
        fi

        known_good_clean_reset_streaming_stack

        validate_streaming_stack_ready
        systemctl disable clouddeploy-wayland-continue.service >/dev/null 2>&1 || true
        systemctl reset-failed clouddeploy-wayland-continue.service >/dev/null 2>&1 || true
        rm -f "${REBOOT_MARKER}" "${REBOOT_REASON_FILE}" || true
        rm -f "${CLOUDDEPLOY_STATE_DIR}/reboot-needed" || true
        systemctl enable --now clouddeploy-watch-streaming.timer >/dev/null 2>&1 || true

        log "Final validation markers"
        "${HOME_DIR}/.local/bin/clouddeploy-kms-status.sh" || true

        print_driver_cuda_sunshine_summary
        print_final_validation_summary

        if command -v tailscale >/dev/null 2>&1; then
                TS_IP="$(tailscale ip -4 2>/dev/null | head -n1 || true)"

                if [[ -n "${TS_IP}" ]]; then
                        echo
                        echo "Sunshine web UI: https://${TS_IP}:47990"
                        echo "Moonlight host: ${TS_IP}"
                else
                        echo
                        echo "Tailscale is installed but could not determine IP address. Check tailscale status for details."
                fi
        fi

        install_optional_apps_nonfatal

        echo "Optional game/app installs requested: ${INSTALL_OPTIONAL_APPS}"
        echo "App install helper: sudo bash -lc 'INSTALL_OPTIONAL_APPS=1 clouddeploy-run'"
        print_known_good_checklist
        exit 0
fi

echo "Running preliminary hardware diagnostics..."

CPU_SPEED_MHZ="$(lscpu | awk -F: '
/CPU max MHz|CPU MHz/ {
    gsub(/^[ \t]+/, "", $2)
    split($2, a, ".")
    print a[1]
    found=1
    exit
}
END {
    if (!found) print 0
}
')"

if [[ "$CPU_SPEED_MHZ" =~ ^[0-9]+$ ]] && [ "$CPU_SPEED_MHZ" -gt 0 ]; then
    CPU_SPEED_GHZ="$(awk "BEGIN { printf \"%.2f\", ${CPU_SPEED_MHZ}/1000 }")"
    echo "CPU Clock Speed: ${CPU_SPEED_GHZ} GHz"
    MIN_SPEED=3500

    if [ "$CPU_SPEED_MHZ" -lt "$MIN_SPEED" ]; then
        echo "CPU clock speed is below minimum threshold required"
        echo "Proceeding anyway..."
    else
        echo "CPU clock speed meets minimum threshold requirement"
        echo "Proceeding with software installation..."
    fi

else
    echo "Could not determine CPU clock speed on this VM"
    echo "Proceeding with software installation..."
fi

log "Ensuring user exists"
ensure_headless_user_context

usermod -aG sudo,video,input,render "${HEADLESS_USER}" || true
ensure_headless_user_admin_access
loginctl enable-linger "${HEADLESS_USER}" 2>/dev/null || true
chown -R "${HEADLESS_USER}:${HEADLESS_USER}" "${HOME_DIR}"

set_phase "base-packages"
repair_dpkg_state_if_needed
apt_update_retry
require_plasma6_available_from_native_repos
mapfile -t KDE_PLASMA_PACKAGES < <(kde_plasma_package_list)
apt_install_wait \
        curl wget ca-certificates gnupg software-properties-common \
        pciutils jq libcap2-bin edid-decode libdrm-tests mesa-utils-extra kmscube \
        dbus-user-session dbus-x11 \
        "${KDE_PLASMA_PACKAGES[@]}" \
        pipewire wireplumber \
        grim imagemagick ffmpeg tcpdump pulseaudio-utils \
        ubuntu-drivers-common
validate_plasma6_runtime_commands
mark_phase_done "base-packages.done"
mark_phase_done "kde-installed.done"

set_phase "nvidia-repository"
repair_dpkg_state_if_needed
log "Detecting CUDA/NVIDIA apt repository support for $(ubuntu_os_summary)"
if [[ "$(ubuntu_version_id)" == "24.04" ]]; then
        ensure_cuda_ubuntu_repo
else
        NVIDIA_DRIVER_SOURCE="native Ubuntu"
        log "Ubuntu $(ubuntu_version_id) detected; CUDA repo setup is deferred until CUDA toolkit phase so native NVIDIA driver packages are preferred."
fi

set_phase "nvidia-driver"
log "CUDA toolkit install is deferred until nvidia-smi works."
install_target_nvidia_driver
mark_phase_done "nvidia-driver.done"

set_phase "cuda-toolkit"
log "NVIDIA driver is active; proceeding to CUDA toolkit."
log "Handling CUDA toolkit install"
repair_dpkg_state_if_needed
install_cuda_toolkit_if_requested

log "Removing pieces that fought the working setup"
systemctl disable --now sddm 2>/dev/null || true
apt_purge_wait xserver-xorg-video-dummy 2>/dev/null || true
rm -f /etc/sddm.conf.d/autologin.conf
rm -f /etc/sddm.conf.d/zz-autologin.conf

set_phase "sunshine-build"
repair_dpkg_state_if_needed
log "Installing Sunshine"
install_sunshine_from_fork_if_requested
mark_phase_done "sunshine-built.done"
SUNSHINE_RUNTIME_BIN="$(sunshine_runtime_bin)"
[[ -x "${SUNSHINE_RUNTIME_BIN}" ]] || die "Sunshine runtime binary is not executable: ${SUNSHINE_RUNTIME_BIN}"
log "Using Sunshine runtime binary: ${SUNSHINE_RUNTIME_BIN}"
ensure_nvidia_egl_vulkan_runtime_config
set_phase "systemd-units"
install_clouddeploy_systemd_units

set_phase "tailscale"
log "Installing Tailscale if requested"
if [[ -n "${TAILSCALE_AUTHKEY}" ]]; then
        if ! command -v tailscale >/dev/null 2>&1; then
                wait_for_apt
                curl -fsSL https://tailscale.com/install.sh | sh
        fi

        systemctl enable --now tailscaled || true
        tailscale up --authkey="${TAILSCALE_AUTHKEY}" --ssh || log "tailscale up failed; continuing"
fi

set_phase "display-detection"
log "Detecting NVIDIA BusID"
NVIDIA_BUSID="${NVIDIA_BUSID:-$(detect_nvidia_busid || true)}"
[[ -n "${NVIDIA_BUSID}" ]] || die "Could not detect NVIDIA BusID."

log "Detecting NVIDIA DRM card node"
if [[ "${SUNSHINE_DRM_DEVICE}" == "auto" ]]; then
        SUNSHINE_DRM_DEVICE="$(detect_nvidia_drm_card || true)"
fi
[[ -n "${SUNSHINE_DRM_DEVICE}" ]] || die "Could not detect NVIDIA DRM card node"
WESTON_DRM_DEVICE="$(basename "${SUNSHINE_DRM_DEVICE}")"
[[ -n "${WESTON_DRM_DEVICE}" ]] || die "Could not derive Weston DRM card basename"
log "Using NVIDIA KMS/DRM device: ${SUNSHINE_DRM_DEVICE}"
log "Using Weston DRM device basename: ${WESTON_DRM_DEVICE}"
write_clouddeploy_env_file

set_phase "edid"
repair_dpkg_state_if_needed
log "Writing Phase 2 EDID profiles"
write_phase2_edids
update_initramfs_clouddeploy -u -k all

SELECTED_EDID_FILE="$(select_phase2_edid_file)"
[[ -n "${SELECTED_EDID_FILE}" ]] || die "Could not determine EDID profile file"
log "Selected EDID profile: ${SELECTED_EDID_FILE} on ${FORCED_CONNECTOR}"

ensure_phase2_kernel_args "${SELECTED_EDID_FILE}"
validate_phase2_display_state "${SELECTED_EDID_FILE}"
mark_phase_done "edid-installed.done"

case "${SESSION_BACKEND}" in
        kwin|plasma|plasma6|realvt|weston)
                ;;
        gamescope)
                die "SESSION_BACKEND=gamescope is reserved for the later game/HDR path."
                ;;
        *)
                die "Unsupported SESSION_BACKEND '${SESSION_BACKEND}'. Supported now: plasma, plasma6, kwin, weston."
                ;;
esac

case "${STREAM_MODE}" in
        plasma)
                log "STREAM_MODE=plasma: reliable KWin real-VT session plus Plasma shell on forced ${FORCED_CONNECTOR}"
                COMPOSITOR_SERVICE="kwin-realvt.service"
                ;;
        plasma6)
                log "STREAM_MODE=plasma6: experimental native Plasma 6/KWin 6 HDR probe on real VT${KWIN_VTNR}"
                require_plasma6_available_from_native_repos
                COMPOSITOR_SERVICE="kwin-realvt.service"
                ;;
        kwin|realvt)
                log "STREAM_MODE=${STREAM_MODE}: bare KWin Wayland DRM fallback on real VT${KWIN_VTNR}"
                COMPOSITOR_SERVICE="kwin-realvt.service"
                ;;
        weston)
                log "STREAM_MODE=weston: diagnostic Weston KMS fallback"
                COMPOSITOR_SERVICE="weston-kms-session.service"
                ;;
        gamescope)
                die "STREAM_MODE=gamescope is reserved for the later game/HDR path."
                ;;
        *)
                die "Unsupported STREAM_MODE '${STREAM_MODE}'. Supported now: plasma, plasma6, kwin, weston."
                ;;
esac

log "Writing full Plasma real-VT launchers, bare KWin fallback, and Weston fallback"
install -d -m 0755 -o "${HEADLESS_USER}" -g "${HEADLESS_USER}" \
        "${HOME_DIR}/.local/bin" \
        "${HOME_DIR}/.local/share" \
        "${HOME_DIR}/.config" \
        "${HOME_DIR}/.config/weston" \
        "${HOME_DIR}/.config/sunshine"

cat > "${HOME_DIR}/.config/startkderc" <<'EOF'
[General]
systemdBoot=false
EOF

cat > "${HOME_DIR}/.config/kscreenlockerrc" <<'EOF'
[Daemon]
Autolock=false
LockOnResume=false
EOF

chown -R "${HEADLESS_USER}:${HEADLESS_USER}" "${HOME_DIR}/.config"

cat > "${HOME_DIR}/.local/bin/start-kwin-realvt.sh" <<EOF
#!/usr/bin/env bash
set -euo pipefail

if [[ "\$(id -u)" == "0" ]]; then
        exec runuser -u "${HEADLESS_USER}" -- env \
                HOME="${HOME_DIR}" \
                USER="${HEADLESS_USER}" \
                LOGNAME="${HEADLESS_USER}" \
                XDG_RUNTIME_DIR="${RUNTIME_DIR}" \
                WAYLAND_DISPLAY="${KWIN_DISPLAY}" \
                DBUS_SESSION_BUS_ADDRESS="unix:path=${RUNTIME_DIR}/bus" \
                QT_QPA_PLATFORM=wayland \
                XDG_CURRENT_DESKTOP=KDE \
                XDG_SESSION_TYPE=wayland \
                "\$0" "\$@"
fi

export HOME="${HOME_DIR}"
export USER="${HEADLESS_USER}"
export LOGNAME="${HEADLESS_USER}"

export XDG_RUNTIME_DIR="${RUNTIME_DIR}"
export DBUS_SESSION_BUS_ADDRESS="unix:path=${RUNTIME_DIR}/bus"

export XDG_SESSION_TYPE=wayland
export XDG_SESSION_CLASS=user
export XDG_SESSION_DESKTOP=KDE
export XDG_CURRENT_DESKTOP=KDE
export DESKTOP_SESSION=plasmawayland
export KDE_FULL_SESSION=true
export XDG_VTNR="${KWIN_VTNR}"

export KWIN_DRM_DEVICES="${SUNSHINE_DRM_DEVICE}"
export KWIN_DRM_NO_DIRECT_SCANOUT=1
export KWIN_FORCE_SW_CURSOR=1
export KWIN_USE_OVERLAYS=0
export GBM_BACKEND=nvidia-drm
export QT_QPA_PLATFORM=wayland
export GDK_BACKEND=wayland,x11
export MOZ_ENABLE_WAYLAND=1
export TERM=xterm

export __EGL_VENDOR_LIBRARY_FILENAMES=/usr/share/glvnd/egl_vendor.d/10_nvidia.json
export __GLX_VENDOR_LIBRARY_NAME=nvidia

unset DISPLAY
unset WAYLAND_DISPLAY

mkdir -p "\$XDG_RUNTIME_DIR" "\$HOME/.config" "\$HOME/.local/share"
chmod 700 "\$XDG_RUNTIME_DIR"

rm -f "\$XDG_RUNTIME_DIR/${KWIN_DISPLAY}" "\$XDG_RUNTIME_DIR/${KWIN_DISPLAY}.lock"

exec /usr/bin/kwin_wayland --drm --xwayland --socket "${KWIN_DISPLAY}"
EOF

chown "${HEADLESS_USER}:${HEADLESS_USER}" "${HOME_DIR}/.local/bin/start-kwin-realvt.sh"
chmod 0755 "${HOME_DIR}/.local/bin/start-kwin-realvt.sh"

cat > "${HOME_DIR}/.local/bin/start-plasma-realvt.sh" <<EOF
#!/usr/bin/env bash
set -euo pipefail

export HOME="${HOME_DIR}"
export USER="${HEADLESS_USER}"
export LOGNAME="${HEADLESS_USER}"

export XDG_RUNTIME_DIR="${RUNTIME_DIR}"
export DBUS_SESSION_BUS_ADDRESS="unix:path=${RUNTIME_DIR}/bus"

export XDG_SESSION_TYPE=wayland
export XDG_SESSION_CLASS=user
export XDG_SESSION_DESKTOP=KDE
export XDG_CURRENT_DESKTOP=KDE
export DESKTOP_SESSION=plasmawayland
export KDE_FULL_SESSION=true
export XDG_VTNR="${KWIN_VTNR}"
export WAYLAND_DISPLAY="${KWIN_DISPLAY}"

export KWIN_DRM_DEVICES="${SUNSHINE_DRM_DEVICE}"
export KWIN_DRM_NO_DIRECT_SCANOUT=1
export KWIN_FORCE_SW_CURSOR=1
export KWIN_USE_OVERLAYS=0
export GBM_BACKEND=nvidia-drm
export QT_QPA_PLATFORM=wayland
export GDK_BACKEND=wayland,x11
export MOZ_ENABLE_WAYLAND=1
export TERM=xterm

export __EGL_VENDOR_LIBRARY_FILENAMES=/usr/share/glvnd/egl_vendor.d/10_nvidia.json
export __GLX_VENDOR_LIBRARY_NAME=nvidia

unset DISPLAY

mkdir -p "\$XDG_RUNTIME_DIR" "\$HOME/.config" "\$HOME/.local/share"
chmod 700 "\$XDG_RUNTIME_DIR"

rm -f "\$XDG_RUNTIME_DIR/${KWIN_DISPLAY}" "\$XDG_RUNTIME_DIR/${KWIN_DISPLAY}.lock"

for _ in \$(seq 1 30); do
        [[ -S "\$XDG_RUNTIME_DIR/bus" ]] && break
        sleep 1
done

if command -v dbus-update-activation-environment >/dev/null 2>&1; then
        dbus-update-activation-environment --systemd \
                DISPLAY WAYLAND_DISPLAY XDG_CURRENT_DESKTOP XDG_SESSION_TYPE XDG_SESSION_DESKTOP DESKTOP_SESSION \
                XDG_RUNTIME_DIR DBUS_SESSION_BUS_ADDRESS QT_QPA_PLATFORM KWIN_DRM_DEVICES || true
fi

PLASMA_LAUNCH_MODE="${PLASMA_LAUNCH_MODE}"
case "\${PLASMA_LAUNCH_MODE}" in
        startplasma)
                exec /usr/bin/startplasma-wayland
                ;;
        kwin-exit-with-session)
                SESSION_HELPER=""
                for candidate in \
                        /usr/lib/*/libexec/startplasma-waylandsession \
                        /usr/lib/*/libexec/startplasma-wayland \
                        /usr/libexec/startplasma-waylandsession \
                        /usr/libexec/startplasma-wayland \
                        /usr/bin/startplasma-wayland
                do
                        if [[ -x "\${candidate}" ]]; then
                                SESSION_HELPER="\${candidate}"
                                break
                        fi
                done

                [[ -n "\${SESSION_HELPER}" ]] || {
                        echo "Could not find a Plasma Wayland session helper for kwin-exit-with-session mode" >&2
                        exit 1
                }

                exec /usr/bin/kwin_wayland --drm --xwayland --socket "${KWIN_DISPLAY}" --exit-with-session "\${SESSION_HELPER}"
                ;;
        *)
                echo "Unsupported PLASMA_LAUNCH_MODE='\${PLASMA_LAUNCH_MODE}'. Supported: startplasma, kwin-exit-with-session." >&2
                exit 1
                ;;
esac
EOF

chown "${HEADLESS_USER}:${HEADLESS_USER}" "${HOME_DIR}/.local/bin/start-plasma-realvt.sh"
chmod 0755 "${HOME_DIR}/.local/bin/start-plasma-realvt.sh"

cat > /usr/local/bin/clouddeploy-force-kwin-mode.sh <<EOF
#!/usr/bin/env bash
set -euo pipefail

LOCK_FILE="/run/clouddeploy-force-kwin-mode.lock"

if [[ "\$(id -u)" == "0" ]]; then
        install -m 0666 /dev/null "\${LOCK_FILE}" 2>/dev/null || true
        if [[ "${HEADLESS_USER}" == "root" && "${ALLOW_ROOT_SESSION}" != "1" ]]; then
                echo "Refusing to run clouddeploy-force-kwin-mode as root session user" >&2
                exit 1
        fi
        exec timeout 30s runuser -u "${HEADLESS_USER}" -- env \
                HOME="${HOME_DIR}" \
                USER="${HEADLESS_USER}" \
                LOGNAME="${HEADLESS_USER}" \
                XDG_RUNTIME_DIR="${RUNTIME_DIR}" \
                WAYLAND_DISPLAY="${KWIN_DISPLAY}" \
                DBUS_SESSION_BUS_ADDRESS="unix:path=${RUNTIME_DIR}/bus" \
                QT_QPA_PLATFORM=wayland \
                XDG_CURRENT_DESKTOP=KDE \
                XDG_SESSION_TYPE=wayland \
                "\$0" "\$@"
fi

exec 9>"\${LOCK_FILE}" 2>/dev/null || exec 9>/tmp/clouddeploy-force-kwin-mode.lock
flock -n 9 || exit 0

export HOME="${HOME_DIR}"
export USER="${HEADLESS_USER}"
export LOGNAME="${HEADLESS_USER}"

export XDG_RUNTIME_DIR="${RUNTIME_DIR}"
export WAYLAND_DISPLAY="${KWIN_DISPLAY}"
export DBUS_SESSION_BUS_ADDRESS="unix:path=${RUNTIME_DIR}/bus"
export QT_QPA_PLATFORM=wayland
export XDG_CURRENT_DESKTOP=KDE
export XDG_SESSION_TYPE=wayland

find_qdbus_bin() {
        local candidate
        for candidate in qdbus qdbus-qt5 /usr/lib/qt5/bin/qdbus qdbus6 /usr/lib/qt6/bin/qdbus; do
                if command -v "\${candidate}" >/dev/null 2>&1; then
                        command -v "\${candidate}"
                        return 0
                elif [[ -x "\${candidate}" ]]; then
                        printf '%s\n' "\${candidate}"
                        return 0
                fi
        done
        return 1
}

kwin_support_info() {
        local qdbus_bin
        qdbus_bin="\$(find_qdbus_bin || true)"
        [[ -n "\${qdbus_bin}" ]] || return 1
        "\${qdbus_bin}" org.kde.KWin /KWin org.kde.KWin.supportInformation 2>/dev/null || true
}

kwin_mode_ready() {
        local info block geometry refresh
        info="\$(kwin_support_info || true)"
        block="\$(printf '%s\n' "\${info}" | awk -v connector="${FORCED_CONNECTOR}" '
                /^Name:/ {
                        if (in_block) exit
                        in_block = (\$0 ~ ("Name:[[:space:]]*" connector "\$"))
                }
                in_block { print }
        ')"
        geometry="\$(printf '%s\n' "\${block}" | grep -E 'Geometry:' | tail -n1 || true)"
        refresh="\$(printf '%s\n' "\${block}" | sed -nE 's/.*Refresh Rate:[[:space:]]*([0-9.]+).*/\\1/p' | tail -n1)"

        printf '%s\n' "\${block}" | grep -E 'Name:|Geometry:|Refresh Rate:' || true

        [[ "\${geometry}" == *"Geometry: 0,0,${TARGET_WIDTH}x${TARGET_HEIGHT}"* \
                || "\${geometry}" == *"Geometry: 0,0 ${TARGET_WIDTH}x${TARGET_HEIGHT}"* ]] || return 1
        [[ "\${refresh}" =~ ^(119|120) ]] || return 1
}

# Apply mode + scale.1 (+ optional position.0,0). Plasma 6 sometimes rejects
# the position argument on virtual/headless outputs even though everything else
# is fine; if that happens, retry without it. The scale.1 piece is the part
# that actually unblocks Sunshine preflight (KWin must report Geometry as the
# full ${TARGET_WIDTH}x${TARGET_HEIGHT}, not a scaled-down logical size).
apply_kscreen_mode() {
        local sel="\$1"
        if kscreen-doctor "\${sel}.enable" "\${sel}.mode.\${MODE_ID}" "\${sel}.scale.1" "\${sel}.position.0,0"; then
                return 0
        fi
        echo "kscreen-doctor rejected position.0,0 for \${sel}; retrying without position argument."
        kscreen-doctor "\${sel}.enable" "\${sel}.mode.\${MODE_ID}" "\${sel}.scale.1"
}

# If ENABLE_HDR=1, attempt to turn on HDR + Wide Color Gamut after the
# ${TARGET_WIDTH}x${TARGET_HEIGHT}@${TARGET_FPS} scale.1 mode is set. KWin may reject HDR on the headless
# DRM virtual output ("the driver rejected the output configuration"); in that
# case we log HDR_REJECTED_BY_DRIVER and continue as SDR.
maybe_apply_hdr() {
        [[ "${ENABLE_HDR}" == "1" ]] || return 0

        local sel
        if [[ -n "\${OUTPUT_ID:-}" ]]; then
                sel="output.\${OUTPUT_ID}"
        else
                sel="output.${FORCED_CONNECTOR}"
        fi

        echo "HDR_PROBE: ENABLE_HDR=1; attempting hdr.enable + wcg.enable + sdr-brightness.300 on \${sel}"
        local hdr_attempt
        hdr_attempt="\$(kscreen-doctor "\${sel}.hdr.enable" "\${sel}.wcg.enable" "\${sel}.sdr-brightness.300" 2>&1)" || true
        printf '%s\n' "\${hdr_attempt}"

        if printf '%s\n' "\${hdr_attempt}" | grep -qi 'the driver rejected the output configuration'; then
                echo "HDR_REJECTED_BY_DRIVER: KWin rejected HDR output config; continuing as ${TARGET_WIDTH}x${TARGET_HEIGHT}@${TARGET_FPS} SDR."
                return 0
        fi

        sleep 1
        local outinfo hdr_state wcg_state
        outinfo="\$(kscreen-doctor -o 2>&1 || true)"
        printf '%s\n' "\${outinfo}"
        hdr_state="\$(printf '%s\n' "\${outinfo}" | awk -v conn="${FORCED_CONNECTOR}" '
                /^Output:/ { in_block = (\$0 ~ conn) ? 1 : 0 }
                in_block && /HDR:/ { print; exit }
        ')"
        wcg_state="\$(printf '%s\n' "\${outinfo}" | awk -v conn="${FORCED_CONNECTOR}" '
                /^Output:/ { in_block = (\$0 ~ conn) ? 1 : 0 }
                in_block && /Wide Color Gamut:/ { print; exit }
        ')"

        if [[ "\${hdr_state}" == *"enabled"* && "\${wcg_state}" == *"enabled"* ]]; then
                echo "HDR_ENABLED: kscreen-doctor reports HDR + Wide Color Gamut enabled on ${FORCED_CONNECTOR}."
        else
                echo "HDR_NOT_CONFIRMED: HDR/WCG did not report as enabled; continuing as ${TARGET_WIDTH}x${TARGET_HEIGHT}@${TARGET_FPS} SDR (hdr=\"\${hdr_state:-unknown}\", wcg=\"\${wcg_state:-unknown}\")."
        fi
}

finalize_success() {
        maybe_apply_hdr || true
        exit 0
}

for _ in \$(seq 1 20); do
        if [[ -S "\${XDG_RUNTIME_DIR}/\${WAYLAND_DISPLAY}" ]] && pgrep -u "${HEADLESS_USER}" -x kwin_wayland >/dev/null 2>&1; then
                break
        fi

        echo "Waiting for KWin Wayland socket \${XDG_RUNTIME_DIR}/\${WAYLAND_DISPLAY}..."
        sleep 1
done

[[ -S "\${XDG_RUNTIME_DIR}/\${WAYLAND_DISPLAY}" ]] || {
        echo "KWin Wayland socket did not appear" >&2
        exit 1
}

if kwin_mode_ready; then
        echo "KWin already reports ${FORCED_CONNECTOR} at ${TARGET_WIDTH}x${TARGET_HEIGHT}@120-ish; force-mode helper succeeded."
        finalize_success
fi

OUT="\$(kscreen-doctor -o 2>&1 || true)"
echo "\${OUT}"

OUTPUT_ID="\$(printf '%s\n' "\${OUT}" | sed -nE 's/.*Output: ([0-9]+) ${FORCED_CONNECTOR}.*/\\1/p' | head -n1)"
MODE_ID="\$(printf '%s\n' "\${OUT}" | grep -oE '[0-9]+:${TARGET_WIDTH}x${TARGET_HEIGHT}@1(19|20)([.][0-9]+)?' | head -n1 | cut -d: -f1)"

[[ -n "\${OUTPUT_ID}" ]] || OUTPUT_ID="1"
[[ -n "\${MODE_ID}" ]] || MODE_ID="1"

echo "Forcing ${FORCED_CONNECTOR} to ${TARGET_WIDTH}x${TARGET_HEIGHT}@${TARGET_FPS} using output.\${OUTPUT_ID}.mode.\${MODE_ID}"

if ! apply_kscreen_mode "output.\${OUTPUT_ID}"; then
        apply_kscreen_mode "output.${FORCED_CONNECTOR}" || true
fi

sleep 2
if ! kscreen-doctor -o; then
        echo "WARNING: kscreen-doctor -o failed after mode set attempt; checking KWin supportInformation before failing."
fi
if kwin_mode_ready; then
        echo "KWin supportInformation confirms ${FORCED_CONNECTOR} ${TARGET_WIDTH}x${TARGET_HEIGHT}@120-ish; force-mode helper succeeded."
        finalize_success
fi
{
        echo "KWin did not report ${FORCED_CONNECTOR} at ${TARGET_WIDTH}x${TARGET_HEIGHT}@120-ish after force-mode." >&2
        exit 1
}
EOF

chmod 0755 /usr/local/bin/clouddeploy-force-kwin-mode.sh

cat > "${HOME_DIR}/.local/bin/start-plasmashell-realvt.sh" <<EOF
#!/usr/bin/env bash
set -euo pipefail

export HOME="${HOME_DIR}"
export USER="${HEADLESS_USER}"
export LOGNAME="${HEADLESS_USER}"

export XDG_RUNTIME_DIR="${RUNTIME_DIR}"
export WAYLAND_DISPLAY="${KWIN_DISPLAY}"
export DBUS_SESSION_BUS_ADDRESS="unix:path=${RUNTIME_DIR}/bus"

export XDG_SESSION_TYPE=wayland
export XDG_SESSION_CLASS=user
export XDG_SESSION_DESKTOP=KDE
export XDG_CURRENT_DESKTOP=KDE
export DESKTOP_SESSION=plasmawayland
export KDE_FULL_SESSION=true

export QT_QPA_PLATFORM=wayland
export GDK_BACKEND=wayland,x11
export MOZ_ENABLE_WAYLAND=1

KACTIVITYMANAGERD="/usr/lib/x86_64-linux-gnu/libexec/kactivitymanagerd"
[[ -x "\${KACTIVITYMANAGERD}" ]] || {
        echo "Missing required kactivitymanagerd binary: \${KACTIVITYMANAGERD}" >&2
        exit 1
}

for _ in \$(seq 1 90); do
        if [[ -S "\${XDG_RUNTIME_DIR}/\${WAYLAND_DISPLAY}" ]] && pgrep -u "${HEADLESS_USER}" -x kwin_wayland >/dev/null 2>&1; then
                if ! pgrep -u "${HEADLESS_USER}" -f 'kactivitymanagerd' >/dev/null 2>&1; then
                        "\${KACTIVITYMANAGERD}" &
                        for __ in \$(seq 1 20); do
                                pgrep -u "${HEADLESS_USER}" -f 'kactivitymanagerd' >/dev/null 2>&1 && break
                                sleep 0.5
                        done
                fi
                exec /usr/bin/plasmashell
        fi

        sleep 1
done

echo "KWin never became ready for plasmashell" >&2
exit 1
EOF

chown "${HEADLESS_USER}:${HEADLESS_USER}" "${HOME_DIR}/.local/bin/start-plasmashell-realvt.sh"
chmod 0755 "${HOME_DIR}/.local/bin/start-plasmashell-realvt.sh"

cat > "${HOME_DIR}/.config/weston.ini" <<EOF
[core]
backend=drm-backend.so
xwayland=true

[output]
name=${FORCED_CONNECTOR}
mode=${WESTON_MODE}
EOF

cat > "${HOME_DIR}/.local/bin/start-weston-kms.sh" <<EOF
#!/usr/bin/env bash
set -euo pipefail

export HOME="${HOME_DIR}"
export USER="${HEADLESS_USER}"
export LOGNAME="${HEADLESS_USER}"

export XDG_RUNTIME_DIR="/tmp/runtime-${HEADLESS_USER}"
export XDG_SESSION_TYPE=wayland
export WAYLAND_DISPLAY="${WESTON_WAYLAND_DISPLAY}"

export __EGL_VENDOR_LIBRARY_FILENAMES=/usr/share/glvnd/egl_vendor.d/10_nvidia.json
export __GLX_VENDOR_LIBRARY_NAME=nvidia

mkdir -p "\$XDG_RUNTIME_DIR" "${HOME_DIR}/.local/share" "${HOME_DIR}/.config/weston"
chmod 700 "\$XDG_RUNTIME_DIR"

exec /usr/bin/weston \
        --backend=drm-backend.so \
        --drm-device="${WESTON_DRM_DEVICE}" \
        --socket="${WESTON_WAYLAND_DISPLAY}" \
        --config="${HOME_DIR}/.config/weston.ini"
EOF

chown "${HEADLESS_USER}:${HEADLESS_USER}" "${HOME_DIR}/.config/weston.ini"
chown "${HEADLESS_USER}:${HEADLESS_USER}" "${HOME_DIR}/.local/bin/start-weston-kms.sh"
chmod 0755 "${HOME_DIR}/.local/bin/start-weston-kms.sh"

cat > /usr/local/bin/clouddeploy-wait-sunshine-session.sh <<EOF
#!/usr/bin/env bash
set -euo pipefail

export HOME="${HOME_DIR}"
export USER="${HEADLESS_USER}"
export LOGNAME="${HEADLESS_USER}"

find_qdbus_bin() {
        local candidate
        for candidate in qdbus qdbus-qt5 /usr/lib/qt5/bin/qdbus qdbus6 /usr/lib/qt6/bin/qdbus; do
                if command -v "\${candidate}" >/dev/null 2>&1; then
                        command -v "\${candidate}"
                        return 0
                elif [[ -x "\${candidate}" ]]; then
                        printf '%s\n' "\${candidate}"
                        return 0
                fi
        done
        return 1
}

kwin_mode_ready() {
        local qdbus_bin info block geometry refresh
        qdbus_bin="\$(find_qdbus_bin || true)"
        [[ -n "\${qdbus_bin}" ]] || return 1
        info="\$("\${qdbus_bin}" org.kde.KWin /KWin org.kde.KWin.supportInformation 2>/dev/null || true)"
        block="\$(printf '%s\n' "\${info}" | awk -v connector="${FORCED_CONNECTOR}" '
                /^Name:/ {
                        if (in_block) exit
                        in_block = (\$0 ~ ("Name:[[:space:]]*" connector "\$"))
                }
                in_block { print }
        ')"
        geometry="\$(printf '%s\n' "\${block}" | grep -E 'Geometry:' | tail -n1 || true)"
        refresh="\$(printf '%s\n' "\${block}" | sed -nE 's/.*Refresh Rate:[[:space:]]*([0-9.]+).*/\\1/p' | tail -n1)"
        [[ "\${geometry}" == *"Geometry: 0,0,${TARGET_WIDTH}x${TARGET_HEIGHT}"* \
                || "\${geometry}" == *"Geometry: 0,0 ${TARGET_WIDTH}x${TARGET_HEIGHT}"* ]] || return 1
        [[ "\${refresh}" =~ ^(119|120) ]] || return 1
}

verify_sunshine_kms_config() {
        local conf="${HOME_DIR}/.config/sunshine/sunshine.conf"
        grep -Eq '^capture[[:space:]]*=[[:space:]]*kms[[:space:]]*$' "\${conf}" || {
                echo "Sunshine config is not using capture = kms" >&2
                exit 1
        }
        grep -Eq '^adapter_name[[:space:]]*=[[:space:]]*/dev/dri/card[0-9]+[[:space:]]*$' "\${conf}" || {
                echo "Sunshine config is not using a DRM card node adapter_name" >&2
                exit 1
        }
}

case "${STREAM_MODE}" in
        plasma|plasma6|kwin|realvt)
                export XDG_RUNTIME_DIR="${RUNTIME_DIR}"
                export WAYLAND_DISPLAY="${KWIN_DISPLAY}"
                export DBUS_SESSION_BUS_ADDRESS="unix:path=${RUNTIME_DIR}/bus"
                export QT_QPA_PLATFORM=wayland
                export XDG_SESSION_TYPE=wayland
                export XDG_CURRENT_DESKTOP=KDE
                WAIT_PROCESS="kwin_wayland"
                ;;
        weston)
                export XDG_RUNTIME_DIR="/tmp/runtime-${HEADLESS_USER}"
                export WAYLAND_DISPLAY="${WESTON_WAYLAND_DISPLAY}"
                WAIT_PROCESS="weston"
                ;;
        *)
                echo "Unsupported STREAM_MODE for Sunshine wait: ${STREAM_MODE}" >&2
                exit 1
                ;;
esac

for _ in \$(seq 1 120); do
        if [[ -S "\${XDG_RUNTIME_DIR}/\${WAYLAND_DISPLAY}" ]] && pgrep -u "${HEADLESS_USER}" -x "\${WAIT_PROCESS}" >/dev/null 2>&1; then
                break
        fi
        echo "Waiting for \${WAIT_PROCESS} Wayland socket \${XDG_RUNTIME_DIR}/\${WAYLAND_DISPLAY}..."
        sleep 1
done

[[ -S "\${XDG_RUNTIME_DIR}/\${WAYLAND_DISPLAY}" ]] || {
        echo "Wayland socket never became ready for Sunshine" >&2
        exit 1
}

if [[ "${STREAM_MODE}" == "plasma" || "${STREAM_MODE}" == "plasma6" || "${STREAM_MODE}" == "kwin" || "${STREAM_MODE}" == "realvt" ]]; then
        /usr/local/bin/clouddeploy-force-kwin-mode.sh || true

        for _ in \$(seq 1 60); do
                kwin_mode_ready && break
                echo "Waiting for KWin to report ${FORCED_CONNECTOR} at ${TARGET_WIDTH}x${TARGET_HEIGHT}@120-ish..."
                sleep 1
        done
        kwin_mode_ready || {
                echo "KWin did not report target 4K120 mode before Sunshine." >&2
                exit 1
        }

        for _ in \$(seq 1 45); do
                pgrep -u "${HEADLESS_USER}" -f 'kactivitymanagerd' >/dev/null 2>&1 \
                        && pgrep -u "${HEADLESS_USER}" -x plasmashell >/dev/null 2>&1 \
                        && break
                echo "Waiting briefly for kactivitymanagerd and plasmashell..."
                sleep 1
        done

        pgrep -u "${HEADLESS_USER}" -f 'kactivitymanagerd' >/dev/null 2>&1 || echo "kactivitymanagerd not observed; continuing after bounded wait."
        pgrep -u "${HEADLESS_USER}" -x plasmashell >/dev/null 2>&1 || echo "plasmashell not observed; continuing after bounded wait."
fi

verify_sunshine_kms_config
echo "Wayland/KMS session is ready for Sunshine."
EOF

chmod 0755 /usr/local/bin/clouddeploy-wait-sunshine-session.sh

cat > "${HOME_DIR}/.local/bin/start-sunshine-headless.sh" <<EOF
#!/usr/bin/env bash
set -euo pipefail

export HOME="${HOME_DIR}"
export USER="${HEADLESS_USER}"
export LOGNAME="${HEADLESS_USER}"
SUNSHINE_BIN="${SUNSHINE_RUNTIME_BIN}"

if [[ ! -x "\${SUNSHINE_BIN}" ]]; then
        echo "Sunshine binary is not executable: \${SUNSHINE_BIN}" >&2
        exit 1
fi

find_qdbus_bin() {
        local candidate
        for candidate in qdbus qdbus-qt5 /usr/lib/qt5/bin/qdbus qdbus6 /usr/lib/qt6/bin/qdbus; do
                if command -v "\${candidate}" >/dev/null 2>&1; then
                        command -v "\${candidate}"
                        return 0
                elif [[ -x "\${candidate}" ]]; then
                        printf '%s\n' "\${candidate}"
                        return 0
                fi
        done
        return 1
}

kwin_mode_ready() {
        local qdbus_bin info block geometry refresh
        qdbus_bin="\$(find_qdbus_bin || true)"
        [[ -n "\${qdbus_bin}" ]] || return 1
        info="\$("\${qdbus_bin}" org.kde.KWin /KWin org.kde.KWin.supportInformation 2>/dev/null || true)"
        block="\$(printf '%s\n' "\${info}" | awk -v connector="${FORCED_CONNECTOR}" '
                /^Name:/ {
                        if (in_block) exit
                        in_block = (\$0 ~ ("Name:[[:space:]]*" connector "\$"))
                }
                in_block { print }
        ')"
        geometry="\$(printf '%s\n' "\${block}" | grep -E 'Geometry:' | tail -n1 || true)"
        refresh="\$(printf '%s\n' "\${block}" | sed -nE 's/.*Refresh Rate:[[:space:]]*([0-9.]+).*/\\1/p' | tail -n1)"
        [[ "\${geometry}" == *"Geometry: 0,0,${TARGET_WIDTH}x${TARGET_HEIGHT}"* \
                || "\${geometry}" == *"Geometry: 0,0 ${TARGET_WIDTH}x${TARGET_HEIGHT}"* ]] || return 1
        [[ "\${refresh}" =~ ^(119|120) ]] || return 1
}

verify_sunshine_kms_config() {
        local conf="${HOME_DIR}/.config/sunshine/sunshine.conf"
        grep -Eq '^capture[[:space:]]*=[[:space:]]*kms[[:space:]]*$' "\${conf}" || {
                echo "Sunshine config is not using capture = kms" >&2
                exit 1
        }
        grep -Eq '^adapter_name[[:space:]]*=[[:space:]]*/dev/dri/card[0-9]+[[:space:]]*$' "\${conf}" || {
                echo "Sunshine config is not using a DRM card node adapter_name" >&2
                exit 1
        }
}

start_kde_shell_bits() {
        [[ "${STREAM_MODE}" == "plasma" || "${STREAM_MODE}" == "plasma6" ]] || return 0

        local kactivitymanagerd="/usr/lib/x86_64-linux-gnu/libexec/kactivitymanagerd"
        if ! pgrep -u "${HEADLESS_USER}" -f 'kactivitymanagerd' >/dev/null 2>&1; then
                if [[ -x "\${kactivitymanagerd}" ]]; then
                        echo "Starting kactivitymanagerd for Plasma shell readiness..."
                        "\${kactivitymanagerd}" &
                else
                        echo "WARNING: missing required kactivitymanagerd binary: \${kactivitymanagerd}"
                fi
        fi

        if [[ -x /usr/bin/plasmashell ]] && ! pgrep -u "${HEADLESS_USER}" -x plasmashell >/dev/null 2>&1; then
                echo "Starting plasmashell for Plasma desktop layer..."
                /usr/bin/plasmashell &
        fi

        for _ in \$(seq 1 15); do
                pgrep -u "${HEADLESS_USER}" -f 'kactivitymanagerd' >/dev/null 2>&1 \
                        && pgrep -u "${HEADLESS_USER}" -x plasmashell >/dev/null 2>&1 \
                        && return 0
                sleep 1
        done

        pgrep -u "${HEADLESS_USER}" -f 'kactivitymanagerd' >/dev/null 2>&1 \
                || echo "WARNING: kactivitymanagerd was not observed before Sunshine startup."
        pgrep -u "${HEADLESS_USER}" -x plasmashell >/dev/null 2>&1 \
                || echo "WARNING: plasmashell was not observed before Sunshine startup; continuing so KMS/NVENC validation can decide."
}

case "${STREAM_MODE}" in
        plasma|plasma6)
                export XDG_RUNTIME_DIR="${RUNTIME_DIR}"
                export WAYLAND_DISPLAY="${KWIN_DISPLAY}"
                export DBUS_SESSION_BUS_ADDRESS="unix:path=${RUNTIME_DIR}/bus"
                export QT_QPA_PLATFORM=wayland
                export XDG_SESSION_TYPE=wayland
                export XDG_CURRENT_DESKTOP=KDE
                export KDE_FULL_SESSION=true
                export DESKTOP_SESSION=plasmawayland
                WAIT_PROCESS="kwin_wayland"
                ;;
        kwin|realvt)
                export XDG_RUNTIME_DIR="${RUNTIME_DIR}"
                export WAYLAND_DISPLAY="${KWIN_DISPLAY}"
                export DBUS_SESSION_BUS_ADDRESS="unix:path=${RUNTIME_DIR}/bus"
                WAIT_PROCESS="kwin_wayland"
                ;;
        weston)
                export XDG_RUNTIME_DIR="/tmp/runtime-${HEADLESS_USER}"
                export WAYLAND_DISPLAY="${WESTON_WAYLAND_DISPLAY}"
                WAIT_PROCESS="weston"
                ;;
        *)
                echo "Unsupported STREAM_MODE for Sunshine: ${STREAM_MODE}" >&2
                exit 1
                ;;
esac

mkdir -p "\$XDG_RUNTIME_DIR"
chmod 700 "\$XDG_RUNTIME_DIR"

FORCE_ATTEMPTED=0
SESSION_MARKER_LOGGED=0
for _ in \$(seq 1 120); do
        if [[ -S "\${XDG_RUNTIME_DIR}/\${WAYLAND_DISPLAY}" ]] && pgrep -u "${HEADLESS_USER}" -x "\${WAIT_PROCESS}" >/dev/null 2>&1; then
                if [[ "${STREAM_MODE}" == "plasma" || "${STREAM_MODE}" == "plasma6" ]] \
                        && ! pgrep -u "${HEADLESS_USER}" -f 'ksmserver|kded5|kded6|plasma_session' >/dev/null 2>&1; then
                        if [[ "\${SESSION_MARKER_LOGGED}" == "0" ]]; then
                                echo "KDE session service marker not observed yet; continuing because KWin DBus/mode validation is authoritative."
                                SESSION_MARKER_LOGGED=1
                        fi
                fi

                if [[ "${STREAM_MODE}" == "kwin" || "${STREAM_MODE}" == "plasma" || "${STREAM_MODE}" == "plasma6" || "${STREAM_MODE}" == "realvt" ]]; then
                        if [[ "\${FORCE_ATTEMPTED}" == "0" ]]; then
                                /usr/local/bin/clouddeploy-force-kwin-mode.sh || echo "WARNING: clouddeploy-force-kwin-mode failed; waiting for KWin mode validation."
                                FORCE_ATTEMPTED=1
                        fi
                        if ! kwin_mode_ready; then
                                echo "Waiting for KWin to report ${FORCED_CONNECTOR} at ${TARGET_WIDTH}x${TARGET_HEIGHT}@120-ish..."
                                sleep 1
                                continue
                        fi
                fi

                start_kde_shell_bits
                verify_sunshine_kms_config
                echo "\${WAIT_PROCESS} Wayland session is ready at \${XDG_RUNTIME_DIR}/\${WAYLAND_DISPLAY}; starting Sunshine"
                exec "\${SUNSHINE_BIN}"
        fi

        echo "Waiting for \${WAIT_PROCESS} Wayland socket \${XDG_RUNTIME_DIR}/\${WAYLAND_DISPLAY}..."
        sleep 1
done

echo "Wayland session never became ready for Sunshine" >&2
exit 1
EOF

chown "${HEADLESS_USER}:${HEADLESS_USER}" "${HOME_DIR}/.local/bin/start-sunshine-headless.sh"
chmod 0755 "${HOME_DIR}/.local/bin/start-sunshine-headless.sh"

cat > "${HOME_DIR}/.local/bin/clouddeploy-kms-status.sh" <<EOF
#!/usr/bin/env bash
set -euo pipefail

echo "=== /proc/cmdline ==="
cat /proc/cmdline || true

echo
echo "=== nvidia_drm modeset ==="
cat /sys/module/nvidia_drm/parameters/modeset 2>/dev/null || true

echo
echo "=== nvidia-smi ==="
nvidia-smi || true

echo
echo "=== Plasma/KWin versions ==="
if command -v plasmashell >/dev/null 2>&1; then
        plasmashell --version || true
else
        dpkg-query -W -f='plasma-workspace \${Version}\n' plasma-workspace 2>/dev/null || true
fi
if command -v kwin_wayland >/dev/null 2>&1; then
        kwin_wayland --version || true
else
        dpkg-query -W -f='kwin-wayland \${Version}\n' kwin-wayland 2>/dev/null || true
fi
echo "Plasma 6 experimental mode active: $(if [[ "${STREAM_MODE}" == "plasma6" || "${ENABLE_PLASMA6}" == "1" ]]; then echo yes; else echo no; fi)"

echo
echo "=== /dev/dri ==="
ls -l /dev/dri || true

echo
echo "=== Sunshine binaries and capabilities ==="
if command -v sunshine >/dev/null 2>&1; then
        sunshine_bin="\$(readlink -f "\$(command -v sunshine)")"
        echo "packaged sunshine: \${sunshine_bin}"
        getcap "\${sunshine_bin}" || true
else
        echo "sunshine not found"
fi
clouddeploy_sunshine_bin="${SUNSHINE_INSTALL_BIN}"
if [[ -e "\${clouddeploy_sunshine_bin}" ]]; then
        clouddeploy_sunshine_bin="\$(readlink -f "\${clouddeploy_sunshine_bin}")"
        echo "clouddeploy sunshine: \${clouddeploy_sunshine_bin}"
        getcap "\${clouddeploy_sunshine_bin}" || true
fi

echo
echo "=== NVIDIA EGL/Vulkan runtime files ==="
ls -l /usr/share/egl/egl_external_platform.d/10_nvidia_wayland.json \
      /usr/share/egl/egl_external_platform.d/15_nvidia_gbm.json \
      /usr/share/glvnd/egl_vendor.d/10_nvidia.json \
      /usr/share/vulkan/icd.d/*nvidia*_icd.json 2>/dev/null || true
echo
echo "=== Sunshine NVIDIA EGL/Vulkan drop-in ==="
cat /etc/systemd/system/sunshine-headless.service.d/20-nvidia-vulkan-egl.conf 2>/dev/null || true

echo
echo "=== Connector status (${FORCED_CONNECTOR}) ==="
cat /sys/class/drm/card*-${FORCED_CONNECTOR}/status 2>/dev/null || true

echo
echo "=== Connector modes (${FORCED_CONNECTOR}) ==="
cat /sys/class/drm/card*-${FORCED_CONNECTOR}/modes 2>/dev/null || true

echo
echo "=== Live EDID HDR markers (${FORCED_CONNECTOR}) ==="
if command -v edid-decode >/dev/null 2>&1; then
        for edid_path in /sys/class/drm/card*-${FORCED_CONNECTOR}/edid; do
                [[ -s "\${edid_path}" ]] || continue
                edid-decode "\${edid_path}" 2>/dev/null \
                        | grep -Ei 'HDR|EOTF|PQ|HLG|BT[.]2020|Static Metadata|SMPTE ST 2084' || true
        done
else
        echo "edid-decode not found"
fi

echo
echo "=== KScreen output ==="
if [[ -S "${RUNTIME_DIR}/${KWIN_DISPLAY}" ]]; then
        runuser -u "${HEADLESS_USER}" -- env HOME="${HOME_DIR}" \
            XDG_RUNTIME_DIR="${RUNTIME_DIR}" \
            WAYLAND_DISPLAY="${KWIN_DISPLAY}" \
            DBUS_SESSION_BUS_ADDRESS="unix:path=${RUNTIME_DIR}/bus" \
            QT_QPA_PLATFORM=wayland \
            kscreen-doctor -o || true
fi

echo
echo "=== KWin supportInformation (${FORCED_CONNECTOR}) ==="
qdbus_bin=""
for candidate in qdbus qdbus-qt5 /usr/lib/qt5/bin/qdbus qdbus6 /usr/lib/qt6/bin/qdbus; do
        if command -v "\${candidate}" >/dev/null 2>&1; then
                qdbus_bin="\$(command -v "\${candidate}")"
                break
        elif [[ -x "\${candidate}" ]]; then
                qdbus_bin="\${candidate}"
                break
        fi
done
if [[ -n "\${qdbus_bin}" ]]; then
        runuser -u "${HEADLESS_USER}" -- env HOME="${HOME_DIR}" \
            XDG_RUNTIME_DIR="${RUNTIME_DIR}" \
            WAYLAND_DISPLAY="${KWIN_DISPLAY}" \
            DBUS_SESSION_BUS_ADDRESS="unix:path=${RUNTIME_DIR}/bus" \
            QT_QPA_PLATFORM=wayland \
            XDG_CURRENT_DESKTOP=KDE \
            XDG_SESSION_TYPE=wayland \
            "\${qdbus_bin}" org.kde.KWin /KWin org.kde.KWin.supportInformation 2>/dev/null \
                | awk -v connector="${FORCED_CONNECTOR}" '
                        /^Name:/ {
                                if (in_block) exit
                                in_block = (\$0 ~ ("Name:[[:space:]]*" connector "\$"))
                        }
                        in_block && /Name:|Geometry:|Refresh Rate:/ { print }
                ' || true
else
        echo "qdbus not found"
fi

echo
echo "=== Service status ==="
systemctl --no-pager --full status \
        plasma-realvt.service \
        kwin-realvt.service \
        plasma-shell-realvt.service \
        weston-kms-session.service \
        sunshine-headless.service \
        tailscaled || true

echo
echo "=== Processes ==="
pgrep -a -u "${HEADLESS_USER}" -f 'kwin_wayland|Xwayland|kactivitymanagerd|plasmashell|startplasma-wayland|ksmserver|kded5|kded6|plasma_session|weston|sunshine|sunshine-clouddeploy' || true

echo
echo "=== KWin environment ==="
KPID="\$(pgrep -n -u "${HEADLESS_USER}" kwin_wayland || true)"
if [[ -n "\${KPID}" ]]; then
        tr '\0' '\n' < "/proc/\${KPID}/environ" \
                | grep -E 'KWIN_DRM_DEVICES|KWIN_DRM_NO_DIRECT_SCANOUT|KWIN_FORCE_SW_CURSOR|KWIN_USE_OVERLAYS|GBM_BACKEND|GLX' || true
else
        echo "kwin_wayland is not running"
fi

echo
echo "=== Sunshine journal markers ==="
journalctl -u sunshine-headless.service -n 220 --no-pager \
        | grep -Ei 'Desktop resolution|Resolution:|Logical size|Name: ${FORCED_CONNECTOR}|Monitor 0|Screencasting with KMS|Found monitor|pixel_format|format=(XR24|AR24|AB30|XB30|P010|P012|XB4H|AR30|XR30)|Color depth|10-bit|Nvenc initialized|Found H[.]264|Found HEVC|Found AV1|sample_all_black|EGL|GL: renderer|llvmpipe|Mismatch|pair|pin|error|fatal' || true
EOF

chown "${HEADLESS_USER}:${HEADLESS_USER}" "${HOME_DIR}/.local/bin/clouddeploy-kms-status.sh"
chmod 0755 "${HOME_DIR}/.local/bin/clouddeploy-kms-status.sh"

log "Writing Sunshine config"
write_sunshine_config
set_phase "systemd-units"
install_clouddeploy_systemd_units

log "Removing stale Sunshine state to avoid broken pre-pairing"
if [[ -f "${HOME_DIR}/.config/sunshine/sunshine_state.json" ]]; then
    mv "${HOME_DIR}/.config/sunshine/sunshine_state.json" \
       "${HOME_DIR}/.config/sunshine/sunshine_state.json.bak.$(date +%s)"
fi

log "Ensuring Sunshine has cap_sys_admin for KMS capture"
if command -v setcap >/dev/null 2>&1 && [[ -x "${SUNSHINE_RUNTIME_BIN}" ]]; then
        setcap cap_sys_admin,cap_sys_nice+ep "$(readlink -f "${SUNSHINE_RUNTIME_BIN}")" || true
fi

if [[ -n "${SUNSHINE_PASS}" ]]; then
        log "Setting Sunshine web UI credentials"
        run_as_user "${HEADLESS_USER}" env HOME="${HOME_DIR}" "${SUNSHINE_RUNTIME_BIN}" --creds "${SUNSHINE_USER}" "${SUNSHINE_PASS}" || true
else
        log "SUNSHINE_PASS was not provided; leaving Sunshine credentials unchanged/default."
fi

log "Writing full Plasma real-VT systemd service"
cat > /etc/systemd/system/plasma-realvt.service <<EOF
[Unit]
Description=Full KDE Plasma Wayland session on real VT${KWIN_VTNR}
After=systemd-logind.service systemd-user-sessions.service network-online.target
Wants=network-online.target
Conflicts=display-manager.service getty@tty${KWIN_VTNR}.service kwin-realvt.service weston-kms-session.service

[Service]
Type=simple
User=${HEADLESS_USER}
Group=${HEADLESS_USER}
SupplementaryGroups=video render input
PAMName=login
WorkingDirectory=${HOME_DIR}

TTYPath=/dev/tty${KWIN_VTNR}
StandardInput=tty
StandardOutput=journal
StandardError=journal
TTYReset=yes
TTYVHangup=no
TTYVTDisallocate=no
UtmpIdentifier=tty${KWIN_VTNR}
UtmpMode=user
TimeoutStartSec=45

Environment=HOME=${HOME_DIR}
Environment=USER=${HEADLESS_USER}
Environment=LOGNAME=${HEADLESS_USER}
Environment=XDG_RUNTIME_DIR=${RUNTIME_DIR}
Environment=WAYLAND_DISPLAY=${KWIN_DISPLAY}
Environment=DBUS_SESSION_BUS_ADDRESS=unix:path=${RUNTIME_DIR}/bus
Environment=XDG_SESSION_TYPE=wayland
Environment=XDG_SESSION_CLASS=user
Environment=XDG_SESSION_DESKTOP=KDE
Environment=XDG_CURRENT_DESKTOP=KDE
Environment=DESKTOP_SESSION=plasmawayland
Environment=KDE_FULL_SESSION=true
Environment=QT_QPA_PLATFORM=wayland
Environment=GDK_BACKEND=wayland,x11
Environment=MOZ_ENABLE_WAYLAND=1
Environment=KWIN_DRM_DEVICES=${SUNSHINE_DRM_DEVICE}
Environment=KWIN_DRM_NO_DIRECT_SCANOUT=1
Environment=KWIN_FORCE_SW_CURSOR=1
Environment=KWIN_USE_OVERLAYS=0
Environment=GBM_BACKEND=nvidia-drm
Environment=__EGL_VENDOR_LIBRARY_FILENAMES=/usr/share/glvnd/egl_vendor.d/10_nvidia.json
Environment=__GLX_VENDOR_LIBRARY_NAME=nvidia

PermissionsStartOnly=true
ExecStartPre=-/usr/bin/systemctl stop getty@tty${KWIN_VTNR}.service
ExecStartPre=-/usr/bin/systemctl start user@${HEADLESS_UID}.service
ExecStartPre=/usr/bin/mkdir -p ${RUNTIME_DIR}
ExecStartPre=/usr/bin/chown ${HEADLESS_USER}:${HEADLESS_USER} ${RUNTIME_DIR}
ExecStartPre=/usr/bin/chmod 700 ${RUNTIME_DIR}
ExecStartPre=/usr/bin/chvt ${KWIN_VTNR}
ExecStartPre=/usr/bin/bash -lc 'for i in \$(seq 1 30); do nvidia-smi >/dev/null 2>&1 && exit 0; sleep 2; done; exit 1'

ExecStart=${HOME_DIR}/.local/bin/start-plasma-realvt.sh
ExecStartPost=-/usr/local/bin/clouddeploy-force-kwin-mode.sh

Restart=on-failure
RestartSec=5

[Install]
WantedBy=multi-user.target
EOF

log "Writing KWin real-VT systemd service"
cat > /etc/systemd/system/kwin-realvt.service <<EOF
[Unit]
Description=KWin Wayland DRM session on real VT${KWIN_VTNR}
After=systemd-logind.service systemd-user-sessions.service network-online.target
Wants=network-online.target
Conflicts=display-manager.service getty@tty${KWIN_VTNR}.service plasma-realvt.service weston-kms-session.service

[Service]
Type=simple
User=${HEADLESS_USER}
Group=${HEADLESS_USER}
SupplementaryGroups=video render input
PAMName=login
WorkingDirectory=${HOME_DIR}

TTYPath=/dev/tty${KWIN_VTNR}
StandardInput=tty
StandardOutput=journal
StandardError=journal
TTYReset=yes
TTYVHangup=no
TTYVTDisallocate=no
UtmpIdentifier=tty${KWIN_VTNR}
UtmpMode=user
TimeoutStartSec=45

Environment=KWIN_DRM_DEVICES=${SUNSHINE_DRM_DEVICE}
Environment=KWIN_DRM_NO_DIRECT_SCANOUT=1
Environment=KWIN_FORCE_SW_CURSOR=1
Environment=KWIN_USE_OVERLAYS=0
Environment=GBM_BACKEND=nvidia-drm
Environment=__GLX_VENDOR_LIBRARY_NAME=nvidia

PermissionsStartOnly=true
ExecStartPre=-/usr/bin/systemctl stop getty@tty${KWIN_VTNR}.service
ExecStartPre=-/usr/bin/systemctl start user@${HEADLESS_UID}.service
ExecStartPre=/usr/bin/mkdir -p ${RUNTIME_DIR}
ExecStartPre=/usr/bin/chown ${HEADLESS_USER}:${HEADLESS_USER} ${RUNTIME_DIR}
ExecStartPre=/usr/bin/chmod 700 ${RUNTIME_DIR}
ExecStartPre=/usr/bin/chvt ${KWIN_VTNR}
ExecStartPre=/usr/bin/bash -lc 'for i in \$(seq 1 30); do nvidia-smi >/dev/null 2>&1 && exit 0; sleep 2; done; exit 1'

ExecStart=${HOME_DIR}/.local/bin/start-kwin-realvt.sh
ExecStartPost=

Restart=no

[Install]
WantedBy=multi-user.target
EOF

log "Writing Plasma shell real-VT systemd service"
cat > /etc/systemd/system/plasma-shell-realvt.service <<EOF
[Unit]
Description=Plasma shell on direct KWin Wayland VT session
After=kwin-realvt.service
Requires=kwin-realvt.service
PartOf=kwin-realvt.service

[Service]
User=${HEADLESS_USER}
Group=${HEADLESS_USER}
SupplementaryGroups=video render input
WorkingDirectory=${HOME_DIR}

Environment=HOME=${HOME_DIR}
Environment=USER=${HEADLESS_USER}
Environment=LOGNAME=${HEADLESS_USER}
Environment=XDG_RUNTIME_DIR=${RUNTIME_DIR}
Environment=WAYLAND_DISPLAY=${KWIN_DISPLAY}
Environment=DBUS_SESSION_BUS_ADDRESS=unix:path=${RUNTIME_DIR}/bus

ExecStart=${HOME_DIR}/.local/bin/start-plasmashell-realvt.sh

Restart=on-failure
RestartSec=5
StandardOutput=journal
StandardError=journal

[Install]
WantedBy=multi-user.target
EOF

log "Writing Weston diagnostic fallback systemd service"
cat > /etc/systemd/system/weston-kms-session.service <<EOF
[Unit]
Description=Weston diagnostic KMS session on NVIDIA
After=network-online.target
Wants=network-online.target
Conflicts=kwin-realvt.service

[Service]
User=${HEADLESS_USER}
Group=${HEADLESS_USER}
SupplementaryGroups=video render input
WorkingDirectory=${HOME_DIR}

Environment=HOME=${HOME_DIR}
Environment=USER=${HEADLESS_USER}
Environment=LOGNAME=${HEADLESS_USER}
Environment=XDG_RUNTIME_DIR=/tmp/runtime-${HEADLESS_USER}
Environment=WAYLAND_DISPLAY=${WESTON_WAYLAND_DISPLAY}

PermissionsStartOnly=true
ExecStartPre=/usr/bin/mkdir -p /tmp/runtime-${HEADLESS_USER}
ExecStartPre=/usr/bin/chown ${HEADLESS_USER}:${HEADLESS_USER} /tmp/runtime-${HEADLESS_USER}
ExecStartPre=/usr/bin/chmod 700 /tmp/runtime-${HEADLESS_USER}
ExecStartPre=/usr/bin/bash -lc 'for i in \$(seq 1 30); do nvidia-smi >/dev/null 2>&1 && exit 0; sleep 2; done; exit 1'

ExecStart=${HOME_DIR}/.local/bin/start-weston-kms.sh

Restart=on-failure
RestartSec=5
StandardOutput=journal
StandardError=journal

[Install]
WantedBy=multi-user.target
EOF

log "Writing Sunshine systemd service"
cat > /etc/systemd/system/sunshine-headless.service <<EOF
[Unit]
Description=Sunshine on CloudDeploy NVIDIA Wayland KMS
After=${COMPOSITOR_SERVICE} network-online.target tailscaled.service
Wants=network-online.target tailscaled.service ${COMPOSITOR_SERVICE}

[Service]
User=${HEADLESS_USER}
Group=${HEADLESS_USER}
SupplementaryGroups=video render input
WorkingDirectory=${HOME_DIR}

Environment=HOME=${HOME_DIR}
Environment=USER=${HEADLESS_USER}
Environment=LOGNAME=${HEADLESS_USER}
Environment=XDG_RUNTIME_DIR=${RUNTIME_DIR}
Environment=WAYLAND_DISPLAY=${KWIN_DISPLAY}
Environment=DBUS_SESSION_BUS_ADDRESS=unix:path=${RUNTIME_DIR}/bus
Environment=SUNSHINE_STREAM_DIAG_REUSE_AUDIO_PEER=1
Environment=SUNSHINE_STREAM_DIAG_VIDEO_PEER_MODE=rtsp-client-port
Environment=SUNSHINE_STREAM_DIAG_IGNORE_CONTROL_TIMEOUT=1
Environment=SUNSHINE_STREAM_DIAG_FORCE_ANNOUNCE_SUCCESS=1
Environment=SUNSHINE_STREAM_DIAG_FORCE_ANNOUNCE_SUCCESS_IMMEDIATE=1

ExecStartPre=/usr/local/bin/clouddeploy-wait-sunshine-session.sh
ExecStart=${SUNSHINE_RUNTIME_BIN} ${HOME_DIR}/.config/sunshine/sunshine.conf

Restart=on-failure
RestartSec=5
StandardOutput=journal
StandardError=journal

[Install]
WantedBy=multi-user.target
EOF

set_phase "streaming-services"
log "Stopping old compositor/session bits"
KWIN_ALREADY_HEALTHY=0
if [[ "${COMPOSITOR_SERVICE}" == "kwin-realvt.service" ]] \
        && systemctl is-active --quiet kwin-realvt.service \
        && [[ -S "${RUNTIME_DIR}/${KWIN_DISPLAY}" ]] \
        && pgrep -u "${HEADLESS_USER}" -x kwin_wayland >/dev/null 2>&1; then
        KWIN_ALREADY_HEALTHY=1
        log "kwin-realvt.service already active; not stopping real-VT KWin"
fi

if [[ "${KWIN_ALREADY_HEALTHY}" == "1" ]]; then
        systemctl stop \
                sunshine-direct.service \
                sunshine-manual.service \
                sunshine-wayland-nodbus.service \
                plasma-kms-session.service \
                plasma-realvt.service \
                plasma-shell-realvt.service \
                weston-kms-session.service \
                sunshine-headless.service \
                2>/dev/null || true
else
        systemctl stop \
                sunshine-direct.service \
                sunshine-manual.service \
                sunshine-wayland-nodbus.service \
                plasma-kms-session.service \
                plasma-realvt.service \
                kwin-realvt.service \
                plasma-shell-realvt.service \
                weston-kms-session.service \
                sunshine-headless.service \
                2>/dev/null || true
fi

systemctl disable \
        sunshine-direct.service \
        sunshine-manual.service \
        sunshine-wayland-nodbus.service \
        plasma-kms-session.service \
        plasma-realvt.service \
        2>/dev/null || true

if [[ "${KWIN_ALREADY_HEALTHY}" == "1" ]]; then
        pkill -9 -u "${HEADLESS_USER}" -f 'plasmashell|kactivitymanagerd|plasma_session|plasma_waitforname|ksmserver|ksplashqml|startplasma-wayland|kdeinit5|klauncher|kded|sunshine|weston' 2>/dev/null || true
else
        pkill -9 -u "${HEADLESS_USER}" -f 'kwin_wayland|kwin_wayland_wrapper|plasmashell|kactivitymanagerd|plasma_session|plasma_waitforname|ksmserver|ksplashqml|startplasma-wayland|kdeinit5|klauncher|kded|sunshine|weston|Xwayland' 2>/dev/null || true
fi

if [[ "${KWIN_ALREADY_HEALTHY}" == "1" ]]; then
        rm -f /tmp/runtime-"${HEADLESS_USER}"/wayland-* 2>/dev/null || true
else
        rm -f "${RUNTIME_DIR}"/wayland-* /tmp/runtime-"${HEADLESS_USER}"/wayland-* 2>/dev/null || true
fi

log "Enabling selected services"
systemctl daemon-reload
systemctl disable \
        plasma-realvt.service \
        kwin-realvt.service \
        plasma-shell-realvt.service \
        weston-kms-session.service \
        sunshine-headless.service \
        >/dev/null 2>&1 || true

if systemctl list-unit-files | grep -q '^tailscaled'; then
        systemctl enable tailscaled || true
fi

if [[ "${COMPOSITOR_SERVICE}" == "kwin-realvt.service" ]]; then
        systemctl enable kwin-realvt.service plasma-shell-realvt.service sunshine-headless.service

        if [[ "${KWIN_ALREADY_HEALTHY}" == "1" ]] && systemctl is-active --quiet kwin-realvt.service; then
                log "kwin-realvt.service already active; not restarting real-VT KWin"
        else
                systemctl restart kwin-realvt.service
        fi
        sleep 8

        /usr/local/bin/clouddeploy-force-kwin-mode.sh || log "WARNING: clouddeploy-force-kwin-mode failed; final validation will decide success."

        systemctl restart plasma-shell-realvt.service || true
        systemctl restart sunshine-headless.service
else
        systemctl enable weston-kms-session.service sunshine-headless.service

        systemctl restart weston-kms-session.service
        sleep 5

        systemctl restart sunshine-headless.service
fi

systemctl restart tailscaled 2>/dev/null || true

install_clouddeploy_helpers
known_good_clean_reset_streaming_stack

validate_streaming_stack_ready

log "Final validation markers"
"${HOME_DIR}/.local/bin/clouddeploy-kms-status.sh" || true

print_driver_cuda_sunshine_summary
print_final_validation_summary

echo "$SCRIPT_VERSION" > "$SENTINEL"

systemctl disable clouddeploy-wayland-continue.service >/dev/null 2>&1 || true
systemctl reset-failed clouddeploy-wayland-continue.service >/dev/null 2>&1 || true
rm -f "${REBOOT_MARKER}" "${REBOOT_REASON_FILE}" || true
rm -f "${CLOUDDEPLOY_STATE_DIR}/reboot-needed" || true
systemctl enable --now clouddeploy-watch-streaming.timer >/dev/null 2>&1 || true

install_optional_apps_nonfatal

echo
echo "Use Moonlight against the Tailscale IP, not the public IP."
if command -v tailscale >/dev/null 2>&1; then
        TS_IP="$(tailscale ip -4 2>/dev/null | head -n1 || true)"

        if [[ -n "${TS_IP}" ]]; then
                echo "Sunshine web UI: https://${TS_IP}:47990"
                echo "Moonlight host:   ${TS_IP}"
        fi
fi

echo "Sunshine web UI username: ${SUNSHINE_USER}"
echo "Sunshine web UI password: stored in ${CLOUDDEPLOY_ENV_FILE} when configured"
echo
echo "If Moonlight shows a PIN, enter it in Sunshine's PIN tab."
echo "Do NOT inject sunshine_state.json pairings in the deploy script."
echo
echo "Status helper: ${HOME_DIR}/.local/bin/clouddeploy-kms-status.sh"
echo "Diagnostic helper: sudo clouddeploy-diagnose-kms"
echo "Pairing helper: sudo clouddeploy-pair-pin"
echo "Optional game/app installs requested: ${INSTALL_OPTIONAL_APPS}"
echo "App install helper: sudo bash -lc 'INSTALL_OPTIONAL_APPS=1 clouddeploy-run'"
echo "Audio is intentionally disabled until video capture is stable."
echo "Expected current milestone: Plasma Wayland visible, raw KMS capture tested, Sunshine KMS/NVENC alive."
echo "Expected KWin path: kwin_wayland --drm --xwayland --socket ${KWIN_DISPLAY}"
echo "Expected Moonlight: ${TARGET_WIDTH}x${TARGET_HEIGHT}, ${TARGET_FPS} FPS, HDR off, AV1 preferred"
print_known_good_checklist
