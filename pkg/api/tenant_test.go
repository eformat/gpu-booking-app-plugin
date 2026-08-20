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
	TenantNames = []string{"t1", "t2", "t99"}
	t.Cleanup(func() { TenantNames = nil })

	tests := []struct {
		url      string
		username string
		want     string
	}{
		// Unprefixed user (cluster-admin) can specify any tenant
		{"/bookings?tenant=t1", "admin", "t1"},
		{"/bookings?tenant=t99", "admin", "t99"},
		{"/bookings?tenant=unknown", "admin", "t1"}, // unknown → first configured
		{"/bookings", "admin", "t1"},               // no param → first configured

		// Tenant-prefixed users are locked to their tenant regardless of ?tenant=
		{"/bookings?tenant=t1", "t99-admin", "t99"},  // t1 requested but locked to t99
		{"/bookings?tenant=t2", "t99-user1", "t99"},  // locked to t99
		{"/bookings", "t1-user1", "t1"},              // derives t1 from username
		{"/bookings?tenant=t99", "t2-user2", "t2"},   // locked to t2
	}
	for _, tt := range tests {
		req := httptest.NewRequest(http.MethodGet, tt.url, nil)
		req = reqWithUser(req, &UserInfo{Username: tt.username, IsAdmin: true})
		if got := resolveTenant(req); got != tt.want {
			t.Errorf("resolveTenant(%q, user=%q) = %q, want %q", tt.url, tt.username, got, tt.want)
		}
	}
}

func TestTenantFromUsername(t *testing.T) {
	TenantNames = []string{"t1", "t99"}
	t.Cleanup(func() { TenantNames = nil })

	tests := []struct{ username, want string }{
		{"t1-user1", "t1"},
		{"t1-user2", "t1"},
		{"t1-admin", "t1"},
		{"t99-admin", "t99"},
		{"t99-user1", "t99"},
		{"admin", ""},          // no prefix
		{"user1", ""},          // no matching tenant prefix
		{"t50-user1", ""},      // t50 not in TenantNames
	}
	for _, tt := range tests {
		if got := tenantFromUsername(tt.username); got != tt.want {
			t.Errorf("tenantFromUsername(%q) = %q, want %q", tt.username, got, tt.want)
		}
	}
}
