# GPU compatibility matrix

CloudDeploy v3's `internal/nvidia` package selects an NVIDIA driver
package family for whatever GPU the deploy host happens to have. The
selector is **evidence-driven**, not table-driven:

1. Walk `lspci -nn` and `dmesg` (and `/var/log/kern.log` as a fallback).
2. Compare against `apt-cache policy` to learn which package families
   are actually installable.
3. Run the decision tree in
   [`internal/nvidia/family.go`](../internal/nvidia/family.go).

The table below is therefore the **expected outcome**, not the input.
Tests in [`internal/nvidia/family_test.go`](../internal/nvidia/family_test.go)
and [`internal/nvidia/database_test.go`](../internal/nvidia/database_test.go)
lock most rows in place.

## Legend

* **Family** = the apt package family v3 prefers
  (`server` / `server-open` / `non-server` / `non-server-open`).
  When two values appear separated by `→`, the second is the fallback
  used when the first is not in apt sources.
* **HDR** = whether the v3 `hdr-4k120` profile is expected to reach
  the Moonlight `AV1 10-bit HDR` success state.
  `yes` = supported. `limited` = HDR via HEVC Main10 only (no AV1).
  `no` = not supported at all (NVENC missing or H.264-only).
* **AV1 encode** = does NVENC on this GPU have an AV1 encoder.
* **CUDA** = does the v3 streaming path need the CUDA toolkit on this
  GPU. Always `no` for the streaming path; only relevant if the host
  is doing something else (training, transcoding) on top.

## Consumer GeForce

| GPU family | Examples | Family | AV1 encode | HDR | Notes |
| --- | --- | --- | --- | --- | --- |
| Blackwell consumer (RTX 50) | RTX 5090 / 5090D / 5080 | `server-open` → `non-server-open` | yes | yes | **Closed kernel module unsupported.** Driver >= 580 required. Selector errors with `ErrNoOpenAvailable` if no `-open` package is in apt sources. |
| Ada (RTX 40) | RTX 4090 / 4070 Ti | either; default `server-open` | yes | yes | Both modules work. If `server-open` is already installed and `nvidia-smi` works, v3 keeps it (v2 regression test). |
| Ampere (RTX 30) | RTX 3090 / 3080 | either; default `server-open` | **no** | limited | NVENC has no AV1; HDR via HEVC Main10 only. v3 doctor warns. |
| Turing RTX (RTX 20) | RTX 2080 | either; default `server-open` | **no** | limited | Same NVENC profile as Ampere consumer. |
| Turing non-RTX (GTX 16) | GTX 1660 | either; default `server-open` | **no** | **no** | Some 16-series SKUs ship without NVENC at all (TU117). v3 doctor must check NVENC presence before allowing HDR profile. |

## Workstation / Professional

| GPU family | Examples | Family | AV1 encode | HDR | Notes |
| --- | --- | --- | --- | --- | --- |
| Blackwell professional | RTX PRO 6000 Blackwell | `server-open` → `non-server-open` | yes | yes | Closed module unsupported, same as consumer Blackwell. |
| Ada professional | RTX 6000 Ada, RTX 5000 Ada | either; default `server-open` | yes | yes | Both modules work. |
| Ampere A-series | RTX A6000 / A5000 / A4000 | either; default `server-open` | **no** | limited | Same NVENC profile as Ampere consumer. |

## Datacenter / cloud

| GPU family | Examples | Family | AV1 encode | HDR | Notes |
| --- | --- | --- | --- | --- | --- |
| Blackwell datacenter | B100 / B200 / GB200 | `server-open` → `non-server-open` | yes | yes | Closed module unsupported, same as the rest of the Blackwell line. |
| Hopper | H100 / H200 | `server` → `server-open` | yes | yes | Server family is the canonical NVIDIA recommendation. Open module also works. |
| Ada datacenter | L4 / L40 / L40S | `server` → `server-open` | yes | yes | The original v3 validated production target. |
| Ampere mid | A10 / A16 / A40 | `server` → `server-open` | **no** | limited | NVENC has no AV1; HDR via HEVC Main10 only. |
| Ampere flagship | A100 | `server` → `server-open` | **no** | limited | Compute-oriented; NVENC limited. |
| Turing datacenter | T4 | `server` → `server-open` | **no** | limited | Common cloud baseline. NVENC H.264 + HEVC. |
| Volta | V100 | `server` → `server-open` | **no** | **no** | NVENC H.264 only. v3 should refuse `hdr-4k120` here. |
| Pascal legacy | P100 / P40 / P4 | `server` | **no** | **no** | P100 has no NVENC at all. P40 / P4 are H.264 only. v3 refuses `hdr-4k120` on these. |

## How the selector decides

[`internal/nvidia/family.go`](../internal/nvidia/family.go) holds the
precedence rules. The high-level summary:

1. **`dmesg`** says `requires use of NVIDIA open kernel modules`?
   Open family, end of discussion. (`ErrNoOpenAvailable` if neither
   `server-open` nor `non-server-open` is in apt.)
2. **Blackwell GPU** of any flavour (consumer, pro, datacenter)?
   Same as 1: open family required.
3. **One family installed + `nvidia-smi` works?** Keep it. This is
   the v2 regression we explicitly do not repeat. (Unless 1 or 2
   triggered; in that case the kernel told us the installed closed
   module is doomed.)
4. **Data-center + `PreferServerFamily`?** Try `server`, fall back
   to `server-open` → `non-server` → `non-server-open` based on apt
   availability.
5. **`PreferOpenFamily` hint?** `server-open` → `non-server-open`.
6. **`PreferServerFamily` hint?** Same fallback chain as 4.
7. **No preference?** Default to `server-open` (the safest modern
   choice on any GPU NVIDIA supports today), then `server` →
   `non-server-open` → `non-server` based on availability.

If none of the four families is available in apt the selector returns
`ErrNoFamilyAvailable` with a reason string naming the driver major
that was attempted.

## Adding a new GPU

You usually do **not** need to add a new file under `config/gpus/`.
The name-regex layer in
[`internal/nvidia/database.go`](../internal/nvidia/database.go) is
broad enough to classify new SKUs in an existing generation
automatically. The selector will pick the right family from the
generation-level classification.

You **do** need to add a file when:

* The new GPU is in a generation that we don't have a regex pattern
  for yet (e.g. a future Rubin-class card).
* The GPU's PCI ID needs to be fingerprinted explicitly (e.g. a
  China-only SKU that doesn't match the standard name pattern).
* The GPU has unusual encode capabilities and the per-GPU
  `streaming.av1_encode` / `streaming.hdr` flags need to advertise
  the limitation.

When you do add a file:

1. Drop it under `config/gpus/<short-name>.yaml`.
2. Update the regex layer in
   `internal/nvidia/database.go` if the generation is new.
3. Add a test row in
   `internal/nvidia/database_test.go`.
4. Add the file's basename to the `want` list in
   `internal/config/config_test.go` (`TestKnownGPUCategoriesPresent`).
5. Add a row to this matrix.

## What is NOT in this matrix

* Mobile GPUs (laptops). CloudDeploy targets cloud VMs; we have no
  validated mobile-GPU deploy.
* AMD or Intel GPUs. Out of scope for v3.
* GRID profiles. Cloud providers expose GRID-licensed GPUs as A10G,
  T4-GRID, etc.; the kernel-driver question is the same as the
  underlying card.
