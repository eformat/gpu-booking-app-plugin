package kube

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"strconv"
	"strings"

	"github.com/eformat/gpu-booking-plugin/pkg/database"
)

// DRA discovery: reads resource.k8s.io/v1 ResourceSlices published by the
// NVIDIA DRA driver (DeviceClass gpu.nvidia.com) and maps consumable-share
// and dedicated devices onto booking catalog specs. Devices carrying MIG
// profile attributes or partitionable-device counters are skipped for now
// (dynamic MIG booking lands with the DRA Phase 2 work).

const draResourceSlicesPath = "/apis/resource.k8s.io/v1/resourceslices"

type draSliceList struct {
	Items []draSlice `json:"items"`
}

type draSlice struct {
	Spec struct {
		Driver   string `json:"driver"`
		NodeName string `json:"nodeName"`
		Pool     struct {
			Name string `json:"name"`
		} `json:"pool"`
		Devices []draDevice `json:"devices"`
	} `json:"spec"`
}

type draDevice struct {
	Name       string                       `json:"name"`
	Attributes map[string]draAttribute      `json:"attributes"`
	Capacity   map[string]draDeviceCapacity `json:"capacity"`
	// Partitionable devices (KEP-4815, GA k8s 1.36 / OCP 5): the full
	// physical device advertises a counter set; partition devices declare
	// which counters they consume. Dynamic MIG publishes partitions this
	// way — advertised before they physically exist, realized on claim.
	Counters map[string]draCounter `json:"counters,omitempty"`
	// ConsumesCounters presence means this device is a PARTITION of a
	// counter-set device (e.g. a MIG profile instance).
	ConsumesCounters []draCounterConsumption `json:"consumesCounters,omitempty"`
	// Consumable capacity (DRA consumable shares, OCP 5 / k8s 1.36): when
	// true the device can back multiple claims, one per share.
	AllowMultipleAllocations bool `json:"allowMultipleAllocations"`
}

type draCounter struct {
	// The API spec says int64, but the live driver serializes the value as
	// a JSON string — accept both via json.Number.
	Value json.Number `json:"value"`
}

// miB returns the counter value in MiB (NVIDIA memory counters). Bare
// numbers are MiB; suffixed quantities go through parseMemoryMi.
func (c draCounter) miB() int {
	s := string(c.Value)
	if s == "" || s == "null" {
		return 0
	}
	if v, err := strconv.ParseInt(s, 10, 64); err == nil {
		return int(v)
	}
	return parseMemoryMi(s)
}

type draCounterConsumption struct {
	CounterSet string                `json:"counterSet"`
	Counters   map[string]draCounter `json:"counters"`
}

type draAttribute struct {
	Int     int64  `json:"int,omitempty"`
	Bool    bool   `json:"bool,omitempty"`
	String  string `json:"string,omitempty"`
	Version string `json:"version,omitempty"`
}

type draDeviceCapacity struct {
	Value         string            `json:"value,omitempty"`
	RequestPolicy *draRequestPolicy `json:"requestPolicy,omitempty"`
}

type draRequestPolicy struct {
	Default    string         `json:"default,omitempty"`
	ValidRange *draValidRange `json:"validRange,omitempty"`
}

type draValidRange struct {
	Min  string `json:"min,omitempty"`
	Max  string `json:"max,omitempty"`
	Step string `json:"step,omitempty"`
}

// Attribute keys published by the NVIDIA DRA driver / FGO dra-plugin-gpu.
const (
	draAttrProductName = "gpu.nvidia.com/productName"
	draAttrModel       = "gpu.nvidia.com/model"
	draAttrProfile     = "gpu.nvidia.com/profile"
	draAttrMIGProfile  = "gpu.nvidia.com/mig-profile"
	draAttrType        = "gpu.nvidia.com/type"
)

// Attribute keys published for GPU devices. The NVIDIA DRA driver publishes
// unqualified keys (productName, type, uuid); FGO's dra-plugin-gpu publishes
// gpu.nvidia.com/-qualified keys. Read both.
var draProductAttrKeys = []string{
	draAttrProductName, "productName", draAttrModel, "model",
}

var draTypeAttrKeys = []string{draAttrType, "type"}

var draProfileAttrKeys = []string{
	draAttrProfile, "profile", draAttrMIGProfile, "migProfile",
}

// discoverDRAResources lists DRA ResourceSlices and returns the raw list
// body (nil when the DRA API is absent). Callers parse node coverage and
// specs from the same body — one API call per discovery cycle. A cluster
// without the DRA API (404) yields no error, so pre-OCP-5 clusters keep
// using device-plugin discovery alone.
func fetchDRASliceBody() ([]byte, error) {
	if !K8sAvailable() {
		return nil, nil
	}
	body, err := K8sGet(draResourceSlicesPath + "?limit=500")
	if err != nil {
		if strings.Contains(err.Error(), "returned 404") {
			slog.Debug("dra discovery: resource.k8s.io/v1 not served, skipping")
			return nil, nil
		}
		return nil, fmt.Errorf("listing resourceslices: %w", err)
	}
	return body, nil
}

// draSliceNodeNames returns the set of nodes publishing DRA slices for the
// NVIDIA device class. Devices on these nodes are DRA-managed and must not
// also be counted by device-plugin (extended-resource) discovery.
func draSliceNodeNames(body []byte) map[string]bool {
	nodes := map[string]bool{}
	if len(body) == 0 {
		return nodes
	}
	var list draSliceList
	if err := json.Unmarshal(body, &list); err != nil {
		return nodes
	}
	for _, slice := range list.Items {
		if slice.Spec.Driver != database.DRADeviceClass {
			continue
		}
		if name := slice.Spec.NodeName; name != "" {
			nodes[name] = true
		}
	}
	return nodes
}

// parseDRASlices converts ResourceSlice JSON into booking catalog specs.
// The DRA driver publishes one pool per node, so devices are aggregated per
// (product, isolation mode) ACROSS pools — matching how device-plugin
// discovery aggregates nodes into one catalog entry. Consumable-share
// capacity defines how many claims each physical device can back.
func parseDRASlices(body []byte, hasGPUQuotaName bool) []database.GPUResourceSpec {
	var list draSliceList
	if err := json.Unmarshal(body, &list); err != nil {
		slog.Error("dra discovery: failed to parse resourceslice list", "error", err)
		return nil
	}

	type deviceGroup struct {
		product   string
		shared    bool
		mig       bool
		profile   string
		devices   int
		maxShares int
		memoryGB  int
		gpuEquiv  float64
		pools     []string // insertion order, for the informational Pool field
		poolSeen  map[string]bool
	}
	groups := map[string]*deviceGroup{}
	var order []string
	skippedNonGPU := 0
	skippedNoProfile := 0

	// partitionable counter-set devices: (pool, deviceName) -> total memory
	// (MiB) — the reference for partition GPU-equivalents.
	counterSets := map[string]int{}
	counterKey := func(pool, dev string) string { return pool + "\x00" + dev }

	addDevice := func(g *deviceGroup) {
		key := g.product + "\x00" + isolationKind(g.shared) + "\x00" + g.profile
		existing, ok := groups[key]
		if !ok {
			g.poolSeen = map[string]bool{}
			groups[key] = g
			order = append(order, key)
			return
		}
		existing.devices += g.devices
		if g.maxShares > existing.maxShares {
			existing.maxShares = g.maxShares
		}
		if g.memoryGB > existing.memoryGB {
			existing.memoryGB = g.memoryGB
		}
		if g.gpuEquiv > existing.gpuEquiv {
			existing.gpuEquiv = g.gpuEquiv
		}
	}

	trackPool := func(g *deviceGroup, pool string) {
		if pool == "" || g.poolSeen[pool] {
			return
		}
		g.poolSeen[pool] = true
		g.pools = append(g.pools, pool)
	}

	for _, slice := range list.Items {
		if slice.Spec.Driver != database.DRADeviceClass {
			continue
		}
		pool := slice.Spec.Pool.Name
		if pool == "" {
			pool = slice.Spec.NodeName
		}

		for _, dev := range slice.Spec.Devices {
			// Partitionable counter-set devices (the full physical GPU under
			// dynamic MIG): recorded for partition memory math, not emitted
			// as a bookable catalog entry — DRA counter exclusivity already
			// prevents full+partition double-booking, and the booking app
			// books profiles, not raw counter sets.
			if len(dev.Counters) > 0 {
				memMiB := 0
				if c, ok := dev.Counters["memory"]; ok {
					// NVIDIA memory counters are MiB.
					memMiB = c.miB()
				} else if memStr, ok := dev.Capacity["memory"]; ok {
					memMiB = parseMemoryMi(memStr.Value)
				}
				if pool != "" {
					counterSets[counterKey(pool, dev.Name)] = memMiB
				}
				continue
			}

			// MIG partition devices (dynamic MIG): grouped per profile.
			// Partitions may ALSO be consumable-share capable (shares
			// capacity + allowMultipleAllocations — verified live on the
			// test bed): booking units per profile = partitions x shares.
			if len(dev.ConsumesCounters) > 0 {
				profile := attrStringAny(dev, draProfileAttrKeys)
				if profile == "" {
					skippedNoProfile++
					continue
				}
				product := attrStringAny(dev, draProductAttrKeys)
				memMiB := 0
				if memStr, ok := dev.Capacity["memory"]; ok {
					memMiB = parseMemoryMi(memStr.Value)
				} else {
					for _, cc := range dev.ConsumesCounters {
						if c, ok := cc.Counters["memory"]; ok {
							memMiB = c.miB()
						}
					}
				}
				// GPU-equivalent: partition memory vs the full physical
				// device's counter-set memory (both MiB).
				deviceMemMiB := 0
				for _, cc := range dev.ConsumesCounters {
					if m, ok := counterSets[counterKey(pool, cc.CounterSet)]; ok && m > 0 {
						deviceMemMiB = m
						break
					}
				}
				equiv := 1.0
				if deviceMemMiB > 0 && memMiB > 0 {
					equiv = math.Round(float64(memMiB)/float64(deviceMemMiB)*1000) / 1000
				}
				maxShares := 1
				if dev.AllowMultipleAllocations {
					maxShares = sharesPerDevice(dev)
				}
				g := &deviceGroup{
					product:   product,
					mig:       true,
					profile:   profile,
					devices:   1,
					maxShares: maxShares,
					memoryGB:  memMiB / 1024,
					gpuEquiv:  equiv,
				}
				addDevice(g)
				key := g.product + "\x00" + isolationKind(false) + "\x00" + g.profile
				trackPool(groups[key], pool)
				continue
			}

			// Only plain GPU devices for the shares/dedicated path: skip
			// other device types (MIG partitions published with a type
			// attribute, VFIO, ...) and profile-attributed devices.
			if t := attrStringAny(dev, draTypeAttrKeys); t != "" && t != "gpu" {
				skippedNonGPU++
				continue
			}
			if _, ok := dev.Attributes[draAttrProfile]; ok {
				skippedNoProfile++
				continue
			}
			if _, ok := dev.Attributes[draAttrMIGProfile]; ok {
				skippedNoProfile++
				continue
			}

			product := attrStringAny(dev, draProductAttrKeys)

			maxShares := 1
			if dev.AllowMultipleAllocations {
				maxShares = sharesPerDevice(dev)
			}

			memoryGB := 0
			if memStr, ok := dev.Capacity["memory"]; ok {
				memoryGB = parseMemoryGi(memStr.Value)
			}

			g := &deviceGroup{
				product:   product,
				shared:    maxShares > 1,
				devices:   1,
				maxShares: maxShares,
				memoryGB:  memoryGB,
			}
			addDevice(g)
			key := g.product + "\x00" + isolationKind(g.shared) + "\x00" + ""
			trackPool(groups[key], pool)
		}
	}

	if skippedNoProfile > 0 {
		slog.Info("dra discovery: skipped devices without MIG profile identity", "devices", skippedNoProfile)
	}
	if skippedNonGPU > 0 {
		slog.Info("dra discovery: skipped non-GPU devices", "devices", skippedNonGPU)
	}

	// With a single DRA product group and no device-plugin GPU the plain
	// "nvidia.com/gpu" quota name is kept (matches the deviceClassMappings
	// convention verified on ocp5-3). Otherwise groups get product-derived
	// logical quota names so ClusterQueue quotas and Kueue flavor usage stay
	// unambiguous per catalog entry.
	qualifyNames := hasGPUQuotaName || len(groups) > 1

	var specs []database.GPUResourceSpec
	for _, key := range order {
		g := groups[key]
		product := formatGPUProductName(g.product)

		spec := database.GPUResourceSpec{
			DRAMode:        true,
			DeviceMemoryGB: g.memoryGB,
		}

		switch {
		case g.mig:
			// Dynamic MIG profile instance: fluid inventory (the count
			// changes as partitions are created/destroyed), hard isolation
			// BETWEEN partitions. Partitions may additionally be
			// consumable-share capable (verified live): booking units are
			// then per-share, with per-share equivalents and memory.
			spec.Name = product + " MIG " + g.profile
			spec.Isolation = database.IsolationMIG
			spec.Count = g.devices
			spec.GPUEquivalent = g.gpuEquiv
			spec.DeviceMemoryGB = g.memoryGB
			if g.maxShares > 1 {
				spec.Count = g.devices * g.maxShares
				spec.MaxSharesPerDevice = g.maxShares
				spec.GPUEquivalent = math.Round((g.gpuEquiv/float64(g.maxShares))*1000) / 1000
				spec.DeviceMemoryGB = g.memoryGB / g.maxShares
			}
		case g.shared:
			spec.Name = product + " Shares"
			spec.Isolation = database.IsolationShared
			spec.MaxSharesPerDevice = g.maxShares
			spec.Count = g.devices * g.maxShares
			spec.GPUEquivalent = math.Round((1.0/float64(g.maxShares))*1000) / 1000
		default:
			spec.Name = product + " GPU"
			spec.Isolation = database.IsolationDedicated
			spec.Count = g.devices
			spec.GPUEquivalent = 1.0
		}
		if qualifyNames {
			spec.Type = draLogicalQuotaName(g.product, g.shared, g.profile)
		} else {
			spec.Type = "nvidia.com/gpu"
		}
		if len(g.pools) > 0 {
			spec.Pool = g.pools[0] // informational: first pool backing this entry
		}
		specs = append(specs, spec)
	}

	return specs
}

func isolationKind(shared bool) string {
	if shared {
		return "shared"
	}
	return "dedicated"
}

// draLogicalQuotaName derives a stable, Kueue-safe quota resource name from
// the device product (and MIG profile, for partition entries), e.g.
// "NVIDIA A100-SXM4-40GB" (shared) -> "a100-sxm4-40gb-share",
// "NVIDIA A100-SXM4-40GB" MIG profile "1g.12gb" -> "a100-sxm4-40gb-mig-1g-12gb".
// Stable across node counts, pool churn and re-partitioning.
func draLogicalQuotaName(product string, shared bool, profile string) string {
	base := product
	if profile != "" {
		base = product + "-mig-" + profile
	}
	var b strings.Builder
	for _, r := range strings.ToLower(base) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	normalized := strings.Trim(b.String(), "-")
	normalized = strings.TrimPrefix(normalized, "nvidia-")
	if normalized == "" {
		normalized = "gpu"
	}
	if profile != "" {
		// MIG entries are already unambiguous via the -mig-<profile> marker;
		// no dedicated/shared suffix.
		return normalized
	}
	if shared {
		return normalized + "-share"
	}
	return normalized + "-gpu"
}

// parseMemoryMi parses a Quantity string into MiB (partition capacities and
// counter values are compared in MiB for GPU-equivalent math).
func parseMemoryMi(s string) int {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	if val, ok := strings.CutSuffix(s, "Ki"); ok {
		v, _ := strconv.ParseInt(val, 10, 64)
		return int(v / 1024)
	}
	if val, ok := strings.CutSuffix(s, "Mi"); ok {
		v, _ := strconv.ParseInt(val, 10, 64)
		return int(v)
	}
	if val, ok := strings.CutSuffix(s, "Gi"); ok {
		v, _ := strconv.ParseInt(val, 10, 64)
		return int(v * 1024)
	}
	if val, ok := strings.CutSuffix(s, "Ti"); ok {
		v, _ := strconv.ParseInt(val, 10, 64)
		return int(v * 1024 * 1024)
	}
	v, _ := strconv.ParseInt(s, 10, 64)
	return int(v / (1024 * 1024))
}

func attrString(dev draDevice, key string) string {
	if attr, ok := dev.Attributes[key]; ok {
		return attr.String
	}
	return ""
}

// attrStringAny returns the first present string attribute among candidate
// keys (the NVIDIA DRA driver and FGO's plugin use different key schemes).
func attrStringAny(dev draDevice, keys []string) string {
	for _, key := range keys {
		if v := attrString(dev, key); v != "" {
			return v
		}
	}
	return ""
}

// parseQuotaInt parses a Quantity string that must be a plain integer count
// (consumable-share validRange values are counts like "4").
func parseQuotaInt(s string) (int, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty quantity")
	}
	return strconv.Atoi(s)
}

// sharesPerDevice extracts the consumable-share count from the device's
// "shares" capacity requestPolicy (validRange.max). Falls back to 1 when the
// driver publishes no policy — a claim then consumes the whole device.
func sharesPerDevice(dev draDevice) int {
	cap, ok := dev.Capacity["shares"]
	if !ok || cap.RequestPolicy == nil || cap.RequestPolicy.ValidRange == nil {
		return 1
	}
	max, err := parseQuotaInt(cap.RequestPolicy.ValidRange.Max)
	if err != nil || max <= 0 {
		return 1
	}
	return max
}
