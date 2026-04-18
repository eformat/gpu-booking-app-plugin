# Architecture Diagrams (Mermaid Source)

Rendered versions of these diagrams are in `ARCHITECTURE.md` using images from `images/`.

## Kueue Resource Hierarchy

```mermaid
flowchart TD
  cohort([Cohort: unreserved\nnominalQuota = total - reservations])

  subgraph shared[Shared Queues]
    cq_unres[ClusterQueue: unreserved\nnominalQuota: 0\nNo preemption policy]
    cq_pri[ClusterQueue: unreserved-priority\nnominalQuota: 0\nborrowWithinCohort: LowerPriority]
  end

  subgraph user_a[User Reservation: user-alice]
    cq_alice[ClusterQueue: user-alice\nnominalQuota: reserved amount\nreclaimWithinCohort: Any]
    lq_alice[LocalQueue: reserved\nNamespace: user-alice]
    hp_alice1[HardwareProfile:\nreserved-mig-35gb]
    hp_alice2[HardwareProfile:\nreserved-gpu]
  end

  subgraph user_b[User Reservation: user-bob]
    cq_bob[ClusterQueue: user-bob\nnominalQuota: reserved amount\nreclaimWithinCohort: Any]
    lq_bob[LocalQueue: reserved\nNamespace: user-bob]
    hp_bob1[HardwareProfile:\nreserved-mig-71gb]
  end

  cohort --- cq_unres
  cohort --- cq_pri
  cohort --- cq_alice
  cohort --- cq_bob
  cq_alice --> lq_alice
  lq_alice --> hp_alice1
  lq_alice --> hp_alice2
  cq_bob --> lq_bob
  lq_bob --> hp_bob1
```

## Preemption Model

```mermaid
flowchart LR
  subgraph preemption[Preemption Flow]
    direction TB
    workload([New Workload Submitted])
    check{Quota\navailable?}
    admit[Admit workload\nwithin nominalQuota]
    borrow{Can borrow\nfrom Cohort?}
    preempt{Can reclaim\nvia reclaimWithinCohort?}
    evict[Evict borrowing\nworkloads from Cohort]
    queue[Queue workload\nwait for capacity]
    run([Workload Running])

    workload --> check
    check -->|Yes| admit
    check -->|No| borrow
    borrow -->|Yes - unreserved CQs| admit
    borrow -->|No| preempt
    preempt -->|User CQ: Any| evict
    preempt -->|Unreserved CQ: Never| queue
    evict --> admit
    admit --> run
  end

  subgraph rules[Preemption Rules]
    direction TB
    r1[User CQs protect\nnominalQuota workloads]
    r2[User CQs can reclaim\nfrom borrowing workloads]
    r3[Unreserved CQs cannot\npreempt user workloads]
    r4[All unreserved workloads\nare borrowing - preemptible]
  end
```

## Consumed vs Reserved Booking Flow

```mermaid
flowchart TD
  subgraph sources[Booking Sources]
    user([User books via UI])
    kueue([Kueue Sync Daemon\nPolls LocalQueues])
  end

  subgraph booking_types[Booking Types]
    reserved[source: reserved\nUser-created bookings\nProtected from eviction]
    consumed[source: consumed\nAuto-synced from K8s\nEvictable by reserved]
  end

  subgraph conflict[Conflict Resolution]
    check_slot{Slot\noccupied?}
    check_source{Occupied by\nreserved or\nconsumed?}
    evict_consumed[Evict consumed booking\nInsert reserved booking]
    reject[409 Conflict\nslot_taken]
    insert[Insert booking]
  end

  subgraph k8s_sync[K8s Resource Sync]
    sync_res[syncReservations]
    apply_cq[Apply ClusterQueue\nuser-username]
    apply_lq[Apply LocalQueue\nreserved]
    apply_hp[Apply HardwareProfile\nper GPU type]
    apply_cohort[Update Cohort\nnominalQuota -= reserved]
  end

  user --> reserved
  kueue --> consumed
  reserved --> check_slot
  check_slot -->|No| insert
  check_slot -->|Yes| check_source
  check_source -->|consumed| evict_consumed
  check_source -->|reserved| reject
  evict_consumed --> insert
  insert -->|triggers| sync_res
  sync_res --> apply_cq
  apply_cq --> apply_lq
  apply_lq --> apply_hp
  sync_res --> apply_cohort
```

## Sync Lifecycle

```mermaid
flowchart TD
  subgraph kueue_loop[Kueue Sync Loop - every 60s]
    direction TB
    k1[List all LocalQueues\nacross namespaces]
    k2{reservingWorkloads > 0\nOR admittedWorkloads > 0?}
    k3[Extract flavorUsage\nper namespace]
    k4[Aggregate GPU counts\nper namespace + resource]
    k5[Lookup namespace owner\nrhai-tmm.dev/owner label]
    k6[Generate booking dates\ntoday through window]
    k7[Assign globally unique\nslot indices per resource]
    k8[Reconcile DB:\nadd missing, remove stale]
    k9{Reserved booking\non slot?}
    k10[Skip slot]
    k11[Create consumed booking\nkueue-ns-resource-slot-date]

    k1 --> k2
    k2 -->|Yes| k3
    k2 -->|No| k1
    k3 --> k4
    k4 --> k5
    k5 --> k6
    k6 --> k7
    k7 --> k8
    k8 --> k9
    k9 -->|Yes| k10
    k9 -->|No| k11
  end

  subgraph res_loop[Reservation Sync Loop - every 10min]
    direction TB
    r1[Query today + tomorrow\nreserved bookings from DB]
    r2[Group by user\nsum GPU counts per resource]
    r3[Calculate CPU/Memory\nfrom GPU shares]
    r4[Determine until timestamp\nfrom latest end_hour]
    r5[Apply ClusterQueue\nuser-username]
    r6[Apply LocalQueue\nreserved in user namespace]
    r7[Apply HardwareProfile\nper GPU resource type]
    r8[Update Cohort\nremaining resources]

    r1 --> r2
    r2 --> r3
    r3 --> r4
    r4 --> r5
    r5 --> r6
    r6 --> r7
    r7 --> r8
  end

  subgraph clean_loop[Expiration Cleaner - every 10min]
    direction TB
    c1[List ClusterQueues with\nrhai-tmm.dev/until label]
    c2{Until timestamp\npast current time?}
    c3[Delete ClusterQueue]
    c4[Delete LocalQueue]
    c5[Delete HardwareProfiles]
    c6[Re-sync Cohort\nnominalQuota]

    c1 --> c2
    c2 -->|Yes| c3
    c2 -->|No| c1
    c3 --> c4
    c4 --> c5
    c5 --> c6
  end

  db[(SQLite DB\nbookings.db)]
  k8s([Kubernetes API\nKueue Resources])

  k11 --> db
  r1 -.-> db
  r5 --> k8s
  r6 --> k8s
  r7 --> k8s
  c3 --> k8s
  c4 --> k8s
  c5 --> k8s
```
