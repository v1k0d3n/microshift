# Phase 4: Virtual IP and Network Configuration

## Why a VIP?
With two kube-apiserver instances, external clients (kubectl, CI tools) and the
kubelets need a stable endpoint. A Virtual IP (VIP) provides:
- Single DNS/IP for `kubeconfig` files
- Automatic failover when one node goes down
- No client-side reconfiguration needed

## VIP Implementation: keepalived

### Why keepalived?
- Lightweight, battle-tested, available in all Linux distros
- Uses VRRP (Virtual Router Redundancy Protocol)
- Sub-second failover
- Already used in OpenShift for API VIP in IPI installations

### Configuration

**VIP Address**: Must be chosen from the same subnet or a routable address.
For our environment, we'll use a configurable VIP (e.g., `192.168.5.200`).

**demo-1 (`/etc/keepalived/keepalived.conf`):**
```
vrrp_instance MICROSHIFT_VIP {
    state MASTER
    interface eth0              # Auto-detected from node IP
    virtual_router_id 51
    priority 100                # Higher = preferred master
    advert_int 1
    authentication {
        auth_type PASS
        auth_pass <generated>
    }
    virtual_ipaddress {
        192.168.5.200/24
    }
    track_script {
        chk_apiserver
    }
}

vrrp_script chk_apiserver {
    script "/usr/libexec/microshift/check-apiserver.sh"
    interval 2
    weight -50                  # Reduce priority if apiserver is down
    fall 3                      # 3 failures before marking down
    rise 2                      # 2 successes before marking up
}
```

**demo-2: Same but `priority 99`** (lower, so demo-1 is preferred).

### Health check script (`check-apiserver.sh`)
```bash
#!/bin/bash
# Check both kube-apiserver AND postgresql health
curl -sk https://localhost:6443/healthz | grep -q "ok" && exit 0
exit 1
```

## MicroShift Integration

### New component: VIPService
```go
// pkg/controllers/vip.go
type VIPService struct {
    cfg *config.Config
}

func (s *VIPService) Name() string { return "vip" }
func (s *VIPService) Dependencies() []string { return []string{"kube-apiserver"} }
```

This service:
1. Generates keepalived configuration from MicroShift config
2. Starts keepalived process (similar to how etcd is managed)
3. Monitors keepalived health

### Configuration
```yaml
# /etc/microshift/config.yaml
twoNode:
  enabled: true
  vip: "192.168.5.200"
  vipInterface: ""              # Auto-detect from node IP if empty
  peer: "192.168.7.167"        # The other node's IP
```

## In-Cluster Networking

### kubernetes Service Endpoint
The `kubernetes` service in `default` namespace needs to list both API servers:
- With `--endpoint-reconciler-type=lease` (default), each kube-apiserver
  registers itself
- Both IPs appear in Endpoints, kube-proxy load-balances
- If one API server goes down, its lease expires and it's removed from Endpoints

### Pod-to-API-server communication
Pods use the `kubernetes.default.svc` DNS name which resolves to the ClusterIP
service. This is load-balanced across both API servers automatically.

### Node-to-node communication
For the 2-node setup, nodes need to communicate:
- PostgreSQL replication: port 5432
- Patroni REST API: port 8008
- Patroni Raft: port 8009
- VRRP: IP protocol 112
- Existing: kubelet (10250), API server (6443)

### Firewall rules (additional for 2-node)
```bash
# PostgreSQL replication
firewall-cmd --add-port=5432/tcp --permanent
# Patroni
firewall-cmd --add-port=8008/tcp --permanent
firewall-cmd --add-port=8009/tcp --permanent
# VRRP
firewall-cmd --add-protocol=vrrp --permanent
```

## OVN-Kubernetes Considerations

MicroShift uses OVN-Kubernetes for CNI. In 2-node mode:
- OVN-Kubernetes needs to know about both nodes
- Each node runs `ovnkube-node`
- The OVN northbound/southbound databases need to be accessible
- Currently MicroShift runs OVN databases on the single node

### OVN HA Options
1. **OVN IC (Interconnect)**: Connect two separate OVN zones
2. **OVN clustered DB**: Run OVN NB/SB with Raft (but same 3-node problem)
3. **Single OVN master + standby**: One node runs OVN DBs, other connects remotely

**Recommendation**: Option 3 for simplicity — the VIP holder runs OVN databases,
the other node connects remotely. On failover, the new VIP holder starts OVN DBs.

## Files to Create/Modify

| Action | Path | Description |
|--------|------|-------------|
| CREATE | `pkg/controllers/vip.go` | VIP/keepalived service |
| CREATE | `assets/keepalived/keepalived.conf.tmpl` | Config template |
| CREATE | `assets/keepalived/check-apiserver.sh` | Health check script |
| MODIFY | `pkg/cmd/run.go` | Add VIP service to chain |
| MODIFY | `pkg/config/twonode.go` | VIP configuration |
| CREATE | `docs/contributor/two-node-ha.md` | Architecture documentation |
