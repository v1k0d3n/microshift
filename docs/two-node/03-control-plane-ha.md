# Phase 3: HA Control Plane on Both Nodes

## Overview
In the 2-node HA setup, both nodes run the full MicroShift control plane. This
document describes how each component operates in HA mode.

## Component Behavior in 2-Node Mode

### kube-apiserver (Active/Active)
- **Both** nodes run kube-apiserver simultaneously
- kube-apiserver is inherently stateless — it reads/writes to the storage backend
- Each connects to its local Kine instance → local PostgreSQL
- On the primary PG node: reads and writes work normally
- On the replica PG node: reads work, writes are forwarded to primary via PG
  replication (Kine handles this transparently when connected to a PG replica
  configured with `hot_standby = on`)

**Actually**: Kine writes MUST go to the PG primary. Two approaches:
1. **Kine connects to local PG only**: The replica node's Kine will get write
   errors. kube-apiserver retries. This is handled by the VIP — clients hit
   the VIP which points to the primary node's apiserver.
2. **Kine connects to PG primary always** (via Patroni's connection routing):
   Kine on both nodes always writes to the PG primary, reads from local.

**Recommendation**: Approach 2 — Kine uses Patroni's REST API to discover the
current primary and connects there for writes. Reads can be local.

### kube-controller-manager (Active/Standby via Leader Election)
- Kubernetes has **built-in leader election** for controller-manager
- Both nodes start controller-manager; only the leader is active
- Leader election uses the Kubernetes API (Lease objects in `kube-system`)
- When the leader node fails, the other node's controller-manager acquires the
  lease and becomes active
- Default lease duration: 15s, renew deadline: 10s, retry period: 2s

No code changes needed — just ensure both nodes start controller-manager.

### kube-scheduler (Active/Standby via Leader Election)
- Same as controller-manager: built-in leader election
- Only one scheduler is active at a time
- Failover happens automatically via Lease objects

No code changes needed.

### kubelet (Active/Active)
- Each node runs its own kubelet managing local pods
- Both kubelets register with the API server as separate nodes
- Workloads are scheduled to both nodes based on available resources

### OpenShift Controllers (Active/Standby)
- route-controller-manager, openshift-crd-manager, etc.
- These use the same leader election mechanism
- MicroShift already configures leader election for these components

## Required Code Changes

### 1. Service startup modifications (`pkg/cmd/run.go`)

```go
func (s *server) addControlPlaneServices() {
    // Storage backend (etcd or kine)
    if s.cfg.Storage.Backend == "kine" {
        s.svcManager.Add(&controllers.PostgreSQLService{cfg: s.cfg})
        s.svcManager.Add(&controllers.KineService{cfg: s.cfg})
    } else {
        s.svcManager.Add(&controllers.EtcdService{cfg: s.cfg})
    }

    // These run on BOTH nodes
    s.svcManager.Add(&controllers.KubeAPIServer{cfg: s.cfg})
    s.svcManager.Add(&controllers.KubeScheduler{cfg: s.cfg})
    s.svcManager.Add(&controllers.KubeControllerManager{cfg: s.cfg})
    // ... other controllers
}
```

### 2. Leader election configuration

kube-controller-manager and kube-scheduler already support `--leader-elect=true`.
MicroShift currently sets `--leader-elect=false` for single-node mode.

**Change**: When `twoNode.enabled: true`, set `--leader-elect=true`.

In `pkg/controllers/kube-controller-manager.go`:
```go
if cfg.TwoNode.Enabled {
    args["leader-elect"] = "true"
    args["leader-elect-lease-duration"] = "15s"
    args["leader-elect-renew-deadline"] = "10s"
    args["leader-elect-retry-period"] = "2s"
} else {
    args["leader-elect"] = "false"
}
```

Same change in `pkg/controllers/kube-scheduler.go`.

### 3. Node identity and registration

Each node needs a unique identity:
- Hostname: already unique (demo-1, demo-2)
- Node IP: already unique
- Certificates: need per-node serving certificates
- kubelet: registers with its own hostname

### 4. kube-apiserver multi-node awareness

kube-apiserver needs to know about both nodes for:
- `--advertise-address`: Use the node's own IP (already configured)
- `--etcd-servers`: Point to local Kine (already `localhost:2379`)
- Service account signing: Must use the SAME signing key on both nodes

**Critical**: Both kube-apiservers must share:
- Service account signing key pair
- API server CA certificates
- Encryption keys (if encryption at rest is enabled)

These are managed in Phase 5 (certificates).

### 5. Networking considerations for dual API server

With two kube-apiserver instances, in-cluster clients (pods) need to reach
whichever is available:
- The `kubernetes` service in `default` namespace has Endpoints pointing to
  API server IPs
- With 2 nodes: both IPs should be in the Endpoints
- kube-apiserver's `--endpoint-reconciler-type=lease` handles this automatically

## Workload Scheduling

With two nodes, the scheduler can place pods on either node:
- DaemonSets run on both nodes automatically
- Deployments with `replicas: 2` spread across both nodes
- Node affinity/anti-affinity works as expected
- Resource-based scheduling uses combined resources of both nodes

## Failure Scenarios

### Node 1 (current PG primary) fails
1. Patroni detects primary failure, promotes Node 2's PG to primary
2. Kine on Node 2 continues serving (was already reading, now can write)
3. Controller-manager/scheduler on Node 2 acquire leader leases
4. VIP floats to Node 2 (keepalived)
5. kubelet on Node 2 continues running pods
6. Pods from Node 1 are marked NotReady, rescheduled to Node 2

### Node 2 (current PG replica) fails
1. Patroni on Node 1 detects replica loss
2. PG on Node 1 switches to async mode (no sync replica)
3. Kine on Node 1 continues serving normally
4. VIP stays on Node 1 (or was already there)
5. Pods from Node 2 rescheduled to Node 1

### Network partition between nodes
1. Both nodes' Patroni lose contact with each other
2. With `failsafe_mode: true`, the current primary stays primary
3. Replica does NOT promote (avoids split-brain)
4. VIP stays on current holder (keepalived VRRP timeout)
5. Cluster operates in degraded single-node mode
6. When connectivity restores, replica reconnects and syncs

## Files to Modify

| Action | Path | Description |
|--------|------|-------------|
| MODIFY | `pkg/cmd/run.go` | Conditional storage backend startup |
| MODIFY | `pkg/controllers/kube-controller-manager.go` | Leader election flags |
| MODIFY | `pkg/controllers/kube-scheduler.go` | Leader election flags |
| MODIFY | `pkg/controllers/kube-apiserver.go` | Shared SA keys, endpoint reconciler |
| CREATE | `pkg/config/twonode.go` | TwoNode config struct |
