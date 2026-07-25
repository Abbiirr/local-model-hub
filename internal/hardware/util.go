package hardware

import "math"

// round2 rounds a float to 2 decimal places for tidy JSON (GiB values).
func round2(f float64) float64 {
	return math.Round(f*100) / 100
}
