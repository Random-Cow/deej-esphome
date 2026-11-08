package deej

import "testing"

func TestUnwrapALSAName(t *testing.T) {
	cases := map[string]string{
		"PipeWire ALSA [music-assistant-companion]": "music-assistant-companion",
		"PipeWire ALSA [ ]":                         "PipeWire ALSA [ ]",
		"PipeWire ALSA []":                          "PipeWire ALSA []",
		"Firefox":                                   "Firefox",
		"spotify":                                   "spotify",
	}

	for in, want := range cases {
		if got := unwrapALSAName(in); got != want {
			t.Errorf("unwrapALSAName(%q) = %q, want %q", in, got, want)
		}
	}
}
