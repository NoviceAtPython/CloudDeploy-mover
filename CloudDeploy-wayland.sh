#!/bin/bash

set -Eeuo pipefail
trap 'echo "Failed at line $LINENO"; exit 1' ERR
export DEBIAN_FRONTEND=noninteractive

DEFAULT_USER="${SUDO_USER:-user}"
HEADLESS_USER="${HEADLESS_USER:-$DEFAULT_USER}"
SUNSHINE_USER="${SUNSHINE_USER:-$DEFAULT_USER}"
SUNSHINE_PASS="${SUNSHINE_PASS:-Aedyn11107@13}"
TAILSCALE_AUTHKEY="${TAILSCALE_AUTHKEY:-tskey-auth-kNatGervUa11CNTRL-jKpWxbykvf7hF6btz5dXg7EuZeMdTToD}"
SUNSHINE_DEB_URL="${SUNSHINE_DEB_URL:-https://github.com/LizardByte/Sunshine/releases/download/v2025.924.154138/sunshine-ubuntu-24.04-amd64.deb}"
FORCED_CONNECTOR="${FORCED_CONNECTOR:-DP-1}"
TARGET_WIDTH="${TARGET_WIDTH:-3840}"
TARGET_HEIGHT="${TARGET_HEIGHT:-2160}"
TARGET_FPS="${TARGET_FPS:-120}"
ENABLE_HDR="${ENABLE_HDR:-1}"
EDID_PROFILE="${EDID_PROFILE:-auto}"
WESTON_MODE="${WESTON_MODE:-${TARGET_WIDTH}x${TARGET_HEIGHT}@${TARGET_FPS}}"
WAYLAND_DISPLAY_NAME="${WAYLAND_DISPLAY_NAME:-wayland-cd}"
SUNSHINE_CAPTURE_METHOD="${SUNSHINE_CAPTURE_METHOD:-kms}"
SUNSHINE_ENCODER="${SUNSHINE_ENCODER:-nvenc}"
SUNSHINE_DRM_DEVICE="${SUNSHINE_DRM_DEVICE:-auto}"
SESSION_BACKEND="${SESSION_BACKEND:-weston}"
KMS_OUTPUT="${KMS_OUTPUT:-${FORCED_CONNECTOR}}"
HEADLESS_RESOLUTION="${HEADLESS_RESOLUTION:-${TARGET_WIDTH}x${TARGET_HEIGHT}}"
ENABLE_AV1="${ENABLE_AV1:-1}"
ENABLE_HEVC="${ENABLE_HEVC:-1}"
SENTINEL="/opt/clouddeploy-wayland.installed"
SCRIPT_VERSION="6"

[[ $EUID -eq 0 ]] || { echo "Run this script using 'sudo' or as root"; exit 1; }

if [[ -f "$SENTINEL" ]] && [[ "$(cat "$SENTINEL")" == "$SCRIPT_VERSION" ]]; then
        echo "CloudDeploy.sh has already run on this machine for version $SCRIPT_VERSION. Restarting existing services and exiting..."
        systemctl daemon-reload || true
        systemctl reset-failed weston-kms-session.service sunshine-headless.service tailscaled || true
        systemctl enable --now weston-kms-session.service sunshine-headless.service tailscaled || true
        systemctl restart weston-kms-session.service sunshine-headless.service tailscaled || true
        sleep 2

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

install_continuation_service() {
        local script_path
        script_path="$(readlink -f "$0")"

        cat > /usr/local/sbin/clouddeploy-wayland-continue.sh <<EOF
#!/usr/bin/env bash
set -euo pipefail

[[ -f /opt/clouddeploy-wayland.needs-reboot ]] || exit 0
rm -f /opt/clouddeploy-wayland.needs-reboot
CLOUDDEPLOY_CONTINUE=1 /bin/bash "${script_path}"
EOF
        chmod 0755 /usr/local/sbin/clouddeploy-wayland-continue.sh

        cat > /etc/systemd/system/clouddeploy-wayland-continue.service <<EOF
[Unit]
Description=Continue CloudDeploy Wayland after EDID reboot
After=network-online.target
Wants=network-online.target

[Service]
Type=oneshot
ExecStart=/usr/local/sbin/clouddeploy-wayland-continue.sh

[Install]
WantedBy=multi-user.target
EOF

        systemctl daemon-reload
        systemctl enable clouddeploy-wayland-continue.service
}

ensure_phase2_kernel_args() {
        local edid_file="$1"
        local arg_edid="drm.edid_firmware=${FORCED_CONNECTOR}:edid/${edid_file}"
        local arg_video="video=${FORCED_CONNECTOR}:e"
        local arg_modeset="nvidia-drm.modeset=1"
        local arg_fbdev="nvidia-drm.fbdev=1"

        if grep -qF "${arg_edid}" /proc/cmdline \
                && grep -qF "${arg_video}" /proc/cmdline \
                && grep -qF "${arg_modeset}" /proc/cmdline \
                && grep -qF "${arg_fbdev}" /proc/cmdline; then
                log "Kernel cmdline already contains required EDID and DRM args"
                return 0
        fi

        if [[ "${CLOUDDEPLOY_CONTINUE:-0}" == "1" ]]; then
                die "Kernel cmdline is still missing required EDID/DRM args after reboot"
        fi

        log "Applying GRUB kernel args for ${FORCED_CONNECTOR} using ${edid_file}"
        local current
        current="$(sed -n 's/^GRUB_CMDLINE_LINUX_DEFAULT="\(.*\)"$/\1/p' /etc/default/grub | head -n1 || true)"

        for arg in "${arg_edid}" "${arg_video}" "${arg_modeset}" "${arg_fbdev}"; do
                if [[ " ${current} " != *" ${arg} "* ]]; then
                        current="${current} ${arg}"
                fi
        done
        current="$(echo "${current}" | sed -E 's/^[[:space:]]+//; s/[[:space:]]+$//; s/[[:space:]]+/ /g')"

        if grep -q '^GRUB_CMDLINE_LINUX_DEFAULT=' /etc/default/grub; then
                sed -i "s|^GRUB_CMDLINE_LINUX_DEFAULT=.*|GRUB_CMDLINE_LINUX_DEFAULT=\"${current}\"|" /etc/default/grub
        else
                echo "GRUB_CMDLINE_LINUX_DEFAULT=\"${current}\"" >> /etc/default/grub
        fi

        update-grub
        install_continuation_service
        touch /opt/clouddeploy-wayland.needs-reboot

        log "Rebooting to apply EDID and DRM kernel arguments"
        reboot
        exit 0
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
                cloud-init status --wait || true
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

# =========================
# Start
# =========================
require_root

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
        pipewire wireplumber xdg-desktop-portal xdg-desktop-portal-kde gamescope \
        ubuntu-drivers-common

log "Checking NVIDIA driver status"
if nvidia_driver_ready; then
        log "NVIDIA drivers are already installed and working"
else
        log "Installing NVIDIA drivers"
        ubuntu-drivers install || die "Failed to install NVIDIA drivers"
        modprobe nvidia || true
        modprobe nvidia_modeset || true
        modprobe nvidia_drm || true

        for _ in $(seq 1 15); do
                if nvidia_driver_ready; then
                        log "NVIDIA drivers are now working"
                        break
                fi
                echo "Waiting for NVIDIA drivers to be ready..."
                sleep 3
        done

        nvidia_driver_ready || die "NVIDIA drivers still not ready after installation. This VM may need a reboot or may not be compatible."
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

        systemctl enable --now tailscaled
        tailscale up --authkey="${TAILSCALE_AUTHKEY}" --ssh
fi

log "Detecting NVIDIA BusID"
NVIDIA_BUSID="${NVIDIA_BUSID:-$(detect_nvidia_busid || true)}"
[[ -n "${NVIDIA_BUSID}" ]] || die "Could not detect NVIDIA BusID."

log "Detecting NVIDIA DRM card node"
if [[ "${SUNSHINE_DRM_DEVICE}" == "auto" ]]; then
        SUNSHINE_DRM_DEVICE="$(detect_nvidia_drm_card || true)"
fi
[[ -n "${SUNSHINE_DRM_DEVICE}" ]] || die "Could not detect NVIDIA DRM card node"
log "Using Sunshine DRM device: ${SUNSHINE_DRM_DEVICE}"

log "Writing Phase 2 EDID profiles"
write_phase2_edids

SELECTED_EDID_FILE="$(select_phase2_edid_file)"
[[ -n "${SELECTED_EDID_FILE}" ]] || die "Could not determine EDID profile file"
log "Selected EDID profile: ${SELECTED_EDID_FILE} on ${FORCED_CONNECTOR}"

ensure_phase2_kernel_args "${SELECTED_EDID_FILE}"

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
export XDG_RUNTIME_DIR="/tmp/runtime-${HEADLESS_USER}"
export XDG_SESSION_TYPE=wayland
export WAYLAND_DISPLAY="${WAYLAND_DISPLAY_NAME}"
export __EGL_VENDOR_LIBRARY_FILENAMES=/usr/share/glvnd/egl_vendor.d/10_nvidia.json
export __GLX_VENDOR_LIBRARY_NAME=nvidia

mkdir -p "\$XDG_RUNTIME_DIR" "${HOME_DIR}/.local/share" "${HOME_DIR}/.config/weston"
chmod 700 "\$XDG_RUNTIME_DIR"

exec /usr/bin/weston \
        --backend=drm-backend.so \
        --drm-device="${SUNSHINE_DRM_DEVICE}" \
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
export XDG_RUNTIME_DIR="/tmp/runtime-${HEADLESS_USER}"
export WAYLAND_DISPLAY="${WAYLAND_DISPLAY_NAME}"

mkdir -p "\$XDG_RUNTIME_DIR"
chmod 700 "\$XDG_RUNTIME_DIR"

SOCKET_PATH="\${XDG_RUNTIME_DIR}/${WAYLAND_DISPLAY_NAME}"

for _ in \$(seq 1 90); do
        if [[ -S "\${SOCKET_PATH}" ]] && pgrep -u "${HEADLESS_USER}" -x weston >/dev/null 2>&1; then
                exec /usr/bin/sunshine
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
export XDG_RUNTIME_DIR="/tmp/runtime-${HEADLESS_USER}"
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
echo "=== /dev/dri ==="
ls -l /dev/dri || true

echo
echo "=== Connector status (${FORCED_CONNECTOR}) ==="
cat /sys/class/drm/card*-${FORCED_CONNECTOR}/status 2>/dev/null || true

echo
echo "=== Connector modes (${FORCED_CONNECTOR}) ==="
cat /sys/class/drm/card*-${FORCED_CONNECTOR}/modes 2>/dev/null || true

if command -v edid-decode >/dev/null 2>&1; then
        echo
        echo "=== EDID decode (${FORCED_CONNECTOR}) ==="
        for edid_path in /sys/class/drm/card*-${FORCED_CONNECTOR}/edid; do
                [[ -f "\${edid_path}" ]] || continue
                echo "-- \${edid_path} --"
                edid-decode "\${edid_path}" || true
        done
fi

echo
echo "=== Service status ==="
systemctl --no-pager --full status weston-kms-session.service sunshine-headless.service gamescope-hdr-test.service || true

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
if [[ "${ENABLE_HEVC}" == "1" && "${ENABLE_HDR}" == "1" ]]; then
        HEVC_MODE="3"
else
        HEVC_MODE="1"
fi

if [[ "${ENABLE_AV1}" == "1" && "${ENABLE_HDR}" == "1" ]]; then
        AV1_MODE="3"
else
        AV1_MODE="1"
fi

cat > "${HOME_DIR}/.config/sunshine/sunshine.conf" <<EOF
min_log_level = debug
encoder = ${SUNSHINE_ENCODER}
capture = kms
output_name = ${FORCED_CONNECTOR}
adapter_name = ${SUNSHINE_DRM_DEVICE}
hevc_mode = ${HEVC_MODE}
av1_mode = ${AV1_MODE}
hdr = ${ENABLE_HDR}
fps = [60, ${TARGET_FPS}]
resolutions = [1920x1080, 2560x1440, ${TARGET_WIDTH}x${TARGET_HEIGHT}]
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
WorkingDirectory=${HOME_DIR}
Environment=HOME=${HOME_DIR}
Environment=USER=${HEADLESS_USER}
Environment=LOGNAME=${HEADLESS_USER}
Environment=XDG_RUNTIME_DIR=/tmp/runtime-${HEADLESS_USER}
Environment=WAYLAND_DISPLAY=${WAYLAND_DISPLAY_NAME}
PermissionsStartOnly=true
ExecStartPre=/usr/bin/mkdir -p /tmp/runtime-${HEADLESS_USER}
ExecStartPre=/usr/bin/chown ${HEADLESS_USER}:${HEADLESS_USER} /tmp/runtime-${HEADLESS_USER}
ExecStartPre=/usr/bin/chmod 700 /tmp/runtime-${HEADLESS_USER}
ExecStartPre=/usr/bin/mkdir -p ${HOME_DIR}/.local/share ${HOME_DIR}/.config ${HOME_DIR}/.config/weston ${HOME_DIR}/.local/bin
ExecStartPre=/usr/bin/chown -R ${HEADLESS_USER}:${HEADLESS_USER} ${HOME_DIR}/.local ${HOME_DIR}/.config
ExecStartPre=/usr/bin/bash -lc 'for i in \$(seq 1 15); do nvidia-smi >/dev/null 2>&1 && exit 0; sleep 2; done; exit 1'
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
Environment=XDG_RUNTIME_DIR=/tmp/runtime-${HEADLESS_USER}
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
Environment=XDG_RUNTIME_DIR=/tmp/runtime-${HEADLESS_USER}
Environment=WAYLAND_DISPLAY=${WAYLAND_DISPLAY_NAME}
ExecStartPre=/usr/bin/mkdir -p /tmp/runtime-${HEADLESS_USER}
ExecStartPre=/usr/bin/chown ${HEADLESS_USER}:${HEADLESS_USER} /tmp/runtime-${HEADLESS_USER}
ExecStartPre=/usr/bin/chmod 700 /tmp/runtime-${HEADLESS_USER}
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

log "Optional desktop apps"
if [[ "${INSTALL_OPTIONAL_APPS}" == "1" ]]; then
        if ! dpkg --print-foreign-architectures | grep -q i386; then
                wait_for_apt
                dpkg --add-architecture i386
                apt_update_retry
        fi

        wait_for_apt
        apt-get upgrade -y
        apt_install_wait flatpak steam-installer wine64 winetricks
        flatpak remote-add --if-not-exists flathub https://flathub.org/repo/flathub.flatpakrepo || true
        flatpak install -y flathub com.heroicgameslauncher.hgl || true
        flatpak install -y flathub net.lutris.Lutris || true
        flatpak install -y flathub com.usebottles.bottles || true
        flatpak install -y flathub org.prismlauncher.PrismLauncher || true

        tmpchrome="/tmp/google-chrome-stable_current_amd64.deb"
        wget -O "${tmpchrome}" https://dl.google.com/linux/direct/google-chrome-stable_current_amd64.deb
        dpkg -i "${tmpchrome}" || apt-get -f install -y
        rm -f "${tmpchrome}"
fi

log "Enabling services"
systemctl daemon-reload
systemctl enable weston-kms-session.service sunshine-headless.service tailscaled
systemctl disable gamescope-hdr-test.service 2>/dev/null || true

systemctl restart tailscaled || true
systemctl restart weston-kms-session.service
sleep 5
systemctl restart sunshine-headless.service
sleep 3

systemctl is-active --quiet weston-kms-session.service || die "weston-kms-session.service failed to start"
systemctl is-active --quiet sunshine-headless.service || die "sunshine-headless.service failed to start"
systemctl is-active --quiet tailscaled || die "tailscaled failed to start"

rm -f /opt/clouddeploy-wayland.needs-reboot || true
systemctl disable clouddeploy-wayland-continue.service >/dev/null 2>&1 || true

log "Finished"
echo
echo "Use Moonlight against the Tailscale IP, not the public IP."
if command -v tailscale >/dev/null 2>&1; then
        TS_IP="$(tailscale ip -4 2>/dev/null | head -n1 || true)"

        if [[ -n "${TS_IP}" ]]; then
                echo "Sunshine web UI: https://${TS_IP}:47990"
                echo "Moonlight host:   ${TS_IP}"
        fi
fi

echo "$SCRIPT_VERSION" > "$SENTINEL"
echo "Sunshine web UI username: ${SUNSHINE_USER}"
echo "Sunshine web UI password: ${SUNSHINE_PASS}"
echo
echo "If Moonlight shows a PIN, enter it in Sunshine's PIN tab."
echo "Do NOT inject sunshine_state.json pairings in the deploy script."
