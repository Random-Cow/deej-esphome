package deej

import "testing"

func TestFindSliderIndex(t *testing.T) {
	esphome := &ESPHome{
		sliderNames: []string{"input_0", "input_1", "input_2"},
	}

	cases := []struct {
		name string
		id   string
		want int
	}{
		{"current format", "sensor/Input 0", 0},
		{"current format, different index", "sensor/Input 1", 1},
		{"current format, sub-device", "sensor/Garage/Input 2", 2},
		{"legacy format", "sensor-input_0", 0},
		{"no match", "sensor/WiFi Signal dB", -1},
		{"unrelated legacy id", "sensor-uptime", -1},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := esphome.findSliderIndex(c.id); got != c.want {
				t.Errorf("findSliderIndex(%q) = %d, want %d", c.id, got, c.want)
			}
		})
	}
}

func TestSanitizeEntityName(t *testing.T) {
	cases := map[string]string{
		"Input 0":        "input_0",
		"input_0":        "input_0",
		"WiFi Signal dB": "wifi_signal_db",
		"  Trim Me  ":    "trim_me",
	}

	for in, want := range cases {
		if got := sanitizeEntityName(in); got != want {
			t.Errorf("sanitizeEntityName(%q) = %q, want %q", in, got, want)
		}
	}
}
