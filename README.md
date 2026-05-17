# CloudDeploy

## Branches at a glance

| Branch | Status | What it deploys |
| --- | --- | --- |
| [`v2`](https://github.com/NoviceAtPython/CloudDeploy-mover/tree/v2) | **Production. Validated.** | `CloudDeploy-wayland.sh` monolith. Reaches Moonlight `AV1 10-bit HDR` on the live VM as of v2 commit `7d850e9`. Pinned Sunshine fork `464bccf1`. |
| [`v3`](https://github.com/NoviceAtPython/CloudDeploy-mover/tree/v3) | **Milestone 4 in progress — Ubuntu-only — NOT v2-equivalent.** | `clouddeployctl` Go orchestrator. Real: `doctor` + `phase ubuntu-upgrade / base-packages / nvidia-driver / cuda / edid` + `apply` / `resume` + `collect-logs`. Missing: KDE/KWin install + patched-KWin HDR build + Sunshine fork build + Tailscale + PipeWire + streaming/HDR validators. **See [`docs/V2-V3-PARITY.md`](docs/V2-V3-PARITY.md) for the full audit.** |

> **`clouddeployctl apply` is NOT a v2-equivalent deploy.** v3 today
> covers the bootstrap-blocker phases (ubuntu-upgrade, base-packages,
> nvidia-driver, cuda, edid, reboot continuation). It does **not** yet
> install Plasma 6 / KWin, build the patched-KWin NVIDIA private HDR
> path, build Sunshine, generate KWin/Plasma/Sunshine systemd units,
> wire Tailscale, install the PipeWire virtual sink, or run the
> streaming / HDR validators. The `apply` command prints a banner
> naming the missing phases when it finishes.
>
> For a full deploy that reaches Moonlight `AV1 10-bit HDR`, keep
> using v2:
>
> ```bash
> sudo ENABLE_HDR=1 bash ./CloudDeploy-wayland.sh
> ```
>
> v3 is **Ubuntu-only**; see
> [`docs/UBUNTU-ONLY.md`](docs/UBUNTU-ONLY.md).
> The full v2 → v3 capability audit is in
> [`docs/V2-V3-PARITY.md`](docs/V2-V3-PARITY.md). The remaining
> milestone work is in
> [`docs/V3-ROADMAP.md`](docs/V3-ROADMAP.md) and
> [`docs/V3-DEPLOYMENT-READINESS.md`](docs/V3-DEPLOYMENT-READINESS.md).

v3 design rationale (single-binary Go orchestrator, no Ansible) is in
[`docs/ADR-0001-orchestrator-language.md`](docs/ADR-0001-orchestrator-language.md)
and
[`docs/ADR-0002-ansible-vs-native-go.md`](docs/ADR-0002-ansible-vs-native-go.md).

GPU compatibility expectations (which families v3 prefers per
architecture, where AV1/HDR is supported and where it isn't) live in
[`docs/GPU-COMPATIBILITY.md`](docs/GPU-COMPATIBILITY.md).

---

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

## Recommended run command — do NOT pass secrets on the command line

`sudo env SUNSHINE_PASS=... TAILSCALE_AUTHKEY=... ./CloudDeploy-wayland.sh`
exposes secrets in `ps`. The script supports a root-owned, mode-600 env
file instead. On the target VM, as root:

```bash
install -d -m 0700 /root
install -m 0600 /dev/null /root/clouddeploy-v2.env
${EDITOR:-nano} /root/clouddeploy-v2.env
```

Example `/root/clouddeploy-v2.env`:

```bash
SUNSHINE_PASS=your-sunshine-password
TAILSCALE_AUTHKEY=your-single-line-tailscale-auth-key
ENABLE_HDR=1
CLOUDDEPLOY_AUTO_DIST_UPGRADE=1
CLOUDDEPLOY_ACCEPT_NON_LTS=1
INSTALL_OPTIONAL_APPS=0
```

Then run:

```bash
sudo bash ./CloudDeploy-wayland.sh
```

`CloudDeploy-wayland.sh` sources `/root/clouddeploy-v2.env` (or whatever
path you put in `CLOUDDEPLOY_USER_ENV_FILE`) before any default is
evaluated, but only if the file is owned by root and mode 600 or 400.
Files with looser permissions or non-root owners are skipped with a
warning.

A flock at `/run/clouddeploy-wayland.lock` refuses to start a second
CloudDeploy run while one is in progress, so accidentally invoking the
script twice won't corrupt apt/dpkg state.

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
