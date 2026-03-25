# Phase 5: Certificate and PKI Management for 2-Node

## Challenge
MicroShift generates all certificates locally during initialization. In a 2-node
setup, both nodes must share certain certificates and have unique per-node certs.

## Certificate Categories

### Shared (identical on both nodes)
These must be copied from the initializing node to the joining node:

| Certificate | Path | Why shared |
|------------|------|------------|
| Service account signing key | `service-account-key/` | Both API servers must validate SA tokens signed by either |
| Etcd/Kine CA | `etcd-signer/` | Both Kine instances use same CA |
| API server CA | `kube-apiserver-*-signer/` | Clients trust one CA for both API servers |
| Aggregator CA | `aggregator-signer/` | Same aggregation trust |
| Kubelet CSR signer CA | `kubelet-csr-signer/` | Both nodes' kubelets CSRs signed by same CA |
| Service CA | `service-ca/` | Services trust one CA |
| Ingress CA | `ingress-ca/` | Ingress trust |
| Client CA bundle | `ca-bundle/` | Shared trust store |

### Per-Node (unique to each node)
These are generated per-node using the shared CAs:

| Certificate | Why unique |
|------------|-----------|
| API server serving cert | Contains node-specific IP/hostname SANs |
| Kubelet client cert | Unique node identity (`system:node:<hostname>`) |
| Kubelet serving cert | Node-specific SANs |
| Etcd/Kine peer cert | Node-specific |
| Etcd/Kine serving cert | Node-specific |

### VIP SANs
The API server serving certificate on **both** nodes must include the VIP address
in its Subject Alternative Names (SANs), so clients connecting via VIP see a valid
certificate regardless of which node they reach.

## Implementation

### 1. Certificate initialization flow

**First node (initializer):**
1. Generates all CA certificates (signers) as normal
2. Generates its own per-node certificates
3. Adds VIP to API server serving cert SANs
4. Stores CA private keys (needed to sign certs for second node)

**Second node (joiner):**
1. Receives shared CA certificates from first node (via `microshift join-cluster`
   command or similar)
2. Generates its own per-node certificates signed by the shared CAs
3. Adds VIP to its API server serving cert SANs

### 2. Certificate distribution mechanism

**Option A: `microshift init-cluster` / `microshift join-cluster` commands**
Similar to `kubeadm init` / `kubeadm join`:

```bash
# On demo-1 (first node):
microshift init-cluster --vip 192.168.5.200

# Outputs a join token/command, e.g.:
# microshift join-cluster --token <base64-encoded-ca-bundle> \
#   --primary 192.168.5.107 --vip 192.168.5.200

# On demo-2:
microshift join-cluster --token <token> \
  --primary 192.168.5.107 --vip 192.168.5.200
```

The token contains:
- Base64-encoded CA certificates (public only for verification)
- Bootstrap token for initial authentication
- Primary node address for fetching full CA bundle

**Option B: Shared filesystem / config management**
Operator copies CA certs manually or via Ansible/Salt.

**Recommendation**: Option A for better UX, matching MicroShift's "minimal config"
philosophy and following the existing `add-node` pattern.

### 3. Code changes to certificate generation

Modify `pkg/cmd/init.go` and certificate chain builders:

```go
// pkg/util/cryptomaterial/certchains/
// Add VIP to SAN list when generating API server certs

func apiServerSANs(cfg *config.Config) []string {
    sans := []string{
        cfg.Node.HostnameOverride,
        cfg.Node.NodeIP,
        "localhost",
        "127.0.0.1",
        "kubernetes",
        "kubernetes.default",
        "kubernetes.default.svc",
        // ... existing SANs
    }
    if cfg.TwoNode.Enabled && cfg.TwoNode.VIP != "" {
        sans = append(sans, cfg.TwoNode.VIP)
    }
    return sans
}
```

### 4. Kubeconfig generation

Both nodes need kubeconfigs that point to the VIP (not localhost) for components
that might need to reach the other node's API server:
- controller-manager kubeconfig → `https://<VIP>:6443`
- scheduler kubeconfig → `https://<VIP>:6443`
- kubelet kubeconfig → `https://<VIP>:6443`

However, for performance, components can also use `localhost:6443` since each
node runs its own API server. The VIP is mainly for:
- External `kubeconfig` (for kubectl users)
- In-cluster service account authentication

### 5. Certificate rotation in 2-node mode

MicroShift already handles certificate rotation by restarting when certs are near
expiry. In 2-node mode:
- Each node independently rotates its per-node certificates
- CA certificates have long lifetimes (10 years) and don't need coordination
- Per-node cert rotation is local and independent

## PostgreSQL TLS Certificates

PostgreSQL replication also needs TLS:
- Generate a PostgreSQL CA (or reuse an existing signer)
- Per-node server certificates for PostgreSQL
- Client certificates for replication user

These can be generated as part of the MicroShift PKI:
```
postgresql-signer/
├── ca.crt
├── ca.key
├── demo-1/
│   ├── server.crt
│   └── server.key
└── demo-2/
    ├── server.crt
    └── server.key
```

## Files to Create/Modify

| Action | Path | Description |
|--------|------|-------------|
| MODIFY | `pkg/cmd/init.go` | VIP SANs, shared CA handling |
| CREATE | `pkg/cmd/initcluster.go` | `init-cluster` command |
| CREATE | `pkg/cmd/joincluster.go` | `join-cluster` command |
| MODIFY | `pkg/util/cryptomaterial/certchains/*.go` | VIP SAN support |
| CREATE | `pkg/util/cryptomaterial/postgresql.go` | PostgreSQL certs |
| MODIFY | `cmd/microshift/main.go` | Register new commands |
