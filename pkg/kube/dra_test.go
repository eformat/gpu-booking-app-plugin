package kube

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/eformat/gpu-booking-plugin/pkg/database"
)

// draSharesSliceFixture mirrors the REAL NVIDIA DRA driver slice shape seen on
// the test bed: unqualified attribute keys (productName, type, uuid), one
// pool per node, consumable-share capacity with requestPolicy.
const draSharesSliceFixture = `{
  "items": [
    {
      "spec": {
        "driver": "gpu.nvidia.com",
        "nodeName": "node-1",
        "pool": { "name": "node-1", "generation": 2, "resourceSliceCount": 1 },
        "devices": [
          {
            "name": "gpu-0",
            "attributes": {
              "productName": { "string": "NVIDIA A100-SXM4-40GB" },
              "type": { "string": "gpu" },
              "uuid": { "string": "GPU-aaa1" },
              "architecture": { "string": "Ampere" }
            },
            "capacity": {
              "memory": { "value": "40Gi", "requestPolicy": { "default": "0", "validRange": { "min": "0", "max": "40Gi", "step": "1Mi" } } },
              "shares": { "value": "4", "requestPolicy": { "default": "1", "validRange": { "min": "1", "max": "4" } } }
            },
            "allowMultipleAllocations": true
          },
          {
            "name": "gpu-1",
            "attributes": {
              "productName": { "string": "NVIDIA A100-SXM4-40GB" },
              "type": { "string": "gpu" },
              "uuid": { "string": "GPU-aaa2" }
            },
            "capacity": {
              "memory": { "value": "40Gi" },
              "shares": { "value": "4", "requestPolicy": { "default": "1", "validRange": { "min": "1", "max": "4" } } }
            },
            "allowMultipleAllocations": true
          }
        ]
      }
    },
    {
      "spec": {
        "driver": "gpu.nvidia.com",
        "nodeName": "node-2",
        "pool": { "name": "node-2", "generation": 2, "resourceSliceCount": 1 },
        "devices": [
          {
            "name": "gpu-0",
            "attributes": {
              "productName": { "string": "NVIDIA A100-SXM4-40GB" },
              "type": { "string": "gpu" },
              "uuid": { "string": "GPU-aaa3" }
            },
            "capacity": {
              "memory": { "value": "40Gi" },
              "shares": { "value": "4", "requestPolicy": { "default": "1", "validRange": { "min": "1", "max": "4" } } }
            },
            "allowMultipleAllocations": true
          },
          {
            "name": "gpu-1",
            "attributes": {
              "productName": { "string": "NVIDIA A100-SXM4-40GB" },
              "type": { "string": "gpu" },
              "uuid": { "string": "GPU-aaa4" }
            },
            "capacity": {
              "memory": { "value": "40Gi" },
              "shares": { "value": "4", "requestPolicy": { "default": "1", "validRange": { "min": "1", "max": "4" } } }
            },
            "allowMultipleAllocations": true
          }
        ]
      }
    }
  ]
}`

func TestParseDRASlices_ConsumableShares(t *testing.T) {
	// Single DRA product group, no device-plugin GPU quota conflict: the spec
	// keeps the plain nvidia.com/gpu quota name (deviceClassMappings
	// convention, 1 quota unit per claim/share). Per-node pools aggregate
	// into ONE catalog entry.
	specs := parseDRASlices([]byte(draSharesSliceFixture), false)
	if len(specs) != 1 {
		t.Fatalf("expected 1 aggregated spec, got %d: %+v", len(specs), specs)
	}

	s := specs[0]
	if s.Type != "nvidia.com/gpu" {
		t.Errorf("Type = %q, want nvidia.com/gpu", s.Type)
	}
	if s.Name != "NVIDIA A100 SXM4 40GB Shares" {
		t.Errorf("Name = %q", s.Name)
	}
	// 2 nodes x 2 devices x 4 shares — pools aggregate into one entry.
	if s.Count != 16 {
		t.Errorf("Count = %d, want 16", s.Count)
	}
	if s.GPUEquivalent != 0.25 {
		t.Errorf("GPUEquivalent = %f, want 0.25", s.GPUEquivalent)
	}
	if !s.DRAMode {
		t.Error("DRAMode should be true")
	}
	if s.Isolation != "shared" {
		t.Errorf("Isolation = %q, want shared", s.Isolation)
	}
	if s.MaxSharesPerDevice != 4 {
		t.Errorf("MaxSharesPerDevice = %d, want 4", s.MaxSharesPerDevice)
	}
	if s.Pool != "node-1" {
		t.Errorf("Pool = %q, want first pool (node-1)", s.Pool)
	}
	if s.DeviceMemoryGB != 40 {
		t.Errorf("DeviceMemoryGB = %d, want 40", s.DeviceMemoryGB)
	}
}

func TestParseDRASlices_DualModeQualifiesType(t *testing.T) {
	// When the device-plugin path already claims the nvidia.com/gpu quota
	// name, DRA specs get product-derived logical quota names so ClusterQueue
	// quotas and Kueue flavor usage stay unambiguous per catalog entry.
	specs := parseDRASlices([]byte(draSharesSliceFixture), true)
	if len(specs) != 1 {
		t.Fatalf("expected 1 spec, got %d", len(specs))
	}
	if specs[0].Type != "a100-sxm4-40gb-share" {
		t.Errorf("Type = %q, want a100-sxm4-40gb-share (product-derived)", specs[0].Type)
	}
}

func TestParseDRASlices_DedicatedAndMIGSkipped(t *testing.T) {
	body := `{
  "items": [
    {
      "spec": {
        "driver": "gpu.nvidia.com",
        "nodeName": "node-1",
        "pool": { "name": "node-1" },
        "devices": [
          {
            "name": "gpu-0",
            "attributes": { "productName": { "string": "NVIDIA-A100-40GB" }, "type": { "string": "gpu" } },
            "capacity": { "memory": { "value": "40960Mi" } }
          },
          {
            "name": "mig-dev",
            "attributes": {
              "productName": { "string": "NVIDIA-A100-40GB" },
              "type": { "string": "mig" }
            },
            "capacity": { "memory": { "value": "12288Mi" } }
          },
          {
            "name": "profiled-dev",
            "attributes": {
              "productName": { "string": "NVIDIA-A100-40GB" },
              "gpu.nvidia.com/profile": { "string": "1g.12gb" }
            },
            "capacity": { "memory": { "value": "12288Mi" } }
          },
          {
            "name": "partitioned-dev",
            "attributes": { "productName": { "string": "NVIDIA-A100-40GB" } },
            "consumesCounters": [ { "counterSet": "gpu-0", "counters": { "memory": { "value": 40960 } } } ]
          }
        ]
      }
    }
  ]
}`

	specs := parseDRASlices([]byte(body), false)
	if len(specs) != 1 {
		t.Fatalf("expected 1 spec (mig/profile/counter devices skipped), got %d: %+v", len(specs), specs)
	}
	s := specs[0]
	if s.Isolation != "dedicated" {
		t.Errorf("Isolation = %q, want dedicated", s.Isolation)
	}
	if s.Count != 1 {
		t.Errorf("Count = %d, want 1 (only the plain GPU device)", s.Count)
	}
	if s.GPUEquivalent != 1.0 {
		t.Errorf("GPUEquivalent = %f, want 1.0", s.GPUEquivalent)
	}
	if s.Name != "NVIDIA A100 40GB GPU" {
		t.Errorf("Name = %q", s.Name)
	}
	if s.MaxSharesPerDevice != 0 {
		t.Errorf("MaxSharesPerDevice = %d, want 0 (unset for dedicated)", s.MaxSharesPerDevice)
	}
}

func TestParseDRASlices_MixedProductsSplit(t *testing.T) {
	// Different products are DIFFERENT catalog entries even when both are
	// shared and spread across the same nodes.
	body := `{
  "items": [
    {
      "spec": {
        "driver": "gpu.nvidia.com",
        "nodeName": "node-1",
        "pool": { "name": "node-1" },
        "devices": [
          {
            "name": "gpu-0",
            "attributes": { "productName": { "string": "NVIDIA A100-SXM4-40GB" }, "type": { "string": "gpu" } },
            "capacity": { "memory": { "value": "40Gi" }, "shares": { "value": "4", "requestPolicy": { "validRange": { "min": "1", "max": "4" } } } },
            "allowMultipleAllocations": true
          },
          {
            "name": "gpu-1",
            "attributes": { "productName": { "string": "NVIDIA H100-80GB" }, "type": { "string": "gpu" } },
            "capacity": { "memory": { "value": "80Gi" }, "shares": { "value": "4", "requestPolicy": { "validRange": { "min": "1", "max": "4" } } } },
            "allowMultipleAllocations": true
          }
        ]
      }
    }
  ]
}`

	specs := parseDRASlices([]byte(body), false)
	if len(specs) != 2 {
		t.Fatalf("expected 2 specs (one per product), got %d: %+v", len(specs), specs)
	}
	byName := map[string]string{}
	for _, s := range specs {
		byName[s.Name] = s.Type
	}
	if byName["NVIDIA A100 SXM4 40GB Shares"] != "a100-sxm4-40gb-share" {
		t.Errorf("A100 Type = %q, want a100-sxm4-40gb-share", byName["NVIDIA A100 SXM4 40GB Shares"])
	}
	if byName["NVIDIA H100 80GB Shares"] != "h100-80gb-share" {
		t.Errorf("H100 Type = %q, want h100-80gb-share", byName["NVIDIA H100 80GB Shares"])
	}
}

func TestParseDRASlices_DedicatedMultipleGroups(t *testing.T) {
	// Two dedicated product groups alongside each other: both get
	// product-derived quota names.
	body := `{
  "items": [
    {
      "spec": {
        "driver": "gpu.nvidia.com",
        "nodeName": "node-1",
        "pool": { "name": "node-1" },
        "devices": [
          {
            "name": "gpu-0",
            "attributes": { "productName": { "string": "NVIDIA A100-SXM4-40GB" }, "type": { "string": "gpu" } },
            "capacity": { "memory": { "value": "40Gi" } }
          },
          {
            "name": "gpu-1",
            "attributes": { "productName": { "string": "NVIDIA L40S" }, "type": { "string": "gpu" } },
            "capacity": { "memory": { "value": "48Gi" } }
          }
        ]
      }
    }
  ]
}`

	specs := parseDRASlices([]byte(body), false)
	if len(specs) != 2 {
		t.Fatalf("expected 2 specs, got %d: %+v", len(specs), specs)
	}
	types := map[string]bool{}
	for _, s := range specs {
		types[s.Type] = true
		if s.Isolation != "dedicated" {
			t.Errorf("%s Isolation = %q, want dedicated", s.Name, s.Isolation)
		}
	}
	if !types["a100-sxm4-40gb-gpu"] || !types["l40s-gpu"] {
		t.Errorf("expected product-derived types a100-sxm4-40gb-gpu + l40s-gpu, got %v", types)
	}
}

func TestParseDRASlices_MultipleAllocationsWithoutPolicy(t *testing.T) {
	// allowMultipleAllocations with no shares requestPolicy: a claim consumes
	// the whole device — treat as dedicated.
	body := `{
  "items": [
    {
      "spec": {
        "driver": "gpu.nvidia.com",
        "nodeName": "node-1",
        "pool": { "name": "node-1" },
        "devices": [
          {
            "name": "gpu-0",
            "attributes": { "productName": { "string": "NVIDIA-A100-40GB" }, "type": { "string": "gpu" } },
            "capacity": { "memory": { "value": "40960Mi" } },
            "allowMultipleAllocations": true
          }
        ]
      }
    }
  ]
}`

	specs := parseDRASlices([]byte(body), false)
	if len(specs) != 1 {
		t.Fatalf("expected 1 spec, got %d", len(specs))
	}
	if specs[0].Isolation != "dedicated" || specs[0].Count != 1 {
		t.Errorf("expected dedicated count=1, got isolation=%q count=%d", specs[0].Isolation, specs[0].Count)
	}
}

func TestParseDRAlices_FGOQualifiedKeys(t *testing.T) {
	// FGO's own dra-plugin-gpu publishes gpu.nvidia.com/-qualified keys —
	// must be read too.
	body := `{
  "items": [
    {
      "spec": {
        "driver": "gpu.nvidia.com",
        "nodeName": "node-1",
        "pool": { "name": "dra-share" },
        "devices": [
          {
            "name": "gpu-0",
            "attributes": {
              "gpu.nvidia.com/productName": { "string": "NVIDIA-A100-40GB" },
              "gpu.nvidia.com/type": { "string": "gpu" },
              "gpu.nvidia.com/uuid": { "string": "GPU-aaa1" }
            },
            "capacity": {
              "memory": { "value": "40960Mi" },
              "shares": { "value": "4", "requestPolicy": { "validRange": { "min": "1", "max": "4" } } }
            },
            "allowMultipleAllocations": true
          }
        ]
      }
    }
  ]
}`

	specs := parseDRASlices([]byte(body), false)
	if len(specs) != 1 {
		t.Fatalf("expected 1 spec, got %d: %+v", len(specs), specs)
	}
	if specs[0].Name != "NVIDIA A100 40GB Shares" {
		t.Errorf("Name = %q, want NVIDIA A100 40GB Shares", specs[0].Name)
	}
	if specs[0].Count != 4 {
		t.Errorf("Count = %d, want 4", specs[0].Count)
	}
}

func TestParseDRASlices_NoProductAttribute(t *testing.T) {
	// No product attribute at all: falls back to "GPU Shares" naming and a
	// generic quota name.
	body := `{
  "items": [
    {
      "spec": {
        "driver": "gpu.nvidia.com",
        "nodeName": "node-1",
        "pool": { "name": "node-1" },
        "devices": [
          {
            "name": "gpu-0",
            "attributes": { "uuid": { "string": "GPU-aaa1" } },
            "capacity": { "memory": { "value": "40Gi" }, "shares": { "value": "4", "requestPolicy": { "validRange": { "min": "1", "max": "4" } } } },
            "allowMultipleAllocations": true
          }
        ]
      }
    }
  ]
}`

	specs := parseDRASlices([]byte(body), false)
	if len(specs) != 1 {
		t.Fatalf("expected 1 spec, got %d", len(specs))
	}
	if specs[0].Name != "GPU Shares" {
		t.Errorf("Name = %q, want GPU Shares", specs[0].Name)
	}
	// Single product group + no device-plugin GPU: keeps the plain
	// nvidia.com/gpu quota name (deviceClassMappings convention).
	if specs[0].Type != "nvidia.com/gpu" {
		t.Errorf("Type = %q, want nvidia.com/gpu (single-group convention)", specs[0].Type)
	}
}

func TestParseDRASlices_NonNvidiaDriverIgnored(t *testing.T) {
	body := `{
  "items": [
    {
      "spec": {
        "driver": "example.com/fake",
        "nodeName": "node-1",
        "pool": { "name": "other" },
        "devices": [
          { "name": "dev-0", "attributes": {}, "capacity": {} }
        ]
      }
    }
  ]
}`

	specs := parseDRASlices([]byte(body), false)
	if len(specs) != 0 {
		t.Errorf("expected 0 specs for non-NVIDIA driver, got %d: %+v", len(specs), specs)
	}
}

func TestParseDRASlices_MalformedJSON(t *testing.T) {
	specs := parseDRASlices([]byte("{not json"), false)
	if specs != nil {
		t.Errorf("expected nil specs for malformed JSON, got %+v", specs)
	}
}

func TestDraLogicalQuotaName(t *testing.T) {
	tests := []struct {
		product string
		shared  bool
		profile string
		want    string
	}{
		{"NVIDIA A100-SXM4-40GB", true, "", "a100-sxm4-40gb-share"},
		{"NVIDIA A100-SXM4-40GB", false, "", "a100-sxm4-40gb-gpu"},
		{"NVIDIA L40S", false, "", "l40s-gpu"},
		{"NVIDIA H100-80GB", true, "", "h100-80gb-share"},
		{"NVIDIA A100-SXM4-40GB", false, "1g.12gb", "a100-sxm4-40gb-mig-1g-12gb"},
		{"NVIDIA H100-80GB", false, "3g.40gb", "h100-80gb-mig-3g-40gb"},
		{"", true, "", "gpu-share"},
	}
	for _, tt := range tests {
		got := draLogicalQuotaName(tt.product, tt.shared, tt.profile)
		if got != tt.want {
			t.Errorf("draLogicalQuotaName(%q, %v, %q) = %q, want %q", tt.product, tt.shared, tt.profile, got, tt.want)
		}
	}
}

func TestParseDRASlices_DynamicMIGPartitionable(t *testing.T) {
	// Dynamic MIG (KEP-4815 partitionable devices): the full device
	// advertises a counter set; partition devices consume counters and carry
	// a MIG profile attribute. Partitions group per profile; the full device
	// is not a bookable catalog entry.
	body := `{
  "items": [
    {
      "spec": {
        "driver": "gpu.nvidia.com",
        "nodeName": "node-1",
        "pool": { "name": "node-1" },
        "devices": [
          {
            "name": "gpu-0",
            "attributes": { "productName": { "string": "NVIDIA A100-SXM4-40GB" }, "type": { "string": "gpu" } },
            "capacity": { "memory": { "value": "40Gi" } },
            "counters": { "memory": { "value": 40960 } }
          },
          {
            "name": "gpu-0-mig-1",
            "attributes": { "productName": { "string": "NVIDIA A100-SXM4-40GB" }, "profile": { "string": "1g.12gb" } },
            "capacity": { "memory": { "value": "12288Mi" } },
            "consumesCounters": [ { "counterSet": "gpu-0", "counters": { "memory": { "value": 12288 } } } ]
          },
          {
            "name": "gpu-0-mig-2",
            "attributes": { "productName": { "string": "NVIDIA A100-SXM4-40GB" }, "profile": { "string": "1g.12gb" } },
            "capacity": { "memory": { "value": "12288Mi" } },
            "consumesCounters": [ { "counterSet": "gpu-0", "counters": { "memory": { "value": 12288 } } } ]
          }
        ]
      }
    }
  ]
}`

	specs := parseDRASlices([]byte(body), false)
	if len(specs) != 1 {
		t.Fatalf("expected 1 MIG profile spec (full device is not bookable), got %d: %+v", len(specs), specs)
	}
	s := specs[0]
	if s.Name != "NVIDIA A100 SXM4 40GB MIG 1g.12gb" {
		t.Errorf("Name = %q", s.Name)
	}
	if s.Isolation != "mig" {
		t.Errorf("Isolation = %q, want mig", s.Isolation)
	}
	if s.Count != 2 {
		t.Errorf("Count = %d, want 2 partitions", s.Count)
	}
	// 12288 / 40960 = 0.3
	if s.GPUEquivalent != 0.3 {
		t.Errorf("GPUEquivalent = %f, want 0.3", s.GPUEquivalent)
	}
	if s.DeviceMemoryGB != 12 {
		t.Errorf("DeviceMemoryGB = %d, want 12 (partition memory)", s.DeviceMemoryGB)
	}
	if s.MaxSharesPerDevice != 0 {
		t.Errorf("MaxSharesPerDevice = %d, want 0 for MIG", s.MaxSharesPerDevice)
	}
	// Single product group + no device-plugin GPU: plain quota name.
	if s.Type != "nvidia.com/gpu" {
		t.Errorf("Type = %q, want nvidia.com/gpu (single-group convention)", s.Type)
	}
}

func TestParseDRASlices_MixedSharesAndMIG(t *testing.T) {
	// The test bed's upcoming state: dra-share node with plain shares-mode
	// devices + dra-mig node with MIG partitions of the same product.
	body := `{
  "items": [
    {
      "spec": {
        "driver": "gpu.nvidia.com",
        "nodeName": "node-1",
        "pool": { "name": "node-1" },
        "devices": [
          {
            "name": "gpu-0",
            "attributes": { "productName": { "string": "NVIDIA A100-SXM4-40GB" }, "type": { "string": "gpu" } },
            "capacity": { "memory": { "value": "40Gi" }, "shares": { "value": "4", "requestPolicy": { "validRange": { "min": "1", "max": "4" } } } },
            "allowMultipleAllocations": true
          }
        ]
      }
    },
    {
      "spec": {
        "driver": "gpu.nvidia.com",
        "nodeName": "node-2",
        "pool": { "name": "node-2" },
        "devices": [
          {
            "name": "gpu-0",
            "attributes": { "productName": { "string": "NVIDIA A100-SXM4-40GB" }, "type": { "string": "gpu" } },
            "capacity": { "memory": { "value": "40Gi" } },
            "counters": { "memory": { "value": 40960 } }
          },
          {
            "name": "gpu-0-mig-1",
            "attributes": { "productName": { "string": "NVIDIA A100-SXM4-40GB" }, "profile": { "string": "2g.24gb" } },
            "capacity": { "memory": { "value": "24576Mi" }, "shares": { "value": "4", "requestPolicy": { "validRange": { "min": "1", "max": "4" } } } },
            "consumesCounters": [ { "counterSet": "gpu-0", "counters": { "memory": { "value": "24576" } } } ],
            "allowMultipleAllocations": true
          }
        ]
      }
    }
  ]
}`

	specs := parseDRASlices([]byte(body), false)
	if len(specs) != 2 {
		t.Fatalf("expected 2 specs (shares entry + MIG profile entry), got %d: %+v", len(specs), specs)
	}
	byName := map[string]struct {
		typ string
		iso string
		eq  float64
	}{
		"NVIDIA A100 SXM4 40GB Shares":      {},
		"NVIDIA A100 SXM4 40GB MIG 2g.24gb": {},
	}
	for _, s := range specs {
		if _, ok := byName[s.Name]; !ok {
			t.Errorf("unexpected spec %q", s.Name)
			continue
		}
		if s.Type != "a100-sxm4-40gb-share" && s.Type != "a100-sxm4-40gb-mig-2g-24gb" {
			t.Errorf("%s Type = %q, want product-derived", s.Name, s.Type)
		}
	}
	for _, s := range specs {
		if s.Name == "NVIDIA A100 SXM4 40GB Shares" {
			if s.Isolation != "shared" || s.Count != 4 {
				t.Errorf("shares entry: isolation=%q count=%d, want shared/4", s.Isolation, s.Count)
			}
		}
		if s.Name == "NVIDIA A100 SXM4 40GB MIG 2g.24gb" {
			// 1 booking unit = 1 whole partition: partition shares advertised
			// by the driver are deliberately NOT multiplied (decision
			// 2026-11-24). Equivalent = 24576/40960 per partition.
			if s.Isolation != "mig" || s.Count != 1 || s.GPUEquivalent != 0.6 || s.MaxSharesPerDevice != 0 || s.DeviceMemoryGB != 24 {
				t.Errorf("MIG entry: iso=%q count=%d equiv=%f shares=%d memGB=%d, want mig/1/0.6/0/24",
					s.Isolation, s.Count, s.GPUEquivalent, s.MaxSharesPerDevice, s.DeviceMemoryGB)
			}
		}
	}
}

func TestDraCounterValueForms(t *testing.T) {
	cases := map[string]int{
		`40960`:    40960,
		`"9856Mi"`: 9856,
		`"40Gi"`:   40960,
		`null`:     0,
	}
	for raw, want := range cases {
		var c draCounter
		if err := json.Unmarshal([]byte(`{"value": `+raw+`}`), &c); err != nil {
			t.Errorf("unmarshal %s: %v", raw, err)
			continue
		}
		if got := c.miB(); got != want {
			t.Errorf("miB(%s) = %d, want %d", raw, got, want)
		}
	}
}

// TestParseDRASlices_LiveFixture runs the parser against a capture of the
// REAL ResourceSlices from the FGO test bed (dra-share + dra-mig nodes with
// dynamic MIG armed). Regenerate with:
//
//	oc get resourceslices -o json > pkg/kube/testdata/resourceslices-live.json
func TestParseDRASlices_LiveFixture(t *testing.T) {
	body, err := os.ReadFile("testdata/resourceslices-live.json")
	if err != nil {
		t.Skipf("live fixture not present: %v", err)
	}

	specs := parseDRASlices(body, true) // dual-mode: device-plugin L40S pool present
	byName := map[string]database.GPUResourceSpec{}
	for _, s := range specs {
		byName[s.Name] = s
	}

	// Shares surface (dra-share node, 8 devices x 4 shares).
	shares, ok := byName["NVIDIA A100 SXM4 40GB Shares"]
	if !ok {
		t.Fatalf("missing A100 Shares spec; got %+v", specs)
	}
	if shares.Type != "a100-sxm4-40gb-share" || shares.Count != 32 || shares.Isolation != "shared" {
		t.Errorf("Shares: type=%q count=%d iso=%q, want a100-sxm4-40gb-share/32/shared",
			shares.Type, shares.Count, shares.Isolation)
	}

	// MIG profile surfaces: the full A100-40GB table. Count = published
	// partitions (1 booking unit = 1 whole partition; partition shares NOT
	// multiplied — decision 2026-11-24). Per-partition equivalents are
	// memory fractions of the board (40192Mi raw, inferred from the largest
	// profile — "40gb" display names are rounded). DeviceMemoryGB = raw
	// partition memory GiB (int-truncated: 4864Mi -> 4).
	migWant := map[string]struct {
		count int
		equiv float64
		memGB int
	}{
		"NVIDIA A100 SXM4 40GB MIG 1g.5gb":    {56, 0.121, 4},
		"NVIDIA A100 SXM4 40GB MIG 1g.5gb+me": {56, 0.121, 4},
		"NVIDIA A100 SXM4 40GB MIG 1g.10gb":   {32, 0.245, 9},
		"NVIDIA A100 SXM4 40GB MIG 2g.10gb":   {24, 0.245, 9},
		"NVIDIA A100 SXM4 40GB MIG 3g.20gb":   {16, 0.497, 19},
		"NVIDIA A100 SXM4 40GB MIG 4g.20gb":   {8, 0.497, 19},
		"NVIDIA A100 SXM4 40GB MIG 7g.40gb":   {8, 1, 39},
	}
	for name, want := range migWant {
		s, ok := byName[name]
		if !ok {
			t.Errorf("missing MIG spec %q", name)
			continue
		}
		if s.Isolation != "mig" {
			t.Errorf("%s Isolation = %q, want mig", name, s.Isolation)
		}
		if s.Count != want.count {
			t.Errorf("%s Count = %d, want %d", name, s.Count, want.count)
		}
		if s.GPUEquivalent != want.equiv {
			t.Errorf("%s GPUEquivalent = %f, want %f", name, s.GPUEquivalent, want.equiv)
		}
		if s.DeviceMemoryGB != want.memGB {
			t.Errorf("%s DeviceMemoryGB = %d, want %d", name, s.DeviceMemoryGB, want.memGB)
		}
		if s.MaxSharesPerDevice != 0 {
			t.Errorf("%s MaxSharesPerDevice = %d, want 0 (partition shares not multiplied)", name, s.MaxSharesPerDevice)
		}
	}
}

// draPartitionableSliceFixture mirrors the fork driver's KEP 4815 split-slice
// announcement (k8s 1.35+, verified on the H200 test bed 2026-11-25): per-node
// device slices; the flex node publishes full-GPU parents (type=gpu,
// consuming their board's entire CounterSet — that is what makes whole-vs-
// partitions mutually exclusive) alongside abstract MIG partitions (type=mig,
// one per profile × placement).
const draPartitionableSliceFixture = `{
  "items": [
    {
      "spec": {
        "driver": "gpu.nvidia.com",
        "nodeName": "dedicated-1",
        "pool": { "name": "dedicated-1", "generation": 3, "resourceSliceCount": 1 },
        "devices": [
          {
            "name": "gpu-0",
            "attributes": {
              "productName": { "string": "NVIDIA H200" },
              "type": { "string": "gpu" },
              "uuid": { "string": "GPU-d0" }
            },
            "capacity": { "memory": { "value": "140Gi" } }
          }
        ]
      }
    },
    {
      "spec": {
        "driver": "gpu.nvidia.com",
        "nodeName": "flex-1",
        "pool": { "name": "flex-1", "generation": 3, "resourceSliceCount": 1 },
        "devices": [
          {
            "name": "gpu-0",
            "attributes": {
              "productName": { "string": "NVIDIA H200" },
              "type": { "string": "gpu" },
              "uuid": { "string": "GPU-f0" }
            },
            "capacity": { "memory": { "value": "140Gi" } },
            "consumesCounters": [
              {
                "counterSet": "gpu-0-counter-set",
                "counters": { "memory": { "value": "140Gi" } }
              }
            ]
          },
          {
            "name": "gpu-0-mig-1g18gb-19-0",
            "attributes": {
              "productName": { "string": "NVIDIA H200" },
              "type": { "string": "mig" },
              "uuid": { "string": "GPU-f0-mig1" },
              "profile": { "string": "1g.18gb" }
            },
            "capacity": { "memory": { "value": "16Gi" } },
            "consumesCounters": [
              {
                "counterSet": "gpu-0-counter-set",
                "counters": { "memory": { "value": "16Gi" } }
              }
            ]
          },
          {
            "name": "gpu-0-mig-1g18gbme-20-0",
            "attributes": {
              "productName": { "string": "NVIDIA H200" },
              "type": { "string": "mig" },
              "uuid": { "string": "GPU-f0-migme" },
              "profile": { "string": "1g.18gb+me" }
            },
            "capacity": { "memory": { "value": "16Gi" } },
            "consumesCounters": [
              {
                "counterSet": "gpu-0-counter-set",
                "counters": { "memory": { "value": "16Gi" } }
              }
            ]
          }
        ]
      }
    }
  ]
}`

// The flex parent is a whole-GPU device that consumes its board's full
// CounterSet — it must land in the dedicated whole lane (not be dropped as a
// profile-less partition), and the +me partition must be menu-filtered
// (decision 2026-11-25). Whole lane = dedicated + flex parents.
func TestParseDRASlices_FlexWholeParentsAndMenuFilter(t *testing.T) {
	specs := parseDRASlices([]byte(draPartitionableSliceFixture), true)

	var whole *database.GPUResourceSpec
	mig := map[string]int{}
	for i := range specs {
		s := &specs[i]
		if strings.Contains(s.Name, "+me") {
			t.Errorf("+me lane must be menu-filtered, got %q", s.Name)
		}
		switch s.Isolation {
		case database.IsolationDedicated:
			if whole != nil {
				t.Fatalf("multiple whole lanes: %q and %q", whole.Name, s.Name)
			}
			whole = s
		case database.IsolationMIG:
			mig[s.Name] = s.Count
		}
	}

	if whole == nil {
		t.Fatal("no whole lane: the flex parent was misclassified")
	}
	if whole.Count != 2 {
		t.Errorf("whole lane count = %d, want 2 (1 dedicated + 1 flex parent)", whole.Count)
	}
	if got, ok := mig["NVIDIA H200 MIG 1g.18gb"]; !ok || got != 1 {
		t.Errorf("1g.18gb lane count = %d (present %v), want 1", got, ok)
	}
	if len(mig) != 1 {
		t.Errorf("expected exactly 1 MIG lane (+me filtered), got %d: %v", len(mig), mig)
	}
}
