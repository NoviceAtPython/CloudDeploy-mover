// Package edid owns the forced-EDID + GRUB cmdline part of the
// deploy. For v3.0 we keep the existing v2 EDID-generation script
// (helpers/write-edids.py / scripts/write-edids.py) and just wrap it
// in a typed Go interface so the EDID phase can call it and so tests
// can exercise the GRUB cmdline planner without touching disk.
//
// What this package does NOT do:
//   - It does not parse EDID binaries (drm_info / scripts/validate-
//     hdr-drm-state.py already do that).
//   - It does not invoke update-grub / update-initramfs directly.
//     The EDID phase invokes them via the runner; this package just
//     produces the data those tools need.
package edid

import (
	"fmt"
	"sort"
	"strings"
)

// UniversalSDR / UniversalHDR are the multi-mode EDID blobs that
// scripts/write-edids.py emits. Each advertises every supported mode
// (see SupportedResolutions x SupportedRefreshes), so a single deploy
// can run any supported resolution; the active mode is chosen at deploy
// time by the profile + clouddeploy-force-kwin-mode.sh. HDR-ness is the
// only axis that needs a distinct blob (the HDR one carries the CTA HDR
// static-metadata + BT.2020 colorimetry blocks).
const (
	UniversalSDR = "virtual-universal-sdr.bin"
	UniversalHDR = "virtual-universal-hdr.bin"
)

// supportedResolutions is the "WxH" set the universal EDIDs advertise.
// MUST stay in sync with scripts/write-edids.py TARGET_MODES.
var supportedResolutions = []string{
	"1280x720",
	"1920x1080",
	"1920x1200",
	"2560x1440",
	"3840x2160",
}

// supportedRefreshes is advertised for every supported resolution
// (the EDID exposes the full resolution x refresh cross-product at 60/120).
var supportedRefreshes = []int{60, 120}

// extraModes are high-refresh "WxH@refresh" modes advertised ONLY for the
// specific resolutions whose CVT-RB timing fits the EDID detailed-timing
// pixel-clock field (<= 655 MHz). 1440p240 (~985 MHz) and any 4K above 120
// are deliberately absent: they exceed that field, and 1440p / non-CTA
// resolutions have no VIC, so a forced EDID cannot express them (they need
// DisplayPort DSC / HDMI FRL). MUST stay in sync with the high-refresh
// entries in scripts/write-edids.py TARGET_MODES.
var extraModes = map[string]bool{
	"1920x1080@144": true,
	"1920x1080@240": true,
	"2560x1440@144": true,
}

// Profile names map to EDID filenames under /lib/firmware/edid/.
// These names match what scripts/write-edids.py writes. The universal
// blobs are the modern path; the legacy single-mode SKUs are still
// generated for backward compatibility with older cmdlines.
var profileToFilename = map[string]string{
	"virtual-universal-sdr": UniversalSDR,
	"virtual-universal-hdr": UniversalHDR,
	"virtual-1080p-sdr":     "virtual-1080p-sdr.bin",
	"virtual-4k60-sdr":      "virtual-4k60-sdr.bin",
	"virtual-4k120-sdr":     "virtual-4k120-sdr.bin",
	"virtual-4k120-hdr":     "virtual-4k120-hdr.bin",
}

// FilenameFor returns the EDID filename a given profile uses.
// Returns "" if the profile name isn't recognised.
func FilenameFor(profile string) string {
	return profileToFilename[profile]
}

// SupportedResolutions returns a copy of the "WxH" resolutions the
// universal EDIDs advertise. Used by config validation + diagnostics.
func SupportedResolutions() []string {
	out := make([]string, len(supportedResolutions))
	copy(out, supportedResolutions)
	return out
}

// SupportedRefreshes returns a copy of the advertised refresh rates.
func SupportedRefreshes() []int {
	out := make([]int, len(supportedRefreshes))
	copy(out, supportedRefreshes)
	return out
}

// IsSupportedResolution reports whether "WxH" (case/space-insensitive)
// is one of the universal EDID's advertised resolutions.
func IsSupportedResolution(resolution string) bool {
	res := strings.ToLower(strings.TrimSpace(resolution))
	for _, r := range supportedResolutions {
		if r == res {
			return true
		}
	}
	return false
}

// IsSupportedRefresh reports whether the refresh rate is advertised.
func IsSupportedRefresh(refresh int) bool {
	for _, r := range supportedRefreshes {
		if r == refresh {
			return true
		}
	}
	return false
}

// SupportsMode reports whether the (resolution, refresh) pair is one the
// universal EDIDs advertise: the 60/120 cross-product for every supported
// resolution, plus the specific high-refresh extraModes.
func SupportsMode(resolution string, refresh int) bool {
	if IsSupportedResolution(resolution) && IsSupportedRefresh(refresh) {
		return true
	}
	res := strings.ToLower(strings.TrimSpace(resolution))
	return extraModes[fmt.Sprintf("%s@%d", res, refresh)]
}

// SupportedModes returns every advertised "WxH@refresh" mode (the 60/120
// cross-product plus the high-refresh extras), sorted, for diagnostics and
// config-validation error messages.
func SupportedModes() []string {
	var out []string
	for _, res := range supportedResolutions {
		for _, r := range supportedRefreshes {
			out = append(out, fmt.Sprintf("%s@%d", res, r))
		}
	}
	for m := range extraModes {
		out = append(out, m)
	}
	sort.Strings(out)
	return out
}

// SelectFilename picks the EDID filename for the deploy profile's
// display config. Both HDR and SDR resolve to the universal multi-mode
// blob (HDR vs SDR variant); the specific resolution/refresh is enforced
// later by force-kwin-mode + drm_display_validate, not by the EDID file.
//
// Returns "" when the mode is unsupported (no forced EDID; the edid
// phase then skips and logs the unsupported config).
func SelectFilename(resolution string, refresh int, hdr bool) string {
	if !SupportsMode(resolution, refresh) {
		return ""
	}
	if hdr {
		return UniversalHDR
	}
	return UniversalSDR
}

// GrubArgs are the kernel cmdline tokens the deploy adds to
// GRUB_CMDLINE_LINUX_DEFAULT. The order is stable for diffing.
type GrubArgs struct {
	EDIDFirmware    string   // drm.edid_firmware=DP-1:edid/virtual-4k120-hdr.bin
	VideoEnable     string   // video=DP-1:e
	VideoDisableAll []string // video=DP-2:d, video=DP-3:d, etc.
	NvidiaModeset   string   // nvidia-drm.modeset=1
	NvidiaFbdev     string   // nvidia-drm.fbdev=1
}

// PlanGrubArgs assembles the cmdline tokens for the given connector
// + EDID filename. `disableConnectors` is the list of other connectors
// to disable (e.g. all DP-N where N != the forced connector).
//
// Pure function; takes no I/O. Tests exercise it directly.
func PlanGrubArgs(connector, edidFile string, disableConnectors []string) GrubArgs {
	out := GrubArgs{
		EDIDFirmware:  fmt.Sprintf("drm.edid_firmware=%s:edid/%s", connector, edidFile),
		VideoEnable:   fmt.Sprintf("video=%s:e", connector),
		NvidiaModeset: "nvidia-drm.modeset=1",
		NvidiaFbdev:   "nvidia-drm.fbdev=1",
	}
	// Sort for stable output.
	d := append([]string(nil), disableConnectors...)
	sort.Strings(d)
	for _, c := range d {
		if c == connector || c == "" {
			continue
		}
		out.VideoDisableAll = append(out.VideoDisableAll, fmt.Sprintf("video=%s:d", c))
	}
	return out
}

// Tokens returns the cmdline tokens in a deterministic order.
func (a GrubArgs) Tokens() []string {
	out := []string{a.EDIDFirmware, a.VideoEnable}
	out = append(out, a.VideoDisableAll...)
	out = append(out, a.NvidiaModeset, a.NvidiaFbdev)
	return out
}

// CmdlineFragment returns the tokens joined with a single space.
// Suitable for embedding inside `GRUB_CMDLINE_LINUX_DEFAULT="..."`.
func (a GrubArgs) CmdlineFragment() string {
	return strings.Join(a.Tokens(), " ")
}

// GrubDropIn renders the /etc/default/grub.d/<name>.cfg content.
// Stable across runs (no timestamps, no random IDs) so re-running
// the EDID phase is idempotent.
func GrubDropIn(a GrubArgs) string {
	return fmt.Sprintf(`# Generated by CloudDeploy v3 EDID phase.
# Adds forced-EDID + NVIDIA-DRM modeset kernel cmdline tokens. Do not
# edit by hand; re-run `+"`clouddeployctl phase edid`"+` to regenerate.
GRUB_CMDLINE_LINUX_DEFAULT="$GRUB_CMDLINE_LINUX_DEFAULT %s"
`, a.CmdlineFragment())
}

// CmdlinePresent returns true iff every token in a appears in the
// supplied /proc/cmdline-style text. Used by doctor and the EDID
// phase to decide whether a reboot is still needed.
func CmdlinePresent(cmdline string, a GrubArgs) bool {
	for _, tok := range a.Tokens() {
		if !strings.Contains(cmdline, tok) {
			return false
		}
	}
	return true
}
