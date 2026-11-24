# DRA & GPU Sharing

Some clusters allocate GPUs through **DRA** (Dynamic Resource Allocation, `resource.k8s.io/v1`) instead of the classic Kubernetes device plugin. The booking plugin detects this automatically and shows DRA pools alongside classic GPU pools — you don't need to know which mechanism backs a given resource, but the sharing semantics differ and it pays to understand them.

## Classic GPUs vs DRA pools

| | Classic (device plugin) | DRA pool |
|---|---|---|
| What you book | A whole GPU, or a static MIG partition | A **share** of a physical GPU, or a dedicated device |
| Isolation | Hard (one workload per GPU) | Depends on the badge — see below |
| How it appears in the catalog | "… Full GPU" cards | Product cards with an isolation badge |

## Isolation badges

Every DRA-backed card and booking row carries a badge telling you who else can use the same silicon:

- **Dedicated** (grey) — the whole device is yours for the slot. Comparable to a classic GPU booking.
- **Shared (no isolation)** (teal) — the physical GPU is divided into *consumable shares* (for example 4 shares per device). Each booking consumes one share. **Several workloads run on the same physical GPU concurrently, with no memory or compute isolation between them.** Your workload gets no guaranteed fraction of memory — shares are a scheduling construct, not a partition.
- **MIG partition** (purple) — the device is a hardware-partitioned slice (MIG). Hard isolation, but only available on GPUs that support MIG.

## Fractional GPU units

A consumable share is displayed as a fraction of a physical GPU, e.g. **0.25 GPU (10 GB)** on a 4-share 40 GB device. The *GPU equivalents* total in the booking dialog and usage panels is computed from these fractions, so bookings across different pools stay comparable: 4 shares on a 4-share device ≈ 1 full GPU of *scheduling capacity* (still without isolation).

## What happens behind the scenes

- Booking a DRA resource reserves **Kueue quota** for your namespace (1 quota unit per share), exactly like classic bookings.
- When a workload in your namespace runs, it presents a **ResourceClaim**; the DRA driver allocates a device with free capacity and Kueue's consumed-booking sync makes the usage visible in the usage panels with the ⚡ marker.
- Cluster admins may qualify DRA pool quota names by pool (for example `dra-share`) when a cluster hosts several DRA pools; the mechanics for you are identical.

## Limits to be aware of

- **No performance guarantee on shared devices**: co-tenants on the same GPU affect each other's throughput and memory headroom.
- Dynamic MIG booking (claim-driven repartitioning) is planned as a follow-up; MIG-capable pools currently appear with their fixed partition layout.
- Device health reporting (per-device Healthy/Unhealthy status on active bookings) lands in a later phase.
