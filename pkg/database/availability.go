package database

import (
	"fmt"
)

// Physical board model for the Dynamic MIG Booking flex design
// (~/.opencode/plans/dynamic-mig-booking.md, decisions 2026-11-25).
//
// The H200 mirror runs 16 boards in two pools:
//   - 8 dedicated boards: whole-only, no MIG surface (the carve cap).
//   - 8 flex boards: whole XOR partitions — a flex board's 8-slice grid is
//     consumed either by one whole allocation or by carved partitions
//     (DRA shared-CounterSet exclusivity enforces this at allocation).
//
// Per-lane Kueue quotas are independent, so lane math alone allows physical
// overcommit: 16 whole + 4x 7g.141gb passes every lane but needs 20 boards.
// This model is the cross-lane check the booking paths run before insert.
const (
	DedicatedBoards = 8
	FlexBoards      = 8
	SlicesPerBoard  = 8
)

// wholeLaneType is the catalog lane for whole boards.
const wholeLaneType = "h200-gpu"

// migSliceCost maps a MIG lane type to the number of grid slices one
// partition consumes on a flex board — from the validated H200 placement
// table (nvidia-smi mig -lgipp, 2026-11-25): 1g.18gb {0..6}:1, 1g.35gb
// {0,2,4,6}:2, 2g.35gb {0,2,4}:2, 3g.71gb {0,4}:4, 4g.71gb {0}:4,
// 7g.141gb {0}:8.
var migSliceCost = map[string]int{
	"h200-mig-1g-18gb":  1,
	"h200-mig-1g-35gb":  2,
	"h200-mig-2g-35gb":  2,
	"h200-mig-3g-71gb":  4,
	"h200-mig-4g-71gb":  4,
	"h200-mig-7g-141gb": 8,
}

// migLaneMaxima maps a MIG lane type to its physical maximum — table
// `instances` x 8 flex boards. The placement grid constrains some lanes
// below the slice-count math (1g.18gb has 7 placements per board, not 8 —
// position 7 is not a valid 1-slice placement), so this is the lane cap,
// independent of the slice-sum feasibility.
var migLaneMaxima = map[string]int{
	"h200-mig-1g-18gb":  56,
	"h200-mig-1g-35gb":  32,
	"h200-mig-2g-35gb":  24,
	"h200-mig-3g-71gb":  16,
	"h200-mig-4g-71gb":  8,
	"h200-mig-7g-141gb": 8,
}

// CheckDateCapacity loads the date's booked lane totals, applies delta, and
// returns the feasibility refusal ("" when feasible). Fails open on a DB
// error — the DRA scheduler's counter exclusivity remains the final
// authority at activation.
func CheckDateCapacity(date string, delta map[string]int) string {
	totals := map[string]int{}
	rows, err := DB().Query("SELECT resource, COUNT(*) FROM bookings WHERE date = ? GROUP BY resource", date)
	if err != nil {
		return ""
	}
	defer rows.Close()
	for rows.Next() {
		var res string
		var n int
		if err := rows.Scan(&res, &n); err != nil {
			continue
		}
		totals[res] = n
	}
	for k, v := range delta {
		totals[k] += v
	}
	return BoardCapacityCheck(totals)
}

// BoardCapacityCheck simulates per-date lane totals against the physical
// board model and returns "" when an assignment exists, else a
// human-readable reason for the refusal.
//
// Packing: whole bookings fill the dedicated boards first (8), then flex
// boards at 8 slices each — the optimal assignment, so feasibility here is
// exact for the whole lane and slice-count-exact for MIG (fragmentation can
// only reduce real availability further; the DRA scheduler is the final
// authority at activation).
func BoardCapacityCheck(totals map[string]int) string {
	whole := totals[wholeLaneType]
	if whole > DedicatedBoards+FlexBoards {
		return fmt.Sprintf("%d whole bookings exceed the %d physical boards", whole, DedicatedBoards+FlexBoards)
	}

	for lane, count := range totals {
		if max, ok := migLaneMaxima[lane]; ok && count > max {
			return fmt.Sprintf("%d bookings on lane %s exceed its %d-partition maximum (%d placements per board x %d flex boards)", count, lane, max, max/FlexBoards, FlexBoards)
		}
	}

	migSlices := 0
	for lane, count := range totals {
		if cost, ok := migSliceCost[lane]; ok {
			migSlices += count * cost
		}
	}
	overflowWhole := whole - DedicatedBoards
	if overflowWhole < 0 {
		overflowWhole = 0
	}
	flexDemand := overflowWhole*SlicesPerBoard + migSlices
	if limit := FlexBoards * SlicesPerBoard; flexDemand > limit {
		return fmt.Sprintf(
			"bookings need %d flex-board slices (%d whole beyond the %d dedicated boards x %d slices + %d MIG slices), capacity is %d",
			flexDemand, overflowWhole, DedicatedBoards, SlicesPerBoard, migSlices, limit)
	}
	return ""
}
