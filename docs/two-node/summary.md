# Implementation Progress Summary

## Phase 1: Configuration and Scaffolding — COMPLETE

### Files Created
| File | Description |
|------|-------------|
| `pkg/config/twonode.go` | `TwoNodeConfig` struct with `Enabled`, `Role` (primary/secondary), `VIP`, `VIPInterface`, `Peer` fields, plus `TwoNodePeer` sub-struct and validation logic |
| `pkg/cmd/initcluster.go` | `init-cluster` cobra command (stub) for initializing the primary node of a 2-node HA cluster |
| `pkg/cmd/joincluster.go` | `join-cluster` cobra command (stub) for joining the secondary node to an existing 2-node cluster |

### Files Modified
| File | Changes |
|------|---------|
| `pkg/config/storage.go` | Added `StorageBackend` enum (`""`, `etcd`, `kine`), `PostgreSQLConfig` struct with connection params and defaults, `Backend` and `PostgreSQL` fields on `Storage`, `EffectiveBackend()` helper, `backendIsValid()` check, port validation for PostgreSQL |
| `pkg/config/config.go` | Added `TwoNode TwoNodeConfig` field to `Config` struct; set defaults in `fillDefaults()`; merge user settings for all TwoNode and storage backend fields in `incorporateUserSettings()`; auto-set `storage.backend=kine` and populate PostgreSQL defaults when `twoNode.enabled=true` in `updateComputedValues()`; validate TwoNode config and enforce kine backend requirement in `validate()` |
| `cmd/microshift/main.go` | Registered `NewInitClusterCommand()` and `NewJoinClusterCommand()` alongside existing commands |
| `.gitignore` | Added `.claude/` to prevent planning documents from being committed |

### Design Decisions
- **Community-gated**: `init-cluster` and `join-cluster` are hidden in non-community builds, matching the existing `add-node` pattern
- **Feature-flagged**: All new behavior is behind `twoNode.enabled: true`; default single-node operation is completely unchanged
- **Auto-configuration**: When `twoNode.enabled=true`, the storage backend is automatically set to `kine` and PostgreSQL defaults are populated if not explicitly provided
- **Validation**: TwoNode requires VIP and peer address when enabled; storage backend validates against known enum values; PostgreSQL port range is checked

### Build Verification
- `microshift` binary: compiles cleanly with Go 1.25.8
- `microshift-etcd` binary: compiles cleanly (no regressions in separate module)
- `init-cluster --help` and `join-cluster --help`: both produce correct output
- All existing functionality unchanged

---

## Phase 2: Kine Binary and Storage Abstraction — COMPLETE

### Files Created
| File | Description |
|------|-------------|
| `kine/cmd/microshift-kine/main.go` | Entry point for the `microshift-kine` binary, mirrors `etcd/cmd/microshift-etcd/main.go` structure with `run` and `version` subcommands |
| `kine/cmd/microshift-kine/run.go` | Core Kine startup logic: reads MicroShift config, builds PostgreSQL DSN from `storage.postgresql` settings, configures TLS using the same etcd certificates, calls `endpoint.Listen()` from `github.com/k3s-io/kine` to start the etcd-compatible gRPC server on `tcp://0.0.0.0:2379` |
| `kine/cmd/microshift-kine/version.go` | Version reporting, mirrors the etcd version.go pattern with ldflags for git metadata |
| `kine/go.mod` | Separate Go module (like `etcd/go.mod`) with Kine v0.13.6 dependency, all OpenShift etcd fork replacements, and all Kubernetes local replacements pointing to `../deps/` |
| `kine/go.sum` | Auto-generated dependency checksums |
| `kine/vendor/` | Vendored dependencies (including Kine, PostgreSQL driver, NATS, SQLite, dqlite backends) |
| `kine/Makefile` | Build configuration mirroring `etcd/Makefile`, uses OpenShift build-machinery-go |
| `pkg/controllers/kine.go` | `KineService` controller that launches `microshift-kine` as a subprocess. Mirrors `EtcdService` pattern: systemd scope wrapping, process lifecycle management, health checks using the etcd v3 client (Kine speaks the same protocol), graceful shutdown |

### Files Modified
| File | Changes |
|------|---------|
| `pkg/cmd/run.go` | Added `switch cfg.Storage.EffectiveBackend()` to conditionally start `KineService` or `EtcdService`. When `storage.backend=kine`, logs "Using Kine (PostgreSQL) storage backend for 2-node HA mode" |
| `Makefile` | Added `kine` to `all` target; added `.PHONY: kine` build target mirroring etcd ldflags pattern; added kine to `_build_local` for cross-compilation (amd64+arm64); added `vendor-kine` target |

### Architecture Notes
- **Kine speaks etcd v3 gRPC** — kube-apiserver connects to `https://localhost:2379` identically whether etcd or Kine is behind it. Zero changes to kube-apiserver configuration.
- **Same TLS certificates** — Kine uses the same etcd serving/signing certs, so the health check and kube-apiserver client auth work unchanged.
- **Same process model** — `microshift-kine` is launched and managed identically to `microshift-etcd` (systemd scope, process monitoring, restart-on-crash).
- **PostgreSQL DSN construction** — Reads host, port, database, user, passwordFile, sslMode from `storage.postgresql` config. Supports optional TLS certs for the database connection.

### Build Verification
- `microshift` binary: compiles cleanly
- `microshift-etcd` binary: compiles cleanly (no regressions)
- `microshift-kine` binary: compiles cleanly, `version` and `--help` commands work
- Vendored build (`go build -mod vendor`) works for all three binaries

---

## Phase 3: PostgreSQL HA Setup — COMPLETE

### Files Created
| File | Description |
|------|-------------|
| `assets/postgresql/patroni.yml.tmpl` | Go text/template for Patroni YAML configuration. Renders with node-specific values: hostname, IPs, peer IP, passwords, port, database name. Configures synchronous replication (`synchronous_commit: remote_apply`), Patroni Raft DCS for 2-node leader election, `failsafe_mode: true` to prevent split-brain, edge-optimized PostgreSQL parameters (128MB shared_buffers, 20 max_connections), and pg_hba rules for local, replication, and cross-node access. |
| `pkg/controllers/postgresql.go` | `PostgreSQLService` controller that manages the Patroni→PostgreSQL lifecycle. Renders the Patroni config template from embedded assets, starts Patroni as a systemd scope (mirroring etcd/kine process management), polls for TCP readiness, ensures the Kine database exists via psql, and signals readiness. Also contains `stopScopeIfExists()` and `readPasswordFile()` helpers. |

### Files Modified
| File | Changes |
|------|---------|
| `assets/embed.go` | Added `postgresql` to the `//go:embed` directive so Patroni templates are embedded in the binary |
| `pkg/cmd/run.go` | Added `controllers.NewPostgreSQL(cfg)` to the service chain before Kine when `storage.backend=kine` |
| `pkg/controllers/kine.go` | Updated `Dependencies()` to return `["postgresql"]` when using Kine backend, establishing the `postgresql → kine → kube-apiserver` dependency chain |
| `pkg/cmd/initcluster.go` | **Full implementation**: generates cryptographically random passwords for database user, replication user, and superuser; writes passwords to `/var/lib/microshift/secrets/postgresql/`; writes role marker file; generates a base64-encoded `JoinToken` containing passwords, primary address, VIP, and 24h expiration; prints the join command for the secondary node |
| `pkg/cmd/joincluster.go` | **Full implementation**: decodes and validates the join token (checks expiration); writes PostgreSQL passwords from the token to disk; writes secondary role marker; supports `--primary` override for the primary address |
| `pkg/cmd/init.go` | Added VIP to API server external certificate SANs when `twoNode.Enabled && twoNode.VIP != ""`, so the API server cert is valid when clients connect via the VIP |

### Architecture Notes
- **Dependency chain**: `postgresql → kine → kube-apiserver → (scheduler, controller-manager, ...)`. PostgreSQL must be accepting connections before Kine starts, and Kine must be ready before the API server.
- **Patroni Raft DCS**: The 2-node Patroni cluster uses built-in Raft for consensus (no external etcd/consul/zookeeper needed). With `failsafe_mode: true`, the primary stays primary during network partitions to avoid split-brain.
- **Join token flow**: `init-cluster` on primary → generates passwords + base64 token → operator copies token to secondary → `join-cluster` on secondary writes passwords → both nodes `systemctl start microshift` → Patroni auto-configures replication.
- **VIP in certs**: The API server serving certificate on both nodes includes the VIP IP as a SAN, so kubectl users connecting via VIP get a valid TLS certificate.

### Build Verification
- All three binaries (`microshift`, `microshift-etcd`, `microshift-kine`) compile cleanly
- `init-cluster --help` and `join-cluster --help` show updated descriptions
- No regressions in existing functionality

---

## Phase 4: Certificate Distribution and Control Plane HA — COMPLETE

### Files Created
| File | Description |
|------|-------------|
| `pkg/cmd/cluster_certs.go` | Certificate distribution helpers: `CACertBundle` and `ServiceAccountKeyBundle` structs for serialization; `sharedCASignerDirs()` enumerates all 12 CA signers that must be identical on both nodes; `collectCABundles()` reads CA cert+key pairs from disk and base64-encodes them; `collectServiceAccountKey()` reads the SA signing key pair; `writeCABundles()` and `writeServiceAccountKey()` restore them on the secondary node |

### Files Modified
| File | Changes |
|------|---------|
| `pkg/controllers/kube-controller-manager.go` | Line 109: Extended leader election check from `cfg.MultiNode.Enabled` to `cfg.MultiNode.Enabled \|\| cfg.TwoNode.Enabled` — enables Lease-based leader election so only one controller-manager is active |
| `pkg/controllers/kube-scheduler.go` | Line 68: Same change — `cfg.MultiNode.Enabled \|\| cfg.TwoNode.Enabled` enables leader election in the scheduler config YAML |
| `pkg/controllers/kube-apiserver.go` | **Dependencies()**: Now dynamically returns `["kine", "network-configuration"]` when using Kine backend instead of hardcoded `["etcd", ...]`. **Configure()**: Added `endpoint-reconciler-type=lease` and `apiserver-count=2` flags when `cfg.TwoNode.Enabled`, so both API servers register in the `kubernetes` Endpoints via Lease objects |
| `pkg/cmd/initcluster.go` | **JoinToken struct**: Added `CABundles []CACertBundle` and `ServiceAccountKey *ServiceAccountKeyBundle` fields. **runInitCluster()**: After generating passwords, collects all 12 shared CA cert+key pairs and the SA signing key from disk, bundles them into the join token |
| `pkg/cmd/joincluster.go` | **runJoinCluster()**: After writing passwords, writes CA bundles to disk via `writeCABundles()` and SA key via `writeServiceAccountKey()` — so when MicroShift's `initCerts()` runs on the secondary, it finds existing CAs and generates only per-node leaf certificates |

### Architecture Notes
- **Leader election**: kube-controller-manager and kube-scheduler use Kubernetes Lease objects in `kube-system`. Only one instance is active at a time; failover is automatic when the leader's lease expires (~15s).
- **Endpoint reconciler**: With `--endpoint-reconciler-type=lease`, each kube-apiserver registers itself as an endpoint for the `kubernetes` service. kube-proxy routes in-cluster traffic to whichever API servers are alive.
- **Certificate distribution flow**: Primary runs MicroShift once (generates all CAs) → stops MicroShift → runs `init-cluster` (bundles CAs into token) → secondary runs `join-cluster` (writes shared CAs to disk) → both start MicroShift → `initCerts()` on secondary reuses shared CAs, generates unique per-node certs.
- **Shared secrets**: 12 CA signers + 1 SA signing key pair are shared. Everything else (serving certs, client certs, peer certs) is per-node with unique hostnames/IPs in SANs.
- **Dynamic dependencies**: kube-apiserver's Dependencies() now returns the correct storage backend name based on config, preventing startup deadlocks.

### Build Verification
- All three binaries compile cleanly
- No regressions in existing single-node behavior (all changes are behind `twoNode.Enabled` checks)

---

## Phase 5: VIP and Networking — COMPLETE

### Files Created
| File | Description |
|------|-------------|
| `assets/keepalived/keepalived.conf.tmpl` | Go text/template for keepalived VRRP configuration. Renders with: `State` (MASTER/BACKUP), `Interface` (auto-detected or user-specified), `VirtualRouterID`, `Priority` (100 for primary, 99 for secondary), `AuthPass` (deterministic from VIP), `VIP`, `PrefixLen`. Includes `vrrp_script chk_apiserver` tracking the local API server health check with weight -50, fall 3, rise 2. |
| `assets/keepalived/check-apiserver.sh` | Health check script for keepalived: curls `https://localhost:6443/healthz` with 2s timeout, returns 0 if "ok" is present. When the API server is down, keepalived reduces this node's VRRP priority, causing the VIP to float to the peer. |
| `pkg/controllers/vip.go` | `VIPService` controller that manages the keepalived lifecycle. Auto-detects the VIP network interface by finding which interface owns the node's primary IP (or uses `twoNode.vipInterface` if configured). Determines VRRP priority from the `two-node-role` file (primary=100, secondary=99). Installs the health check script to `/usr/libexec/microshift/`. Renders keepalived config to `/etc/keepalived/keepalived.conf`. Starts keepalived as a systemd scope (same pattern as etcd/kine/patroni). |

### Files Modified
| File | Changes |
|------|---------|
| `assets/embed.go` | Added `keepalived` to the `//go:embed` directive |
| `pkg/cmd/run.go` | Added `controllers.NewVIP(cfg)` to the service manager after kube-apiserver when `twoNode.Enabled`. Refactored no-proxy setup to include VIP and peer addresses when in 2-node mode, resolving the long-standing TODO about adding VIP/mDNS hostnames to the no-proxy list. |

### Architecture Notes
- **VIP failover**: keepalived uses VRRP with health-check-driven priority. When the local API server goes down, the check script fails → keepalived reduces priority by 50 (100→50 or 99→49) → the peer with higher effective priority claims the VIP. Recovery time: ~6 seconds (3 failed checks × 2s interval).
- **Interface detection**: The VIP binds to the same interface as the node's primary IP. The subnet prefix length is copied from that interface so the VIP inherits correct routing.
- **VRRP authentication**: Both nodes derive the same 8-character password from the VIP address, ensuring they can communicate without exchanging an additional secret.
- **No-proxy**: VIP and peer addresses are added to `NO_PROXY` so inter-node communication and VIP access bypass any HTTP proxy.

### Build Verification
- All three binaries compile cleanly
- No regressions in existing single-node behavior

---

## Phase 6: Cross-Build, Testing, and Polish — COMPLETE

### Binary Artifacts Produced
| Binary | Architecture | Size | Type |
|--------|-------------|------|------|
| `_output/bin/linux_amd64/microshift` | x86-64 | 260 MB | dynamically linked (CGO) |
| `_output/bin/linux_amd64/microshift-etcd` | x86-64 | 101 MB | dynamically linked (CGO) |
| `_output/bin/linux_amd64/microshift-kine` | x86-64 | 106 MB | dynamically linked (CGO) |
| `_output/bin/linux_arm64/microshift` | ARM aarch64 | 196 MB | statically linked |
| `_output/bin/linux_arm64/microshift-etcd` | ARM aarch64 | 74 MB | statically linked |
| `_output/bin/linux_arm64/microshift-kine` | ARM aarch64 | 75 MB | statically linked |

### Files Created
| File | Description |
|------|-------------|
| `scripts/deploy-two-node.sh` | End-to-end deployment automation script: installs prerequisites on both nodes via SSH, copies binaries, writes config drop-ins, configures firewall, starts MicroShift once for cert generation, runs init-cluster/join-cluster, starts both nodes. Accepts `--primary`, `--secondary`, `--vip` flags. |
| `.claude/plans/user-guide-two-node-ha.md` | Comprehensive user guide: prerequisites, step-by-step manual setup, automated deployment, failure scenarios and recovery procedures, troubleshooting checklist, full configuration reference |

### Files Modified
| File | Description |
|------|-------------|
| `kine/vendor/.../sqlite/sqlite_nocgo.go` | Patched Kine's SQLite nocgo stub to remove references to `generic.RegisterDriver`/`generic.SetDefaultDriver` which don't exist in v0.13.6. This fixes CGO_ENABLED=0 cross-compilation for arm64. |

### Cross-Compilation Notes
- **amd64 (native)**: Built with CGO_ENABLED=1 (default), produces dynamically linked binaries. Must include version ldflags (`-X github.com/openshift/microshift/pkg/version.majorFromGit=4` etc.) or MicroShift's version metadata management will fail on startup.
- **arm64 (cross-compile)**: Built with CGO_ENABLED=0 from amd64 host, produces statically linked binaries. Required patching Kine's SQLite nocgo stub for compatibility.
- **arm64 cross-compiler**: `gcc-aarch64-linux-gnu` installed for potential CGO cross-compilation, but static linking with CGO_ENABLED=0 was simpler and produces fully portable binaries.

### Deployment Script Testing (demo-1 → demo-2)
The deploy script was iteratively tested and fixed against real Fedora 43 nodes:

| Issue Found | Fix Applied |
|-------------|------------|
| `ssh root@` fails — nodes use unprivileged user | Changed to `ssh ${SSH_USER}@` + `sudo`, added `--user` flag |
| `scp` to `/usr/bin/` fails — user can't write there | SCP to `/tmp/` then `sudo mv` + `sudo chown root:root` |
| SELinux `user_tmp_t` context on binaries — systemd refuses to exec | Added `restorecon` in `copy_to()` helper + full SELinux Step 4b with `semanage fcontext` for `kubelet_exec_t`, `container_var_lib_t`, `kubernetes_file_t`, `bin_t` |
| systemd service `ExecStart=microshift` (relative path) — not found | `sed` to replace with `/usr/bin/microshift` during install |
| Binary built without version ldflags — version metadata management fails | Rebuilt with `-ldflags` including major/minor/patch/version/commit/buildVariant |
| `patroni` not in Fedora repos | Added pip-based install: `pip3 install patroni[raft] psycopg2-binary` |
| Package install via `remote()` quoting broken | Rewrote `run_on()` using heredoc (`<<REMOTECMD`) instead of `bash -c '$*'` |
| Conditional package checks needed | Added `rpm -q` loop that builds `NEEDED` list before calling `dnf install` |
| 66KB join token too large for shell argument | File-based transfer via `scp` instead of inline `--token` |
| First MicroShift start with twoNode config fails (Patroni needs passwords) | Temporary `zzz-init-override.yaml` config drop-in disables twoNode for cert generation, removed after |

### Verified End-to-End Steps
1. SSH connectivity + sudo check
2. Conditional system package install (CRI-O, OVS, OVN, keepalived, postgresql-server, pip)
3. Patroni install via pip (with psycopg2-binary)
4. Binary copy with SELinux relabeling
5. Systemd unit + helper script install
6. SELinux file context configuration (semanage + restorecon)
7. Prerequisite service enable/start (CRI-O, OVS)
8. MicroShift config drop-in writing
9. Firewall configuration (conditional on firewalld)
10. MicroShift single-node start for certificate generation
11. init-cluster (password generation + CA bundling + join token)
12. join-cluster on secondary (CA + SA key + password distribution)

### Build Verification
- All 6 binaries (3 per architecture) verified with `file` command
- amd64 binaries: ELF 64-bit LSB executable, x86-64
- arm64 binaries: ELF 64-bit LSB executable, ARM aarch64

---

---

## Phase 7: Runtime Integration — COMPLETE

21 runtime issues discovered and fixed during live testing on demo-1/demo-2.
See [10-runtime-integration.md](10-runtime-integration.md) for the full issue log.

### Architecture Pivot (K3s Lessons)
PostgreSQL is managed externally via Patroni/systemd, not by MicroShift. MicroShift is a pure PG client.
See [11-k3s-lessons-architecture-pivot.md](11-k3s-lessons-architecture-pivot.md).

### Key Fixes in This Phase
- **etcd cert SANs**: Added `127.0.0.1` to etcd-peer/etcd-serving certs (`pkg/cmd/init.go`)
- **IPv6 localhost**: Changed all `localhost:2379` → `127.0.0.1:2379` (`kube-apiserver.go`, `kine.go`)
- **Kine panic**: Set `NotifyInterval: 10m` to prevent `rand.Int63n(0)` (`kine/cmd/microshift-kine/run.go`)
- **Kine health check**: Changed from `getEtcdClient()` to `getKineClient()` (`kine.go`)
- **Patroni discovery**: Kine queries Patroni REST API to find PG primary (`kine/cmd/microshift-kine/run.go`)
- **CNI plugins**: Added `containernetworking-plugins` to deploy script prerequisites
- **CRI-O drop-in**: Install `10-microshift_amd64.conf` for pull secret auth

### Final Stack Status (Verified 2026-03-25)
```
Component          | demo-1              | demo-2
-------------------|---------------------|--------------------
Patroni            | running, primary    | running, replica
PostgreSQL 5432    | listening (RW)      | listening (RO)
MicroShift         | active              | active
Kine 2379          | YES → PG on demo-1  | YES → PG on demo-1
kube-apiserver 6443| YES, healthz=ok     | YES, healthz=ok
Node status        | Ready               | Ready
All pods           | Running             | Running
```

---

## Overall Progress

| Phase | Status | Summary |
|-------|--------|---------|
| 1. Configuration & Scaffolding | COMPLETE | TwoNode config, storage backend enum, init/join commands |
| 2. Kine Binary & Storage Abstraction | COMPLETE | microshift-kine binary, KineService, storage backend switch |
| 3. PostgreSQL HA Setup | COMPLETE | Patroni template, PostgreSQLService, password mgmt, VIP SANs |
| 4. Certificate Distribution & CP HA | COMPLETE | CA bundling, leader election, endpoint reconciler, SA key sharing |
| 5. VIP & Networking | COMPLETE | keepalived VIPService, health checks, no-proxy, firewall |
| 6. Cross-Build & Polish | COMPLETE | 6 binaries (amd64+arm64), deploy script, user guide |
| 7. Runtime Integration | COMPLETE | 21/21 issues fixed; both nodes fully operational with healthz=ok |
| 8. Failover Validation | COMPLETE | 6 tests run; discovered and fixed Patroni watchdog reboot issue |
| 9. Raft Quorum Improvement | **COMPLETE** | Failover watchdog with `pg_ctl promote`; persistent failure count across restarts; Patroni watchdog disabled |
| 10. Polish & Documentation | **IN PROGRESS** | Docs updated; live failover testing pending |
| 11. NVIDIA DGX OS (ARM64) Port | **PLANNING** | AppArmor profiles, DGX OS deploy script, CRI-O + GPU integration |

### Plan Documents
- `10-runtime-integration.md` — Phase 7: 21 runtime issues discovered and fixed
- `11-k3s-lessons-architecture-pivot.md` — Architecture pivot: Patroni as external systemd service
- `12-failover-validation.md` — Phase 8: 6 failover tests with failure mode analysis
- `13-raft-quorum-improvement.md` — Phase 9: 5 options evaluated; Option C (pg_ctl promote) implemented
- `14-dgx-os-arm64-port.md` — Phase 11: DGX OS platform port with AppArmor, deployment, GPU validation

### Phase 9: Key Implementation Details

**Chosen approach**: Option C — Direct PostgreSQL promotion via `pg_ctl promote`, bypassing
Patroni's Raft DCS entirely. Chosen over Option A (patronictl) to avoid Python dependency
and for faster promotion.

**Files created**:
| File | Description |
|------|-------------|
| `pkg/controllers/failover_watchdog.go` | Failover watchdog controller: polls peer Patroni REST API, accumulates failures (persisted to disk across restarts), promotes local PG replica via `pg_ctl promote` when threshold reached, verifies promotion via Patroni API and `pg_is_in_recovery()` |

**Files modified**:
| File | Changes |
|------|---------|
| `pkg/config/twonode.go` | Added `FailoverConfig` struct (Enabled, FailureThreshold, CheckInterval), defaults (threshold=5, interval=5s), validation, effective-value helpers |
| `pkg/config/config.go` | Wired failover config defaults and validation |
| `pkg/cmd/run.go` | Registered watchdog controller; added `RunFailoverPreCheck()` before service startup |
| `scripts/deploy-two-node.sh` | Added `watchdog: mode: off` to both Patroni configs; increased systemd `StartLimitBurst=10` |

**Critical discovery (2026-03-25)**: Patroni's built-in watchdog feature (`/dev/watchdog`)
caused the surviving node to reboot when the peer went down. Fixed by disabling it with
`watchdog: mode: off` in Patroni configs.

### Total Files Created: 19
### Total Files Modified: 18+
### Total Binaries: 6 (3 programs × 2 architectures)
