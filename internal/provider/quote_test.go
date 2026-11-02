package provider

import (
	"errors"
	"math"
	"testing"
)

func TestConvert(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name         string
		amount, rate int64
		want         int64
		invalid      bool
	}{
		{"exact", 1_000_000, 73_500_000, 735_000, false},
		{"floors", 999, 73_500_000, 734, false},
		{"par at the largest amount", math.MaxInt64, rateScale, math.MaxInt64, false},
		{"a tiny rate at the largest amount", math.MaxInt64, 1, math.MaxInt64 / rateScale, false},
		{"overflows 64 bits", math.MaxInt64, math.MaxInt64, 0, true},
		{"just over 64 bits", math.MaxInt64, rateScale + 1, 0, true},
		{"converts to nothing", 1, 1, 0, true},
		{"zero amount", 0, rateScale, 0, true},
		{"negative amount", -5, rateScale, 0, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got, err := convert(c.amount, c.rate)
			var validation *ValidationError
			if c.invalid != errors.As(err, &validation) || (!c.invalid && err != nil) || got != c.want {
				t.Fatalf("convert(%d, %d) = %d, %v; want %d, invalid %v", c.amount, c.rate, got, err, c.want, c.invalid)
			}
		})
	}
}
