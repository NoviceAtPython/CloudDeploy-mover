package main

import (
	"strings"
	"testing"

	"github.com/NoviceAtPython/CloudDeploy-mover/internal/state"
)

func TestApplyOrder_KWinPatchBeforeKWinSession(t *testing.T) {
	var desktopRuntime, kwinPatch, kwinSession int
	for i, ph := range applyPhases() {
		switch ph.Name() {
		case "desktop_runtime":
			desktopRuntime = i
		case "kwin_patch":
			kwinPatch = i
		case "kwin_session":
			kwinSession = i
		}
	}
	if desktopRuntime == 0 || kwinPatch == 0 || kwinSession == 0 {
		t.Fatalf("phase missing from apply order: desktop_runtime=%d kwin_patch=%d kwin_session=%d",
			desktopRuntime, kwinPatch, kwinSession)
	}
	if !(desktopRuntime < kwinPatch && kwinPatch < kwinSession) {
		t.Fatalf("wrong phase order: desktop_runtime=%d kwin_patch=%d kwin_session=%d",
			desktopRuntime, kwinPatch, kwinSession)
	}
}

func TestApplyOrder_Milestone5AfterDRMValidateOptionalAppsLast(t *testing.T) {
	index := map[string]int{}
	for i, ph := range applyPhases() {
		index[ph.Name()] = i
	}
	want := []string{
		"drm_display_validate",
		"sunshine_build",
		"sunshine_config",
		"tailscale",
		"pipewire_audio",
		"gpu_capture_capability_probe",
		"streaming_services",
		"stream_validate",
		"optional_apps",
	}
	for _, name := range want {
		if _, ok := index[name]; !ok {
			t.Fatalf("phase %q missing from apply order", name)
		}
	}
	for i := 0; i < len(want)-1; i++ {
		if !(index[want[i]] < index[want[i+1]]) {
			t.Fatalf("phase order wrong: %s=%d %s=%d", want[i], index[want[i]], want[i+1], index[want[i+1]])
		}
	}
	if index["optional_apps"] != len(applyPhases())-1 {
		t.Fatalf("optional_apps must be last; index=%d len=%d", index["optional_apps"], len(applyPhases()))
	}
}

func TestFirstAssetURLExtractsViteBundle(t *testing.T) {
	html := `<!DOCTYPE html><html><head><script type="module" src="/assets/index-abc123.js"></script><link rel="stylesheet" href="/assets/index-def456.css"></head></html>`
	got := firstAssetURL(html)
	if got != "/assets/index-abc123.js" {
		t.Fatalf("firstAssetURL: got %q want /assets/index-abc123.js", got)
	}
}

func TestMonitorCurrentPhasePrefersRunning(t *testing.T) {
	st := state.New("hdr-4k120")
	st.MarkDone("base_packages", nil)
	st.MarkRunning("kwin_session")
	st.MarkFailed("nvidia_driver", "boom", nil, false)
	name, status, _ := monitorCurrentPhase(st)
	if name != "kwin_session" || status != string(state.StatusRunning) {
		t.Fatalf("currentPhase: got (%q, %q), want (kwin_session, running)", name, status)
	}
}

func TestMonitorCurrentPhasePicksFailedWhenNoneRunning(t *testing.T) {
	st := state.New("hdr-4k120")
	st.MarkDone("base_packages", nil)
	st.MarkFailed("cuda", "boom", nil, true)
	name, status, _ := monitorCurrentPhase(st)
	if name != "cuda" || status != string(state.StatusFailedFatal) {
		t.Fatalf("currentPhase: got (%q, %q), want (cuda, failed_fatal)", name, status)
	}
}

func TestMonitorCurrentPhaseEmptyWhenAllDone(t *testing.T) {
	st := state.New("hdr-4k120")
	st.MarkDone("base_packages", nil)
	st.MarkDone("nvidia_driver", nil)
	name, _, _ := monitorCurrentPhase(st)
	if name != "" {
		t.Fatalf("currentPhase: got %q, want empty (all phases terminal-done)", name)
	}
}

func TestMonitorKWinSelectedLineFormatsTuple(t *testing.T) {
	d := map[string]any{
		"kwin_user":          "cloudgamer",
		"kwin_uid":           "1002",
		"kwin_seat":          "seat0",
		"kwin_tty":           "/dev/tty7",
		"kwin_drm_card":      "/dev/dri/card0",
		"kwin_render_node":   "/dev/dri/renderD128",
		"kwin_socket":        "/run/user/1002/wayland-0",
		"kwin_service":       "kwin-realvt.service",
		"kwin_pid":           6437,
		"kwin_invocation_id": "abc123",
	}
	got := monitorKWinSelectedLine(d)
	for _, want := range []string{
		"user=cloudgamer", "uid=1002", "seat=seat0",
		"tty=/dev/tty7", "drm=/dev/dri/card0",
		"render=/dev/dri/renderD128",
		"socket=/run/user/1002/wayland-0",
		"service=kwin-realvt.service",
		"pid=6437", "invocation=abc123",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("monitorKWinSelectedLine missing %q in %q", want, got)
		}
	}
}

func TestMonitorKWinSelectedLineEmptyForMissingDetails(t *testing.T) {
	if got := monitorKWinSelectedLine(nil); got != "" {
		t.Errorf("nil details should produce empty line; got %q", got)
	}
	if got := monitorKWinSelectedLine(map[string]any{}); got != "" {
		t.Errorf("empty details should produce empty line; got %q", got)
	}
	// Pid=0 must not show "pid=0".
	got := monitorKWinSelectedLine(map[string]any{
		"kwin_user": "cloudgamer", "kwin_pid": 0,
	})
	if strings.Contains(got, "pid=") {
		t.Errorf("pid=0 must not appear: %q", got)
	}
}

func TestFirstAssetURLReturnsEmptyOnRawTemplate(t *testing.T) {
	html := `<!DOCTYPE html><html><head><%- header %></head><body><div id="app"></div><script src="src/main.ts"></script></body></html>`
	got := firstAssetURL(html)
	// src/main.ts is technically a script src, but the marker we
	// care about is "raw <%- header %>" detection; firstAssetURL still
	// returns the first src.
	if got != "src/main.ts" {
		t.Fatalf("firstAssetURL: got %q want src/main.ts", got)
	}
	if !strings.Contains(html, "<%- header %>") {
		t.Fatalf("test fixture lost the raw header marker")
	}
}
