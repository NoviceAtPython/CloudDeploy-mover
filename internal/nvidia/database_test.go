package nvidia

import (
	"testing"
)

// TestClassifyByName covers the broad name-regex taxonomy. The PCI-ID
// table is its own concern (tested via TestClassifyByPCIID).
func TestClassifyByName(t *testing.T) {
	cases := []struct {
		name      string
		gpuName   string
		wantCat   Category
		wantKind  Kind
		blackwell bool
		dc        bool
		legacy    bool
	}{
		// Datacenter Blackwell.
		{"B200 datacenter", "NVIDIA B200", CategoryDataCenter, KindDataCenterBlackwell, false, true, false},
		{"B100 datacenter", "NVIDIA B100", CategoryDataCenter, KindDataCenterBlackwell, false, true, false},
		{"GB200 superchip", "NVIDIA GB200 Grace Blackwell", CategoryDataCenter, KindDataCenterBlackwell, false, true, false},

		// Datacenter Hopper.
		{"H100 SXM5", "NVIDIA H100 80GB HBM3", CategoryDataCenter, KindDataCenterHopper, false, true, false},
		{"H200", "NVIDIA H200", CategoryDataCenter, KindDataCenterHopper, false, true, false},

		// Datacenter Ada-L.
		{"L4", "NVIDIA L4", CategoryDataCenter, KindDataCenterAdaL, false, true, false},
		{"L40S", "NVIDIA L40S", CategoryDataCenter, KindDataCenterAdaL, false, true, false},
		{"L40", "NVIDIA L40", CategoryDataCenter, KindDataCenterAdaL, false, true, false},

		// Datacenter Ampere.
		{"A100", "NVIDIA A100 80GB PCIe", CategoryDataCenter, KindDataCenterAmpere, false, true, false},
		{"A40", "NVIDIA A40", CategoryDataCenter, KindDataCenterAmpere, false, true, false},
		{"A16", "NVIDIA A16", CategoryDataCenter, KindDataCenterAmpere, false, true, false},
		{"A10", "NVIDIA A10", CategoryDataCenter, KindDataCenterAmpere, false, true, false},

		// Datacenter Turing T4.
		{"T4", "NVIDIA Tesla T4", CategoryDataCenter, KindDataCenterTuringT4, false, true, false},

		// Datacenter Volta.
		{"V100 PCIe", "NVIDIA Tesla V100-PCIE-16GB", CategoryDataCenter, KindDataCenterVolta, false, true, false},

		// Datacenter Pascal legacy.
		{"P100 16GB", "NVIDIA Tesla P100-PCIE-16GB", CategoryDataCenter, KindDataCenterPascal, false, true, true},
		{"P40", "NVIDIA Tesla P40", CategoryDataCenter, KindDataCenterPascal, false, true, true},
		{"P4", "NVIDIA Tesla P4", CategoryDataCenter, KindDataCenterPascal, false, true, true},

		// Workstation Blackwell pro.
		{"RTX PRO Blackwell", "NVIDIA RTX PRO 6000 Blackwell Workstation Edition", CategoryWorkstationProfessional, KindWorkstationBlackwell, false, false, false},
		{"RTX PRO bare", "NVIDIA RTX PRO 5000", CategoryWorkstationProfessional, KindWorkstationBlackwell, false, false, false},

		// Workstation Ada pro must beat the consumer regex.
		{"RTX 6000 Ada", "NVIDIA RTX 6000 Ada Generation", CategoryWorkstationProfessional, KindWorkstationAda, false, false, false},
		{"RTX 5000 Ada", "NVIDIA RTX 5000 Ada Generation", CategoryWorkstationProfessional, KindWorkstationAda, false, false, false},

		// Workstation Ampere A-series.
		{"RTX A6000", "NVIDIA RTX A6000", CategoryWorkstationProfessional, KindWorkstationAmpereA, false, false, false},
		{"RTX A4000", "NVIDIA RTX A4000", CategoryWorkstationProfessional, KindWorkstationAmpereA, false, false, false},

		// Consumer GeForce.
		{"RTX 5090", "NVIDIA GeForce RTX 5090", CategoryConsumerGeForce, KindGeForceBlackwell, true, false, false},
		{"RTX 5080", "NVIDIA GeForce RTX 5080", CategoryConsumerGeForce, KindGeForceBlackwell, true, false, false},
		{"RTX 4090", "NVIDIA GeForce RTX 4090", CategoryConsumerGeForce, KindGeForceAda, false, false, false},
		{"RTX 4070 Ti", "NVIDIA GeForce RTX 4070 Ti", CategoryConsumerGeForce, KindGeForceAda, false, false, false},
		{"RTX 3090", "NVIDIA GeForce RTX 3090", CategoryConsumerGeForce, KindGeForceAmpere, false, false, false},
		{"RTX 2080", "NVIDIA GeForce RTX 2080", CategoryConsumerGeForce, KindGeForceTuringRTX, false, false, false},
		{"GTX 1660", "NVIDIA GeForce GTX 1660", CategoryConsumerGeForce, KindGeForceTuring16, false, false, false},

		// Unknown.
		{"junk", "NVIDIA Mystery 9001", CategoryUnknown, KindUnknown, false, false, false},
		{"empty", "", CategoryUnknown, KindUnknown, false, false, false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cls := Classify(c.gpuName, "")
			if cls.Category != c.wantCat {
				t.Errorf("Category: got %s want %s", cls.Category, c.wantCat)
			}
			if cls.Kind != c.wantKind {
				t.Errorf("Kind: got %s want %s", cls.Kind, c.wantKind)
			}
			if c.blackwell != cls.HardEvidence.IsBlackwellConsumer {
				t.Errorf("IsBlackwellConsumer: got %v want %v for %s", cls.HardEvidence.IsBlackwellConsumer, c.blackwell, c.gpuName)
			}
			if cls.HardEvidence.IsDataCenter != c.dc {
				t.Errorf("IsDataCenter: got %v want %v", cls.HardEvidence.IsDataCenter, c.dc)
			}
			if cls.HardEvidence.IsLegacyPascal != c.legacy {
				t.Errorf("IsLegacyPascal: got %v want %v", cls.HardEvidence.IsLegacyPascal, c.legacy)
			}
		})
	}
}

// TestClassifyByPCIID picks up the small handful of known SKUs.
func TestClassifyByPCIID(t *testing.T) {
	cases := []struct {
		name     string
		pciID    string
		wantCat  Category
		wantKind Kind
	}{
		{"RTX 5090", "10de:2b85", CategoryConsumerGeForce, KindGeForceBlackwell},
		{"RTX 4090", "10de:2684", CategoryConsumerGeForce, KindGeForceAda},
		{"L4", "10de:27b8", CategoryDataCenter, KindDataCenterAdaL},
		{"L40S", "10de:26b9", CategoryDataCenter, KindDataCenterAdaL},
		{"A100 80GB PCIe", "10de:20b5", CategoryDataCenter, KindDataCenterAmpere},
		{"H100 SXM5", "10de:2330", CategoryDataCenter, KindDataCenterHopper},
		{"T4", "10de:1eb8", CategoryDataCenter, KindDataCenterTuringT4},
		{"V100 PCIe 16GB", "10de:1db4", CategoryDataCenter, KindDataCenterVolta},
		{"P40", "10de:1b38", CategoryDataCenter, KindDataCenterPascal},

		// Unknown PCI ID falls through to name regex (which will also
		// fail with an empty name, returning CategoryUnknown).
		{"junk pciid", "10de:ffff", CategoryUnknown, KindUnknown},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cls := Classify("", c.pciID)
			if cls.Category != c.wantCat {
				t.Errorf("Category: got %s want %s", cls.Category, c.wantCat)
			}
			if cls.Kind != c.wantKind {
				t.Errorf("Kind: got %s want %s", cls.Kind, c.wantKind)
			}
		})
	}
}

func TestKindSupportsAV1Encode(t *testing.T) {
	supports := []Kind{
		KindGeForceBlackwell, KindGeForceAda,
		KindWorkstationBlackwell, KindWorkstationAda,
		KindDataCenterBlackwell, KindDataCenterHopper, KindDataCenterAdaL,
	}
	doesNot := []Kind{
		KindGeForceAmpere, KindGeForceTuringRTX, KindGeForceTuring16,
		KindWorkstationAmpereA,
		KindDataCenterAmpere, KindDataCenterTuringT4,
		KindDataCenterVolta, KindDataCenterPascal,
		KindUnknown,
	}
	for _, k := range supports {
		if !k.SupportsAV1Encode() {
			t.Errorf("%s should support AV1 encode", k)
		}
	}
	for _, k := range doesNot {
		if k.SupportsAV1Encode() {
			t.Errorf("%s should NOT support AV1 encode", k)
		}
	}
}

func TestKindSupportsHDRStreaming(t *testing.T) {
	// HDR streaming requirement is the same as AV1 encode.
	for _, k := range []Kind{KindGeForceBlackwell, KindGeForceAda, KindDataCenterAdaL} {
		if !k.SupportsHDRStreaming() {
			t.Errorf("%s should support HDR streaming", k)
		}
	}
	for _, k := range []Kind{KindDataCenterPascal, KindDataCenterVolta, KindGeForceTuring16} {
		if k.SupportsHDRStreaming() {
			t.Errorf("%s should NOT support HDR streaming", k)
		}
	}
}
