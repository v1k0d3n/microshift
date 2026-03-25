# Phase 1: Kine Integration as Storage Backend

## What is Kine?
Kine (`github.com/k3s-io/kine`) provides an etcd-compatible gRPC server backed by
relational databases (PostgreSQL, MySQL, SQLite) or other KV stores. It implements
the subset of the etcd v3 API that Kubernetes actually uses:
- `Range` (Get/List)
- `Put`
- `DeleteRange`
- `Txn` (Compare-and-Swap for leader election)
- `Compact`
- `Watch`

## Integration Strategy

### Option A: Replace microshift-etcd binary with microshift-kine (Recommended)
MicroShift already runs etcd as a separate process (`microshift-etcd`). We follow
the same pattern:

1. Create a new binary `microshift-kine` in `kine/cmd/microshift-kine/`
2. Modify `pkg/controllers/etcd.go` → `pkg/controllers/storage.go` to launch
   either `microshift-etcd` or `microshift-kine` based on configuration
3. kube-apiserver connects to Kine's gRPC endpoint the same way it would connect
   to etcd (Kine speaks etcd protocol)

### Option B: Embed Kine in-process
Run Kine as a library inside the main MicroShift process. Simpler but less isolated.

**We choose Option A** to match existing patterns and maintain process isolation.

## Code Changes Required

### 1. New module: `kine/`
Mirror the existing `etcd/` directory structure:

```
kine/
├── cmd/
│   └── microshift-kine/
│       ├── main.go          # Entry point
│       ├── run.go           # Kine server startup
│       └── version.go       # Version info
├── go.mod                   # Separate module (like etcd/)
└── go.sum
```

**`kine/go.mod` key dependencies:**
```go
module github.com/openshift/microshift/kine

go 1.24

require (
    github.com/k3s-io/kine v0.13.x
    github.com/openshift/microshift v0.0.0
    github.com/lib/pq v1.10.x  // PostgreSQL driver
)

replace github.com/openshift/microshift => ../
```

### 2. `kine/cmd/microshift-kine/run.go`
```go
// Core startup logic:
// 1. Read MicroShift config for database connection string
// 2. Start Kine with PostgreSQL endpoint
// 3. Listen on localhost:2379 (same port as etcd for compatibility)
// 4. TLS configuration using same etcd certificates

func RunKine(ctx context.Context, cfg *config.Config) error {
    endpoint := fmt.Sprintf(
        "postgres://%s:%s@%s:5432/microshift?sslmode=verify-full",
        cfg.Storage.PostgreSQL.User,
        cfg.Storage.PostgreSQL.Password,
        cfg.Storage.PostgreSQL.Host,
    )

    listener, err := tls.Listen("tcp", "localhost:2379", tlsConfig)
    // ... start Kine gRPC server on this listener
}
```

### 3. Modify `pkg/controllers/storage.go` (renamed from etcd.go)
```go
// StorageService wraps either etcd or kine based on config
type StorageService struct {
    cfg *config.Config
}

func (s *StorageService) Run(ctx context.Context, ready, stopped chan<- struct{}) error {
    switch s.cfg.Storage.Backend {
    case "etcd":
        return s.runEtcd(ctx, ready, stopped)
    case "kine":
        return s.runKine(ctx, ready, stopped)
    default:
        return fmt.Errorf("unsupported storage backend: %s", s.cfg.Storage.Backend)
    }
}
```

### 4. Kine ↔ kube-apiserver compatibility
Kine listens on the **same port (2379)** and speaks **the same etcd v3 gRPC
protocol**. The kube-apiserver configuration does NOT need to change:
- `--etcd-servers=https://localhost:2379` (unchanged)
- `--etcd-cafile`, `--etcd-certfile`, `--etcd-keyfile` (unchanged)

The only change is what process is behind that port.

### 5. Health checks
Kine supports the same etcd health endpoints. The existing health check in
`checkIfEtcdIsReady()` should work as-is since it uses the etcd client library
to call `Status()` and `Get()`.

## Kine PostgreSQL Connection String Format
```
postgres://user:password@host:5432/dbname?sslmode=verify-full&sslcert=...&sslkey=...&sslrootcert=...
```

## Performance Considerations
- Kine adds ~1-2ms latency per operation compared to native etcd
- For a 2-node MicroShift cluster, this is negligible
- PostgreSQL is optimized for this exact workload pattern (transactional reads/writes)
- K3s runs production clusters with 100+ nodes on Kine+PostgreSQL

## Migration Path
- New installations: Configure `storage.backend: kine` and PostgreSQL connection
- Existing installations: Data migration tool (etcd → PostgreSQL) as future work
- Default remains `etcd` for single-node backward compatibility

## Files to Create/Modify

| Action | Path | Description |
|--------|------|-------------|
| CREATE | `kine/cmd/microshift-kine/main.go` | Kine binary entry point |
| CREATE | `kine/cmd/microshift-kine/run.go` | Kine startup and config |
| CREATE | `kine/cmd/microshift-kine/version.go` | Version reporting |
| CREATE | `kine/go.mod` | Module definition |
| MODIFY | `pkg/controllers/etcd.go` | Refactor into storage.go |
| CREATE | `pkg/controllers/kine.go` | Kine process management |
| MODIFY | `pkg/controllers/kube-apiserver.go` | Storage-agnostic etcd server discovery |
| MODIFY | `pkg/config/config.go` | Add Storage.Backend config |
| MODIFY | `Makefile` | Add kine build target |
