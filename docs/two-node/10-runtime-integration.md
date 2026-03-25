# Phase 7: Runtime Integration and Live Debugging

## Status: COMPLETE (21/21 issues resolved)

This phase covers the real-world integration issues discovered during live testing
on demo-1 (192.168.5.107) and demo-2 (192.168.7.167). All code from Phases 1-6
compiled and the deploy script ran end-to-end, but 21 runtime issues were discovered
and fixed during live testing.

## Issues: Deploy Script and Bootstrap (1-10)

### 1. SSH as root (FIXED)
- **Problem**: Deploy script used `ssh root@` but nodes use unprivileged `fedora` user
- **Fix**: Changed to `ssh ${SSH_USER}@` + `sudo`, added `--user` flag
- **File**: `scripts/deploy-two-node.sh`

### 2. SELinux file contexts (FIXED)
- **Problem**: Binaries copied via `/tmp/` get `user_tmp_t` context; systemd refuses to exec
- **Fix**: Added Step 4b with `semanage fcontext` for `kubelet_exec_t`, `container_var_lib_t`, `kubernetes_file_t` + `restorecon`
- **File**: `scripts/deploy-two-node.sh`

### 3. systemd ExecStart relative path (FIXED)
- **Problem**: `ExecStart=microshift run` not found by newer systemd
- **Fix**: `sed` to replace with `/usr/bin/microshift` during install
- **File**: `scripts/deploy-two-node.sh`

### 4. Version ldflags missing (FIXED)
- **Problem**: Binary built with `go build` lacks version metadata; version management fails at startup
- **Fix**: Rebuild with `-ldflags` including major/minor/patch/version/commit/buildVariant
- **File**: Build commands

### 5. Patroni not in Fedora repos (FIXED)
- **Problem**: `dnf install patroni` fails
- **Fix**: `pip3 install patroni[raft] psycopg2-binary`
- **File**: `scripts/deploy-two-node.sh` Step 2

### 6. Patroni Raft needs both peers (FIXED)
- **Problem**: Single-node Patroni with Raft DCS sits in `uninitialized` forever waiting for quorum
- **Fix**: Primary starts without `partner_addrs`; secondary starts with partner pointing to primary. After join, partner added to primary config.
- **File**: `scripts/deploy-two-node.sh`

### 7. PostgreSQL data dir ownership (FIXED)
- **Problem**: PostgreSQL refuses to run with root-owned data directory
- **Fix**: `chown -R postgres:postgres` on postgres/patroni dirs; `chmod 711` on parent for traverse
- **File**: `scripts/deploy-two-node.sh`

### 8. Patroni runs as postgres user (FIXED)
- **Problem**: PostgreSQL can't start as root
- **Fix**: Patroni systemd service runs as `User=postgres`
- **File**: `packaging/systemd/microshift-patroni.service`

### 9. Kine TLS to PostgreSQL (FIXED)
- **Problem**: Kine connects with `sslmode=verify-full` but PG has no TLS certs
- **Fix**: Default `sslMode` changed to `disable`; host changed from `localhost` to `127.0.0.1`
- **Files**: `kine/cmd/microshift-kine/run.go`, `pkg/config/storage.go`

### 10. Cert generation with twoNode config (FIXED)
- **Problem**: First MicroShift start for cert generation tries to start Kine (which needs PG passwords that don't exist yet)
- **Fix**: Temporary `zzz-init-override.yaml` config drop-in disables twoNode for the cert-generation pass
- **File**: `scripts/deploy-two-node.sh`

## Issues: Architecture Pivot (11-13)

### 11. Architecture pivot: Patroni managed externally (FIXED)
- **Problem**: MicroShift managing Patroni lifecycle caused cascading failures
- **Fix**: Followed K3s model — PostgreSQL is managed externally via systemd. `PostgreSQLService` removed from MicroShift service chain. Patroni runs as independent `microshift-patroni.service`.
- **Files**: `pkg/cmd/run.go`, `pkg/controllers/kine.go`, `packaging/systemd/microshift-patroni.service`
- **Design doc**: See [11-k3s-lessons-architecture-pivot.md](11-k3s-lessons-architecture-pivot.md)

### 12. Patroni solo Raft bootstrap (FIXED)
- **Problem**: Primary Patroni needs to self-elect without partner for initial bootstrap
- **Fix**: Primary starts without `partner_addrs`; partner added after secondary joins. `synchronous_mode_strict: false` so primary operates without sync standby during bootstrap.
- **File**: `scripts/deploy-two-node.sh`

### 13. Patroni bootstrap.users doesn't work (FIXED)
- **Problem**: Patroni v4.1.0 `bootstrap.users` config doesn't reliably create PG roles
- **Fix**: Deploy script creates role+db explicitly via `psql` as postgres superuser (Step 8b). Detects PG primary via Patroni REST API.
- **File**: `scripts/deploy-two-node.sh`

## Issues: Certificate and Connectivity (14-18)

### 14. Stale leaf certs after join-cluster (FIXED)
- **Problem**: join-cluster writes shared CAs but leaf certs from previous cert-gen pass were signed by old CAs.
- **Fix**: After join-cluster, delete all leaf cert subdirectories and ca-bundle so `initCerts()` regenerates them.
- **File**: `scripts/deploy-two-node.sh` (Step 11b)

### 15. PG primary elected on demo-2 (NOTED)
- **Observation**: Raft elected demo-2 (192.168.7.167) as PG primary after secondary joined, not demo-1 as expected.
- **Impact**: No fix needed — Patroni elects based on Raft consensus; the MicroShift role (primary/secondary) is independent of the Patroni role.

### 16. etcd serving cert missing 127.0.0.1 SAN (FIXED)
- **Problem**: TLS handshake failed when connecting to `127.0.0.1:2379` because the cert only had the node's external IP and `localhost` (DNS) — no `IP Address:127.0.0.1` SAN.
- **Fix**: Added `"127.0.0.1"` to Hostnames for both etcd-peer and etcd-serving certs.
- **File**: `pkg/cmd/init.go` (line ~330, PeerCertificateSigningRequestInfo Hostnames)

### 17. Kine health check used wrong client (FIXED)
- **Problem**: `checkIfKineIsReady()` called `getEtcdClient()` (from etcd.go, using `localhost`) instead of `getKineClient()` (using `127.0.0.1`). Health check connected via IPv6 and failed.
- **Fix**: Changed to call `getKineClient()`.
- **File**: `pkg/controllers/kine.go` (line ~177)

### 18. IPv6 localhost resolution in all etcd/kine clients (FIXED)
- **Problem**: `localhost` resolves to `[::1]` on Fedora 43 but Kine only binds IPv4 `0.0.0.0:2379`.
- **Fix**: Changed all `localhost:2379` references to `127.0.0.1:2379` in kube-apiserver's `discoverEtcdServers()` and kine's `getKineClient()`.
- **Files**: `pkg/controllers/kube-apiserver.go` (lines ~460, 469, 477), `pkg/controllers/kine.go` (lines ~190, 242)

## Issues: Kine Stability and Replica Node (19-21)

### 19. Kine panic: `invalid argument to Int63n` (FIXED)
- **Problem**: Kine's Watch handler called `rand.Int63n(0)` because `NotifyInterval` was unset (zero value). `Int63n` panics when n <= 0.
- **Fix**: Set `NotifyInterval: 10 * time.Minute` in `endpoint.Config` (matches etcd default).
- **File**: `kine/cmd/microshift-kine/run.go` (line ~78)
- **Upstream**: Kine v0.13.6 bug — `server/watch.go:27` should guard against zero interval.

### 20. Kine on replica node can't write (FIXED)
- **Problem**: Secondary node's Kine connects to local PG which is a read-only replica; writes fail.
- **Fix**: Added Patroni REST API discovery in `buildPostgreSQLEndpoint()`. On startup, Kine queries `http://<nodeIP>:8008/patroni` on both the local and peer node to find the PG primary. Both Kines connect to the PG primary regardless of which node they run on.
- **File**: `kine/cmd/microshift-kine/run.go` (new `discoverPatroniPrimary()` function)
- **Failover behavior**: On Patroni failover, the PG connection breaks → MicroShift restarts → Kine re-discovers the new primary.

### 21. Missing CNI plugins and CRI-O config (FIXED)
- **Problem**: `containernetworking-plugins` package not installed; CRI-O drop-in config missing. Kubelet reported `NetworkPluginNotReady`, CRI-O's watchdog killed it.
- **Fix**: Added `containernetworking-plugins` to deploy script Step 1 packages. Install CRI-O drop-in config (`10-microshift_amd64.conf`) in Step 4 which sets `global_auth_file` for pull secret.
- **File**: `scripts/deploy-two-node.sh`

## Final Stack Status (Verified 2026-03-25)

```
Component          | demo-1              | demo-2
-------------------|---------------------|--------------------
Patroni            | running, primary    | running, replica
PostgreSQL 5432    | listening (RW)      | listening (RO, streaming)
MicroShift service | active              | active
Kine 2379          | YES → PG on demo-1  | YES → PG on demo-1
kube-apiserver 6443| YES, healthz=ok     | YES, healthz=ok
Node status        | Ready               | Ready
OVN-Kubernetes     | 4/4 Running         | 4/4 Running
DNS                | Running             | Running
Ingress router     | Running             | —
service-ca         | Running             | —
CSI snapshot ctrl  | Running             | —
```

## Files Modified During Phase 7

| File | Changes |
|------|---------|
| `pkg/cmd/init.go` | Added `127.0.0.1` to etcd-peer and etcd-serving cert SANs |
| `pkg/controllers/kube-apiserver.go` | Changed `localhost:2379` → `127.0.0.1:2379` in `discoverEtcdServers()` |
| `pkg/controllers/kine.go` | Changed health check to use `getKineClient()`; `127.0.0.1` endpoints |
| `kine/cmd/microshift-kine/run.go` | Added `NotifyInterval`, Patroni discovery (`discoverPatroniPrimary()`), `time`/`net/http`/`encoding/json` imports |
| `pkg/config/storage.go` | `PostgreSQLDefaults()`: sslMode `disable`, host `127.0.0.1` |
| `pkg/cmd/run.go` | PostgreSQLService removed from service chain (K3s model) |
| `scripts/deploy-two-node.sh` | `containernetworking-plugins` in packages; CRI-O drop-in; `oc` CLI install; kubeconfig fetch |
