package api

import (
	"net/http"

	"github.com/eformat/gpu-booking-plugin/pkg/database"
)

var BookingWindowDays = 30

func ConfigHandler(w http.ResponseWriter, r *http.Request) {
	JsonResponse(w, database.GetConfig(BookingWindowDays))
}
