package kube

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
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
	FlavorsReservation []k8sFlavorUsageEntry `json:"flavorsReservation"`
	FlavorsUsage       []k8sFlavorUsageEntry `json:"flavorsUsage"`
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
	Name        string            `json:"name"`
	Labels      map[string]string `json:"labels"`
	Annotations map[string]string `json:"annotations"`
}

type resourceUsage struct {
	Namespace string
	User      string
	Resource  string
	Count     int
}

// nsResKey aggregates usage per namespace and quota resource — the key the
// quota-name view and the claim-attributed view share.
type nsResKey struct{ ns, resource string }

var (
	KueueSyncEnabled  bool
	KueueSyncInterval int
	KueueBookingDays  int
	// DRAQuotaNaming selects the lane-name scheme for DRA catalog entries:
	// "product" (default, ocp5-3 convention) or "profile" (dynamic-lab
	// agnostic scheme — see draLogicalQuotaName).
	DRAQuotaNaming string
	k8sHost        string
	k8sToken       string
	k8sHTTPClient  *http.Client
)

func InitK8sClient() {
	if initK8sInCluster() {
		return
	}
	if initK8sFromKubeconfig() {
		return
	}
	slog.Info("k8s client: no cluster access available")
}

// K8sAvailable reports whether a Kubernetes API client is configured.
// Callers use this to skip live API checks (e.g. namespace verification)
// when running outside a cluster.
func K8sAvailable() bool {
	return k8sHost != "" && k8sToken != ""
}

// K8sResourceExists reports whether a resource at the given API path exists.
// Returns (false, nil) for 404 and (false, err) for other failures.
func K8sResourceExists(path string) (bool, error) {
	if !K8sAvailable() {
		return false, fmt.Errorf("k8s client not available")
	}
	req, err := http.NewRequest("GET", k8sHost+path, nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("Authorization", "Bearer "+k8sToken)
	req.Header.Set("Accept", "application/json")

	resp, err := k8sHTTPClient.Do(req)
	if err != nil {
		return false, fmt.Errorf("k8s API request %s: %w", path, err)
	}
	defer resp.Body.Close()
	io.ReadAll(resp.Body)

	switch {
	case resp.StatusCode == http.StatusOK:
		return true, nil
	case resp.StatusCode == http.StatusNotFound:
		return false, nil
	default:
		return false, fmt.Errorf("k8s API %s returned %d", path, resp.StatusCode)
	}
}

// NamespaceExists checks whether a namespace exists in the cluster.
// Returns an error if the API client is not configured or the request fails.
func NamespaceExists(ns string) (bool, error) {
	return K8sResourceExists("/api/v1/namespaces/" + ns)
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

	slog.Info("k8s client: using in-cluster config", "host", k8sHost)
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
				// Sandbox/lab kubeconfigs commonly use insecure-skip-tls-verify
				// (no CA data) — honor it for the out-of-cluster dev/dry-run
				// path; the in-cluster path is untouched.
				InsecureSkipVerify bool `json:"insecure-skip-tls-verify"`
			} `json:"cluster"`
		} `json:"clusters"`
		Users []struct {
			User struct {
				Token string `json:"token"`
			} `json:"user"`
		} `json:"users"`
	}
	if err := json.Unmarshal(out, &kc); err != nil {
		slog.Error("k8s client: failed to parse kubeconfig", "error", err)
		return false
	}

	if len(kc.Clusters) == 0 || kc.Clusters[0].Cluster.Server == "" {
		return false
	}
	if len(kc.Users) == 0 || kc.Users[0].User.Token == "" {
		slog.Warn("k8s client: kubeconfig has no token (client cert auth not supported)")
		return false
	}

	k8sHost = kc.Clusters[0].Cluster.Server
	k8sToken = kc.Users[0].User.Token

	tlsConfig := &tls.Config{}
	if kc.Clusters[0].Cluster.InsecureSkipVerify {
		tlsConfig.InsecureSkipVerify = true
	}
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

	slog.Info("k8s client: using kubeconfig", "host", k8sHost)
	return true
}

func InitKueueSync() {
	if !KueueSyncEnabled {
		slog.Info("kueue sync disabled")
		return
	}
	if k8sHost == "" || k8sToken == "" {
		slog.Info("kueue sync: no k8s client available, disabling")
		return
	}

	slog.Info("kueue sync: enabled", "interval_seconds", KueueSyncInterval, "booking_days", KueueBookingDays)
	go kueueSyncLoop()
}

func kueueSyncLoop() {
	time.Sleep(5 * time.Second)
	for {
		if err := kueueSync(); err != nil {
			slog.Error("kueue sync error", "error", err)
		}
		time.Sleep(time.Duration(KueueSyncInterval) * time.Second)
	}
}

func kueueSync() error {
	queues, err := listLocalQueues()
	if err != nil {
		return fmt.Errorf("listing local queues: %w", err)
	}

	// The cycle rebuilds the whole picture from live Kueue state.
	resetCoveredUsage()

	aggregated := map[nsResKey]int{}
	nsCache := map[string]string{}

	for _, q := range queues.Items {
		if q.Status.ReservingWorkloads == 0 && q.Status.AdmittedWorkloads == 0 {
			continue
		}

		flavors := q.Status.FlavorsReservation
		if len(flavors) == 0 {
			flavors = q.Status.FlavorsUsage
		}
		if len(flavors) == 0 {
			flavors = q.Status.FlavorUsage
		}

		for _, flavor := range flavors {
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
						slog.Warn("kueue sync: cannot get requester for namespace", "namespace", q.Metadata.Namespace, "error", err)
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

	// Quota names cannot attribute lanes on their own: deviceClassMappings
	// fold whole-GPU shares claims and MIG claims into one quota resource,
	// so every DRA booking charges the same name. The claims name the device
	// each booking actually received, and the slice names its profile —
	// attribute from there, falling back to the quota-name view unless the
	// split conserves the quota totals.
	if split, ok := splitDRAUsageByClaim(aggregated, nsCache); ok {
		usages = split
	}

	dates := getBookingDates()
	return syncBookings(usages, dates)
}

// splitDRAUsageByClaim re-attributes the DRA-charged quota usage onto
// catalog resources by what each allocated claim actually received.
//
// The quota-name view (aggregated) stays the authority on totals: the split
// is only used when it conserves them — every device result an allocated
// claim carries must resolve to a catalog resource, and the result count
// must equal the DRA quota total. Anything else (a slice not yet republished,
// a device the index has never seen, a claim whose class maps to no quota)
// returns not-ok and the caller keeps the quota-name view, which is what the
// app rendered before claims were read.
func splitDRAUsageByClaim(aggregated map[nsResKey]int, nsCache map[string]string) ([]resourceUsage, bool) {
	quotaView := make([]resourceUsage, 0, len(aggregated))
	nonDRA := make([]resourceUsage, 0)
	expected := 0
	for key, count := range aggregated {
		usage := resourceUsage{
			Namespace: key.ns,
			User:      nsCache[key.ns],
			Resource:  key.resource,
			Count:     count,
		}
		quotaView = append(quotaView, usage)
		if spec, ok := database.GPUSpecByType(key.resource); ok && spec.DRAMode {
			expected += count
			continue
		}
		nonDRA = append(nonDRA, usage)
	}
	if expected == 0 {
		return quotaView, false
	}

	claims, err := fetchAllocatedClaims()
	if err != nil {
		slog.Warn("kueue sync: claim attribution unavailable, keeping quota-name view", "error", err)
		return quotaView, false
	}
	sliceBody, err := fetchDRASliceBody()
	if err != nil {
		slog.Warn("kueue sync: claim attribution unavailable, keeping quota-name view", "error", err)
		return quotaView, false
	}
	devices := buildDRADeviceIndex(sliceBody)
	if len(devices) == 0 {
		slog.Warn("kueue sync: no DRA devices indexed, keeping quota-name view")
		return quotaView, false
	}

	requester := func(ns string) string {
		if user, ok := nsCache[ns]; ok && user != "" {
			return user
		}
		user, err := getNamespaceRequester(ns)
		if err != nil {
			slog.Warn("kueue sync: cannot get requester for namespace", "namespace", ns, "error", err)
			return ns
		}
		return user
	}

	// Only namespaces the quota view knows about are attributed: Kueue
	// charges usage per LocalQueue namespace, so a claim allocated outside
	// that scope (an unmanaged namespace, a pod that bypassed Kueue) has no
	// quota unit behind it. Skipping those keeps the attribution scoped
	// exactly like the quota-name view it replaces — and keeps the
	// conservation check meaningful rather than failing on every
	// out-of-band claim.
	managed := map[string]bool{}
	for key := range aggregated {
		managed[key.ns] = true
	}

	attributed, ok := attributeClaims(claims, devices, expected, managed)
	if !ok {
		slog.Warn("kueue sync: claim attribution incomplete, keeping quota-name view",
			"claims", len(claims), "quota", expected)
		return quotaView, false
	}

	usages := nonDRA
	for key, count := range attributed {
		usages = append(usages, resourceUsage{
			Namespace: key.ns,
			User:      requester(key.ns),
			Resource:  key.resource,
			Count:     count,
		})
	}
	return usages, true
}

// attributeClaims maps every allocated device result onto its catalog
// resource, via the slice device's attributes. Claims outside the managed
// namespaces (no quota entry behind them — unmanaged namespaces, pods that
// bypassed Kueue) are skipped, matching the quota-name view's scope. It
// returns not-ok unless the attribution is complete and conserves the quota
// totals: any device the index cannot resolve, any device without a catalog
// identity, and any total mismatch all fail the whole split — a partial
// attribution would book consumption onto the wrong lanes, and the
// quota-name view is the honest answer whenever the claims and the slices
// disagree.
func attributeClaims(claims []k8sResourceClaim, devices map[string]draDevice, expected int, managed map[string]bool) (map[nsResKey]int, bool) {
	attributed := map[nsResKey]int{}
	count := 0
	for _, claim := range claims {
		if !managed[claim.Metadata.Namespace] {
			continue
		}
		for _, result := range claim.Status.Allocation.Devices.Results {
			dev, ok := devices[result.Pool+"\x00"+result.Device]
			if !ok {
				slog.Warn("kueue sync: allocated device not in slice index",
					"claim", claim.Metadata.Namespace+"/"+claim.Metadata.Name,
					"device", result.Device, "pool", result.Pool)
				return nil, false
			}
			resource, ok := catalogResourceForDevice(dev)
			if !ok {
				slog.Warn("kueue sync: allocated device has no catalog identity",
					"claim", claim.Metadata.Namespace+"/"+claim.Metadata.Name,
					"device", result.Device)
				return nil, false
			}
			attributed[nsResKey{claim.Metadata.Namespace, resource}]++
			count++
		}
	}
	if count != expected {
		slog.Warn("kueue sync: claim attribution does not conserve quota totals",
			"claims", count, "quota", expected)
		return nil, false
	}
	return attributed, true
}

// catalogResourceForDevice names the catalog resource a DRA device books
// onto — the same naming parseDRASlices gives the catalog entry, so
// attributed consumption lands on lanes the UI already renders. MIG
// partitions name their profile lane; whole-GPU devices name the shares or
// dedicated lane by whether the device backs multiple claims.
func catalogResourceForDevice(dev draDevice) (string, bool) {
	product := attrStringAny(dev, draProductAttrKeys)
	if product == "" {
		return "", false
	}
	switch attrStringAny(dev, draTypeAttrKeys) {
	case "mig":
		profile := attrStringAny(dev, draProfileAttrKeys)
		if profile == "" {
			return "", false
		}
		return draLogicalQuotaName(product, false, profile), true
	case "", "gpu":
		return draLogicalQuotaName(product, dev.AllowMultipleAllocations, ""), true
	}
	return "", false
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
	// Resolution order:
	//  1. rhai-tmm.dev/owner label (pool convention, where set)
	//  2. openshift.io/requester annotation — the OpenShift namespace
	//     creator (user-admin → admin); without this the reservation
	//     coverage check compares the namespace name against booking users
	//     and every booking-backed workload looks like an uncovered
	//     stranger, double-counting capacity.
	//  3. namespace name with a leading "user-" stripped (user-user1 → user1)
	//  4. the namespace name itself
	metadata := namespace.Metadata
	if owner := metadata.Labels["rhai-tmm.dev/owner"]; owner != "" {
		return owner, nil
	}
	if requester := metadata.Annotations["openshift.io/requester"]; requester != "" {
		return requester, nil
	}
	trimmed := strings.TrimPrefix(ns, "user-")
	return trimmed, nil
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
	today := time.Now().UTC()
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

	for resource, group := range byResource {
		maxSlots := 0
		if spec, ok := database.GPUSpecByType(resource); ok {
			maxSlots = spec.Count
		}

		sort.Slice(group.usages, func(i, j int) bool {
			return group.usages[i].Namespace < group.usages[j].Namespace
		})

		// Build set of slots occupied by reserved bookings (per date) and count per user
		reservedSlots := map[string]map[int]bool{}      // date -> set of slot indices
		reservedSlotUser := map[string]map[int]string{} // date -> slot index -> normalised user
		userReserved := map[string]map[string]int{}     // date -> normalised user -> count of reserved slots
		for _, date := range dates {
			reserved := map[int]bool{}
			slotUser := map[int]string{}
			perUser := map[string]int{}
			rows, err := db.Query(
				"SELECT slot_index, user FROM bookings WHERE resource = ? AND date = ? AND source = ?",
				resource, date, database.SourceReserved,
			)
			if err == nil {
				for rows.Next() {
					var idx int
					var u string
					if rows.Scan(&idx, &u) == nil {
						reserved[idx] = true
						slotUser[idx] = normalizeUser(u)
						perUser[coverageUser(u)]++
					}
				}
				rows.Close()
			}
			reservedSlots[date] = reserved
			reservedSlotUser[date] = slotUser
			userReserved[date] = perUser
		}

		// Assign consumed bookings to first available slot, skipping units already covered by reservations
		consumedSlots := map[string]map[int]bool{}
		for _, date := range dates {
			consumedSlots[date] = map[int]bool{}
		}

		for _, u := range group.usages {
			for i := 0; i < u.Count; i++ {
				normalUser := coverageUser(u.User)

				// Determine which dates are covered vs uncovered by reservations.
				var coveredDates, uncoveredDates []string
				for _, date := range dates {
					if userReserved[date][normalUser] > 0 {
						coveredDates = append(coveredDates, date)
					} else {
						uncoveredDates = append(uncoveredDates, date)
					}
				}

				// Fully covered: reservation accounts for this unit on every date
				if len(uncoveredDates) == 0 {
					for _, date := range coveredDates {
						userReserved[date][normalUser]--
						recordCoveredUsage(normalUser, u.Resource, date)
					}
					continue
				}

				// Find a slot that is not already consumed on any date and not
				// reserved on the uncovered dates (where the consumed booking
				// will actually be written).
				slotIdx := -1
				for candidate := 0; maxSlots == 0 || candidate < maxSlots; candidate++ {
					free := true
					for _, date := range dates {
						if consumedSlots[date][candidate] {
							free = false
							break
						}
					}
					if !free {
						continue
					}
					for _, date := range uncoveredDates {
						if reservedSlots[date][candidate] {
							free = false
							break
						}
					}
					if free {
						slotIdx = candidate
						break
					}
				}
				if slotIdx < 0 {
					slog.Warn("kueue sync: no free slot within resource limit, skipping",
						"resource", u.Resource, "user", u.User, "maxSlots", maxSlots)
					continue
				}

				// Decrement reservation credits on covered dates — the unit is
				// consuming the reservation, recorded so the UI can render the
				// reserved slot as actively consumed (green) while it runs.
				for _, date := range coveredDates {
					userReserved[date][normalUser]--
					recordCoveredUsage(normalUser, u.Resource, date)
				}

				// Mark slot occupied on all dates, create bookings only on uncovered dates
				for _, date := range dates {
					consumedSlots[date][slotIdx] = true
				}
				for _, date := range uncoveredDates {
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
		}
	}

	rows, err := db.Query("SELECT id, resource, slot_index, date, slot_type FROM bookings WHERE source = ?", database.SourceConsumed)
	if err != nil {
		return fmt.Errorf("querying kueue bookings: %w", err)
	}
	defer rows.Close()

	existing := map[string]bool{}
	toRemove := []string{}

	today := time.Now().UTC().Format("2006-01-02")
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
		slog.Error("kueue sync: error iterating existing bookings", "error", err)
	}

	for _, id := range toRemove {
		db.Exec("DELETE FROM bookings WHERE id = ? AND source = ?", id, database.SourceConsumed)
	}
	if len(toRemove) > 0 {
		slog.Info("kueue sync: removed stale bookings", "count", len(toRemove))
	}

	added := 0
	skipped := 0
	for id, key := range desired {
		if existing[id] {
			continue
		}

		user := desiredMeta[id]
		createdAt := time.Now().UTC().Format(time.RFC3339)

		// Check if a reserved booking occupies this slot
		var reservedUser string
		err := db.QueryRow(
			"SELECT user FROM bookings WHERE resource = ? AND slot_index = ? AND date = ? AND slot_type IN (?, ?) AND source = ?",
			key.resource, key.slotIndex, key.date, database.SlotTypeFull, key.slotType, database.SourceReserved,
		).Scan(&reservedUser)
		if err == nil {
			skipped++
			continue
		}

		_, err = db.Exec(
			"INSERT OR IGNORE INTO bookings (id, user, email, resource, slot_index, date, slot_type, created_at, source, description, start_hour, end_hour, utc_offset) VALUES (?, ?, '', ?, ?, ?, ?, ?, ?, '', 0, 24, 0)",
			id, user, key.resource, key.slotIndex, key.date, key.slotType, createdAt, database.SourceConsumed,
		)
		if err != nil {
			slog.Error("kueue sync: failed to insert booking", "bookingId", id, "error", err)
			continue
		}
		added++
	}

	if added > 0 || len(toRemove) > 0 {
		slog.Info("kueue sync: reconciled", "added", added, "removed", len(toRemove), "skipped", skipped, "total_desired", len(desired))
	}

	return nil
}

// PreemptedWorkload represents a Kueue Workload that has been preempted.
type PreemptedWorkload struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	Owner     string `json:"owner"`
	Reason    string `json:"reason"`
	Message   string `json:"message"`
	Timestamp string `json:"timestamp"`
}

// ListPreemptedWorkloads queries the Kueue Workloads API and returns any
// workloads that have a Preempted or Evicted (reason=Preempted) condition.
func ListPreemptedWorkloads() ([]PreemptedWorkload, error) {
	if k8sHost == "" || k8sToken == "" {
		return nil, nil
	}

	body, err := K8sGet("/apis/kueue.x-k8s.io/v1beta1/workloads")
	if err != nil {
		return nil, fmt.Errorf("listing workloads: %w", err)
	}

	var list struct {
		Items []struct {
			Metadata struct {
				Name      string `json:"name"`
				Namespace string `json:"namespace"`
			} `json:"metadata"`
			Spec struct {
				PodSets []struct {
					Template struct {
						Spec struct {
							ServiceAccountName string `json:"serviceAccountName"`
						} `json:"spec"`
					} `json:"template"`
				} `json:"podSets"`
			} `json:"spec"`
			Status struct {
				Conditions []struct {
					Type               string `json:"type"`
					Status             string `json:"status"`
					Reason             string `json:"reason"`
					Message            string `json:"message"`
					LastTransitionTime string `json:"lastTransitionTime"`
				} `json:"conditions"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, fmt.Errorf("parsing workload list: %w", err)
	}

	var result []PreemptedWorkload
	for _, item := range list.Items {
		for _, cond := range item.Status.Conditions {
			matched := false
			switch {
			case cond.Status == "True" && (cond.Type == "Preempted" || (cond.Type == "Evicted" && cond.Reason == "Preempted")):
				matched = true
			case cond.Status == "False" && cond.Type == "QuotaReserved" && cond.Reason == "Pending":
				matched = true
			}
			if !matched {
				continue
			}

			owner := item.Metadata.Namespace
			if user, err := getNamespaceRequester(item.Metadata.Namespace); err == nil && user != "" {
				owner = user
			}

			result = append(result, PreemptedWorkload{
				Name:      item.Metadata.Name,
				Namespace: item.Metadata.Namespace,
				Owner:     owner,
				Reason:    cond.Reason,
				Message:   cond.Message,
				Timestamp: cond.LastTransitionTime,
			})
			break
		}
	}

	return result, nil
}

func normalizeUser(u string) string {
	if i := strings.Index(u, "@"); i >= 0 {
		return u[:i]
	}
	return u
}

// coverageUser is the identity the coverage matcher keys on. System bookings
// store the target namespace (e.g. user-user1) while admitted-workload usage
// is attributed to the namespace's resolved requester (e.g. user1 — see
// getNamespaceRequester's user- strip), so reservation credits keyed on the
// raw string never matched the usage and every date looked uncovered. Trim
// the same prefix on both sides: personal bookings already store the
// requester, which is a fixed point of the trim.
func coverageUser(u string) string {
	return strings.TrimPrefix(normalizeUser(u), "user-")
}

func kueueBookingID(namespace, resource string, slotIndex int, date string) string {
	parts := strings.Split(resource, "/")
	short := parts[len(parts)-1]
	// Strip everything after "." and remove dashes for compact ID
	// e.g. "mig-3g.71gb" -> "mig-3g" -> "mig3g", "gpu" -> "gpu"
	if dotIdx := strings.Index(short, "."); dotIdx >= 0 {
		short = short[:dotIdx]
	}
	short = strings.ReplaceAll(short, "-", "")
	return fmt.Sprintf("kueue-%s-%s-s%d-%s", namespace, short, slotIndex, date)
}

// CoveredUsageEntry is one unit of booked capacity that an admitted workload
// is actively consuming: the reservation stays in the bookings table, and
// this overlay tells the UI to render it as consumed (green) while the
// workload runs.
type CoveredUsageEntry struct {
	User     string `json:"user"`
	Resource string `json:"resource"`
	Date     string `json:"date"`
	Count    int    `json:"count"`
}

type coveredKey struct {
	user     string
	resource string
	date     string
}

var (
	coveredUsageMu   sync.Mutex
	lastCoveredUsage = map[coveredKey]int{}
)

// recordCoveredUsage is called by the sync for every unit whose consumption
// is absorbed by an existing reservation.
func recordCoveredUsage(user, resource, date string) {
	coveredUsageMu.Lock()
	defer coveredUsageMu.Unlock()
	lastCoveredUsage[coveredKey{user: user, resource: resource, date: date}]++
}

// resetCoveredUsage clears the overlay at the start of each sync cycle — the
// cycle rebuilds the full picture from live Kueue state.
func resetCoveredUsage() {
	coveredUsageMu.Lock()
	defer coveredUsageMu.Unlock()
	lastCoveredUsage = map[coveredKey]int{}
}

// CoveredUsageSnapshot returns the active consumption of booked capacity
// from the most recent sync cycle.
func CoveredUsageSnapshot() []CoveredUsageEntry {
	coveredUsageMu.Lock()
	defer coveredUsageMu.Unlock()
	out := make([]CoveredUsageEntry, 0, len(lastCoveredUsage))
	for k, count := range lastCoveredUsage {
		out = append(out, CoveredUsageEntry{User: k.user, Resource: k.resource, Date: k.date, Count: count})
	}
	return out
}
