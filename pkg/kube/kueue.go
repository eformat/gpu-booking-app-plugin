package kube

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/eformat/gpu-booking-plugin/pkg/database"
)

// Kubernetes API types for LocalQueue and Namespace

type k8sLocalQueueList struct {
	Items []k8sLocalQueue `json:"items"`
}

type k8sLocalQueue struct {
	Metadata k8sMetadata         `json:"metadata"`
	Status   k8sLocalQueueStatus `json:"status"`
}

type k8sMetadata struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
}

type k8sLocalQueueStatus struct {
	ReservingWorkloads int                   `json:"reservingWorkloads"`
	AdmittedWorkloads  int                   `json:"admittedWorkloads"`
	FlavorUsage        []k8sFlavorUsageEntry `json:"flavorUsage"`
}

type k8sFlavorUsageEntry struct {
	Name      string             `json:"name"`
	Resources []k8sResourceUsage `json:"resources"`
}

type k8sResourceUsage struct {
	Name  string `json:"name"`
	Total string `json:"total"`
}

type k8sNamespace struct {
	Metadata k8sNamespaceMetadata `json:"metadata"`
}

type k8sNamespaceMetadata struct {
	Name   string            `json:"name"`
	Labels map[string]string `json:"labels"`
}

type resourceUsage struct {
	Namespace string
	User      string
	Resource  string
	Count     int
}

var (
	KueueSyncEnabled  bool
	KueueSyncInterval int
	KueueBookingDays  int
	k8sHost           string
	k8sToken          string
	k8sHTTPClient     *http.Client
)

func InitK8sClient() {
	if initK8sInCluster() {
		return
	}
	if initK8sFromKubeconfig() {
		return
	}
	log.Println("k8s client: no cluster access available")
}

func initK8sInCluster() bool {
	host := os.Getenv("KUBERNETES_SERVICE_HOST")
	port := os.Getenv("KUBERNETES_SERVICE_PORT")
	if host == "" || port == "" {
		return false
	}
	k8sHost = fmt.Sprintf("https://%s:%s", host, port)

	tokenBytes, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/token")
	if err != nil {
		k8sHost = ""
		return false
	}
	k8sToken = strings.TrimSpace(string(tokenBytes))

	k8sHTTPClient = &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{},
		},
	}

	caCert, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/ca.crt")
	if err == nil {
		pool, _ := x509.SystemCertPool()
		if pool == nil {
			pool = x509.NewCertPool()
		}
		pool.AppendCertsFromPEM(caCert)
		k8sHTTPClient.Transport.(*http.Transport).TLSClientConfig.RootCAs = pool
	}

	log.Printf("k8s client: using in-cluster config (%s)", k8sHost)
	return true
}

func initK8sFromKubeconfig() bool {
	out, err := exec.Command("kubectl", "config", "view", "--minify", "--flatten", "--raw", "-o", "json").Output()
	if err != nil {
		out, err = exec.Command("oc", "config", "view", "--minify", "--flatten", "--raw", "-o", "json").Output()
		if err != nil {
			return false
		}
	}

	var kc struct {
		Clusters []struct {
			Cluster struct {
				Server                   string `json:"server"`
				CertificateAuthorityData string `json:"certificate-authority-data"`
			} `json:"cluster"`
		} `json:"clusters"`
		Users []struct {
			User struct {
				Token string `json:"token"`
			} `json:"user"`
		} `json:"users"`
	}
	if err := json.Unmarshal(out, &kc); err != nil {
		log.Printf("k8s client: failed to parse kubeconfig: %v", err)
		return false
	}

	if len(kc.Clusters) == 0 || kc.Clusters[0].Cluster.Server == "" {
		return false
	}
	if len(kc.Users) == 0 || kc.Users[0].User.Token == "" {
		log.Println("k8s client: kubeconfig has no token (client cert auth not supported)")
		return false
	}

	k8sHost = kc.Clusters[0].Cluster.Server
	k8sToken = kc.Users[0].User.Token

	tlsConfig := &tls.Config{}
	if caData := kc.Clusters[0].Cluster.CertificateAuthorityData; caData != "" {
		caCert, err := base64.StdEncoding.DecodeString(caData)
		if err == nil {
			pool, _ := x509.SystemCertPool()
			if pool == nil {
				pool = x509.NewCertPool()
			}
			pool.AppendCertsFromPEM(caCert)
			tlsConfig.RootCAs = pool
		}
	}

	k8sHTTPClient = &http.Client{
		Timeout:   30 * time.Second,
		Transport: &http.Transport{TLSClientConfig: tlsConfig},
	}

	log.Printf("k8s client: using kubeconfig (%s)", k8sHost)
	return true
}

func InitKueueSync() {
	if !KueueSyncEnabled {
		log.Println("kueue sync disabled")
		return
	}
	if k8sHost == "" || k8sToken == "" {
		log.Println("kueue sync: no k8s client available, disabling")
		return
	}

	log.Printf("kueue sync: enabled, interval=%ds, bookingDays=%d (0=rest of week)", KueueSyncInterval, KueueBookingDays)
	go kueueSyncLoop()
}

func kueueSyncLoop() {
	time.Sleep(5 * time.Second)
	for {
		if err := kueueSync(); err != nil {
			log.Printf("kueue sync error: %v", err)
		}
		time.Sleep(time.Duration(KueueSyncInterval) * time.Second)
	}
}

func kueueSync() error {
	queues, err := listLocalQueues()
	if err != nil {
		return fmt.Errorf("listing local queues: %w", err)
	}

	type nsResKey struct{ ns, resource string }
	aggregated := map[nsResKey]int{}
	nsCache := map[string]string{}

	for _, q := range queues.Items {
		if q.Status.ReservingWorkloads == 0 && q.Status.AdmittedWorkloads == 0 {
			continue
		}

		for _, flavor := range q.Status.FlavorUsage {
			for _, res := range flavor.Resources {
				count := parseResourceCount(res.Total)
				if count <= 0 || !database.IsGPUResource(res.Name) {
					continue
				}

				key := nsResKey{q.Metadata.Namespace, res.Name}
				aggregated[key] += count

				if _, ok := nsCache[q.Metadata.Namespace]; !ok {
					user, err := getNamespaceRequester(q.Metadata.Namespace)
					if err != nil {
						log.Printf("kueue sync: cannot get requester for namespace %s: %v", q.Metadata.Namespace, err)
						user = q.Metadata.Namespace
					}
					nsCache[q.Metadata.Namespace] = user
				}
			}
		}
	}

	usages := []resourceUsage{}
	for key, count := range aggregated {
		usages = append(usages, resourceUsage{
			Namespace: key.ns,
			User:      nsCache[key.ns],
			Resource:  key.resource,
			Count:     count,
		})
	}

	dates := getBookingDates()
	return syncBookings(usages, dates)
}

func listLocalQueues() (*k8sLocalQueueList, error) {
	body, err := K8sGet("/apis/kueue.x-k8s.io/v1beta1/localqueues")
	if err != nil {
		return nil, err
	}
	var list k8sLocalQueueList
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, fmt.Errorf("parsing local queue list: %w", err)
	}
	return &list, nil
}

func getNamespaceRequester(ns string) (string, error) {
	body, err := K8sGet("/api/v1/namespaces/" + ns)
	if err != nil {
		return "", err
	}
	var namespace k8sNamespace
	if err := json.Unmarshal(body, &namespace); err != nil {
		return "", fmt.Errorf("parsing namespace: %w", err)
	}
	owner := namespace.Metadata.Labels["rhai-tmm.dev/owner"]
	if owner == "" {
		return ns, nil
	}
	return owner, nil
}

func K8sGet(path string) ([]byte, error) {
	req, err := http.NewRequest("GET", k8sHost+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+k8sToken)
	req.Header.Set("Accept", "application/json")

	resp, err := k8sHTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("k8s API request %s: %w", path, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading k8s API response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("k8s API %s returned %d: %s", path, resp.StatusCode, string(body))
	}

	return body, nil
}

func parseResourceCount(total string) int {
	total = strings.TrimSpace(total)
	if total == "" || total == "0" {
		return 0
	}
	var count int
	if _, err := fmt.Sscanf(total, "%d", &count); err == nil {
		if fmt.Sprintf("%d", count) == total {
			return count
		}
	}
	return 0
}


func getBookingDates() []string {
	today := time.Now()
	var days int
	if KueueBookingDays > 0 {
		days = KueueBookingDays
	} else {
		weekday := int(today.Weekday())
		if weekday == 0 {
			days = 7
		} else {
			days = 7 - weekday
		}
	}

	dates := []string{}
	for i := 0; i <= days; i++ {
		d := today.AddDate(0, 0, i)
		dates = append(dates, d.Format("2006-01-02"))
	}
	return dates
}

func syncBookings(usages []resourceUsage, dates []string) error {
	db := database.DB()

	type bookingKey struct {
		resource  string
		slotIndex int
		date      string
		slotType  string
	}
	desired := map[string]bookingKey{}
	desiredMeta := map[string]string{}

	type resGroup struct {
		usages []resourceUsage
	}
	byResource := map[string]*resGroup{}
	for _, u := range usages {
		g, ok := byResource[u.Resource]
		if !ok {
			g = &resGroup{}
			byResource[u.Resource] = g
		}
		g.usages = append(g.usages, u)
	}

	for _, group := range byResource {
		slotOffset := 0
		for _, u := range group.usages {
			for i := 0; i < u.Count; i++ {
				slotIdx := slotOffset + i
				for _, date := range dates {
					id := kueueBookingID(u.Namespace, u.Resource, slotIdx, date)
					desired[id] = bookingKey{
						resource:  u.Resource,
						slotIndex: slotIdx,
						date:      date,
						slotType:  database.SlotTypeFull,
					}
					desiredMeta[id] = u.User
				}
			}
			slotOffset += u.Count
		}
	}

	rows, err := db.Query("SELECT id, resource, slot_index, date, slot_type FROM bookings WHERE source = ?", database.SourceConsumed)
	if err != nil {
		return fmt.Errorf("querying kueue bookings: %w", err)
	}
	defer rows.Close()

	existing := map[string]bool{}
	toRemove := []string{}

	today := time.Now().Format("2006-01-02")
	for rows.Next() {
		var id, resource, date, slotType string
		var slotIndex int
		if err := rows.Scan(&id, &resource, &slotIndex, &date, &slotType); err != nil {
			continue
		}
		existing[id] = true
		if _, want := desired[id]; !want && date >= today {
			toRemove = append(toRemove, id)
		}
	}
	if err := rows.Err(); err != nil {
		log.Printf("kueue sync: error iterating existing bookings: %v", err)
	}

	for _, id := range toRemove {
		db.Exec("DELETE FROM bookings WHERE id = ? AND source = ?", id, database.SourceConsumed)
	}
	if len(toRemove) > 0 {
		log.Printf("kueue sync: removed %d stale bookings", len(toRemove))
	}

	added := 0
	skipped := 0
	for id, key := range desired {
		if existing[id] {
			continue
		}

		user := desiredMeta[id]
		createdAt := time.Now().UTC().Format(time.RFC3339)

		var count int
		err := db.QueryRow(
			"SELECT COUNT(*) FROM bookings WHERE resource = ? AND slot_index = ? AND date = ? AND slot_type IN (?, ?) AND source = ?",
			key.resource, key.slotIndex, key.date, database.SlotTypeFull, key.slotType, database.SourceReserved,
		).Scan(&count)
		if err != nil {
			log.Printf("kueue sync: conflict check failed for %s: %v", id, err)
			continue
		}
		if count > 0 {
			skipped++
			continue
		}

		_, err = db.Exec(
			"INSERT OR IGNORE INTO bookings (id, user, email, resource, slot_index, date, slot_type, created_at, source, description, start_hour, end_hour) VALUES (?, ?, '', ?, ?, ?, ?, ?, ?, '', 0, 24)",
			id, user, key.resource, key.slotIndex, key.date, key.slotType, createdAt, database.SourceConsumed,
		)
		if err != nil {
			log.Printf("kueue sync: failed to insert booking %s: %v", id, err)
			continue
		}
		added++
	}

	if added > 0 || len(toRemove) > 0 {
		log.Printf("kueue sync: added=%d, removed=%d, skipped=%d (manual conflict), total_desired=%d",
			added, len(toRemove), skipped, len(desired))
	}

	return nil
}

func kueueBookingID(namespace, resource string, slotIndex int, date string) string {
	short := resource
	switch resource {
	case "nvidia.com/gpu":
		short = "gpu"
	case "nvidia.com/mig-3g.71gb":
		short = "mig3g"
	case "nvidia.com/mig-2g.35gb":
		short = "mig2g"
	case "nvidia.com/mig-1g.18gb":
		short = "mig1g"
	}
	return fmt.Sprintf("kueue-%s-%s-s%d-%s", namespace, short, slotIndex, date)
}
