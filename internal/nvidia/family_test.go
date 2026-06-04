package nvidia

import (
	"errors"
	"strings"
	"testing"
)

// allFamiliesAvailable returns an Evidence with every Available* flag
// set and AvailabilityKnown=true. Used as a base for tests that don't
// care about apt availability (assume everything is installable).
func allFamiliesAvailable() Evidence {
	return Evidence{
		AvailabilityKnown:      true,
		AvailableServer:        true,
		AvailableServerOpen:    true,
		AvailableNonServer:     true,
		AvailableNonServerOpen: true,
	}
}

// TestSelectFamilyDecisionMatrix covers the v3 brief's nine scenarios
// plus the original hardware/profile-hint coverage. The table is the
// authoritative spec for the selector; failing a row means the rule
// changed semantically.
func TestSelectFamilyDecisionMatrix(t *testing.T) {
	type want struct {
		family    Family
		err       error
		reasonHas string
	}
	cases := []struct {
		name string
		ev   Evidence
		want want
	}{
		// ---- Original v3-brief scenarios (Milestone 1.1) ----

		{
			name: "RTX 5090 + server-open available -> server-open",
			ev: Evidence{
				PCIID:               "10de:2b85",
				GPUName:             "NVIDIA GeForce RTX 5090",
				IsBlackwellConsumer: true,
				PreferOpenFamily:    true,
				AvailabilityKnown:   true,
				AvailableServerOpen: true,
				AvailableServer:     true, // doesn't matter; open is required
			},
			want: want{family: FamilyServerOpen, reasonHas: "Blackwell consumer"},
		},
		{
			name: "RTX 5090 + only non-server-open available -> non-server-open",
			ev: Evidence{
				PCIID:                  "10de:2b85",
				GPUName:                "NVIDIA GeForce RTX 5090",
				IsBlackwellConsumer:    true,
				PreferOpenFamily:       true,
				AvailabilityKnown:      true,
				AvailableNonServerOpen: true,
			},
			want: want{family: FamilyNonServerOpen, reasonHas: "non-server-open"},
		},
		{
			name: "RTX 5090 + no open packages available -> ErrNoOpenAvailable",
			ev: Evidence{
				PCIID:               "10de:2b85",
				GPUName:             "NVIDIA GeForce RTX 5090",
				IsBlackwellConsumer: true,
				PreferOpenFamily:    true,
				AvailabilityKnown:   true,
				// Only the closed families are available.
				AvailableServer:    true,
				AvailableNonServer: true,
			},
			want: want{family: FamilyUnknown, err: ErrNoOpenAvailable, reasonHas: "Blackwell consumer"},
		},
		{
			// Live VM (2026-06): the 595 archive ships BOTH
			// nvidia-driver-595-open and -595-server-open. A cloud
			// gaming deploy (no prefer_server) must land on the
			// UDA / Game Ready non-server-open branch.
			name: "RTX 5090 + BOTH open families available + gaming default -> non-server-open (GRD/UDA)",
			ev: Evidence{
				PCIID:                  "10de:2b85",
				GPUName:                "NVIDIA GeForce RTX 5090",
				IsBlackwellConsumer:    true,
				PreferOpenFamily:       true,
				AvailabilityKnown:      true,
				AvailableServerOpen:    true,
				AvailableNonServerOpen: true,
			},
			want: want{family: FamilyNonServerOpen, reasonHas: "Blackwell consumer"},
		},
		{
			name: "RTX 5090 + BOTH open families available + PreferServerFamily -> server-open",
			ev: Evidence{
				PCIID:                  "10de:2b85",
				GPUName:                "NVIDIA GeForce RTX 5090",
				IsBlackwellConsumer:    true,
				PreferServerFamily:     true,
				AvailabilityKnown:      true,
				AvailableServerOpen:    true,
				AvailableNonServerOpen: true,
			},
			want: want{family: FamilyServerOpen, reasonHas: "Blackwell consumer"},
		},
		{
			name: "L4 + server available + PreferServerFamily -> server",
			ev: Evidence{
				PCIID:               "10de:27b8",
				GPUName:             "NVIDIA L4",
				IsDataCenter:        true,
				PreferServerFamily:  true,
				AvailabilityKnown:   true,
				AvailableServer:     true,
				AvailableServerOpen: true,
			},
			want: want{family: FamilyServer, reasonHas: "data-center"},
		},
		{
			name: "L4 + server unavailable + server-open available -> server-open",
			ev: Evidence{
				PCIID:               "10de:27b8",
				GPUName:             "NVIDIA L4",
				IsDataCenter:        true,
				PreferServerFamily:  true,
				AvailabilityKnown:   true,
				AvailableServerOpen: true,
			},
			want: want{family: FamilyServerOpen, reasonHas: "falling back to server-open"},
		},
		{
			name: "RTX 4090 + server installed + nvidia-smi works -> keep server even if prefer_open_family=true",
			ev: Evidence{
				PCIID:               "10de:2684",
				GPUName:             "NVIDIA GeForce RTX 4090",
				InstalledServer:     true,
				NvidiaSmiWorks:      true,
				PreferOpenFamily:    true,
				AvailabilityKnown:   true,
				AvailableServer:     true,
				AvailableServerOpen: true,
			},
			want: want{family: FamilyServer, reasonHas: "already loaded"},
		},
		{
			name: "Unknown NVIDIA GPU + server-open available -> server-open default",
			ev: Evidence{
				PCIID:               "10de:9999",
				AvailabilityKnown:   true,
				AvailableServerOpen: true,
				AvailableServer:     true,
			},
			want: want{family: FamilyServerOpen, reasonHas: "server-open"},
		},
		{
			name: "Unknown NVIDIA GPU + only server available -> server (fallback from server-open default)",
			ev: Evidence{
				PCIID:             "10de:9999",
				AvailabilityKnown: true,
				AvailableServer:   true,
			},
			want: want{family: FamilyServer, reasonHas: "falling back to server"},
		},
		{
			name: "dmesg requires open + server installed but nvidia-smi broken + server-open available -> server-open",
			ev: Evidence{
				PCIID:                         "10de:2684",
				GPUName:                       "NVIDIA GeForce RTX 4090",
				DmesgRequiresOpenKernelModule: true,
				InstalledServer:               true,
				NvidiaSmiWorks:                false,
				AvailabilityKnown:             true,
				AvailableServer:               true,
				AvailableServerOpen:           true,
			},
			want: want{family: FamilyServerOpen, reasonHas: "dmesg"},
		},

		// ---- Existing coverage carried over ----

		{
			name: "RTX 5090 + dmesg + all available -> server-open with dmesg reason",
			ev: Evidence{
				PCIID:                         "10de:2b85",
				IsBlackwellConsumer:           true,
				DmesgRequiresOpenKernelModule: true,
				PreferOpenFamily:              true,
				AvailabilityKnown:             true,
				AvailableServerOpen:           true,
			},
			want: want{family: FamilyServerOpen, reasonHas: "dmesg"},
		},
		{
			name: "RTX 4090 + server-open already installed + nvidia-smi works -> keep server-open",
			ev: Evidence{
				PCIID:               "10de:2684",
				GPUName:             "NVIDIA GeForce RTX 4090",
				InstalledServerOpen: true,
				NvidiaSmiWorks:      true,
				PreferOpenFamily:    true,
				AvailabilityKnown:   true,
				AvailableServerOpen: true,
			},
			want: want{family: FamilyServerOpen, reasonHas: "already loaded"},
		},
		{
			name: "L4 + no preference + all available -> default server-open",
			ev: Evidence{
				PCIID:               "10de:27b8",
				IsDataCenter:        true,
				AvailabilityKnown:   true,
				AvailableServer:     true,
				AvailableServerOpen: true,
			},
			want: want{family: FamilyServerOpen, reasonHas: "default"},
		},
		{
			name: "RTX PRO Blackwell workstation -> server-open (open required)",
			ev: Evidence{
				GPUName:             "NVIDIA RTX PRO 6000 Blackwell",
				IsBlackwellPro:      true,
				AvailabilityKnown:   true,
				AvailableServerOpen: true,
			},
			want: want{family: FamilyServerOpen, reasonHas: "Blackwell RTX PRO"},
		},
		{
			name: "B200 datacenter Blackwell -> server-open (open required)",
			ev: Evidence{
				GPUName:             "NVIDIA B200",
				IsBlackwellDC:       true,
				IsDataCenter:        true,
				AvailabilityKnown:   true,
				AvailableServerOpen: true,
			},
			want: want{family: FamilyServerOpen, reasonHas: "Blackwell datacenter"},
		},

		// ---- AvailabilityKnown semantics ----

		{
			name: "AvailabilityKnown=false + no Available* -> default server-open (optimistic)",
			ev: Evidence{
				PCIID: "10de:9999",
				// AvailabilityKnown deliberately false.
			},
			want: want{family: FamilyServerOpen, reasonHas: "default"},
		},
		{
			name: "AvailabilityKnown=true + no Available* + non-Blackwell -> ErrNoFamilyAvailable",
			ev: Evidence{
				PCIID:             "10de:9999",
				AvailabilityKnown: true,
			},
			want: want{family: FamilyUnknown, err: ErrNoFamilyAvailable, reasonHas: "no NVIDIA driver family available"},
		},
		{
			name: "AvailabilityKnown=true + Blackwell + no open -> ErrNoOpenAvailable",
			ev: Evidence{
				PCIID:               "10de:2b85",
				IsBlackwellConsumer: true,
				AvailabilityKnown:   true,
				AvailableServer:     true, // closed available, open not; Blackwell wants open
			},
			want: want{family: FamilyUnknown, err: ErrNoOpenAvailable, reasonHas: "Blackwell consumer"},
		},

		// ---- Soft PreferOpenFamily on a non-Blackwell GPU ----
		// These are the scenarios the v3 brief specifically asked us
		// to handle: PreferOpenFamily must NOT fail when only closed
		// packages are available on a GPU that does not require open.

		{
			name: "RTX 4090 + prefer_open_family=true + only server available -> server (no error)",
			ev: Evidence{
				PCIID:             "10de:2684",
				GPUName:           "NVIDIA GeForce RTX 4090",
				PreferOpenFamily:  true,
				AvailabilityKnown: true,
				AvailableServer:   true,
			},
			want: want{family: FamilyServer, reasonHas: "falling back to server"},
		},
		{
			name: "RTX 4090 + prefer_open_family=true + only non-server available -> non-server (no error)",
			ev: Evidence{
				PCIID:              "10de:2684",
				GPUName:            "NVIDIA GeForce RTX 4090",
				PreferOpenFamily:   true,
				AvailabilityKnown:  true,
				AvailableNonServer: true,
			},
			want: want{family: FamilyNonServer, reasonHas: "falling back to non-server"},
		},
		{
			name: "L4 + prefer_open_family=true + only server available -> server (no error; non-Blackwell so open is soft)",
			ev: Evidence{
				PCIID:             "10de:27b8",
				GPUName:           "NVIDIA L4",
				IsDataCenter:      true,
				PreferOpenFamily:  true,
				AvailabilityKnown: true,
				AvailableServer:   true,
			},
			want: want{family: FamilyServer, reasonHas: "falling back to server"},
		},
		{
			name: "RTX 5090 + only server available -> ErrNoOpenAvailable (hard open requirement)",
			ev: Evidence{
				PCIID:               "10de:2b85",
				GPUName:             "NVIDIA GeForce RTX 5090",
				IsBlackwellConsumer: true,
				AvailabilityKnown:   true,
				AvailableServer:     true,
			},
			want: want{family: FamilyUnknown, err: ErrNoOpenAvailable, reasonHas: "Blackwell consumer"},
		},
		{
			name: "dmesg open required + only server available -> ErrNoOpenAvailable",
			ev: Evidence{
				PCIID:                         "10de:2684",
				GPUName:                       "NVIDIA GeForce RTX 4090",
				DmesgRequiresOpenKernelModule: true,
				AvailabilityKnown:             true,
				AvailableServer:               true,
			},
			want: want{family: FamilyUnknown, err: ErrNoOpenAvailable, reasonHas: "dmesg"},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gotFam, gotReason, gotErr := SelectFamily(c.ev)
			if c.want.err != nil {
				if !errors.Is(gotErr, c.want.err) {
					t.Fatalf("SelectFamily err: got %v, want %v (reason=%q)", gotErr, c.want.err, gotReason)
				}
			} else if gotErr != nil {
				t.Fatalf("SelectFamily unexpected err: %v (reason=%q)", gotErr, gotReason)
			}
			if gotFam != c.want.family {
				t.Errorf("SelectFamily family: got %q want %q (reason=%q)", gotFam, c.want.family, gotReason)
			}
			if c.want.reasonHas != "" && !strings.Contains(strings.ToLower(gotReason), strings.ToLower(c.want.reasonHas)) {
				t.Errorf("SelectFamily reason: got %q does not contain %q", gotReason, c.want.reasonHas)
			}
		})
	}
}

// TestValidateInstalledRespectsInstalledFamily locks in the v2
// regression: validator must accept the installed family even if
// the profile would have picked a different one.
func TestValidateInstalledRespectsInstalledFamily(t *testing.T) {
	t.Run("server-open installed + nvidia-smi works -> validates as server-open", func(t *testing.T) {
		ev := Evidence{InstalledServerOpen: true, NvidiaSmiWorks: true}
		if err := ValidateInstalled(FamilyServerOpen, ev); err != nil {
			t.Errorf("expected nil, got %v", err)
		}
	})
	t.Run("server-open installed + validator expects server -> error mentions both", func(t *testing.T) {
		ev := Evidence{InstalledServerOpen: true, NvidiaSmiWorks: true}
		err := ValidateInstalled(FamilyServer, ev)
		if err == nil {
			t.Fatalf("expected error")
		}
		if !strings.Contains(err.Error(), "server-open") || !strings.Contains(err.Error(), "server") {
			t.Errorf("error should name both families: %v", err)
		}
	})
	t.Run("server-open installed + nvidia-smi broken -> error mentions dkms", func(t *testing.T) {
		ev := Evidence{InstalledServerOpen: true}
		err := ValidateInstalled(FamilyServerOpen, ev)
		if err == nil || !strings.Contains(err.Error(), "dkms") {
			t.Errorf("expected dkms hint, got %v", err)
		}
	})
	t.Run("nothing installed -> error", func(t *testing.T) {
		if err := ValidateInstalled(FamilyServerOpen, Evidence{}); err == nil {
			t.Fatalf("expected error")
		}
	})
	t.Run("two families installed -> multiple", func(t *testing.T) {
		ev := Evidence{InstalledServer: true, InstalledServerOpen: true}
		err := ValidateInstalled(FamilyServerOpen, ev)
		if err == nil || !strings.Contains(err.Error(), "multiple") {
			t.Errorf("expected multiple error, got %v", err)
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
			t.Errorf("%s.DriverPackage: got %q want %q", c.f, got, c.wantDriver)
		}
		if got := c.f.DkmsPackage(c.major); got != c.wantDkms {
			t.Errorf("%s.DkmsPackage: got %q want %q", c.f, got, c.wantDkms)
		}
	}
}

func TestFamilyClassifiers(t *testing.T) {
	cases := []struct {
		f      Family
		open   bool
		server bool
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

func TestEvidenceAvailability(t *testing.T) {
	// AvailabilityKnown=false -> every family looks available
	// (optimistic mode; doctor against a developer host).
	t.Run("availability unknown -> optimistic", func(t *testing.T) {
		none := Evidence{}
		if none.HasAvailabilityInfo() {
			t.Error("empty Evidence should report HasAvailabilityInfo=false")
		}
		for _, f := range AllFamilies() {
			if !none.IsAvailable(f) {
				t.Errorf("%s should be considered available with AvailabilityKnown=false", f)
			}
		}
	})

	// AvailabilityKnown=true + nothing flagged -> nothing available.
	// This is the new trinary state the v3 brief asked for.
	t.Run("availability known + all-false -> nothing available", func(t *testing.T) {
		known := Evidence{AvailabilityKnown: true}
		if !known.HasAvailabilityInfo() {
			t.Error("Evidence with AvailabilityKnown=true should report HasAvailabilityInfo=true")
		}
		for _, f := range AllFamilies() {
			if known.IsAvailable(f) {
				t.Errorf("%s must NOT be considered available with AvailabilityKnown=true + all flags false", f)
			}
		}
	})

	// AvailabilityKnown=true + partial info -> only the marked
	// families are available.
	t.Run("availability known + partial info", func(t *testing.T) {
		some := Evidence{AvailabilityKnown: true, AvailableServer: true}
		if !some.HasAvailabilityInfo() {
			t.Error("Evidence should report HasAvailabilityInfo=true")
		}
		if !some.IsAvailable(FamilyServer) {
			t.Error("FamilyServer should be reported available")
		}
		if some.IsAvailable(FamilyServerOpen) {
			t.Error("FamilyServerOpen should not be reported available")
		}
	})
}

func TestIsBlackwellConsumerPCIID(t *testing.T) {
	if !IsBlackwellConsumerPCIID("10de:2b85") {
		t.Error("RTX 5090 should classify as Blackwell consumer")
	}
	if IsBlackwellConsumerPCIID("10de:2684") {
		t.Error("RTX 4090 should not classify as Blackwell consumer")
	}
	if IsBlackwellConsumerPCIID("10de:27b8") {
		t.Error("L4 should not classify as Blackwell consumer")
	}
	if !IsBlackwellConsumerPCIID("10DE:2B85") {
		t.Error("PCI ID classification should be case-insensitive")
	}
}

func TestIsDataCenterPCIID(t *testing.T) {
	if !IsDataCenterPCIID("10de:27b8") {
		t.Error("L4 PCI ID should classify as data-center")
	}
	if !IsDataCenterPCIID("10de:2330") {
		t.Error("H100 PCI ID should classify as data-center")
	}
	if IsDataCenterPCIID("10de:2b85") {
		t.Error("RTX 5090 PCI ID should not classify as data-center")
	}
}

func TestParsePCIID(t *testing.T) {
	cases := []struct {
		line string
		want string
	}{
		{"01:00.0 VGA compatible controller [0300]: NVIDIA Corporation GB202 [GeForce RTX 5090] [10de:2b85] (rev a1)", "10de:2b85"},
		{"01:00.0 VGA compatible controller: NVIDIA Corporation L4 [10de:27b8] (rev a1)", "10de:27b8"},
		{"01:00.0 3D controller [0302]: NVIDIA Corporation AD104GL [L4] [10de:27b8] (rev a1)", "10de:27b8"},
		{"no pci id here", ""},
		{"[1234:notpci]", ""},
	}
	for _, c := range cases {
		if got := ParsePCIID(c.line); got != c.want {
			t.Errorf("ParsePCIID(%q): got %q want %q", c.line, got, c.want)
		}
	}
}
