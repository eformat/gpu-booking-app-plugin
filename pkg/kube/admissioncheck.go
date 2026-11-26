package kube

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/eformat/gpu-booking-plugin/pkg/database"
)

// The booker enforcer: a Kueue AdmissionCheck driven by the booking app that
// makes lane capacity non-advisory on any operand (no upgrade required).
//
// Every workload queued into a booker-managed LocalQueue ("reserved" or
// "unreserved") carries the "booker" check. This controller reconciles the
// check state per workload, FIFO by creation time:
//
//   - Booking coverage (either lane): the DRA lane charges (from the claim
//     template, via the C1 class→lane convention) must fit under the user's
//     ACTIVE bookings for the current UTC window. Covered → Ready; the
//     booking units are spent. Bookings are lane-agnostic budgets — a holder
//     can deploy through the unreserved LQ and still run on their booking.
//   - Reserved-lane, uncovered → Pending with a rebooking hint.
//   - Unreserved-lane, uncovered → free-capacity policy: the charge must fit
//     catalog capacity − Σ active bookings − Σ other unbooked charges, and a
//     single pod's charge is also capped at the per-node device maximum
//     (DRA allocates a pod's devices node-locally; a larger claim can never
//     schedule). Backfill: a gated workload does not consume budget, so
//     smaller later workloads can still run.
//
// Kueue holds pending workloads gated and EVICTS admitted ones when a check
// leaves Ready — that is the expiry eviction and the make-room preemption:
// book a lane someone holds unbooked and their workload is evicted on the
// next pass; its Deployment's replacement gates instead of churning.

const (
	BookerCheckName      = "booker"
	BookerControllerName = "booker.gpu-booking-plugin"

	bookerAPIBase = "/apis/kueue.x-k8s.io/v1beta1"

	bookerReservedLQName   = "reserved"
	bookerUnreservedLQName = "unreserved"

	// bookerUnreservedCQName is the static unreserved ClusterQueue the
	// "unreserved" LocalQueues target (all-zero quotas, borrows the
	// remaining capacity from the Cohort). The enforcer wires the check
	// into it so unreserved workloads gate before admission too.
	bookerUnreservedCQName = "unreserved"
)

// BookerEnforcerEnabled gates the enforcement end to end: the AdmissionCheck
// wiring into reservation ClusterQueues and the state reconciler. One env
// flip (BOOKER_ENFORCER_ENABLED) disables it cluster-wide — the sync then
// rebuilds CQs without the check and every workload admits as before.
var BookerEnforcerEnabled = false

// BookerEnforcerInterval is the reconciler cadence in seconds.
var BookerEnforcerInterval = 30

// EnsureBookerAdmissionCheck applies the AdmissionCheck object (idempotent
// SSA). Called at startup and every reconciler pass so a deleted check
// self-heals before workloads reference a missing check and hard-gate.
func EnsureBookerAdmissionCheck() error {
	ac := map[string]any{
		"apiVersion": "kueue.x-k8s.io/v1beta1",
		"kind":       "AdmissionCheck",
		"metadata": map[string]any{
			"name": BookerCheckName,
			"labels": map[string]any{
				"rhai-tmm.dev/managed": "gpu-booking-plugin",
			},
		},
		"spec": map[string]any{
			"controllerName": BookerControllerName,
		},
	}
	return k8sApply(bookerAPIBase+"/admissionchecks/"+BookerCheckName, ac)
}

// laneByClassMap builds the deviceClass → catalog-lane mapping from the
// current GPU config: per-profile MIG classes (C1) charge their lane; the
// folded whole-device classes charge the dedicated lane.
func laneByClassMap() map[string]string {
	m := map[string]string{}
	for _, spec := range database.GetGPUConfig().Resources {
		if !spec.DRAMode {
			continue
		}
		switch spec.Isolation {
		case database.IsolationMIG:
			if spec.Profile != "" {
				m[migDeviceClassName(spec.Profile)] = spec.Type
			}
		case database.IsolationDedicated:
			m["gpu.nvidia.com"] = spec.Type
			m["mig.nvidia.com"] = spec.Type
		}
	}
	return m
}

type bookerWorkload struct {
	Name      string
	Namespace string
	QueueName string
	CreatedAt time.Time
	// Admitted: the workload currently holds a quota reservation.
	Admitted bool
	// Active: spec.active (nil means active). Deactivated workloads are
	// deposed — skipped entirely (no states, no charge accounting).
	Active bool
	// charges maps catalog lane type → device units this workload requests.
	Charges map[string]int
	// existing holds other controllers' check entries verbatim (preserved on patch).
	ExistingChecks []map[string]any
	// currentState/currentMessage are the booker entry's values, "" when unset.
	CurrentState string
	CurrentMessage string
}

type admissionCheckState struct {
	Name               string `json:"name"`
	State              string `json:"state"`
	Message            string `json:"message,omitempty"`
	LastTransitionTime string `json:"lastTransitionTime,omitempty"`
}

// listBookerWorkloads returns every workload queued into a booker-managed
// LocalQueue (reserved or unreserved), cluster-wide, with its DRA lane
// charges resolved from the claim templates.
func listBookerWorkloads(laneByClass map[string]string) ([]bookerWorkload, error) {
	body, err := K8sGet(bookerAPIBase + "/workloads")
	if err != nil {
		return nil, fmt.Errorf("listing workloads: %w", err)
	}
	var list struct {
		Items []struct {
			Metadata struct {
				Name              string `json:"name"`
				Namespace         string `json:"namespace"`
				CreationTimestamp string `json:"creationTimestamp"`
			} `json:"metadata"`
			Spec struct {
				QueueName string `json:"queueName"`
				Active    *bool  `json:"active"`
				PodSets   []struct {
					Template struct {
						Spec struct {
							ResourceClaims []struct {
								Name                      string `json:"name"`
								ResourceClaimTemplateName string `json:"resourceClaimTemplateName"`
							} `json:"resourceClaims"`
						} `json:"spec"`
					} `json:"template"`
				} `json:"podSets"`
			} `json:"spec"`
			Status struct {
				Admission *struct {
					ClusterQueue string `json:"clusterQueue"`
				} `json:"admission"`
				AdmissionChecks []struct {
					Name    string `json:"name"`
					State   string `json:"state"`
					Message string `json:"message"`
				} `json:"admissionChecks"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, fmt.Errorf("parsing workload list: %w", err)
	}

	rctCache := map[string]map[string]int{} // "ns/name" -> lane -> units
	var out []bookerWorkload
	for _, item := range list.Items {
		if item.Spec.QueueName != bookerReservedLQName && item.Spec.QueueName != bookerUnreservedLQName {
			continue
		}
		w := bookerWorkload{
			Name:      item.Metadata.Name,
			Namespace: item.Metadata.Namespace,
			QueueName: item.Spec.QueueName,
			Active:    item.Spec.Active == nil || *item.Spec.Active,
			Admitted:  item.Status.Admission != nil,
			Charges:   map[string]int{},
		}
		if t, err := time.Parse(time.RFC3339, item.Metadata.CreationTimestamp); err == nil {
			w.CreatedAt = t
		}
		skip := false
		for _, ps := range item.Spec.PodSets {
			for _, claim := range ps.Template.Spec.ResourceClaims {
				if claim.ResourceClaimTemplateName == "" {
					continue
				}
				key := item.Metadata.Namespace + "/" + claim.ResourceClaimTemplateName
				charges, ok := rctCache[key]
				if !ok {
					charges, err = rctLaneCharges(item.Metadata.Namespace, claim.ResourceClaimTemplateName, laneByClass)
					if err != nil {
						// Skip the whole WORKLOAD: a partial charge map would
						// under-count lanes and wrongly mark it covered.
						slog.Warn("booker: failed to resolve claim template charges, skipping workload",
							"workload", item.Metadata.Name, "namespace", item.Metadata.Namespace,
							"rct", claim.ResourceClaimTemplateName, "error", err)
						skip = true
						break
					}
					rctCache[key] = charges
				}
				for lane, units := range charges {
					w.Charges[lane] += units
				}
			}
			if skip {
				break
			}
		}
		if skip {
			continue
		}
		for _, c := range item.Status.AdmissionChecks {
			if c.Name == BookerCheckName {
				w.CurrentState = c.State
				w.CurrentMessage = c.Message
			}
		}
		out = append(out, w)
	}
	return out, nil
}

// rctLaneCharges resolves one ResourceClaimTemplate's device requests into
// catalog lane charges. Unmapped device classes are skipped — Kueue's own DRA
// path refuses claims naming a class that deviceClassMappings does not cover.
func rctLaneCharges(namespace, rctName string, laneByClass map[string]string) (map[string]int, error) {
	body, err := K8sGet("/apis/resource.k8s.io/v1/namespaces/" + namespace + "/resourceclaimtemplates/" + rctName)
	if err != nil {
		return nil, err
	}
	return parseRCTCharges(body, laneByClass)
}

// parseRCTCharges extracts lane charges from a ResourceClaimTemplate payload.
// The template nests the claim spec one level deeper than a ResourceClaim
// (spec.spec.devices — the exact bug this separation guards against: reading
// spec.devices yields empty charges with no error and silently ungates
// every workload).
func parseRCTCharges(body []byte, laneByClass map[string]string) (map[string]int, error) {
	var rct struct {
		Spec struct {
			Spec struct {
				Devices struct {
					Requests []struct {
						Exactly struct {
							DeviceClassName string `json:"deviceClassName"`
							Count           int64  `json:"count"`
						} `json:"exactly"`
					} `json:"requests"`
				} `json:"devices"`
			} `json:"spec"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(body, &rct); err != nil {
		return nil, fmt.Errorf("parsing resourceclaimtemplate: %w", err)
	}
	charges := map[string]int{}
	for _, r := range rct.Spec.Spec.Devices.Requests {
		lane, ok := laneByClass[r.Exactly.DeviceClassName]
		if !ok {
			continue
		}
		count := r.Exactly.Count
		if count <= 0 {
			count = 1
		}
		charges[lane] += int(count)
	}
	return charges, nil
}

// coverageCredits aggregates the ACTIVE reservations into per-user per-lane
// units, keyed on the same coverage identity the booking sync uses
// (coverageUser: system bookings store the target namespace, usage is
// attributed to the namespace's requester — the user- prefix trim unifies).
func coverageCredits(now time.Time) (map[string]map[string]int, error) {
	reservations, err := getActiveReservationsAt(now)
	if err != nil {
		return nil, err
	}
	return creditsFromReservations(reservations), nil
}

// creditsFromReservations folds reservations into per-user per-lane credit
// maps (the coverage currency the state assignment spends, FIFO).
func creditsFromReservations(reservations []userReservation) map[string]map[string]int {
	credits := map[string]map[string]int{}
	for _, r := range reservations {
		user := coverageUser(r.User)
		if credits[user] == nil {
			credits[user] = map[string]int{}
		}
		for lane, units := range r.Resources {
			credits[user][lane] += units
		}
	}
	return credits
}

// bookerCapacity carries the lane-level facts the unreserved free-capacity
// policy needs. All maps are keyed by catalog lane type (e.g. "h200-gpu").
type bookerCapacity struct {
	// Catalog: lane → total units across the cluster (from the GPU config).
	Catalog map[string]int
	// NodeMax: lane → largest single-node device count. DRA allocates a
	// pod's devices node-locally, so a charge above this can never schedule.
	// 0 = unknown (lane absent from the published slices) — not enforced.
	NodeMax map[string]int
	// Booked: lane → units held by ALL active bookings (they are off-limits
	// to unbooked workloads whether or not the holder is running).
	Booked map[string]int
}

// buildBookerCapacity assembles the capacity view from the GPU config, the
// active reservations, and the published ResourceSlices (per-node counts).
func buildBookerCapacity(cfg *database.GPUConfig, reservations []userReservation, sliceBody []byte) bookerCapacity {
	capacity := bookerCapacity{
		Catalog: map[string]int{},
		NodeMax: map[string]int{},
		Booked:  map[string]int{},
	}
	for _, spec := range cfg.Resources {
		if spec.DRAMode {
			capacity.Catalog[spec.Type] = spec.Count
		}
	}
	for _, r := range reservations {
		for lane, units := range r.Resources {
			capacity.Booked[lane] += units
		}
	}
	for lane, perNode := range perNodeLaneCounts(sliceBody, cfg.Resources) {
		nodeMax := 0
		for _, count := range perNode {
			if count > nodeMax {
				nodeMax = count
			}
		}
		capacity.NodeMax[lane] = nodeMax
	}
	return capacity
}

// perNodeLaneCounts parses a ResourceSlice list into per-node device counts
// per catalog lane, mirroring the discovery classification: whole boards
// (type=gpu, including flex parents carrying full-board ConsumesCounters)
// count toward the dedicated lane; MIG partitions count toward their
// profile's lane. Counter-set definers are not claimable units — skipped.
func perNodeLaneCounts(body []byte, specs []database.GPUResourceSpec) map[string]map[string]int {
	out := map[string]map[string]int{}
	if len(body) == 0 {
		return out
	}
	dedicatedLane := ""
	profileLane := map[string]string{}
	for _, spec := range specs {
		if !spec.DRAMode {
			continue
		}
		switch spec.Isolation {
		case database.IsolationDedicated:
			dedicatedLane = spec.Type
		case database.IsolationMIG:
			if spec.Profile != "" {
				profileLane[spec.Profile] = spec.Type
			}
		}
	}
	if dedicatedLane == "" && len(profileLane) == 0 {
		return out
	}
	var list draSliceList
	if err := json.Unmarshal(body, &list); err != nil {
		return out
	}
	for _, slice := range list.Items {
		if slice.Spec.Driver != database.DRADeviceClass {
			continue
		}
		node := slice.Spec.NodeName
		if node == "" {
			node = slice.Spec.Pool.Name
		}
		if node == "" {
			continue
		}
		for _, dev := range slice.Spec.Devices {
			// Counter-set definers (the full physical GPU under dynamic MIG)
			// are not themselves claimable catalog units.
			if len(dev.Counters) > 0 {
				continue
			}
			devType := attrStringAny(dev, draTypeAttrKeys)
			profile := attrStringAny(dev, draProfileAttrKeys)
			lane := ""
			if (devType == "mig" || devType == "") && len(dev.ConsumesCounters) > 0 {
				lane = profileLane[profile]
			} else if devType == "gpu" || (devType == "" && profile == "" && len(dev.ConsumesCounters) == 0) {
				lane = dedicatedLane
			}
			if lane == "" {
				continue
			}
			if out[lane] == nil {
				out[lane] = map[string]int{}
			}
			out[lane][node]++
		}
	}
	return out
}

// assignBookerStates orders workloads FIFO (oldest first, across both
// lanes) and assigns the booker check state:
//
//  1. Booking coverage (either lane): charges that fit under the user's
//     remaining booking credits are Ready and SPEND those credits — a
//     booking is a lane-agnostic budget spent FIFO.
//  2. Reserved-lane, uncovered → Pending with a rebooking hint.
//  3. Unreserved-lane, uncovered → free-capacity policy: charge ≤ catalog
//     capacity − Σ active bookings − Σ other unbooked charges granted this
//     pass, and charge ≤ per-node device maximum (a bigger single-pod claim
//     can never schedule — DRA is node-local). Gated workloads consume no
//     budget, so smaller later workloads backfill past them.
//
// Returns the desired state per workload (only workloads whose state
// CHANGES are patched by the caller).
func assignBookerStates(workloads []bookerWorkload, credits map[string]map[string]int, capacity bookerCapacity, requesterOf func(ns string) string) map[string]admissionCheckState {
	ordered := make([]bookerWorkload, len(workloads))
	copy(ordered, workloads)
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].CreatedAt.Equal(ordered[j].CreatedAt) {
			return ordered[i].Name < ordered[j].Name
		}
		return ordered[i].CreatedAt.Before(ordered[j].CreatedAt)
	})

	remaining := map[string]map[string]int{}
	for u, lanes := range credits {
		remaining[u] = map[string]int{}
		for lane, units := range lanes {
			remaining[u][lane] = units
		}
	}
	// used tracks the unreserved budget granted this pass (lane → units).
	used := map[string]int{}

	states := map[string]admissionCheckState{}
	now := time.Now().UTC().Format(time.RFC3339)
	for _, w := range ordered {
		// Deactivated (deposed) workloads hold nothing and get nothing:
		// they are skipped so they neither consume booking credits nor
		// receive states (they cannot re-admit while inactive).
		if !w.Active {
			continue
		}
		// Workloads without DRA lane charges (no claims, or a class the
		// mapping does not cover) pass immediately — Kueue gates every
		// workload in a check-wired CQ, and a CPU-only workload has nothing
		// to book. Unmapped claim classes are Kueue's own DRA path's business.
		if len(w.Charges) == 0 {
			states[w.Name] = admissionCheckState{
				Name:               BookerCheckName,
				State:              "Ready",
				Message:            "no DRA lane charges — nothing to book",
				LastTransitionTime: now,
			}
			continue
		}
		user := coverageUser(requesterOf(w.Namespace))
		// 1) Booking coverage — lane-agnostic, FIFO (oldest workloads spend
		// the user's credits first).
		covered := true
		var shortLane string
		var booked, needed int
		for lane, charge := range w.Charges {
			avail := remaining[user][lane]
			if avail < charge {
				covered = false
				shortLane, booked, needed = lane, avail, charge
				break
			}
		}
		if covered {
			for lane, charge := range w.Charges {
				remaining[user][lane] -= charge
			}
			states[w.Name] = admissionCheckState{
				Name:               BookerCheckName,
				State:              "Ready",
				Message:            "covered by an active booking",
				LastTransitionTime: now,
			}
			continue
		}
		// 2) Uncovered: the reserved lane requires a booking, period.
		if w.QueueName == bookerReservedLQName {
			states[w.Name] = admissionCheckState{
				Name:               BookerCheckName,
				State:              "Pending",
				Message:            fmt.Sprintf("no active booking covering %s (available %d, requested %d) — book it or deploy with reserved=false", shortLane, booked, needed),
				LastTransitionTime: now,
			}
			continue
		}
		// 3) Unreserved lane: free capacity after bookings, FIFO with backfill.
		if gate := unreservedGate(w.Charges, capacity, used); gate != "" {
			states[w.Name] = admissionCheckState{
				Name:               BookerCheckName,
				State:              "Pending",
				Message:            gate,
				LastTransitionTime: now,
			}
			continue
		}
		for lane, charge := range w.Charges {
			used[lane] += charge
		}
		states[w.Name] = admissionCheckState{
			Name:               BookerCheckName,
			State:              "Ready",
			Message:            "within free lane capacity",
			LastTransitionTime: now,
		}
	}
	return states
}

// unreservedGate returns the gating message for an unreserved workload's
// charges, or "" when the workload fits. Checks, in order:
//
//  1. per-node device maximum (DRA is node-local — a bigger single-pod
//     claim can never schedule),
//  2. the physical board model (whole boards and MIG slices share the
//     dedicated + flex pools — 16 whole + one 7g carve needs a 17th board),
//  3. the flat lane budget (catalog − bookings − other unreserved charges).
//
// Lanes absent from the capacity view fail open (unknown → not enforced).
func unreservedGate(charges map[string]int, capacity bookerCapacity, used map[string]int) string {
	lanes := make([]string, 0, len(charges))
	for lane := range charges {
		lanes = append(lanes, lane)
	}
	sort.Strings(lanes)
	for _, lane := range lanes {
		charge := charges[lane]
		if nodeMax := capacity.NodeMax[lane]; nodeMax > 0 && charge > nodeMax {
			return fmt.Sprintf("charge %d %s per pod exceeds the per-node maximum of %d — deploy replicas of ≤%d instead", charge, lane, nodeMax, nodeMax)
		}
	}
	// Board model: whole boards and MIG slices draw on the same dedicated +
	// flex pools. A carve in a MIG lane consumes flex boards the whole lane
	// still counts — the flat budget alone admits whole workloads that can
	// never allocate (16 whole + a 7g carve needs 17 physical boards).
	totals := map[string]int{}
	for lane, units := range capacity.Booked {
		totals[lane] += units
	}
	for lane, units := range used {
		totals[lane] += units
	}
	for _, lane := range lanes {
		totals[lane] += charges[lane]
	}
	if reason := database.BoardCapacityCheck(totals); reason != "" {
		return fmt.Sprintf("board pool exhausted: %s — book it, wait, or reduce the request", reason)
	}
	for _, lane := range lanes {
		charge := charges[lane]
		catalog, known := capacity.Catalog[lane]
		if !known {
			continue
		}
		free := catalog - capacity.Booked[lane] - used[lane]
		if charge > free {
			return fmt.Sprintf("lane %s at capacity: free %d, requested %d (capacity %d, active bookings hold %d, unreserved workloads hold %d) — book it, wait, or reduce the request", lane, free, charge, catalog, capacity.Booked[lane], used[lane])
		}
	}
	return ""
}

// ensureBookerActive marks the AdmissionCheck active. Kueue refuses to use
// an inactive check for admission ("references inactive AdmissionCheck(s)"),
// and for custom checks the Active condition is the controller's
// responsibility — Kueue does not set it.
func ensureBookerActive() error {
	body, err := K8sGet(bookerAPIBase + "/admissionchecks/" + BookerCheckName)
	if err != nil {
		return err
	}
	var ac struct {
		Status struct {
			Conditions []map[string]any `json:"conditions"`
		} `json:"status"`
	}
	if err := json.Unmarshal(body, &ac); err != nil {
		return fmt.Errorf("parsing admissioncheck: %w", err)
	}
	for _, c := range ac.Status.Conditions {
		if c["type"] == "Active" && c["status"] == "True" {
			return nil // already active
		}
	}
	cond := map[string]any{
		"type":               "Active",
		"status":             "True",
		"reason":             "BookerControllerRunning",
		"message":            "the booking plugin's enforcer is reconciling this check",
		"lastTransitionTime": time.Now().UTC().Format(time.RFC3339),
	}
	payload, err := json.Marshal(map[string]any{
		"status": map[string]any{"conditions": []map[string]any{cond}},
	})
	if err != nil {
		return err
	}
	return k8sMergePatch(bookerAPIBase+"/admissionchecks/"+BookerCheckName+"/status", payload)
}

// isBookerGateMessage reports whether an admission check message was written
// by this controller's gating paths (the discriminator for deactivating
// stale admitted workloads — Kueue's own init messages never match).
func isBookerGateMessage(msg string) bool {
	if msg == "" {
		return false
	}
	if strings.HasPrefix(msg, "no active booking covering") {
		return true
	}
	// Our other gate messages end with an actionable hint after an em-dash;
	// require that signature so foreign messages never trigger deactivation.
	if !strings.Contains(msg, " — ") {
		return false
	}
	return strings.HasPrefix(msg, "charge ") ||
		strings.HasPrefix(msg, "lane ") ||
		strings.HasPrefix(msg, "board pool exhausted")
}

// ensureUnreservedCQCheck wires the booker check into the static unreserved
// ClusterQueue (the "unreserved" LocalQueues target). Kueue only gates and
// evicts on checks a CQ references — without this, unreserved workloads
// admit on cohort borrowing before the enforcer evaluates them. Idempotent;
// the check is added when the enforcer runs and never removed here (flip
// BOOKER_ENFORCER_ENABLED off and rebuild the CQ to unwire).
func ensureUnreservedCQCheck() error {
	body, err := K8sGet(bookerAPIBase + "/clusterqueues/" + bookerUnreservedCQName)
	if err != nil {
		// The static CQ may not exist on clusters without the unreserved
		// queue layout — warn and continue the pass.
		slog.Warn("booker: unreserved ClusterQueue not readable, skipping check wiring", "error", err)
		return nil
	}
	var cq struct {
		Spec struct {
			Strategy struct {
				Checks []struct {
					Name string `json:"name"`
				} `json:"admissionChecks"`
			} `json:"admissionChecksStrategy"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(body, &cq); err != nil {
		return fmt.Errorf("parsing clusterqueue: %w", err)
	}
	for _, c := range cq.Spec.Strategy.Checks {
		if c.Name == BookerCheckName {
			return nil // already wired
		}
	}
	// Preserve any other controllers' checks — append, never clobber.
	checks := make([]map[string]any, 0, len(cq.Spec.Strategy.Checks)+1)
	for _, c := range cq.Spec.Strategy.Checks {
		checks = append(checks, map[string]any{"name": c.Name})
	}
	checks = append(checks, map[string]any{"name": BookerCheckName})
	payload, err := json.Marshal(map[string]any{
		"spec": map[string]any{
			"admissionChecksStrategy": map[string]any{
				"admissionChecks": checks,
			},
		},
	})
	if err != nil {
		return err
	}
	if err := k8sMergePatch(bookerAPIBase+"/clusterqueues/"+bookerUnreservedCQName, payload); err != nil {
		return err
	}
	slog.Info("booker: wired check into unreserved ClusterQueue", "clusterqueue", bookerUnreservedCQName)
	return nil
}

// reconcileBookerStates is one pass: ensure the check exists and is active,
// ensure it is wired into the unreserved CQ, list booker-managed workloads,
// evaluate coverage + free-capacity, and patch changed states.
func reconcileBookerStates(requesterOf func(ns string) string) error {
	if err := EnsureBookerAdmissionCheck(); err != nil {
		return fmt.Errorf("ensuring AdmissionCheck: %w", err)
	}
	if err := ensureBookerActive(); err != nil {
		return fmt.Errorf("ensuring AdmissionCheck active: %w", err)
	}
	if err := ensureUnreservedCQCheck(); err != nil {
		return fmt.Errorf("ensuring unreserved CQ check wiring: %w", err)
	}
	laneByClass := laneByClassMap()
	workloads, err := listBookerWorkloads(laneByClass)
	if err != nil {
		return err
	}
	reservations, err := getActiveReservationsAt(time.Now().UTC())
	if err != nil {
		return fmt.Errorf("loading active reservations: %w", err)
	}
	credits := creditsFromReservations(reservations)
	// The capacity view needs the published slices; a DRA-absent cluster
	// (nil body, no error) yields an empty view — the unreserved budget then
	// fails open and only booking coverage is enforced.
	sliceBody, err := fetchDRASliceBody()
	if err != nil {
		slog.Warn("booker: resource slice list unavailable, per-node and budget caps degrade to coverage-only", "error", err)
		sliceBody = nil
	}
	capacity := buildBookerCapacity(database.GetGPUConfig(), reservations, sliceBody)

	states := assignBookerStates(workloads, credits, capacity, requesterOf)
	byName := map[string]bookerWorkload{}
	for _, w := range workloads {
		byName[w.Name] = w
	}
	patched := 0
	for name, desired := range states {
		w := byName[name]
		// STALE GENERATION / ADMISSION RACE: an admitted workload whose
		// charges are no longer grantable must not hold quota. If the check
		// has already been patched with a gating message and the workload is
		// STILL admitted (Kueue had no transition to evict on — it was
		// admitted before the check existed or its CQ carried no checks),
		// depose it: deactivate once, quota and claim release, the
		// Deployment's replacement hits the check like any fresh workload.
		if desired.State == "Pending" && w.Admitted && w.Active && isBookerGateMessage(w.CurrentMessage) {
			if err := deactivateWorkload(w.Namespace, w.Name); err != nil {
				slog.Error("booker: failed to deactivate stale admitted workload",
					"workload", name, "namespace", w.Namespace, "error", err)
				continue
			}
			slog.Info("booker: deactivated stale admitted workload (gated, pre-enforcement admission)",
				"workload", name, "namespace", w.Namespace)
			continue
		}
		// Steady state: skip only when BOTH state and message match. The
		// message matters — patching a not-Ready check that Kueue
		// initialized itself (e.g. on a workload admitted before the check
		// existed) is what triggers Kueue's reconcile and the eviction.
		if w.CurrentState == desired.State && w.CurrentMessage == desired.Message {
			continue
		}
		if err := patchWorkloadCheckState(w.Namespace, w.Name, desired); err != nil {
			slog.Error("booker: failed to patch check state",
				"workload", name, "namespace", w.Namespace, "state", desired.State, "error", err)
			continue
		}
		patched++
		if desired.State != "Ready" {
			slog.Info("booker: workload gated (uncovered or over free lane capacity)",
				"workload", name, "namespace", w.Namespace, "message", desired.Message)
		}
	}
	if patched > 0 {
		slog.Info("booker: reconciled check states", "patched", patched, "evaluated", len(states))
	}
	return nil
}

// deactivateWorkload sets spec.active=false on a workload: Kueue evicts it
// (pods deleted, quota and DRA claim released) and it cannot re-admit while
// inactive — the user redeploys (or rebooks and redeploys) to come back.
func deactivateWorkload(namespace, name string) error {
	payload, err := json.Marshal(map[string]any{
		"spec": map[string]any{"active": false},
	})
	if err != nil {
		return err
	}
	return k8sMergePatch(bookerAPIBase+"/namespaces/"+namespace+"/workloads/"+name, payload)
}

// patchWorkloadCheckState upserts the booker entry in the workload's
// status.admissionChecks, preserving other controllers' entries. Kueue gates
// pending workloads until every check is Ready and evicts admitted workloads
// when a check leaves Ready.
func patchWorkloadCheckState(namespace, name string, desired admissionCheckState) error {
	body, err := K8sGet(bookerAPIBase + "/namespaces/" + namespace + "/workloads/" + name)
	if err != nil {
		return fmt.Errorf("reading workload: %w", err)
	}
	var wl struct {
		Status struct {
			AdmissionChecks []map[string]any `json:"admissionChecks"`
		} `json:"status"`
	}
	if err := json.Unmarshal(body, &wl); err != nil {
		return fmt.Errorf("parsing workload: %w", err)
	}

	desiredMap := map[string]any{
		"name":                desired.Name,
		"state":               desired.State,
		"lastTransitionTime":  desired.LastTransitionTime,
	}
	if desired.Message != "" {
		desiredMap["message"] = desired.Message
	}

	checks := []map[string]any{}
	found := false
	for _, c := range wl.Status.AdmissionChecks {
		cn, _ := c["name"].(string)
		if cn == BookerCheckName {
			checks = append(checks, desiredMap)
			found = true
			continue
		}
		checks = append(checks, c)
	}
	if !found {
		checks = append(checks, desiredMap)
	}

	payload, err := json.Marshal(map[string]any{
		"status": map[string]any{"admissionChecks": checks},
	})
	if err != nil {
		return err
	}
	return k8sMergePatch(bookerAPIBase+"/namespaces/"+namespace+"/workloads/"+name+"/status", payload)
}

// k8sMergePatch issues a merge patch against a resource (main or /status
// subresource — the path decides). Merge-patch (not SSA) so we only touch
// the fields we own and never fight Kueue's own status manager.
func k8sMergePatch(path string, payload []byte) error {
	req, err := http.NewRequest("PATCH", k8sHost+path, strings.NewReader(string(payload)))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+k8sToken)
	req.Header.Set("Content-Type", "application/merge-patch+json")
	req.Header.Set("Accept", "application/json")

	resp, err := k8sHTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("k8s patch %s: %w", path, err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return fmt.Errorf("k8s patch %s returned %d: %s", path, resp.StatusCode, truncateBody(respBody))
	}
	return nil
}

// RunBookerEnforcer runs the reconciliation loop until the process exits.
func RunBookerEnforcer(interval time.Duration) {
	requesterCache := map[string]string{}
	var mu sync.Mutex
	requesterOf := func(ns string) string {
		mu.Lock()
		defer mu.Unlock()
		if u, ok := requesterCache[ns]; ok {
			return u
		}
		u, err := getNamespaceRequester(ns)
		if err != nil || u == "" {
			u = ns // fall through to the namespace name itself
		}
		requesterCache[ns] = u
		return u
	}
	for {
		if err := reconcileBookerStates(requesterOf); err != nil {
			slog.Error("booker: reconciliation pass failed", "error", err)
		}
		time.Sleep(interval)
	}
}
