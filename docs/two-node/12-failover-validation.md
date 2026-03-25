# Phase 8: Failover Validation Results

## Status: COMPLETE

This phase covers live failover testing on the 2-node HA cluster (demo-1 / demo-2)
after all runtime integration issues (Phase 7) were resolved.

## Test Environment

| Node | IP | Patroni Role (at test start) |
|------|----|------|
| demo-1 | 192.168.5.107 | PG primary (promoted during Phase 7 recovery) |
| demo-2 | 192.168.7.167 | PG replica |

Both nodes: Fedora 43, MicroShift 4.21.0-two-node, Kine v0.13.6, Patroni v4.1.0
with Raft DCS, PostgreSQL with synchronous streaming replication.

## Test Results

### Test 1: Cluster Baseline — PASS

**Action**: Query the cluster via `oc` after both nodes are running.

**Result**:
- Both nodes registered: `demo-1.novalocal` and `demo-2.novalocal`
- Both API servers: `healthz=ok`, `readyz=ok`
- DaemonSet pods scheduled to both nodes (ImagePullBackOff expected — community build without pull secret)
- Shared storage working: objects created on one API server visible from the other

### Test 2: MicroShift Failure on One Node — PASS

**Action**: `systemctl stop microshift` on demo-2 (PG primary). Patroni left running.

**Result**:
- demo-1 API server immediately healthy, no disruption
- `oc get nodes` works through demo-1, shows both nodes
- demo-2 API server unreachable (expected)
- No Kine disruption — demo-1's Kine still reaches PG on demo-2 because Patroni/PG are independent services

**Recovery**: `systemctl start microshift` on demo-2 — came back in ~10s, both nodes healthy.

**Conclusion**: Control plane survives the loss of one MicroShift instance. The Patroni/PG
independence model (K3s-style) pays off here — the database layer is unaffected by
MicroShift failures.

### Test 3: Patroni Failover (Stop Patroni on PG Primary) — PARTIAL PASS

**Action**: `systemctl stop microshift-patroni` on demo-2 (PG primary at the time).

**Result**:
- demo-1 (PG replica) could NOT self-promote to PG primary
- Patroni logs on demo-1: `ERROR: Error communicating with DCS` — Raft quorum lost (1/2 nodes)
- Both MicroShift instances failed: Kine lost PG connectivity
- After restarting Patroni on demo-2 (restoring Raft quorum), demo-1 was promoted to PG primary
- Both MicroShift instances recovered after manual restart

**Root Cause**: Patroni Raft DCS requires 2/2 nodes for quorum. A single surviving node
cannot elect itself as leader — this is a fundamental property of Raft consensus.

**Conclusion**: Automatic Patroni failover does not work when the peer is completely
unreachable. The `failsafe_mode: true` setting only protects an *existing* primary
from being demoted — it does not promote a replica. See Phase 9 plan (13-raft-quorum-improvement.md)
for proposed solutions.

### Test 4: Full Node Failure (PG Primary Survives) — PASS

**Action**: Stop both MicroShift and Patroni on demo-2. demo-1 is now the PG primary.

**Result**:
- demo-1 API server: `healthz=ok` for the first ~15 seconds
- Brief disruption at ~20s: Patroni lost Raft quorum, demoted itself from "primary" to "replica"
  in Patroni's view. However, `failsafe_mode: true` kept PostgreSQL writable.
- API server recovered to `healthz=ok` within ~5 seconds of the blip
- `oc get nodes` continued to work throughout

**Key Insight**: When the **PG primary survives**, `failsafe_mode` keeps the cluster operational
even without Raft quorum. The ~5s disruption is caused by:
1. Patroni's internal role change (primary → replica in Raft terms)
2. `synchronous_commit: remote_apply` briefly blocking writes until PostgreSQL falls back
   (enabled by `synchronous_mode_strict: false`)

**Conclusion**: The cluster survives full peer failure **when the PG primary is the surviving node**.
Recovery time: ~5 seconds. This is the "good" failure mode.

### Test 5: Node Recovery — PASS

**Action**: Restart Patroni and MicroShift on demo-2 after Test 4.

**Result**:
- Patroni on demo-2 rejoined as PG replica, streaming replication re-established
- demo-1's MicroShift briefly restarted during Patroni role re-negotiation
- Both nodes returned to full health within ~30s
- `oc get nodes` shows both nodes

**Conclusion**: Recovery after a full node failure works correctly. The brief disruption on
the surviving node during re-negotiation is a minor issue — Patroni changing roles causes
a PG configuration reload that briefly interrupts connections.

### Test 6: Peer Reboot Causes Surviving Node Reboot — DISCOVERED + FIXED

**Action**: Rebooted demo-2 (192.168.7.167) from a separate SSH session. Observed
behavior on the surviving node (192.168.5.105).

**Result**:
- The surviving node **also rebooted** — completely unexpected
- Both nodes went down simultaneously, defeating the entire HA purpose

**Root Cause**: Patroni's built-in Linux watchdog feature. When the peer reboots,
Patroni's 2-node Raft cluster loses quorum. The surviving Patroni cannot maintain
its leader lock, so it stops petting the `/dev/watchdog` kernel device. The kernel's
watchdog timer expires and triggers a hard reboot.

This is a fencing mechanism designed for 3+ node clusters where losing quorum means
the node is likely network-isolated and should fence itself to prevent split-brain.
In a 2-node setup, losing quorum is the *normal* failure mode — self-fencing is
counterproductive.

**Fix**:
- Added `watchdog: mode: off` to both Patroni configs in `deploy-two-node.sh`
- For already-deployed clusters: edit `/var/lib/microshift/patroni/patroni.yml` on
  each node and add `watchdog: mode: off` before the `tags:` section, then restart
  Patroni

**Conclusion**: Patroni's default watchdog behavior is **incompatible with 2-node HA**.
Must be explicitly disabled. Our MicroShift failover watchdog handles the promotion
path without needing Patroni's self-fencing.

## Summary Matrix

| Test | Scenario | Result | Disruption |
|------|----------|--------|------------|
| 1 | Baseline | PASS | None |
| 2 | MicroShift failure (one node) | PASS | None |
| 3 | Patroni failover (PG primary killed) | PARTIAL | Full outage until Raft quorum restored |
| 4 | Full node failure (PG primary survives) | PASS | ~5s blip |
| 5 | Node recovery | PASS | ~10s during re-negotiation |
| 6 | Peer reboot (Patroni watchdog) | **FIXED** | Both nodes rebooted → fixed with `watchdog: mode: off` |

## Failure Mode Analysis

```
                         PG Primary         PG Replica
                         Survives           Survives
                     ┌─────────────────┬─────────────────┐
MicroShift only down │  PASS (0s)      │  PASS (0s)      │
                     │  Test 2          │  (symmetric)     │
                     ├─────────────────┼─────────────────┤
Full node down       │  PASS (~5s)     │  FAIL (blocked)  │
(MicroShift+Patroni) │  Test 4          │  Test 3          │
                     │  failsafe_mode   │  No Raft quorum  │
                     │  keeps PG up     │  can't promote   │
                     └─────────────────┴─────────────────┘
```

The asymmetry in the "Full node down" row is the key issue to address in Phase 9.
When the PG replica is the only survivor, it cannot self-promote because Raft requires
a majority. This means **which node fails matters** — the cluster can only tolerate
the loss of the PG replica node, not the PG primary.

## Recommendations

1. **CRITICAL**: Ensure `watchdog: mode: off` is set in all Patroni configs. Without this,
   losing one node causes both nodes to reboot (Test 6).

2. ~~**Short-term**: Document the asymmetric failure behavior.~~ **DONE** — Phase 9 implemented
   the failover watchdog with `pg_ctl promote` to handle the PG replica survivor case.

3. ~~**Medium-term**: Implement automatic self-promotion.~~ **DONE** — See `13-raft-quorum-improvement.md`.
   The watchdog accumulates failures across MicroShift restarts and promotes via `pg_ctl promote`
   when the threshold is reached.

4. **Long-term**: Consider adding a lightweight witness node (no PG data, just a Raft voter)
   to provide the third quorum vote. This eliminates the asymmetry entirely and allows
   Patroni to handle failover natively.
