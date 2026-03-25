# MicroShift 2-Node HA: Project Overview

## Goal
Transform MicroShift from a single-node-only Kubernetes distribution into a 2-node
High Availability (HA) deployment, where both nodes actively run workloads and the
control plane survives the loss of one node.

## Environment
| Host   | IP              | Arch   |
|--------|-----------------|--------|
| demo-1 | 192.168.5.107   | x86_64 |
| demo-2 | 192.168.7.167   | varies |

## Core Technical Decision: Replace etcd with Kine + PostgreSQL

### Why not etcd?
- etcd requires **3+ nodes** for a proper quorum. With only 2 nodes, losing one
  node means losing quorum — which is *worse* than a single-node setup.
- etcd's Raft consensus protocol fundamentally requires an odd number of voters
  for fault tolerance: `(n-1)/2` failures tolerated.

### Why Kine + PostgreSQL?
- **Kine** (https://github.com/k3s-io/kine) is a shim that translates the etcd
  v3 gRPC API into SQL operations. It's battle-tested in K3s/RKE2 at scale.
- **PostgreSQL** supports **synchronous streaming replication** between exactly 2
  nodes, giving us true HA with automatic failover.
- This approach is proven: K3s runs millions of clusters using Kine with various
  SQL backends.
- PostgreSQL 2-node replication is well-understood operationally.

### Architecture

```
┌─────────────────────────────┐     ┌─────────────────────────────┐
│         demo-1              │     │         demo-2              │
│  ┌───────────────────────┐  │     │  ┌───────────────────────┐  │
│  │   kube-apiserver      │  │     │  │   kube-apiserver      │  │
│  │   (active)            │  │     │  │   (active)            │  │
│  └──────────┬────────────┘  │     │  └──────────┬────────────┘  │
│             │               │     │             │               │
│  ┌──────────▼────────────┐  │     │  ┌──────────▼────────────┐  │
│  │   Kine (etcd shim)    │  │     │  │   Kine (etcd shim)    │  │
│  └──────────┬────────────┘  │     │  └──────────┬────────────┘  │
│             │               │     │             │               │
│  ┌──────────▼────────────┐  │     │  ┌──────────▼────────────┐  │
│  │   PostgreSQL          │◄─┼─────┼──►  PostgreSQL           │  │
│  │   (primary/replica)   │  │     │  │  (replica/primary)    │  │
│  └───────────────────────┘  │     │  └───────────────────────┘  │
│                             │     │                             │
│  ┌───────────────────────┐  │     │  ┌───────────────────────┐  │
│  │ controller-manager    │  │     │  │ controller-manager    │  │
│  │ scheduler             │  │     │  │ scheduler             │  │
│  │ kubelet               │  │     │  │ kubelet               │  │
│  │ (leader-elected)      │  │     │  │ (standby)             │  │
│  └───────────────────────┘  │     │  └───────────────────────┘  │
│                             │     │                             │
│  ┌───────────────────────┐  │     │  ┌───────────────────────┐  │
│  │  keepalived (VIP)     │  │     │  │  keepalived (VIP)     │  │
│  └───────────────────────┘  │     │  └───────────────────────┘  │
└─────────────────────────────┘     └─────────────────────────────┘
```

**Key points:**
- Both nodes run ALL control plane components
- `kube-controller-manager` and `kube-scheduler` use Kubernetes leader election
  (already built into these components) — only one is active at a time
- Both `kube-apiserver` instances are active simultaneously (stateless, reads
  from local Kine→PostgreSQL)
- PostgreSQL handles data replication with synchronous streaming replication
- Patroni or repmgr manages PostgreSQL failover
- A Virtual IP (VIP) via keepalived floats between nodes for the API endpoint

## Plan Documents

| Document | Description | Status |
|----------|-------------|--------|
| [01-kine-integration.md](01-kine-integration.md) | Integrating Kine as the storage backend | Complete |
| [02-postgresql-ha.md](02-postgresql-ha.md) | PostgreSQL HA setup with streaming replication | Complete |
| [03-control-plane-ha.md](03-control-plane-ha.md) | Running HA control plane on both nodes | Complete |
| [04-vip-and-networking.md](04-vip-and-networking.md) | Virtual IP and network considerations | Complete |
| [05-certificates-and-pki.md](05-certificates-and-pki.md) | Certificate distribution for 2-node | Complete |
| [06-configuration.md](06-configuration.md) | Configuration schema changes | Complete |
| [07-implementation-phases.md](07-implementation-phases.md) | Phased implementation plan (master tracker) | Phases 1-9 Complete |
| [08-build-and-deploy.md](08-build-and-deploy.md) | Cross-compilation and deployment | Complete |
| [09-risks-and-mitigations.md](09-risks-and-mitigations.md) | Known risks and mitigations | Updated |
| [10-runtime-integration.md](10-runtime-integration.md) | Phase 7: Runtime integration (21 issues) | Complete |
| [11-k3s-lessons-architecture-pivot.md](11-k3s-lessons-architecture-pivot.md) | Architecture pivot: Patroni as external service | Complete |
| [12-failover-validation.md](12-failover-validation.md) | Phase 8: Failover test results | Complete |
| [13-raft-quorum-improvement.md](13-raft-quorum-improvement.md) | Phase 9: Automatic failover watchdog | Complete |
| [summary.md](summary.md) | Implementation progress summary | Updated |
| [user-guide-two-node-ha.md](user-guide-two-node-ha.md) | User guide for deployment and operations | Updated |

## Success Criteria
1. Both nodes run MicroShift control plane and workloads
2. Loss of either node does not kill the cluster (workloads reschedule)
3. API server remains accessible via VIP after single node failure
4. Data consistency maintained through PostgreSQL synchronous replication
5. x86_64 and arm64 binaries produced
6. Configuration is simple and follows MicroShift's "minimal config" philosophy
