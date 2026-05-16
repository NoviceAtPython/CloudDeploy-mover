# CloudDeploy v3 targets Ubuntu Linux only

## Decision

CloudDeploy v3 is an Ubuntu-only deploy tool. It runs on, and produces
deploys for, **Ubuntu cloud VMs** (24.04 LTS and 25.10) with NVIDIA
GPUs, KDE/KWin Wayland on KMS, Sunshine, and AV1 10-bit HDR
streaming to Moonlight.

The legacy Windows-VM provisioning path stays in the repo for v2
callers but is **not** part of v3 development. v3 will not add
features, fixes, or tests for Windows hosts.

## Why

1. **The validated technical stack is Ubuntu.** The
   [`AV1 10-bit HDR` success state](final-hdr-success/KNOWN_GOOD_SUNSHINE_STATE.md)
   was reached on Ubuntu 25.10 with patched KWin Wayland on KMS. The
   non-trivial pieces (NVIDIA private DRM HDR props, patched KWin,
   Sunshine fork) all target Linux.
2. **Single-target keeps the deploy logic small.** A Go module that
   has to support Linux + Windows hosts ends up doing 2x the work
   for the same outcome. v3's contributors are best served by a
   focused codebase.
3. **The v2 Windows scripts (`main.py`, `CloudDeploy.ps1`) are not
   stable infrastructure.** They worked for one-off provisioning of a
   developer Windows host but were never validated end-to-end against
   the current target stack. Their continued presence makes the repo
   look multi-platform when in practice v3 is single-target.
4. **Cloud-VM provisioners cover the multi-OS case better than we do.**
   Terraform, Pulumi, hand-rolled cloud-init, etc. can stand up a
   fresh Ubuntu VM in any cloud. v3 takes over from there.

## What this means in practice

### Build / run

`clouddeployctl` is a Go binary built for `GOOS=linux`. It is run on
the Ubuntu VM that will host Sunshine. There is no v3 client running
on a Windows operator workstation.

For developing the Go module, `go build ./...` and `go test ./...`
still work on macOS and Windows (and CI runs on `ubuntu-24.04`). Some
tests that invoke Linux-only tools (`dpkg-query`, `apt-cache`,
`lspci`, `dmesg`) gracefully degrade to "no evidence" instead of
failing on a non-Linux host.

### Files retained but classified as legacy

These files stay in the repository so v2 deploys keep working. They
are **not** part of the v3 production path:

| File | Status |
| --- | --- |
| `CloudDeploy-wayland.sh` | v2 entrypoint. Reaches AV1 10-bit HDR on Ubuntu. v3 does not call it. |
| `CloudDeploy.ps1` | v2 Windows orchestration helper. **Not maintained for v3.** |
| `main.py` | v2 Python orchestration for the Windows path. **Not maintained for v3.** |
| `clouddeploy-run-rootless-systemd.sh` | v2 rootless variant. v3 uses its own systemd integration. |

`.gitattributes` marks all four as `linguist-vendored
linguist-generated linguist-detectable=false` so they don't pollute
GitHub's language stats. v3's docs reference them only when calling
out v2 fallback.

### File names you should NOT touch in v3 PRs

* `CloudDeploy-wayland.sh`
* `CloudDeploy.ps1`
* `main.py`
* `clouddeploy-run-rootless-systemd.sh`
* `docs/HDR-NVIDIA-PRIVATE.md`
* `docs/SUNSHINE-HDR-NEGOTIATION.md`
* `docs/final-hdr-success/KNOWN_GOOD_SUNSHINE_STATE.md`
* `patches/kwin-clouddeploy-nvidia-private-hdr.patch`
* `scripts/validate-hdr-drm-state.py`

These are part of the v2-validated stack. Changes ride on the `v2`
branch.

### When the Windows path might come back

If a future milestone needs to provision the VM **itself** (instead
of starting from a pre-provisioned Ubuntu VM), the right answer is:

1. Use a real infrastructure-as-code tool (Terraform, Pulumi, Bicep)
   to stand up the VM.
2. Have that IaC tool drop `bootstrap.sh` on the new VM and run it.
3. Bootstrap.sh installs Go, builds `clouddeployctl`, runs the
   deploy.

That keeps `clouddeployctl` itself Ubuntu-only and gives the
operator a real provisioning story without resurrecting `main.py`.

### Why v2 stays

The HDR success path on the live VM was reached with v2. v3 cannot
yet reproduce it end-to-end. Until v3 reaches Milestone 5, an
operator's safest deploy command remains:

```bash
sudo ENABLE_HDR=1 bash ./CloudDeploy-wayland.sh
```

That command continues to work on the `v2` branch. Once v3 has been
validated end-to-end on real hardware, v2 moves to `legacy/` and the
v3 path becomes the default.
