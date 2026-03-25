# Phase 7b: Architecture Pivot — K3s Lessons Learned

## Key Discovery

Research into K3s's Kine+PostgreSQL implementation revealed that K3s:

1. **Does NOT manage PostgreSQL lifecycle** — PostgreSQL must be running before K3s starts
2. **Embeds Kine in-process** — no separate binary; Kine runs inside the K3s server
3. **Kine auto-creates the database** — calls `CREATE DATABASE IF NOT EXISTS` on startup
4. **No Patroni** — PostgreSQL HA is the user's responsibility
5. **Bootstrap data stored in database** — CA certs, keys, tokens are stored in the Kine KV store; no file-based join token needed for CA distribution
6. **Multi-server is simple** — each server connects to the same PG and reads bootstrap data from the database

## Implications for MicroShift 2-Node HA

### What We Should Change

**Remove `PostgreSQLService` controller.** MicroShift should NOT manage PostgreSQL.
Instead, the user is responsible for having a working PostgreSQL instance (just like
K3s). The deploy script helps set this up, but MicroShift itself is a pure client.

**Keep `microshift-kine` as a separate binary** (MicroShift's architecture uses
separate processes for isolation, unlike K3s which is monolithic). But Kine should
auto-create the database on connect — no need for `ensureDatabase()`.

**Simplify the Patroni setup** — Patroni becomes part of the deploy script / user
guide, not managed by MicroShift's service manager. Patroni is started via systemd
as a standalone service, separate from MicroShift.

### Revised Architecture

```
User's responsibility (before MicroShift starts):
  ┌─────────────────────────────────┐
  │  PostgreSQL + Patroni           │
  │  (managed via systemd, NOT by   │
  │   MicroShift)                   │
  └────────────┬────────────────────┘
               │
MicroShift's responsibility:
  ┌────────────▼────────────────────┐
  │  microshift-kine                │
  │  (connects to existing PG)      │
  │  (auto-creates database)        │
  └────────────┬────────────────────┘
               │
  ┌────────────▼────────────────────┐
  │  kube-apiserver                 │
  │  (connects to Kine on :2379)    │
  └─────────────────────────────────┘
```

### Revised Deploy Script Flow

1. Install PostgreSQL + Patroni on both nodes (as systemd services)
2. Start Patroni on primary → bootstraps PG
3. Start Patroni on secondary → joins as streaming replica
4. Verify PG is running on both nodes
5. Start MicroShift on primary → Kine connects to PG, auto-creates DB
6. Run init-cluster → generate join token
7. Run join-cluster on secondary
8. Start MicroShift on secondary → Kine connects to PG, finds DB already exists

### What This Fixes

- **No more `PostgreSQLService` crashing MicroShift** — Patroni runs independently
- **No more Raft quorum chicken-and-egg** — Patroni is started before MicroShift
- **No more PG user/database creation issues** — Kine handles it (or Patroni bootstrap does)
- **No more PG ownership/SELinux issues in MicroShift** — MicroShift never touches PG data dirs
- **Simpler code** — remove postgresql.go, simplify run.go

### Files to Change

| Action | File | Description |
|--------|------|-------------|
| REMOVE | `pkg/controllers/postgresql.go` | No longer needed |
| MODIFY | `pkg/cmd/run.go` | Remove PostgreSQLService from service chain |
| MODIFY | `scripts/deploy-two-node.sh` | Start Patroni via systemd before MicroShift |
| CREATE | `packaging/systemd/microshift-patroni@.service` | Patroni systemd unit template |
| MODIFY | `kine/cmd/microshift-kine/run.go` | Add database auto-creation |
| MODIFY | `10-runtime-integration.md` | Update with new approach |
