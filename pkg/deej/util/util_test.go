package util

import "testing"

func TestNormalizeScalar(t *testing.T) {
	cases := map[float32]float32{
		0:       0,
		0.15442: 0.15,
		0.9:     0.9,
		0.29:    0.29,
		1:       1,
	}

	for in, want := range cases {
		if got := NormalizeScalar(in); got != want {
			t.Errorf("NormalizeScalar(%v) = %v, want %v", in, got, want)
		}
	}
}
