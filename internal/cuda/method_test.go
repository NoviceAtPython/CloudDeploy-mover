package cuda

import "testing"

func TestParseMethod(t *testing.T) {
	cases := []struct {
		in   string
		want Method
		err  bool
	}{
		{"", MethodAuto, false},
		{"auto", MethodAuto, false},
		{"AUTO", MethodAuto, false},
		{"apt", MethodApt, false},
		{"runfile", MethodRunfile, false},
		{"none", MethodNone, false},
		{"yes-please", "", true},
	}
	for _, c := range cases {
		got, err := ParseMethod(c.in)
		if c.err {
			if err == nil {
				t.Errorf("ParseMethod(%q): expected error, got %q", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseMethod(%q): err=%v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("ParseMethod(%q): got %q want %q", c.in, got, c.want)
		}
	}
}
