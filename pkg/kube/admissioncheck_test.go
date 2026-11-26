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
		mk("wl-unmapped", time.Hour, nil),                               // no claims: not our gate
	}
	states := assignBookerStates(workloads, credits, requesterOf)

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
	if _, gated := states["wl-unmapped"]; gated {
		t.Errorf("workload without charges must not be gated, got %v", states["wl-unmapped"])
	}

	// FIFO: with two units booked, both fit and the older one is charged first.
	credits["user1"]["h200-mig-7g-141gb"] = 2
	states = assignBookerStates(workloads[:2], credits, requesterOf)
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
	states := assignBookerStates([]bookerWorkload{w}, credits, func(ns string) string { return "user1" })
	if states["wl"].State != "Ready" {
		t.Errorf("system booking should cover the namespace's workloads, got %v (%s)",
			states["wl"].State, states["wl"].Message)
	}
}
