package services

import "testing"

func TestCrossed(t *testing.T) {
	cases := []struct {
		name               string
		was, known, within bool
		want               bool
	}{
		{"first sight, outside", false, false, false, false},
		{"first sight, inside", false, false, true, true},
		{"stays outside", false, true, false, false},
		{"enters", false, true, true, true},
		{"stays inside", true, true, true, false},
		{"leaves", true, true, false, true},
	}
	for _, c := range cases {
		if got := crossed(c.was, c.known, c.within); got != c.want {
			t.Errorf("%s: crossed(%v,%v,%v)=%v want %v", c.name, c.was, c.known, c.within, got, c.want)
		}
	}
}

func TestHaversineAtWarehouseScale(t *testing.T) {
	// Two points ~111 m apart along a meridian near Jakarta.
	d := haversineMeters(-6.2000, 106.8000, -6.2010, 106.8000)
	if d < 105 || d > 117 {
		t.Fatalf("expected ~111 m, got %.1f", d)
	}
	if haversineMeters(-6.2, 106.8, -6.2, 106.8) != 0 {
		t.Fatal("same point must be zero")
	}
}
