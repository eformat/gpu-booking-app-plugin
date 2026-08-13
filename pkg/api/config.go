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
	JsonResponse(w, database.GetConfig(BookingWindowDays, TenantNames))
}
