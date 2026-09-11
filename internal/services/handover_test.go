package services

import (
	"strings"
	"testing"

	"github.com/shopspring/decimal"
)

// People write the same Indonesian mobile four different ways. Normalising is
// what turns "the code never arrived" from a formatting problem into a real
// failure worth investigating.
func TestWhatsAppNumberNormalisation(t *testing.T) {
	cases := []struct{ in, want string }{
		{"081234567890", "6281234567890"},
		{"+6281234567890", "6281234567890"},
		{"6281234567890", "6281234567890"},
		{"0812-3456-7890", "6281234567890"},
		{"0812 3456 7890", "6281234567890"},
		{"81234567890", "6281234567890"},
	}

	for _, c := range cases {
		got, err := normaliseWhatsApp(c.in)
		if err != nil {
			t.Errorf("normaliseWhatsApp(%q) errored: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("normaliseWhatsApp(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestWhatsAppNumberRejectsNonsense(t *testing.T) {
	for _, in := range []string{"", "12", "not a number", "0812345678901234567"} {
		if _, err := normaliseWhatsApp(in); err == nil {
			t.Errorf("normaliseWhatsApp(%q) was accepted", in)
		}
	}
}

// A code of 000042 must stay six digits. Trimmed to "42" it would never match
// what the PIC reads out, and the failure would look like a wrong code.
func TestGeneratedCodeIsAlwaysSixDigits(t *testing.T) {
	for i := 0; i < 500; i++ {
		code, err := generateCode()
		if err != nil {
			t.Fatalf("generateCode: %v", err)
		}
		if len(code) != handoverCodeDigits {
			t.Fatalf("generateCode() = %q, want %d digits", code, handoverCodeDigits)
		}
		if strings.Trim(code, "0123456789") != "" {
			t.Fatalf("generateCode() = %q, want digits only", code)
		}
	}
}

func TestGeneratedCodesDiffer(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		code, _ := generateCode()
		seen[code] = true
	}
	// 200 draws from a million values collide vanishingly rarely; a generator
	// stuck on one value is what this catches.
	if len(seen) < 190 {
		t.Errorf("only %d distinct codes in 200 draws", len(seen))
	}
}

// Volume is derived from the dimensions and stored, so a client cannot submit
// one that disagrees with its own measurements.
func TestItemVolumeIsDerivedFromDimensions(t *testing.T) {
	dec := func(v string) *decimal.Decimal {
		d, _ := decimal.NewFromString(v)
		return &d
	}

	// 100 x 50 x 40 cm = 200,000 cm³ = 0.2 m³.
	items := itemsFrom([]OrderItemInput{{
		Name:     "CPO Drum",
		LengthCm: dec("100"), WidthCm: dec("50"), HeightCm: dec("40"),
	}})

	if items[0].VolumeM3 == nil {
		t.Fatal("volume should be derived when all three dimensions are given")
	}
	if got := items[0].VolumeM3.String(); got != "0.2" {
		t.Errorf("volume = %s, want 0.2", got)
	}
}

// A partial entry must leave volume blank rather than treating a missing height
// as zero, which would make a crate of any size occupy nothing.
func TestPartialDimensionsLeaveVolumeBlank(t *testing.T) {
	dec := func(v string) *decimal.Decimal {
		d, _ := decimal.NewFromString(v)
		return &d
	}

	items := itemsFrom([]OrderItemInput{{
		Name: "Garmen Carton", LengthCm: dec("50"), WidthCm: dec("40"),
	}})

	if items[0].VolumeM3 != nil {
		t.Errorf("volume = %v, want nil when a dimension is missing", items[0].VolumeM3)
	}
}
