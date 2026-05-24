package phase

import (
	"context"
	"strings"
	"testing"

	"github.com/NoviceAtPython/CloudDeploy-mover/internal/state"
)

func TestDesktopPackages_DefaultListIncludesCompositorAndKscreen(t *testing.T) {
	// Sanity: the default list MUST contain the packages downstream
	// phases shell out to.
	must := []string{"kwin-wayland", "xwayland", "plasma-desktop", "dolphin", "systemsettings", "kscreen", "dbus-user-session", "pipewire", "qt6-wayland", "drm-info"}
	have := map[string]bool{}
	for _, p := range DefaultDesktopPackages {
		have[p] = true
	}
	for _, m := range must {
		if !have[m] {
			t.Errorf("DefaultDesktopPackages missing required entry %q", m)
		}
	}
}

func TestGamingEnvironmentEnablesPlayStationHIDRaw(t *testing.T) {
	for _, want := range []string{
		"PROTON_ENABLE_HIDRAW=1",
		"SDL_JOYSTICK_HIDAPI_PS5=1",
		"SDL_JOYSTICK_HIDAPI_PS4=1",
	} {
		if !strings.Contains(gamingEnvironmentBody, want) {
			t.Fatalf("gaming environment missing %q:\n%s", want, gamingEnvironmentBody)
		}
	}
}

func TestWrapSteamDesktopEntryAddsPlayStationEnv(t *testing.T) {
	body := []byte("[Desktop Entry]\nName=Install Steam\nExec=/usr/games/steam %U\n")
	got := string(wrapSteamDesktopEntry(body))
	for _, want := range []string{
		"Name=Steam",
		"Exec=env PROTON_ENABLE_HIDRAW=1 SDL_JOYSTICK_HIDAPI_PS5=1 SDL_JOYSTICK_HIDAPI_PS4=1 /usr/games/steam %U",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("wrapped Steam desktop entry missing %q:\n%s", want, got)
		}
	}
}

func TestDesktopPackages_RunMarksDone(t *testing.T) {
	deps := newDeps(t, desktopProfile(), nil)
	// apt.Transaction in tests runs in DryRun -> apt-get is a no-op.
	if err := (DesktopPackages{
		// Trim to a tiny list so the test stays fast and doesn't
		// depend on the real default.
		PackagesOverrideFn: func() []string {
			return []string{"kwin-wayland", "kscreen", "drm-info"}
		},
	}).Run(context.Background(), deps); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := deps.State.Get(DesktopPackagesName).Status; got != state.StatusDone {
		t.Errorf("status: got %q want done", got)
	}
	d := deps.State.Get(DesktopPackagesName).Details
	pkgs, _ := d["packages"].([]string)
	if len(pkgs) != 3 {
		t.Errorf("packages: got %v want 3 entries", pkgs)
	}
}

func TestDesktopPackages_SkipsWhenAlreadyDone(t *testing.T) {
	deps := newDeps(t, desktopProfile(), nil)
	deps.State.MarkDone(DesktopPackagesName, map[string]any{"packages": []string{}})
	called := false
	ph := DesktopPackages{
		PackagesOverrideFn: func() []string {
			called = true
			return nil
		},
	}
	if err := ph.Run(context.Background(), deps); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if called {
		t.Errorf("PackagesOverrideFn must not run when phase already done")
	}
}
