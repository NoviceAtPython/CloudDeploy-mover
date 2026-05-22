package main

import "testing"

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
