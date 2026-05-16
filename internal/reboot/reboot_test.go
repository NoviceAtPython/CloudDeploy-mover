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
		"EnvironmentFile=-/etc/clouddeploy/continue.env",
		"ExecStart=/usr/local/bin/clouddeployctl resume --profile hdr-4k120 --state-path /var/lib/clouddeploy/state.json",
		"WantedBy=multi-user.target",
	}
	for _, w := range want {
		if !strings.Contains(body, w) {
			t.Errorf("unit missing %q\n--- unit ---\n%s", w, body)
		}
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
}

func TestRenderEnvFile(t *testing.T) {
	body := RenderEnvFile(Args{Profile: "hdr-4k120", StatePath: "/var/lib/clouddeploy/state.json"})
	if !strings.Contains(body, "CLOUDDEPLOY_PROFILE=hdr-4k120") {
		t.Errorf("env file missing profile: %s", body)
	}
	if !strings.Contains(body, "CLOUDDEPLOY_STATE_PATH=/var/lib/clouddeploy/state.json") {
		t.Errorf("env file missing state-path: %s", body)
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
