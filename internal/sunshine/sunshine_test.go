package sunshine

import (
	"strings"
	"testing"
)

func TestDecodeCodecModeSupportMain10Flags(t *testing.T) {
	raw := SCMH264 | SCMHEVC | SCMHEVCMain10 | SCMAV1Main8 | SCMAV1Main10
	got := DecodeCodecModeSupport(raw)
	if !got.H264 || !got.HEVC || !got.HEVCMain10 || !got.AV1Main8 || !got.AV1Main10 {
		t.Fatalf("decoded support missing expected flags: %+v", got)
	}
	names := strings.Join(got.Names(), ",")
	for _, want := range []string{"H264", "HEVC", "HEVC_MAIN10", "AV1_MAIN8", "AV1_MAIN10"} {
		if !strings.Contains(names, want) {
			t.Fatalf("Names() missing %q: %s", want, names)
		}
	}
}

func TestServerInfoValueAndInt(t *testing.T) {
	xml := `<root status_code="200"><ServerCodecModeSupport>197377</ServerCodecModeSupport><MaxLumaPixelsHEVC>1869449984</MaxLumaPixelsHEVC></root>`
	if got := ServerInfoValue(xml, "ServerCodecModeSupport"); got != "197377" {
		t.Fatalf("ServerCodecModeSupport: got %q", got)
	}
	if got, ok := ServerInfoInt(xml, "MaxLumaPixelsHEVC"); !ok || got != 1869449984 {
		t.Fatalf("MaxLumaPixelsHEVC: got %d ok=%v", got, ok)
	}
}
