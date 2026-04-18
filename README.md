# GPU Booking App

A web application for booking and managing shared GPU resources on OpenShift clusters with [Kueue](https://kueue.sigs.k8s.io/) integration. Users reserve GPU time slots through an interactive calendar interface, and the system automatically manages Kueue ClusterQueue quotas to enforce reservation priority via workload preemption.

## Features

- **Interactive calendar** with day/multi-day/hour-range bookings, Ctrl+click multi-select, Shift+click ranges, and right-click context menu
- **GPU resource types** - full H200 GPUs and MIG partitions (3g.71gb, 2g.35gb, 1g.18gb)
- **Kueue integration** - automatically syncs LocalQueue GPU usage as "consumed" bookings; user reservations take priority and trigger workload preemption
- **Reservation system** - creates per-user ClusterQueues with protected `nominalQuota`, ensuring reserved workloads cannot be preempted by unreserved ones
- **Admin dashboard** - sortable/filterable bookings table, per-resource summary tiles, runtime reservation sync toggle, bulk delete
- **OpenShift OAuth** - authentication via OAuth proxy sidecar container

## Architecture

```
+-----------------+     +------------------+     +------------------+
|  Next.js Client | --> |   Go Backend     | --> |   SQLite DB      |
|  (port 3000)    |     |   (port 8080)    |     |   (/data/)       |
+-----------------+     +------------------+     +------------------+
        ^                       |
        |                       v
+------------------+    +------------------+
|  OAuth Proxy     |    |  Kueue API       |
|  (port 4180)     |    |  (LocalQueues)   |
+------------------+    +------------------+
```

All three application containers run in a single pod. The OAuth proxy handles authentication, the Next.js client serves the UI (using server actions to call the backend), and the Go backend manages bookings and Kueue synchronisation.

## Getting Started

### Prerequisites

- Go 1.22+
- Node.js 20+
- SQLite development libraries (`sqlite-devel` or `sqlite-libs`)

### Local Development

```bash
# Start the backend
cd server
ADMIN_PASSWORD=changeme DEV_USER=testuser KUEUE_SYNC_ENABLED=false go run .

# Start the frontend (in another terminal)
cd client
npm install
npm run dev
```

The backend runs on `:8080` and the frontend on `:3000`. Set `DEV_USER` to bypass the OAuth proxy user header for local development.

### Configuration

| Variable | Default | Description |
|----------|---------|-------------|
| `PORT` | `8080` | Server port |
| `DB_PATH` | `./bookings.db` | SQLite database path |
| `ADMIN_PASSWORD` | - | Admin login password (required) |
| `BOOKING_WINDOW_DAYS` | `30` | How far ahead users can book |
| `DEV_USER` | - | Override user identity for local dev |
| `KUEUE_SYNC_ENABLED` | `true` | Enable LocalQueue watcher |
| `KUEUE_SYNC_INTERVAL` | `60` | Kueue poll interval in seconds |
| `KUEUE_BOOKING_DAYS` | `0` | Days to book ahead for consumed slots (0 = rest of week) |

## Deployment

### Build Container Images

```bash
# Build server and client images
make podman-build

# Push to your registry
make podman-push
```

Update the image repositories in `chart/values.yaml` to point to your own container registry.

### Deploy with Helm

```bash
helm install gpu-booking chart/ \
  --set server.adminPassword=changeme \
  --set oauth.cookieSecret=$(openssl rand -base64 32) \
  --set route.host=gpu-booking.apps.example.com
```

#### Key Helm Values

| Value | Default | Description |
|-------|---------|-------------|
| `server.adminPassword` | - | Admin login password (required) |
| `server.bookingWindowDays` | `30` | Booking window in days |
| `server.storage.size` | `1Gi` | PVC size for SQLite |
| `server.storage.storageClassName` | - | Optional storage class |
| `server.kueueSync.enabled` | `false` | Enable Kueue LocalQueue sync |
| `server.kueueSync.interval` | `60` | Kueue sync interval in seconds |
| `server.kueueSync.bookingDays` | `0` | Days ahead for consumed bookings |
| `oauth.cookieSecret` | - | OAuth proxy cookie secret (required) |
| `route.host` | - | OpenShift Route hostname |

### Kueue Reservation System

When Kueue sync is enabled, the app:

1. Polls all LocalQueues for GPU usage and creates "consumed" bookings
2. When users reserve slots, consumed bookings are evicted and per-user ClusterQueues are created with protected quotas
3. Kueue's `reclaimWithinCohort` preemption ensures reserved workloads take priority over unreserved ones

See the [Handling Reservations](CLAUDE.md#handling-reservations) section in CLAUDE.md for the full quota flow and preemption model.

#### Testing Reservations with Helm

```bash
# No reservations (default)
helm template rbac applications/rbac/ -s templates/reservations.yaml

# Single user reservation
EOD=$(date -d "23:59:59" +%s)
helm template rbac applications/rbac/ -s templates/reservations.yaml \
    --set-json='reservations={"h200": {"userA": {"nvidia.com/mig-2g.35gb": 1, "until": '$EOD'}}}'
```

## API Endpoints

| Method | Endpoint | Description |
|--------|----------|-------------|
| `GET` | `/api/config` | Public GPU resource configuration |
| `GET` | `/api/bookings` | List all bookings |
| `POST` | `/api/bookings` | Create a booking |
| `POST` | `/api/bookings/bulk` | Bulk create bookings |
| `DELETE` | `/api/bookings?id=<id>` | Cancel a booking (owner or admin) |
| `POST` | `/api/admin/login` | Admin authentication |
| `GET` | `/api/admin` | Admin bookings + config |
| `DELETE` | `/api/admin?id=<id>` | Admin delete booking |
| `DELETE` | `/api/admin` | Admin delete all bookings |
| `POST` | `/api/admin/reservations` | Toggle reservation sync |

## License

Apache License 2.0
