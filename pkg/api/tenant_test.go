package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/eformat/gpu-booking-plugin/pkg/database"
)


func TestGetBookings_TenantFilter(t *testing.T) {
	setupTestDB(t)
	TenantNames = []string{"tenanta", "tenantb"}
	t.Cleanup(func() { TenantNames = nil })

	db := database.DB()
	today := time.Now().UTC().Format("2006-01-02")

	db.Exec("INSERT INTO bookings ("+database.BookingColumns+") VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
		"b-a", "tenanta-user1", "", "nvidia.com/gpu", 0, today, "full", "now", database.SourceReserved, "", 0, 24, 0, "tenanta")
	db.Exec("INSERT INTO bookings ("+database.BookingColumns+") VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
		"b-b", "tenantb-user1", "", "nvidia.com/gpu", 1, today, "full", "now", database.SourceReserved, "", 0, 24, 0, "tenantb")

	req := httptest.NewRequest(http.MethodGet, "/bookings?tenant=tenanta", nil)
	req = reqWithUser(req, testUser())
	w := httptest.NewRecorder()
	GetBookings(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var resp struct {
		Bookings []database.Booking `json:"bookings"`
	}
	json.NewDecoder(w.Body).Decode(&resp)

	if len(resp.Bookings) != 1 {
		t.Fatalf("got %d bookings, want 1 (only tenanta)", len(resp.Bookings))
	}
	if resp.Bookings[0].Tenant != "tenanta" {
		t.Errorf("booking tenant = %q, want tenanta", resp.Bookings[0].Tenant)
	}
	if resp.Bookings[0].ID != "b-a" {
		t.Errorf("booking id = %q, want b-a", resp.Bookings[0].ID)
	}
}

func TestGetBookings_TenantDefault(t *testing.T) {
	setupTestDB(t)
	TenantNames = []string{"tenanta"}
	t.Cleanup(func() { TenantNames = nil })

	db := database.DB()
	today := time.Now().UTC().Format("2006-01-02")

	db.Exec("INSERT INTO bookings ("+database.BookingColumns+") VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
		"b-a", "tenanta-user1", "", "nvidia.com/gpu", 0, today, "full", "now", database.SourceReserved, "", 0, 24, 0, "tenanta")

	// No ?tenant= param — should default to first configured tenant
	req := httptest.NewRequest(http.MethodGet, "/bookings", nil)
	req = reqWithUser(req, testUser())
	w := httptest.NewRecorder()
	GetBookings(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	var resp struct {
		Bookings []database.Booking `json:"bookings"`
	}
	json.NewDecoder(w.Body).Decode(&resp)
	if len(resp.Bookings) != 1 {
		t.Errorf("got %d bookings, want 1 (default tenant)", len(resp.Bookings))
	}
}

func TestResolveTenant(t *testing.T) {
	TenantNames = []string{"tenanta", "tenantb"}
	t.Cleanup(func() { TenantNames = nil })

	tests := []struct {
		url  string
		want string
	}{
		{"/bookings?tenant=tenanta", "tenanta"},
		{"/bookings?tenant=tenantb", "tenantb"},
		{"/bookings?tenant=unknown", "tenanta"}, // unknown → first configured
		{"/bookings", "tenanta"},                // no param → first configured
	}
	for _, tt := range tests {
		req := httptest.NewRequest(http.MethodGet, tt.url, nil)
		if got := resolveTenant(req); got != tt.want {
			t.Errorf("resolveTenant(%q) = %q, want %q", tt.url, got, tt.want)
		}
	}
}
