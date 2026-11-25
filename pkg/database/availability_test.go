package database

import (
	"strings"
	"testing"
)

// The board model is the cross-lane capacity truth: per-lane quotas are
// independent and allow physical overcommit (16 whole + 4x 7g.141gb passes
// every lane but needs 20 boards on 16 physical).
func TestBoardCapacityCheck(t *testing.T) {
	tests := []struct {
		name      string
		totals    map[string]int
		feasible  bool
		mentions  string // substring expected in the refusal reason
	}{
		{
			name:     "empty books nothing",
			totals:   map[string]int{},
			feasible: true,
		},
		{
			name:     "mike's overcommit: 16 whole + 4x 7g needs 20 boards on 16",
			totals:   map[string]int{"h200-gpu": 16, "h200-mig-7g-141gb": 4},
			feasible: false,
			mentions: "flex-board slices",
		},
		{
			name:     "12 whole + 4x 7g exactly saturates the flex pool",
			totals:   map[string]int{"h200-gpu": 12, "h200-mig-7g-141gb": 4},
			feasible: true,
		},
		{
			name:     "all small slices fit the flex pool",
			totals:   map[string]int{"h200-mig-1g-18gb": 56},
			feasible: true,
		},
		{
			name:     "57 small slices do not (placement cap 56)",
			totals:   map[string]int{"h200-mig-1g-18gb": 57},
			feasible: false,
			mentions: "maximum",
		},
		{
			name:     "8 near-whole partitions fill the flex pool",
			totals:   map[string]int{"h200-mig-7g-141gb": 8},
			feasible: true,
		},
		{
			name:     "9 near-whole partitions do not (lane cap 8)",
			totals:   map[string]int{"h200-mig-7g-141gb": 9},
			feasible: false,
			mentions: "maximum",
		},
		{
			name:     "17 whole bookings exceed the physical boards",
			totals:   map[string]int{"h200-gpu": 17},
			feasible: false,
			mentions: "physical boards",
		},
		{
			name:     "mixed profiles sum their slice costs (52 of 64)",
			totals:   map[string]int{"h200-mig-1g-18gb": 28, "h200-mig-3g-71gb": 4, "h200-mig-4g-71gb": 2},
			feasible: true, // 28x1 + 4x4 + 2x4 = 52
		},
		{
			name:     "mixed profiles exceeding the flex pool are refused",
			totals:   map[string]int{"h200-mig-1g-18gb": 28, "h200-mig-3g-71gb": 8, "h200-mig-4g-71gb": 4},
			feasible: false, // 28 + 32 + 16 = 76 > 64
			mentions: "flex-board slices",
		},
		{
			name:     "16 whole with no partitions fills every board",
			totals:   map[string]int{"h200-gpu": 16},
			feasible: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			msg := BoardCapacityCheck(tc.totals)
			if tc.feasible && msg != "" {
				t.Fatalf("expected feasible, refused: %s", msg)
			}
			if !tc.feasible {
				if msg == "" {
					t.Fatal("expected refusal, got none")
				}
				if tc.mentions != "" && !strings.Contains(msg, tc.mentions) {
					t.Errorf("refusal %q does not mention %q", msg, tc.mentions)
				}
			}
		})
	}
}
