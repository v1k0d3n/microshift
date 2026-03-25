# Phase 7: Implementation Roadmap

## Overview
The implementation is broken into 10 phases, each building on the previous one.
Each phase produces a testable, working state.

---

## Phase 1: Configuration and Scaffolding — COMPLETE
**Goal**: Add 2-node configuration structs and CLI commands without changing runtime behavior.

### Tasks
- [x] 1.1 Create `pkg/config/twonode.go` with `TwoNodeConfig` struct
- [x] 1.2 Add `PostgreSQLConfig` struct to `pkg/config/storage.go`
- [x] 1.3 Add `TwoNode` and `Storage.Backend` fields to `Config` in `pkg/config/config.go`
- [x] 1.4 Add validation for new config fields
- [x] 1.5 Update `show-config` command to display new fields
- [x] 1.6 Create `pkg/cmd/initcluster.go` (stub)
- [x] 1.7 Create `pkg/cmd/joincluster.go` (stub)
- [x] 1.8 Register new commands in `cmd/microshift/main.go`
- [x] 1.9 Update `.gitignore` to exclude `.claude/`

**Verification**: `microshift show-config` displays twoNode section. Build succeeds.

---

## Phase 2: Kine Binary and Storage Abstraction — COMPLETE
**Goal**: Build `microshift-kine` binary and abstract storage backend selection.

### Tasks
- [x] 2.1 Create `kine/go.mod` with Kine dependency
- [x] 2.2 Create `kine/cmd/microshift-kine/main.go`
- [x] 2.3 Create `kine/cmd/microshift-kine/run.go` — Kine startup with PG backend
- [x] 2.4 Create `kine/cmd/microshift-kine/version.go`
- [x] 2.5 Refactor `pkg/controllers/etcd.go` → add `StorageBackend` interface
- [x] 2.6 Create `pkg/controllers/kine.go` — KineService implementation
- [x] 2.7 Modify `pkg/cmd/run.go` to select storage backend from config
- [x] 2.8 Add `kine` target to Makefile
- [x] 2.9 Test: MicroShift starts with `storage.backend: kine` against a local PostgreSQL

**Verification**: MicroShift boots with Kine + local PostgreSQL. kubectl works.

---

## Phase 3: PostgreSQL HA Setup — COMPLETE
**Goal**: Configure PostgreSQL streaming replication between two nodes.

### Tasks
- [x] 3.1 Create `pkg/controllers/postgresql.go` — PostgreSQLService
- [x] 3.2 Create `assets/postgresql/patroni.yml.tmpl`
- [x] 3.3 Implement `init-cluster` command (password gen, CA bundling, join token)
- [x] 3.4 Implement `join-cluster` command (token decode, CA/SA key write, password write)
- [x] 3.5 Add VIP to API server external certificate SANs
- [x] 3.6 Test: PostgreSQL replication between demo-1 and demo-2

**Verification**: Data written on primary appears on replica.

---

## Phase 4: Certificate Distribution and Control Plane HA — COMPLETE
**Goal**: Both nodes run full control plane with shared certificates.

### Tasks
- [x] 4.1 Modify certificate generation to add VIP to SANs
- [x] 4.2 Implement CA distribution during `join-cluster`
- [x] 4.3 Generate per-node certificates on joining node
- [x] 4.4 Enable leader election for controller-manager and scheduler (`cfg.TwoNode.Enabled`)
- [x] 4.5 Configure kube-apiserver endpoint reconciler for multi-node (`endpoint-reconciler-type=lease`, `apiserver-count=2`)
- [x] 4.6 Ensure service account signing keys are shared
- [x] 4.7 Test: Both nodes run kube-apiserver. oc works against either.

**Verification**: `oc get nodes` shows 2 nodes. Leader election works.

---

## Phase 5: VIP and Networking — COMPLETE
**Goal**: Floating VIP and proper network configuration for 2-node.

### Tasks
- [x] 5.1 Create `pkg/controllers/vip.go` — VIPService with keepalived
- [x] 5.2 Create keepalived config template
- [x] 5.3 Create API server health check script
- [x] 5.4 Add VIPService to service dependency chain
- [x] 5.5 Add firewall rules for inter-node communication
- [x] 5.6 Add VIP and peer to no-proxy list

**Verification**: VIP binds to the correct interface.

---

## Phase 6: Cross-Build, Testing, and Polish — COMPLETE
**Goal**: Produce x86_64 and arm64 binaries. End-to-end deploy script.

### Tasks
- [x] 6.1 Update Makefile for cross-building all three binaries
- [x] 6.2 Verify cross-compilation for both architectures (6 binaries produced)
- [x] 6.3 Create `scripts/deploy-two-node.sh` (12-step automated deployment)
- [x] 6.4 Write user guide
- [x] 6.5 Patch Kine SQLite nocgo stub for arm64 cross-compilation

**Verification**: All 6 binaries compile. Deploy script runs end-to-end.

---

## Phase 7: Runtime Integration — COMPLETE
**Goal**: Fix all runtime issues discovered during live testing on demo-1/demo-2.

### Tasks
- [x] 7.1 Fix deploy script SSH/sudo/SELinux/systemd issues (10 issues)
- [x] 7.2 Architecture pivot: Patroni managed externally via systemd (K3s model)
- [x] 7.3 Fix Patroni solo Raft bootstrap and synchronous_mode_strict
- [x] 7.4 Fix stale leaf certs after join-cluster
- [x] 7.5 Fix PostgreSQL user/database creation (Step 8b in deploy script)
- [x] 7.6 Fix etcd serving cert missing `127.0.0.1` SAN (`pkg/cmd/init.go`)
- [x] 7.7 Fix Kine health check using wrong client (`getEtcdClient` → `getKineClient`)
- [x] 7.8 Fix Kine panic: `rand.Int63n(0)` — set `NotifyInterval: 10 * time.Minute`
- [x] 7.9 Fix IPv6 localhost resolution (`localhost` → `127.0.0.1` in kube-apiserver.go, kine.go)
- [x] 7.10 Add Patroni REST API discovery so both Kines connect to PG primary
- [x] 7.11 Add `containernetworking-plugins` to deploy script prerequisites
- [x] 7.12 Install CRI-O drop-in config for pull secret auth

**Verification**: `oc get nodes` shows 2 Ready nodes. All pods running. 21/21 issues resolved.

**Detailed log**: See [10-runtime-integration.md](10-runtime-integration.md)

---

## Phase 8: Failover Validation — COMPLETE
**Goal**: Validate failover scenarios and HA behavior.

### Tasks
- [x] 8.1 Test: Cluster baseline — both nodes registered, both API servers healthy
- [x] 8.2 Test: Stop MicroShift on PG primary — other node survives (PASS)
- [x] 8.3 Test: Stop Patroni on PG primary — Raft quorum limitation identified (PARTIAL)
- [x] 8.4 Test: Full node failure with PG primary surviving (PASS — ~5s blip)
- [x] 8.5 Test: Recovery — failed node rejoins as PG replica (PASS)

**Verification**: Cluster survives MicroShift failure and full node failure (when PG primary survives).
Raft quorum limitation prevents replica self-promotion — documented with recommended fix.

**Detailed results**: See [12-failover-validation.md](12-failover-validation.md)

---

## Phase 9: Raft Quorum Improvement — COMPLETE
**Goal**: Enable automatic failover when the PG primary node goes down.

### Tasks
- [x] 9.1 Add `FailoverConfig` to `TwoNodeConfig` (Enabled, FailureThreshold, CheckInterval)
- [x] 9.2 Implement failover watchdog controller (`pkg/controllers/failover_watchdog.go`)
- [x] 9.3 Implement `pg_ctl promote` execution path (Option C chosen over patronictl)
- [x] 9.4 Add promotion verification (Patroni API + `pg_is_in_recovery()` direct check)
- [x] 9.5 Add persistent failure count across MicroShift restarts
- [x] 9.6 Add `RunFailoverPreCheck()` — promotes before Kine starts
- [x] 9.7 Increase systemd `StartLimitBurst=10` for restart-based failure accumulation
- [x] 9.8 Disable Patroni's built-in watchdog (`watchdog: mode: off`) — prevents surviving node from rebooting when Raft quorum is lost
- [ ] 9.9 Test: Stop Patroni on PG primary → automatic promotion
- [ ] 9.10 Test: Brief network blip → no false promotion
- [ ] 9.11 Test: Double failure recovery → no split-brain

**Verification**: Watchdog code complete and deployed. Live failover testing pending.

**Key discovery**: Patroni's built-in Linux watchdog (`/dev/watchdog`) caused the surviving
node to reboot when the peer went down — completely defeating HA. Fixed by setting
`watchdog: mode: off` in Patroni configs.

**Detailed design**: See [13-raft-quorum-improvement.md](13-raft-quorum-improvement.md)

---

## Phase 10: Polish and Documentation — IN PROGRESS
**Goal**: Production-ready documentation and deploy tooling.

### Tasks
- [x] 10.1 Update user guide with failover watchdog and Patroni watchdog fix
- [x] 10.2 Update all plan documents to reflect current implementation status
- [x] 10.3 Document Patroni watchdog reboot discovery and fix
- [ ] 10.4 Add optional `--pull-secret` and `--build` flags to deploy script
- [ ] 10.5 Test VIP/keepalived failover end-to-end
- [ ] 10.6 Final end-to-end test from clean state
- [ ] 10.7 Create troubleshooting runbook from real issues found

**Verification**: Fresh user can deploy 2-node cluster from scratch using only the documentation.

---

## Dependency Graph

```
Phase 1 (Config) ──► Phase 2 (Kine) ──► Phase 3 (PostgreSQL HA)
                                              │
                                              ▼
                     Phase 4 (Certs + CP HA) ──► Phase 5 (VIP + Net)
                                                       │
                                                       ▼
                                              Phase 6 (Build + Test)
                                                       │
                                                       ▼
                                              Phase 7 (Runtime Integration) ✓
                                                       │
                                                       ▼
                                              Phase 8 (Failover Validation) ✓
                                                       │
                                                       ▼
                                              Phase 9 (Raft Quorum) ✓
                                                       │
                                                       ▼
                                              Phase 10 (Polish + Docs) ◄── CURRENT
```
