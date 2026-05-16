package nvidia

import (
	"strings"
	"testing"
)

// TestSelectFamilyDecisionMatrix covers the eight scenarios listed in
// the v3 brief plus a handful of edge cases. The table is the
// authoritative spec for the selector.
func TestSelectFamilyDecisionMatrix(t *testing.T) {
	cases := []struct {
		name       string
		ev         Evidence
		want       Family
		reasonHas  string // substring expected in the human-readable reason
	}{
		{
			name: "RTX 5090 / GB202 fresh install -> server-open",
			ev: Evidence{
				PCIID:               "10de:2b85",
				GPUName:             "NVIDIA GeForce RTX 5090",
				IsBlackwellConsumer: true,
				PreferOpenFamily:    true,
			},
			want:      FamilyServerOpen,
			reasonHas: "Blackwell",
		},
		{
			name: "RTX 5090 + dmesg open required wins over Blackwell heuristic",
			ev: Evidence{
				PCIID:                         "10de:2b85",
				IsBlackwellConsumer:           true,
				DmesgRequiresOpenKernelModule: true,
				PreferOpenFamily:              true,
			},
			want:      FamilyServerOpen,
			reasonHas: "dmesg",
		},
		{
			name: "RTX 4090 fresh install + PreferServerFamily false -> server-open default",
			ev: Evidence{
				PCIID:            "10de:2684",
				GPUName:          "NVIDIA GeForce RTX 4090",
				PreferOpenFamily: true,
			},
			want:      FamilyServerOpen,
			reasonHas: "open",
		},
		{
			name: "RTX 4090 + server-open already installed + nvidia-smi works -> keep server-open",
			ev: Evidence{
				PCIID:               "10de:2684",
				GPUName:             "NVIDIA GeForce RTX 4090",
				InstalledServerOpen: true,
				NvidiaSmiWorks:      true,
				PreferOpenFamily:    true,
			},
			want:      FamilyServerOpen,
			reasonHas: "already loaded",
		},
		{
			name: "RTX 4090 + server already installed + nvidia-smi works -> keep server (v2 regression test)",
			ev: Evidence{
				PCIID:           "10de:2684",
				GPUName:         "NVIDIA GeForce RTX 4090",
				InstalledServer: true,
				NvidiaSmiWorks:  true,
				// Note: PreferOpenFamily would normally point at open;
				// the "installed family works" rule MUST override so
				// the deploy doesn't uninstall a working driver.
				PreferOpenFamily: true,
			},
			want:      FamilyServer,
			reasonHas: "already loaded",
		},
		{
			name: "L4 data-center + PreferServerFamily -> server",
			ev: Evidence{
				PCIID:              "10de:27b8",
				GPUName:            "NVIDIA L4",
				IsDataCenter:       true,
				PreferServerFamily: true,
			},
			want:      FamilyServer,
			reasonHas: "data-center",
		},
		{
			name: "L4 data-center + no preference -> default server-open",
			ev: Evidence{
				PCIID:        "10de:27b8",
				GPUName:      "NVIDIA L4",
				IsDataCenter: true,
			},
			want:      FamilyServerOpen,
			reasonHas: "default",
		},
		{
			name: "dmesg open required on a non-Blackwell GPU still wins",
			ev: Evidence{
				PCIID:                         "10de:2684",
				GPUName:                       "NVIDIA GeForce RTX 4090",
				DmesgRequiresOpenKernelModule: true,
				PreferServerFamily:            true,
			},
			want:      FamilyServerOpen,
			reasonHas: "dmesg",
		},
		{
			name: "Unknown GPU + no preferences -> default server-open",
			ev:   Evidence{PCIID: "10de:9999"},
			want: FamilyServerOpen,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gotFam, gotReason := SelectFamily(c.ev)
			if gotFam != c.want {
				t.Errorf("SelectFamily: got %q want %q (reason: %s)", gotFam, c.want, gotReason)
			}
			if c.reasonHas != "" && !strings.Contains(strings.ToLower(gotReason), strings.ToLower(c.reasonHas)) {
				t.Errorf("SelectFamily reason: got %q does not contain %q", gotReason, c.reasonHas)
			}
		})
	}
}

// TestValidateInstalledRespectsInstalledFamily is the v2-regression
// test: validation MUST accept the installed family even if a naive
// "expected" computation would pick a different one. The bug we are
// preventing: v2's validator demanded nvidia-driver-580-server while
// the operator had nvidia-driver-580-server-open installed and
// working.
func TestValidateInstalledRespectsInstalledFamily(t *testing.T) {
	t.Run("server-open installed + nvidia-smi works -> validates as server-open", func(t *testing.T) {
		ev := Evidence{
			InstalledServerOpen: true,
			NvidiaSmiWorks:      true,
		}
		if err := ValidateInstalled(FamilyServerOpen, ev); err != nil {
			t.Errorf("expected nil, got %v", err)
		}
	})
	t.Run("server-open installed + validator expects server -> error mentions mismatch", func(t *testing.T) {
		ev := Evidence{
			InstalledServerOpen: true,
			NvidiaSmiWorks:      true,
		}
		err := ValidateInstalled(FamilyServer, ev)
		if err == nil {
			t.Fatalf("expected error, got nil")
		}
		if !strings.Contains(err.Error(), "server-open") || !strings.Contains(err.Error(), "server") {
			t.Errorf("error should name both families: %v", err)
		}
	})
	t.Run("server-open installed + nvidia-smi broken -> error mentions dkms", func(t *testing.T) {
		ev := Evidence{
			InstalledServerOpen: true,
			NvidiaSmiWorks:      false,
		}
		err := ValidateInstalled(FamilyServerOpen, ev)
		if err == nil {
			t.Fatalf("expected error, got nil")
		}
		if !strings.Contains(err.Error(), "dkms") {
			t.Errorf("error should suggest dkms/initramfs: %v", err)
		}
	})
	t.Run("nothing installed -> error", func(t *testing.T) {
		err := ValidateInstalled(FamilyServerOpen, Evidence{})
		if err == nil {
			t.Fatalf("expected error, got nil")
		}
	})
	t.Run("two families installed -> error", func(t *testing.T) {
		ev := Evidence{
			InstalledServer:     true,
			InstalledServerOpen: true,
		}
		err := ValidateInstalled(FamilyServerOpen, ev)
		if err == nil {
			t.Fatalf("expected error, got nil")
		}
		if !strings.Contains(err.Error(), "multiple") {
			t.Errorf("error should say multiple: %v", err)
		}
	})
}

func TestFamilyPackages(t *testing.T) {
	cases := []struct {
		f          Family
		major      string
		wantDriver string
		wantDkms   string
	}{
		{FamilyServer, "580", "nvidia-driver-580-server", "nvidia-dkms-580-server"},
		{FamilyServerOpen, "580", "nvidia-driver-580-server-open", "nvidia-dkms-580-server-open"},
		{FamilyNonServer, "580", "nvidia-driver-580", "nvidia-dkms-580"},
		{FamilyNonServerOpen, "580", "nvidia-driver-580-open", "nvidia-dkms-580-open"},
	}
	for _, c := range cases {
		if got := c.f.DriverPackage(c.major); got != c.wantDriver {
			t.Errorf("%s.DriverPackage(%s): got %q want %q", c.f, c.major, got, c.wantDriver)
		}
		if got := c.f.DkmsPackage(c.major); got != c.wantDkms {
			t.Errorf("%s.DkmsPackage(%s): got %q want %q", c.f, c.major, got, c.wantDkms)
		}
	}
}

func TestFamilyClassifiers(t *testing.T) {
	cases := []struct {
		f         Family
		open      bool
		server    bool
	}{
		{FamilyServer, false, true},
		{FamilyServerOpen, true, true},
		{FamilyNonServer, false, false},
		{FamilyNonServerOpen, true, false},
	}
	for _, c := range cases {
		if c.f.IsOpen() != c.open {
			t.Errorf("%s.IsOpen: got %v want %v", c.f, c.f.IsOpen(), c.open)
		}
		if c.f.IsServer() != c.server {
			t.Errorf("%s.IsServer: got %v want %v", c.f, c.f.IsServer(), c.server)
		}
	}
}

func TestIsBlackwellConsumerPCIID(t *testing.T) {
	if !IsBlackwellConsumerPCIID("10de:2b85") {
		t.Error("RTX 5090 PCI ID should classify as Blackwell consumer")
	}
	if IsBlackwellConsumerPCIID("10de:2684") {
		t.Error("RTX 4090 PCI ID should NOT classify as Blackwell consumer")
	}
	if IsBlackwellConsumerPCIID("10de:27b8") {
		t.Error("L4 PCI ID should NOT classify as Blackwell consumer")
	}
	if !IsBlackwellConsumerPCIID("10DE:2B85") {
		t.Error("PCI ID matching should be case-insensitive")
	}
}

func TestParsePCIID(t *testing.T) {
	cases := []struct {
		line string
		want string
	}{
		{
			line: "01:00.0 VGA compatible controller [0300]: NVIDIA Corporation GB202 [GeForce RTX 5090] [10de:2b85] (rev a1)",
			want: "10de:2b85",
		},
		{
			line: "01:00.0 VGA compatible controller: NVIDIA Corporation L4 [10de:27b8] (rev a1)",
			want: "10de:27b8",
		},
		{
			// Class code [0300] is not a PCI ID; the function should
			// keep looking and find the real one.
			line: "01:00.0 3D controller [0302]: NVIDIA Corporation AD104GL [L4] [10de:27b8] (rev a1)",
			want: "10de:27b8",
		},
		{
			line: "no pci id here",
			want: "",
		},
		{
			line: "[1234:notpci]",
			want: "",
		},
	}
	for _, c := range cases {
		t.Run(c.line, func(t *testing.T) {
			if got := ParsePCIID(c.line); got != c.want {
				t.Errorf("got %q want %q", got, c.want)
			}
		})
	}
}
