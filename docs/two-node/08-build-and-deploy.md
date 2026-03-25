# Phase 8: Build and Deployment

## Binary Artifacts

### Three binaries
| Binary | Purpose | Used in |
|--------|---------|---------|
| `microshift` | Main control plane + kubelet | All modes |
| `microshift-etcd` | Embedded etcd (existing) | Single-node mode |
| `microshift-kine` | Kine storage shim (new) | 2-node HA mode |

### Cross-compilation targets
| Architecture | GOOS | GOARCH | Output Path |
|-------------|------|--------|-------------|
| x86_64 | linux | amd64 | `_output/bin/linux_amd64/` |
| ARM64 | linux | arm64 | `_output/bin/linux_arm64/` |

## Makefile Changes

### New targets
```makefile
# Build Kine binary
.PHONY: kine
kine:
	cd kine && $(MAKE) -f ../Makefile _build_microshift \
		GOOS=$(GOOS) GOARCH=$(GOARCH) \
		MICROSHIFT_BINARY=microshift-kine \
		SOURCE_GIT_COMMIT=$(SOURCE_GIT_COMMIT)

# Cross-build all binaries
.PHONY: cross-build-all
cross-build-all: cross-build cross-build-kine

.PHONY: cross-build-kine
cross-build-kine: cross-build-kine-linux-amd64 cross-build-kine-linux-arm64

.PHONY: cross-build-kine-linux-amd64
cross-build-kine-linux-amd64:
	+$(MAKE) kine GOOS=linux GOARCH=amd64

.PHONY: cross-build-kine-linux-arm64
cross-build-kine-linux-arm64:
	+$(MAKE) kine GOOS=linux GOARCH=arm64
```

### Build verification
```bash
# Build all binaries for both architectures
make cross-build-all

# Expected output:
# _output/bin/linux_amd64/microshift
# _output/bin/linux_amd64/microshift-etcd
# _output/bin/linux_amd64/microshift-kine
# _output/bin/linux_arm64/microshift
# _output/bin/linux_arm64/microshift-etcd
# _output/bin/linux_arm64/microshift-kine
```

## CGO Considerations

### Kine + PostgreSQL driver
- `lib/pq` (pure Go PostgreSQL driver) — no CGO needed
- Kine itself is pure Go
- Cross-compilation works without CGO for the Kine binary

### MicroShift main binary
- Currently requires CGO for some OpenShift dependencies
- Cross-compilation may need appropriate C toolchains
- For arm64 cross-compile on amd64: `aarch64-linux-gnu-gcc`

## Deployment to demo-1 and demo-2

### Initial deployment script
```bash
#!/bin/bash
# deploy-two-node.sh

PRIMARY_IP="192.168.5.107"
SECONDARY_IP="192.168.7.167"
VIP="192.168.5.200"
ARCH=$(uname -m | sed 's/x86_64/amd64/' | sed 's/aarch64/arm64/')
BIN_DIR="_output/bin/linux_${ARCH}"

# Install prerequisites on both nodes
for NODE in $PRIMARY_IP $SECONDARY_IP; do
    ssh root@$NODE "dnf install -y postgresql-server patroni keepalived"
done

# Copy binaries to both nodes
for NODE in $PRIMARY_IP $SECONDARY_IP; do
    scp ${BIN_DIR}/microshift root@${NODE}:/usr/bin/microshift
    scp ${BIN_DIR}/microshift-kine root@${NODE}:/usr/bin/microshift-kine
done

# Initialize primary
ssh root@$PRIMARY_IP "microshift init-cluster --vip $VIP --peer $SECONDARY_IP"

# Get join token
TOKEN=$(ssh root@$PRIMARY_IP "cat /var/lib/microshift/join-token")

# Join secondary
ssh root@$SECONDARY_IP "microshift join-cluster --token $TOKEN --primary $PRIMARY_IP --vip $VIP"

# Start both nodes
ssh root@$PRIMARY_IP "systemctl start microshift"
ssh root@$SECONDARY_IP "systemctl start microshift"
```

### Systemd unit modifications
The existing `microshift.service` is reused. Additional units:

```ini
# microshift-postgresql.service (managed by MicroShift internally via Patroni)
# Not a separate systemd unit — Patroni is started by MicroShift process

# keepalived.service — can be managed by MicroShift or separately
```

## RPM Packaging

### New package: `microshift-ha`
```spec
Name: microshift-ha
Version: %{version}
Summary: MicroShift 2-Node HA Support
Requires: microshift = %{version}
Requires: postgresql-server >= 15
Requires: patroni >= 3.0
Requires: keepalived >= 2.0

%description
Adds 2-node High Availability support to MicroShift using
PostgreSQL streaming replication and Kine storage backend.

%install
install -m 755 microshift-kine %{buildroot}/usr/bin/microshift-kine
install -m 644 keepalived.conf.tmpl %{buildroot}/usr/share/microshift/
install -m 755 check-apiserver.sh %{buildroot}/usr/libexec/microshift/
```

## Deployment Verification

### Smoke test after deployment
```bash
# From any machine with kubectl:
export KUBECONFIG=/var/lib/microshift/resources/kubeadmin/kubeconfig

# Check nodes
kubectl get nodes
# Expected: demo-1 Ready, demo-2 Ready

# Check control plane pods
kubectl -n kube-system get pods
# Expected: apiserver, controller-manager, scheduler pods on both nodes

# Check storage
kubectl -n kube-system get endpoints kubernetes
# Expected: Both node IPs listed

# Test failover: stop primary
ssh root@demo-1 "systemctl stop microshift"

# Verify cluster still works via VIP
kubectl --server=https://192.168.5.200:6443 get nodes
# Expected: demo-1 NotReady, demo-2 Ready

# Restart primary
ssh root@demo-1 "systemctl start microshift"
```

## Files to Create/Modify

| Action | Path | Description |
|--------|------|-------------|
| MODIFY | `Makefile` | Add kine and cross-build-all targets |
| CREATE | `scripts/deploy-two-node.sh` | Deployment automation |
| CREATE | `packaging/rpm/microshift-ha.spec` | HA RPM package spec |
| MODIFY | `packaging/microshift/microshift.service` | No changes needed |
