package phase

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NoviceAtPython/CloudDeploy-mover/internal/config"
	"github.com/NoviceAtPython/CloudDeploy-mover/internal/state"
)

func runtimeProfile() *config.Profile {
	p := desktopProfile()
	p.Display = config.DisplayConfig{
		Resolution:      "3840x2160",
		Refresh:         120,
		HDR:             true,
		ForcedConnector: "DP-1",
		Edid:            "virtual-4k120-hdr.bin",
	}
	return p
}

func seedDRINodes(t *testing.T, dir string, cards, renderNodes int) {
	t.Helper()
	for i := 0; i < cards; i++ {
		full := filepath.Join(dir, "card"+itoa(i))
		if err := os.WriteFile(full, []byte("x"), 0o644); err != nil {
			t.Fatalf("seed card: %v", err)
		}
	}
	for i := 128; i < 128+renderNodes; i++ {
		full := filepath.Join(dir, "renderD"+itoa(i))
		if err := os.WriteFile(full, []byte("x"), 0o644); err != nil {
			t.Fatalf("seed renderD: %v", err)
		}
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	buf := [20]byte{}
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

func seedFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("seed %s: %v", path, err)
	}
}

func TestDesktopRuntime_HappyPath(t *testing.T) {
	deps := newDeps(t, runtimeProfile(), nil)

	driDir := t.TempDir()
	seedDRINodes(t, driDir, 1, 1)
	nvmsPath := filepath.Join(t.TempDir(), "modeset")
	seedFile(t, nvmsPath, "Y\n")
	cmdlinePath := filepath.Join(t.TempDir(), "cmdline")
	seedFile(t, cmdlinePath,
		"BOOT_IMAGE=/vmlinuz ro nvidia-drm.modeset=1 nvidia-drm.fbdev=1 "+
			"drm.edid_firmware=DP-1:edid/virtual-4k120-hdr.bin video=DP-1:e\n")

	ph := DesktopRuntime{
		DRIDir:               driDir,
		NvidiaDRMModesetPath: nvmsPath,
		CmdlinePath:          cmdlinePath,
		UserGroupsFn: func(context.Context, *Deps, string) ([]string, error) {
			return []string{"cloudgamer", "video", "render", "input", "audio", "systemd-journal"}, nil
		},
	}
	if err := ph.Run(context.Background(), deps); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := deps.State.Get(DesktopRuntimeName).Status; got != state.StatusDone {
		t.Errorf("status: got %q want done", got)
	}
	d := deps.State.Get(DesktopRuntimeName).Details
	if d["cmdline_ok"] != true {
		t.Errorf("cmdline_ok: got %v want true", d["cmdline_ok"])
	}
	if d["user_device_access_ok"] != true {
		t.Errorf("user_device_access_ok: got %v want true", d["user_device_access_ok"])
	}
	if d["nvidia_drm_modeset"] != "Y" {
		t.Errorf("nvidia_drm_modeset: got %v want Y", d["nvidia_drm_modeset"])
	}
}

func TestDesktopRuntime_MissingCmdlineTokensFailsFatal(t *testing.T) {
	deps := newDeps(t, runtimeProfile(), nil)
	driDir := t.TempDir()
	seedDRINodes(t, driDir, 1, 1)
	nvmsPath := filepath.Join(t.TempDir(), "modeset")
	seedFile(t, nvmsPath, "Y\n")
	cmdlinePath := filepath.Join(t.TempDir(), "cmdline")
	// Missing nvidia-drm.fbdev=1 + edid + DP-1 enabled.
	seedFile(t, cmdlinePath, "BOOT_IMAGE=/vmlinuz ro nvidia-drm.modeset=1\n")

	ph := DesktopRuntime{
		DRIDir:               driDir,
		NvidiaDRMModesetPath: nvmsPath,
		CmdlinePath:          cmdlinePath,
		UserGroupsFn: func(context.Context, *Deps, string) ([]string, error) {
			return []string{"cloudgamer", "video", "render"}, nil
		},
	}
	err := ph.Run(context.Background(), deps)
	if err == nil {
		t.Fatalf("expected fatal when cmdline tokens missing")
	}
	if got := deps.State.Get(DesktopRuntimeName).Status; got != state.StatusFailedFatal {
		t.Errorf("status: got %q want failed_fatal", got)
	}
	d := deps.State.Get(DesktopRuntimeName).Details
	missing, _ := d["cmdline_missing"].([]string)
	if len(missing) < 2 {
		t.Errorf("cmdline_missing should list >=2 entries; got %v", missing)
	}
	if !strings.Contains(err.Error(), "kernel cmdline") {
		t.Errorf("error should explain cmdline mismatch: %v", err)
	}
}

func TestDesktopRuntime_NoRenderNodeFailsFatal(t *testing.T) {
	deps := newDeps(t, runtimeProfile(), nil)
	driDir := t.TempDir()
	seedDRINodes(t, driDir, 1, 0) // card but no renderD
	nvmsPath := filepath.Join(t.TempDir(), "modeset")
	seedFile(t, nvmsPath, "Y\n")
	cmdlinePath := filepath.Join(t.TempDir(), "cmdline")
	seedFile(t, cmdlinePath,
		"nvidia-drm.modeset=1 nvidia-drm.fbdev=1 "+
			"drm.edid_firmware=DP-1:edid/virtual-4k120-hdr.bin video=DP-1:e\n")
	ph := DesktopRuntime{
		DRIDir:               driDir,
		NvidiaDRMModesetPath: nvmsPath,
		CmdlinePath:          cmdlinePath,
		UserGroupsFn: func(context.Context, *Deps, string) ([]string, error) {
			return []string{"cloudgamer", "video", "render"}, nil
		},
	}
	err := ph.Run(context.Background(), deps)
	if err == nil {
		t.Fatalf("expected fatal when render node missing")
	}
	if !strings.Contains(err.Error(), "DRM device nodes") {
		t.Errorf("error should mention DRM nodes: %v", err)
	}
}

func TestDesktopRuntime_UserMissingVideoOrRenderFailsFatal(t *testing.T) {
	deps := newDeps(t, runtimeProfile(), nil)
	driDir := t.TempDir()
	seedDRINodes(t, driDir, 1, 1)
	nvmsPath := filepath.Join(t.TempDir(), "modeset")
	seedFile(t, nvmsPath, "Y\n")
	cmdlinePath := filepath.Join(t.TempDir(), "cmdline")
	seedFile(t, cmdlinePath,
		"nvidia-drm.modeset=1 nvidia-drm.fbdev=1 "+
			"drm.edid_firmware=DP-1:edid/virtual-4k120-hdr.bin video=DP-1:e\n")

	ph := DesktopRuntime{
		DRIDir:               driDir,
		NvidiaDRMModesetPath: nvmsPath,
		CmdlinePath:          cmdlinePath,
		UserGroupsFn: func(context.Context, *Deps, string) ([]string, error) {
			// Missing render group.
			return []string{"cloudgamer", "video"}, nil
		},
	}
	err := ph.Run(context.Background(), deps)
	if err == nil {
		t.Fatalf("expected fatal when user is missing render group")
	}
	if !strings.Contains(err.Error(), "render") {
		t.Errorf("error should mention render group: %v", err)
	}
}

func TestCmdlineTokenForConnector(t *testing.T) {
	if got := CmdlineTokenForConnector("DP-1", true); got != "video=DP-1:e" {
		t.Errorf("enabled: got %q want video=DP-1:e", got)
	}
	if got := CmdlineTokenForConnector("HDMI-A-1", false); got != "video=HDMI-A-1:d" {
		t.Errorf("disabled: got %q want video=HDMI-A-1:d", got)
	}
	if got := CmdlineTokenForConnector("", true); got != "" {
		t.Errorf("empty connector: got %q want empty", got)
	}
}

func TestCmdlineTokenForEdid(t *testing.T) {
	if got := CmdlineTokenForEdid("DP-1", "virtual-4k120-hdr.bin"); got != "drm.edid_firmware=DP-1:edid/virtual-4k120-hdr.bin" {
		t.Errorf("got %q", got)
	}
	if got := CmdlineTokenForEdid("", "x.bin"); got != "" {
		t.Errorf("empty conn: got %q want empty", got)
	}
}
