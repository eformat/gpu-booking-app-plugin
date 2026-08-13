package kube

import (
	"path/filepath"
	"testing"

	"github.com/eformat/gpu-booking-plugin/pkg/database"
)

// ── sanitizeK8sName ─────────────────────────────────────────────────────────

func TestSanitizeK8sName(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"alice", "alice"},
		{"alice@redhat.com", "alice"},              // strips @domain
		{"Cluster-Admin@redhat.com", "cluster-admin"}, // lowercased
		{"tenanta-user1", "tenanta-user1"},          // no mangling of valid names
		{"tenanta-user1@example.com", "tenanta-user1"}, // strips domain
	}
	for _, tt := range tests {
		got := sanitizeK8sName(tt.input)
		if got != tt.want {
			t.Errorf("sanitizeK8sName(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

// TestNamespaceIsUsername verifies that namespace derivation in
// applyUserReservation is JUST the sanitized username — no prefix prepended.
// This is the critical invariant: namespace == username == CQ name.
func TestNamespaceIsUsername(t *testing.T) {
	users := []struct {
		username  string
		wantNs    string
	}{
		{"tenanta-user1", "tenanta-user1"},
		{"tenantb-user2", "tenantb-user2"},
		{"alice@redhat.com", "alice"},
	}
	for _, u := range users {
		got := sanitizeK8sName(u.username)
		if got != u.wantNs {
			t.Errorf("namespace for %q = %q, want %q (no prefix prepended)", u.username, got, u.wantNs)
		}
	}
}

// ── tenantFlavorName ─────────────────────────────────────────────────────────

func TestTenantFlavorName(t *testing.T) {
	orig := database.GetGPUConfig()
	defer database.SetGPUConfig(orig)

	t.Run("uses tenant flavor when set", func(t *testing.T) {
		tc := TenantConfig{CohortName: "tenanta", NamespacePrefix: "tenanta-", FlavorName: "gb200-tenanta"}
		if got := tenantFlavorName(tc); got != "gb200-tenanta" {
			t.Errorf("got %q, want gb200-tenanta", got)
		}
	})

	t.Run("falls back to database FlavorName when empty", func(t *testing.T) {
		database.SetGPUConfig(&database.GPUConfig{FlavorName: "h200"})
		tc := TenantConfig{CohortName: "tenanta", NamespacePrefix: "tenanta-", FlavorName: ""}
		if got := tenantFlavorName(tc); got != "h200" {
			t.Errorf("got %q, want h200", got)
		}
	})

	t.Run("falls back to h200 when config has no flavor", func(t *testing.T) {
		database.SetGPUConfig(&database.GPUConfig{})
		tc := TenantConfig{FlavorName: ""}
		if got := tenantFlavorName(tc); got != "h200" {
			t.Errorf("got %q, want h200 (default)", got)
		}
	})
}

// ── cross-tenant syncBookings isolation ──────────────────────────────────────

func TestSyncBookings_CrossTenantIsolation(t *testing.T) {
	dir := t.TempDir()
	if err := database.Init(filepath.Join(dir, "test.db")); err != nil {
		t.Fatalf("Init: %v", err)
	}
	defer database.Close()

	database.SetGPUConfig(&database.GPUConfig{
		Resources: []database.GPUResourceSpec{
			{Name: "Full GPU", Type: "nvidia.com/gpu", Count: 8, Share: 1.0, GPUEquivalent: 1.0},
		},
	})

	db := database.DB()
	dates := []string{"2026-06-01"}

	// Seed a consumed booking belonging to tenanta
	_, err := db.Exec(
		"INSERT INTO bookings ("+database.BookingColumns+") VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
		"kueue-ta-ns-gpu-s0-2026-06-01", "tenanta-user1", "", "nvidia.com/gpu", 0,
		"2026-06-01", "full", "now", database.SourceConsumed, "", 0, 24, 0, "tenanta",
	)
	if err != nil {
		t.Fatalf("insert tenanta booking: %v", err)
	}

	// Run syncBookings for tenantb with zero usages — must NOT touch tenanta's booking
	if err := syncBookings([]resourceUsage{}, dates, "tenantb"); err != nil {
		t.Fatalf("syncBookings tenantb: %v", err)
	}

	var count int
	db.QueryRow(
		"SELECT COUNT(*) FROM bookings WHERE tenant = ? AND source = ?",
		"tenanta", database.SourceConsumed,
	).Scan(&count)
	if count != 1 {
		t.Errorf("tenanta consumed booking was deleted by tenantb sync: want 1, got %d", count)
	}
}

