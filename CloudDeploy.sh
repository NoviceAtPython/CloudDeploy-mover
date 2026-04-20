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
NVIDIA_DISPLAY_DEVICE="${NVIDIA_DISPLAY_DEVICE:-DFP-0}"
HEADLESS_RESOLUTION="${HEADLESS_RESOLUTION:-1920x1200}"
SUNSHINE_RENDER_NODE="${SUNSHINE_RENDER_NODE:-/dev/dri/renderD128}"
SENTINEL="/opt/clouddeploy.installed"
SCRIPT_VERSION="3"

[[ $EUID -eq 0 ]] || { echo "Run this script using 'sudo' or as root"; exit 1; }

if [[ -f "$SENTINEL" ]] && [[ "$(cat "$SENTINEL")" == "$SCRIPT_VERSION" ]]; then
        echo "CloudDeploy.sh has already run on this machine for version $SCRIPT_VERSION. Restarting existing services and exiting..."
        systemctl daemon-reload || true
        systemctl reset-failed headless-plasma.service sunshine-headless.service tailscaled || true
        systemctl enable --now headless-plasma.service sunshine-headless.service tailscaled || true
        systemctl restart headless-plasma.service sunshine-headless.service tailscaled || true
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
        systemctl --no-pager --full status headless-plasma.service | sed -n '1,8p' || true
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
        dbus-x11 xinit x11-xserver-utils pciutils jq libcap2-bin \
        kde-plasma-desktop xserver-xorg xserver-xorg-legacy \
        ubuntu-drivers-common

log "Configuring Xorg wrapper permissions"
cat > /etc/X11/Xwrapper.config <<EOF
allowed_users=anybody
needs_root_rights=yes
EOF

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

echo "Configuring monitor with X11 driver..."
cat > /etc/X11/xorg.conf <<EOF

Section "Files"
        ModulePath                              "/usr/lib/x86_64-linux-gnu/nvidia/xorg"
        ModulePath                              "/usr/lib64/xorg/modules"
        ModulePath                              "/usr/lib/xorg/modules"
EndSection

Section "ServerLayout"
        Identifier                              "Layout0"
        Screen 0                                "Screen0"
EndSection

Section "Monitor"
        Identifier                              "Monitor0"
        HorizSync                               28.0-160.0
        VertRefresh                             48.0-144.0
EndSection

Section "Device"
        Identifier                              "NvidiaCard"
        Driver                                  "nvidia"
        BusID                                   "${NVIDIA_BUSID}"
        Option "PrimaryGPU"                     "yes"
        Option "AllowEmptyInitialConfiguration" "True"
        Option "ConnectedMonitor"               "${NVIDIA_DISPLAY_DEVICE}"
        Option "MetaModes"                      "${HEADLESS_RESOLUTION}"
        Option "UseDisplayDevice"               "${NVIDIA_DISPLAY_DEVICE}"
        Option "ModeValidation"                 "NoEdidModes, NoMaxPClkCheck, AllowNonEdidModes, NoHorizSyncCheck, NoVertRefreshCheck"
EndSection

Section "Screen"
        Identifier                              "Screen0"
        Monitor                                 "Monitor0"
        Device                                  "NvidiaCard"
        DefaultDepth                            24
        Option "TwinView"                       "True"
        Option "MetaModes"                      "${HEADLESS_RESOLUTION}"

        SubSection "Display"
                Depth   24
                Modes   "${HEADLESS_RESOLUTION}"
        EndSubSection
EndSection
EOF

log "Writing Plasma X11 session startup"
install -d -m 0755 -o "${HEADLESS_USER}" -g "${HEADLESS_USER}" \
    "${HOME_DIR}/.local/bin" \
    "${HOME_DIR}/.config/sunshine"

cat > "${HOME_DIR}/.xinitrc" <<EOF
#!/usr/bin/env bash
set -euo pipefail

export HOME="${HOME_DIR}"
export USER="${HEADLESS_USER}"
export LOGNAME="${HEADLESS_USER}"
export DISPLAY=:0
export XAUTHORITY=/tmp/serverauth.sunshine
export XDG_RUNTIME_DIR="/tmp/runtime-${HEADLESS_USER}"

mkdir -p "\$XDG_RUNTIME_DIR" "${HOME_DIR}/.local/share" "${HOME_DIR}/.config"
chmod 700 "\$XDG_RUNTIME_DIR"

exec dbus-run-session -- startplasma-x11
EOF

chown "${HEADLESS_USER}:${HEADLESS_USER}" "${HOME_DIR}/.xinitrc"
chmod 0755 "${HOME_DIR}/.xinitrc"

cat > "${HOME_DIR}/.local/bin/start-sunshine-headless.sh" <<EOF
#!/usr/bin/env bash
set -euo pipefail

export HOME="${HOME_DIR}"
export DISPLAY=:0
export XAUTHORITY=/tmp/serverauth.sunshine
export XDG_RUNTIME_DIR="/tmp/runtime-${HEADLESS_USER}"

mkdir -p "\$XDG_RUNTIME_DIR"
chmod 700 "\$XDG_RUNTIME_DIR"

for _ in $(seq 1 90); do
    if DISPLAY=:0 XAUTHORITY=/tmp/serverauth.sunshine xrandr --query >/dev/null 2>&1 \
       && pgrep -u "${HEADLESS_USER}" plasmashell >/dev/null 2>&1 \
       && pgrep -u "${HEADLESS_USER}" kwin_x11 >/dev/null 2>&1; then
        DISPLAY=:0 XAUTHORITY=/tmp/serverauth.sunshine xrandr --output HDMI-0 --mode "${HEADLESS_RESOLUTION}" || true
        exec /usr/bin/sunshine
    fi
    sleep 1
done

echo "X11 session never became ready for Sunshine" >&2
exit 1
EOF

chown "${HEADLESS_USER}:${HEADLESS_USER}" "${HOME_DIR}/.local/bin/start-sunshine-headless.sh"
chmod 0755 "${HOME_DIR}/.local/bin/start-sunshine-headless.sh"

log "Writing Sunshine config"
cat > "${HOME_DIR}/.config/sunshine/sunshine.conf" <<EOF
min_log_level = info
encoder = nvenc
capture = nvfbc
adapter_name = ${SUNSHINE_RENDER_NODE}
hevc_mode = 1
av1_mode = 1
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

log "Ensuring Sunshine has cap_sys_admin for NvFBC"
if command -v setcap >/dev/null 2>&1 && command -v sunshine >/dev/null 2>&1; then
        setcap cap_sys_admin+p "$(readlink -f "$(command -v sunshine)")" || true
fi

log "Setting Sunshine web UI credentials"
run_as_user "${HEADLESS_USER}" env HOME="${HOME_DIR}" sunshine --creds "${SUNSHINE_USER}" "${SUNSHINE_PASS}" || true

log "Writing headless Plasma systemd service"
cat > /etc/systemd/system/headless-plasma.service <<EOF
[Unit]
Description=Headless X11 Plasma session on NVIDIA
After=network-online.target
Wants=network-online.target

[Service]
User=${HEADLESS_USER}
Group=${HEADLESS_USER}
WorkingDirectory=${HOME_DIR}
Environment=HOME=${HOME_DIR}
Environment=DISPLAY=:0
Environment=XDG_RUNTIME_DIR=/tmp/runtime-${HEADLESS_USER}
Environment=XAUTHORITY=/tmp/serverauth.sunshine
PermissionsStartOnly=true
ExecStartPre=/usr/bin/mkdir -p /tmp/runtime-${HEADLESS_USER}
ExecStartPre=/usr/bin/chown ${HEADLESS_USER}:${HEADLESS_USER} /tmp/runtime-${HEADLESS_USER}
ExecStartPre=/usr/bin/chmod 700 /tmp/runtime-${HEADLESS_USER}
ExecStartPre=/usr/bin/rm -f /tmp/serverauth.sunshine ${HOME_DIR}/.Xauthority
ExecStart=/usr/bin/startx ${HOME_DIR}/.xinitrc -- :0 -auth /tmp/serverauth.sunshine
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
Description=Sunshine on headless NVIDIA X11
After=headless-plasma.service network-online.target tailscaled.service
Wants=network-online.target tailscaled.service
Requires=headless-plasma.service
PartOf=headless-plasma.service

[Service]
User=${HEADLESS_USER}
Group=${HEADLESS_USER}
WorkingDirectory=${HOME_DIR}
Environment=HOME=${HOME_DIR}
Environment=DISPLAY=:0
Environment=XDG_RUNTIME_DIR=/tmp/runtime-${HEADLESS_USER}
Environment=XAUTHORITY=/tmp/serverauth.sunshine
ExecStart=${HOME_DIR}/.local/bin/start-sunshine-headless.sh
Restart=always
RestartSec=5
StandardOutput=journal
StandardError=journal

[Install]
WantedBy=multi-user.target
EOF

log "Stopping any old broken session bits"
pkill -u "${HEADLESS_USER}" startplasma-x11 2>/dev/null || true
pkill -u "${HEADLESS_USER}" plasmashell 2>/dev/null || true
pkill -u "${HEADLESS_USER}" kwin_x11 2>/dev/null || true
pkill sunshine 2>/dev/null || true
pkill Xorg 2>/dev/null || true

log "Optional desktop apps"
if [[ "${INSTALL_OPTIONAL_APPS}" == "1" ]]; then
        if ! dpkg --print-foreign-architectures | grep -q i386; then
                wait_for_apt
                dpkg --add-architecture i386
                apt_update_retry
        fi

        wait_for_apt
        apt-get upgrade -y
        apt_install_wait flatpak steam-installer
        flatpak remote-add --if-not-exists flathub https://flathub.org/repo/flathub.flatpakrepo || true
        flatpak install -y flathub com.heroicgameslauncher.hgl || true
        flatpak install -y flathub org.prismlauncher.PrismLauncher || true

        tmpchrome="/tmp/google-chrome-stable_current_amd64.deb"
        wget -O "${tmpchrome}" https://dl.google.com/linux/direct/google-chrome-stable_current_amd64.deb
        dpkg -i "${tmpchrome}" || apt-get -f install -y
        rm -f "${tmpchrome}"
fi

log "Verifying NVIDIA Xorg module exists"
find /usr /usr/lib64 -type f -name 'nvidia_drv.so*' 2>/dev/null | grep -q . || die "NVIDIA Xorg driver module not found. NVIDIA drivers may not be installed correctly."

log "Enabling services"
systemctl daemon-reload
systemctl enable --now headless-plasma.service
systemctl restart headless-plasma.service
systemctl enable --now sunshine-headless.service
systemctl enable --now tailscaled
systemctl restart tailscaled

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
