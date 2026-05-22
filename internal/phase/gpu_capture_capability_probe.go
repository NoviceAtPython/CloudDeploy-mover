package phase

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/NoviceAtPython/CloudDeploy-mover/internal/runner"
)

const GPUCaptureCapabilityProbeName = "gpu_capture_capability_probe"

const (
	BackendWaylandKMSNVENCHDR = "wayland_kms_nvenc_hdr"
	BackendX11NvFBCNVENC      = "x11_nvfbc_nvenc"
	BackendX11NVENCFallback   = "x11_nvenc_fallback"
	BackendNone               = "none"
)

type GPUCaptureCapabilityProbe struct {
	NVENCProbeFn           func(context.Context, *Deps, string, string) error
	SunshineStartupProbeFn func(context.Context, *Deps, string, string, string, string) (string, error)
}

func (GPUCaptureCapabilityProbe) Name() string { return GPUCaptureCapabilityProbeName }

func (p GPUCaptureCapabilityProbe) Run(ctx context.Context, deps *Deps) error {
	if shouldSkip(deps.State, GPUCaptureCapabilityProbeName) {
		logger(deps).Info("phase gpu-capture-capability-probe: already done; skipping")
		return nil
	}
	deps.State.MarkRunning(GPUCaptureCapabilityProbeName)
	_ = deps.PersistState()

	desk := deps.Profile.EffectiveDesktop()
	cfg := deps.Profile.EffectiveSunshine()
	uid := uidFromState(deps)
	drm := selectedDRMDevice(deps)
	if drm == "" {
		drm = "/dev/dri/card1"
	}
	render := RenderNodeForCard(drm)
	connector := "DP-1"
	resolution := "3840x2160"
	if deps.Profile != nil {
		if strings.TrimSpace(deps.Profile.Display.ForcedConnector) != "" {
			connector = strings.TrimSpace(deps.Profile.Display.ForcedConnector)
		}
		if strings.TrimSpace(deps.Profile.Display.Resolution) != "" {
			resolution = strings.TrimSpace(deps.Profile.Display.Resolution)
		}
	}
	details := map[string]any{
		"selected_backend":             BackendNone,
		"selected_drm_device":          drm,
		"selected_render_node":         render,
		"forced_connector":             connector,
		"target_resolution":            resolution,
		"x11_nvfbc_nvenc_available":    false,
		"x11_nvenc_fallback_available": false,
	}
	if deps.DryRun {
		details["selected_backend"] = BackendWaylandKMSNVENCHDR
		details["dry_run"] = true
		deps.State.MarkDone(GPUCaptureCapabilityProbeName, details)
		_ = deps.PersistState()
		return nil
	}

	kmsOK := probeKMSDisplayCapability(drm, connector, resolution, details)
	eglOK := probeNVIDIAEGLRuntime(ctx, deps, desk.User, uid, details)
	nvencFn := p.NVENCProbeFn
	if nvencFn == nil {
		nvencFn = probeFFmpegNVENCAsUser
	}
	h264OK := recordNVENCProbe(ctx, deps, details, nvencFn, desk.User, "h264")
	hevcOK := recordNVENCProbe(ctx, deps, details, nvencFn, desk.User, "hevc-main10")

	conf := sunshineConfigPath(deps)
	probeFn := p.SunshineStartupProbeFn
	if probeFn == nil {
		probeFn = probeSunshineStartupAsUser
	}
	sunshineOut, sunshineErr := probeFn(ctx, deps, desk.User, uid, cfg.InstallBin, conf)
	details["sunshine_startup_probe_excerpt"] = lastLines(sunshineOut, 40)
	details["sunshine_startup_probe_error"] = ""
	if sunshineErr != nil {
		details["sunshine_startup_probe_error"] = sunshineErr.Error()
	}
	interopFail, interopReason := classifySunshineKMSInteropFailure(sunshineOut, sunshineErr)
	details["sunshine_kms_egl_nvenc_interop_ok"] = !interopFail
	if interopReason != "" {
		details["sunshine_kms_egl_nvenc_interop_failure"] = interopReason
	}

	waylandOK := kmsOK && eglOK && h264OK && hevcOK && !interopFail
	nvfbcOK := false
	x11FallbackOK := false
	selected := selectCaptureBackend(backendProbeResult{WaylandKMSNVENCHDR: waylandOK, X11NvFBCNVENC: nvfbcOK, X11NVENCFallback: x11FallbackOK})
	details["wayland_kms_nvenc_hdr_available"] = waylandOK
	details["selected_backend"] = selected
	if selected == BackendWaylandKMSNVENCHDR {
		deps.State.MarkDone(GPUCaptureCapabilityProbeName, details)
		_ = deps.PersistState()
		return nil
	}

	// v3 does not yet have a production X11/NvFBC service chain. Keep the
	// selection explicit and fatal instead of letting the KMS service start and
	// later fail with a misleading encoder error. The details map records the
	// exact missing capability so the next implementation can choose NvFBC when
	// that probe exists.
	err := fmt.Errorf("no supported capture backend passed: wayland_kms_nvenc_hdr=%v (kms=%v egl=%v h264_nvenc=%v hevc_main10_nvenc=%v sunshine_interop=%v reason=%s); x11_nvfbc_nvenc is not available in v3 yet", waylandOK, kmsOK, eglOK, h264OK, hevcOK, !interopFail, interopReason)
	return failPhase(deps, GPUCaptureCapabilityProbeName, details, "capture backend capability probe failed", err, true)
}

func probeKMSDisplayCapability(drm, connector, resolution string, details map[string]any) bool {
	ok := true
	if _, err := os.Stat(drm); err != nil {
		details["kms_card_exists"] = false
		details["kms_card_error"] = err.Error()
		ok = false
	} else {
		details["kms_card_exists"] = true
	}
	render := RenderNodeForCard(drm)
	details["render_node_by_pci"] = render
	if render == "" {
		details["render_node_found"] = false
		ok = false
	} else if _, err := os.Stat(render); err != nil {
		details["render_node_found"] = false
		details["render_node_error"] = err.Error()
		ok = false
	} else {
		details["render_node_found"] = true
	}
	cardName := filepath.Base(drm)
	connDir := filepath.Join(defaultSysClassDRM, cardName+"-"+connector)
	status := readTrimmedFile(filepath.Join(connDir, "status"))
	modes := readTrimmedFile(filepath.Join(connDir, "modes"))
	details["kms_connector_status"] = status
	details["kms_connector_modes_excerpt"] = lastLines(modes, 8)
	if !strings.EqualFold(status, "connected") {
		ok = false
	}
	if resolution != "" && !strings.Contains(modes, resolution) {
		ok = false
	}
	details["kms_display_capable"] = ok
	return ok
}

func probeNVIDIAEGLRuntime(ctx context.Context, deps *Deps, user, uid string, details map[string]any) bool {
	ldconfig, _ := output(ctx, deps, "", []string{"bash", "-lc", "ldconfig -p 2>/dev/null | grep -E 'libEGL_nvidia\\.so\\.0|libnvidia-egl-(gbm|wayland)\\.so\\.1' || true"}, 10*time.Second, false)
	details["egl_ldconfig_excerpt"] = strings.TrimSpace(ldconfig)
	jsons, _ := output(ctx, deps, "", []string{"bash", "-lc", "grep -Rsl 'libEGL_nvidia.so' /usr/share/glvnd/egl_vendor.d/*.json 2>/dev/null || true"}, 10*time.Second, false)
	if strings.Contains(ldconfig, "libEGL_nvidia.so.0") && strings.TrimSpace(jsons) == "" {
		err := ensureNVIDIAEGLVendorJSON()
		details["egl_vendor_json_repaired"] = err == nil
		if err != nil {
			details["egl_vendor_json_repair_error"] = err.Error()
		} else {
			jsons, _ = output(ctx, deps, "", []string{"bash", "-lc", "grep -Rsl 'libEGL_nvidia.so' /usr/share/glvnd/egl_vendor.d/*.json 2>/dev/null || true"}, 10*time.Second, false)
		}
	}
	details["egl_vendor_json"] = strings.TrimSpace(jsons)
	eglo, err := outputAsDesktop(ctx, deps, user, uid, []string{"bash", "-lc", "command -v eglinfo >/dev/null 2>&1 && timeout 15s eglinfo --display gbm 2>&1 || true"}, 20*time.Second)
	details["eglinfo_gbm_excerpt"] = lastLines(eglo, 20)
	egloLower := strings.ToLower(eglo)
	ok := strings.Contains(ldconfig, "libEGL_nvidia.so.0") && strings.TrimSpace(jsons) != "" && strings.Contains(egloLower, "nvidia") && !strings.Contains(egloLower, "llvmpipe")
	details["egl_nvidia_runtime_ok"] = ok
	if err != nil {
		details["eglinfo_error"] = err.Error()
	}
	return ok
}

func ensureNVIDIAEGLVendorJSON() error {
	const dir = "/usr/share/glvnd/egl_vendor.d"
	const path = dir + "/10_nvidia.json"
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	content := []byte(`{
    "file_format_version" : "1.0.0",
    "ICD" : {
        "library_path" : "libEGL_nvidia.so.0"
    }
}
`)
	if err := os.WriteFile(path, content, 0o644); err != nil {
		return err
	}
	return nil
}
func recordNVENCProbe(ctx context.Context, deps *Deps, details map[string]any, fn func(context.Context, *Deps, string, string) error, user, codec string) bool {
	err := fn(ctx, deps, user, codec)
	key := "nvenc_" + strings.ReplaceAll(codec, "-", "_")
	details[key+"_ok"] = err == nil
	if err != nil {
		details[key+"_error"] = err.Error()
	}
	return err == nil
}

func probeFFmpegNVENCAsUser(ctx context.Context, deps *Deps, user, codec string) error {
	filter := "testsrc2=size=128x72:rate=1"
	encoder := "h264_nvenc"
	extra := []string{}
	if codec == "hevc-main10" {
		filter = "testsrc2=size=128x72:rate=1,format=p010le"
		encoder = "hevc_nvenc"
		extra = []string{"-profile:v", "main10", "-pix_fmt", "p010le"}
	}
	args := []string{"runuser", "-u", user, "--", "ffmpeg", "-hide_banner", "-nostdin", "-loglevel", "error", "-f", "lavfi", "-i", filter, "-frames:v", "1", "-an", "-c:v", encoder}
	args = append(args, extra...)
	args = append(args, "-f", "null", "-")
	res := deps.Runner.Exec(ctx, runner.CommandSpec{Argv: args, Sudo: true, Timeout: 45 * time.Second, LogFile: "-", DryRun: deps.DryRun})
	if res.Err != nil {
		return fmt.Errorf("ffmpeg %s smoke test: %w stderr=%s", codec, res.Err, lastLines(res.Stderr, 8))
	}
	return nil
}

func probeSunshineStartupAsUser(ctx context.Context, deps *Deps, user, uid, bin, conf string) (string, error) {
	if strings.TrimSpace(bin) == "" {
		bin = "/usr/local/bin/sunshine-clouddeploy"
	}
	if strings.TrimSpace(conf) == "" {
		conf = "/home/" + user + "/" + defaultSunshineConfRel
	}
	args := []string{
		"runuser", "-u", user, "--", "env",
		"HOME=/home/" + user,
		"USER=" + user,
		"LOGNAME=" + user,
		"XDG_RUNTIME_DIR=/run/user/" + uid,
		"WAYLAND_DISPLAY=wayland-0",
		"DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/" + uid + "/bus",
		"GBM_BACKEND=nvidia-drm",
		"EGL_PLATFORM=gbm",
		"__EGL_VENDOR_LIBRARY_FILENAMES=/usr/share/glvnd/egl_vendor.d/10_nvidia.json",
		"__EGL_EXTERNAL_PLATFORM_CONFIG_DIRS=/usr/share/egl/egl_external_platform.d",
		"__GLX_VENDOR_LIBRARY_NAME=nvidia",
		"LIBGL_ALWAYS_SOFTWARE=0",
		"CUDA_VISIBLE_DEVICES=0",
		"timeout", "25s", bin, conf,
	}
	res := deps.Runner.Exec(ctx, runner.CommandSpec{Argv: args, Sudo: true, Timeout: 35 * time.Second, LogFile: "-", DryRun: deps.DryRun})
	combined := strings.TrimSpace(res.Stdout + "\n" + res.Stderr)
	if res.Err != nil && res.ExitCode != 124 {
		return combined, fmt.Errorf("sunshine startup probe: %w", res.Err)
	}
	return combined, nil
}

func classifySunshineKMSInteropFailure(output string, err error) (bool, string) {
	lower := strings.ToLower(output)
	markers := []string{
		"couldn't open egl display",
		"couldn't initialize egl display",
		"encoder [nvenc] failed",
		"video failed to find working encoder",
		"fatal: unable to find display or encoder",
		"platform failed to initialize",
	}
	for _, marker := range markers {
		if strings.Contains(lower, marker) {
			return true, marker
		}
	}
	if err != nil {
		return true, err.Error()
	}
	return false, ""
}

type backendProbeResult struct {
	WaylandKMSNVENCHDR bool
	X11NvFBCNVENC      bool
	X11NVENCFallback   bool
}

func selectCaptureBackend(r backendProbeResult) string {
	switch {
	case r.WaylandKMSNVENCHDR:
		return BackendWaylandKMSNVENCHDR
	case r.X11NvFBCNVENC:
		return BackendX11NvFBCNVENC
	case r.X11NVENCFallback:
		return BackendX11NVENCFallback
	default:
		return BackendNone
	}
}
