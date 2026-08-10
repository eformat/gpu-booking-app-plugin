---
name: gpu-workload-audit
description: Identify which workloads are consuming GPU and MIG slice resources via Kueue, and surface stray workloads that hold Kueue quota but have no running pods.
---

## GPU Workload Audit

Audits Kueue workloads for GPU and MIG slice usage, then cross-references with actual running pods to find resource leaks — workloads holding admitted quota with no corresponding running pods.

### Step 1: List all Kueue workloads with their resource requests

```bash
oc get workloads.kueue.x-k8s.io -A -o json | jq -r '
  .items[] |
  . as $w |
  ($w.metadata.namespace + "/" + $w.metadata.name) as $id |
  ($w.status.conditions // []) as $conds |
  ($conds | map(select(.type == "QuotaReserved")) | first) as $qr |
  ($conds | map(select(.type == "Admitted"))      | first) as $ad |
  ($conds | map(select(.type == "Finished"))      | first) as $fin |
  ($w.spec.podSets // [] | [.[].template.spec.containers[]?.resources.requests // {} | to_entries[] | select(.key | test("nvidia\\.com/(gpu|mig-)")) | {(.key): .value}] | add // {}) as $gpuReqs |
  select($gpuReqs | length > 0) |
  {
    id:          $id,
    quotaReserved: ($qr.status // "Unknown"),
    admitted:      ($ad.status // "Unknown"),
    finished:      ($fin.status // "Unknown"),
    finishedReason: ($fin.reason // ""),
    gpuRequests:   $gpuReqs,
    ownerKind:     ($w.metadata.ownerReferences[0].kind // "none"),
    ownerName:     ($w.metadata.ownerReferences[0].name // "none")
  }
' | jq -s .
```

This shows every workload that requested `nvidia.com/gpu` or `nvidia.com/mig-*` resources along with its Kueue condition status.

### Step 2: Find stray workloads — quota held but no running pods

Fetches all running pods in one call, then correlates in-memory — avoids per-workload API calls which time out on large clusters.

```bash
WORKLOADS=$(oc get workloads.kueue.x-k8s.io -A -o json | jq -r '
  .items[] |
  select(
    (.status.conditions // [] | map(select(.type == "QuotaReserved" and .status == "True")) | length > 0) and
    (.status.conditions // [] | map(select(.type == "Finished" and .status == "True")) | length == 0)
  ) |
  ((.spec.podSets // []) | [.[].template.spec.containers[]?.resources.requests // {} | to_entries[] | select(.key | test("nvidia\\.com/(gpu|mig-)")) | .value] | add) as $gpuTotal |
  select($gpuTotal != null) |
  [
    .metadata.namespace,
    (.metadata.ownerReferences[0].name // "NO-OWNER"),
    (.metadata.ownerReferences[0].kind // "none"),
    (.spec.podSets // [] | [.[].template.spec.containers[]?.resources.requests // {} | to_entries[] | select(.key | test("nvidia\\.com/(gpu|mig-)")) | "\(.key)=\(.value)"] | join(","))
  ] | @tsv
')

RUNNING_PODS=$(oc get pods -A --field-selector=status.phase=Running -o json | jq -r '.items[] | [.metadata.namespace, .metadata.name] | @tsv')

printf "%-35s %-55s %-15s %-20s %s\n" "NAMESPACE" "OWNER" "KIND" "GPU-REQUEST" "STATUS"
printf "%-35s %-55s %-15s %-20s %s\n" "---------" "-----" "----" "-----------" "------"

echo "$WORKLOADS" | while IFS=$'\t' read ns owner kind gpureq; do
  # For Pod owners: check if that exact pod is running
  # For StatefulSet owners: pods are named <owner>-0, <owner>-1 etc — check prefix
  # For no-owner: always stray
  if [ "$owner" = "NO-OWNER" ]; then
    status="*** STRAY (no owner ref)"
  elif [ "$kind" = "Pod" ] && echo "$RUNNING_PODS" | grep -q "^${ns}	${owner}$"; then
    status="RUNNING"
  elif [ "$kind" = "StatefulSet" ] && echo "$RUNNING_PODS" | grep -q "^${ns}	${owner}-[0-9]"; then
    status="RUNNING"
  elif [ "$kind" = "Pod" ] || [ "$kind" = "StatefulSet" ]; then
    status="*** STRAY (not running)"
  else
    # Jobs, RayJobs etc — check if any pod with matching label exists
    status="CHECK MANUALLY (kind=$kind)"
  fi
  printf "%-35s %-55s %-15s %-20s %s\n" "$ns" "$owner" "$kind" "$gpureq" "$status"
done
```

Lines marked `*** STRAY` hold Kueue GPU quota but have no running workload.

### Step 3: Show LocalQueue GPU usage summary

```bash
oc get localqueues.kueue.x-k8s.io -A -o json | jq -r '
  .items[] |
  select(.status.reservingWorkloads > 0 or .status.admittedWorkloads > 0) |
  (.status.flavorsReservation // .status.flavorsUsage // .status.flavorUsage // []) as $flavors |
  $flavors[].resources[] |
  select(.name | test("nvidia\\.com/(gpu|mig-)")) |
  {
    queue:     (..|strings | if . == "reservingWorkloads" then "x" else . end) ,
    resource:  .name,
    total:     .total
  }
' 2>/dev/null || \
oc get localqueues.kueue.x-k8s.io -A -o json | jq '[
  .items[] |
  . as $q |
  ($q.status.flavorsReservation // $q.status.flavorsUsage // $q.status.flavorUsage // [])[] |
  .resources[] |
  select(.name | test("nvidia\\.com/(gpu|mig-)")) |
  {
    namespace: $q.metadata.namespace,
    queue:     $q.metadata.name,
    resource:  .name,
    reserved:  .total
  }
]'
```

### Step 4: Identify workloads pending admission (quota waiting)

These hold a slot in the ClusterQueue but haven't been admitted yet — they may be blocking other users.

```bash
oc get workloads.kueue.x-k8s.io -A -o json | jq '[
  .items[] |
  select(
    (.status.conditions // [] | map(select(.type == "QuotaReserved" and .status == "False" and .reason == "Pending")) | length > 0)
  ) |
  {
    namespace: .metadata.namespace,
    name:      .metadata.name,
    owner:     (.metadata.ownerReferences[0].name // "none"),
    ownerKind: (.metadata.ownerReferences[0].kind // "none"),
    gpuRequests: (
      [.spec.podSets[]?.template.spec.containers[]?.resources.requests // {} |
       to_entries[] | select(.key | test("nvidia\\.com/(gpu|mig-)")) | {(.key): .value}] | add // {}
    ),
    pendingSince: (.status.conditions[] | select(.type == "QuotaReserved") | .lastTransitionTime)
  }
  | select(.gpuRequests | length > 0)
]'
```

### Step 5: Check for evicted or preempted workloads still holding quota

```bash
oc get workloads.kueue.x-k8s.io -A -o json | jq '[
  .items[] |
  select(
    (.status.conditions // [] | map(select(
      (.type == "Evicted" or .type == "Preempted") and .status == "True"
    )) | length > 0) and
    (.status.conditions // [] | map(select(.type == "Finished" and .status == "True")) | length == 0)
  ) |
  {
    namespace:   .metadata.namespace,
    name:        .metadata.name,
    owner:       (.metadata.ownerReferences[0].name // "none"),
    evictReason: (.status.conditions[] | select(.type == "Evicted" or .type == "Preempted") | .reason),
    evictMsg:    (.status.conditions[] | select(.type == "Evicted" or .type == "Preempted") | .message)
  }
]'
```

### Interpreting results

| Condition | Meaning |
|---|---|
| `QuotaReserved=True`, `Admitted=True`, `running_pods=0` | **Stray** — quota consumed, no work happening |
| `QuotaReserved=False`, `reason=Pending` | Waiting for quota — check ClusterQueue capacity |
| `Evicted=True`, `Finished=False` | Preempted but not cleaned up — may need manual deletion |
| `Finished=True` | Completed — quota should be released automatically |

### Cleaning up stray workloads

If a workload is confirmed stray (owner Job/RayJob is gone or failed, no running pods), delete the workload to release quota:

```bash
# Dry-run: show what would be deleted
oc get workloads.kueue.x-k8s.io -n <namespace> <workload-name> -o yaml

# Delete the stray workload (releases quota immediately)
oc delete workloads.kueue.x-k8s.io -n <namespace> <workload-name>
```

Alternatively, delete the owner object (Job, RayJob) — Kueue garbage-collects the workload automatically.
