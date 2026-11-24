package kube

import (
	"os"
	"testing"
)

// migDevice is a MIG partition device as the fork-main DRA driver publishes
// it: UNQUALIFIED attribute keys (profile, type, productName). The
// gpu.nvidia.com/-qualified spellings are what FGO's dra-plugin-gpu and the
// 0.5.1-era driver publish; attribution must read both.
func migDevice(name, profile string) draDevice {
	return draDevice{
		Name: name,
		Attributes: map[string]draAttribute{
			"profile":     {String: profile},
			"type":        {String: "mig"},
			"productName": {String: "NVIDIA A100-SXM4-40GB"},
		},
	}
}

func qualifiedMigDevice(name, profile string) draDevice {
	return draDevice{
		Name: name,
		Attributes: map[string]draAttribute{
			"gpu.nvidia.com/profile":     {String: profile},
			"gpu.nvidia.com/type":        {String: "mig"},
			"gpu.nvidia.com/productName": {String: "NVIDIA A100-SXM4-40GB"},
		},
	}
}

func gpuDevice(name string, shared bool) draDevice {
	return draDevice{
		Name:                     name,
		Attributes:               map[string]draAttribute{"type": {String: "gpu"}, "productName": {String: "NVIDIA A100-SXM4-40GB"}},
		AllowMultipleAllocations: shared,
	}
}

func claimWithDevices(ns, name string, results ...k8sClaimDeviceResult) k8sResourceClaim {
	claim := k8sResourceClaim{Metadata: k8sMetadata{Name: name, Namespace: ns}}
	claim.Status.Allocation = &struct {
		Devices struct {
			Results []k8sClaimDeviceResult `json:"results"`
		} `json:"devices"`
	}{}
	claim.Status.Allocation.Devices.Results = results
	return claim
}

func TestCatalogResourceForDevice_BareAndQualifiedKeys(t *testing.T) {
	// The fork-main driver publishes bare keys; the lanes it must land on
	// are named exactly as parseDRASlices names the catalog entries.
	mig, ok := catalogResourceForDevice(migDevice("gpu-0-mig-1g10gb-15-2", "1g.10gb"))
	if !ok || mig != "a100-sxm4-40gb-mig-1g-10gb" {
		t.Errorf("bare-key MIG device: got %q ok=%v, want a100-sxm4-40gb-mig-1g-10gb", mig, ok)
	}

	qualified, ok := catalogResourceForDevice(qualifiedMigDevice("gpu-0-mig-1g5gb-19-0", "1g.5gb"))
	if !ok || qualified != "a100-sxm4-40gb-mig-1g-5gb" {
		t.Errorf("qualified-key MIG device: got %q ok=%v, want a100-sxm4-40gb-mig-1g-5gb", qualified, ok)
	}

	// Whole-GPU devices name the lane by whether the device backs multiple
	// claims: consumable shares land on the share lane, dedicated on -gpu.
	shared, ok := catalogResourceForDevice(gpuDevice("gpu-0", true))
	if !ok || shared != "a100-sxm4-40gb-share" {
		t.Errorf("shared whole-GPU device: got %q ok=%v, want a100-sxm4-40gb-share", shared, ok)
	}
	dedicated, ok := catalogResourceForDevice(gpuDevice("gpu-0", false))
	if !ok || dedicated != "a100-sxm4-40gb-gpu" {
		t.Errorf("dedicated whole-GPU device: got %q ok=%v, want a100-sxm4-40gb-gpu", dedicated, ok)
	}

	// A device without a product identity has no lane; the caller falls
	// back to the quota-name view rather than guess.
	noProduct := migDevice("gpu-0-mig-1g10gb-15-2", "1g.10gb")
	delete(noProduct.Attributes, "productName")
	if _, ok := catalogResourceForDevice(noProduct); ok {
		t.Error("device without product attribute: expected not-ok")
	}

	// A MIG partition without its profile cannot name a lane either.
	noProfile := migDevice("gpu-0-mig-1g10gb-15-2", "1g.10gb")
	delete(noProfile.Attributes, "profile")
	if _, ok := catalogResourceForDevice(noProfile); ok {
		t.Error("MIG device without profile attribute: expected not-ok")
	}
}

func TestAttributeClaims_LanesAndConservation(t *testing.T) {
	devices := map[string]draDevice{
		"pool-a\x00gpu-0-mig-1g10gb-15-0": migDevice("gpu-0-mig-1g10gb-15-0", "1g.10gb"),
		"pool-a\x00gpu-0-mig-1g10gb-15-2": migDevice("gpu-0-mig-1g10gb-15-2", "1g.10gb"),
		"pool-b\x00gpu-0":                 gpuDevice("gpu-0", true),
	}
	managed := map[string]bool{"ns-a": true, "ns-b": true}

	// Two MIG bookings on one pool and a shares booking on another: each
	// lands on its own lane, and the attribution conserves the quota total
	// of 3 (the deviceClassMappings fold charges all three to one name).
	claims := []k8sResourceClaim{
		claimWithDevices("ns-a", "claim-mig-a",
			k8sClaimDeviceResult{Device: "gpu-0-mig-1g10gb-15-0", Pool: "pool-a"}),
		claimWithDevices("ns-a", "claim-mig-b",
			k8sClaimDeviceResult{Device: "gpu-0-mig-1g10gb-15-2", Pool: "pool-a"}),
		claimWithDevices("ns-b", "claim-gpu",
			k8sClaimDeviceResult{Device: "gpu-0", Pool: "pool-b"}),
	}
	attributed, ok := attributeClaims(claims, devices, 3, managed)
	if !ok {
		t.Fatal("expected attribution to conserve the quota total of 3")
	}
	if got := attributed[nsResKey{"ns-a", "a100-sxm4-40gb-mig-1g-10gb"}]; got != 2 {
		t.Errorf("1g.10gb lane: got %d, want 2", got)
	}
	if got := attributed[nsResKey{"ns-b", "a100-sxm4-40gb-share"}]; got != 1 {
		t.Errorf("shares lane: got %d, want 1", got)
	}

	// A claim outside the managed namespaces carries no quota unit behind
	// it — Kueue never charged it — so it is skipped and the conservation
	// check still holds for the rest.
	claims = append(claims, claimWithDevices("ns-unmanaged", "claim-raw",
		k8sClaimDeviceResult{Device: "gpu-0", Pool: "pool-b"}))
	attributed, ok = attributeClaims(claims, devices, 3, managed)
	if !ok {
		t.Fatal("unmanaged-namespace claim should be skipped, not fail the split")
	}
	if _, ok := attributed[nsResKey{"ns-unmanaged", "a100-sxm4-40gb-share"}]; ok {
		t.Error("unmanaged claim was attributed")
	}

	// A device the index cannot resolve fails the whole split — a partial
	// attribution would book consumption onto the wrong lanes.
	claims = append(claims, claimWithDevices("ns-a", "claim-stale",
		k8sClaimDeviceResult{Device: "gpu-7-mig-1g5gb-19-6", Pool: "pool-a"}))
	if _, ok := attributeClaims(claims, devices, 4, managed); ok {
		t.Error("unresolvable device: expected attribution to fail")
	}

	// A total that does not match the quota view fails too: the claims and
	// the quota disagree, and the quota-name view is the honest answer.
	if _, ok := attributeClaims(claims[:2], devices, 3, managed); ok {
		t.Error("count mismatch: expected attribution to fail")
	}
}

func TestBuildDRADeviceIndex_LiveFixture(t *testing.T) {
	body, err := os.ReadFile("testdata/resourceslices-live.json")
	if err != nil {
		t.Skipf("no live fixture: %v", err)
	}
	index := buildDRADeviceIndex(body)
	if len(index) == 0 {
		t.Fatal("live fixture indexed nothing")
	}
	// Index keys are exactly the coordinates a claim's allocation names.
	for key := range index {
		pool, dev, found := cutIndexKey(key)
		if !found || pool == "" || dev == "" {
			t.Fatalf("malformed index key %q", key)
		}
	}
}

// cutIndexKey splits an index key back into its pool and device name (the
// NUL separator is never part of either name).
func cutIndexKey(key string) (pool, dev string, found bool) {
	for i := 0; i < len(key); i++ {
		if key[i] == 0 {
			return key[:i], key[i+1:], true
		}
	}
	return "", "", false
}
