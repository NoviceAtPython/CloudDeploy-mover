package phase

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestSelectCaptureBackendPrefersWaylandThenNvFBC(t *testing.T) {
	if got := selectCaptureBackend(backendProbeResult{WaylandKMSNVENCHDR: true, X11NvFBCNVENC: true}); got != BackendWaylandKMSNVENCHDR {
		t.Fatalf("got %q want wayland", got)
	}
	if got := selectCaptureBackend(backendProbeResult{X11NvFBCNVENC: true, X11NVENCFallback: true}); got != BackendX11NvFBCNVENC {
		t.Fatalf("got %q want nvfbc", got)
	}
	if got := selectCaptureBackend(backendProbeResult{X11NVENCFallback: true}); got != BackendX11NVENCFallback {
		t.Fatalf("got %q want x11 fallback", got)
	}
	if got := selectCaptureBackend(backendProbeResult{}); got != BackendNone {
		t.Fatalf("got %q want none", got)
	}
}

func TestClassifySunshineKMSInteropFailureEGL(t *testing.T) {
	failed, reason := classifySunshineKMSInteropFailure("Couldn't open EGL display: [00003000]\nEncoder [nvenc] failed\nVideo failed to find working encoder", nil)
	if !failed {
		t.Fatalf("expected EGL/NVENC interop failure")
	}
	if !strings.Contains(reason, "egl display") {
		t.Fatalf("reason should mention EGL display, got %q", reason)
	}
}

func TestGPUCaptureCapabilityProbeFailsWhenSunshineKMSEGLInteropFails(t *testing.T) {
	deps := milestone5Deps(t)
	deps.DryRun = false
	ph := GPUCaptureCapabilityProbe{
		NVENCProbeFn: func(context.Context, *Deps, string, string) error { return nil },
		SunshineStartupProbeFn: func(context.Context, *Deps, string, string, string, string) (string, error) {
			return "STREAM_DIAG kms capture selected drm_device=/dev/dri/card1 connector=DP-1 width=3840 height=2160\nCouldn't open EGL display: [00003000]\nEncoder [nvenc] failed\nVideo failed to find working encoder", nil
		},
	}
	// Avoid depending on the developer host's /dev/dri or EGL while still
	// exercising the backend failure classifier through the phase: dry-run KMS
	// is not allowed here, so this phase may fail before Sunshine on Windows.
	// The direct classifier test above pins the RTX 3090 log regression; this
	// smoke asserts the phase records a terminal failure rather than marking done.
	err := ph.Run(context.Background(), deps)
	if err == nil {
		t.Fatalf("expected capability probe to fail on developer host / EGL failure")
	}
	if deps.State.Get(GPUCaptureCapabilityProbeName).Status == "done" {
		t.Fatalf("capability probe must not mark done when backend does not pass")
	}
}

func TestStreamingServicesRejectsUnsupportedSelectedBackend(t *testing.T) {
	deps := milestone5Deps(t)
	deps.DryRun = false
	deps.State.MarkDone(GPUCaptureCapabilityProbeName, map[string]any{"selected_backend": BackendX11NvFBCNVENC})
	err := (StreamingServices{}).Run(context.Background(), deps)
	if err == nil {
		t.Fatalf("expected unsupported selected backend to fail until X11/NvFBC renderer exists")
	}
	if !strings.Contains(err.Error(), "unsupported selected capture backend") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestProbeResultErrorClassifiesUnknownError(t *testing.T) {
	failed, reason := classifySunshineKMSInteropFailure("", errors.New("boom"))
	if !failed || !strings.Contains(reason, "boom") {
		t.Fatalf("got failed=%v reason=%q", failed, reason)
	}
}
