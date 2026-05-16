package state

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")

	st := New("hdr-4k120")
	st.MarkDone("base_packages", map[string]interface{}{
		"installed": []string{"git", "cmake"},
	})
	st.MarkSkipped("cuda", "not required for KMS/NVENC/HDR path")
	st.MarkRunning("nvidia_driver")
	st.MarkDone("nvidia_driver", map[string]interface{}{
		"package_family": "server-open",
		"driver_version": "580.126.20",
	})

	if err := st.Save(path); err != nil {
		t.Fatalf("save: %v", err)
	}

	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if loaded.Profile != "hdr-4k120" {
		t.Errorf("profile mismatch: got %q", loaded.Profile)
	}
	if loaded.Get("base_packages").Status != StatusDone {
		t.Errorf("base_packages status: got %q want done", loaded.Get("base_packages").Status)
	}
	if loaded.Get("cuda").Status != StatusSkipped {
		t.Errorf("cuda status: got %q want skipped", loaded.Get("cuda").Status)
	}
	if loaded.Get("cuda").Reason != "not required for KMS/NVENC/HDR path" {
		t.Errorf("cuda reason: got %q", loaded.Get("cuda").Reason)
	}
	if loaded.Get("nvidia_driver").Status != StatusDone {
		t.Errorf("nvidia_driver status: got %q want done", loaded.Get("nvidia_driver").Status)
	}
	if loaded.Get("nvidia_driver").Details["package_family"] != "server-open" {
		t.Errorf("nvidia_driver details lost: got %#v", loaded.Get("nvidia_driver").Details)
	}
}

func TestSaveIsAtomic(t *testing.T) {
	// Verify Save uses temp + rename, not in-place write. Inspecting
	// the directory between Save calls should show no half-written
	// state file.
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")

	st := New("hdr-4k120")
	st.MarkDone("base_packages", nil)
	if err := st.Save(path); err != nil {
		t.Fatalf("save: %v", err)
	}
	// Now mutate and save again; confirm the previous content is fully
	// replaced and no stray temp file lingers.
	st.MarkDone("nvidia_driver", map[string]interface{}{"driver_version": "580.126.20"})
	if err := st.Save(path); err != nil {
		t.Fatalf("save (2): %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	for _, e := range entries {
		if e.Name() != "state.json" {
			t.Errorf("stray file after atomic save: %s", e.Name())
		}
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var got State
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Get("nvidia_driver").Details["driver_version"] != "580.126.20" {
		t.Errorf("second save did not take: %#v", got.Get("nvidia_driver"))
	}
}

func TestLoadVersionMismatch(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	// Pretend a future version wrote this file.
	if err := os.WriteFile(path, []byte(`{"version":99,"phases":{}}`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := Load(path); err == nil {
		t.Fatalf("expected version-mismatch error, got nil")
	}
}

func TestLoadMissingFile(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "does-not-exist.json"))
	if err == nil {
		t.Fatalf("expected error loading missing file")
	}
	if !os.IsNotExist(err) {
		t.Fatalf("expected os.IsNotExist, got %v", err)
	}
}

func TestPhaseStatusTerminal(t *testing.T) {
	cases := map[PhaseStatus]bool{
		StatusPending:                 false,
		StatusRunning:                 false,
		StatusDone:                    true,
		StatusSkipped:                 true,
		StatusFailedFatal:             true,
		StatusFailedNonfatal:          true,
		StatusPendingMoonlightConnect: true,
	}
	for status, want := range cases {
		if got := status.IsTerminal(); got != want {
			t.Errorf("status %q IsTerminal: got %v want %v", status, got, want)
		}
	}
}
