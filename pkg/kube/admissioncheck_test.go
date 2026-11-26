package kube

import (
	"strings"
	"testing"
	"time"

	"github.com/eformat/gpu-booking-plugin/pkg/database"
)

func TestLaneByClassMap(t *testing.T) {
	database.SetGPUConfig(&database.GPUConfig{
		Resources: []database.GPUResourceSpec{
			{Type: "h200-gpu", DRAMode: true, Isolation: database.IsolationDedicated},
			{Type: "h200-mig-7g-141gb", DRAMode: true, Isolation: database.IsolationMIG, Profile: "7g.141gb"},
			{Type: "h200-mig-1g-18gb", DRAMode: true, Isolation: database.IsolationMIG, Profile: "1g.18gb"},
		},
	})
	defer database.SetGPUConfig(&database.GPUConfig{})

	m := laneByClassMap()
	want := map[string]string{
		"gpu.nvidia.com":          "h200-gpu",
		"mig.nvidia.com":          "h200-gpu",
		"mig.nvidia.com-7g-141gb": "h200-mig-7g-141gb",
		"mig.nvidia.com-1g-18gb":  "h200-mig-1g-18gb",
	}
	for class, lane := range want {
		if got := m[class]; got != lane {
			t.Errorf("laneByClassMap[%q] = %q, want %q", class, got, lane)
		}
	}
}

func TestAssignBookerStates(t *testing.T) {
	base := time.Date(2026, 11, 26, 0, 0, 0, 0, time.UTC)
	mk := func(name string, age time.Duration, charges map[string]int) bookerWorkload {
		return bookerWorkload{
			Name: name, Namespace: "user-user1", QueueName: "reserved",
			CreatedAt: base.Add(age), Charges: charges, Active: true,
		}
	}
	requesterOf := func(ns string) string { return "user1" }

	credits := map[string]map[string]int{
		"user1": {"h200-mig-7g-141gb": 1},
	}
	workloads := []bookerWorkload{
		mk("wl-old", 0, map[string]int{"h200-mig-7g-141gb": 1}), // covered: consumes the booking
		mk("wl-new", time.Hour, map[string]int{"h200-mig-7g-141gb": 1}), // booking exhausted
		mk("wl-unmapped", time.Hour, nil),                               // no claims: nothing to book
	}
	states := assignBookerStates(workloads, credits, bookerCapacity{}, requesterOf)

	if got := states["wl-old"].State; got != "Ready" {
		t.Errorf("wl-old state = %q, want Ready", got)
	}
	if got := states["wl-new"].State; got != "Pending" {
		t.Errorf("wl-new state = %q, want Pending", got)
	}
	msg := states["wl-new"].Message
	if !strings.Contains(msg, "h200-mig-7g-141gb") || !strings.Contains(msg, "reserved=false") {
		t.Errorf("wl-new message %q should name the lane and the reserved=false hint", msg)
	}
	// No-claim workloads in a check-wired CQ must PASS (Kueue gates them
	// waiting on the booker — leaving them stateless would hang them).
	if got := states["wl-unmapped"]; got.State != "Ready" {
		t.Errorf("workload without charges should pass the check, got %+v", got)
	}

	// FIFO: with two units booked, both fit and the older one is charged first.
	credits["user1"]["h200-mig-7g-141gb"] = 2
	states = assignBookerStates(workloads[:2], credits, bookerCapacity{}, requesterOf)
	if states["wl-old"].State != "Ready" || states["wl-new"].State != "Ready" {
		t.Errorf("FIFO: both should be Ready under a 2-unit booking, got %v / %v",
			states["wl-old"].State, states["wl-new"].State)
	}
}

func TestParseRCTCharges(t *testing.T) {
	// Fixture: the REAL ResourceClaimTemplate shape — the claim spec nests
	// under spec.spec (this regression covers the shallow-read bug that
	// silently returned empty charges and ungated every workload).
	body := []byte(`{
	  "spec": { "spec": { "devices": { "requests": [
	    { "name": "gpu", "exactly": { "deviceClassName": "mig.nvidia.com-7g-141gb", "allocationMode": "ExactCount", "count": 1 } }
	  ]}}}}
	`)
	laneByClass := map[string]string{"mig.nvidia.com-7g-141gb": "h200-mig-7g-141gb"}
	charges, err := parseRCTCharges(body, laneByClass)
	if err != nil {
		t.Fatalf("parseRCTCharges: %v", err)
	}
	if charges["h200-mig-7g-141gb"] != 1 {
		t.Fatalf("charges = %v, want h200-mig-7g-141gb: 1", charges)
	}
	// An unmapped class contributes nothing (Kueue gates those itself).
	body2 := []byte(`{"spec":{"spec":{"devices":{"requests":[
	  {"name":"gpu","exactly":{"deviceClassName":"example.com/other","count":3}}]}}}}`)
	charges2, err := parseRCTCharges(body2, laneByClass)
	if err != nil {
		t.Fatalf("parseRCTCharges unmapped: %v", err)
	}
	if len(charges2) != 0 {
		t.Fatalf("unmapped class should yield no charges, got %v", charges2)
	}
}

func TestAssignBookerStatesSystemBookingCoversNamespaceUsers(t *testing.T) {	base := time.Date(2026, 11, 26, 0, 0, 0, 0, time.UTC)
	w := bookerWorkload{
		Name: "wl", Namespace: "user-user1", QueueName: "reserved",
		CreatedAt: base, Charges: map[string]int{"h200-gpu": 4}, Active: true,
	}
	// A system booking stores user=user-user1; coverageUser trims to user1,
	// matching the namespace requester resolution.
	credits := map[string]map[string]int{
		"user1": {"h200-gpu": 4},
	}
	states := assignBookerStates([]bookerWorkload{w}, credits, bookerCapacity{}, func(ns string) string { return "user1" })
	if states["wl"].State != "Ready" {
		t.Errorf("system booking should cover the namespace's workloads, got %v (%s)",
			states["wl"].State, states["wl"].Message)
	}
}

// mkUn builds an unreserved-lane workload fixture.
func mkUn(base time.Time, name string, age time.Duration, charges map[string]int) bookerWorkload {
	return bookerWorkload{
		Name: name, Namespace: "user-user1", QueueName: "unreserved",
		CreatedAt: base.Add(age), Charges: charges, Active: true,
	}
}

func testCapacity() bookerCapacity {
	return bookerCapacity{
		Catalog: map[string]int{"h200-gpu": 16},
		NodeMax: map[string]int{"h200-gpu": 8},
		Booked:  map[string]int{},
	}
}

func TestAssignBookerStatesUnreservedBudget(t *testing.T) {
	base := time.Date(2026, 11, 26, 0, 0, 0, 0, time.UTC)
	requesterOf := func(ns string) string { return "user1" }
	capacity := testCapacity()

	// FIFO within free capacity: 6 + 8 fit (14 ≤ 16, both within the
	// per-node max of 8); the third (4) does not (free 2) and gates WITHOUT
	// consuming budget.
	workloads := []bookerWorkload{
		mkUn(base, "wl-1", 0, map[string]int{"h200-gpu": 6}),
		mkUn(base, "wl-2", time.Minute, map[string]int{"h200-gpu": 8}),
		mkUn(base, "wl-3", 2*time.Minute, map[string]int{"h200-gpu": 4}),
	}
	states := assignBookerStates(workloads, map[string]map[string]int{}, capacity, requesterOf)
	if states["wl-1"].State != "Ready" || states["wl-2"].State != "Ready" {
		t.Errorf("first two should fit free capacity, got %v / %v", states["wl-1"].State, states["wl-2"].State)
	}
	if states["wl-3"].State != "Pending" {
		t.Errorf("wl-3 exceeds remaining free capacity, got %v (%s)", states["wl-3"].State, states["wl-3"].Message)
	}
	if !strings.Contains(states["wl-3"].Message, "at capacity") {
		t.Errorf("wl-3 message should carry the capacity hint, got %q", states["wl-3"].Message)
	}
	// Backfill: a later SMALL workload fits in the units a gated big one
	// did not consume.
	big := []bookerWorkload{
		mkUn(base, "wl-big", 0, map[string]int{"h200-gpu": 15}),
		mkUn(base, "wl-small", time.Minute, map[string]int{"h200-gpu": 2}),
	}
	states = assignBookerStates(big, map[string]map[string]int{}, capacity, requesterOf)
	if states["wl-big"].State != "Pending" || states["wl-small"].State != "Ready" {
		t.Errorf("backfill: big should gate (%v), small should run (%v)", states["wl-big"].State, states["wl-small"].State)
	}
}

func TestAssignBookerStatesUnreservedShapeCap(t *testing.T) {
	base := time.Date(2026, 11, 26, 0, 0, 0, 0, time.UTC)
	requesterOf := func(ns string) string { return "user1" }

	// 15 whole boards in ONE pod: no node has 15 — DRA is node-local; the
	// charge can never schedule and must gate with the replicas hint,
	// regardless of free cluster capacity.
	w := mkUn(base, "wl-15", 0, map[string]int{"h200-gpu": 15})
	states := assignBookerStates([]bookerWorkload{w}, map[string]map[string]int{}, testCapacity(), requesterOf)
	if states["wl-15"].State != "Pending" {
		t.Fatalf("15-per-pod must gate, got %v (%s)", states["wl-15"].State, states["wl-15"].Message)
	}
	if !strings.Contains(states["wl-15"].Message, "per-node maximum of 8") || !strings.Contains(states["wl-15"].Message, "replicas") {
		t.Errorf("shape message should carry the per-node cap and replicas hint, got %q", states["wl-15"].Message)
	}

	// Make-room: a booking shrinks the free budget below what already-
	// Running unreserved workloads hold — holders that no longer fit flip
	// Pending (Kueue evicts them); FIFO keeps the OLDEST allocation when the
	// budget is contended. Booking 10 of 16 → free 6: the oldest 4-unit
	// holder keeps running, the youngest's 4 no longer fit.
	capacity := testCapacity()
	capacity.Booked["h200-gpu"] = 10
	workloads := []bookerWorkload{
		mkUn(base, "wl-old", 0, map[string]int{"h200-gpu": 4}),
		mkUn(base, "wl-young", time.Minute, map[string]int{"h200-gpu": 4}),
	}
	states = assignBookerStates(workloads, map[string]map[string]int{}, capacity, requesterOf)
	if states["wl-old"].State != "Ready" {
		t.Errorf("oldest holder should keep its allocation under FIFO, got %v (%s)", states["wl-old"].State, states["wl-old"].Message)
	}
	if states["wl-young"].State != "Pending" {
		t.Errorf("youngest holder should be deposed for the booking, got %v (%s)", states["wl-young"].State, states["wl-young"].Message)
	}

	// A booking holder deploying through the UNRESERVED lane still runs on
	// their booking — and does not consume the free unbooked budget.
	credits := map[string]map[string]int{"user1": {"h200-gpu": 2}}
	holder := mkUn(base, "wl-holder", 0, map[string]int{"h200-gpu": 2})
	unbooked := mkUn(base, "wl-unbooked", time.Minute, map[string]int{"h200-gpu": 14})
	states = assignBookerStates([]bookerWorkload{holder, unbooked}, credits, capacity, requesterOf)
	if states["wl-holder"].State != "Ready" || !strings.Contains(states["wl-holder"].Message, "covered by an active booking") {
		t.Errorf("booking holder should run via coverage on any lane, got %v (%s)", states["wl-holder"].State, states["wl-holder"].Message)
	}
	if states["wl-unbooked"].State != "Pending" {
		t.Errorf("unbooked workload must not spend the booking holder's units, got %v (%s)", states["wl-unbooked"].State, states["wl-unbooked"].Message)
	}
}

func TestIsBookerGateMessage(t *testing.T) {
	ours := []string{
		"no active booking covering h200-gpu (available 0, requested 4) — book it or deploy with reserved=false",
		"charge 15 h200-gpu per pod exceeds the per-node maximum of 8 — deploy replicas of ≤8 instead",
		"lane h200-gpu at capacity: free 2, requested 4 (capacity 16, active bookings hold 10, unreserved workloads hold 0) — book it, wait, or reduce the request",
	}
	for _, m := range ours {
		if !isBookerGateMessage(m) {
			t.Errorf("isBookerGateMessage(%q) = false, want true", m)
		}
	}
	foreign := []string{
		"",
		"The workload is deactivated",
		"Quota reserved in ClusterQueue system-user-user1",
		"charge",              // bare prefix without our em-dash signature
		"lane full, try later", // no em-dash signature
	}
	for _, m := range foreign {
		if isBookerGateMessage(m) {
			t.Errorf("isBookerGateMessage(%q) = true, want false", m)
		}
	}
}

func TestPerNodeLaneCounts(t *testing.T) {
	// Two nodes: 8 whole boards each; the flex node publishes MIG partitions
	// of the 7g.141gb profile alongside. Counter-set definers are skipped.
	body := []byte(`{"items":[
	  {"spec":{"driver":"gpu.nvidia.com","nodeName":"node-a","pool":{"name":"pool-a"},"devices":[
	    {"name":"gpu-0","attributes":{"type":{"string":"gpu"}}},
	    {"name":"gpu-1","attributes":{"type":{"string":"gpu"}}},
	    {"name":"cs-0","attributes":{},"counters":{"memory":{"value":"9856Mi"}}},
	    {"name":"gpu-7","attributes":{"type":{"string":"gpu"}}}
	  ]}},
	  {"spec":{"driver":"gpu.nvidia.com","nodeName":"node-b","pool":{"name":"pool-b"},"devices":[
	    {"name":"gpu-0","attributes":{"type":{"string":"gpu"}},"consumesCounters":[{"counterSet":"cs0"}]},
	    {"name":"gpu-0-mig-7g","attributes":{"type":{"string":"mig"},"profile":{"string":"7g.141gb"}},"consumesCounters":[{"counterSet":"cs0"}]},
	    {"name":"gpu-0-mig-7g-2","attributes":{"type":{"string":"mig"},"profile":{"string":"7g.141gb"}},"consumesCounters":[{"counterSet":"cs0"}]},
	    {"name":"gpu-0-mig-2g","attributes":{"type":{"string":"mig"},"profile":{"string":"2g.35gb"}},"consumesCounters":[{"counterSet":"cs0"}]}
	  ]}}
	]}`)
	specs := []database.GPUResourceSpec{
		{Type: "h200-gpu", DRAMode: true, Isolation: database.IsolationDedicated},
		{Type: "h200-mig-7g-141gb", DRAMode: true, Isolation: database.IsolationMIG, Profile: "7g.141gb"},
	}
	counts := perNodeLaneCounts(body, specs)
	if counts["h200-gpu"]["node-a"] != 3 {
		t.Errorf("node-a whole boards = %d, want 3 (counter-set definer skipped)", counts["h200-gpu"]["node-a"])
	}
	// node-b's whole board carries full-board ConsumesCounters — still the
	// dedicated lane (flex parent).
	if counts["h200-gpu"]["node-b"] != 1 {
		t.Errorf("node-b whole boards = %d, want 1 (flex parent)", counts["h200-gpu"]["node-b"])
	}
	if counts["h200-mig-7g-141gb"]["node-b"] != 2 {
		t.Errorf("node-b 7g partitions = %d, want 2", counts["h200-mig-7g-141gb"]["node-b"])
	}
	// NodeMax reduction: the largest single node governs the shape cap.
	capacity := buildBookerCapacity(&database.GPUConfig{Resources: specs}, nil, body)
	if capacity.NodeMax["h200-gpu"] != 3 {
		t.Errorf("NodeMax[h200-gpu] = %d, want 3", capacity.NodeMax["h200-gpu"])
	}
	if capacity.NodeMax["h200-mig-7g-141gb"] != 2 {
		t.Errorf("NodeMax[h200-mig-7g-141gb] = %d, want 2", capacity.NodeMax["h200-mig-7g-141gb"])
	}
}
