#!/bin/bash

set -Eeuo pipefail
export DEBIAN_FRONTEND=noninteractive

HEADLESS_USER="${HEADLESS_USER:-ubuntu}"
SUNSHINE_USER="${SUNSHINE_USER:-aedyn}"
SUNSHINE_PASS="${SUNSHINE_PASS:-Aedyn11107@13}"
TAILSCALE_AUTHKEY="${TAILSCALE_AUTHKEY:-tskey-auth-kNatGervUa11CNTRL-jKpWxbykvf7hF6btz5dXg7EuZeMdTToD}"
SUNSHINE_DEB_URL="${SUNSHINE_DEB_URL:-https://github.com/LizardByte/Sunshine/releases/download/v2025.924.154138/sunshine-ubuntu-24.04-amd64.deb}"
CPU_GATE_EXIT="${CPU_GATE_EXIT:-42}"
CPU_BENCH_SECONDS="${CPU_BENCH_SECONDS:-1.5}"
CPU_BENCH_MIN="${CPU_BENCH_MIN:-33000000}"

NVIDIA_DISPLAY_DEVICE="${NVIDIA_DISPLAY_DEVICE:-DFP-0}"
HEADLESS_RESOLUTION="${HEADLESS_RESOLUTION:-1920x1200}"
SUNSHINE_RENDER_NODE="${SUNSHINE_RENDER_NODE:-/dev/dri/renderD128}"

# =========================
# Helpers
# =========================
log() {
        printf '\n[%s] %s\n' "$(date '+%F %T')" "$*"
}

die() {
        log "ERROR: $*"
        exit 1
}

require_root() {
        [[ "${EUID}" -eq 0 ]] || die "Run this script as root."
}

user_home() {
        getent passwd "$1" | cut -d: -f6
}

run_as_user() {
        local user="$1"
        shift
        runuser -u "${user}" -- "$@"
}

echo "Running preliminary hardware diagnostics..."
command -v python3 >/dev/null 2>&1 || die "Python 3 is required for CPU gate benchmarking."

cpu_score="$(python3 - "$CPU_BENCH_SECONDS" << 'PY'
import sys, time, statistics

duration = float(sys.argv[1])
scores = []

for _ in range(3):
        end = time.perf_counter() + duration
        x = 1
        n = 0
        while time.perf_counter() < end:
                x = (x * 1664525 + 1013904223) & 0xFFFFFFFF
                n += 1
        scores.append(n)

print(int(statistics.median(scores)))
PY
)"

log "CPU single-threaded score: ${cpu_score}"

if [[ "${cpu_score}" -lt "${CPU_BENCH_MIN}" ]]; then
        log "Warning: CPU score ${cpu_score} is below the minimum threshold of ${PU_BENCH_MIN}."
        log "Sunshine may not perform well. Consider using a more powerful CPU or adjusting the CPU_BENCH_MIN threshold."
        log "Exiting with code ${CPU_GATE_EXIT}."
        exit "${CPU_GATE_EXIT}"
fi
log "CPU gate passed with a score of ${cpu_score} (threshold: ${PU_BENCH_MIN}). Proceeding with installation."

INSTALL_OPTIONAL_APPS="${INSTALL_OPTIONAL_APPS:-0}"

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
apt-get update
apt-get install -y \
        curl wget ca-certificates gnupg software-properties-common \
        dbus-x11 xinit x11-xserver-utils pciutils jq libcap2-bin \
        kde-plasma-desktop

log "Removing pieces that fought the working setup"
systemctl disable --now sddm 2>/dev/null || true
apt-get purge -y xserver-xorg-video-dummy 2>/dev/null || true
rm -f /etc/sddm.conf.d/autologin.conf
rm -f /etc/sddm.conf.d/zz-autologin.conf

log "Installing Sunshine"
if ! command -v sunshine >/dev/null 2>&1; then
        tmpdeb="$(mktemp /tmp/sunshine.XXXXXX.deb)"
        wget -O "${tmpdeb}" "${SUNSHINE_DEB_URL}"
        dpkg -i "${tmpdeb}" || apt-get -f install -y
        rm -f "${tmpdeb}"
fi

log "Installing Tailscale if requested"
if [[ -n "${TAILSCALE_AUTHKEY}" ]]; then
        if ! command -v tailscale >/dev/null 2>&1; then
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

Section "ServerLayout"
        Identifier                              "Layout0"
        Screen 0                                "Screen0"
EndSection

Section "Monitor"
        Identifier                              "Monitor0"
        Option "Enable"                         "true"
EndSection

Section "Device"
        Identifier                              "NvidiaCard"
        Driver                                  "nvidia"
        BusID                                   "${NVIDIA_BUSID}"
        Option "PrimaryGPU"                     "yes"
        Option "AllowEmptyInitialConfiguration" "True"
        Option "UseDisplayDevice"               "${NVIDIA_DISPLAY_DEVICE}"
        Option "ConnectedMonitor"               "${NVIDIA_DISPLAY_DEVICE}"
        Option "MetaModes"                      "${HEADLESS_RESOLUTION}"
        Option "ModeValidation"                 "NoDFPNativeResolutionCheck,NoVirtualSizeCheck,NoMaxPClkCheck,NoHorizSyncCheck,NoVertRefreshCheck,NoWidthAlignmentCheck"
EndSection

Section "Screen"
        Identifier                              "Screen0"
        Monitor                                 "Monitor0"
        Device                                  "NvidiaCard"
        DefaultDepth                            24
        Option "TwinView"                       "True"

        SubSection "Display"
                Depth   24
                Modes   "${HEADLESS_RESOLUTION}"
        EndSubSection
EndSection
EOF

log "Writing Plasma X11 session startup"
cat > "${HOME_DIR}/.xinitrc" <<EOF
export XDG_RUNTIME_DIR=/tmp/runtime-${HEADLESS_USER}
exec dbus-run-session startplasma-x11
EOF
chown "${HEADLESS_USER}:${HEADLESS_USER}" "${HOME_DIR}/.xinitrc"
chmod 0644 "${HOME_DIR}/.xinitrc"

log "Writing Sunshine config"
install -d -m 0755 -o "${HEADLESS_USER}" -g "${HEADLESS_USER}" "${HOME_DIR}/.config/sunshine"

cat > "${HOME_DIR}/.config/sunshine/sunshine.conf" <<EOF
encoder = nvenc
capture = x11
adapter_name = ${SUNSHINE_RENDER_NODE}
EOF

chown -R "${HEADLESS_USER}:${HEADLESS_USER}" "${HOME_DIR}/.config"

log "Removing stale Sunshine state to avoid broken pre-pairing"
if [[ -f "${HOME_DIR}/.config/sunshine/sunshine_state.json" ]]; then
        mv "${HOME_DIR}/.config/sunshine/sunshine_state.json" \
                "${HOME_DIR}/.config/sunshine/sunshine_state.json.bak.$(date +%s)"
fi

log "Removing Sunshine KMS capability so X11 capture is used"
if command -v setcap >/dev/null 2>&1 && command -v sunshine >/dev/null 2>&1; then
        setcap -r "$(readlink -f "$(command -v sunshine)")" || true
fi

log "Setting Sunshine web UI credentials"
run_as_user "${HEADLESS_USER}" env HOME="${HOME_DIR}" sunshine --creds "${SUNSHINE_USER}" "${SUNSHINE_PASS}"

log "Writing headless Plasma systemd service"
cat > /etc/systemd/system/headless-plasma.service <<EOF
[Unit]
Description=Headless X11 Plasma session on NVIDIA
After=network-online.target
Wants=network-online.target
Before=sunshine-headless.service

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
ExecStart=/usr/bin/startx -- :0 -auth /tmp/serverauth.sunshine
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
EOF

log "Writing Sunshine systemd service"
cat > /etc/systemd/system/sunshine-headless.service <<EOF
[Unit]
Description=Sunshine on headless NVIDIA X11
After=headless-plasma.service network-online.target tailscaled.service
Wants=headless-plasma.service network-online.target
Requires=headless-plasma.service

[Service]
User=${HEADLESS_USER}
Group=${HEADLESS_USER}
WorkingDirectory=${HOME_DIR}
Environment=HOME=${HOME_DIR}
Environment=DISPLAY=:0
Environment=XDG_RUNTIME_DIR=/tmp/runtime-${HEADLESS_USER}
Environment=XAUTHORITY=/tmp/serverauth.sunshine
ExecStartPre=/bin/bash -lc 'for i in {1..60}; do /usr/bin/xrandr --display :0 >/dev/null 2>&1 && exit 0; sleep 1; done; echo "X session never became ready" >&2; exit 1'
ExecStart=/usr/bin/sunshine
Restart=always
RestartSec=5

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
        dpkg --add-architecture i386
        apt-get-repository -y multiverse || true
        apt-get update
        apt-get install -y flatpak steam-installer
        flatpak remote-add --if-not-exists flathub https://flathub.org/repo/flathub.flatpakrepo || true
        flatpak install -y flathub com.heroicgameslauncher.hgl || true
        flatpak install -y flathub org.prismlauncher.PrismLauncher || true

        tmpchrome="/tmp/google-chrome-stable_current_amd64.deb"
        wget -O "${tmpchrome}" https://dl.google.com/linux/direct/google-chrome-stable_current_amd64.deb
        dpkg -i "${tmpchrome}" || apt-get -f install -y
        rm -f "${tmpchrome}"
fi

log "Enabling services"
systemctl daemon-reload
systemctl enable --now headless-plasma.service
systemctl enable --now sunshine-headless.service

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
echo "Sunshine web UI username: ${SUNSHINE_USER}"
echo "Sunshine web UI password: ${SUNSHINE_PASS}"
echo
echo "If Moonlight shows a PIN, enter it in Sunshine's PIN tab."
echo "Do NOT inject sunshine_state.json pairings in the deploy script."
