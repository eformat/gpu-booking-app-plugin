# Dynamic Resource Allocation & Kueue Enforcement

How this plugin integrates with Kubernetes [Dynamic Resource Allocation (DRA)](https://kubernetes.io/docs/concepts/scheduling-eviction/dynamic-resource-allocation/) and [Kueue](https://kueue.sigs.k8s.io/) to make GPU capacity **non-advisory**: a booked lane is physically reserved, an unbooked workload cannot start, and a booking always evicts unbooked holders that are in its way.

Scope: this document reflects the DRA-enabled deployment model (the `rhoai-36-dra` workstream, H200 mirror testbed) running on the Red Hat Kueue build shipped with RHOAI 3.6 (operand lineage v0.19; served APIs `kueue.x-k8s.io/v1beta1` + `v1beta2`). Upstream concepts doc: [Kueue — Dynamic Resource Allocation](https://kueue.sigs.k8s.io/docs/concepts/dynamic_resource_allocation/).

---

## 1. DRA in one page

DRA is the Kubernetes API for requesting hardware devices. Three object types matter here:

| Object | Published by | Role |
|---|---|---|
| **ResourceSlice** | the DRA driver (per node) | the device inventory: every allocatable device with attributes and capacity |
| **DeviceClass** | admin / this plugin's discovery sync | a *selector* naming a category of devices (e.g. "any whole board", "a 7g.141gb MIG partition") |
| **ResourceClaim / ResourceClaimTemplate** | workloads | the actual request: *N devices from this class, ExactCount* |

The decisive scheduling fact: **a pod's devices are allocated node-locally, all-or-nothing.** A pod runs on exactly one node, and the DRA scheduler must allocate *all* of the pod's claims from that node's published inventory. Kueue quotas and bookings are *cluster* totals — the gap between the two is what the booker's per-node shape cap closes (see §5).

### Partitionable devices (KEP-4815)

Under dynamic MIG each board publishes:

- a **counter-set device** — the full physical GPU advertising its counters (e.g. `memory: 141Gi`). Not itself a claimable catalog unit.
- **partition devices** — MIG profile instances declaring `consumesCounters` (what they take from the parent counter set) plus a `profile` attribute (e.g. `7g.141gb`).
- the **whole-board device** (`type=gpu`) — carries the full-board `consumesCounters` too, which is exactly what makes *whole board* and *its partitions* mutually exclusive: claiming either consumes the board's counter set.

Counter exclusivity is enforced physically by the driver/scheduler — the booking layer never has to prevent a full board and one of its partitions being allocated to different pods.

### Consumable shares

Devices with `allowMultipleAllocations: true` plus a `capacity.shares.requestPolicy.validRange` back multiple claims (one per share). The booking catalog models a shares lane as N units per board; each claim consumes one share. (Shares lanes and MIG lanes are separate catalog entries; DRA counter exclusivity governs full-vs-partition, not share-vs-share.)

## 2. The inventory model

Discovery (`pkg/kube/dra.go`, run every `GPU_DISCOVERY_INTERVAL`) lists ResourceSlices and classifies devices exactly once into catalog lanes:

| Device signature | Catalog lane |
|---|---|
| `type=gpu`, incl. flex parents carrying full-board `consumesCounters` | the dedicated whole-board lane (e.g. `h200-gpu`) |
| `type=mig` + `consumesCounters` + `profile=<p>` | the per-profile lane `h200-mig-<p>` |
| `counters` present (counter-set definer) | skipped — not claimable |
| `profile=1g.18gb+me` | skipped (menu-filtered; no lane targets it) |

On the H200 mirror: 2 GPU nodes × 8 boards — a **dedicated** pool (whole-only) and a **flex** pool (whole XOR partitions). Lane capacities are the *cluster* totals (16 whole; per-profile maxima = table instances × 8 flex boards). The booking availability engine enforces the cross-lane board math (a 16-whole + 4×7g booking passes every lane quota but would need 20 boards — refused).

### Per-profile DeviceClasses (C1)

For every MIG profile in the catalog, the discovery sync provisions a DeviceClass named `mig.nvidia.com-<dashed-profile>` (e.g. `mig.nvidia.com-7g-141gb`) with a CEL selector on driver/type/profile, managed via a label and GC'd when the lane leaves the catalog. Workloads claiming a per-profile class charge that profile's **own quota lane**; the folded classes `gpu.nvidia.com` / `mig.nvidia.com` remain as the whole-lane fallback. This is what makes per-profile Kueue quota mappings possible at all — one DeviceClass maps to one logical quota name.

## 3. Kueue integration

### The quota path

This plugin uses the **ResourceClaimTemplate path**: workloads reference an RCT naming a DeviceClass; Kueue maps `deviceClassName` → logical quota name via `deviceClassMappings` in the Kueue config (`rhoai-operators/kueue-cr.yaml`), charging `count` units (device-count mode). Only `ExactCount` requests are supported (by Kueue and by this plugin's charge parser).

Counter-based and capacity-based quota modes exist upstream (beta/alpha) but are not used here: the catalog lanes are device-count lanes, and the cross-lane board feasibility is enforced at *booking* time rather than via counter-unit quotas.

### The borrow model

```
Cohort "unreserved"  ← holds the REAL remaining capacity (catalog − Σ bookings)
   |
   |- CQ unreserved          (all quotas 0 — borrows everything from the Cohort)
   |- CQ system-<namespace>  (per booking: reserved quota; the rest borrows)
   |- CQ user-<username>     (per user booking, same shape)
   |- CQ unreserved-priority (borrowWithCohort LowerPriority, threshold 100)
```

- ClusterQueues carry **zeroed** GPU quotas; the Cohort holds what's left after reservations. Workloads borrow.
- Reserving subtracts from the Cohort (`applyCohortRemaining`) — a booking *shrinks the borrowable pool*, which is also what triggers Kueue's own `reclaimWithinCohort` preemption for reserved workloads.
- Each booking materializes: a **ClusterQueue** (`system-<ns>` / `user-<name>`), a **LocalQueue** (`reserved`) in the target namespace, **HardwareProfiles** per reserved lane, and — in DRA mode — namespaced **ResourceClaimTemplates** (`reserved-<lane>-rct`).

### AdmissionCheck wiring — the load-bearing detail

Kueue gates and evicts only on checks a ClusterQueue *references*. On this operand, **the plain `spec.admissionChecks` list on a ClusterQueue is silently dropped** — only the strategy form persists:

```yaml
spec:
  admissionChecksStrategy:
    admissionChecks:
      - name: booker
```

Everything that wires the booker into a CQ writes the strategy form (`applyUserReservation`, `ensureUnreservedCQCheck`, `rhoai-operators/kueue-queues.yaml`). A CQ built with the plain list has *no* enforcement, and a CQ rebuild loses it — this was found live and is regression-relevant for any future operand bump.

## 4. The booker AdmissionCheck

**File:** `pkg/kube/admissioncheck.go` · **Flag:** `BOOKER_ENFORCER_ENABLED` · **Cadence:** `BOOKER_ENFORCER_INTERVAL` (30s)

The plugin owns a cluster-scoped **AdmissionCheck** named `booker` (controller name `booker.gpu-booking-plugin`). Every CQ on a booker-managed lane references it; Kueue initializes the check `Pending` on each workload, holds gated pods until every check is `Ready`, and **evicts admitted workloads when a check leaves Ready**. The enforcer reconciles each workload's check state every pass.

For a custom check, the **`Active` condition is the controller's responsibility** — Kueue refuses to admit workloads referencing an inactive check (`references inactive AdmissionCheck(s)`). The enforcer marks it Active on every pass (idempotent) and re-creates the object if deleted.

### The policy (unified FIFO, both lanes)

Workloads are evaluated oldest-first across the `reserved` and `unreserved` LocalQueues:

1. **Booking coverage wins on either lane.** If the workload's lane charges fit under the user's *active bookings* (system bookings cover all requesters of the target namespace — `coverageUser` trims the `user-` prefix on both sides), the check is `Ready` and the workload **spends booking credits**. A booking is a lane-agnostic budget: a holder deploying through the unreserved LQ still runs on their booking without consuming free capacity.
2. **Reserved lane, uncovered** → `Pending`: *"no active booking covering \<lane\> (available N, requested M) — book it or deploy with reserved=false"*.
3. **Unreserved lane, uncovered** → free-capacity policy, all of:
   - **Shape cap:** charge ≤ the lane's **per-node device maximum** (computed live from ResourceSlices). DRA is node-local — a bigger single-pod claim can *never* schedule, so it gates before admission with *"deploy replicas of ≤N instead"*.
   - **Board model:** the whole lane and the MIG lanes share the physical pools (8 dedicated boards, whole-only; 8 flex boards of an 8-slice grid each, whole XOR partitions). The check packs `booked + granted + this workload's` units with the same engine the booking flow uses (`database.BoardCapacityCheck`) — a carve in a MIG lane consumes flex boards the flat whole-lane count still counts, so `16 whole + one 7g.141gb carve` (needing a 17th board) is refused with *"board pool exhausted: …"*. Live finding, 2026-11-26: two 8-board pods + a carve were all admitted under the flat budget and the second pod sat Pending with an unallocatable claim.
   - **Flat budget:** charge ≤ catalog capacity − Σ active bookings − Σ other unreserved charges granted this pass — the catch-all for lanes outside the static board model. Else *"lane \<lane\> at capacity: free N, requested M (…) — book it, wait, or reduce the request"*.
   - **Backfill:** a gated workload consumes no budget, so smaller later workloads still run past a blocked big one — an unschedulable shape cannot head-of-line-block the lane.
4. **Zero-charge workloads** (no claims, or a class the mapping doesn't cover) pass immediately — the check gates every workload in a wired CQ, and a CPU-only workload has nothing to book. Unmapped claim classes remain Kueue's own DRA path's business.

### Make-room and eviction

The same flip does all eviction work:

| Trigger | Mechanism |
|---|---|
| Booking expires / cancelled | holder's charges lose coverage → check flips `Ready`→`Pending` → Kueue evicts |
| New booking shrinks the free budget | over-budget unreserved holders flip `Pending` → evicted; when contended, **oldest keep their allocation, youngest lose** |
| Workload admitted before its CQ carried the check | no `Ready`→`Pending` transition exists for Kueue to fire on → the **deactivation discriminator** deposes it: first pass patches the gating message; if still admitted on the next pass, the workload is deactivated once (`spec.active=false` — quota + claim released, pods deleted). Its Deployment's replacement then hits the check like any fresh workload |

The discriminator matches only this controller's own gate messages (`isBookerGateMessage` — known prefixes plus an em-dash signature) so Kueue's own messages never trigger a deactivation. Deactivated workloads are skipped entirely — they hold nothing, spend nothing, and never re-admit; the user redeploys (the gating message says so).

Booking feasibility (`CheckDateCapacity`) counts `source='reserved'` rows only: **bookings compete with bookings**. Live usage from unbooked workloads is *consumed* rows — evictable, never a reason to refuse a booking. Together with make-room this is the phase-C semantic: you can always book free catalog capacity, and the holders move out of the way.

### RBAC (beyond the base sync rules)

| API group | Resources | Why |
|---|---|---|
| `kueue.x-k8s.io` | `admissionchecks`, `admissionchecks/status` | the check object + its Active condition (**`/status` needs its own rule**) |
| `kueue.x-k8s.io` | `workloads`, `workloads/status` | read charges, patch check states (**`/status` needs its own rule**) |
| `kueue.x-k8s.io` | `clusterqueues` (+`/status`) | check wiring self-heal on the static CQ |
| `resource.k8s.io` | `resourceslices`, `resourceclaims` | per-node device counts; consumed-usage attribution |
| `resource.k8s.io` | `resourceclaimtemplates` | charge resolution (`get` needed even with list/create/patch) |
| `resource.k8s.io` | `deviceclasses` | C1 per-profile class provisioning + GC |

## 5. Sizing workloads on this cluster

Bookings are cluster totals; **claims are pod shapes**. The two numbers are different axes:

- A **booking** of N units reserves N catalog units cluster-wide — booking 16 = the whole cluster for the window. Never capped.
- A **pod claim** (`dra.count`) allocates node-locally: **≤ the per-node maximum** (8 whole boards; per-profile maxima = instances × boards per node). The booker gates anything larger before admission — a bigger claim would sit Pending forever as an unsatisfiable quota-holder.
- More capacity = **more replicas**, each with its own ≤max claim. An 8-whole-board claim is the "one dedicated node" pattern.

For LLM serving: tensor parallelism spans one node's NVLink domain → **TP ≤ 8 per pod** (TP 16 is impossible without inter-node NVLink). Cross-node pipeline parallelism needs multi-pod orchestration (Ray / LeaderWorkerSet) — each stage pod still claims ≤max node-locally — which a KServe raw-deployment InferenceService does not provide. The KServe-native scaling axis is data parallel: N replicas × TP-sized claims. A 16-unit booking maps to 2×TP8 or 16×1.

## 6. Consumed-usage attribution

The Kueue sync (30s) reads ClusterQueue `flavorUsage` and records **consumed** booking rows so the UI shows real usage alongside reservations. In DRA mode it joins allocated ResourceClaims → their slice device attributes → catalog lane (`catalogResourceForDevice`), so a claim through a per-profile class lights the *profile* lane, not the folded whole lane. Guard: Σ(attributed) must equal Σ(quota-name flavorUsage totals) scoped to managed namespaces — any mismatch falls back to the quota-name view (never under- or over-attributes).

## 7. Operational notes and hard-won gotchas

| Gotcha | Detail |
|---|---|
| **Cross-lane board holds** | a MIG carve consumes flex boards from the whole lane's physical pool; per-lane budgets alone admit whole workloads that can never allocate — the booker packs whole + MIG demand through the board model (`BoardCapacityCheck`) before granting |
| **CQ `admissionChecks` vs strategy** | the plain list is silently dropped by this operand — always write `admissionChecksStrategy` (§3) |
| **`cohort` vs `cohortName`** | served CQs expose `spec.cohortName`; readers must use it |
| **RBAC `/status` subresources** | `admissionchecks/status` and `workloads/status` need their own rules; `resourceclaimtemplates` needs `get` even with list/create/patch |
| **RCT nesting** | a ResourceClaimTemplate nests the claim spec at **`spec.spec.devices`** (a ResourceClaim uses `spec.devices`) — reading the shallow path yields empty charges with no error |
| **AdmissionCheck Active condition** | for custom checks it is the controller's job; an inactive check hard-gates everything referencing it |
| **Pre-enforcement admissions** | Kueue does not evict a workload whose check was initialized post-admission — hence the deactivation discriminator's two-pass dance |
| **Enforcer ↔ CQ coupling** | `BOOKER_ENFORCER_ENABLED=false` rebuilds per-user CQs without the check (advisory again); the static `unreserved` CQ unwires via its GitOps manifest |
| **Whole-board claims vs MIG surface** | whole-GPU claims flip boards' MIG mode off at prepare; profile *geometry* queries must stay mode-independent (driver-side contract) |
| **Node clock skew** | the flex node runs ~20 min behind — compare API ages, not log timestamps, when correlating |

### Verification quick reference

```bash
# Check state per workload (the booker's decisions)
kubectl -n <ns> get workloads.kueue.x-k8s.io -o json | \
  jq -r '.items[] | .metadata.name as $w | .status.admissionChecks[]?
         | select(.name=="booker") | "\($w) \(.state) \(.message)"'

# CQ wiring (must be the strategy form)
kubectl get clusterqueue <name> -o jsonpath='{.spec.admissionChecksStrategy}'

# The check is Active (the enforcer's job)
kubectl get admissioncheck booker \
  -o jsonpath='{.status.conditions[?(@.type=="Active")].status}'

# Per-node inventory behind the shape cap
kubectl get resourceslices -o json | \
  jq -r '[.items[] | {node: .spec.nodeName, n: (.spec.devices | length)}] | group_by(.node) |
         map({node: .[0].node, devices: (map(.n) | add)})'
```

## 8. Version notes

- **Kueue operand (RHEL build, v0.19 lineage):** `KueueDRAIntegration` beta-on; DRA quota is charged at admission from claim templates, but **admission does not verify device feasibility** — quota numbers say 16 while no node has more than 8. The booker's shape cap + budget close this gap on any operand; upstream's `KueueDRADeviceFeasibility` (alpha, v0.20) attacks the same gap via TAS and needs a `topologyName` flavor.
- **k8s 1.36 / OCP 5:** `DRAPartitionableDevices` (KEP-4815) beta-on — the partitionable enumeration this catalog is built on; `DRAConsumableCapacity` (KEP-5075) beta.
- **API versions:** the plugin speaks `kueue.x-k8s.io/v1beta1` for CQs/workloads/admission checks; GitOps manifests may use `v1beta2` (same storage, converted). `resource.k8s.io/v1` for slices/claims/classes.

## 9. References

- Upstream Kueue DRA concepts: <https://kueue.sigs.k8s.io/docs/concepts/dynamic_resource_allocation/>
- Set up Kueue DRA: <https://kueue.sigs.k8s.io/docs/tasks/manage/setup_dra/>
- Run workloads with DRA devices: <https://kueue.sigs.k8s.io/docs/tasks/run/dra/>
- Develop an AdmissionCheck controller: <https://kueue.sigs.k8s.io/docs/tasks/dev/develop-acc/>
- Kubernetes DRA: <https://kubernetes.io/docs/concepts/scheduling-eviction/dynamic-resource-allocation/>
- KEP-4815 partitionable devices, KEP-5004 extended resources, KEP-5075 consumable capacity — <https://github.com/kubernetes/enhancements/>
- Booking-model design record: `~/.opencode/plans/dynamic-mig-booking.md` (testbed sessions)
