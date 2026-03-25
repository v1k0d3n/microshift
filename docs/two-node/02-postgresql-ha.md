# Phase 2: PostgreSQL HA with Streaming Replication

## Why PostgreSQL for 2-Node HA?

PostgreSQL's **synchronous streaming replication** is one of the few distributed
database solutions that works correctly with exactly 2 nodes:

- **Synchronous mode**: Primary waits for replica to confirm writes before
  acknowledging to client → zero data loss
- **No quorum needed**: Unlike etcd's Raft, PostgreSQL replication is
  primary/replica, not consensus-based
- **Automatic failover**: With Patroni, the replica can be promoted to primary
  when the original primary fails

## Architecture

```
demo-1 (192.168.5.107)              demo-2 (192.168.7.167)
┌─────────────────────┐             ┌─────────────────────┐
│  Patroni            │             │  Patroni            │
│  ├─ PostgreSQL      │────WAL────► │  ├─ PostgreSQL      │
│  │  (primary)       │  streaming  │  │  (sync replica)  │
│  │  port: 5432      │  repl.      │  │  port: 5432      │
│  └──────────────────┘             │  └──────────────────┘
│                                   │                      │
│  Kine → localhost:5432            │  Kine → localhost:5432
└─────────────────────┘             └─────────────────────┘
```

## Component: Patroni

Patroni is the industry-standard PostgreSQL HA manager. For a 2-node setup:

### DCS (Distributed Configuration Store)
Patroni normally uses etcd/ZooKeeper/Consul as a DCS for leader election. For our
2-node setup, we use **Patroni's Raft mode** (`raft` DCS backend), which works
without an external service. With 2 nodes, Raft can detect node failures but
cannot achieve quorum alone — we handle this with:

**Option A: Patroni with `synchronous_mode: true` + `failsafe_mode: true`**
- When the primary loses contact with the replica, it continues serving
  (degraded mode, async writes)
- When the replica loses contact with the primary, it promotes itself
- Risk: split-brain. Mitigated by fencing (see below).

**Option B: Patroni with Raft DCS + witness on shared network element**
- A lightweight witness (could be a Raspberry Pi, cloud VM, or network device)
  provides the third vote for Raft consensus
- Most robust, but requires a third entity

**Option C: PostgreSQL built-in replication + pg_auto_failover**
- Simpler alternative to Patroni, designed specifically for 2-node HA
- Uses a "monitor" node (can be lightweight) for arbitration
- Alternatively, runs without monitor using `--disable-monitor` with manual
  failover

**Recommendation: Option A (Patroni + failsafe)** for simplicity, with fencing
to prevent split-brain.

### Split-Brain Prevention (Fencing)
When both nodes think they're primary:
1. **STONITH via IPMI/BMC**: If available, power-fence the other node
2. **Shared storage check**: Before accepting writes, verify you can reach a
   shared resource (e.g., a well-known IP or storage endpoint)
3. **Self-fencing**: If a node cannot confirm it should be primary, it demotes
   itself to read-only

For MicroShift's edge use case, we implement a **watchdog-based approach**:
- The Kine process on each node checks if its local PostgreSQL is primary
- If the local PostgreSQL is a replica, Kine serves reads but rejects writes
  (kube-apiserver sees "read-only" errors and retries on the other node)

## PostgreSQL Configuration

### `postgresql.conf` (key settings)
```ini
# Replication
wal_level = replica
max_wal_senders = 3
synchronous_standby_names = '*'
synchronous_commit = remote_apply    # Strongest consistency

# Connection
listen_addresses = '*'
port = 5432

# Performance (edge-optimized)
shared_buffers = 128MB
effective_cache_size = 256MB
work_mem = 4MB
maintenance_work_mem = 64MB
max_connections = 20                 # Kine uses very few connections

# WAL
wal_keep_size = 256MB
max_wal_size = 512MB

# Timeouts
wal_sender_timeout = 30s
wal_receiver_timeout = 30s
```

### `pg_hba.conf`
```
# Local connections
local   all   microshift                     peer
host    all   microshift   127.0.0.1/32      scram-sha-256

# Replication
host    replication   replicator   192.168.5.107/32   scram-sha-256
host    replication   replicator   192.168.7.167/32   scram-sha-256

# Cross-node Kine (for read-replica scenarios)
host    microshift    microshift   192.168.5.107/32   scram-sha-256
host    microshift    microshift   192.168.7.167/32   scram-sha-256
```

### Patroni Configuration (`patroni.yml`)
```yaml
scope: microshift-postgres
name: <hostname>

restapi:
  listen: 0.0.0.0:8008
  connect_address: <node_ip>:8008

raft:
  data_dir: /var/lib/microshift/patroni/raft
  self_addr: <node_ip>:8009
  partner_addrs:
    - <other_node_ip>:8009

bootstrap:
  dcs:
    ttl: 30
    loop_wait: 10
    retry_timeout: 10
    maximum_lag_on_failover: 0       # Zero data loss
    synchronous_mode: true
    failsafe_mode: true
    postgresql:
      use_pg_rewind: true
      parameters:
        wal_level: replica
        synchronous_commit: remote_apply
        synchronous_standby_names: '*'
        max_connections: 20

  initdb:
    - encoding: UTF8
    - data-checksums

  users:
    microshift:
      password: <generated>
      options:
        - createrole
        - createdb
    replicator:
      password: <generated>
      options:
        - replication

postgresql:
  listen: 0.0.0.0:5432
  connect_address: <node_ip>:5432
  data_dir: /var/lib/microshift/postgres/data
  authentication:
    superuser:
      username: postgres
      password: <generated>
    replication:
      username: replicator
      password: <generated>
```

## MicroShift Integration

### New component: PostgreSQLService
```go
// pkg/controllers/postgresql.go
type PostgreSQLService struct {
    cfg *config.Config
}

func (s *PostgreSQLService) Name() string { return "postgresql" }
func (s *PostgreSQLService) Dependencies() []string { return nil }
```

This service:
1. Initializes PostgreSQL data directory if not present
2. Starts Patroni (which manages PostgreSQL)
3. Waits for PostgreSQL to be ready (primary or replica)
4. Signals readiness

### Dependency chain update
```
postgresql → kine → kube-apiserver → (scheduler, controller-manager, ...)
```
Replaces:
```
etcd → kube-apiserver → ...
```

### Database schema
Kine manages its own schema. The database just needs to exist:
```sql
CREATE DATABASE microshift;
```
Kine creates a single table `kine` with columns for key, value, revision, etc.

## Deployment: PostgreSQL Installation

### Option 1: System package (Recommended for edge)
```bash
# Included in RHEL/Fedora repos
dnf install postgresql-server postgresql-contrib patroni
```

### Option 2: Bundled with MicroShift
Ship PostgreSQL binaries alongside MicroShift (increases package size but
simplifies deployment).

**Recommendation**: Option 1 — declare postgresql-server and patroni as RPM
dependencies of the microshift-ha package.

## Files to Create/Modify

| Action | Path | Description |
|--------|------|-------------|
| CREATE | `pkg/controllers/postgresql.go` | PostgreSQL/Patroni service manager |
| CREATE | `pkg/config/postgresql.go` | PostgreSQL configuration structs |
| CREATE | `assets/postgresql/patroni.yml.tmpl` | Patroni config template |
| CREATE | `assets/postgresql/pg_hba.conf.tmpl` | PG auth template |
| MODIFY | `pkg/cmd/run.go` | Add postgresql to service chain |
| CREATE | `packaging/rpm/microshift-ha.spec` | HA package with PG deps |
