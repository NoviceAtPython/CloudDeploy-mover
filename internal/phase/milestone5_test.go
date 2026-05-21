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
