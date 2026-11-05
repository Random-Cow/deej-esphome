package deej

import "testing"

func TestEscapeMenuLabel(t *testing.T) {
	cases := map[string]string{
		"Sliders: input_0:100%  input_1:67%": "Sliders: input__0:100%  input__1:67%",
		"Sessions: 4 active":                 "Sessions: 4 active",
	}

	for in, want := range cases {
		if got := escapeMenuLabel(in); got != want {
			t.Errorf("escapeMenuLabel(%q) = %q, want %q", in, got, want)
		}
	}
}
