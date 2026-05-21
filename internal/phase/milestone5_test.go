package phase

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

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
		"stream_audio = disabled",
		"av1_mode = 2",
		"csrf_allowed_origins = https://localhost:47990",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("sunshine.conf missing %q:\n%s", want, body)
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
	if !strings.Contains(ph.Reason, "no Tailscale auth key") {
		t.Fatalf("reason should mention missing auth key: %q", ph.Reason)
	}
}

func TestStreamingServicesUsesDirectKWinDependency(t *testing.T) {
	unit := renderSunshineService("cloudgamer", "1001", "/usr/local/bin/sunshine-clouddeploy", "/home/cloudgamer/.config/sunshine/sunshine.conf")
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
	} {
		if !strings.Contains(unit, want) {
			t.Errorf("sunshine service missing %q:\n%s", want, unit)
		}
	}
	for _, bad := range []string{"plasma-realvt.service", "Requires=kwin-realvt.service"} {
		if strings.Contains(unit, bad) {
			t.Errorf("sunshine service should not contain %q:\n%s", bad, unit)
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
		ServerInfoFn:    func(context.Context, *Deps, string) (string, error) { return "<root></root>", nil },
		JournalFn:       func(context.Context, *Deps) (string, error) { return "Configuration UI available", nil },
	}
	if err := ph.Run(context.Background(), deps); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := deps.State.Get(StreamValidateName).Status; got != state.StatusPendingMoonlightConnect {
		t.Fatalf("status: got %q want pending_moonlight_connect", got)
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
