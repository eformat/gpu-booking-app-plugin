# GPU Resources

<span class="badge">Topics: H200, MIG, Partitioning</span>

---

## Available Resources

The booking system manages a pool of NVIDIA H200 GPUs with MIG (Multi-Instance GPU) partitioning enabled. You can book either a full GPU or a smaller MIG partition depending on your workload needs.

| Resource | Type | Units | Use case |
|----------|------|-------|----------|
| **H200 Full GPU** | `nvidia.com/gpu` | 8 | Large model training, full GPU inference |
| **MIG 3g.71gb** | `nvidia.com/mig-3g.71gb` | 8 | Medium model fine-tuning, large inference |
| **MIG 2g.35gb** | `nvidia.com/mig-2g.35gb` | 8 | Small model training, inference workloads |
| **MIG 1g.18gb** | `nvidia.com/mig-1g.18gb` | 16 | Notebooks, small experiments, development |

![images/gpu-resources.png](images/gpu-resources.png)

---

## What is MIG?

Multi-Instance GPU (MIG) lets a single physical GPU be partitioned into multiple isolated instances. Each instance has its own compute, memory, and memory bandwidth -- they behave like independent smaller GPUs.

### MIG partition sizes

The H200 supports the following partition layout:

```
H200 GPU (80GB HBM3)
├── 3g.71gb  (3 compute slices, 71GB memory)  x8
├── 2g.35gb  (2 compute slices, 35GB memory)  x8
└── 1g.18gb  (1 compute slice,  18GB memory)  x16
```

### GPU equivalents

Each resource type has a GPU equivalent weight used for capacity planning:

| Resource | GPU Equivalent |
|----------|---------------|
| H200 Full GPU | 1.0 |
| MIG 3g.71gb | 0.5 |
| MIG 2g.35gb | 0.25 |
| MIG 1g.18gb | 0.125 |

The calendar badges and booking modal show GPU equivalent totals to help you understand the overall capacity impact of your bookings.

### Choosing the right resource

- **Full GPU** -- use when your workload needs the full 80GB memory or all compute cores (e.g., training large models, multi-GPU distributed training)
- **MIG 3g.71gb** -- good for most single-GPU training and large inference workloads
- **MIG 2g.35gb** -- suitable for smaller model fine-tuning and inference
- **MIG 1g.18gb** -- ideal for Jupyter notebooks, development, small experiments, and inference of quantised models

<div class="alert alert-info">
  <strong>Tip</strong>
  <p>Start with the smallest partition that fits your workload. You can always book a larger resource if needed. Smaller partitions leave more capacity available for other users.</p>
</div>

---

## Resource Selector

Switch between resource types using the four cards in the header.

![images/resource-selector-detail.png](images/resource-selector-detail.png)

### Multi-select

You can view multiple resource types simultaneously:

- **Click** a card to select it exclusively
- **Ctrl+click** (or Cmd+click on Mac) to add or remove a resource type from the selection
- At least one resource type must remain selected

When multiple resources are selected, the booking grid shows a separate table for each resource type, each with its own unit columns and availability counts.

![images/gpu-resources-multiple.png](images/gpu-resources-multiple.png)

### Default selection

The app starts with **H200 Full GPU** selected. You can Ctrl+click to add MIG resources alongside it.

---

## Cluster Resources

Each booking reservation translates into Kueue ClusterQueue resources in the OpenShift cluster. When you have an active booking:

- A **ClusterQueue** is created with your reserved GPU quota (plus proportional CPU and memory)
- A **LocalQueue** in your namespace points to your ClusterQueue
- **HardwareProfile(s)** are created for each GPU resource type you reserved

Your workloads submitted to the `reserved` LocalQueue are protected from preemption by unreserved workloads.

### Reservation expiry

Reservations use the `rhai-tmm.dev/until` label with a UTC timestamp derived from the latest `end_hour` across your bookings for the day. Full day bookings expire at midnight UTC (00:00 next day). Hourly bookings expire at their specified end hour.

---

## Next Steps

- [Making Bookings](making-bookings) -- reserve GPU slots
- [Kueue & Auto-Bookings](kueue) -- how automatic bookings from workloads work
