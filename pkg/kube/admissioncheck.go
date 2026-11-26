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
// makes reserved lanes non-advisory on any operand (no upgrade required).
//
// Every workload queued into a reservation ClusterQueue carries the "booker"
// check. This controller reconciles the check state per workload: the DRA
// lane charges (from the claim template, via the C1 class→lane convention)
// must be covered by the user's ACTIVE bookings for the current UTC window.
// Covered → Ready (Kueue admits); uncovered → Pending with a message (Kueue
// holds pending workloads gated and EVICTS admitted ones when a check leaves
// Ready — that is the expiry eviction and the make-room preemption: book a
// lane someone holds unbooked and their workload is evicted on the next
// pass; its Deployment's replacement gates instead of churning).
//
// Unreserved-lane workloads (queueName != reserved) are not touched: the DRA
// scheduler's physical exclusivity remains the floor there.

const (
	BookerCheckName      = "booker"
	BookerControllerName = "booker.gpu-booking-plugin"

	bookerAPIBase        = "/apis/kueue.x-k8s.io/v1beta1"
	bookerReservedLQName = "reserved"
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

// listBookerWorkloads returns every workload queued into the reserved LQ,
// cluster-wide, with its DRA lane charges resolved from the claim templates.
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
		if item.Spec.QueueName != bookerReservedLQName {
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
	return credits, nil
}

// assignBookerStates orders each user's reserved-lane workloads FIFO (oldest
// first) and marks them Ready while their active booking units cover the
// cumulative charge; the excess gates with a rebooking hint. Returns the
// desired state per workload (only workloads whose state CHANGES are patched
// by the caller).
func assignBookerStates(workloads []bookerWorkload, credits map[string]map[string]int, requesterOf func(ns string) string) map[string]admissionCheckState {
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
		// mapping does not cover) are not the booker's business — Kueue's
		// own DRA path gates unmapped classes, and a no-claim workload has
		// nothing to book.
		if len(w.Charges) == 0 {
			continue
		}
		user := coverageUser(requesterOf(w.Namespace))
		covered := len(w.Charges) > 0
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
		if !covered {
			states[w.Name] = admissionCheckState{
				Name:               BookerCheckName,
				State:              "Pending",
				Message:            fmt.Sprintf("no active booking covering %s (available %d, requested %d) — book it or deploy with reserved=false", shortLane, booked, needed),
				LastTransitionTime: now,
			}
			continue
		}
		for lane, charge := range w.Charges {
			remaining[user][lane] -= charge
		}
		states[w.Name] = admissionCheckState{
			Name:               BookerCheckName,
			State:              "Ready",
			Message:            "covered by an active booking",
			LastTransitionTime: now,
		}
	}
	return states
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

// reconcileBookerStates is one pass: ensure the check exists, ensure it is
// active, list reserved-lane workloads, evaluate coverage, and patch changed
// states.
func reconcileBookerStates(requesterOf func(ns string) string) error {
	if err := EnsureBookerAdmissionCheck(); err != nil {
		return fmt.Errorf("ensuring AdmissionCheck: %w", err)
	}
	if err := ensureBookerActive(); err != nil {
		return fmt.Errorf("ensuring AdmissionCheck active: %w", err)
	}
	laneByClass := laneByClassMap()
	workloads, err := listBookerWorkloads(laneByClass)
	if err != nil {
		return err
	}
	credits, err := coverageCredits(time.Now().UTC())
	if err != nil {
		return fmt.Errorf("loading active reservations: %w", err)
	}

	states := assignBookerStates(workloads, credits, requesterOf)
	byName := map[string]bookerWorkload{}
	for _, w := range workloads {
		byName[w.Name] = w
	}
	patched := 0
	for name, desired := range states {
		w := byName[name]
		// STALE GENERATION / ADMISSION RACE: an admitted workload whose
		// charges are uncovered must not hold quota. If the check has
		// already been patched with the gating message and the workload is
		// STILL admitted (Kueue had no transition to evict on — it was
		// admitted before the check existed), depose it: deactivate once,
		// quota and claim release, the Deployment's replacement hits the
		// check like any fresh workload.
		if desired.State == "Pending" && w.Admitted && w.Active && strings.HasPrefix(w.CurrentMessage, "no active booking covering") {
			if err := deactivateWorkload(w.Namespace, w.Name); err != nil {
				slog.Error("booker: failed to deactivate stale admitted workload",
					"workload", name, "namespace", w.Namespace, "error", err)
				continue
			}
			slog.Info("booker: deactivated stale admitted workload (uncovered, pre-enforcement admission)",
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
			slog.Info("booker: workload gated (lane not covered by an active booking)",
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
