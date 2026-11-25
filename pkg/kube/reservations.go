package kube

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/eformat/gpu-booking-plugin/pkg/database"
)

const (
	reservationCleanInterval = 2 * time.Minute
	drainTimeoutSeconds      = 30 * 60 // 30 minutes: force-delete CQ if drain exceeds this
)

// sanitizeK8sName converts a username (e.g. "cluster-admin@redhat.com") into a
// valid Kubernetes resource name by stripping the @domain suffix and replacing
// any invalid characters. This matches the namespace convention (e.g. "user-mhepburn").
func sanitizeK8sName(name string) string {
	name = strings.ToLower(name)
	// Strip @domain - namespaces use short usernames (e.g. "user-mhepburn")
	if idx := strings.Index(name, "@"); idx >= 0 {
		name = name[:idx]
	}
	// Replace any remaining invalid characters with dashes
	var b strings.Builder
	for _, c := range name {
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' || c == '.' {
			b.WriteRune(c)
		} else {
			b.WriteRune('-')
		}
	}
	result := b.String()
	result = strings.Trim(result, "-.")
	if len(result) > 253 {
		result = result[:253]
	}
	return result
}

var ReservationSyncEnabled = true

// syncState tracks coalesced sync requests:
//
//	0 = idle, 1 = running, 2 = running + re-run requested
var syncState int32

// TriggerSyncReservations requests a reservation sync without blocking the caller.
// Multiple rapid calls are coalesced — at most one goroutine runs at a time, but
// if a new trigger arrives while running, a follow-up sync is guaranteed.
func TriggerSyncReservations() {
	if !ReservationSyncEnabled || k8sHost == "" || k8sToken == "" {
		return
	}
	for {
		state := atomic.LoadInt32(&syncState)
		switch state {
		case 0: // idle → start running
			if atomic.CompareAndSwapInt32(&syncState, 0, 1) {
				go func() {
					for {
						SyncReservations()
						// Try to go idle; if re-run was requested (state==2), loop again
						if atomic.CompareAndSwapInt32(&syncState, 1, 0) {
							return
						}
						// state was 2 (re-run requested), reset to 1 and loop
						atomic.StoreInt32(&syncState, 1)
					}
				}()
				return
			}
		case 1: // running → request re-run
			if atomic.CompareAndSwapInt32(&syncState, 1, 2) {
				return
			}
		case 2: // re-run already requested, nothing to do
			return
		}
	}
}

type userReservation struct {
	User      string
	Resources map[string]int
	CPU       int
	Memory    int
	Until     int64
	// IsSystem marks admin-created system bookings for an existing namespace
	// (e.g. shared model-serving capacity). User booking: namespace "user-<name>",
	// ClusterQueue named after the namespace. System booking: namespace is the
	// target namespace itself, ClusterQueue named "system-<namespace>".
	IsSystem bool
}

// cqName returns the ClusterQueue name for a reservation.
func (r userReservation) cqName() string {
	if r.IsSystem {
		return "system-" + r.User
	}
	return "user-" + sanitizeK8sName(r.User)
}

// namespace returns the Kubernetes namespace the reservation applies to.
func (r userReservation) namespace() string {
	if r.IsSystem {
		return r.User
	}
	return "user-" + sanitizeK8sName(r.User)
}

func InitReservationSync() {
	if k8sHost == "" || k8sToken == "" {
		slog.Info("reservation sync: not running in-cluster, disabling")
		return
	}
	slog.Info("reservation sync: enabled", "cleaner_interval", "2m")
	go reservationCleanerLoop()
}

func reservationCleanerLoop() {
	time.Sleep(10 * time.Second)
	for {
		if ReservationSyncEnabled {
			SyncReservations()
			if err := cleanExpiredReservations(); err != nil {
				slog.Error("reservation cleaner error", "error", err)
			}
		}
		time.Sleep(reservationCleanInterval)
	}
}

func SyncReservations() {
	if k8sHost == "" || k8sToken == "" || !ReservationSyncEnabled {
		return
	}

	reservations, err := getActiveReservations()
	if err != nil {
		slog.Error("reservation sync failed", "error", err)
		return
	}

	for _, res := range reservations {
		if err := applyUserReservation(res); err != nil {
			slog.Error("reservation sync: failed to apply for user", "user", res.User, "error", err)
		}
	}

	activeUsers := map[string]bool{}
	for _, res := range reservations {
		activeUsers[res.cqName()] = true
	}
	if err := removeStaleReservations(activeUsers); err != nil {
		slog.Error("reservation sync: failed to remove stale", "error", err)
	}

	if err := applyCohortRemaining(reservations); err != nil {
		slog.Error("reservation sync: failed to update cohort", "error", err)
	}

	if len(reservations) > 0 {
		slog.Info("reservation sync: applied user reservations", "count", len(reservations))
	}
}

func getActiveReservations() ([]userReservation, error) {
	return getActiveReservationsAt(time.Now().UTC())
}

func getActiveReservationsAt(now time.Time) ([]userReservation, error) {
	db := database.DB()
	yesterday := now.AddDate(0, 0, -1).Format("2006-01-02")
	tomorrow := now.AddDate(0, 0, 1).Format("2006-01-02")

	rows, err := db.Query(
		`SELECT user, resource, slot_index, date, start_hour, end_hour, utc_offset, booking_type
		 FROM bookings WHERE date BETWEEN ? AND ? AND source = ?`,
		yesterday, tomorrow, database.SourceReserved,
	)
	if err != nil {
		return nil, fmt.Errorf("querying reservations: %w", err)
	}
	defer rows.Close()

	type resKey struct {
		user     string
		isSystem bool
	}
	userMap := map[resKey]map[string]map[int]bool{}
	userMaxUtcEnd := map[resKey]time.Time{}
	for rows.Next() {
		var user, resource, date string
		var slotIndex, startHour, endHour int
		var utcOffset float64
		var bookingType string
		if err := rows.Scan(&user, &resource, &slotIndex, &date, &startHour, &endHour, &utcOffset, &bookingType); err != nil {
			continue
		}
		if !database.IsGPUResource(resource) {
			continue
		}

		base, err := time.Parse("2006-01-02", date)
		if err != nil {
			continue
		}
		utcStart := base.Add(time.Duration((float64(startHour) - utcOffset) * float64(time.Hour)))
		utcEnd := base.Add(time.Duration((float64(endHour) - utcOffset) * float64(time.Hour)))

		if now.Before(utcStart) || !now.Before(utcEnd) {
			continue
		}

		key := resKey{user: user, isSystem: bookingType == database.BookingTypeSystem}
		if _, ok := userMap[key]; !ok {
			userMap[key] = map[string]map[int]bool{}
		}
		if _, ok := userMap[key][resource]; !ok {
			userMap[key][resource] = map[int]bool{}
		}
		userMap[key][resource][slotIndex] = true

		if utcEnd.After(userMaxUtcEnd[key]) {
			userMaxUtcEnd[key] = utcEnd
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating reservation rows: %w", err)
	}

	var reservations []userReservation
	for key, resourceSlots := range userMap {
		resources := map[string]int{}
		cfg := database.GetGPUConfig()
		for _, spec := range cfg.Resources {
			resources[spec.Type] = 0
		}
		for resource, slots := range resourceSlots {
			resources[resource] = len(slots)
		}

		until := userMaxUtcEnd[key]

		res := userReservation{
			User:      key.user,
			Resources: resources,
			Until:     until.Unix(),
			IsSystem:  key.isSystem,
		}
		for gpuRes, count := range resources {
			spec, ok := database.GPUSpecByType(gpuRes)
			if !ok {
				continue
			}
			cfg := database.GetGPUConfig()
			share := float64(count) * spec.Share
			cpu := int(math.Ceil(share * float64(cfg.TotalCPU)))
			if cpu < 2 {
				cpu = 2
			}
			mem := int(math.Ceil(share * float64(cfg.TotalMemory)))
			if mem < 8 {
				mem = 8
			}
			res.CPU += cpu
			res.Memory += mem
		}
		reservations = append(reservations, res)
	}

	return reservations, nil
}

func normalizedResourceID(resource string) string {
	parts := strings.Split(resource, "/")
	short := parts[len(parts)-1]

	// For MIG types like "mig-3g.71gb", simplify to "mig-71gb"
	if strings.HasPrefix(short, "mig-") {
		if dotIdx := strings.LastIndex(short, "."); dotIdx >= 0 {
			short = "mig-" + short[dotIdx+1:]
		}
	}

	return short
}

func applyUserReservation(res userReservation) error {
	ns := res.namespace()
	cqName := res.cqName()
	untilStr := strconv.FormatInt(res.Until, 10)

	coveredResources := []string{"cpu", "memory"}
	quotaResources := []map[string]any{
		{"name": "cpu", "nominalQuota": strconv.Itoa(res.CPU)},
		{"name": "memory", "nominalQuota": fmt.Sprintf("%dGi", res.Memory)},
	}
	cfg := database.GetGPUConfig()
	for _, spec := range cfg.Resources {
		coveredResources = append(coveredResources, spec.Type)
		count := res.Resources[spec.Type]
		quotaResources = append(quotaResources, map[string]any{
			"name": spec.Type, "nominalQuota": strconv.Itoa(count),
		})
	}

	cqLabels := map[string]string{
		"rhai-tmm.dev/until": untilStr,
	}
	// System bookings record their target namespace on the ClusterQueue so the
	// drain/cleanup paths can find the LocalQueue/HardwareProfiles to remove.
	if res.IsSystem {
		cqLabels["rhai-tmm.dev/namespace"] = ns
	}

	cq := map[string]any{
		"apiVersion": "kueue.x-k8s.io/v1beta1",
		"kind":       "ClusterQueue",
		"metadata": map[string]any{
			"name":   cqName,
			"labels": cqLabels,
		},
		"spec": map[string]any{
			"cohort": "unreserved",
			"namespaceSelector": map[string]any{
				"matchLabels": map[string]string{
					"kubernetes.io/metadata.name": ns,
				},
			},
			"preemption": map[string]any{
				"borrowWithinCohort": map[string]any{
					"policy": "Never",
				},
				"reclaimWithinCohort": "Any",
				"withinClusterQueue":  "Never",
			},
			"flavorFungibility": map[string]any{
				"whenCanBorrow":  "Borrow",
				"whenCanPreempt": "TryNextFlavor",
			},
			"queueingStrategy": "BestEffortFIFO",
			"stopPolicy":       "None",
			"resourceGroups": []map[string]any{
				{
					"coveredResources": coveredResources,
					"flavors": []map[string]any{
						{
							"name":      database.FlavorName(),
							"resources": quotaResources,
						},
					},
				},
			},
		},
	}
	if err := k8sApply("/apis/kueue.x-k8s.io/v1beta1/clusterqueues/"+cqName, cq); err != nil {
		return fmt.Errorf("applying ClusterQueue %s: %w", cqName, err)
	}

	lq := map[string]any{
		"apiVersion": "kueue.x-k8s.io/v1beta1",
		"kind":       "LocalQueue",
		"metadata": map[string]any{
			"name":      "reserved",
			"namespace": ns,
			"labels": map[string]string{
				"rhai-tmm.dev/until": untilStr,
			},
			"annotations": map[string]string{
				"argocd.argoproj.io/sync-options": "SkipDryRunOnMissingResource=true",
				"argocd.argoproj.io/sync-wave":    "1",
			},
		},
		"spec": map[string]any{
			"clusterQueue": cqName,
		},
	}
	if err := k8sApply(fmt.Sprintf("/apis/kueue.x-k8s.io/v1beta1/namespaces/%s/localqueues/reserved", ns), lq); err != nil {
		slog.Error("reservation sync: failed to apply LocalQueue", "namespace", ns, "error", err)
	}

	for gpuRes, count := range res.Resources {
		normalized := normalizedResourceID(gpuRes)
		profileName := "reserved-" + normalized

		defaultCPU := min(2, res.CPU)
		defaultMemGi := min(8, res.Memory)

		identifiers := []map[string]any{
			{
				"identifier":   "cpu",
				"displayName":  "CPU",
				"resourceType": "CPU",
				"minCount":     "250m",
				"maxCount":     res.CPU,
				"defaultCount": defaultCPU,
			},
			{
				"identifier":   "memory",
				"displayName":  "Memory",
				"resourceType": "Memory",
				"minCount":     "250Mi",
				"maxCount":     fmt.Sprintf("%dGi", res.Memory),
				"defaultCount": fmt.Sprintf("%dGi", defaultMemGi),
			},
			{
				"identifier":   gpuRes,
				"displayName":  strings.ToUpper(normalized),
				"resourceType": "Accelerator",
				"minCount":     1,
				"maxCount":     count,
				"defaultCount": 1,
			},
		}

		hp := map[string]any{
			"apiVersion": "infrastructure.opendatahub.io/v1",
			"kind":       "HardwareProfile",
			"metadata": map[string]any{
				"name":      profileName,
				"namespace": ns,
				"labels": map[string]string{
					"rhai-tmm.dev/until": untilStr,
				},
				"annotations": map[string]string{
					"opendatahub.io/dashboard-feature-visibility": "[]",
					"opendatahub.io/disabled":                     "false",
					"opendatahub.io/display-name":                 "Reserved " + strings.ToUpper(normalized),
					"opendatahub.io/managed":                      "false",
					"opendatahub.io/description":                  fmt.Sprintf("Your reserved quota of %d %s", count, gpuRes),
				},
			},
			"spec": map[string]any{
				"identifiers": identifiers,
				"scheduling": map[string]any{
					"type": "Queue",
					"kueue": map[string]any{
						"localQueueName": "reserved",
						"priorityClass":  "None",
					},
				},
			},
		}

		// DRA-backed resources: the profile references a namespaced
		// ResourceClaimTemplate the reservation also provisions, so workloads
		// admitted through it get a DRA ResourceClaim (DeviceClass
		// gpu.nvidia.com; consumable shares make claims co-allocate on one
		// device, one share per claim).
		if specInfo, ok := database.GPUSpecByType(gpuRes); ok && specInfo.DRAMode {
			rctName := profileName + "-rct"
			rct := map[string]any{
				"apiVersion": "resource.k8s.io/v1",
				"kind":       "ResourceClaimTemplate",
				"metadata": map[string]any{
					"name":      rctName,
					"namespace": ns,
					"labels": map[string]string{
						"rhai-tmm.dev/until": untilStr,
					},
				},
				"spec": map[string]any{
					"spec": map[string]any{
						"devices": map[string]any{
							"requests": []map[string]any{
								{
									"name": "gpu",
									"exactly": map[string]any{
										"deviceClassName": database.DRADeviceClass,
									},
								},
							},
						},
					},
				},
			}
			if err := k8sApply(fmt.Sprintf("/apis/resource.k8s.io/v1/namespaces/%s/resourceclaimtemplates/%s", ns, rctName), rct); err != nil {
				slog.Error("reservation sync: failed to apply ResourceClaimTemplate", "namespace", ns, "template", rctName, "error", err)
			}
			hp["spec"].(map[string]any)["dra"] = map[string]any{
				"resourceClaimTemplateName": rctName,
			}
		}

		if err := k8sApply(
			fmt.Sprintf("/apis/infrastructure.opendatahub.io/v1/namespaces/%s/hardwareprofiles/%s", ns, profileName),
			hp,
		); err != nil {
			slog.Error("reservation sync: failed to apply HardwareProfile", "namespace", ns, "profile", profileName, "error", err)
		}
	}

	return nil
}

func applyCohortRemaining(reservations []userReservation) error {
	cfg := database.GetGPUConfig()
	if len(cfg.Resources) == 0 {
		return nil
	}
	remainingCPU := cfg.TotalCPU
	remainingMem := cfg.TotalMemory
	remainingGPUs := map[string]int{}
	for _, spec := range cfg.Resources {
		remainingGPUs[spec.Type] = spec.Count
	}

	for _, res := range reservations {
		remainingCPU -= res.CPU
		remainingMem -= res.Memory
		for gpuRes, count := range res.Resources {
			remainingGPUs[gpuRes] -= count
		}
	}

	coveredResources := []string{"cpu", "memory"}
	quotaResources := []map[string]any{
		{"name": "cpu", "nominalQuota": strconv.Itoa(remainingCPU)},
		{"name": "memory", "nominalQuota": fmt.Sprintf("%dGi", remainingMem)},
	}
	for res, count := range remainingGPUs {
		coveredResources = append(coveredResources, res)
		quotaResources = append(quotaResources, map[string]any{
			"name": res, "nominalQuota": strconv.Itoa(count),
		})
	}

	cohort := map[string]any{
		"apiVersion": "kueue.x-k8s.io/v1beta1",
		"kind":       "Cohort",
		"metadata": map[string]any{
			"name": "unreserved",
		},
		"spec": map[string]any{
			"resourceGroups": []map[string]any{
				{
					"coveredResources": coveredResources,
					"flavors": []map[string]any{
						{
							"name":      database.FlavorName(),
							"resources": quotaResources,
						},
					},
				},
			},
		},
	}

	return k8sApply("/apis/kueue.x-k8s.io/v1beta1/cohorts/unreserved", cohort)
}

// hasBookingsForUnknownResource reports whether the user holds reserved
// bookings on a resource type the current GPU config no longer lists — the
// signature of a discovery-drift window (transient empty/renamed MIG surface)
// rather than an expired reservation. Fails open (false) on a DB error so the
// drain path keeps its pre-guard behavior when the database is unreachable.
func hasBookingsForUnknownResource(user string) bool {
	rows, err := database.DB().Query(
		"SELECT DISTINCT resource FROM bookings WHERE user = ? AND source = ?",
		user, database.SourceReserved,
	)
	if err != nil {
		return false
	}
	defer rows.Close()
	for rows.Next() {
		var res string
		if err := rows.Scan(&res); err != nil {
			continue
		}
		if _, ok := database.GPUSpecByType(res); !ok {
			return true
		}
	}
	return false
}

func removeStaleReservations(activeUsers map[string]bool) error {
	items, err := k8sListWithLabel(
		"/apis/kueue.x-k8s.io/v1beta1/clusterqueues",
		"rhai-tmm.dev/until",
	)
	if err != nil {
		return fmt.Errorf("listing labeled ClusterQueues: %w", err)
	}

	for _, item := range items {
		if activeUsers[item.Name] {
			continue
		}

		// Config-drift guard: discovery can temporarily drop or rename a
		// booked resource type (e.g. the MIG surface flapping during
		// whole-board claim churn), which empties the active set without any
		// booking actually expiring. Draining in that state destroys live
		// reservations. Skip CQs whose user still holds reserved bookings for
		// resource types the current GPU config doesn't know about. A CQ kept
		// alive by this guard is cleaned up once discovery repopulates the
		// resource (active set heals) or by cleanExpiredReservations when the
		// until label lapses.
		if user, found := strings.CutPrefix(item.Name, "system-"); found {
			if hasBookingsForUnknownResource(user) {
				slog.Warn("reservation sync: skipping drain, booked resource missing from GPU config (discovery drift?)",
					"clusterqueue", item.Name, "user", user)
				continue
			}
		}

		ns := namespaceForCQ(item.Name, item.Labels)

		// Already draining — check if ready to finalize
		if item.Labels["rhai-tmm.dev/draining"] == "true" {
			finalizeDrainedClusterQueue(item)
			continue
		}

		// Phase 1: initiate drain
		workloads := getClusterQueueWorkloadCount(item.Name)
		if workloads == 0 {
			// No workloads — delete immediately
			deleteUserReservationResources(item.Name, ns)
			slog.Info("reservation sync: removed stale reservation", "clusterqueue", item.Name)
		} else {
			// Has workloads — initiate graceful drain
			drainClusterQueue(item.Name, ns)
		}
	}

	return nil
}

func cleanExpiredReservations() error {
	now := time.Now().UTC().Unix()

	items, err := k8sListWithLabel(
		"/apis/kueue.x-k8s.io/v1beta1/clusterqueues",
		"rhai-tmm.dev/until",
	)
	if err != nil {
		return fmt.Errorf("listing labeled ClusterQueues: %w", err)
	}

	// Also find draining CQs that may have lost their until label
	drainingItems, err := k8sListWithLabel(
		"/apis/kueue.x-k8s.io/v1beta1/clusterqueues",
		"rhai-tmm.dev/draining",
	)
	if err == nil {
		seen := map[string]bool{}
		for _, item := range items {
			seen[item.Name] = true
		}
		for _, item := range drainingItems {
			if !seen[item.Name] {
				items = append(items, item)
			}
		}
	}

	var expired, active, draining int
	for _, item := range items {
		// Already draining — check if ready to finalize
		if item.Labels["rhai-tmm.dev/draining"] == "true" {
			if finalizeDrainedClusterQueue(item) {
				expired++
			} else {
				draining++
			}
			continue
		}

		untilStr, ok := item.Labels["rhai-tmm.dev/until"]
		if !ok {
			continue
		}
		until, err := strconv.ParseInt(untilStr, 10, 64)
		if err != nil {
			continue
		}
		if until < now {
			workloads := getClusterQueueWorkloadCount(item.Name)
			if workloads == 0 {
				deleteUserReservationResources(item.Name, namespaceForCQ(item.Name, item.Labels))
				slog.Info("reservation cleaner: deleted expired reservation", "clusterqueue", item.Name, "until", untilStr)
				expired++
			} else {
				drainClusterQueue(item.Name, namespaceForCQ(item.Name, item.Labels))
				draining++
			}
		} else {
			active++
		}
	}

	if expired > 0 || draining > 0 {
		slog.Info("reservation cleaner: summary", "expired", expired, "draining", draining, "active", active)
	}

	return nil
}

// getClusterQueueWorkloadCount returns the number of admitted + pending workloads
// on a ClusterQueue by reading its status. Returns 0 if the CQ doesn't exist.
func getClusterQueueWorkloadCount(name string) int {
	body, err := K8sGet(fmt.Sprintf("/apis/kueue.x-k8s.io/v1beta1/clusterqueues/%s", name))
	if err != nil {
		return 0
	}
	var cq struct {
		Status struct {
			AdmittedWorkloads  int `json:"admittedWorkloads"`
			PendingWorkloads   int `json:"pendingWorkloads"`
			ReservingWorkloads int `json:"reservingWorkloads"`
		} `json:"status"`
	}
	if err := json.Unmarshal(body, &cq); err != nil {
		return 0
	}
	return cq.Status.AdmittedWorkloads + cq.Status.PendingWorkloads + cq.Status.ReservingWorkloads
}

// namespaceForCQ derives the Kubernetes namespace holding a ClusterQueue's
// LocalQueue and HardwareProfiles. System bookings label their ClusterQueue
// with the target namespace; for user bookings the CQ name equals the
// namespace. Falls back to stripping the "system-" prefix for system CQs
// missing the label.
func namespaceForCQ(name string, labels map[string]string) string {
	if ns, ok := labels["rhai-tmm.dev/namespace"]; ok && ns != "" {
		return ns
	}
	if ns, ok := strings.CutPrefix(name, "system-"); ok {
		return ns
	}
	return name
}

// drainClusterQueue sets stopPolicy=HoldAndDrain and adds draining labels,
// then deletes the LocalQueue and HardwareProfiles to prevent new submissions.
func drainClusterQueue(cqName, ns string) {
	nowStr := strconv.FormatInt(time.Now().UTC().Unix(), 10)

	labels := map[string]string{
		"rhai-tmm.dev/draining":    "true",
		"rhai-tmm.dev/drain-start": nowStr,
	}
	// Preserve the until label so cleanExpiredReservations can still find this CQ
	body, err := K8sGet(fmt.Sprintf("/apis/kueue.x-k8s.io/v1beta1/clusterqueues/%s", cqName))
	if err == nil {
		var existing struct {
			Metadata struct {
				Labels map[string]string `json:"labels"`
			} `json:"metadata"`
		}
		if json.Unmarshal(body, &existing) == nil {
			if v, ok := existing.Metadata.Labels["rhai-tmm.dev/until"]; ok {
				labels["rhai-tmm.dev/until"] = v
			}
		}
	}

	patch := map[string]any{
		"apiVersion": "kueue.x-k8s.io/v1beta1",
		"kind":       "ClusterQueue",
		"metadata": map[string]any{
			"name":   cqName,
			"labels": labels,
		},
		"spec": map[string]any{
			"stopPolicy": "HoldAndDrain",
		},
	}
	if err := k8sApply("/apis/kueue.x-k8s.io/v1beta1/clusterqueues/"+cqName, patch); err != nil {
		slog.Error("reservation drain: failed to set HoldAndDrain", "clusterqueue", cqName, "error", err)
		return
	}

	// Delete LocalQueue, HardwareProfiles and ResourceClaimTemplates
	// immediately to block new submissions
	deleteLabeledInNamespace(ns, "/apis/infrastructure.opendatahub.io/v1/namespaces/%s/hardwareprofiles", "reservation drain: failed to delete HardwareProfile")
	deleteLabeledInNamespace(ns, "/apis/resource.k8s.io/v1/namespaces/%s/resourceclaimtemplates", "reservation drain: failed to delete ResourceClaimTemplate")

	if err := k8sDeletePath(fmt.Sprintf(
		"/apis/kueue.x-k8s.io/v1beta1/namespaces/%s/localqueues/reserved", ns,
	)); err != nil {
		slog.Error("reservation drain: failed to delete LocalQueue", "namespace", ns, "error", err)
	}

	slog.Info("reservation drain: initiated HoldAndDrain", "clusterqueue", cqName)
}

// finalizeDrainedClusterQueue checks if a draining CQ is ready to delete.
// Returns true if the CQ was deleted (or force-deleted due to timeout).
func finalizeDrainedClusterQueue(item k8sResourceItem) bool {
	workloads := getClusterQueueWorkloadCount(item.Name)

	if workloads == 0 {
		if err := k8sDeletePath(fmt.Sprintf(
			"/apis/kueue.x-k8s.io/v1beta1/clusterqueues/%s", item.Name,
		)); err != nil {
			slog.Error("reservation drain: failed to delete drained ClusterQueue", "clusterqueue", item.Name, "error", err)
			return false
		}
		slog.Info("reservation drain: deleted drained ClusterQueue", "clusterqueue", item.Name)
		return true
	}

	// Check drain timeout
	drainStartStr := item.Labels["rhai-tmm.dev/drain-start"]
	if drainStartStr != "" {
		drainStart, err := strconv.ParseInt(drainStartStr, 10, 64)
		if err == nil && time.Now().UTC().Unix()-drainStart > drainTimeoutSeconds {
			slog.Warn("reservation drain: force-deleting ClusterQueue after drain timeout", "clusterqueue", item.Name, "active_workloads", workloads)
			if err := k8sDeletePath(fmt.Sprintf(
				"/apis/kueue.x-k8s.io/v1beta1/clusterqueues/%s", item.Name,
			)); err != nil {
				slog.Error("reservation drain: failed to force-delete ClusterQueue", "clusterqueue", item.Name, "error", err)
			}
			return true
		}
	}

	slog.Info("reservation drain: waiting for workloads to drain", "clusterqueue", item.Name, "active_workloads", workloads)
	return false
}

func deleteUserReservationResources(cqName, ns string) {
	deleteLabeledInNamespace(ns, "/apis/infrastructure.opendatahub.io/v1/namespaces/%s/hardwareprofiles", "reservation cleanup: failed to delete HardwareProfile")
	deleteLabeledInNamespace(ns, "/apis/resource.k8s.io/v1/namespaces/%s/resourceclaimtemplates", "reservation cleanup: failed to delete ResourceClaimTemplate")

	if err := k8sDeletePath(fmt.Sprintf(
		"/apis/kueue.x-k8s.io/v1beta1/namespaces/%s/localqueues/reserved", ns,
	)); err != nil {
		slog.Error("reservation cleanup: failed to delete LocalQueue", "namespace", ns, "error", err)
	}

	if err := k8sDeletePath(fmt.Sprintf(
		"/apis/kueue.x-k8s.io/v1beta1/clusterqueues/%s", cqName,
	)); err != nil {
		slog.Error("reservation cleanup: failed to delete ClusterQueue", "clusterqueue", cqName, "error", err)
	}
}

// deleteLabeledInNamespace deletes every resource listed at basePathFormat
// (a format string taking the namespace) that carries the reservation until
// label. Used for HardwareProfiles and DRA ResourceClaimTemplates during
// reservation drain/cleanup.
func deleteLabeledInNamespace(ns, basePathFormat, errContext string) {
	basePath := fmt.Sprintf(basePathFormat, ns)
	items, err := k8sListWithLabel(basePath, "rhai-tmm.dev/until")
	if err != nil {
		return
	}
	for _, item := range items {
		if err := k8sDeletePath(basePath + "/" + item.Name); err != nil {
			slog.Error(errContext, "namespace", ns, "name", item.Name, "error", err)
		}
	}
}

func k8sApply(path string, manifest map[string]any) error {
	body, err := json.Marshal(manifest)
	if err != nil {
		return fmt.Errorf("marshaling manifest: %w", err)
	}

	u := k8sHost + path + "?fieldManager=booking-app&force=true"
	req, err := http.NewRequest("PATCH", u, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+k8sToken)
	req.Header.Set("Content-Type", "application/apply-patch+yaml")
	req.Header.Set("Accept", "application/json")

	resp, err := k8sHTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("k8s apply %s: %w", path, err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return fmt.Errorf("k8s apply %s returned %d: %s", path, resp.StatusCode, truncateBody(respBody))
	}

	return nil
}

func k8sDeletePath(path string) error {
	req, err := http.NewRequest("DELETE", k8sHost+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+k8sToken)
	req.Header.Set("Accept", "application/json")

	resp, err := k8sHTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("k8s delete %s: %w", path, err)
	}
	defer resp.Body.Close()
	io.ReadAll(resp.Body)

	if resp.StatusCode >= 300 && resp.StatusCode != 404 {
		return fmt.Errorf("k8s delete %s returned %d", path, resp.StatusCode)
	}

	return nil
}

type k8sResourceItem struct {
	Name      string
	Namespace string
	Labels    map[string]string
}

func k8sListWithLabel(basePath, labelKey string) ([]k8sResourceItem, error) {
	path := fmt.Sprintf("%s?labelSelector=%s", basePath, url.QueryEscape(labelKey))
	body, err := K8sGet(path)
	if err != nil {
		return nil, err
	}

	var result struct {
		Items []struct {
			Metadata struct {
				Name      string            `json:"name"`
				Namespace string            `json:"namespace"`
				Labels    map[string]string `json:"labels"`
			} `json:"metadata"`
		} `json:"items"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("parsing list response: %w", err)
	}

	items := make([]k8sResourceItem, len(result.Items))
	for i, item := range result.Items {
		items[i] = k8sResourceItem{
			Name:      item.Metadata.Name,
			Namespace: item.Metadata.Namespace,
			Labels:    item.Metadata.Labels,
		}
	}
	return items, nil
}

func truncateBody(body []byte) string {
	s := string(body)
	if len(s) > 200 {
		return s[:200] + "..."
	}
	return s
}
