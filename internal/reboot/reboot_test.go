package reboot

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NoviceAtPython/CloudDeploy-mover/internal/runner"
)

func TestRenderUnit_ContainsKeys(t *testing.T) {
	body := RenderUnit(Args{Profile: "hdr-4k120", StatePath: "/var/lib/clouddeploy/state.json"}, "", "")
	want := []string{
		"[Unit]",
		"Description=CloudDeploy v3 continuation",
		"After=network-online.target",
		"ConditionPathExists=/var/lib/clouddeploy/state.json",
		"[Service]",
		"Type=oneshot",
		"TimeoutStartSec=infinity",
		"EnvironmentFile=-/etc/clouddeploy/continue.env",
		// Secrets file loaded with EnvironmentFile=- so the absence
		// is fine; presence preserves operator-supplied credentials
		// (Tailscale auth key, future tokens) across the reboot.
		"EnvironmentFile=-/etc/clouddeploy/secrets.env",
		"ExecStart=/usr/local/bin/clouddeployctl resume --profile hdr-4k120 --config-dir /opt/clouddeploy-mover/config --state-path /var/lib/clouddeploy/state.json",
		"WantedBy=multi-user.target",
	}
	for _, w := range want {
		if !strings.Contains(body, w) {
			t.Errorf("unit missing %q\n--- unit ---\n%s", w, body)
		}
	}
}

func TestRenderUnitWithSecrets_CustomSecretsPath(t *testing.T) {
	body := RenderUnitWithSecrets(
		Args{Profile: "hdr-4k120", StatePath: "/var/lib/clouddeploy/state.json"},
		"", "", "/run/secrets-custom.env",
	)
	if !strings.Contains(body, "EnvironmentFile=-/run/secrets-custom.env") {
		t.Errorf("custom secrets path not in unit body:\n%s", body)
	}
	// Default continue.env should still be present.
	if !strings.Contains(body, "EnvironmentFile=-/etc/clouddeploy/continue.env") {
		t.Errorf("non-secret continue.env line missing:\n%s", body)
	}
}

func TestService_SecretsEnvFile_HonorsEnvOverride(t *testing.T) {
	t.Setenv("CLOUDDEPLOY_SECRETS_ENV", "/run/from-env.env")
	dir := t.TempDir()
	unitPath := filepath.Join(dir, "continue.service")
	s := &Service{UnitPath: unitPath, EnvFile: filepath.Join(dir, "continue.env")}
	if err := s.Install(context.Background(), Args{Profile: "p", StatePath: "/s.json"}); err == nil {
		// Install always runs systemctl daemon-reload, which fails
		// outside Linux. The path resolution happens during writeUnit
		// BEFORE systemctl, so we read the file regardless.
	}
	body, _ := os.ReadFile(unitPath)
	if !strings.Contains(string(body), "EnvironmentFile=-/run/from-env.env") {
		t.Errorf("CLOUDDEPLOY_SECRETS_ENV override not honored:\n%s", string(body))
	}
}

func TestRenderUnit_HonorsCustomPaths(t *testing.T) {
	body := RenderUnit(
		Args{Profile: "sdr-safe", StatePath: "/tmp/state.json"},
		"/tmp/continue.env",
		"/usr/local/bin/clouddeployctl",
	)
	if !strings.Contains(body, "EnvironmentFile=-/tmp/continue.env") {
		t.Errorf("custom env-file path not used:\n%s", body)
	}
	if !strings.Contains(body, "--profile sdr-safe") {
		t.Errorf("custom profile not used:\n%s", body)
	}
	if !strings.Contains(body, "--config-dir /opt/clouddeploy-mover/config") {
		t.Errorf("default config-dir not rendered:\n%s", body)
	}
}

func TestRenderEnvFile(t *testing.T) {
	body := RenderEnvFile(Args{Profile: "hdr-4k120", StatePath: "/var/lib/clouddeploy/state.json"})
	if !strings.Contains(body, "CLOUDDEPLOY_PROFILE=hdr-4k120") {
		t.Errorf("env file missing profile: %s", body)
	}
	if !strings.Contains(body, "CLOUDDEPLOY_STATE_PATH=/var/lib/clouddeploy/state.json") {
		t.Errorf("env file missing state-path: %s", body)
	}
	if !strings.Contains(body, "CLOUDDEPLOY_CONFIG_DIR=/opt/clouddeploy-mover/config") {
		t.Errorf("env file missing config-dir: %s", body)
	}
}

func TestService_InstallWritesFiles(t *testing.T) {
	dir := t.TempDir()
	unitPath := filepath.Join(dir, "clouddeploy-v3-continue.service")
	envFile := filepath.Join(dir, "continue.env")

	s := &Service{
		UnitPath: unitPath,
		EnvFile:  envFile,
		Binary:   "/usr/local/bin/clouddeployctl",
		Runner:   &runner.Runner{LogDir: "-"},
		DryRun:   true, // do not actually call systemctl
	}
	if err := s.Install(context.Background(), Args{Profile: "hdr-4k120", StatePath: "/var/lib/clouddeploy/state.json"}); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if _, err := os.Stat(unitPath); err != nil {
		t.Fatalf("unit not written: %v", err)
	}
	if _, err := os.Stat(envFile); err != nil {
		t.Fatalf("env file not written: %v", err)
	}
	if !s.IsInstalled() {
		t.Errorf("IsInstalled should be true after Install")
	}
}

func TestService_DisableRemovesFiles(t *testing.T) {
	dir := t.TempDir()
	unitPath := filepath.Join(dir, "u.service")
	envFile := filepath.Join(dir, "e.env")
	_ = os.WriteFile(unitPath, []byte("unit"), 0o644)
	_ = os.WriteFile(envFile, []byte("env"), 0o644)

	s := &Service{
		UnitPath: unitPath,
		EnvFile:  envFile,
		Runner:   &runner.Runner{LogDir: "-"},
		DryRun:   true,
	}
	if err := s.Disable(context.Background()); err != nil {
		t.Fatalf("Disable: %v", err)
	}
	if _, err := os.Stat(unitPath); !os.IsNotExist(err) {
		t.Errorf("unit not removed; err=%v", err)
	}
	if _, err := os.Stat(envFile); !os.IsNotExist(err) {
		t.Errorf("env file not removed; err=%v", err)
	}
}

func TestService_DisableIdempotent(t *testing.T) {
	// Calling Disable when nothing is installed is a no-op.
	dir := t.TempDir()
	s := &Service{
		UnitPath: filepath.Join(dir, "missing.service"),
		EnvFile:  filepath.Join(dir, "missing.env"),
		Runner:   &runner.Runner{LogDir: "-"},
		DryRun:   true,
	}
	if err := s.Disable(context.Background()); err != nil {
		t.Errorf("Disable on missing files should be a no-op: %v", err)
	}
}
