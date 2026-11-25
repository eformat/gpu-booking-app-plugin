package kube

import (
	"fmt"
	"log/slog"
	"strings"

	"github.com/eformat/gpu-booking-plugin/pkg/database"
)

// Per-profile DeviceClasses (phase C1,
// ~/.opencode/plans/phase-c-h200-dra-booking.md): Kueue charges a DRA claim
// against the quota lane named by the claim's DeviceClass via the operator's
// deviceClassMappings (workloads/rhoai-operators/kueue-cr.yaml). The shared
// mig.nvidia.com class folds EVERY profile onto the whole lane, which made
// MIG bookings advisory: a claim referencing it charged h200-gpu and never
// touched the reserved per-lane quota. The sync therefore mirrors each MIG
// lane in the GPU catalog with a selecting DeviceClass
// (mig.nvidia.com-<dashed profile>) that claims reference to charge the
// exact lane; admission against a lane with zero quota then stays pending.

const (
	// deviceClassManagedLabel marks DeviceClasses this plugin owns, for GC.
	deviceClassManagedLabel = "rhai-tmm.dev/managed"
	deviceClassManagedValue = "gpu-booking-plugin"

	deviceClassAPIBase = "/apis/resource.k8s.io/v1/deviceclasses"
)

// migDeviceClassName derives the per-profile DeviceClass name for a MIG
// lane: the dotted profile attribute (7g.141gb) becomes the k8s-name-safe
// dashed form, matching the entries pre-provisioned in kueue-cr.yaml.
func migDeviceClassName(profile string) string {
	return "mig.nvidia.com-" + strings.ReplaceAll(profile, ".", "-")
}

// deviceClassManifest builds the selecting DeviceClass for one MIG profile.
// The CEL mirrors the shared mig.nvidia.com class narrowed to the profile.
func deviceClassManifest(name, profile string) map[string]any {
	return map[string]any{
		"apiVersion": "resource.k8s.io/v1",
		"kind":       "DeviceClass",
		"metadata": map[string]any{
			"name": name,
			"labels": map[string]any{
				deviceClassManagedLabel: deviceClassManagedValue,
			},
		},
		"spec": map[string]any{
			"selectors": []any{
				map[string]any{
					"cel": map[string]any{
						"expression": fmt.Sprintf(
							"device.driver == 'gpu.nvidia.com' && device.attributes['gpu.nvidia.com'].type == 'mig' && device.attributes['gpu.nvidia.com'].profile == '%s'",
							profile),
					},
				},
			},
		},
	}
}

// ApplyDeviceClasses mirrors every MIG lane in the catalog with its
// per-profile DeviceClass (idempotent server-side apply) and deletes managed
// classes whose lane left the catalog (GC). Skipped entirely when discovery
// returned no DRA lanes — a transient discovery failure must not tear down
// infrastructure that allocated claims still reference (same drift rule as
// the reservation cleaner's config-drift guard).
func ApplyDeviceClasses(resources []database.GPUResourceSpec) {
	desired := map[string]string{} // class name -> profile
	draAlive := false
	for _, r := range resources {
		if r.DRAMode {
			draAlive = true
		}
		if r.Isolation == database.IsolationMIG && r.Profile != "" {
			desired[migDeviceClassName(r.Profile)] = r.Profile
		}
	}
	if !draAlive {
		slog.Debug("deviceclass sync: no DRA lanes in discovery, skipping")
		return
	}

	for name, profile := range desired {
		if err := k8sApply(deviceClassAPIBase+"/"+name, deviceClassManifest(name, profile)); err != nil {
			slog.Error("deviceclass sync: failed to apply per-profile DeviceClass",
				"deviceclass", name, "profile", profile, "error", err)
		}
	}

	items, err := k8sListWithLabel(deviceClassAPIBase, deviceClassManagedLabel)
	if err != nil {
		slog.Warn("deviceclass sync: failed to list managed DeviceClasses for GC", "error", err)
		return
	}
	for _, item := range items {
		if _, ok := desired[item.Name]; ok {
			continue
		}
		if err := k8sDeletePath(deviceClassAPIBase + "/" + item.Name); err != nil {
			slog.Warn("deviceclass sync: failed to delete stale DeviceClass",
				"deviceclass", item.Name, "error", err)
			continue
		}
		slog.Info("deviceclass sync: removed stale per-profile DeviceClass", "deviceclass", item.Name)
	}
}
