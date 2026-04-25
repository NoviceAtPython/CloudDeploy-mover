#!/bin/bash

set -Eeuo pipefail
trap 'echo "Failed at line $LINENO"; exit 1' ERR
export DEBIAN_FRONTEND=noninteractive

DEFAULT_USER="${SUDO_USER:-user}"
HEADLESS_USER="${HEADLESS_USER:-$DEFAULT_USER}"
SUNSHINE_USER="${SUNSHINE_USER:-$DEFAULT_USER}"
SUNSHINE_PASS="${SUNSHINE_PASS:-}"
TAILSCALE_AUTHKEY="${TAILSCALE_AUTHKEY:-}"
SUNSHINE_DEB_URL="${SUNSHINE_DEB_URL:-https://github.com/LizardByte/Sunshine/releases/download/v2025.924.154138/sunshine-ubuntu-24.04-amd64.deb}"
FORCED_CONNECTOR="${FORCED_CONNECTOR:-DP-1}"
TARGET_WIDTH="${TARGET_WIDTH:-3840}"
TARGET_HEIGHT="${TARGET_HEIGHT:-2160}"
TARGET_FPS="${TARGET_FPS:-120}"
ENABLE_HDR="${ENABLE_HDR:-0}"
EDID_PROFILE="${EDID_PROFILE:-4k120-sdr}"
WESTON_MODE="${WESTON_MODE:-${TARGET_WIDTH}x${TARGET_HEIGHT}@${TARGET_FPS}}"
WAYLAND_DISPLAY_NAME="${WAYLAND_DISPLAY_NAME:-wayland-cd}"
SUNSHINE_CAPTURE_METHOD="${SUNSHINE_CAPTURE_METHOD:-kms}"
SUNSHINE_ENCODER="${SUNSHINE_ENCODER:-nvenc}"
SUNSHINE_DRM_DEVICE="${SUNSHINE_DRM_DEVICE:-auto}"
SESSION_BACKEND="${SESSION_BACKEND:-weston}"
KMS_OUTPUT="${KMS_OUTPUT:-${FORCED_CONNECTOR}}"
HEADLESS_RESOLUTION="${HEADLESS_RESOLUTION:-${TARGET_WIDTH}x${TARGET_HEIGHT}}"
ENABLE_AV1="${ENABLE_AV1:-0}"
ENABLE_HEVC="${ENABLE_HEVC:-0}"
SUNSHINE_AV1_MODE="${SUNSHINE_AV1_MODE:-0}"
SUNSHINE_HEVC_MODE="${SUNSHINE_HEVC_MODE:-0}"
RUNTIME_DIR="${RUNTIME_DIR:-/tmp/runtime-user}"
SENTINEL="/opt/clouddeploy-wayland.installed"
SCRIPT_VERSION="9"
REBOOT_MARKER="/opt/clouddeploy-wayland.needs-reboot"
REBOOT_REASON_FILE="/opt/clouddeploy-wayland.reboot-reason"
GRUB_OVERRIDE_FILE="/etc/default/grub.d/99-clouddeploy-edid.cfg"
CLOUDDEPLOY_ENV_FILE="/etc/clouddeploy-wayland.env"

INSTALL_OPTIONAL_APPS="${INSTALL_OPTIONAL_APPS:-1}"

# =========================
# Helpers
# =========================
log() {
        printf '\n[%s] %s\n' "$(date '+%F %T')" "$*"
}

die() {
        echo "ERROR: $*" >&2
        exit 1
}

require_root() {
        [[ "${EUID}" -eq 0 ]] || die "Run this script as root."
}

user_home() {
        getent passwd "$1" | cut -d: -f6
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
        find "/lib/modules/${kernel}" -type f \
                \( -name 'nvidia*.ko' -o -name 'nvidia*.ko.xz' -o -name 'nvidia*.ko.zst' \) \
                2>/dev/null | grep -q .
}

write_phase2_edids() {
        install -d -m 0755 /lib/firmware/edid

        local script_dir
        script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

        python3 "${script_dir}/scripts/write-edids.py"
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

write_clouddeploy_env_file() {
        install -m 0600 /dev/null "${CLOUDDEPLOY_ENV_FILE}"

        {
                printf 'HEADLESS_USER=%q\n' "${HEADLESS_USER}"
                printf 'SUNSHINE_USER=%q\n' "${SUNSHINE_USER}"
                printf 'SUNSHINE_PASS=%q\n' "${SUNSHINE_PASS}"
                printf 'TAILSCALE_AUTHKEY=%q\n' "${TAILSCALE_AUTHKEY}"
                printf 'FORCED_CONNECTOR=%q\n' "${FORCED_CONNECTOR}"
                printf 'TARGET_WIDTH=%q\n' "${TARGET_WIDTH}"
                printf 'TARGET_HEIGHT=%q\n' "${TARGET_HEIGHT}"
                printf 'TARGET_FPS=%q\n' "${TARGET_FPS}"
                printf 'ENABLE_HDR=%q\n' "${ENABLE_HDR}"
                printf 'EDID_PROFILE=%q\n' "${EDID_PROFILE}"
                printf 'WESTON_MODE=%q\n' "${WESTON_MODE}"
                printf 'WAYLAND_DISPLAY_NAME=%q\n' "${WAYLAND_DISPLAY_NAME}"
                printf 'SUNSHINE_CAPTURE_METHOD=%q\n' "${SUNSHINE_CAPTURE_METHOD}"
                printf 'SUNSHINE_ENCODER=%q\n' "${SUNSHINE_ENCODER}"
                printf 'SUNSHINE_DRM_DEVICE=%q\n' "${SUNSHINE_DRM_DEVICE}"
                printf 'SESSION_BACKEND=%q\n' "${SESSION_BACKEND}"
                printf 'KMS_OUTPUT=%q\n' "${KMS_OUTPUT}"
                printf 'HEADLESS_RESOLUTION=%q\n' "${HEADLESS_RESOLUTION}"
                printf 'SUNSHINE_AV1_MODE=%q\n' "${SUNSHINE_AV1_MODE}"
                printf 'SUNSHINE_HEVC_MODE=%q\n' "${SUNSHINE_HEVC_MODE}"
                printf 'RUNTIME_DIR=%q\n' "${RUNTIME_DIR}"
                printf 'INSTALL_OPTIONAL_APPS=%q\n' "${INSTALL_OPTIONAL_APPS}"
        } > "${CLOUDDEPLOY_ENV_FILE}"

        chmod 0600 "${CLOUDDEPLOY_ENV_FILE}"
}

install_continuation_service() {
        local script_path
        script_path="$(readlink -f "$0")"

        cat > /usr/local/sbin/clouddeploy-wayland-continue.sh <<EOF
#!/usr/bin/env bash
set -euo pipefail

[[ -f "${REBOOT_MARKER}" ]] || exit 0
reason="\$(cat "${REBOOT_REASON_FILE}" 2>/dev/null || echo unknown)"
rm -f "${REBOOT_MARKER}" "${REBOOT_REASON_FILE}"
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

        write_clouddeploy_env_file
        install_continuation_service
        echo "${reason}" > "${REBOOT_REASON_FILE}"
        touch "${REBOOT_MARKER}"
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
                        drm.edid_firmware=*edid/virtual-*.bin|video=${FORCED_CONNECTOR}:e|nvidia-drm.modeset=*|nvidia-drm.fbdev=*)
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
        local arg_modeset="$3"
        local arg_fbdev="$4"

        install -d -m 0755 /etc/default/grub.d
        sanitize_grub_default_cmdline

        cat > "${GRUB_OVERRIDE_FILE}" <<EOF
# Managed by CloudDeploy Wayland script.
GRUB_CMDLINE_LINUX_DEFAULT="\${GRUB_CMDLINE_LINUX_DEFAULT} ${arg_edid} ${arg_video} ${arg_modeset} ${arg_fbdev}"
EOF
}

cmdline_has_required_phase2_args() {
        local edid_file="$1"
        local arg_edid="drm.edid_firmware=${FORCED_CONNECTOR}:edid/${edid_file}"
        local arg_video="video=${FORCED_CONNECTOR}:e"
        local arg_modeset="nvidia-drm.modeset=1"

        grep -qF "${arg_edid}" /proc/cmdline \
                && grep -qF "${arg_video}" /proc/cmdline \
                && grep -qF "${arg_modeset}" /proc/cmdline
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

ensure_phase2_kernel_args() {
        local edid_file="$1"
        local arg_edid="drm.edid_firmware=${FORCED_CONNECTOR}:edid/${edid_file}"
        local arg_video="video=${FORCED_CONNECTOR}:e"
        local arg_modeset="nvidia-drm.modeset=1"
        local arg_fbdev="nvidia-drm.fbdev=1"

        log "Writing GRUB kernel args in ${GRUB_OVERRIDE_FILE} for ${FORCED_CONNECTOR} using ${edid_file}"
        write_phase2_grub_override "${arg_edid}" "${arg_video}" "${arg_modeset}" "${arg_fbdev}"
        update-grub

        if grep -qF "${arg_edid}" /proc/cmdline \
                && grep -qF "${arg_video}" /proc/cmdline \
                && grep -qF "${arg_modeset}" /proc/cmdline \
                && grep -qF "${arg_fbdev}" /proc/cmdline; then
                log "Kernel cmdline already contains required EDID and DRM args"
                return 0
        fi

        if [[ "${CLOUDDEPLOY_CONTINUE_REASON:-}" == "edid-kernel-args" ]]; then
                die "Kernel cmdline is still missing required EDID/DRM args after reboot"
        fi

        schedule_reboot_for_continuation "edid-kernel-args" "Rebooting to apply EDID and DRM kernel arguments"
}

nvidia_driver_ready() {
        command -v nvidia-smi >/dev/null 2>&1 && nvidia-smi >/dev/null 2>&1
}

run_as_user() {
        local user="$1"
        shift
        runuser -u "${user}" -- "$@"
}

wait_for_cloud_init() {
        if command -v cloud-init >/dev/null 2>&1; then
                echo "Waiting for cloud-init..."
                timeout 180 cloud-init status --wait || echo "cloud-init timeout; continuing"
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

apt_update_retry() {
        wait_for_cloud_init
        wait_for_apt
        DEBIAN_FRONTEND=noninteractive
        apt-get update -o Acquire::Retries=6 -o Acquire::http::Timeout=20
}

apt_install_wait() {
        wait_for_apt
        DEBIAN_FRONTEND=noninteractive
        apt-get install -y "$@"
}

apt_purge_wait() {
        wait_for_apt
        apt-get purge -y "$@"
}

KNOWN_WESTON_MODE_LINE=""
KNOWN_SUNSHINE_RESOLUTION_LINE=""
KNOWN_SUNSHINE_MONITOR_LINE=""
KNOWN_SUNSHINE_KMS_LINE=""
KNOWN_SUNSHINE_NVENC_LINE=""
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
                | awk -v output="${FORCED_CONNECTOR}" \
                        -v target="${target_mode}" \
                        -v refresh='@(119([.][0-9]+)?|120([.][0-9]+)?)' '
                        $0 ~ ("Output " output " video modes:") {
                                in_target_output=1
                                next
                        }
                        $0 ~ /Output .+ video modes:/ && $0 !~ ("Output " output " video modes:") {
                                in_target_output=0
                        }
                        in_target_output && $0 ~ target refresh && $0 ~ /current/ {
                                line=$0
                        }
                        $0 ~ output && $0 ~ target refresh && $0 ~ /current/ {
                                line=$0
                        }
                        END {
                                if (line != "") print line
                        }' \
                | tail -n1
}

refresh_streaming_log_markers() {
        local sunshine_since="${1:-}"

        LAST_WESTON_LOG="$(journalctl -u weston-kms-session.service -n 260 --no-pager 2>/dev/null || true)"
        LAST_SUNSHINE_LOG="$(sunshine_journal_since "${sunshine_since}")"

        KNOWN_WESTON_MODE_LINE="$(weston_current_mode_line_from_log "${LAST_WESTON_LOG}" || true)"
        KNOWN_SUNSHINE_RESOLUTION_LINE="$(printf '%s\n' "${LAST_SUNSHINE_LOG}" \
                | grep -F "Desktop resolution: ${TARGET_WIDTH}x${TARGET_HEIGHT}" \
                | tail -n1 || true)"
        KNOWN_SUNSHINE_MONITOR_LINE="$(printf '%s\n' "${LAST_SUNSHINE_LOG}" \
                | grep -F "Monitor 0 is ${FORCED_CONNECTOR}" \
                | tail -n1 || true)"
        KNOWN_SUNSHINE_KMS_LINE="$(printf '%s\n' "${LAST_SUNSHINE_LOG}" \
                | grep -F "Found monitor for DRM screencasting" \
                | tail -n1 || true)"
        KNOWN_SUNSHINE_NVENC_LINE="$(printf '%s\n' "${LAST_SUNSHINE_LOG}" \
                | grep -Ei 'Nvenc initialized successfully|Found H[.]264 encoder: h264_nvenc|h264_nvenc' \
                | tail -n1 || true)"
}

streaming_log_markers_ready() {
        [[ -n "${KNOWN_WESTON_MODE_LINE}" ]] \
                && [[ -n "${KNOWN_SUNSHINE_RESOLUTION_LINE}" ]] \
                && [[ -n "${KNOWN_SUNSHINE_MONITOR_LINE}" ]] \
                && [[ -n "${KNOWN_SUNSHINE_KMS_LINE}" ]] \
                && [[ -n "${KNOWN_SUNSHINE_NVENC_LINE}" ]]
}

sunshine_started_with_zero_resolution() {
        printf '%s\n' "${LAST_SUNSHINE_LOG}" | grep -Fq "Desktop resolution: 0x0"
}

print_streaming_diagnostics() {
        echo "=== Wayland socket ==="
        ls -lah "${RUNTIME_DIR}/${WAYLAND_DISPLAY_NAME}" "${RUNTIME_DIR}/${WAYLAND_DISPLAY_NAME}.lock" 2>/dev/null || true
        echo
        echo "=== Weston process ==="
        pgrep -a -u "${HEADLESS_USER}" -x weston || true
        echo
        echo "=== systemctl status weston-kms-session.service ==="
        systemctl --no-pager --full status weston-kms-session.service | sed -n '1,14p' || true
        echo
        echo "=== systemctl status sunshine-headless.service ==="
        systemctl --no-pager --full status sunshine-headless.service | sed -n '1,14p' || true
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
        local socket_path="${RUNTIME_DIR}/${WAYLAND_DISPLAY_NAME}"

        for _ in $(seq 1 90); do
                LAST_WESTON_LOG="$(journalctl -u weston-kms-session.service -n 260 --no-pager 2>/dev/null || true)"
                KNOWN_WESTON_MODE_LINE="$(weston_current_mode_line_from_log "${LAST_WESTON_LOG}" || true)"

                if [[ -S "${socket_path}" ]] \
                        && pgrep -u "${HEADLESS_USER}" -x weston >/dev/null 2>&1 \
                        && [[ -n "${KNOWN_WESTON_MODE_LINE}" ]]; then
                        return 0
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

start_streaming_stack_ordered() {
        log "Starting Weston before Sunshine"
        systemctl stop sunshine-headless.service 2>/dev/null || true
        systemctl stop weston-kms-session.service 2>/dev/null || true
        rm -f "${RUNTIME_DIR}/${WAYLAND_DISPLAY_NAME}" "${RUNTIME_DIR}/${WAYLAND_DISPLAY_NAME}.lock" || true

        if ! systemctl start weston-kms-session.service; then
                print_streaming_diagnostics
                return 1
        fi

        if ! wait_for_weston_ready; then
                print_streaming_diagnostics
                return 1
        fi

        LAST_SUNSHINE_START_SINCE="$(date '+%F %T')"
        if ! systemctl start sunshine-headless.service; then
                print_streaming_diagnostics
                return 1
        fi
}

stabilize_sunshine_after_start() {
        local rc=0

        wait_for_sunshine_post_start_markers "${LAST_SUNSHINE_START_SINCE}" && return 0
        rc=$?

        if [[ "${rc}" -eq 2 ]]; then
                echo "Detected Sunshine Desktop resolution: 0x0; diagnostics before one clean ordered restart:"
                print_streaming_diagnostics
                start_streaming_stack_ordered || return 1
                wait_for_sunshine_post_start_markers "${LAST_SUNSHINE_START_SINCE}" && return 0
                rc=$?
        fi

        print_streaming_diagnostics
        return "${rc}"
}

ordered_restart_streaming_stack() {
        start_streaming_stack_ordered || return 1
        stabilize_sunshine_after_start
}

validate_streaming_stack_ready() {
        local target_mode="${TARGET_WIDTH}x${TARGET_HEIGHT}"

        if ! nvidia_driver_ready; then
                print_server_validation_diagnostics
                die "NVIDIA driver check failed: nvidia-smi is not healthy"
        fi
        if ! connector_forced_connected; then
                print_server_validation_diagnostics
                die "${FORCED_CONNECTOR} is not connected"
        fi
        if ! connector_has_mode "${target_mode}"; then
                print_server_validation_diagnostics
                die "${target_mode} is not exposed on ${FORCED_CONNECTOR}"
        fi
        if ! systemctl is-active --quiet weston-kms-session.service; then
                print_server_validation_diagnostics
                die "weston-kms-session.service failed to start"
        fi
        if ! systemctl is-active --quiet sunshine-headless.service; then
                print_server_validation_diagnostics
                die "sunshine-headless.service failed to start"
        fi

        if ! wait_for_streaming_log_markers; then
                print_server_validation_diagnostics
                die "Did not observe expected Weston mode / Sunshine KMS+NVENC log markers"
        fi
}

print_known_good_checklist() {
        local target_mode="${TARGET_WIDTH}x${TARGET_HEIGHT}"

        echo
        echo "CloudDeploy 4K120 SDR status:"
        echo "[OK] NVIDIA driver working"
        echo "[OK] ${FORCED_CONNECTOR} forced connected"
        echo "[OK] ${target_mode} mode exposed"
        echo "[OK] Weston active"
        echo "[OK] Weston current mode ${target_mode}@119.9-ish (${KNOWN_WESTON_MODE_LINE})"
        echo "[OK] Sunshine active"
        echo "[OK] Sunshine Desktop resolution ${target_mode} (${KNOWN_SUNSHINE_RESOLUTION_LINE})"
        echo "[OK] Sunshine Monitor 0 is ${FORCED_CONNECTOR} (${KNOWN_SUNSHINE_MONITOR_LINE})"
        echo "[OK] Sunshine KMS capture found monitor (${KNOWN_SUNSHINE_KMS_LINE})"
        echo "[OK] NVENC initialized (${KNOWN_SUNSHINE_NVENC_LINE})"
        echo "Next: connect Moonlight at ${target_mode} ${TARGET_FPS} FPS, HDR off"
}

install_optional_apps_nonfatal() {
        local tmpchrome

        if [[ "${INSTALL_OPTIONAL_APPS}" != "1" ]]; then
                return 0
        fi

        log "Installing optional desktop apps (non-fatal)"

        if ! dpkg --print-foreign-architectures | grep -q i386; then
                wait_for_apt
                dpkg --add-architecture i386 || log "Could not add i386 architecture; continuing"
                apt_update_retry || log "Apt update failed after adding i386 architecture; continuing"
        fi

        wait_for_apt
        apt-get upgrade -y || log "apt-get upgrade failed; continuing"
        apt_install_wait flatpak steam-installer wine64 winetricks || log "Optional apt packages failed; continuing"

        if command -v flatpak >/dev/null 2>&1; then
                flatpak remote-add --if-not-exists flathub https://flathub.org/repo/flathub.flatpakrepo || true
                flatpak install -y flathub com.heroicgameslauncher.hgl || true
                flatpak install -y flathub net.lutris.Lutris || true
                flatpak install -y flathub com.usebottles.bottles || true
                flatpak install -y flathub org.prismlauncher.PrismLauncher || true
        else
                log "Flatpak is unavailable; skipping Flatpak app installs"
        fi

        tmpchrome="/tmp/google-chrome-stable_current_amd64.deb"
        if wget -O "${tmpchrome}" https://dl.google.com/linux/direct/google-chrome-stable_current_amd64.deb; then
                dpkg -i "${tmpchrome}" || apt-get -f install -y || log "Google Chrome install failed; continuing"
                rm -f "${tmpchrome}"
        else
                log "Google Chrome download failed; continuing"
        fi
}

# =========================
# Start
# =========================
require_root

if [[ -z "${SUNSHINE_PASS}" ]]; then
        die "SUNSHINE_PASS is required. Re-run with: sudo SUNSHINE_PASS='<strong-password>' ... bash ./CloudDeploy-wayland.sh"
fi

if [[ -f "$SENTINEL" ]] && [[ "$(cat "$SENTINEL")" == "$SCRIPT_VERSION" ]]; then
        log "CloudDeploy-wayland has already run on this machine for version $SCRIPT_VERSION. Restarting in the known-good order and validating..."
        systemctl daemon-reload || true
        systemctl reset-failed weston-kms-session.service sunshine-headless.service tailscaled || true
        systemctl enable weston-kms-session.service sunshine-headless.service || true
        if systemctl list-unit-files | grep -q '^tailscaled'; then
                systemctl enable tailscaled || true
                systemctl restart tailscaled || true
        fi

        ordered_restart_streaming_stack || die "Streaming stack did not pass post-start stabilization"

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

        validate_streaming_stack_ready
        rm -f "${CLOUDDEPLOY_ENV_FILE}" || true

        echo
        echo "Service states:"
        systemctl --no-pager --full status weston-kms-session.service | sed -n '1,8p' || true
        systemctl --no-pager --full status sunshine-headless.service | sed -n '1,8p' || true
        systemctl --no-pager --full status tailscaled | sed -n '1,8p' || true

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
if ! id "${HEADLESS_USER}" >/dev/null 2>&1; then
        useradd -m -s /bin/bash "${HEADLESS_USER}"
fi

HOME_DIR="$(user_home "${HEADLESS_USER}")"
[[ -n "${HOME_DIR}" ]] || die "Could not determine home directory for ${HEADLESS_USER}"

usermod -aG sudo,video,input,render "${HEADLESS_USER}" || true
chown -R "${HEADLESS_USER}:${HEADLESS_USER}" "${HOME_DIR}"

log "Installing base packages"
apt_update_retry
apt_install_wait \
        curl wget ca-certificates gnupg software-properties-common \
        pciutils jq libcap2-bin edid-decode libdrm-tests mesa-utils-extra kmscube \
        kde-plasma-desktop plasma-workspace-wayland kwin-wayland weston xwayland seatd \
        pipewire wireplumber xdg-desktop-portal xdg-desktop-portal-kde \
        ubuntu-drivers-common

log "Installing Gamescope package (optional)"
apt_install_wait gamescope || echo "Gamescope apt package unavailable; continuing without it"

log "Checking NVIDIA driver status"
if nvidia_driver_ready; then
        log "NVIDIA drivers are already installed and working"
else
        log "Installing NVIDIA drivers"
        MODPROBE_FAILED=0
        ubuntu-drivers install || die "Failed to install NVIDIA drivers"
        if ! modprobe nvidia; then
                MODPROBE_FAILED=1
        fi
        if ! modprobe nvidia_modeset; then
                MODPROBE_FAILED=1
        fi
        if ! modprobe nvidia_drm; then
                MODPROBE_FAILED=1
        fi

        for _ in $(seq 1 15); do
                if nvidia_driver_ready; then
                        log "NVIDIA drivers are now working"
                        break
                fi
                echo "Waiting for NVIDIA drivers to be ready..."
                sleep 3
        done

        if ! nvidia_driver_ready; then
                if [[ "${MODPROBE_FAILED}" -eq 1 ]] && ! nvidia_modules_present_for_running_kernel; then
                        if [[ "${CLOUDDEPLOY_CONTINUE_REASON:-}" == "nvidia-driver" ]]; then
                                die "NVIDIA modules are still missing for kernel $(uname -r) after continuation reboot"
                        fi
                        schedule_reboot_for_continuation "nvidia-driver" "NVIDIA modules are missing for kernel $(uname -r); rebooting and continuing automatically"
                fi

                if [[ "${CLOUDDEPLOY_CONTINUE_REASON:-}" == "nvidia-driver" ]]; then
                        die "NVIDIA drivers are still not ready after continuation reboot"
                fi

                die "NVIDIA drivers still not ready after installation. This VM may need a reboot or may not be compatible."
        fi
fi

log "Removing pieces that fought the working setup"
systemctl disable --now sddm 2>/dev/null || true
apt_purge_wait xserver-xorg-video-dummy 2>/dev/null || true
rm -f /etc/sddm.conf.d/autologin.conf
rm -f /etc/sddm.conf.d/zz-autologin.conf

log "Installing Sunshine"
if ! command -v sunshine >/dev/null 2>&1; then
        tmpdeb="$(mktemp /tmp/sunshine.XXXXXX.deb)"
        wget -O "${tmpdeb}" "${SUNSHINE_DEB_URL}"
        wait_for_apt
        dpkg -i "${tmpdeb}" || apt-get -f install -y
        rm -f "${tmpdeb}"
fi

log "Installing Tailscale if requested"
if [[ -n "${TAILSCALE_AUTHKEY}" ]]; then
        if ! command -v tailscale >/dev/null 2>&1; then
                wait_for_apt
                curl -fsSL https://tailscale.com/install.sh | sh
        fi

        systemctl enable --now tailscaled || true
        tailscale up --authkey="${TAILSCALE_AUTHKEY}" --ssh || log "tailscale up failed; continuing"
fi

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
log "Using Sunshine DRM device: ${SUNSHINE_DRM_DEVICE}"
log "Using Weston DRM device basename: ${WESTON_DRM_DEVICE}"

log "Writing Phase 2 EDID profiles"
write_phase2_edids
update-initramfs -u -k all

SELECTED_EDID_FILE="$(select_phase2_edid_file)"
[[ -n "${SELECTED_EDID_FILE}" ]] || die "Could not determine EDID profile file"
log "Selected EDID profile: ${SELECTED_EDID_FILE} on ${FORCED_CONNECTOR}"

ensure_phase2_kernel_args "${SELECTED_EDID_FILE}"
validate_phase2_display_state "${SELECTED_EDID_FILE}"

if [[ "${SESSION_BACKEND}" != "weston" ]]; then
        die "Unsupported SESSION_BACKEND '${SESSION_BACKEND}'. Phase 2 currently supports weston only."
fi

log "Writing Weston KMS session startup"
install -d -m 0755 -o "${HEADLESS_USER}" -g "${HEADLESS_USER}" \
        "${HOME_DIR}/.local/bin" \
        "${HOME_DIR}/.local/share" \
        "${HOME_DIR}/.config" \
        "${HOME_DIR}/.config/weston" \
        "${HOME_DIR}/.config/sunshine"

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
export XDG_RUNTIME_DIR="${RUNTIME_DIR}"
export XDG_SESSION_TYPE=wayland
export WAYLAND_DISPLAY="${WAYLAND_DISPLAY_NAME}"
export LIBSEAT_BACKEND=seatd
export __EGL_VENDOR_LIBRARY_FILENAMES=/usr/share/glvnd/egl_vendor.d/10_nvidia.json
export __GLX_VENDOR_LIBRARY_NAME=nvidia

mkdir -p "\$XDG_RUNTIME_DIR" "${HOME_DIR}/.local/share" "${HOME_DIR}/.config/weston"
chmod 700 "\$XDG_RUNTIME_DIR"

exec /usr/bin/weston \
        --backend=drm-backend.so \
        --drm-device="${WESTON_DRM_DEVICE}" \
        --socket="${WAYLAND_DISPLAY_NAME}" \
        --config="${HOME_DIR}/.config/weston.ini"
EOF

chown "${HEADLESS_USER}:${HEADLESS_USER}" "${HOME_DIR}/.config/weston.ini"
chown "${HEADLESS_USER}:${HEADLESS_USER}" "${HOME_DIR}/.local/bin/start-weston-kms.sh"
chmod 0755 "${HOME_DIR}/.local/bin/start-weston-kms.sh"

cat > "${HOME_DIR}/.local/bin/start-sunshine-headless.sh" <<EOF
#!/usr/bin/env bash
set -euo pipefail

export HOME="${HOME_DIR}"
export USER="${HEADLESS_USER}"
export LOGNAME="${HEADLESS_USER}"
export XDG_RUNTIME_DIR="${RUNTIME_DIR}"
export WAYLAND_DISPLAY="${WAYLAND_DISPLAY_NAME}"

mkdir -p "\$XDG_RUNTIME_DIR"
chmod 700 "\$XDG_RUNTIME_DIR"

SOCKET_PATH="\${XDG_RUNTIME_DIR}/${WAYLAND_DISPLAY_NAME}"
SOCKET_STABILIZED=0

for _ in \$(seq 1 120); do
        if [[ -S "\${SOCKET_PATH}" ]]; then
                if [[ "\${SOCKET_STABILIZED}" -eq 0 ]]; then
                        sleep 2
                        SOCKET_STABILIZED=1
                fi

                if [[ -S "\${SOCKET_PATH}" ]] && pgrep -u "${HEADLESS_USER}" -x weston >/dev/null 2>&1; then
                        exec /usr/bin/sunshine
                fi
        else
                SOCKET_STABILIZED=0
        fi

        sleep 1
done

echo "Wayland session never became ready for Sunshine" >&2
exit 1
EOF

chown "${HEADLESS_USER}:${HEADLESS_USER}" "${HOME_DIR}/.local/bin/start-sunshine-headless.sh"
chmod 0755 "${HOME_DIR}/.local/bin/start-sunshine-headless.sh"

cat > "${HOME_DIR}/.local/bin/start-gamescope-hdr-test.sh" <<EOF
#!/usr/bin/env bash
set -euo pipefail

export HOME="${HOME_DIR}"
export USER="${HEADLESS_USER}"
export LOGNAME="${HEADLESS_USER}"
export XDG_RUNTIME_DIR="${RUNTIME_DIR}"
export WAYLAND_DISPLAY="${WAYLAND_DISPLAY_NAME}"
export __EGL_VENDOR_LIBRARY_FILENAMES=/usr/share/glvnd/egl_vendor.d/10_nvidia.json
export __GLX_VENDOR_LIBRARY_NAME=nvidia

mkdir -p "\$XDG_RUNTIME_DIR"
chmod 700 "\$XDG_RUNTIME_DIR"

exec /usr/bin/gamescope \
        -O "${FORCED_CONNECTOR}" \
        -W "${TARGET_WIDTH}" -H "${TARGET_HEIGHT}" \
        -w "${TARGET_WIDTH}" -h "${TARGET_HEIGHT}" \
        -r "${TARGET_FPS}" \
        --expose-wayland \
        --force-composition \
        --hdr-enabled \
        --hdr-itm-enable \
        -- /usr/bin/weston-simple-shm
EOF

chown "${HEADLESS_USER}:${HEADLESS_USER}" "${HOME_DIR}/.local/bin/start-gamescope-hdr-test.sh"
chmod 0755 "${HOME_DIR}/.local/bin/start-gamescope-hdr-test.sh"

cat > "${HOME_DIR}/.local/bin/clouddeploy-kms-status.sh" <<EOF
#!/usr/bin/env bash
set -euo pipefail

echo "=== /proc/cmdline ==="
cat /proc/cmdline || true

echo
echo "=== nvidia-smi ==="
nvidia-smi || true

echo
echo "=== /dev/dri ==="
ls -lah /dev/dri || true

echo
echo "=== Connector status (${FORCED_CONNECTOR}) ==="
cat /sys/class/drm/card*-${FORCED_CONNECTOR}/status 2>/dev/null || true

echo
echo "=== Connector modes (${FORCED_CONNECTOR}) ==="
cat /sys/class/drm/card*-${FORCED_CONNECTOR}/modes 2>/dev/null || true

if command -v edid-decode >/dev/null 2>&1; then
        echo
        echo "=== EDID decode (firmware /lib/firmware/edid/${SELECTED_EDID_FILE}) ==="
        edid-decode /lib/firmware/edid/${SELECTED_EDID_FILE} || true

        echo
        echo "=== EDID decode (${FORCED_CONNECTOR}) ==="
        for edid_path in /sys/class/drm/card*-${FORCED_CONNECTOR}/edid; do
                [[ -f "\${edid_path}" ]] || continue
                echo "-- \${edid_path} --"
                edid-decode "\${edid_path}" || true
        done
fi

echo
echo "=== systemctl status weston-kms-session.service ==="
systemctl --no-pager --full status weston-kms-session.service || true

echo
echo "=== systemctl status sunshine-headless.service ==="
systemctl --no-pager --full status sunshine-headless.service || true

echo
echo "=== Weston journal (last 160) ==="
journalctl -u weston-kms-session.service -n 160 --no-pager || true

echo
echo "=== Sunshine journal (last 160) ==="
journalctl -u sunshine-headless.service -n 160 --no-pager || true

if command -v modetest >/dev/null 2>&1; then
        echo
        echo "=== modetest -M nvidia-drm -p ==="
        modetest -M nvidia-drm -p || true
fi
EOF

chown "${HEADLESS_USER}:${HEADLESS_USER}" "${HOME_DIR}/.local/bin/clouddeploy-kms-status.sh"
chmod 0755 "${HOME_DIR}/.local/bin/clouddeploy-kms-status.sh"

log "Writing Sunshine config"
cat > "${HOME_DIR}/.config/sunshine/sunshine.conf" <<EOF
min_log_level = debug
encoder = nvenc
capture = kms
adapter_name = ${SUNSHINE_DRM_DEVICE}
hevc_mode = ${SUNSHINE_HEVC_MODE}
av1_mode = ${SUNSHINE_AV1_MODE}
stream_audio = enabled
address_family = ipv4
ping_timeout = 60000
EOF

chown -R "${HEADLESS_USER}:${HEADLESS_USER}" "${HOME_DIR}/.config"

log "Removing stale Sunshine state to avoid broken pre-pairing"
if [[ -f "${HOME_DIR}/.config/sunshine/sunshine_state.json" ]]; then
    mv "${HOME_DIR}/.config/sunshine/sunshine_state.json" \
       "${HOME_DIR}/.config/sunshine/sunshine_state.json.bak.$(date +%s)"
fi

log "Ensuring Sunshine has cap_sys_admin for KMS capture"
if command -v setcap >/dev/null 2>&1 && command -v sunshine >/dev/null 2>&1; then
        setcap cap_sys_admin+p "$(readlink -f "$(command -v sunshine)")" || true
fi

log "Setting Sunshine web UI credentials"
run_as_user "${HEADLESS_USER}" env HOME="${HOME_DIR}" sunshine --creds "${SUNSHINE_USER}" "${SUNSHINE_PASS}" || true

log "Writing Weston KMS systemd service"
cat > /etc/systemd/system/weston-kms-session.service <<EOF
[Unit]
Description=Weston KMS session on NVIDIA
After=network-online.target
Wants=network-online.target

[Service]
User=${HEADLESS_USER}
Group=${HEADLESS_USER}
SupplementaryGroups=video render input
WorkingDirectory=${HOME_DIR}
Environment=HOME=${HOME_DIR}
Environment=USER=${HEADLESS_USER}
Environment=LOGNAME=${HEADLESS_USER}
Environment=XDG_RUNTIME_DIR=${RUNTIME_DIR}
Environment=WAYLAND_DISPLAY=${WAYLAND_DISPLAY_NAME}
Environment=LIBSEAT_BACKEND=seatd
Environment=__EGL_VENDOR_LIBRARY_FILENAMES=/usr/share/glvnd/egl_vendor.d/10_nvidia.json
Environment=__GLX_VENDOR_LIBRARY_NAME=nvidia
PermissionsStartOnly=true
ExecStartPre=/usr/bin/mkdir -p ${RUNTIME_DIR}
ExecStartPre=/usr/bin/chown ${HEADLESS_USER}:${HEADLESS_USER} ${RUNTIME_DIR}
ExecStartPre=/usr/bin/chmod 700 ${RUNTIME_DIR}
ExecStartPre=/usr/bin/mkdir -p ${HOME_DIR}/.local/share ${HOME_DIR}/.config ${HOME_DIR}/.config/weston ${HOME_DIR}/.local/bin
ExecStartPre=/usr/bin/chown -R ${HEADLESS_USER}:${HEADLESS_USER} ${HOME_DIR}/.local ${HOME_DIR}/.config
ExecStartPre=/usr/bin/bash -lc 'for i in \$(seq 1 30); do nvidia-smi >/dev/null 2>&1 && exit 0; sleep 2; done; exit 1'
ExecStart=/bin/bash ${HOME_DIR}/.local/bin/start-weston-kms.sh
Restart=always
RestartSec=5
StandardOutput=journal
StandardError=journal

[Install]
WantedBy=multi-user.target
EOF

log "Writing Sunshine systemd service"
cat > /etc/systemd/system/sunshine-headless.service <<EOF
[Unit]
Description=Sunshine on headless NVIDIA Wayland
After=weston-kms-session.service network-online.target tailscaled.service
Wants=network-online.target tailscaled.service
Requires=weston-kms-session.service
PartOf=weston-kms-session.service

[Service]
User=${HEADLESS_USER}
Group=${HEADLESS_USER}
WorkingDirectory=${HOME_DIR}
Environment=HOME=${HOME_DIR}
Environment=USER=${HEADLESS_USER}
Environment=LOGNAME=${HEADLESS_USER}
Environment=XDG_RUNTIME_DIR=${RUNTIME_DIR}
Environment=WAYLAND_DISPLAY=${WAYLAND_DISPLAY_NAME}
ExecStart=${HOME_DIR}/.local/bin/start-sunshine-headless.sh
Restart=always
RestartSec=5
StandardOutput=journal
StandardError=journal

[Install]
WantedBy=multi-user.target
EOF

log "Writing experimental Gamescope HDR test service"
cat > /etc/systemd/system/gamescope-hdr-test.service <<EOF
[Unit]
Description=Experimental Gamescope HDR KMS test
After=network-online.target
Wants=network-online.target
Conflicts=weston-kms-session.service sunshine-headless.service

[Service]
User=${HEADLESS_USER}
Group=${HEADLESS_USER}
WorkingDirectory=${HOME_DIR}
Environment=HOME=${HOME_DIR}
Environment=USER=${HEADLESS_USER}
Environment=LOGNAME=${HEADLESS_USER}
Environment=XDG_RUNTIME_DIR=${RUNTIME_DIR}
Environment=WAYLAND_DISPLAY=${WAYLAND_DISPLAY_NAME}
ExecStartPre=/usr/bin/mkdir -p ${RUNTIME_DIR}
ExecStartPre=/usr/bin/chown ${HEADLESS_USER}:${HEADLESS_USER} ${RUNTIME_DIR}
ExecStartPre=/usr/bin/chmod 700 ${RUNTIME_DIR}
ExecStart=/bin/bash ${HOME_DIR}/.local/bin/start-gamescope-hdr-test.sh
Restart=on-failure
RestartSec=5
StandardOutput=journal
StandardError=journal

[Install]
WantedBy=multi-user.target
EOF

log "Stopping any old broken session bits"
pkill -u "${HEADLESS_USER}" -f 'weston|gamescope|sunshine|kwin_wayland|plasmashell|startplasma-wayland' 2>/dev/null || true
systemctl stop sddm 2>/dev/null || true

log "Enabling services"
systemctl daemon-reload
systemctl enable weston-kms-session.service sunshine-headless.service
if systemctl list-unit-files | grep -q '^tailscaled'; then
        systemctl enable tailscaled || true
fi
systemctl disable gamescope-hdr-test.service 2>/dev/null || true

if systemctl list-unit-files | grep -q '^tailscaled'; then
        systemctl restart tailscaled || true
fi
ordered_restart_streaming_stack || die "Streaming stack did not pass post-start stabilization"

validate_streaming_stack_ready

echo "$SCRIPT_VERSION" > "$SENTINEL"

rm -f "${REBOOT_MARKER}" "${REBOOT_REASON_FILE}" || true
systemctl disable clouddeploy-wayland-continue.service >/dev/null 2>&1 || true
systemctl stop clouddeploy-wayland-continue.service >/dev/null 2>&1 || true

install_optional_apps_nonfatal
rm -f "${CLOUDDEPLOY_ENV_FILE}" || true

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
echo "Sunshine web UI password: ${SUNSHINE_PASS}"
echo
echo "If Moonlight shows a PIN, enter it in Sunshine's PIN tab."
echo "Do NOT inject sunshine_state.json pairings in the deploy script."
echo
echo "Weston journal marker: ${KNOWN_WESTON_MODE_LINE}"
echo "Sunshine resolution marker: ${KNOWN_SUNSHINE_RESOLUTION_LINE}"
echo "Sunshine monitor marker: ${KNOWN_SUNSHINE_MONITOR_LINE}"
echo "Sunshine KMS marker: ${KNOWN_SUNSHINE_KMS_LINE}"
echo "Sunshine NVENC marker: ${KNOWN_SUNSHINE_NVENC_LINE}"
print_known_good_checklist
