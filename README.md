# CloudDeploy

CloudDeploy is an experimental Ubuntu cloud-gaming VM deployer for NVIDIA
GPUs. The current v3 path builds a headless KDE/KWin Wayland desktop,
patched Sunshine, Tailscale access, PipeWire 7.1 audio, Steam/Proton, and
optional desktop gaming apps for Moonlight streaming.

The proven target is specific: **4K, 120 Hz, HDR, AV1, Sunshine/Moonlight**
on an Ada/RTX 40-class NVIDIA GPU. This project is still beta. It was
developed by one person against specific Vast.ai hardware, and it may need
fixes on other hosts, images, client GPUs, or Moonlight clients.

## Current Status

Validated on a live Vast.ai RTX 4090 VM:

- 3840x2160 at 120 Hz
- HDR through the CloudDeploy KWin/Sunshine path
- AV1 10-bit HDR in Moonlight
- Sunshine starts as a systemd service and is enabled at boot
- 7.1 output audio through the persistent `clouddeploy-surround71` sink
- DualSense/PlayStation controller handling through Sunshine `gamepad=auto`
  and HIDRAW access
- Xbox controllers should be selected when the Moonlight client reports an
  Xbox controller

Known gaps:

- Microphone / voice input does not stream through Moonlight — neither
  Moonlight nor Sunshine implements a client-to-host mic path. You do not need
  one; see "Voice Chat And Microphone" below.
- This is not a general-purpose Linux desktop installer.
- Some cloud GPU hosts expose broken PCIe/MSI topology; `nvidia-smi` can work
  while NVENC hangs. Pick VM-capable hosts with sane GPU passthrough.
- RTX 30-series cards do not have AV1 NVENC. They can only offer HEVC for this
  class of HDR stream, and the tested laptop/client did not handle that path.
  Your client decoder hardware matters.

## Hardware Expectations

Recommended GPUs:

- RTX 4090 / 4080 / 4070 Ti / 4070
- L4
- L40 / L40S
- RTX 6000 Ada

Avoid RTX 30-series unless you know your Moonlight client supports the HEVC
HDR path you want. The main tested path here assumes an AV1-capable client
decoder and an AV1-capable NVIDIA encoder.

For Vast.ai, prefer:

- VM/KVM-capable offers, not plain Docker-only offers
- `vms_enabled=true`
- direct SSH port available
- high reliability
- datacenter hosts when possible
- Ada/40-series GPU under $1/hr when available

## What It Installs

Core stack:

- NVIDIA driver/tooling
- KDE Plasma/KWin Wayland session for headless capture
- patched KWin HDR path
- patched Sunshine fork
- Tailscale
- PipeWire/WirePlumber
- persistent 7.1 virtual audio sink
- Sunshine systemd service and watchdog
- PlayStation HIDRAW and Xbox/PlayStation controller permissions

Optional app stack:

- Steam
- Google Chrome
- Discord
- Heroic Games Launcher
- Lutris
- Bottles
- Prism Launcher
- ProtonUp-Qt
- Wine / Winetricks and 32-bit graphics libraries for Proton

## Host Machine Prerequisites

On your laptop or desktop:

- Git
- SSH client
- Moonlight
- Tailscale account and client
- Vast.ai account, if using Vast
- Python launcher on Windows (`py`) if using the Vast CLI
- Go only if you want to build `clouddeployctl` locally instead of on the VM

Install and authenticate the Vast CLI on Windows PowerShell:

```powershell
py -3 -m pip install --upgrade vastai
vastai --help
vastai set api-key <YOUR_VAST_API_KEY>
```

Create or upload an SSH key before renting a VM:

```powershell
if (!(Test-Path "$env:USERPROFILE\.ssh\id_ed25519.pub")) {
    ssh-keygen -t ed25519 -f "$env:USERPROFILE\.ssh\id_ed25519" -N ""
}
vastai create ssh-key "$env:USERPROFILE\.ssh\id_ed25519.pub"
```

## Quick Start On A Fresh VM

SSH into the Ubuntu VM as root, then create a root-owned secrets file:

```bash
install -d -m 0700 /etc/clouddeploy
install -m 0600 /dev/null /etc/clouddeploy/secrets.env
nano /etc/clouddeploy/secrets.env
```

Example `/etc/clouddeploy/secrets.env`:

```bash
SUNSHINE_USER=user
SUNSHINE_PASS=change-this-password
TAILSCALE_AUTHKEY=<YOUR_TAILSCALE_AUTHKEY>
```

Run the v3 bootstrap:

```bash
curl -fsSL https://raw.githubusercontent.com/NoviceAtPython/CloudDeploy-mover/v3/bootstrap.sh \
  | PROFILE=hdr-4k120-cuda-auto-unattended-optional CLOUDDEPLOY_UNATTENDED=1 bash
```

The recommended profile enables unattended reboot/resume and optional apps.
Reboots during deployment are expected.

If the repo is already cloned on the VM:

```bash
git clone -b v3 https://github.com/NoviceAtPython/CloudDeploy-mover.git
cd CloudDeploy-mover
PROFILE=hdr-4k120-cuda-auto-unattended-optional CLOUDDEPLOY_UNATTENDED=1 ./bootstrap.sh
```

## Display profiles & supported resolutions

The deploy drives a *virtual* display via a forced EDID, so resolution / refresh /
HDR are a **deploy-time** choice set by the profile's `display:` block. A single
universal EDID advertises every supported mode, so the in-VM KDE display settings
list real options too.

**Supported modes:** `1280x720`, `1920x1080`, `1920x1200`, `2560x1440`, `3840x2160`
— each at **60** and **120** Hz, in SDR or HDR. (4K@120 is the streaming flagship;
720p60 is the lightest target for constrained links.)

Ready-made profiles:

| Profile | Mode | HDR |
|---|---|---|
| `hdr-4k120` (+ `-cuda*` variants) | 3840x2160@120 | yes |
| `hdr-1440p120` | 2560x1440@120 | yes |
| `sdr-1440p120` | 2560x1440@120 | no |
| `sdr-1200p120` | 1920x1200@120 | no |
| `sdr-1080p120` | 1920x1080@120 | no |
| `sdr-720p60` | 1280x720@60 | no |
| `sdr-safe` | 1920x1080@60 (no forced EDID) | no |

To run a different mode, copy a profile and edit `display.resolution` /
`display.refresh` / `display.hdr` to any supported combination. SDR profiles skip
the patched-KWin HDR build entirely, so they deploy faster. Config validation
rejects unsupported resolutions/refreshes at load time with the supported list.

## After Deployment

Check service state:

```bash
systemctl status sunshine-headless.service --no-pager
systemctl is-enabled sunshine-headless.service
clouddeployctl state show
clouddeployctl doctor sunshine
```

Pairing is one command. It is not a live prompt at the end of the deploy
because the deploy reboots several times and finishes unattended in the
background — there is no terminal attached at the moment it completes. So when
it finishes it prints your Tailscale IP and the single command that does the
pairing, and that same hint is shown on every SSH login:

```bash
sudo clouddeployctl pair
```

That command is the in-terminal pairing step: it shows the Tailscale IP to add
in Moonlight, waits for the 4-digit PIN Moonlight displays, and submits it to
Sunshine for you (credentials are read from `/etc/clouddeploy/secrets.env`). No
raw bash needed. If you prefer, the Sunshine web UI on
`https://<tailscale-ip>:47990` still works too.

Steam should be launched from the desktop icon or application menu so the
CloudDeploy PlayStation HIDRAW environment is applied. If Steam was already
running before a controller fix, fully exit Steam and relaunch it.

## Audio And Controller Notes

Audio output is routed through a persistent 7.1 PipeWire sink named
`clouddeploy-surround71`. Sunshine is pinned to capture that sink so rebooting
or restarting Sunshine does not revert to the silent default.

Controller support is intentionally metadata-driven:

- PlayStation/DualSense clients should be exposed as a virtual DualSense.
- Xbox clients should be exposed as Xbox.
- Third-party or ambiguous controllers may fall back to Xbox-style handling.

The important regression guard is `/dev/hidraw*` access. Without it,
Steam/SDL/Proton silently fall back to evdev-only PlayStation handling, which
can cause duplicated or scrambled input.

## Voice Chat And Microphone

There is no microphone / voice-input path into the VM, and you do not need one.
Neither Moonlight nor Sunshine implements client-to-host mic capture (verified
against current upstream Sunshine), so your headset mic does not travel to the
VM — and routing it there would be the wrong design anyway.

Run your voice app (Discord, etc.) on your **local** machine with your headset,
and run only the game on the VM:

- Game audio streams to you through Moonlight via the `clouddeploy-surround71`
  7.1 sink — full quality.
- Your voice goes straight from your headset into Discord and out to your
  friends — one hop, no extra encode.

Pushing the mic onto the VM would add a second network hop and a second Opus
transcode, so keeping voice local is both simpler and higher quality. You hear
game audio and friends mixed together on your PC exactly like a normal local
gaming session. (Discord is still in the optional VM app list for convenience,
but for voice chat, local is the better path.)

The only case this does not cover is a game with its own in-game voice chat
that captures from the system mic *on the VM*. That has no supported path today.

## Useful Commands

Resume after a manual reboot:

```bash
clouddeployctl resume --profile hdr-4k120-cuda-auto-unattended-optional
```

Collect logs for debugging:

```bash
clouddeployctl collect-logs
```

Restart Sunshine:

```bash
systemctl restart sunshine-headless.service
```

Reset Sunshine credentials:

```bash
SUNSHINE_USER=user SUNSHINE_PASS=new-password \
  clouddeployctl sunshine reset-credentials --profile hdr-4k120-cuda-auto-unattended-optional
systemctl restart sunshine-headless.service
```

## Repository Layout

- `cmd/clouddeployctl/` - CLI entrypoint
- `internal/phase/` - deployment phases
- `config/profiles/` - deploy profiles
- `patches/` - KWin patch assets
- `docs/` - engineering notes, compatibility notes, and validation history
- `bootstrap.sh` - VM-side bootstrap wrapper
- `CloudDeploy-wayland.sh` - older monolithic v2 script kept for history/fallback

## License

MIT License. See [`LICENSE`](LICENSE).

## Beta Notice

This project is public so other people can inspect, learn from, and improve
the work. It is not a polished commercial product. Expect rough edges, and
verify the VM, GPU, client decoder, audio, controller, and network path before
depending on it for a long gaming session.
