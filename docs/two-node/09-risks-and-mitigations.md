# Risks and Mitigations

## Critical Risks

### 1. Split-Brain with 2 Nodes
**Risk**: Both nodes believe they are the primary PostgreSQL instance, leading to
divergent data and potential data corruption.

**Likelihood**: Medium — this is the fundamental challenge of 2-node HA.

**Mitigation**:
- Patroni's `failsafe_mode: true` prevents automatic promotion when quorum is lost
- `synchronous_mode: true` ensures the replica has all data before primary commits
- Fencing: if a node cannot confirm it should be primary (via peer connectivity
  check), it demotes itself
- Worst case: Patroni manual intervention, documented in runbook

**Residual risk**: Network partition where both nodes can reach clients but not
each other. Mitigated by VIP (only one node has VIP, only that node serves writes).

### 2. Kine API Compatibility
**Risk**: Kine doesn't implement 100% of the etcd v3 API. Some Kubernetes
operations might fail or behave differently.

**Likelihood**: Low — K3s has proven Kine works for standard Kubernetes operations.

**Mitigation**:
- K3s is battle-tested at scale with Kine
- Run the full Kubernetes conformance test suite
- Monitor for `etcd` errors in kube-apiserver logs during testing
- Kine is actively maintained by Rancher/SUSE

**Residual risk**: OpenShift-specific CRD operations that aren't in standard K8s.
Test thoroughly.

### 3. OVN-Kubernetes 2-Node Challenges
**Risk**: OVN-Kubernetes uses its own databases (northbound/southbound) which have
their own clustering requirements. Running OVN on 2 nodes may cause issues.

**Likelihood**: Medium — MicroShift's OVN is simplified but still expects single-node.

**Mitigation**:
- Start with the OVN master on VIP-holding node only
- On failover, start OVN on the new VIP holder
- Long-term: evaluate OVN IC (Interconnect) for multi-site
- Alternative: consider Flannel/Calico as simpler CNI options for 2-node

### 4. Performance Degradation
**Risk**: Kine + PostgreSQL adds latency compared to embedded etcd. Could affect
API server responsiveness.

**Likelihood**: Low — measured at 1-2ms additional latency in K3s benchmarks.

**Mitigation**:
- PostgreSQL on local SSD
- Connection pooling in Kine
- Both API servers active reduces per-server load
- Monitor with `apiserver_request_duration_seconds` metrics

### 5. Certificate Distribution Security
**Risk**: The join token contains or provides access to CA certificates. If
intercepted, an attacker could join a malicious node.

**Likelihood**: Low — requires network access during the brief join window.

**Mitigation**:
- Join tokens are time-limited (expire after 24 hours)
- Join tokens are single-use
- TLS on all communication channels
- Join process requires explicit approval on primary node

### 12. Patroni Watchdog Reboots Surviving Node (DISCOVERED + FIXED)
**Risk**: Patroni's built-in Linux watchdog feature (`/dev/watchdog`) causes the
kernel to hard-reboot the surviving node when Raft quorum is lost. This completely
defeats the HA design — rebooting one node causes both nodes to go down.

**Likelihood**: High — this is the **default Patroni behavior** unless explicitly disabled.

**Root cause**: When the peer goes down, Patroni's 2-node Raft cluster loses quorum.
Patroni cannot maintain its leader lock, so it stops petting the kernel watchdog device.
The kernel's watchdog timer expires and triggers a hard reboot. This is a fencing mechanism
designed for 3+ node clusters to prevent split-brain, but in a 2-node setup where losing
one node is the *expected* failure mode, it's catastrophic.

**Fix applied**:
- Added `watchdog: mode: off` to both Patroni configs in `scripts/deploy-two-node.sh`
- Our MicroShift failover watchdog (`pkg/controllers/failover_watchdog.go`) handles
  promotion correctly via `pg_ctl promote` without needing Patroni's self-fencing

**Residual risk**: None — with `watchdog: mode: off`, Patroni does not interact with
the kernel watchdog device. Split-brain is prevented by the "only replica promotes"
invariant in our failover watchdog.

**Status**: MITIGATED (2026-03-25)

## Moderate Risks

### 6. PostgreSQL Resource Consumption
**Risk**: PostgreSQL adds memory/CPU overhead to MicroShift's already-constrained
edge environment.

**Mitigation**:
- PostgreSQL configured with minimal settings (128MB shared_buffers, 20 max_connections)
- Kine's workload is light (Kubernetes API patterns are well-suited to RDBMS)
- Net memory may be similar to etcd (which also uses ~128-256MB)

### 7. Upgrade Complexity
**Risk**: Upgrading MicroShift now requires coordinating upgrades across 2 nodes
with PostgreSQL schema compatibility.

**Mitigation**:
- Rolling upgrade procedure: upgrade secondary first, then primary
- Kine manages its own schema (backward compatible)
- PostgreSQL major version upgrades handled separately
- Document upgrade procedure in detail

### 8. Patroni Dependency
**Risk**: Adding Patroni as a dependency increases operational complexity and
package size.

**Mitigation**:
- Patroni is Python-based and lightweight
- Available in RHEL/Fedora repos
- Well-documented with large community
- Alternative: `pg_auto_failover` if Patroni proves too heavy

### 9. Network Requirements
**Risk**: 2-node HA requires reliable network between nodes. Flaky connectivity
causes constant failovers.

**Mitigation**:
- Configurable timeouts in Patroni and keepalived
- Dampening: don't fail over on brief network blips
- Document network requirements (latency < 10ms, packet loss < 0.1%)

## Low Risks

### 10. CGO Cross-Compilation
**Risk**: Cross-compiling for arm64 from amd64 may fail due to CGO dependencies.

**Mitigation**:
- Kine binary has no CGO requirement (pure Go)
- MicroShift main binary already handles cross-compilation
- Use Docker/Podman buildx for clean cross-compilation environment

### 11. Backward Compatibility
**Risk**: Changes break single-node MicroShift operation.

**Mitigation**:
- All changes are behind `twoNode.enabled: true` config flag
- Default behavior unchanged (`storage.backend: etcd`)
- Existing tests continue to pass
- Feature-gated like existing multinode code

## Risk Summary Matrix

| # | Risk | Likelihood | Impact | Overall | Status |
|---|------|-----------|--------|---------|--------|
| 1 | Split-brain | Medium | Critical | High | Mitigated by Patroni failsafe + replica-only promotion |
| 2 | Kine compatibility | Low | High | Medium | Mitigated by K3s track record |
| 3 | OVN 2-node issues | Medium | Medium | Medium | Needs investigation |
| 4 | Performance | Low | Medium | Low | Benchmarking planned |
| 5 | Cert security | Low | High | Medium | Time-limited tokens |
| 6 | PG resources | Low | Medium | Low | Tuned for edge |
| 7 | Upgrade complexity | Medium | Medium | Medium | Rolling upgrade procedure |
| 8 | Patroni dependency | Low | Low | Low | Standard package (pip) |
| 9 | Network requirements | Medium | Medium | Medium | Documented requirements |
| 10 | CGO cross-compile | Low | Low | Low | Pure Go alternative |
| 11 | Backward compat | Low | High | Medium | Feature-gated |
| 12 | **Patroni watchdog reboot** | **High** | **Critical** | **Critical** | **FIXED** — `watchdog: mode: off` |
