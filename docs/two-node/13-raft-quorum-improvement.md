# Phase 9: Raft Quorum Improvement — Automatic Failover for 2-Node Clusters

## Status: COMPLETE

## Problem Statement

In a 2-node Patroni cluster using Raft DCS, the loss of the PG primary node causes a
full outage because the surviving PG replica cannot form a Raft majority (requires 2/2
nodes) and therefore cannot promote itself to primary.

This was confirmed during failover validation (Phase 8, Test 3): when Patroni on the
PG primary was stopped, the surviving replica logged `ERROR: Error communicating with DCS`
indefinitely and never promoted.

The `failsafe_mode: true` setting only protects an existing primary from being demoted —
it does not enable a replica to self-promote.

### Impact

The cluster has **asymmetric failure tolerance**:
- Loss of PG replica node: cluster survives (~5s blip) — `failsafe_mode` keeps the primary running
- Loss of PG primary node: **full outage** until the primary returns or manual intervention

This means the effective availability depends on *which* node fails, which is
unacceptable for a production HA system.

## Requirements

1. When one node in a 2-node cluster becomes unreachable, the surviving node MUST
   be able to assume the PG primary role and continue serving API requests.
2. The promotion MUST NOT happen during brief network blips (must have dampening).
3. Split-brain MUST be prevented — at most one node may be the PG primary at any time.
4. The solution MUST work without adding a third physical node.
5. Recovery when the failed node returns MUST be automatic.

## Solution Options

### Option A: MicroShift Watchdog with `patronictl failover`

**Approach**: Add a watchdog goroutine to MicroShift (or a sidecar) that monitors the
peer's health. When the peer is confirmed unreachable for a configurable duration,
the watchdog runs `patronictl failover` with `--force` to bypass Raft quorum and
promote the local replica.

**Implementation**:
- New controller: `pkg/controllers/failover_watchdog.go`
- Monitors peer health via Patroni REST API (`GET http://<peer>:8008/patroni`)
- Configurable thresholds: e.g., 5 consecutive failures over 30 seconds
- Runs `patronictl failover --master <old-primary> --candidate <local> --force`
- Logs all decisions for audit trail

**Pros**:
- No changes to Patroni itself
- Fully within MicroShift's control
- Simple to understand and debug
- `patronictl failover --force` is a supported Patroni operation

**Cons**:
- Race condition risk: both nodes might try to force-failover simultaneously during
  a network partition (split-brain)
- Requires `patronictl` to be installed (Python dependency)
- `--force` bypasses safety checks — must be used carefully

**Split-brain mitigation**: Only the node that is currently the PG replica should attempt
promotion. The PG primary should use `failsafe_mode` to keep running. Since the replica
is read-only, even if both nodes think they should act, only the replica's `patronictl
failover` would change anything.

### Option B: Patroni REST API Failover

**Approach**: Similar to Option A, but use Patroni's REST API instead of CLI.

**Implementation**:
- `POST http://localhost:8008/failover` with `{"leader": "<old-primary>", "candidate": "<local>"}`
- Patroni REST API supports failover operations
- Watchdog implemented as HTTP calls from MicroShift

**Pros**:
- No dependency on `patronictl` CLI
- RESTful — can be called from Go without shelling out
- Same safety model as Option A

**Cons**:
- Same split-brain risk as Option A
- The REST API may still require Raft quorum for failover (needs testing)

### Option C: Direct PostgreSQL Promotion (Bypass Patroni)

**Approach**: When the peer is unreachable, directly promote the local PostgreSQL
replica using `pg_ctl promote` or `SELECT pg_promote()`, bypassing Patroni entirely.
Then notify Patroni of the new state.

**Implementation**:
- Watchdog detects peer is down
- Runs `sudo -u postgres pg_ctl promote -D /var/lib/microshift/postgres/data`
- PostgreSQL transitions from recovery to primary mode
- Patroni eventually detects the change and updates its state

**Pros**:
- Fastest promotion path — no Patroni overhead
- Works even if Patroni is in a bad state
- No Python/patronictl dependency

**Cons**:
- Patroni and PostgreSQL may disagree on who is primary
- Patroni might try to demote the node back when it regains DCS access
- Most likely to cause split-brain if both nodes promote
- Requires careful Patroni config to prevent state conflicts

### Option D: Lightweight Witness / Raft Arbiter

**Approach**: Run a third Raft voter on a lightweight process (no PostgreSQL data)
to provide the quorum tie-breaker. This could run on:
- A small VM or container on the same network
- One of the existing nodes as a separate Patroni process (virtual 3rd member)
- An external service (e.g., the deployment workstation)

**Implementation**:
- Deploy a Patroni instance with `nofailover: true`, `noloadbalance: true`, `nosync: true`
  (it never holds PG data, just votes in Raft)
- Configure both real nodes' `partner_addrs` to include the witness
- With 3 Raft voters, losing 1 node still leaves a 2/3 majority

**Pros**:
- Eliminates the quorum problem entirely
- Standard Patroni pattern (well-documented)
- No custom code needed — just configuration
- No split-brain risk (Raft guarantees single leader with 3 voters)

**Cons**:
- Requires a third endpoint — may not be available in all edge deployments
- Adds operational complexity (third thing to monitor)
- The witness is a single point of failure for the quorum (though not for data)
- Contradicts the "2-node" design goal

### Option E: Patroni DCS Bypass with Fencing (STONITH-style)

**Approach**: When the peer is unreachable, the surviving node fences the failed node
(e.g., via IPMI, cloud API, or SSH power-off) to guarantee it's truly down, then
promotes itself.

**Implementation**:
- MicroShift watchdog detects peer is down
- Executes fencing agent to power off / isolate the failed node
- Once fencing confirms the peer is dead, runs `patronictl failover --force`
- On recovery, the fenced node is powered back on and rejoins as replica

**Pros**:
- Strongest split-brain protection — the failed node is guaranteed dead
- Standard HA pattern (used by Pacemaker/Corosync for decades)
- Works for both PG primary and PG replica failure

**Cons**:
- Requires fencing hardware/API (IPMI, cloud provider API, smart PDU)
- Not available in all edge environments
- Adds significant operational complexity
- Fencing itself can fail

## Chosen Approach: Option C (Direct PostgreSQL Promotion)

After evaluating all options, **Option C** was implemented instead of the originally
recommended Option A. The key reasons:

1. **No Python/patronictl dependency** — `pg_ctl promote` is a PostgreSQL binary,
   always available where PG is installed
2. **Fastest promotion path** — bypasses Patroni's Raft DCS (which has no quorum anyway)
3. **Works even when Patroni is in a bad state** — promotes PG directly at the database level
4. **Patroni detects the promotion** and updates its internal state when the DCS recovers

The concern about Patroni disagreeing with PG state was mitigated by:
- `failsafe_mode: true` prevents Patroni from demoting a running primary
- The watchdog verifies PG is writable after promotion via `SELECT pg_is_in_recovery()`
- Patroni eventually recognizes the new primary when the peer returns and Raft quorum is restored

### Additional Discovery: Patroni Watchdog Reboot (2026-03-25)

During live testing, rebooting the peer node caused the **surviving node to also reboot**.
Root cause: Patroni's built-in Linux watchdog feature (`/dev/watchdog`). When Raft quorum
is lost, Patroni stops petting the kernel watchdog device, which triggers a hard reboot.
This is designed for 3+ node clusters as a split-brain fencing mechanism, but is catastrophic
in a 2-node setup where the surviving node should keep running.

**Fix**: Added `watchdog: mode: off` to both Patroni configs in `deploy-two-node.sh`.
Our failover watchdog handles promotion correctly without needing Patroni's self-fencing.

## Implementation Details

### Architecture (as implemented)

```
┌──────────────────────────┐
│     MicroShift Node      │
│                          │
│  ┌────────────────────┐  │
│  │  Failover Watchdog  │  │
│  │  (goroutine)        │  │
│  │                     │  │
│  │  1. Poll peer:8008  │──────► GET http://<peer>:8008/patroni
│  │  2. Track failures  │
│  │     (persisted to   │
│  │      disk across    │
│  │      restarts)      │
│  │  3. If threshold    │
│  │     exceeded AND    │
│  │     I am replica:   │
│  │     pg_ctl promote  │──────► sudo -u postgres pg_ctl promote -D <datadir>
│  │  4. Verify PG is    │
│  │     writable        │──────► SELECT pg_is_in_recovery()
│  └────────────────────┘  │
│                          │
│  Pre-check (before       │
│  services start):        │
│  RunFailoverPreCheck()   │──────► Same logic, runs synchronously at startup
│                          │
└──────────────────────────┘
```

### Key Design: Persistent Failure Count

A critical insight: when the PG primary goes down, Kine loses its database and crashes,
causing MicroShift to restart. Without persistence, the watchdog would reset its failure
count to 0 on each restart and **never** reach the promotion threshold.

The watchdog persists its failure count to `/var/lib/microshift/failover-watchdog-state`.
Each MicroShift restart increments the count. When the count reaches the threshold
(default: 5), the pre-check promotes PG before Kine starts, so Kine connects to the
now-local primary.

### Configuration (as implemented)

```yaml
twoNode:
  enabled: true
  failover:
    # Enable automatic failover when peer is unreachable.
    # Default: true when twoNode.enabled is true.
    enabled: true

    # Number of consecutive health check failures before triggering failover.
    # In pre-check mode, this is failures across MicroShift restarts.
    # Default: 5 (stays within systemd's StartLimitBurst)
    failureThreshold: 5

    # Interval between peer health checks (runtime watchdog only).
    # Default: 5s
    checkInterval: 5s
```

### Watchdog State Machine (as implemented)

```
                ┌──────────┐
                │  HEALTHY │ ◄── peer Patroni responds (HTTP 200)
                │          │     reset failure count, clear state file
                └────┬─────┘
                     │ peer unreachable
                     ▼
                ┌──────────┐
                │ DEGRADED │ ◄── increment failure count
                │          │     persist to disk
                └────┬─────┘
                     │ failures >= threshold (default: 5)
                     │ AND local Patroni role == "replica"
                     ▼
                ┌──────────┐
                │PROMOTING │ ◄── pg_ctl promote -D /var/lib/microshift/postgres/data
                │          │     wait 5s for PG to finish promotion
                │          │     verify via Patroni API or pg_is_in_recovery()
                └────┬─────┘
                     │ PG writable confirmed
                     ▼
                ┌──────────┐
                │ PROMOTED │ ◄── clear state file
                │          │     continue monitoring for peer recovery
                └──────────┘
```

### Safety Invariants

1. **Only the replica promotes**: The watchdog checks the local Patroni role before
   acting. If the local node is already the PG primary (or thinks it is), it does
   nothing — `failsafe_mode` handles that case.

2. **Dampening**: The default threshold of 5 failures prevents promotion during
   brief network blips. With the pre-check model, each failure = one MicroShift
   restart, so 5 failures ≈ 5 restart cycles.

3. **Dual verification**: After `pg_ctl promote`, the watchdog checks both the
   Patroni REST API role AND `pg_is_in_recovery()` directly. If Patroni's DCS
   is down (expected), the direct PG check serves as the authority.

4. **Synchronous replication guarantee**: Because `synchronous_mode: true` is
   configured, the replica has all committed data from the primary. Promotion
   loses zero transactions.

5. **Patroni watchdog disabled**: `watchdog: mode: off` in Patroni config prevents
   the kernel from rebooting the surviving node when Raft quorum is lost.

### Files Created / Modified

| File | Change |
|------|--------|
| `pkg/controllers/failover_watchdog.go` | **NEW**: Watchdog controller with state machine, pre-check, persistent failure count, `pg_ctl promote`, dual verification |
| `pkg/config/twonode.go` | **MODIFIED**: Added `FailoverConfig` struct with `Enabled`, `FailureThreshold`, `CheckInterval` fields; defaults; validation |
| `pkg/config/config.go` | **MODIFIED**: Wire defaults and validation for failover config |
| `pkg/cmd/run.go` | **MODIFIED**: Register watchdog controller; call `RunFailoverPreCheck()` before services start |
| `scripts/deploy-two-node.sh` | **MODIFIED**: Added `watchdog: mode: off` to both Patroni configs; increased systemd `StartLimitBurst=10` to allow enough restarts for the pre-check to reach the promotion threshold |

### Resolved Open Questions

1. **Patroni running but PG stopped**: The watchdog checks the Patroni REST API which
   reflects PG state. If PG is stopped, Patroni returns a non-200 status.

2. **`synchronous_mode_strict`**: Kept at `false` — this allows the primary to continue
   accepting writes when the replica is down, which is the correct behavior for failover.

3. **Controller vs. separate service**: Implemented as a MicroShift controller (goroutine)
   with a **pre-check** that runs before services start. This gives the best of both
   approaches: the pre-check promotes before Kine tries to connect, and the runtime
   watchdog handles failures during normal operation.

4. **patronictl not needed**: By using `pg_ctl promote` directly (Option C), there is
   no dependency on `patronictl` or Python.
