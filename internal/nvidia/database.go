// GPU classification database for the NVIDIA package-family selector.
//
// We classify a GPU into a (Category, Kind) pair from any of:
//
//   - exact PCI ID (most reliable; small list, expand as new SKUs ship);
//   - GPU name regex pattern (broad coverage for everything we have not
//     individually fingerprinted yet);
//   - "NVIDIA" + workstation/data-center/consumer cues in the name.
//
// Classify(name, pciID) returns the Category + Kind + a set of
// Evidence flags. The caller merges those flags into the live
// Evidence the selector consumes.
//
// The point of this file is to keep the family-selection rules in
// family.go pure and to make the GPU taxonomy table-driven so adding
// a new SKU is one regex / one PCI ID and one test row, not a new
// branch in SelectFamily.

package nvidia

import (
	"regexp"
	"strings"
)

// Category is the high-level NVIDIA product line that determines our
// preferred package family.
type Category int

const (
	CategoryUnknown                 Category = iota
	CategoryConsumerGeForce                  // GeForce RTX / GTX consumer cards
	CategoryWorkstationProfessional          // RTX PRO Blackwell, RTX Ada Pro, RTX A-series, Quadro
	CategoryDataCenter                       // L4 / L40 / A* / H* / B* (and legacy T4, V100, P*)
)

// Kind is finer-grained within a Category: which architecture/generation,
// and whether the closed kernel module is supported.
type Kind int

const (
	KindUnknown Kind = iota

	// Consumer GeForce
	KindGeForceBlackwell // RTX 50-series; closed module unsupported
	KindGeForceAda       // RTX 40-series
	KindGeForceAmpere    // RTX 30-series
	KindGeForceTuringRTX // RTX 20-series
	KindGeForceTuring16  // GTX 16-series; encode caveats

	// Workstation / professional
	KindWorkstationBlackwell // RTX PRO Blackwell workstation; closed unsupported
	KindWorkstationAda       // RTX 6000 Ada, RTX 5000 Ada, etc.
	KindWorkstationAmpereA   // RTX A-series (A4000, A5000, A6000)

	// Datacenter / cloud
	KindDataCenterBlackwell // B100 / B200 / GB200; closed unsupported
	KindDataCenterHopper    // H100 / H200
	KindDataCenterAdaL      // L4 / L40 / L40S
	KindDataCenterAmpere    // A10 / A16 / A40 / A100
	KindDataCenterTuringT4  // T4
	KindDataCenterVolta     // V100
	KindDataCenterPascal    // P100 / P40 / P4 -- legacy; no AV1, no HDR
)

// Classification is the result of Classify(name, pciID).
type Classification struct {
	Category Category
	Kind     Kind
	// HardEvidence are flags Classify wants to add to the Evidence
	// struct before SelectFamily runs. Currently used to set the
	// Blackwell / DataCenter / Pascal-legacy flags from a name match.
	HardEvidence Evidence
}

// Classify returns the Category + Kind + Evidence flags for an
// NVIDIA GPU identified by name and/or PCI ID. Empty inputs return
// CategoryUnknown. PCI ID match (when present) wins over name regex.
func Classify(name, pciID string) Classification {
	// Try the small exact-PCI-ID table first. It is exhaustive only
	// for SKUs CloudDeploy has fingerprinted; everything else falls
	// through to the name-regex layer.
	if pciID != "" {
		if c, ok := classifyByPCIID(pciID); ok {
			return c
		}
	}
	// Name-regex layer. We compile patterns once at init().
	if name != "" {
		if c, ok := classifyByName(name); ok {
			return c
		}
	}
	return Classification{}
}

// ---- Exact PCI-ID table ----
//
// Lowercase only; the lookup is case-insensitive. Add new entries as
// the deploy fleet grows. Generic name-regex coverage in
// classifyByName picks up SKUs not listed here.

var pciTable = map[string]Classification{
	// Blackwell consumer (RTX 50-series)
	"10de:2b85": {Category: CategoryConsumerGeForce, Kind: KindGeForceBlackwell, HardEvidence: Evidence{IsBlackwellConsumer: true}},
	"10de:2b87": {Category: CategoryConsumerGeForce, Kind: KindGeForceBlackwell, HardEvidence: Evidence{IsBlackwellConsumer: true}}, // 5090 D
	"10de:2c02": {Category: CategoryConsumerGeForce, Kind: KindGeForceBlackwell, HardEvidence: Evidence{IsBlackwellConsumer: true}}, // 5080

	// Ada consumer
	"10de:2684": {Category: CategoryConsumerGeForce, Kind: KindGeForceAda}, // 4090
	"10de:2685": {Category: CategoryConsumerGeForce, Kind: KindGeForceAda}, // 4090 D
	"10de:2705": {Category: CategoryConsumerGeForce, Kind: KindGeForceAda}, // 4070 Ti SUPER
	"10de:2782": {Category: CategoryConsumerGeForce, Kind: KindGeForceAda}, // 4070 Ti
	"10de:2786": {Category: CategoryConsumerGeForce, Kind: KindGeForceAda}, // 4070

	// Ampere consumer
	"10de:2204": {Category: CategoryConsumerGeForce, Kind: KindGeForceAmpere}, // 3090
	"10de:2208": {Category: CategoryConsumerGeForce, Kind: KindGeForceAmpere}, // 3080 Ti
	"10de:2206": {Category: CategoryConsumerGeForce, Kind: KindGeForceAmpere}, // 3080

	// Datacenter Ada-L
	"10de:27b8": {Category: CategoryDataCenter, Kind: KindDataCenterAdaL, HardEvidence: Evidence{IsDataCenter: true}}, // L4
	"10de:26b9": {Category: CategoryDataCenter, Kind: KindDataCenterAdaL, HardEvidence: Evidence{IsDataCenter: true}}, // L40S
	"10de:26b5": {Category: CategoryDataCenter, Kind: KindDataCenterAdaL, HardEvidence: Evidence{IsDataCenter: true}}, // L40

	// Datacenter Ampere
	"10de:2235": {Category: CategoryDataCenter, Kind: KindDataCenterAmpere, HardEvidence: Evidence{IsDataCenter: true}}, // A10
	"10de:25b6": {Category: CategoryDataCenter, Kind: KindDataCenterAmpere, HardEvidence: Evidence{IsDataCenter: true}}, // A16
	"10de:20b5": {Category: CategoryDataCenter, Kind: KindDataCenterAmpere, HardEvidence: Evidence{IsDataCenter: true}}, // A100 80GB PCIe
	"10de:20f1": {Category: CategoryDataCenter, Kind: KindDataCenterAmpere, HardEvidence: Evidence{IsDataCenter: true}}, // A100 40GB SXM4
	"10de:2236": {Category: CategoryDataCenter, Kind: KindDataCenterAmpere, HardEvidence: Evidence{IsDataCenter: true}}, // A40

	// Datacenter Hopper
	"10de:2330": {Category: CategoryDataCenter, Kind: KindDataCenterHopper, HardEvidence: Evidence{IsDataCenter: true}}, // H100 80GB SXM5
	"10de:2331": {Category: CategoryDataCenter, Kind: KindDataCenterHopper, HardEvidence: Evidence{IsDataCenter: true}}, // H100 80GB PCIe
	"10de:2335": {Category: CategoryDataCenter, Kind: KindDataCenterHopper, HardEvidence: Evidence{IsDataCenter: true}}, // H200

	// Datacenter Blackwell - PCI IDs are placeholders/best-guess
	// until NVIDIA publishes them or CloudDeploy fingerprints a real
	// card. The name-regex layer covers these regardless.
	// "10de:????": {Category: CategoryDataCenter, Kind: KindDataCenterBlackwell, HardEvidence: Evidence{IsDataCenter: true, IsBlackwellDC: true}},

	// Datacenter Turing
	"10de:1eb8": {Category: CategoryDataCenter, Kind: KindDataCenterTuringT4, HardEvidence: Evidence{IsDataCenter: true}}, // T4

	// Datacenter Volta
	"10de:1db4": {Category: CategoryDataCenter, Kind: KindDataCenterVolta, HardEvidence: Evidence{IsDataCenter: true}}, // V100 16GB PCIe
	"10de:1db5": {Category: CategoryDataCenter, Kind: KindDataCenterVolta, HardEvidence: Evidence{IsDataCenter: true}}, // V100 32GB SXM2
	"10de:1db6": {Category: CategoryDataCenter, Kind: KindDataCenterVolta, HardEvidence: Evidence{IsDataCenter: true}}, // V100 32GB PCIe

	// Datacenter Pascal (legacy)
	"10de:15f7": {Category: CategoryDataCenter, Kind: KindDataCenterPascal, HardEvidence: Evidence{IsDataCenter: true, IsLegacyPascal: true}}, // P100 12GB PCIe
	"10de:15f8": {Category: CategoryDataCenter, Kind: KindDataCenterPascal, HardEvidence: Evidence{IsDataCenter: true, IsLegacyPascal: true}}, // P100 16GB PCIe
	"10de:1b38": {Category: CategoryDataCenter, Kind: KindDataCenterPascal, HardEvidence: Evidence{IsDataCenter: true, IsLegacyPascal: true}}, // P40
	"10de:1bb3": {Category: CategoryDataCenter, Kind: KindDataCenterPascal, HardEvidence: Evidence{IsDataCenter: true, IsLegacyPascal: true}}, // P4
}

func classifyByPCIID(pciID string) (Classification, bool) {
	c, ok := pciTable[strings.ToLower(pciID)]
	return c, ok
}

// ---- Name-regex layer ----
//
// Ordered: first match wins. More specific patterns must come before
// more general ones (e.g. "RTX PRO" before generic "RTX").

type namePattern struct {
	re       *regexp.Regexp
	classify func(name string) Classification
}

var namePatterns []namePattern

func init() {
	register := func(pattern string, c Classification) {
		namePatterns = append(namePatterns, namePattern{
			re: regexp.MustCompile("(?i)" + pattern),
			classify: func(_ string) Classification {
				return c
			},
		})
	}

	// ---- Datacenter Blackwell (most specific first) ----
	register(`GB200`, Classification{Category: CategoryDataCenter, Kind: KindDataCenterBlackwell, HardEvidence: Evidence{IsDataCenter: true, IsBlackwellDC: true}})
	register(`\bB200\b`, Classification{Category: CategoryDataCenter, Kind: KindDataCenterBlackwell, HardEvidence: Evidence{IsDataCenter: true, IsBlackwellDC: true}})
	register(`\bB100\b`, Classification{Category: CategoryDataCenter, Kind: KindDataCenterBlackwell, HardEvidence: Evidence{IsDataCenter: true, IsBlackwellDC: true}})

	// ---- Datacenter Hopper ----
	register(`\bH200\b`, Classification{Category: CategoryDataCenter, Kind: KindDataCenterHopper, HardEvidence: Evidence{IsDataCenter: true}})
	register(`\bH100\b`, Classification{Category: CategoryDataCenter, Kind: KindDataCenterHopper, HardEvidence: Evidence{IsDataCenter: true}})

	// ---- Datacenter Ada-L (L4 / L40 / L40S) ----
	register(`\bL40S?\b`, Classification{Category: CategoryDataCenter, Kind: KindDataCenterAdaL, HardEvidence: Evidence{IsDataCenter: true}})
	register(`\bL4\b`, Classification{Category: CategoryDataCenter, Kind: KindDataCenterAdaL, HardEvidence: Evidence{IsDataCenter: true}})

	// ---- Datacenter Ampere (A100 / A40 / A16 / A10) ----
	register(`\bA100\b`, Classification{Category: CategoryDataCenter, Kind: KindDataCenterAmpere, HardEvidence: Evidence{IsDataCenter: true}})
	register(`\bA40\b`, Classification{Category: CategoryDataCenter, Kind: KindDataCenterAmpere, HardEvidence: Evidence{IsDataCenter: true}})
	register(`\bA16\b`, Classification{Category: CategoryDataCenter, Kind: KindDataCenterAmpere, HardEvidence: Evidence{IsDataCenter: true}})
	register(`\bA10\b`, Classification{Category: CategoryDataCenter, Kind: KindDataCenterAmpere, HardEvidence: Evidence{IsDataCenter: true}})

	// ---- Datacenter Turing T4 ----
	register(`\bT4\b`, Classification{Category: CategoryDataCenter, Kind: KindDataCenterTuringT4, HardEvidence: Evidence{IsDataCenter: true}})

	// ---- Datacenter Volta V100 ----
	register(`\bV100\b`, Classification{Category: CategoryDataCenter, Kind: KindDataCenterVolta, HardEvidence: Evidence{IsDataCenter: true}})

	// ---- Datacenter Pascal legacy (P100 / P40 / P4) ----
	register(`\bP100\b`, Classification{Category: CategoryDataCenter, Kind: KindDataCenterPascal, HardEvidence: Evidence{IsDataCenter: true, IsLegacyPascal: true}})
	register(`\bP40\b`, Classification{Category: CategoryDataCenter, Kind: KindDataCenterPascal, HardEvidence: Evidence{IsDataCenter: true, IsLegacyPascal: true}})
	register(`\bP4\b`, Classification{Category: CategoryDataCenter, Kind: KindDataCenterPascal, HardEvidence: Evidence{IsDataCenter: true, IsLegacyPascal: true}})

	// ---- Workstation Blackwell (RTX PRO 6000 Blackwell, etc.) ----
	register(`RTX\s*PRO.*Blackwell`, Classification{Category: CategoryWorkstationProfessional, Kind: KindWorkstationBlackwell, HardEvidence: Evidence{IsBlackwellPro: true}})
	register(`RTX\s*PRO\b`, Classification{Category: CategoryWorkstationProfessional, Kind: KindWorkstationBlackwell, HardEvidence: Evidence{IsBlackwellPro: true}})

	// ---- Workstation Ada Professional (RTX 6000 Ada, RTX 5000 Ada, etc.) ----
	// Must appear before the consumer-Ada matcher so "RTX 6000 Ada" is
	// classified as professional, not consumer.
	register(`RTX\s*\d{4}\s*Ada`, Classification{Category: CategoryWorkstationProfessional, Kind: KindWorkstationAda})

	// ---- Workstation Ampere A-series (RTX A4000 / A5000 / A6000) ----
	register(`RTX\s*A\d{4}\b`, Classification{Category: CategoryWorkstationProfessional, Kind: KindWorkstationAmpereA})

	// ---- Consumer GeForce, generation by generation ----
	register(`GeForce\s*RTX\s*50\d{2}\b`, Classification{Category: CategoryConsumerGeForce, Kind: KindGeForceBlackwell, HardEvidence: Evidence{IsBlackwellConsumer: true}})
	register(`GeForce\s*RTX\s*40\d{2}\b`, Classification{Category: CategoryConsumerGeForce, Kind: KindGeForceAda})
	register(`GeForce\s*RTX\s*30\d{2}\b`, Classification{Category: CategoryConsumerGeForce, Kind: KindGeForceAmpere})
	register(`GeForce\s*RTX\s*20\d{2}\b`, Classification{Category: CategoryConsumerGeForce, Kind: KindGeForceTuringRTX})
	register(`GeForce\s*GTX\s*16\d{2}\b`, Classification{Category: CategoryConsumerGeForce, Kind: KindGeForceTuring16})
}

func classifyByName(name string) (Classification, bool) {
	for _, p := range namePatterns {
		if p.re.MatchString(name) {
			return p.classify(name), true
		}
	}
	return Classification{}, false
}

// String makes Category and Kind self-describing in log lines and
// test output.

func (c Category) String() string {
	switch c {
	case CategoryConsumerGeForce:
		return "consumer-geforce"
	case CategoryWorkstationProfessional:
		return "workstation-professional"
	case CategoryDataCenter:
		return "datacenter"
	}
	return "unknown"
}

func (k Kind) String() string {
	switch k {
	case KindGeForceBlackwell:
		return "geforce-blackwell"
	case KindGeForceAda:
		return "geforce-ada"
	case KindGeForceAmpere:
		return "geforce-ampere"
	case KindGeForceTuringRTX:
		return "geforce-turing-rtx"
	case KindGeForceTuring16:
		return "geforce-turing-16"
	case KindWorkstationBlackwell:
		return "workstation-blackwell"
	case KindWorkstationAda:
		return "workstation-ada"
	case KindWorkstationAmpereA:
		return "workstation-ampere-a-series"
	case KindDataCenterBlackwell:
		return "datacenter-blackwell"
	case KindDataCenterHopper:
		return "datacenter-hopper"
	case KindDataCenterAdaL:
		return "datacenter-ada-l"
	case KindDataCenterAmpere:
		return "datacenter-ampere"
	case KindDataCenterTuringT4:
		return "datacenter-turing-t4"
	case KindDataCenterVolta:
		return "datacenter-volta"
	case KindDataCenterPascal:
		return "datacenter-pascal"
	}
	return "unknown"
}

// SupportsAV1Encode reports whether this Kind has an NVENC AV1
// encoder. Used by `doctor` to surface a clear "no AV1 on this GPU"
// message rather than letting Sunshine fail later.
func (k Kind) SupportsAV1Encode() bool {
	switch k {
	case KindGeForceBlackwell, KindGeForceAda,
		KindWorkstationBlackwell, KindWorkstationAda,
		KindDataCenterBlackwell, KindDataCenterHopper, KindDataCenterAdaL:
		return true
	}
	return false
}

// SupportsHDRStreaming reports whether the GPU + driver path can
// deliver the AV1 10-bit HDR success state. AV1 encode is the gating
// requirement.
func (k Kind) SupportsHDRStreaming() bool {
	return k.SupportsAV1Encode()
}
