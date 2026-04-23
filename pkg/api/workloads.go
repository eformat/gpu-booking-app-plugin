package api

import (
	"log/slog"
	"net/http"

	"github.com/eformat/gpu-booking-plugin/pkg/kube"
)

func PreemptedWorkloadsHandler(w http.ResponseWriter, r *http.Request) {
	workloads, err := kube.ListPreemptedWorkloads()
	if err != nil {
		slog.Error("failed to list preempted workloads", "error", err)
		JsonResponse(w, map[string]any{"workloads": []any{}})
		return
	}
	if workloads == nil {
		workloads = []kube.PreemptedWorkload{}
	}
	JsonResponse(w, map[string]any{"workloads": workloads})
}
