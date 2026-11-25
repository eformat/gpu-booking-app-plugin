package kube

import (
	"fmt"
	"testing"
)

func TestMigDeviceClassName(t *testing.T) {
	cases := map[string]string{
		"7g.141gb": "mig.nvidia.com-7g-141gb",
		"1g.18gb":  "mig.nvidia.com-1g-18gb",
		"1g.35gb":  "mig.nvidia.com-1g-35gb",
		"2g.35gb":  "mig.nvidia.com-2g-35gb",
		"3g.71gb":  "mig.nvidia.com-3g-71gb",
		"4g.71gb":  "mig.nvidia.com-4g-71gb",
	}
	for profile, want := range cases {
		if got := migDeviceClassName(profile); got != want {
			t.Errorf("migDeviceClassName(%q) = %q, want %q", profile, got, want)
		}
	}
}

func TestDeviceClassManifest(t *testing.T) {
	const profile = "7g.141gb"
	m := deviceClassManifest(migDeviceClassName(profile), profile)

	meta, _ := m["metadata"].(map[string]any)
	if meta["name"] != "mig.nvidia.com-7g-141gb" {
		t.Errorf("manifest name = %v", meta["name"])
	}
	labels, _ := meta["labels"].(map[string]any)
	if labels[deviceClassManagedLabel] != deviceClassManagedValue {
		t.Errorf("managed label missing: %v", labels)
	}
	spec, _ := m["spec"].(map[string]any)
	sels, _ := spec["selectors"].([]any)
	if len(sels) != 1 {
		t.Fatalf("selectors = %v", sels)
	}
	cel, _ := sels[0].(map[string]any)["cel"].(map[string]any)
	expr, _ := cel["expression"].(string)
	want := fmt.Sprintf("device.attributes['gpu.nvidia.com'].profile == '%s'", profile)
	if !contains(expr, want) {
		t.Errorf("CEL expression %q does not select profile %q", expr, profile)
	}
	if !contains(expr, "type == 'mig'") {
		t.Errorf("CEL expression %q must narrow to MIG devices", expr)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}
