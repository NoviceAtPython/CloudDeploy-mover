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
