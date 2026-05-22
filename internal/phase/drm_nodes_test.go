package phase

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRenderNodeForCardWithSysfsMatchesByPCIIdentity(t *testing.T) {
	sysfs := t.TempDir()
	pci := filepath.Join(t.TempDir(), "pci-0000-01-00-0")
	if err := os.MkdirAll(pci, 0o755); err != nil {
		t.Fatal(err)
	}
	card := filepath.Join(sysfs, "card1")
	render := filepath.Join(sysfs, "renderD128")
	if err := os.MkdirAll(card, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(render, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(card, "device"), []byte(pci), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(render, "device"), []byte(pci), 0o644); err != nil {
		t.Fatal(err)
	}

	got := RenderNodeForCardWithSysfs("/dev/dri/card1", sysfs)
	if got != "/dev/dri/renderD128" {
		t.Fatalf("RenderNodeForCardWithSysfs(card1): got %q want /dev/dri/renderD128", got)
	}
}

func TestRenderNodeForCardWithSysfsDoesNotGuessMissingRenderD129(t *testing.T) {
	sysfs := t.TempDir()
	pciCard := filepath.Join(t.TempDir(), "pci-0000-01-00-0")
	pciOther := filepath.Join(t.TempDir(), "pci-0000-02-00-0")
	for _, p := range []string{pciCard, pciOther, filepath.Join(sysfs, "card1"), filepath.Join(sysfs, "renderD128")} {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(sysfs, "card1", "device"), []byte(pciCard), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sysfs, "renderD128", "device"), []byte(pciOther), 0o644); err != nil {
		t.Fatal(err)
	}

	got := RenderNodeForCardWithSysfs("/dev/dri/card1", sysfs)
	if got != "" {
		t.Fatalf("must not infer renderD129 or mismatched render node; got %q", got)
	}
}
