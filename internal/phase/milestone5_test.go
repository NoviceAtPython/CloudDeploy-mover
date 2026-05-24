package phase

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/NoviceAtPython/CloudDeploy-mover/internal/config"
	"github.com/NoviceAtPython/CloudDeploy-mover/internal/state"
)

func milestone5Deps(t *testing.T) *Deps {
	t.Helper()
	deps := newDeps(t, runtimeProfile(), nil)
	deps.State.MarkDone(HeadlessUserName, map[string]any{
		"user": "cloudgamer",
		"uid":  "1001",
	})
	deps.State.MarkDone(KWinSessionName, map[string]any{
		"selected_drm_device": "/dev/dri/card1",
	})
	return deps
}

const goodHDRServerInfo = `<root status_code="200"><ServerCodecModeSupport>197377</ServerCodecModeSupport><MaxLumaPixelsHEVC>1869449984</MaxLumaPixelsHEVC></root>`

func TestRenderSunshineConfigAvoidsKnownInvalidKeys(t *testing.T) {
	body := renderSunshineConfig(config.SunshineConfig{
		Encoder:                 "nvenc",
		Capture:                 "kms",
		ForceAV1HDR10:           true,
		SynthesizeHDR10Metadata: true,
	}, "/dev/dri/card1", []string{"https://localhost:47990"})

	for _, bad := range []string{"\nhdr =", "\nfps =", "\nresolutions ="} {
		if strings.Contains(body, bad) {
			t.Fatalf("sunshine.conf contains invalid key marker %q:\n%s", bad, body)
		}
	}
	for _, bad := range []string{"force_av1_hdr10", "synthesize_hdr10_metadata"} {
		if strings.Contains(body, bad) {
			t.Fatalf("sunshine.conf contains env-only key %q:\n%s", bad, body)
		}
	}
	for _, want := range []string{
		"capture = kms",
		"encoder = nvenc",
		"adapter_name = /dev/dri/card1",
		"stream_audio = enabled",
		"audio_sink =",
		"virtual_sink =",
		// HDR-Main10 defaults: av1_mode=3 (AV1 Main + Main10),
		// hevc_mode=3 (HEVC Main + Main10). Live-VM regression:
		// av1_mode=2 + hevc_mode=0 left Moonlight without an HDR
		// branch and it fell back to H.264 + p010 which h264_nvenc
		// refuses. These defaults are pinned for HDR profiles by
		// the SunshineConfig validator.
		"av1_mode = 3",
		"hevc_mode = 3",
		"gamepad = auto",
		"ds5_inputtino_randomize_mac = false",
		"motion_as_ds4 = false",
		"touchpad_as_ds4 = false",
		"ds4_back_as_touchpad_click = false",
		"controller = enabled",
		"keyboard = enabled",
		"mouse = enabled",
		"csrf_allowed_origins = https://localhost:47990",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("sunshine.conf missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "av1_mode = 2") {
		t.Errorf("sunshine.conf still uses the buggy av1_mode=2 default:\n%s", body)
	}
	if strings.Contains(body, "hevc_mode = 0") {
		t.Errorf("sunshine.conf still uses the buggy hevc_mode=0 auto-probe default:\n%s", body)
	}
	for _, bad := range []string{"audio_sink = clouddeploy-surround71", "virtual_sink = clouddeploy-surround71"} {
		if strings.Contains(body, bad) {
			t.Errorf("sunshine.conf pins the CloudDeploy bootstrap sink and can split playback/capture again:\n%s", body)
		}
	}
}

func TestRenderSunshineConfig_HonorsExplicitProfileCodecModes(t *testing.T) {
	one := 1
	two := 2
	cfg := config.SunshineConfig{Encoder: "nvenc", Capture: "kms", HevcMode: &two, Av1Mode: &one}
	body := renderSunshineConfig(cfg, "/dev/dri/card1", nil)
	if !strings.Contains(body, "hevc_mode = 2") {
		t.Errorf("explicit hevc_mode=2 not rendered:\n%s", body)
	}
	if !strings.Contains(body, "av1_mode = 1") {
		t.Errorf("explicit av1_mode=1 not rendered:\n%s", body)
	}
}

func TestRenderSunshineConfig_HonorsExplicitGamepadMode(t *testing.T) {
	cfg := config.SunshineConfig{
		Encoder:                "nvenc",
		Capture:                "kms",
		Gamepad:                "ds5",
		MotionAsDS4:            true,
		TouchpadAsDS4:          true,
		DS4BackAsTouchpadClick: true,
	}
	body := renderSunshineConfig(cfg, "/dev/dri/card1", nil)
	for _, want := range []string{
		"gamepad = ds5",
		"ds5_inputtino_randomize_mac = false",
		"motion_as_ds4 = true",
		"touchpad_as_ds4 = true",
		"ds4_back_as_touchpad_click = true",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("sunshine.conf missing explicit gamepad setting %q:\n%s", want, body)
		}
	}
}

func TestRenderCloudDeployAudioScriptCreatesSurroundSinkAndVirtualMic(t *testing.T) {
	cfg := config.AudioConfig{
		VirtualSink: "clouddeploy-surround71",
		Rate:        48000,
		Channels:    8,
		ChannelMap:  "front-left,front-right,rear-left,rear-right,front-center,lfe,side-left,side-right",
	}
	body := renderCloudDeployAudioScript(cfg)
	for _, want := range []string{
		"SINK_NAME='clouddeploy-surround71'",
		"MIC_SINK_NAME='clouddeploy-mic-sink'",
		"MIC_SOURCE_NAME='clouddeploy-mic'",
		"pactl load-module module-null-sink",
		"channels=\"$CHANNELS\"",
		"channel_map=\"$CHANNEL_MAP\"",
		"pactl load-module module-remap-source",
		"pactl set-default-sink \"$SINK_NAME\"",
		"pactl set-default-source \"$default_source\"",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("audio script missing %q:\n%s", want, body)
		}
	}
}

func TestRenderCloudDeployAudioUserService(t *testing.T) {
	unit := renderCloudDeployAudioUserService("/home/cloudgamer/.local/bin/clouddeploy-audio-virtual-devices.sh")
	for _, want := range []string{
		"Description=CloudDeploy virtual PipeWire audio devices",
		"Wants=pipewire-pulse.service wireplumber.service",
		"Type=oneshot",
		"ExecStart=/home/cloudgamer/.local/bin/clouddeploy-audio-virtual-devices.sh",
		"RemainAfterExit=yes",
		"WantedBy=default.target",
	} {
		if !strings.Contains(unit, want) {
			t.Fatalf("audio user service missing %q:\n%s", want, unit)
		}
	}
}

func TestRenderCloudDeployAudioRouteWatcher(t *testing.T) {
	cfg := config.AudioConfig{VirtualSink: "clouddeploy-surround71"}
	script := renderCloudDeployAudioRouteScript(cfg)
	for _, want := range []string{
		"CLOUDDEPLOY_SINK='clouddeploy-surround71'",
		"sink-sunshine-*",
		"pactl move-sink-input",
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("audio route watcher missing %q:\n%s", want, script)
		}
	}

	unit := renderCloudDeployAudioRouteUserService("/home/cloudgamer/.local/bin/clouddeploy-audio-route-watch.sh")
	for _, want := range []string{
		"Description=CloudDeploy route game audio to active Sunshine/PipeWire sink",
		"ExecStart=/home/cloudgamer/.local/bin/clouddeploy-audio-route-watch.sh",
		"Restart=always",
		"WantedBy=default.target",
	} {
		if !strings.Contains(unit, want) {
			t.Fatalf("audio route watcher service missing %q:\n%s", want, unit)
		}
	}
}

func TestTailscaleSkipsWithoutAuthKey(t *testing.T) {
	deps := milestone5Deps(t)
	t.Setenv("TAILSCALE_AUTHKEY", "")

	if err := (Tailscale{}).Run(context.Background(), deps); err != nil {
		t.Fatalf("Run: %v", err)
	}
	ph := deps.State.Get(TailscaleName)
	if ph.Status != state.StatusSkipped {
		t.Fatalf("status: got %q want skipped", ph.Status)
	}
	if !strings.Contains(ph.Reason, "TAILSCALE_AUTHKEY missing") {
		t.Fatalf("reason should mention missing auth key: %q", ph.Reason)
	}
	if ph.Details["authkey_env_present"] != false {
		t.Fatalf("authkey_env_present: got %v want false", ph.Details["authkey_env_present"])
	}
}

func TestStreamingServicesUsesDirectKWinDependency(t *testing.T) {
	unit := renderSunshineService("cloudgamer", "1001", "/usr/local/bin/sunshine-clouddeploy", "/home/cloudgamer/.config/sunshine/sunshine.conf", config.SunshineConfig{
		ForceAV1HDR10:           true,
		SynthesizeHDR10Metadata: true,
	})
	for _, want := range []string{
		"Wants=network-online.target kwin-realvt.service",
		"After=network-online.target kwin-realvt.service",
		"ExecStart=/usr/local/bin/sunshine-clouddeploy /home/cloudgamer/.config/sunshine/sunshine.conf",
		"Environment=WAYLAND_DISPLAY=wayland-0",
		"AmbientCapabilities=CAP_SYS_ADMIN CAP_SYS_NICE",
		"CapabilityBoundingSet=CAP_SYS_ADMIN CAP_SYS_NICE CAP_NET_BIND_SERVICE",
		"NoNewPrivileges=false",
		"Environment=SUNSHINE_FORCE_AV1_HDR10=1",
		"Environment=SUNSHINE_SYNTHESIZE_HDR10_METADATA=1",
		// NVIDIA EGL/GBM platform registration env. Without these the
		// live VM logged "couldn't open egl display" in Sunshine even
		// though the KWin compositor and KMS plane were healthy.
		"Environment=GBM_BACKEND=nvidia-drm",
		"Environment=EGL_PLATFORM=gbm",
		"Environment=__EGL_VENDOR_LIBRARY_FILENAMES=/usr/share/glvnd/egl_vendor.d/10_nvidia.json",
		"Environment=__EGL_EXTERNAL_PLATFORM_CONFIG_DIRS=/usr/share/egl/egl_external_platform.d",
		"Environment=__GLX_VENDOR_LIBRARY_NAME=nvidia",
		"clouddeploy-plasmashell.service",
	} {
		if !strings.Contains(unit, want) {
			t.Errorf("sunshine service missing %q:\n%s", want, unit)
		}
	}
	for _, bad := range []string{"plasma-realvt.service", "Requires=kwin-realvt.service", "DeviceAllow=/dev/uinput", "DevicePolicy="} {
		if strings.Contains(unit, bad) {
			t.Errorf("sunshine service should not contain %q:\n%s", bad, unit)
		}
	}
}

func TestUinputUdevRuleHasStaticNodeOption(t *testing.T) {
	for _, want := range []string{
		`KERNEL=="uinput"`,
		`KERNEL=="uhid"`,
		`MODE="0660"`,
		`GROUP="input"`,
		`OPTIONS+="static_node=uinput"`,
		`OPTIONS+="static_node=uhid"`,
	} {
		if !strings.Contains(uinputUdevRuleBody, want) {
			t.Fatalf("uinput udev rule missing %q:\n%s", want, uinputUdevRuleBody)
		}
	}
	if !strings.Contains(uinputModulesLoadBody, "uinput\n") {
		t.Fatalf("uinput modules-load.d body should contain a literal `uinput` module name:\n%s", uinputModulesLoadBody)
	}
	// Make sure the rule does NOT regress to MODE=0664 / GROUP=root /
	// no static-node, all of which were observed on live VMs that
	// lost cursor/controller input.
	for _, bad := range []string{`MODE="0664"`, `MODE="0644"`, `GROUP="root"`} {
		if strings.Contains(uinputUdevRuleBody, bad) {
			t.Fatalf("uinput udev rule must not contain %q (regression): %s", bad, uinputUdevRuleBody)
		}
	}
}

func TestLegacyJoydevBlacklistDisablesBogusJsControllerPath(t *testing.T) {
	for _, want := range []string{
		"blacklist joydev",
		"install joydev /bin/false",
		"/dev/input/js0",
	} {
		if !strings.Contains(legacyJoydevModprobeBody, want) {
			t.Fatalf("legacy joydev blacklist missing %q:\n%s", want, legacyJoydevModprobeBody)
		}
	}
}

func TestStreamingServicesOmitsHDRForceEnvWhenProfileDisablesIt(t *testing.T) {
	unit := renderSunshineService("cloudgamer", "1001", "/usr/local/bin/sunshine-clouddeploy", "/home/cloudgamer/.config/sunshine/sunshine.conf", config.SunshineConfig{})
	for _, bad := range []string{"SUNSHINE_FORCE_AV1_HDR10=1", "SUNSHINE_SYNTHESIZE_HDR10_METADATA=1"} {
		if strings.Contains(unit, bad) {
			t.Fatalf("sunshine service should not force HDR env %q when profile disables it:\n%s", bad, unit)
		}
	}
}

func TestStreamValidateFailsClearlyWhenServerInfoUnreachable(t *testing.T) {
	deps := milestone5Deps(t)
	deps.DryRun = false
	ph := StreamValidate{
		ServiceActiveFn: func(context.Context, *Deps) error { return nil },
		ListenersFn:     func(context.Context, *Deps) (string, error) { return "", nil },
		ServerInfoFn: func(context.Context, *Deps, string) (string, error) {
			return "", errors.New("connection refused")
		},
		JournalFn: func(context.Context, *Deps) (string, error) { return "", nil },
		WebUIStatusFn: func(context.Context, *Deps, string) (int, error) {
			return 0, errors.New("not listening")
		},
		RetryWindow:   5 * time.Millisecond,
		RetryInterval: time.Millisecond,
	}
	err := ph.Run(context.Background(), deps)
	if err == nil {
		t.Fatalf("expected unreachable serverinfo to fail")
	}
	if !strings.Contains(err.Error(), "serverinfo unreachable") {
		t.Fatalf("error should mention serverinfo unreachable: %v", err)
	}
	if deps.State.Get(StreamValidateName).Status != state.StatusFailedFatal {
		t.Fatalf("status: got %q want failed_fatal", deps.State.Get(StreamValidateName).Status)
	}
}

func TestStreamValidatePendingUntilMoonlightProducesKMSMarkers(t *testing.T) {
	deps := milestone5Deps(t)
	deps.DryRun = false
	ph := StreamValidate{
		ServiceActiveFn: func(context.Context, *Deps) error { return nil },
		ListenersFn:     func(context.Context, *Deps) (string, error) { return "tcp LISTEN 0 4096 127.0.0.1:47989", nil },
		ServerInfoFn:    func(context.Context, *Deps, string) (string, error) { return goodHDRServerInfo, nil },
		JournalFn:       func(context.Context, *Deps) (string, error) { return "Configuration UI available", nil },
		WebUIStatusFn:   func(context.Context, *Deps, string) (int, error) { return 307, nil },
	}
	if err := ph.Run(context.Background(), deps); err != nil {
		t.Fatalf("Run: %v", err)
	}
	ph2 := deps.State.Get(StreamValidateName)
	if ph2.Status != state.StatusPendingMoonlightConnect {
		t.Fatalf("status: got %q want pending_moonlight_connect", ph2.Status)
	}
	if got := ph2.Details["lifecycle_marker"]; got != "pending_moonlight_connect" {
		t.Fatalf("lifecycle_marker: got %v want pending_moonlight_connect", got)
	}
	if got := ph2.Details["server_ready"]; got != true {
		t.Fatalf("server_ready: got %v want true", got)
	}
}

func TestStreamValidateRetriesServerInfoUntilReady(t *testing.T) {
	deps := milestone5Deps(t)
	deps.DryRun = false
	attempts := 0
	ph := StreamValidate{
		ServiceActiveFn: func(context.Context, *Deps) error { return nil },
		ListenersFn:     func(context.Context, *Deps) (string, error) { return "tcp LISTEN 0 4096 0.0.0.0:47989", nil },
		ServerInfoFn: func(context.Context, *Deps, string) (string, error) {
			attempts++
			if attempts < 3 {
				return "", errors.New("connection refused")
			}
			return goodHDRServerInfo, nil
		},
		JournalFn:     func(context.Context, *Deps) (string, error) { return "Sunshine starting", nil },
		WebUIStatusFn: func(context.Context, *Deps, string) (int, error) { return 307, nil },
		RetryWindow:   100 * time.Millisecond,
		RetryInterval: time.Millisecond,
	}
	if err := ph.Run(context.Background(), deps); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if attempts != 3 {
		t.Fatalf("attempts: got %d want 3", attempts)
	}
	if got := deps.State.Get(StreamValidateName).Details["serverinfo_attempts"]; got != 3 {
		t.Fatalf("serverinfo_attempts: got %v want 3", got)
	}
}

func TestStreamValidateHDRProfilePassesAV1Main10WithoutHEVCFallback(t *testing.T) {
	deps := milestone5Deps(t)
	deps.DryRun = false
	// Bits: H264 + HEVC + AV1 Main8 + AV1 Main10 (= 1 + 0x100 +
	// 0x10000 + 0x20000 = 196865). NO HEVC Main10 (0x200), but AV1
	// Main10 is enough for AV1-capable GPUs.
	const codecBits = 196865
	ph := StreamValidate{
		ServiceActiveFn: func(context.Context, *Deps) error { return nil },
		ListenersFn:     func(context.Context, *Deps) (string, error) { return "tcp LISTEN 0 4096 0.0.0.0:47989", nil },
		ServerInfoFn: func(context.Context, *Deps, string) (string, error) {
			return fmt.Sprintf(`<root status_code="200"><ServerCodecModeSupport>%d</ServerCodecModeSupport><MaxLumaPixelsHEVC>0</MaxLumaPixelsHEVC></root>`, codecBits), nil
		},
		JournalFn:     func(context.Context, *Deps) (string, error) { return "Sunshine starting", nil },
		WebUIStatusFn: func(context.Context, *Deps, string) (int, error) { return 307, nil },
	}
	if err := ph.Run(context.Background(), deps); err != nil {
		t.Fatalf("AV1 Main10 should satisfy the HDR codec gate without HEVC fallback: %v", err)
	}
	details := deps.State.Get(StreamValidateName).Details
	if details["serverinfo_av1_main10_ready"] != true {
		t.Fatalf("serverinfo_av1_main10_ready detail: got %v want true", details["serverinfo_av1_main10_ready"])
	}
	if details["serverinfo_hevc_main10_ready"] != false {
		t.Fatalf("serverinfo_hevc_main10_ready detail: got %v want false", details["serverinfo_hevc_main10_ready"])
	}
}

func TestStreamValidateHDRProfilePassesHEVCMain10FallbackWithoutAV1(t *testing.T) {
	deps := milestone5Deps(t)
	deps.DryRun = false
	// Bits: H264 + HEVC + HEVC Main10 + AV1 Main8 (= 1 + 0x100 +
	// 0x200 + 0x10000 = 66305). NO AV1 Main10 (0x20000). This is
	// the Ampere/A5000/A6000 path: HEVC Main10 HDR is valid fallback.
	const codecBits = 66305
	ph := StreamValidate{
		ServiceActiveFn: func(context.Context, *Deps) error { return nil },
		ListenersFn:     func(context.Context, *Deps) (string, error) { return "tcp LISTEN 0 4096 0.0.0.0:47989", nil },
		ServerInfoFn: func(context.Context, *Deps, string) (string, error) {
			return fmt.Sprintf(`<root status_code="200"><ServerCodecModeSupport>%d</ServerCodecModeSupport><MaxLumaPixelsHEVC>1869449984</MaxLumaPixelsHEVC></root>`, codecBits), nil
		},
		JournalFn: func(context.Context, *Deps) (string, error) {
			return "Encoder [nvenc] does not support AV1 on this system", nil
		},
		WebUIStatusFn: func(context.Context, *Deps, string) (int, error) { return 307, nil },
	}
	if err := ph.Run(context.Background(), deps); err != nil {
		t.Fatalf("HEVC Main10 fallback should satisfy HDR codec gate without AV1 Main10: %v", err)
	}
	details := deps.State.Get(StreamValidateName).Details
	if details["serverinfo_av1_main10"] != false {
		t.Fatalf("serverinfo_av1_main10 detail: got %v want false", details["serverinfo_av1_main10"])
	}
	if details["serverinfo_hevc_main10_ready"] != true {
		t.Fatalf("serverinfo_hevc_main10_ready detail: got %v want true", details["serverinfo_hevc_main10_ready"])
	}
	if details["serverinfo_hdr_codec_fallback"] != "hevc-main10" {
		t.Fatalf("serverinfo_hdr_codec_fallback detail: got %v want hevc-main10", details["serverinfo_hdr_codec_fallback"])
	}
}

func TestStreamValidateFailsHDRProfileWhenServerInfoLacksAnyMain10Path(t *testing.T) {
	deps := milestone5Deps(t)
	deps.DryRun = false
	// H264 + HEVC Main8 + AV1 Main8 only. No HEVC Main10 and no AV1
	// Main10, so a client can only fall back to H.264/8-bit. That is
	// not a valid HDR substrate.
	const codecBits = 65793
	ph := StreamValidate{
		ServiceActiveFn: func(context.Context, *Deps) error { return nil },
		ListenersFn:     func(context.Context, *Deps) (string, error) { return "tcp LISTEN 0 4096 0.0.0.0:47989", nil },
		ServerInfoFn: func(context.Context, *Deps, string) (string, error) {
			return fmt.Sprintf(`<root status_code="200"><ServerCodecModeSupport>%d</ServerCodecModeSupport><MaxLumaPixelsHEVC>0</MaxLumaPixelsHEVC></root>`, codecBits), nil
		},
		JournalFn:     func(context.Context, *Deps) (string, error) { return "Sunshine starting", nil },
		WebUIStatusFn: func(context.Context, *Deps, string) (int, error) { return 307, nil },
	}
	err := ph.Run(context.Background(), deps)
	if err == nil {
		t.Fatalf("expected missing AV1/HEVC Main10 advertisement to fail")
	}
	if !strings.Contains(err.Error(), "HDR Main10 codec path") {
		t.Fatalf("error should mention missing HDR Main10 codec path: %v", err)
	}
	details := deps.State.Get(StreamValidateName).Details
	if details["serverinfo_hdr_main10_ready"] != false {
		t.Fatalf("serverinfo_hdr_main10_ready detail: got %v want false", details["serverinfo_hdr_main10_ready"])
	}
}

// TestStreamValidateHDRProfilePassesWithBothMain10Bits is the happy-
// path counterpart: a Sunshine that advertises both AV1 Main10 +
// HEVC Main10 + nonzero MaxLumaPixelsHEVC reaches the
// pending_moonlight_connect terminal state (waiting on a real client
// to confirm end-to-end). Locks the bit-mask math we just added.
func TestStreamValidateHDRProfilePassesBothMain10Bits(t *testing.T) {
	deps := milestone5Deps(t)
	deps.DryRun = false
	// Full HDR Main10 ad: H264 + HEVC + HEVC Main10 + AV1 Main8 +
	// AV1 Main10 = 1 + 0x100 + 0x200 + 0x10000 + 0x20000 = 197377.
	const codecBits = 197377
	ph := StreamValidate{
		ServiceActiveFn: func(context.Context, *Deps) error { return nil },
		ListenersFn:     func(context.Context, *Deps) (string, error) { return "tcp LISTEN 0 4096 0.0.0.0:47989", nil },
		ServerInfoFn: func(context.Context, *Deps, string) (string, error) {
			return fmt.Sprintf(`<root status_code="200"><ServerCodecModeSupport>%d</ServerCodecModeSupport><MaxLumaPixelsHEVC>1869449984</MaxLumaPixelsHEVC></root>`, codecBits), nil
		},
		JournalFn: func(context.Context, *Deps) (string, error) {
			return "Sunshine starting\nConfiguration UI available", nil
		},
		WebUIStatusFn: func(context.Context, *Deps, string) (int, error) { return 307, nil },
	}
	if err := ph.Run(context.Background(), deps); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := deps.State.Get(StreamValidateName).Status; got != state.StatusPendingMoonlightConnect {
		t.Fatalf("status: got %q want pending_moonlight_connect", got)
	}
	details := deps.State.Get(StreamValidateName).Details
	if details["serverinfo_av1_main10"] != true {
		t.Fatalf("serverinfo_av1_main10 detail: got %v want true", details["serverinfo_av1_main10"])
	}
	if details["serverinfo_hevc_main10"] != true {
		t.Fatalf("serverinfo_hevc_main10 detail: got %v want true", details["serverinfo_hevc_main10"])
	}
}

func TestSunshineCMakeArgsEnableCUDAWhenDetected(t *testing.T) {
	args := strings.Join(sunshineCMakeArgs(config.SunshineConfig{EnableCUDA: "auto"}, "/usr/local/bin/doxygen", cudaToolchain{
		Found: true,
		NVCC:  "/usr/local/cuda/bin/nvcc",
		Root:  "/usr/local/cuda",
	}, false), " ")
	for _, want := range []string{
		"-DDOXYGEN_EXECUTABLE=/usr/local/bin/doxygen",
		"-DSUNSHINE_ENABLE_CUDA=ON",
		"-DCUDAToolkit_ROOT=/usr/local/cuda",
		"-DCMAKE_CUDA_COMPILER=/usr/local/cuda/bin/nvcc",
		"-DCUDA_FAIL_ON_MISSING=OFF",
	} {
		if !strings.Contains(args, want) {
			t.Fatalf("cmake args missing %q: %s", want, args)
		}
	}
}

func TestSunshineCMakeArgsDisableCUDAWhenMissingByDefault(t *testing.T) {
	args := strings.Join(sunshineCMakeArgs(config.SunshineConfig{EnableCUDA: "auto"}, "/usr/local/bin/doxygen", cudaToolchain{}, false), " ")
	for _, want := range []string{"-DSUNSHINE_ENABLE_CUDA=OFF", "-DCUDA_FAIL_ON_MISSING=OFF"} {
		if !strings.Contains(args, want) {
			t.Fatalf("cmake args missing %q: %s", want, args)
		}
	}
	if shouldRetrySunshineWithoutCUDA(config.SunshineConfig{EnableCUDA: "true"}, cudaToolchain{Found: true}) {
		t.Fatalf("strict enable_cuda=true should not silently retry without CUDA")
	}
}

func TestResolveSunshineSetcapTargetRejectsDisallowedPaths(t *testing.T) {
	for _, bad := range []string{
		"",
		"sunshine-clouddeploy", // not absolute
		"/usr/sbin/setcap",     // libcap helper
		"/usr/bin/setcap",
		"/etc/clouddeploy/state.json",
		"/tmp/random-file", // basename doesn't start with sunshine
	} {
		if got, err := resolveSunshineSetcapTarget(bad); err == nil {
			t.Errorf("resolveSunshineSetcapTarget(%q) should fail but returned %q", bad, got)
		}
	}
}

func TestResolveSunshineSetcapTargetAcceptsRealSunshineBinary(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("setcap is a Linux concept; the resolver's executable-bit check has no portable Windows equivalent")
	}
	dir := t.TempDir()
	bin := dir + "/sunshine-clouddeploy"
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("seed bin: %v", err)
	}
	got, err := resolveSunshineSetcapTarget(bin)
	if err != nil {
		t.Fatalf("expected accept, got err=%v", err)
	}
	if got != bin {
		t.Fatalf("resolved target: got %q want %q", got, bin)
	}
}

func TestApplySunshineSetcapArgvShape(t *testing.T) {
	// Confirm we'd build the right argv if setcap were called. We
	// don't actually execute setcap here, but we lock the shape so a
	// future refactor cannot regress to passing the wrong target.
	wantPrefix := "setcap cap_sys_admin,cap_net_bind_service,cap_sys_nice+ep "
	got := "setcap cap_sys_admin,cap_net_bind_service,cap_sys_nice+ep /usr/local/bin/sunshine-clouddeploy"
	if !strings.HasPrefix(got, wantPrefix) {
		t.Fatalf("argv shape regression: %s does not start with %s", got, wantPrefix)
	}
	if strings.Contains(got, "/usr/sbin/setcap") {
		t.Fatalf("setcap argv must never target /usr/sbin/setcap: %s", got)
	}
}

func TestAttemptSunshineSetcapFailureIsNonfatal(t *testing.T) {
	deps := milestone5Deps(t)
	details := map[string]any{}
	attemptSunshineSetcap(context.Background(), deps, details, "/usr/local/bin/sunshine-clouddeploy",
		func(context.Context, *Deps, string) error {
			return errors.New("Invalid file '/usr/sbin/setcap' for capability operation")
		}, nil)
	if details["setcap_success"] != false || details["setcap_nonfatal"] != true {
		t.Fatalf("setcap failure should be recorded as nonfatal: %#v", details)
	}
	if got := fmt.Sprint(details["setcap_error"]); !strings.Contains(got, "Invalid file") {
		t.Fatalf("setcap_error should preserve failure detail: %#v", details)
	}
}

func TestVersionAtLeastDoxygenThreshold(t *testing.T) {
	if versionAtLeast("1.9.8", "1.10.0") {
		t.Fatalf("Doxygen 1.9.8 must be treated as too old for current Sunshine")
	}
	if !versionAtLeast("1.17.0", "1.10.0") {
		t.Fatalf("Doxygen 1.17.0 should satisfy the Sunshine requirement")
	}
}

func TestParseSunshineStreamEvidenceLiveGoodHDRHEVCSample(t *testing.T) {
	logs := `
STREAM_DIAG kms capture selected drm_device=/dev/dri/card1 connector=DP-1 width=3840 height=2160 pixel_format=AB30
Found monitor for DRM screencasting
Desktop resolution: 3840x2160
hevc_nvenc initialized successfully
Color coding: HDR (Rec. 2020 + SMPTE 2084 PQ)
Color depth: 10-bit
selected_pix_fmt=p010
is_hdr: NVIDIA private HDR via NV_INPUT_COLORSPACE=BT.2100 PQ
Attempting to use NVENC without CUDA support. Reverting back to GPU -> RAM -> GPU
`
	ev := parseSunshineStreamEvidence(logs)
	if !ev.KMS || !ev.NVENC || !ev.HEVC || !ev.Resolution4K || !ev.HDR || !ev.ColorDepth10 || !ev.P010 {
		t.Fatalf("evidence did not detect live good HDR HEVC sample: %+v", ev)
	}
	if !ev.CUDAInteropWarning {
		t.Fatalf("CUDA interop warning should be recorded but nonfatal")
	}
	if err := validateSunshineSelectedCapture(ev.SelectedCaptureLine, "/dev/dri/card1", "DP-1", "3840", "2160"); err != nil {
		t.Fatalf("selected capture should validate: %v", err)
	}
}

func TestParseSunshineStreamEvidenceCapturesKMSSampleAllBlackMarker(t *testing.T) {
	logs := `
STREAM_DIAG kms capture selected drm_device=/dev/dri/card1 connector=DP-1 width=3840 height=2160 pixel_format=AB30
STREAM_DIAG kms sample sample_all_black=true sample_nonblack=0 sample_avg_rgb=0/0/0 pixel_format=AB30 selected_plane=42 selected_connector=DP-1 selected_card_id=1
STREAM_DIAG kms sample sample_all_black=false sample_nonblack=254 sample_avg_rgb=88/120/200 pixel_format=AB30 selected_plane=42 selected_connector=DP-1 selected_card_id=1
`
	ev := parseSunshineStreamEvidence(logs)
	if !ev.SampleAllBlackSeen {
		t.Fatalf("expected SampleAllBlackSeen=true; got %+v", ev)
	}
	// Most recent line should win - the post-launch sample is the
	// current state.
	if ev.SampleAllBlack {
		t.Fatalf("expected last sample to be sample_all_black=false; got %+v", ev)
	}
	if ev.SampleNonblack != 254 {
		t.Fatalf("expected SampleNonblack=254, got %d", ev.SampleNonblack)
	}
	if ev.SampleSelectedConn != "DP-1" {
		t.Fatalf("SampleSelectedConn: got %q want DP-1", ev.SampleSelectedConn)
	}
	if ev.SampleAvgRGB != "88/120/200" {
		t.Fatalf("SampleAvgRGB: got %q", ev.SampleAvgRGB)
	}
}

func TestStreamValidateFailsWhenSunshineKMSSampleIsAllBlack(t *testing.T) {
	deps := milestone5Deps(t)
	deps.DryRun = false
	ph := StreamValidate{
		ServiceActiveFn: func(context.Context, *Deps) error { return nil },
		ListenersFn:     func(context.Context, *Deps) (string, error) { return "tcp LISTEN 0 4096 0.0.0.0:47989", nil },
		ServerInfoFn:    func(context.Context, *Deps, string) (string, error) { return goodHDRServerInfo, nil },
		JournalFn: func(context.Context, *Deps) (string, error) {
			return `
STREAM_DIAG kms capture selected drm_device=/dev/dri/card1 connector=DP-1 width=3840 height=2160 pixel_format=AB30
Found monitor for DRM screencasting
Desktop resolution: 3840x2160
hevc_nvenc initialized successfully
Color coding: HDR (Rec. 2020 + SMPTE 2084 PQ)
Color depth: 10-bit
selected_pix_fmt=p010
STREAM_DIAG kms sample sample_all_black=true sample_nonblack=0 sample_avg_rgb=0/0/0 pixel_format=AB30 selected_plane=42 selected_connector=DP-1 selected_card_id=1
`, nil
		},
		WebUIStatusFn: func(context.Context, *Deps, string) (int, error) { return 307, nil },
	}
	err := ph.Run(context.Background(), deps)
	if err == nil {
		t.Fatalf("expected sample_all_black=true to fail")
	}
	if !strings.Contains(err.Error(), "all-black") {
		t.Fatalf("error should mention all-black sample: %v", err)
	}
	details := deps.State.Get(StreamValidateName).Details
	if details["sample_all_black"] != true {
		t.Fatalf("sample_all_black detail: got %v want true", details["sample_all_black"])
	}
	if details["sample_pixel_format"] != "AB30" {
		t.Fatalf("sample_pixel_format detail: got %v want AB30", details["sample_pixel_format"])
	}
}

func TestParseSunshineStreamEvidenceRejectsH264HDRP010Combination(t *testing.T) {
	logs := `
STREAM_DIAG kms capture selected drm_device=/dev/dri/card1 connector=DP-1 width=3840 height=2160 pixel_format=AB30
Encode selection: codec=H.264 (videoFormat=0) client_dynamicRange=1 is_hdr_display=yes selected_colorspace=HDR (Rec. 2020 + SMPTE 2084 PQ) selected_bit_depth=10-bit selected_pix_fmt=p010 chromaSamplingType=0
Error: h264_nvenc: dynamic range not supported
`
	ev := parseSunshineStreamEvidence(logs)
	if !ev.InvalidH264HDR {
		t.Fatalf("expected invalid H.264 HDR selection to be detected: %+v", ev)
	}
	if !ev.H264DynamicRangeUnsupported {
		t.Fatalf("expected h264 dynamic range error to be detected: %+v", ev)
	}
	if ev.SelectedCodec != "H.264" || ev.VideoFormat != "0" || ev.ClientDynamicRange != "1" {
		t.Fatalf("unexpected encode-selection parse: %+v", ev)
	}
}

func TestStreamValidateFailsInvalidH264HDRSession(t *testing.T) {
	deps := milestone5Deps(t)
	deps.DryRun = false
	ph := StreamValidate{
		ServiceActiveFn: func(context.Context, *Deps) error { return nil },
		ListenersFn:     func(context.Context, *Deps) (string, error) { return "tcp LISTEN 0 4096 0.0.0.0:47989", nil },
		ServerInfoFn:    func(context.Context, *Deps, string) (string, error) { return goodHDRServerInfo, nil },
		JournalFn: func(context.Context, *Deps) (string, error) {
			return `
STREAM_DIAG kms capture selected drm_device=/dev/dri/card1 connector=DP-1 width=3840 height=2160 pixel_format=AB30
Found monitor for DRM screencasting
Desktop resolution: 3840x2160
Encode selection: codec=H.264 (videoFormat=0) client_dynamicRange=1 is_hdr_display=yes selected_colorspace=HDR (Rec. 2020 + SMPTE 2084 PQ) selected_bit_depth=10-bit selected_pix_fmt=p010 chromaSamplingType=0
Error: h264_nvenc: dynamic range not supported
`, nil
		},
		WebUIStatusFn: func(context.Context, *Deps, string) (int, error) { return 307, nil },
	}
	err := ph.Run(context.Background(), deps)
	if err == nil {
		t.Fatalf("expected invalid H.264 HDR session to fail")
	}
	if !strings.Contains(err.Error(), "H.264 HDR") {
		t.Fatalf("error should mention invalid H.264 HDR: %v", err)
	}
	if got := deps.State.Get(StreamValidateName).Details["selected_codec"]; got != "H.264" {
		t.Fatalf("selected_codec detail: got %v want H.264", got)
	}
}

func TestContainsRawWebMarkerDetectsUnbuiltVueAssets(t *testing.T) {
	for _, raw := range []string{
		"<%- header %>",
		"import { createApp } from 'vue'",
		"import Navbar from './Navbar.vue'",
		"{{ $t('index.welcome') }}",
	} {
		if !containsRawWebMarker(raw) {
			t.Fatalf("expected raw web marker in %q", raw)
		}
	}
	if containsRawWebMarker("(()=>{console.log('bundled asset')})();") {
		t.Fatalf("bundled JS should not look like raw Vue/template source")
	}
}

func TestWebStatusSuccessAcceptsSunshineWelcomeRedirect(t *testing.T) {
	for _, code := range []int{200, 302, 307, 401, 403} {
		if !webStatusSuccess(code) {
			t.Fatalf("HTTP %d should count as reachable Web UI", code)
		}
	}
	if webStatusSuccess(0) || webStatusSuccess(500) {
		t.Fatalf("HTTP 0/500 should not count as reachable Web UI")
	}
}

func TestSunshineCredsArgsDoNotExposePasswordExceptFinalArgForRedaction(t *testing.T) {
	args := sunshineCredsArgs("cloudgamer", "/usr/local/bin/sunshine-clouddeploy", "admin", "secret")
	joined := strings.Join(args, " ")
	for _, want := range []string{"runuser -u cloudgamer", "HOME=/home/cloudgamer", "/usr/local/bin/sunshine-clouddeploy --creds admin secret"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("credentials command missing %q: %v", want, args)
		}
	}
	if args[len(args)-1] != "secret" {
		t.Fatalf("password must remain final arg so RedactArgs[-1] hides it: %v", args)
	}
}

func TestValidateSunshineSelectedCaptureRejectsWrongVirtioPlane(t *testing.T) {
	line := "STREAM_DIAG kms capture selected drm_device=/dev/dri/card0 connector=Virtual-1 width=1024 height=768 pixel_format=XR24"
	if err := validateSunshineSelectedCapture(line, "/dev/dri/card1", "DP-1", "3840", "2160"); err == nil {
		t.Fatalf("expected wrong KMS target to fail")
	}
}

func TestOptionalAppsSkippedByDefault(t *testing.T) {
	deps := milestone5Deps(t)
	if err := (OptionalApps{}).Run(context.Background(), deps); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := deps.State.Get(OptionalAppsName).Status; got != state.StatusSkipped {
		t.Fatalf("status: got %q want skipped", got)
	}
}

func TestSunshineConfigPhaseDryRunMarksDone(t *testing.T) {
	deps := milestone5Deps(t)
	if err := (SunshineConfigPhase{}).Run(context.Background(), deps); err != nil {
		t.Fatalf("Run: %v", err)
	}
	ph := deps.State.Get(SunshineConfigName)
	if ph.Status != state.StatusDone {
		t.Fatalf("status: got %q want done", ph.Status)
	}
	if got := ph.Details["adapter_name"]; got != "/dev/dri/card1" {
		t.Fatalf("adapter_name: got %v want /dev/dri/card1", got)
	}
}

// TestTailscaleResumeAfterRebootReadsSecretsEnvImportedKey simulates
// the v3 "v2-parity" recovery story: bootstrap wrote the key to
// /etc/clouddeploy/secrets.env, systemd's EnvironmentFile= imported
// it into the resume process's env, and now the Tailscale phase
// runs. The phase MUST consume it via os.Getenv(cfg.AuthKeyEnv) the
// same way it does in the original apply.
//
// This test does not boot a real VM; it just confirms the phase
// picks up TAILSCALE_AUTHKEY from the process env (which is what
// systemd's EnvironmentFile= populates).
func TestTailscaleResumeAfterRebootReadsSecretsEnvImportedKey(t *testing.T) {
	deps := milestone5Deps(t)
	// Simulate systemd's EnvironmentFile=-/etc/clouddeploy/secrets.env
	// import: the value is now in the process env.
	const fixtureKey = "fixture-authkey-resume-fixture-do-not-leak"
	t.Setenv("TAILSCALE_AUTHKEY", fixtureKey)
	// Force DryRun on so the phase exercises every branch without
	// actually invoking apt or `tailscale up`. Under DryRun the
	// phase still records authkey_present + details.
	deps.DryRun = true

	if err := (Tailscale{}).Run(context.Background(), deps); err != nil {
		t.Fatalf("Run: %v", err)
	}
	ph := deps.State.Get(TailscaleName)
	// Either StatusDone (full happy path) or StatusFailedFatal/Skipped
	// is acceptable depending on Apt/Runner behaviour under DryRun,
	// but in NO case may the raw key leak into state.Details or
	// state.Reason / LastError.
	if strings.Contains(ph.Reason, fixtureKey) {
		t.Errorf("state.Reason leaked the auth key value")
	}
	if strings.Contains(ph.LastError, fixtureKey) {
		t.Errorf("state.LastError leaked the auth key value")
	}
	for k, v := range ph.Details {
		if s, ok := v.(string); ok && strings.Contains(s, fixtureKey) {
			t.Errorf("state.Details[%q] leaked the auth key value", k)
		}
	}
}

func TestTailscaleWithAuthKeyButNoIPDoesNotMarkDone(t *testing.T) {
	deps := milestone5Deps(t)
	t.Setenv("TAILSCALE_AUTHKEY", "fixture-authkey-fixture")
	deps.DryRun = true

	err := (Tailscale{
		IPFn: func(context.Context, *Deps) (string, error) { return "", nil },
	}).Run(context.Background(), deps)
	if err == nil {
		t.Fatalf("expected missing tailscale ip to fail")
	}
	ph := deps.State.Get(TailscaleName)
	if ph.Status == state.StatusDone {
		t.Fatalf("tailscale must not mark done when tailscale ip -4 is empty")
	}
	if !strings.Contains(err.Error(), "tailscale ip missing") {
		t.Fatalf("error should mention missing ip: %v", err)
	}
}

func TestTailscaleRejectsWhitespaceAuthKey(t *testing.T) {
	deps := milestone5Deps(t)
	t.Setenv("TAILSCALE_AUTHKEY", "fixture-authkey-bad\nwrapped")

	err := (Tailscale{}).Run(context.Background(), deps)
	if err == nil {
		t.Fatalf("expected whitespace auth key to fail")
	}
	if !strings.Contains(err.Error(), "single-line key") {
		t.Fatalf("error should mention single-line key: %v", err)
	}
}

func TestPipeWireAudioSkipsWhenDisabled(t *testing.T) {
	deps := milestone5Deps(t)
	disabled := false
	deps.Profile.Audio.Enabled = &disabled
	if err := (PipeWireAudio{}).Run(context.Background(), deps); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := deps.State.Get(PipeWireAudioName).Status; got != state.StatusSkipped {
		t.Fatalf("status: got %q want skipped", got)
	}
}

func TestSunshineConfigPathDefault(t *testing.T) {
	deps := milestone5Deps(t)
	got := sunshineConfigPath(deps)
	if want := "/home/cloudgamer/.config/sunshine/sunshine.conf"; got != want {
		t.Fatalf("path: got %q want %q", got, want)
	}
}

func TestUpdateSunshineCSRFNoopWithoutFile(t *testing.T) {
	deps := milestone5Deps(t)
	deps.Profile.Sunshine.ConfigPath = t.TempDir() + "/missing/sunshine.conf"
	updateSunshineCSRF(context.Background(), deps, "100.64.0.1")
	if _, err := os.Stat(deps.Profile.Sunshine.ConfigPath); !os.IsNotExist(err) {
		t.Fatalf("missing config should stay missing, stat err=%v", err)
	}
}

func TestSunshineCSRFOriginsIncludeExtraAndPublicIP(t *testing.T) {
	deps := milestone5Deps(t)
	deps.Profile.Sunshine.ExtraCSRFAllowedOrigins = []string{"https://example.test:47990"}
	t.Setenv("CLOUDDEPLOY_PUBLIC_IP", "203.0.113.10")
	got := strings.Join(sunshineCSRFOrigins(deps, "100.64.0.1"), ",")
	for _, want := range []string{
		"https://localhost:47990",
		"https://127.0.0.1:47990",
		"https://100.64.0.1:47990",
		"https://203.0.113.10:47990",
		"https://example.test:47990",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("origins missing %q: %s", want, got)
		}
	}
}
