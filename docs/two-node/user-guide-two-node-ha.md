# MicroShift 2-Node HA User Guide

## Overview

MicroShift 2-Node HA allows you to run a MicroShift cluster across two physical
or virtual machines with automatic failover. Both nodes run the full Kubernetes
control plane, and workloads are scheduled across both.

### Architecture
```
┌──────────────────┐         ┌──────────────────┐
│   Primary Node   │◄──VIP──►│  Secondary Node  │
│                  │         │                  │
│  kube-apiserver  │         │  kube-apiserver  │
│  kube-scheduler  │         │  kube-scheduler  │
│  controller-mgr  │         │  controller-mgr  │
│  kubelet         │         │  kubelet         │
│  Kine ─────────────────────────► PG primary   │
│  keepalived      │  WAL    │  keepalived      │
│                  │  repl.  │                  │
│  Patroni → PG   │◄───────►│  Patroni → PG    │
└──────────────────┘         └──────────────────┘
```

**Key**: Both Kine instances connect to whichever node is the PostgreSQL primary
(discovered via Patroni REST API at startup). Patroni manages PG replication and
failover independently from MicroShift.

### Key Components
| Component | Purpose |
|-----------|---------|
| **Kine** | etcd-compatible API backed by PostgreSQL |
| **PostgreSQL + Patroni** | Database with synchronous replication and automatic failover |
| **keepalived** | Floating Virtual IP for the API server endpoint |
| **Leader election** | Only one scheduler and controller-manager active at a time |

---

## Prerequisites

### Hardware
- 2 Linux machines (physical or virtual), each with:
  - 2+ CPU cores
  - 4+ GB RAM
  - 10+ GB disk
- Network connectivity between the nodes (latency < 10ms)
- A free IP address in the same subnet for the VIP

### Software

Install on **both** nodes:
```bash
# System packages (RHEL/Fedora)
sudo dnf install -y cri-o openvswitch ovn-host ovn-central keepalived \
  conntrack-tools containernetworking-plugins jq postgresql-server python3-pip

# Patroni (not in Fedora repos — install via pip)
sudo pip3 install "patroni[raft]" psycopg2-binary

# OpenShift CLI (for cluster management)
curl -sL https://mirror.openshift.com/pub/openshift-v4/clients/ocp/stable/openshift-client-linux.tar.gz | \
  sudo tar xzf - -C /usr/local/bin oc kubectl
```

### SSH Access
The deploy script requires:
- SSH access from the build machine to both nodes (as a non-root user)
- **Passwordless sudo** on both nodes for that user

### Pull Secret
A Red Hat pull secret is required for OCP container images:
1. Download from https://console.redhat.com/openshift/downloads#tool-pull-secret
2. Place at `/etc/crio/openshift-pull-secret` on **both** nodes with mode `600`

### CRI-O Configuration
The MicroShift CRI-O drop-in must be installed on both nodes:
```bash
sudo cp packaging/crio.conf.d/10-microshift_amd64.conf /etc/crio/crio.conf.d/
sudo systemctl restart crio
```
This configures CRI-O to use the pull secret for image authentication.

### Network
The following ports must be open between the two nodes:
| Port | Protocol | Purpose |
|------|----------|---------|
| 6443 | TCP | Kubernetes API server |
| 5432 | TCP | PostgreSQL replication |
| 8008 | TCP | Patroni REST API |
| 8009 | TCP | Patroni Raft consensus |
| 112 | IP (VRRP) | keepalived VIP |
| 10250 | TCP | kubelet |
| 6641 | TCP | OVN northbound DB |
| 6642 | TCP | OVN southbound DB |
| 6081 | UDP | Geneve tunnel (OVN) |

```bash
# Open firewall ports on both nodes
sudo firewall-cmd --add-port=6443/tcp --add-port=5432/tcp \
  --add-port=8008/tcp --add-port=8009/tcp --add-port=10250/tcp \
  --add-port=6641/tcp --add-port=6642/tcp --add-port=6081/udp \
  --add-protocol=vrrp --permanent
sudo firewall-cmd --reload
```

### SELinux Configuration
MicroShift binaries require correct SELinux contexts:
```bash
# On both nodes
sudo dnf install -y policycoreutils-python-utils
for bin in /usr/bin/microshift /usr/bin/microshift-kine; do
  sudo semanage fcontext -a -t kubelet_exec_t "$bin" 2>/dev/null || \
  sudo semanage fcontext -m -t kubelet_exec_t "$bin"
done
sudo restorecon -Rv /usr/bin/microshift*
```

---

## Step-by-Step Manual Setup

### Step 1: Install MicroShift Binaries

Build with version ldflags (required):
```bash
GOOS=linux GOARCH=amd64 go build \
  -ldflags "-X github.com/openshift/microshift/pkg/version.majorFromGit=4 \
            -X github.com/openshift/microshift/pkg/version.minorFromGit=21 \
            -X github.com/openshift/microshift/pkg/version.patchFromGit=0 \
            -X github.com/openshift/microshift/pkg/version.versionFromGit=4.21.0-two-node \
            -X github.com/openshift/microshift/pkg/version.commitFromGit=$(git rev-parse --short HEAD) \
            -X github.com/openshift/microshift/pkg/version.gitTreeState=dirty \
            -X github.com/openshift/microshift/pkg/version.buildDate=$(date -u +%Y-%m-%dT%H:%M:%SZ) \
            -X github.com/openshift/microshift/pkg/version.buildVariant=community" \
  -o _output/bin/linux_amd64/microshift ./cmd/microshift/

cd kine && GOOS=linux GOARCH=amd64 go build \
  -ldflags "..." \
  -o ../_output/bin/linux_amd64/microshift-kine ./cmd/microshift-kine/
```

Copy to both nodes:
```bash
for NODE in <primary-ip> <secondary-ip>; do
  scp _output/bin/linux_amd64/microshift user@${NODE}:/tmp/
  scp _output/bin/linux_amd64/microshift-kine user@${NODE}:/tmp/
  ssh user@${NODE} "sudo mv /tmp/microshift /usr/bin/ && sudo mv /tmp/microshift-kine /usr/bin/ && \
    sudo chown root:root /usr/bin/microshift /usr/bin/microshift-kine && \
    sudo chmod 755 /usr/bin/microshift /usr/bin/microshift-kine && \
    sudo restorecon /usr/bin/microshift /usr/bin/microshift-kine"
done
```

### Step 2: Configure the Primary Node

Create `/etc/microshift/config.d/two-node.yaml` on the **primary** node:

```yaml
twoNode:
  enabled: true
  role: primary
  vip: "192.168.5.200"          # Your chosen VIP address
  peer:
    address: "192.168.7.167"    # Secondary node's IP
storage:
  backend: kine
```

### Step 3: Configure the Secondary Node

Create `/etc/microshift/config.d/two-node.yaml` on the **secondary** node:

```yaml
twoNode:
  enabled: true
  role: secondary
  vip: "192.168.5.200"          # Same VIP as primary
  peer:
    address: "192.168.5.107"    # Primary node's IP
storage:
  backend: kine
```

### Step 4: Set Up Patroni and PostgreSQL

Patroni manages PostgreSQL independently from MicroShift (the K3s model). Set up
Patroni on both nodes before starting MicroShift. See the deploy script
(`scripts/deploy-two-node.sh` Steps 8-8b) for the full Patroni config template
and bootstrap procedure.

Key points:
- Primary starts Patroni first (solo Raft, no `partner_addrs`)
- Secondary starts Patroni with `partner_addrs` pointing to primary
- After secondary joins, add partner to primary config and restart Patroni
- Create PG role/database via `psql` (Patroni bootstrap.users is unreliable in v4.1.0)

### Step 5: Generate Certificates

Start MicroShift briefly on the primary in **single-node mode** to generate certs:

```bash
# Temporary override to disable twoNode during cert generation
sudo tee /etc/microshift/config.d/zzz-init-override.yaml <<EOF
twoNode:
  enabled: false
storage:
  backend: etcd
EOF

sudo systemctl start microshift
sleep 45  # Wait for cert generation
sudo systemctl stop microshift
sudo rm /etc/microshift/config.d/zzz-init-override.yaml
```

### Step 6: Initialize and Join

```bash
# On the primary:
sudo microshift init-cluster
# This generates a join token file at /var/lib/microshift/join-token

# Copy token to secondary and run:
sudo microshift join-cluster --token $(cat /path/to/join-token)
```

### Step 7: Clean Stale Certs on Secondary

After join-cluster, remove leaf certs so they're regenerated from shared CAs:
```bash
# On the secondary:
CERTS_DIR=/var/lib/microshift/certs
for signer_dir in "$CERTS_DIR"/*/; do
  for leaf_dir in "$signer_dir"/*/; do
    [ -d "$leaf_dir" ] && sudo rm -rf "$leaf_dir"
  done
done
sudo rm -rf "$CERTS_DIR/ca-bundle" /var/lib/microshift/resources
```

### Step 8: Start Both Nodes

```bash
# On both nodes:
sudo systemctl start microshift
```

Wait ~60 seconds for all components to start.

### Step 9: Verify

```bash
# Fetch kubeconfig:
ssh user@<primary-ip> "sudo cat /var/lib/microshift/resources/kubeadmin/kubeconfig" | \
  sed "s|https://127.0.0.1:6443|https://<primary-ip>:6443|" > ~/.kube/config

# Check nodes:
oc get nodes
# NAME               STATUS   ROLES                         AGE   VERSION
# demo-1.novalocal   Ready    control-plane,master,worker   5m    ...
# demo-2.novalocal   Ready    control-plane,master,worker   3m    ...

# Check pods:
oc get pods -A

# Check Patroni:
curl -s http://<primary-ip>:8008/patroni | python3 -m json.tool
curl -s http://<secondary-ip>:8008/patroni | python3 -m json.tool
```

---

## Automated Deployment

A deployment script automates all steps:

```bash
./scripts/deploy-two-node.sh \
  --primary 192.168.5.107 \
  --secondary 192.168.7.167 \
  --vip 192.168.5.200 \
  --user fedora
```

This handles: package installation, Patroni bootstrap, binary deployment, SELinux,
CRI-O config, certificate generation, init-cluster/join-cluster, cert cleanup,
kubeconfig fetch, and `oc` CLI installation.

**Prerequisites for the deploy script**:
- Binaries already built in `_output/bin/linux_amd64/`
- SSH key access to both nodes as `--user` with passwordless sudo
- Pull secret at `/etc/crio/openshift-pull-secret` on both nodes (or added manually after)

---

## Failure Scenarios

### MicroShift Failure on One Node
1. The other node's API server continues serving immediately
2. Patroni/PostgreSQL are unaffected (independent systemd service)
3. **Result**: Zero downtime. Restart MicroShift to recover.

### Full Node Failure (PG Primary Survives)
1. Patroni loses Raft quorum, but `failsafe_mode` keeps PG writable
2. Surviving node's API server has a ~5 second blip during Patroni role transition
3. **Result**: Cluster continues on the surviving node.

### Full Node Failure (PG Replica Survives)
1. Patroni loses Raft quorum — Patroni cannot promote the replica via Raft
2. Kine loses PG connection → MicroShift restarts
3. The **failover watchdog** accumulates failure counts across restarts (persisted to disk)
4. After reaching the threshold (default: 5 restarts), the watchdog promotes the local
   PG replica via `pg_ctl promote` **before** Kine starts
5. Kine connects to the now-local PG primary and the cluster recovers
6. **Result**: Automatic recovery after ~5 restart cycles. No manual intervention needed.

### Recovery After Failure
When a failed node comes back:
1. Patroni auto-rejoins the Raft cluster
2. Raft quorum is restored; roles may swap
3. PostgreSQL streaming replication re-establishes
4. MicroShift restarts and Kine discovers the current PG primary
5. Node registers with the API server

### CRITICAL: Patroni Watchdog Must Be Disabled
Patroni has a built-in Linux watchdog feature (`/dev/watchdog`) that is designed for
3+ node clusters. In a 2-node setup, **it causes the surviving node to reboot when
the peer goes down**, completely defeating HA.

This is already configured in the deploy script (`watchdog: mode: off`), but if you're
setting up Patroni manually, you **must** include this in `patroni.yml`:

```yaml
watchdog:
  mode: off
```

Without this, rebooting one node will cause both nodes to go down.

---

## Troubleshooting

### Check Component Status
```bash
# MicroShift logs
sudo journalctl -u microshift -f

# Kine logs (separate process)
sudo journalctl _COMM=microshift-kine -f

# Patroni status
curl -s http://localhost:8008/patroni | python3 -m json.tool

# Patroni on both nodes
for IP in <primary> <secondary>; do
  echo "${IP}: $(curl -s http://${IP}:8008/patroni | python3 -c 'import sys,json; d=json.load(sys.stdin); print(f"role={d[\"role\"]} state={d[\"state\"]}")')"
done

# keepalived / VIP status
ip addr show | grep <VIP>

# PostgreSQL replication status (on PG primary)
sudo -u postgres psql -c "SELECT * FROM pg_stat_replication;"

# Port check
sudo ss -tlnp | grep -E '2379|5432|6443|8008'
```

### Common Issues

**"twoNode.vip is required"**
→ Add `twoNode.vip` to your config file.

**Patroni not found**
→ Install via pip: `sudo pip3 install "patroni[raft]" psycopg2-binary`
→ Note: Patroni is NOT in standard Fedora/RHEL repos.

**CRI-O watchdog kills CRI-O / "NetworkPluginNotReady"**
→ Install CNI plugins: `sudo dnf install -y containernetworking-plugins`
→ Install CRI-O drop-in: copy `packaging/crio.conf.d/10-microshift_amd64.conf` to `/etc/crio/crio.conf.d/`
→ Restart CRI-O: `sudo systemctl restart crio`

**ImagePullBackOff for OCP images**
→ Pull secret missing or not installed correctly.
→ Place at `/etc/crio/openshift-pull-secret` with mode `600`, then `sudo systemctl restart crio`

**TLS: certificate is valid for X, not 127.0.0.1**
→ etcd certs missing `127.0.0.1` SAN. Delete etcd leaf certs and restart MicroShift to regenerate:
```bash
sudo rm -rf /var/lib/microshift/certs/etcd-signer/etcd-serving \
  /var/lib/microshift/certs/etcd-signer/etcd-peer \
  /var/lib/microshift/certs/etcd-signer/apiserver-etcd-client
sudo systemctl restart microshift
```

**Kine panic: "invalid argument to Int63n"**
→ Kine NotifyInterval is zero. Ensure `microshift-kine` binary is built from the latest source.

**VIP not floating after node failure**
→ Check firewall allows VRRP (protocol 112)
→ Check `journalctl -u microshift` for keepalived errors
→ Verify health check script: `/usr/libexec/microshift/check-apiserver.sh`

**Join token expired**
→ Run `microshift init-cluster` again on the primary to generate a new token.

**MicroShift restart loop: "Start request repeated too quickly"**
→ Run `sudo systemctl reset-failed microshift` before restarting.

**Rebooting one node causes the other node to reboot**
→ Patroni's built-in watchdog is enabled (default). Add `watchdog: mode: off` to
  `/var/lib/microshift/patroni/patroni.yml` on both nodes, then restart Patroni:
  `sudo systemctl restart microshift-patroni`

**Failover watchdog not promoting after peer failure**
→ Check the persistent failure count: `cat /var/lib/microshift/failover-watchdog-state`
→ Check MicroShift logs for watchdog messages: `sudo journalctl -u microshift | grep -i failover`
→ Verify systemd allows enough restarts: check `StartLimitBurst=10` in
  `/etc/systemd/system/microshift.service.d/restart-limit.conf`
→ Manual promotion: `sudo -u postgres pg_ctl promote -D /var/lib/microshift/postgres/data`

---

## Configuration Reference

### twoNode section
```yaml
twoNode:
  enabled: false              # Enable 2-node HA mode
  role: ""                    # "primary" or "secondary"
  vip: ""                     # Virtual IP address
  vipInterface: ""            # Network interface for VIP (auto-detect if empty)
  peer:
    address: ""               # Peer node IP address
    hostname: ""              # Peer node hostname (optional)
  failover:
    enabled: true             # Enable automatic failover watchdog (default: true)
    failureThreshold: 5       # Consecutive failures before promoting (default: 5)
    checkInterval: 5s         # Interval between peer health checks (default: 5s)
```

### storage section (2-node additions)
```yaml
storage:
  backend: "kine"             # "etcd" (default) or "kine"
  postgresql:
    host: "127.0.0.1"         # PostgreSQL host (overridden by Patroni discovery)
    port: 5432                # PostgreSQL port
    database: "microshift"    # Database name
    user: "microshift"        # Database user
    passwordFile: "/var/lib/microshift/secrets/postgresql/password"
    sslMode: "disable"        # TLS mode for local PG connections
```

### Patroni configuration (critical settings)
```yaml
# In /var/lib/microshift/patroni/patroni.yml on each node:
watchdog:
  mode: off                   # CRITICAL: Must be off for 2-node clusters

bootstrap:
  dcs:
    failsafe_mode: true       # Prevents PG primary from demoting when Raft quorum is lost
    synchronous_mode: true    # Ensures replica has all committed data
    synchronous_mode_strict: false  # Allows primary to operate without sync standby
```
