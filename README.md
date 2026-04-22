## This is an automated deployment script for spinning up a headless NVIDIA Linux cloud gaming VM with **KDE Plasma**, **X11**, **Sunshine**, and **Tailscale**.

This project is aimed at making fresh cloud instances usable in minutes instead of hours of manual setup and installs.

## What this project is

`CloudDeploy.sh` is a bootstrap/deployment script for Ubuntu-based cloud VMs that prepares a machine for remote desktop/game streaming with NVIDIA hardware.

The current focus is:

- headless NVIDIA X11 setup
- KDE Plasma desktop
- Sunshine for game streaming
- Tailscale for private remote access
- Desktop/gaming app installs
- Fast, repeatable setup on fresh cloud instances

## Current status

### Phase 1: Working baseline
The current script successfully automates a working baseline for:

- Ubuntu cloud VM
- NVIDIA GPU
- headless X11 session
- KDE Plasma desktop
- Sunshine startup
- Tailscale connectivity
- remote desktop access through Moonlight

This is the version that turned a long, failure-prone manual setup process into something quick.

### Phase 2: In progress
The next stage is focused on improving the streaming stack for modern gaming goals such as:

- AV1
- HDR
- higher refresh rates
- better 4K / 120 Hz behavior
- improved capture/encoding path for newer displays

## What the script does

Depending on configuration, the script can:

- install required system packages
- configure NVIDIA/Xorg for headless use
- create a Plasma X11 session startup path
- configure Sunshine
- configure and enable systemd services
- install and connect Tailscale
- install optional desktop applications and game launchers
- reduce the amount of manual post-deploy repair work

## Intended use case

This project is for users who want to launch a fresh cloud VM and quickly turn it into a remotely accessible Linux gaming/desktop machine.

Typical target scenario:

- rent a cloud GPU VM
- run `CloudDeploy.sh`
- connect over Tailscale
- log into Sunshine
- pair Moonlight
- use the machine as a remote gaming/desktop box

## Requirements

At minimum, you should expect to need:

- Ubuntu-based VM
- NVIDIA GPU
- sudo/root access
- internet access on the VM
- Moonlight on the client side
- a Tailscale account if using the private-network workflow

## Quick start

Clone the repo and run the script:

```bash
git clone <your-repo-url>
cd <your-repo-folder>
chmod +x CloudDeploy.sh
sudo bash ./CloudDeploy.sh
