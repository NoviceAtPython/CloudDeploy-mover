// Package sunshine owns Sunshine fork pin + build + dual-install +
// setcap + sunshine.conf generation + HDR env wiring.
//
// Milestone-4 scope. The state v3 must preserve verbatim:
//
//   - Pin to the profile-supplied Sunshine fork commit
//     (or the profile-supplied override).
//   - Source-build via the same cmake/ninja flow as v2.
//   - Install to BOTH /usr/local/bin/sunshine-clouddeploy and
//     /usr/local/bin/sunshine.
//   - setcap cap_sys_admin,cap_net_bind_service,cap_sys_nice+ep on
//     both binaries.
//   - Generate sunshine-headless.service with HDR env vars when the
//     profile sets ForceAV1HDR10 / SynthesizeHDR10Metadata.
//   - Do NOT write hdr=, fps=, or resolutions= to sunshine.conf.
//   - CSRF allowlist: localhost + 127.0.0.1 + Tailscale IP.
//
// See docs/final-hdr-success/KNOWN_GOOD_SUNSHINE_STATE.md for the
// authoritative success-state spec.
package sunshine

import (
	"fmt"
	"strconv"
	"strings"
)

// Install does the pin + build + dual-install + setcap + config
// write. NOT YET IMPLEMENTED.
func Install() error { return nil }

// NOTE (2026-07-29): CloudDeploy used to refuse to build Sunshine's CUDA
// capture module on Ubuntu 25.10+ because CUDA 13.0/13.1 headers
// redeclared rsqrt/rsqrtf incompatibly with glibc 2.41+, breaking nvcc
// detection during CMake configure. That rule is OBSOLETE: measured on
// Ubuntu 25.10 (glibc 2.42, gcc 15.2) with CUDA 13.3, compile+link of
// device code using rsqrtf and CMake's CUDA compiler detection both
// succeed. Blanket-disabling by OS version silently produced a
// capture-fallback binary (GPU -> RAM -> GPU, ~40fps at 4K120 HDR) on
// hosts that were perfectly capable.
//
// The durable protection is NOT an OS allowlist - it is refusing to
// silently ship a CUDA-less binary when the profile asked for CUDA.
// See shouldRetrySunshineWithoutCUDA in internal/phase/milestone5.go
// and Profile.EffectiveSunshine, which promotes cuda.mode=required into
// sunshine.enable_cuda=true so a failed CUDA configure fails the deploy
// instead of degrading it.

// ServerCodecModeSupport bits are the GameStream codec-advertisement
// flags Sunshine writes into /serverinfo. Keep these in one package so
// deploy validation and doctors decode the same way.
//
// The values mirror Sunshine/Moonlight's SCM_* constants:
//
//	H.264 baseline support is always bit 0, HEVC starts at 0x100,
//	HEVC Main10 is 0x200, AV1 Main8 is 0x10000, and AV1 Main10 is
//	0x20000. The 4:4:4 flags are useful diagnostics but are not part of
//	CloudDeploy's 4K120 HDR success gate.
const (
	SCMH264          = 0x00000001
	SCMHEVC          = 0x00000100
	SCMHEVCMain10    = 0x00000200
	SCMAV1Main8      = 0x00010000
	SCMAV1Main10     = 0x00020000
	SCMH264High8444  = 0x00040000
	SCMHEVCRExt8444  = 0x00080000
	SCMHEVCRExt10444 = 0x00100000
	SCMAV1High8444   = 0x00200000
	SCMAV1High10444  = 0x00400000
)

type CodecModeSupport struct {
	Raw           int
	H264          bool
	H264High8444  bool
	HEVC          bool
	HEVCRExt8444  bool
	HEVCMain10    bool
	HEVCRExt10444 bool
	AV1Main8      bool
	AV1High8444   bool
	AV1Main10     bool
	AV1High10444  bool
}

func DecodeCodecModeSupport(raw int) CodecModeSupport {
	return CodecModeSupport{
		Raw:           raw,
		H264:          raw&SCMH264 != 0,
		H264High8444:  raw&SCMH264High8444 != 0,
		HEVC:          raw&SCMHEVC != 0,
		HEVCRExt8444:  raw&SCMHEVCRExt8444 != 0,
		HEVCMain10:    raw&SCMHEVCMain10 != 0,
		HEVCRExt10444: raw&SCMHEVCRExt10444 != 0,
		AV1Main8:      raw&SCMAV1Main8 != 0,
		AV1High8444:   raw&SCMAV1High8444 != 0,
		AV1Main10:     raw&SCMAV1Main10 != 0,
		AV1High10444:  raw&SCMAV1High10444 != 0,
	}
}

func (s CodecModeSupport) Names() []string {
	var out []string
	add := func(ok bool, name string) {
		if ok {
			out = append(out, name)
		}
	}
	add(s.H264, "H264")
	add(s.H264High8444, "H264_HIGH8_444")
	add(s.HEVC, "HEVC")
	add(s.HEVCRExt8444, "HEVC_REXT8_444")
	add(s.HEVCMain10, "HEVC_MAIN10")
	add(s.HEVCRExt10444, "HEVC_REXT10_444")
	add(s.AV1Main8, "AV1_MAIN8")
	add(s.AV1High8444, "AV1_HIGH8_444")
	add(s.AV1Main10, "AV1_MAIN10")
	add(s.AV1High10444, "AV1_HIGH10_444")
	return out
}

func (s CodecModeSupport) String() string {
	names := s.Names()
	if len(names) == 0 {
		return fmt.Sprintf("raw=%d flags=(none)", s.Raw)
	}
	return fmt.Sprintf("raw=%d flags=%s", s.Raw, strings.Join(names, ","))
}

func ServerInfoValue(body, key string) string {
	if strings.TrimSpace(body) == "" || strings.TrimSpace(key) == "" {
		return ""
	}
	for _, pat := range []string{"<" + key + ">", key + "=\""} {
		idx := strings.Index(body, pat)
		if idx < 0 {
			continue
		}
		rest := body[idx+len(pat):]
		if strings.HasSuffix(pat, ">") {
			if end := strings.Index(rest, "</"+key+">"); end >= 0 {
				return strings.TrimSpace(rest[:end])
			}
		} else if end := strings.Index(rest, "\""); end >= 0 {
			return strings.TrimSpace(rest[:end])
		}
	}
	return ""
}

func ServerInfoInt(body, key string) (int, bool) {
	v := strings.TrimSpace(ServerInfoValue(body, key))
	if v == "" {
		return 0, false
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, false
	}
	return n, true
}
