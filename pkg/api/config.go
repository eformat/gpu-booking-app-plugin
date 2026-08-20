package api

import (
	"net/http"

	"github.com/eformat/gpu-booking-plugin/pkg/database"
)

var (
	BookingWindowDays = 30
	// TenantNames is the ordered list of cohort names for all configured tenants.
	// Set from main.go after parsing KUEUE_TENANTS.
	TenantNames []string
)

func ConfigHandler(w http.ResponseWriter, r *http.Request) {
	user := GetUser(r)

	// Restrict the tenant list to just the user's own tenant so the UI
	// selector only shows (and locks to) their tenant. Unprefixed users
	// (e.g. cluster-admin "admin") see all tenants.
	tenants := TenantNames
	if locked := tenantFromUsername(user.Username); locked != "" {
		tenants = []string{locked}
	}

	JsonResponse(w, database.GetConfig(BookingWindowDays, tenants))
}
