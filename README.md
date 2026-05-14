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
```

## Rootless Cloud VM Recovery Launch

Some cloud images ship with the default user outside sudo, no root password,
and no `pkexec`, while polkit still permits `systemd-run`. If `sudo` says
`user is not in the sudoers file`, create a private env file instead of
pasting secrets into the shell command:

```bash
mkdir -p ~/.config/clouddeploy
chmod 700 ~/.config/clouddeploy
install -m 0600 /dev/null ~/.config/clouddeploy/env
editor ~/.config/clouddeploy/env
```

Example `~/.config/clouddeploy/env`:

```bash
SUNSHINE_PASS=your-sunshine-password
TAILSCALE_AUTHKEY=your-single-line-tailscale-auth-key
INSTALL_OPTIONAL_APPS=0
```

Then launch the deploy as a transient root service:

```bash
./clouddeploy-run-rootless-systemd.sh
```

Follow a failed transient run with:

```bash
journalctl -u clouddeploy-manual-rerun.service -n 300 --no-pager -l
```

`TAILSCALE_AUTHKEY` must be a single line. If a key was pasted with line
breaks during debugging, rotate it before rerunning.
