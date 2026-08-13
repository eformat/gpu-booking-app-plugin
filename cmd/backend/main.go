package main

import (
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/eformat/gpu-booking-plugin/pkg/api"
	"github.com/eformat/gpu-booking-plugin/pkg/database"
	"github.com/eformat/gpu-booking-plugin/pkg/kube"
	"github.com/gorilla/mux"
)

func main() {
	// Structured JSON logging
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))
	slog.SetDefault(logger)

	// Database path
	dbPath := os.Getenv("DB_PATH")
	if dbPath == "" {
		dataDir := os.Getenv("SKILLS_DATA_DIR")
		if dataDir == "" {
			dataDir = "/app/data"
		}
		dbPath = filepath.Join(dataDir, "bookings.db")
	}

	// Booking window
	api.BookingWindowDays = 30
	if bw := os.Getenv("BOOKING_WINDOW_DAYS"); bw != "" {
		if n, err := strconv.Atoi(bw); err == nil && n > 0 {
			api.BookingWindowDays = n
		}
	}

	// Kueue sync config
	kube.KueueSyncEnabled = os.Getenv("KUEUE_SYNC_ENABLED") == "true"
	kube.KueueSyncInterval = 60
	if v := os.Getenv("KUEUE_SYNC_INTERVAL"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			kube.KueueSyncInterval = n
		}
	}
	kube.KueueBookingDays = 0
	// Accept both KUEUE_BOOKING_DAYS (canonical) and KUEUE_SYNC_DAYS (chart alias, fixes prior mismatch)
	for _, envKey := range []string{"KUEUE_BOOKING_DAYS", "KUEUE_SYNC_DAYS"} {
		if v := os.Getenv(envKey); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n >= 0 {
				kube.KueueBookingDays = n
				break
			}
		}
	}

	// Tenant configuration — KUEUE_TENANTS takes precedence over legacy single-tenant vars.
	// Format: comma-separated "cohortName:namespacePrefix" pairs.
	// Example: "unreserved:user-,tenanta:tenanta-user-,tenantb:tenantb-user-"
	kube.Tenants = parseTenants(
		os.Getenv("KUEUE_TENANTS"),
		os.Getenv("KUEUE_COHORT_NAME"),
		os.Getenv("KUEUE_NAMESPACE_PREFIX"),
	)
	for _, t := range kube.Tenants {
		api.TenantNames = append(api.TenantNames, t.CohortName)
	}
	slog.Info("tenant config", "tenants", len(kube.Tenants))

	// GPU discovery config
	kube.DiscoveryEnabled = os.Getenv("GPU_DISCOVERY_ENABLED") == "true"
	kube.DiscoveryInterval = 600
	if v := os.Getenv("GPU_DISCOVERY_INTERVAL"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			kube.DiscoveryInterval = n
		}
	}

	// Dev mode (anonymous admin access when outside cluster)
	api.DevMode = os.Getenv("DEV_MODE") == "true"
	if api.DevMode {
		slog.Warn("DEV_MODE enabled — anonymous admin access is active, do NOT use in production")
	}

	// Load GPU config from file (falls back to built-in defaults if not found)
	gpuConfigPath := os.Getenv("GPU_CONFIG_PATH")
	if gpuConfigPath == "" {
		gpuConfigPath = "/app/config/gpu-config.json"
	}
	if _, err := os.Stat(gpuConfigPath); err == nil {
		if err := database.LoadConfigFromFile(gpuConfigPath); err != nil {
			slog.Error("failed to load GPU config", "path", gpuConfigPath, "error", err)
			os.Exit(1)
		}
		cfg := database.GetGPUConfig()
		slog.Info("GPU config loaded", "path", gpuConfigPath, "resources", len(cfg.Resources), "totalCPU", cfg.TotalCPU, "totalMemory", cfg.TotalMemory)
	} else {
		slog.Info("GPU config file not found, using built-in defaults", "path", gpuConfigPath)
	}

	// Init database
	if err := database.Init(dbPath); err != nil {
		slog.Error("failed to initialize database", "path", dbPath, "error", err)
		os.Exit(1)
	}
	defer database.Close()
	slog.Info("database initialized", "path", dbPath)

	// Init Kubernetes client, GPU discovery, and sync loops
	kube.InitK8sClient()
	kube.InitDiscovery()
	kube.InitKueueSync()
	kube.InitReservationSync()

	// Router
	r := mux.NewRouter()

	// API routes with rate limiting + auth middleware
	apiRouter := r.PathPrefix("/api").Subrouter()
	apiRouter.Use(api.RateLimitMiddleware)
	apiRouter.Use(api.AuthMiddleware)

	// Auth
	apiRouter.HandleFunc("/auth/me", api.MeHandler).Methods("GET")

	// Config
	apiRouter.HandleFunc("/config", api.ConfigHandler).Methods("GET")

	// Bookings
	apiRouter.HandleFunc("/bookings", api.GetBookings).Methods("GET")
	apiRouter.HandleFunc("/bookings", api.CreateBooking).Methods("POST")
	apiRouter.HandleFunc("/bookings", api.DeleteBooking).Methods("DELETE")
	apiRouter.HandleFunc("/bookings/bulk", api.BulkBookingHandler).Methods("POST")
	apiRouter.HandleFunc("/bookings/bulk/cancel", api.BulkCancelHandler).Methods("DELETE")

	// Workloads
	apiRouter.HandleFunc("/workloads/preempted", api.PreemptedWorkloadsHandler).Methods("GET")

	// Admin
	apiRouter.HandleFunc("/admin", api.AdminListBookings).Methods("GET")
	apiRouter.HandleFunc("/admin", api.AdminDeleteBooking).Methods("DELETE")
	apiRouter.HandleFunc("/admin/reservations", api.AdminReservationToggleHandler).Methods("POST")
	apiRouter.HandleFunc("/admin/discover", api.AdminDiscoverGPUHandler).Methods("POST")
	apiRouter.HandleFunc("/admin/database/export", api.AdminExportDatabase).Methods("GET")
	apiRouter.HandleFunc("/admin/database/import", api.AdminImportDatabase).Methods("POST")

	// Health (with DB liveness check)
	apiRouter.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		ns := os.Getenv("POD_NAMESPACE")
		if ns == "" {
			ns = "default"
		}
		if err := database.DB().PingContext(r.Context()); err != nil {
			slog.Error("health check: database ping failed", "error", err)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprintf(w, `{"status":"error","error":"database_unavailable","namespace":"%s"}`, ns)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"status":"ok","namespace":"%s"}`, ns)
	}).Methods("GET")

	// Serve console plugin static files
	pluginDir := os.Getenv("PLUGIN_DIST_DIR")
	if pluginDir == "" {
		pluginDir = "dist"
	}

	// Plugin manifest for console discovery
	r.HandleFunc("/plugin-manifest.json", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, filepath.Join(pluginDir, "plugin-manifest.json"))
	})

	// Static assets (must be last — catch-all)
	r.PathPrefix("/").Handler(http.FileServer(http.Dir(pluginDir)))

	port := os.Getenv("PORT")
	if port == "" {
		port = "9443"
	}

	// TLS support for OpenShift serving certs
	certFile := os.Getenv("TLS_CERT_FILE")
	keyFile := os.Getenv("TLS_KEY_FILE")

	slog.Info("starting server", "port", port)

	if certFile != "" && keyFile != "" {
		slog.Info("TLS enabled", "cert", certFile, "key", keyFile)
		if err := http.ListenAndServeTLS(":"+port, certFile, keyFile, r); err != nil {
			slog.Error("server failed", "error", err)
			os.Exit(1)
		}
	} else if api.DevMode {
		slog.Warn("TLS disabled — running plain HTTP (dev mode only)")
		if err := http.ListenAndServe(":"+port, r); err != nil {
			slog.Error("server failed", "error", err)
			os.Exit(1)
		}
	} else {
		slog.Error("TLS_CERT_FILE and TLS_KEY_FILE are required in production (set DEV_MODE=true to bypass)")
		os.Exit(1)
	}
}

// parseTenants builds the Tenants slice from env vars.
// KUEUE_TENANTS (e.g. "unreserved:user-,tenanta:tenanta-user-") takes precedence.
// Falls back to KUEUE_COHORT_NAME / KUEUE_NAMESPACE_PREFIX for single-tenant compat.
// Default when nothing is set: one tenant {CohortName:"unreserved", NamespacePrefix:"user-"}.
func parseTenants(tenantsEnv, cohortEnv, prefixEnv string) []kube.TenantConfig {
	if tenantsEnv != "" {
		var tenants []kube.TenantConfig
		for _, pair := range strings.Split(tenantsEnv, ",") {
			pair = strings.TrimSpace(pair)
			if pair == "" {
				continue
			}
			parts := strings.SplitN(pair, ":", 3)
			if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
				slog.Warn("KUEUE_TENANTS: skipping malformed entry", "entry", pair)
				continue
			}
			flavor := ""
			if len(parts) == 3 {
				flavor = parts[2]
			}
			tenants = append(tenants, kube.TenantConfig{CohortName: parts[0], NamespacePrefix: parts[1], FlavorName: flavor})
		}
		if len(tenants) > 0 {
			return tenants
		}
	}
	// Single-tenant fallback
	cohort := "unreserved"
	if cohortEnv != "" {
		cohort = cohortEnv
	}
	prefix := "user-"
	if prefixEnv != "" {
		prefix = prefixEnv
	}
	return []kube.TenantConfig{{CohortName: cohort, NamespacePrefix: prefix}}
}
