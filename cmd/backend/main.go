package main

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"

	"github.com/eformat/gpu-booking-plugin/pkg/api"
	"github.com/eformat/gpu-booking-plugin/pkg/database"
	"github.com/eformat/gpu-booking-plugin/pkg/kube"
	"github.com/gorilla/mux"
)

func main() {
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
	if v := os.Getenv("KUEUE_BOOKING_DAYS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			kube.KueueBookingDays = n
		}
	}

	// Load GPU config from file (falls back to built-in defaults if not found)
	gpuConfigPath := os.Getenv("GPU_CONFIG_PATH")
	if gpuConfigPath == "" {
		gpuConfigPath = "/app/config/gpu-config.json"
	}
	if _, err := os.Stat(gpuConfigPath); err == nil {
		if err := database.LoadConfigFromFile(gpuConfigPath); err != nil {
			log.Fatalf("Failed to load GPU config from %s: %v", gpuConfigPath, err)
		}
		log.Printf("GPU config loaded from %s (%d resources, totalCPU=%d, totalMemory=%d)",
			gpuConfigPath, len(database.GPUResourceSpecs), database.TotalCPU, database.TotalMemory)
	} else {
		log.Printf("GPU config file not found at %s, using built-in defaults", gpuConfigPath)
	}

	// Init database
	if err := database.Init(dbPath); err != nil {
		log.Fatalf("Failed to initialize database: %v", err)
	}
	defer database.Close()
	log.Printf("database initialized at %s", dbPath)

	// Init Kubernetes client and sync loops
	kube.InitK8sClient()
	kube.InitKueueSync()
	kube.InitReservationSync()

	// Router
	r := mux.NewRouter()

	// API routes with auth middleware
	apiRouter := r.PathPrefix("/api").Subrouter()
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

	// Admin
	apiRouter.HandleFunc("/admin", api.AdminListBookings).Methods("GET")
	apiRouter.HandleFunc("/admin", api.AdminDeleteBooking).Methods("DELETE")
	apiRouter.HandleFunc("/admin/reservations", api.AdminReservationToggleHandler).Methods("POST")
	apiRouter.HandleFunc("/admin/database/export", api.AdminExportDatabase).Methods("GET")
	apiRouter.HandleFunc("/admin/database/import", api.AdminImportDatabase).Methods("POST")

	// Health
	apiRouter.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		ns := os.Getenv("POD_NAMESPACE")
		if ns == "" {
			ns = "default"
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

	// Static assets
	r.PathPrefix("/").Handler(http.FileServer(http.Dir(pluginDir)))

	port := os.Getenv("PORT")
	if port == "" {
		port = "9443"
	}

	// TLS support for OpenShift serving certs
	certFile := os.Getenv("TLS_CERT_FILE")
	keyFile := os.Getenv("TLS_KEY_FILE")

	log.Printf("Starting server on :%s", port)

	if certFile != "" && keyFile != "" {
		log.Printf("TLS enabled: cert=%s key=%s", certFile, keyFile)
		if err := http.ListenAndServeTLS(":"+port, certFile, keyFile, r); err != nil {
			log.Fatalf("Server failed: %v", err)
		}
	} else {
		if err := http.ListenAndServe(":"+port, r); err != nil {
			log.Fatalf("Server failed: %v", err)
		}
	}
}
