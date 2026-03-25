#!/bin/bash
# deploy-two-node.sh — Deploy MicroShift 2-Node HA cluster
#
# Automates the full deployment of a 2-node MicroShift HA cluster including
# all runtime dependencies. Designed for Fedora/RHEL systems.
#
# Usage:
#   ./scripts/deploy-two-node.sh [--primary IP] [--secondary IP] [--vip IP] [--user USER]

set -euo pipefail

PRIMARY_IP="${PRIMARY_IP:-192.168.5.107}"
SECONDARY_IP="${SECONDARY_IP:-192.168.7.167}"
VIP="${VIP:-192.168.5.200}"
SSH_USER="${SSH_USER:-$(whoami)}"

while [[ $# -gt 0 ]]; do
    case "$1" in
        --primary)   PRIMARY_IP="$2"; shift 2 ;;
        --secondary) SECONDARY_IP="$2"; shift 2 ;;
        --vip)       VIP="$2"; shift 2 ;;
        --user)      SSH_USER="$2"; shift 2 ;;
        --help|-h)
            echo "Usage: $0 [--primary IP] [--secondary IP] [--vip IP] [--user USER]"
            echo "  --primary IP     Primary node IP (default: $PRIMARY_IP)"
            echo "  --secondary IP   Secondary node IP (default: $SECONDARY_IP)"
            echo "  --vip IP         Virtual IP address (default: $VIP)"
            echo "  --user USER      SSH user with sudo (default: current user)"
            exit 0 ;;
        *) echo "Unknown option: $1"; exit 1 ;;
    esac
done

SCRIPT_DIR="$(cd "$(dirname "$0")/.." && pwd)"
ARCH=$(uname -m | sed 's/x86_64/amd64/' | sed 's/aarch64/arm64/')
BIN_DIR="${SCRIPT_DIR}/_output/bin/linux_${ARCH}"
PACKAGING_DIR="${SCRIPT_DIR}/packaging"
SSH_OPTS="-o StrictHostKeyChecking=no -o ConnectTimeout=10"
OC_VERSION="${OC_VERSION:-stable}"
OC_MIRROR="https://mirror.openshift.com/pub/openshift-v4/clients/ocp/${OC_VERSION}"

# ── Helpers ─────────────────────────────────────────────────────────────────

# Run a command on a remote node. Uses a heredoc to avoid quoting issues.
run_on() {
    local node="$1"; shift
    ssh ${SSH_OPTS} "${SSH_USER}@${node}" "sudo bash -s" <<REMOTECMD
$@
REMOTECMD
}

# Copy a local file to a remote path (via tmp + sudo mv + SELinux relabel).
copy_to() {
    local src="$1" node="$2" dest="$3" mode="${4:-755}"
    local tmp="/tmp/deploy-ms-$(basename "${src}").$$"
    scp ${SSH_OPTS} "${src}" "${SSH_USER}@${node}:${tmp}"
    ssh ${SSH_OPTS} "${SSH_USER}@${node}" \
        "sudo mv '${tmp}' '${dest}' && sudo chown root:root '${dest}' && sudo chmod ${mode} '${dest}' && sudo restorecon -v '${dest}' 2>/dev/null || true"
}

echo "=== MicroShift 2-Node HA Deployment ==="
echo "Primary:   ${PRIMARY_IP}"
echo "Secondary: ${SECONDARY_IP}"
echo "VIP:       ${VIP}"
echo "SSH User:  ${SSH_USER}"
echo "Binaries:  ${BIN_DIR}"
echo ""

# ── Preflight checks ───────────────────────────────────────────────────────

for bin in microshift microshift-kine; do
    if [[ ! -f "${BIN_DIR}/${bin}" ]]; then
        echo "ERROR: ${BIN_DIR}/${bin} not found. Build first."
        exit 1
    fi
done

echo "--- Preflight: checking SSH connectivity ---"
for NODE in "${PRIMARY_IP}" "${SECONDARY_IP}"; do
    if ! ssh ${SSH_OPTS} "${SSH_USER}@${NODE}" "echo ok" &>/dev/null; then
        echo "ERROR: Cannot SSH to ${SSH_USER}@${NODE}"; exit 1
    fi
    if ! ssh ${SSH_OPTS} "${SSH_USER}@${NODE}" "sudo -n true" &>/dev/null; then
        echo "ERROR: ${SSH_USER} lacks passwordless sudo on ${NODE}"; exit 1
    fi
    echo "  ${NODE}: OK"
done

# ── Step 1: Install system packages ────────────────────────────────────────

echo ""
echo "--- Step 1: Install system packages on both nodes ---"
for NODE in "${PRIMARY_IP}" "${SECONDARY_IP}"; do
    echo "  [${NODE}] Checking and installing packages..."
    run_on "${NODE}" '
        NEEDED=""
        for pkg in cri-o openvswitch ovn-host ovn-central keepalived \
                   conntrack-tools containernetworking-plugins jq \
                   postgresql-server python3-pip; do
            if ! rpm -q "$pkg" &>/dev/null; then
                NEEDED="$NEEDED $pkg"
            fi
        done
        if [ -n "$NEEDED" ]; then
            echo "  Installing:$NEEDED"
            dnf install -y $NEEDED 2>&1 | tail -5
        else
            echo "  All system packages already installed."
        fi
    '
done

# ── Step 2: Install Patroni via pip (not in Fedora repos) ──────────────────

echo ""
echo "--- Step 2: Install Patroni on both nodes ---"
for NODE in "${PRIMARY_IP}" "${SECONDARY_IP}"; do
    echo "  [${NODE}] Checking Patroni..."
    run_on "${NODE}" '
        if command -v patroni &>/dev/null; then
            echo "  Patroni already installed: $(patroni --version 2>&1 || true)"
        else
            echo "  Installing patroni via pip..."
            pip3 install "patroni[raft]" psycopg2-binary 2>&1 | tail -3
            echo "  Installed: $(patroni --version 2>&1 || true)"
        fi
    '
done

# ── Step 2b: Install oc CLI (locally and on both nodes) ───────────────────

echo ""
echo "--- Step 2b: Install OpenShift CLI (oc) ---"

# Install oc locally if not present
if ! command -v oc &>/dev/null; then
    echo "  [local] Installing oc..."
    OC_TMP=$(mktemp -d)
    if curl -sL "${OC_MIRROR}/openshift-client-linux.tar.gz" -o "${OC_TMP}/oc.tar.gz" 2>/dev/null; then
        tar xzf "${OC_TMP}/oc.tar.gz" -C "${OC_TMP}" oc kubectl 2>/dev/null || true
        if [ -f "${OC_TMP}/oc" ]; then
            sudo mv "${OC_TMP}/oc" /usr/local/bin/oc
            sudo chmod 755 /usr/local/bin/oc
            # Also install kubectl from the same bundle if not present
            if ! command -v kubectl &>/dev/null && [ -f "${OC_TMP}/kubectl" ]; then
                sudo mv "${OC_TMP}/kubectl" /usr/local/bin/kubectl
                sudo chmod 755 /usr/local/bin/kubectl
            fi
            echo "  [local] oc installed: $(oc version --client 2>/dev/null | head -1)"
        else
            echo "  [local] WARNING: oc binary not found in tarball"
        fi
    else
        echo "  [local] WARNING: Could not download oc from ${OC_MIRROR}"
    fi
    rm -rf "${OC_TMP}"
else
    echo "  [local] oc already installed: $(oc version --client 2>/dev/null | head -1)"
fi

# Install oc on both nodes
for NODE in "${PRIMARY_IP}" "${SECONDARY_IP}"; do
    echo "  [${NODE}] Checking oc..."
    run_on "${NODE}" "
        if command -v oc &>/dev/null; then
            echo '  oc already installed:' \$(oc version --client 2>/dev/null | head -1)
        else
            echo '  Installing oc...'
            OC_TMP=\$(mktemp -d)
            if curl -sL '${OC_MIRROR}/openshift-client-linux.tar.gz' -o \"\${OC_TMP}/oc.tar.gz\" 2>/dev/null; then
                tar xzf \"\${OC_TMP}/oc.tar.gz\" -C \"\${OC_TMP}\" oc kubectl 2>/dev/null || true
                if [ -f \"\${OC_TMP}/oc\" ]; then
                    mv \"\${OC_TMP}/oc\" /usr/local/bin/oc
                    chmod 755 /usr/local/bin/oc
                    if ! command -v kubectl &>/dev/null && [ -f \"\${OC_TMP}/kubectl\" ]; then
                        mv \"\${OC_TMP}/kubectl\" /usr/local/bin/kubectl
                        chmod 755 /usr/local/bin/kubectl
                    fi
                    echo '  oc installed:' \$(oc version --client 2>/dev/null | head -1)
                else
                    echo '  WARNING: oc binary not found in tarball'
                fi
            else
                echo '  WARNING: Could not download oc'
            fi
            rm -rf \"\${OC_TMP}\"
        fi
    "
done

# ── Step 3: Copy binaries ──────────────────────────────────────────────────

echo ""
echo "--- Step 3: Copy MicroShift binaries to both nodes ---"
for NODE in "${PRIMARY_IP}" "${SECONDARY_IP}"; do
    echo "  [${NODE}] Copying..."
    copy_to "${BIN_DIR}/microshift"      "${NODE}" "/usr/bin/microshift"
    copy_to "${BIN_DIR}/microshift-kine" "${NODE}" "/usr/bin/microshift-kine"
    if [[ -f "${BIN_DIR}/microshift-etcd" ]]; then
        copy_to "${BIN_DIR}/microshift-etcd" "${NODE}" "/usr/bin/microshift-etcd"
    fi
done

# ── Step 4: Install systemd units and helper scripts ───────────────────────

echo ""
echo "--- Step 4: Install systemd units and helpers on both nodes ---"
for NODE in "${PRIMARY_IP}" "${SECONDARY_IP}"; do
    echo "  [${NODE}] Installing..."
    # Install microshift.service with absolute ExecStart path
    # (the packaged version uses a relative path that requires WorkingDirectory)
    sed 's|^ExecStart=microshift|ExecStart=/usr/bin/microshift|' \
        "${PACKAGING_DIR}/systemd/microshift.service" > /tmp/microshift.service.$$
    copy_to "/tmp/microshift.service.$$" \
        "${NODE}" "/etc/systemd/system/microshift.service" 644
    rm -f /tmp/microshift.service.$$
    copy_to "${PACKAGING_DIR}/systemd/microshift-ovs-init.service" \
        "${NODE}" "/etc/systemd/system/microshift-ovs-init.service" 644
    copy_to "${PACKAGING_DIR}/systemd/configure-ovs.sh" \
        "${NODE}" "/usr/bin/configure-ovs.sh"
    copy_to "${PACKAGING_DIR}/systemd/configure-ovs-microshift.sh" \
        "${NODE}" "/usr/bin/configure-ovs-microshift.sh"
    copy_to "${PACKAGING_DIR}/tuned/microshift-cleanup-kubelet.service" \
        "${NODE}" "/etc/systemd/system/microshift-cleanup-kubelet.service" 644
    run_on "${NODE}" 'systemctl daemon-reload'
done

# ── Step 4b: Configure SELinux file contexts ───────────────────────────────

echo ""
echo "--- Step 4b: Configure SELinux on both nodes ---"
for NODE in "${PRIMARY_IP}" "${SECONDARY_IP}"; do
    echo "  [${NODE}] Setting SELinux file contexts..."
    run_on "${NODE}" '
        if ! command -v semanage &>/dev/null; then
            dnf install -y policycoreutils-python-utils 2>&1 | tail -2
        fi

        # MicroShift binaries need kubelet_exec_t to be started by systemd
        for bin in /usr/bin/microshift /usr/bin/microshift-etcd /usr/bin/microshift-kine; do
            if [ -f "$bin" ]; then
                semanage fcontext -a -t kubelet_exec_t "$bin" 2>/dev/null || \
                semanage fcontext -m -t kubelet_exec_t "$bin" 2>/dev/null || true
            fi
        done

        # MicroShift data and config directories
        semanage fcontext -a -t container_var_lib_t "/var/lib/microshift(/.*)?" 2>/dev/null || \
        semanage fcontext -m -t container_var_lib_t "/var/lib/microshift(/.*)?" 2>/dev/null || true

        semanage fcontext -a -t container_var_lib_t "/var/lib/microshift-backups(/.*)?" 2>/dev/null || \
        semanage fcontext -m -t container_var_lib_t "/var/lib/microshift-backups(/.*)?" 2>/dev/null || true

        semanage fcontext -a -t kubernetes_file_t "/etc/microshift(/.*)?" 2>/dev/null || \
        semanage fcontext -m -t kubernetes_file_t "/etc/microshift(/.*)?" 2>/dev/null || true

        # OVS helper scripts
        for script in /usr/bin/configure-ovs.sh /usr/bin/configure-ovs-microshift.sh; do
            if [ -f "$script" ]; then
                semanage fcontext -a -t bin_t "$script" 2>/dev/null || \
                semanage fcontext -m -t bin_t "$script" 2>/dev/null || true
            fi
        done

        # Apply contexts
        restorecon -Rv /usr/bin/microshift* 2>/dev/null || true
        restorecon -Rv /usr/bin/configure-ovs*.sh 2>/dev/null || true
        restorecon -Rv /etc/microshift 2>/dev/null || true
        if [ -d /var/lib/microshift ]; then
            restorecon -Rv /var/lib/microshift 2>/dev/null || true
        fi

        echo "    SELinux contexts applied."
    '
done

# ── Step 5: Enable and start prerequisite services ─────────────────────────

echo ""
echo "--- Step 5: Enable and start prerequisite services ---"
for NODE in "${PRIMARY_IP}" "${SECONDARY_IP}"; do
    echo "  [${NODE}] Starting services..."
    run_on "${NODE}" '
        systemctl enable --now crio 2>&1 || true
        systemctl enable --now openvswitch 2>&1 || true
        systemctl enable --now NetworkManager 2>&1 || true
        systemctl enable microshift-ovs-init 2>&1 || true
        systemctl enable microshift-cleanup-kubelet 2>&1 || true
    '
    # Verify
    for svc in crio openvswitch; do
        if run_on "${NODE}" "systemctl is-active ${svc}" &>/dev/null; then
            echo "    ${svc}: running"
        else
            echo "    WARNING: ${svc} not running on ${NODE}"
            echo "      Debug: ssh ${SSH_USER}@${NODE} 'sudo journalctl -u ${svc} --no-pager -n 20'"
        fi
    done
done

# ── Step 6: Write MicroShift configuration ─────────────────────────────────

echo ""
echo "--- Step 6: Write MicroShift configuration ---"

echo "  [${PRIMARY_IP}] Writing primary config..."
ssh ${SSH_OPTS} "${SSH_USER}@${PRIMARY_IP}" \
    "sudo mkdir -p /etc/microshift/config.d && sudo tee /etc/microshift/config.d/two-node.yaml >/dev/null" <<EOF
twoNode:
  enabled: true
  role: primary
  vip: "${VIP}"
  peer:
    address: "${SECONDARY_IP}"
storage:
  backend: kine
EOF

echo "  [${SECONDARY_IP}] Writing secondary config..."
ssh ${SSH_OPTS} "${SSH_USER}@${SECONDARY_IP}" \
    "sudo mkdir -p /etc/microshift/config.d && sudo tee /etc/microshift/config.d/two-node.yaml >/dev/null" <<EOF
twoNode:
  enabled: true
  role: secondary
  vip: "${VIP}"
  peer:
    address: "${PRIMARY_IP}"
storage:
  backend: kine
EOF

# ── Step 7: Configure firewall ─────────────────────────────────────────────

echo ""
echo "--- Step 7: Configure firewall on both nodes ---"
for NODE in "${PRIMARY_IP}" "${SECONDARY_IP}"; do
    echo "  [${NODE}] Configuring firewall..."
    run_on "${NODE}" '
        if command -v firewall-cmd &>/dev/null && systemctl is-active firewalld &>/dev/null; then
            firewall-cmd --add-port=6443/tcp --permanent 2>/dev/null || true
            firewall-cmd --add-port=10250/tcp --permanent 2>/dev/null || true
            firewall-cmd --add-port=5432/tcp --permanent 2>/dev/null || true
            firewall-cmd --add-port=8008/tcp --permanent 2>/dev/null || true
            firewall-cmd --add-port=8009/tcp --permanent 2>/dev/null || true
            firewall-cmd --add-protocol=vrrp --permanent 2>/dev/null || true
            firewall-cmd --add-port=6641/tcp --permanent 2>/dev/null || true
            firewall-cmd --add-port=6642/tcp --permanent 2>/dev/null || true
            firewall-cmd --add-port=6081/udp --permanent 2>/dev/null || true
            firewall-cmd --reload 2>/dev/null || true
            echo "    Firewall configured."
        else
            echo "    firewalld not active, skipping."
        fi
    '
done

# ── Step 8: Render Patroni configs and start PostgreSQL ────────────────────
#
# Following the K3s model: PostgreSQL is managed separately from MicroShift.
# Patroni runs as a standalone systemd service. MicroShift simply connects
# to an already-running PostgreSQL via Kine.

echo ""
echo "--- Step 8: Set up Patroni and PostgreSQL ---"

# Install the Patroni systemd service
for NODE in "${PRIMARY_IP}" "${SECONDARY_IP}"; do
    copy_to "${PACKAGING_DIR}/systemd/microshift-patroni.service" \
        "${NODE}" "/etc/systemd/system/microshift-patroni.service" 644
    run_on "${NODE}" 'systemctl daemon-reload'
done

echo "  [${PRIMARY_IP}] Rendering Patroni config (primary, solo bootstrap)..."
run_on "${PRIMARY_IP}" "
    mkdir -p /var/lib/microshift/patroni/raft /var/lib/microshift/postgres /var/run/postgresql
    chown -R postgres:postgres /var/lib/microshift/patroni /var/lib/microshift/postgres /var/run/postgresql
    chmod 711 /var/lib/microshift

    # Read passwords
    DB_PW=\$(cat /var/lib/microshift/secrets/postgresql/password 2>/dev/null || echo 'changeme')
    REPL_PW=\$(cat /var/lib/microshift/secrets/postgresql/replication-password 2>/dev/null || echo 'changeme')
    SU_PW=\$(cat /var/lib/microshift/secrets/postgresql/superuser-password 2>/dev/null || echo 'changeme')

    cat > /var/lib/microshift/patroni/patroni.yml <<PCFG
scope: microshift-postgres
name: \$(hostname)

restapi:
  listen: 0.0.0.0:8008
  connect_address: ${PRIMARY_IP}:8008

raft:
  data_dir: /var/lib/microshift/patroni/raft
  self_addr: ${PRIMARY_IP}:8009

bootstrap:
  dcs:
    ttl: 30
    loop_wait: 10
    retry_timeout: 10
    maximum_lag_on_failover: 0
    synchronous_mode: true
    synchronous_mode_strict: false
    failsafe_mode: true
    postgresql:
      use_pg_rewind: true
      parameters:
        wal_level: replica
        max_wal_senders: 3
        synchronous_commit: remote_apply
        synchronous_standby_names: '*'
        max_connections: 100
        shared_buffers: 128MB
        wal_keep_size: 256MB
        listen_addresses: '*'

  initdb:
    - encoding: UTF8
    - data-checksums

  pg_hba:
    - local all all peer
    - host all all 127.0.0.1/32 scram-sha-256
    - host all all ${PRIMARY_IP}/32 scram-sha-256
    - host all all ${SECONDARY_IP}/32 scram-sha-256
    - host replication replicator ${PRIMARY_IP}/32 scram-sha-256
    - host replication replicator ${SECONDARY_IP}/32 scram-sha-256

  users:
    microshift:
      password: \${DB_PW}
      options:
        - createrole
        - createdb
    replicator:
      password: \${REPL_PW}
      options:
        - replication

postgresql:
  listen: 0.0.0.0:5432
  connect_address: ${PRIMARY_IP}:5432
  data_dir: /var/lib/microshift/postgres/data
  pgpass: /var/lib/microshift/postgres/pgpass
  authentication:
    superuser:
      username: postgres
      password: \${SU_PW}
    replication:
      username: replicator
      password: \${REPL_PW}
  parameters:
    unix_socket_directories: /var/run/postgresql

tags:
  nofailover: false
  noloadbalance: false
  clonefrom: false
  nosync: false
PCFG
    chown postgres:postgres /var/lib/microshift/patroni/patroni.yml
    chmod 600 /var/lib/microshift/patroni/patroni.yml
"
echo "  Primary Patroni config written."

echo "  [${PRIMARY_IP}] Starting Patroni (solo bootstrap)..."
run_on "${PRIMARY_IP}" 'systemctl enable --now microshift-patroni'
echo "  Waiting 30s for PostgreSQL to bootstrap..."
sleep 30

echo "  Checking primary PostgreSQL..."
if curl -s http://${PRIMARY_IP}:8008/patroni 2>/dev/null | python3 -c "import sys,json; d=json.load(sys.stdin); print(f'  Patroni: state={d[\"state\"]} role={d[\"role\"]}')" 2>/dev/null; then
    :
else
    echo "  WARNING: Patroni not responding on primary. Check: ssh ${SSH_USER}@${PRIMARY_IP} 'sudo journalctl -u microshift-patroni -n 20'"
fi

echo ""
echo "  [${SECONDARY_IP}] Rendering Patroni config (secondary, with partner)..."
run_on "${SECONDARY_IP}" "
    mkdir -p /var/lib/microshift/patroni/raft /var/lib/microshift/postgres /var/run/postgresql
    chown -R postgres:postgres /var/lib/microshift/patroni /var/lib/microshift/postgres /var/run/postgresql
    chmod 711 /var/lib/microshift

    DB_PW=\$(cat /var/lib/microshift/secrets/postgresql/password 2>/dev/null || echo 'changeme')
    REPL_PW=\$(cat /var/lib/microshift/secrets/postgresql/replication-password 2>/dev/null || echo 'changeme')
    SU_PW=\$(cat /var/lib/microshift/secrets/postgresql/superuser-password 2>/dev/null || echo 'changeme')

    cat > /var/lib/microshift/patroni/patroni.yml <<PCFG
scope: microshift-postgres
name: \$(hostname)

restapi:
  listen: 0.0.0.0:8008
  connect_address: ${SECONDARY_IP}:8008

raft:
  data_dir: /var/lib/microshift/patroni/raft
  self_addr: ${SECONDARY_IP}:8009
  partner_addrs:
    - ${PRIMARY_IP}:8009

bootstrap:
  dcs:
    ttl: 30
    loop_wait: 10
    retry_timeout: 10
    maximum_lag_on_failover: 0
    synchronous_mode: true
    synchronous_mode_strict: false
    failsafe_mode: true
    postgresql:
      use_pg_rewind: true
      parameters:
        wal_level: replica
        max_wal_senders: 3
        synchronous_commit: remote_apply
        synchronous_standby_names: '*'
        max_connections: 100
        shared_buffers: 128MB
        wal_keep_size: 256MB
        listen_addresses: '*'

  initdb:
    - encoding: UTF8
    - data-checksums

  pg_hba:
    - local all all peer
    - host all all 127.0.0.1/32 scram-sha-256
    - host all all ${PRIMARY_IP}/32 scram-sha-256
    - host all all ${SECONDARY_IP}/32 scram-sha-256
    - host replication replicator ${PRIMARY_IP}/32 scram-sha-256
    - host replication replicator ${SECONDARY_IP}/32 scram-sha-256

  users:
    microshift:
      password: \${DB_PW}
      options:
        - createrole
        - createdb
    replicator:
      password: \${REPL_PW}
      options:
        - replication

postgresql:
  listen: 0.0.0.0:5432
  connect_address: ${SECONDARY_IP}:5432
  data_dir: /var/lib/microshift/postgres/data
  pgpass: /var/lib/microshift/postgres/pgpass
  authentication:
    superuser:
      username: postgres
      password: \${SU_PW}
    replication:
      username: replicator
      password: \${REPL_PW}
  parameters:
    unix_socket_directories: /var/run/postgresql

tags:
  nofailover: false
  noloadbalance: false
  clonefrom: false
  nosync: false
PCFG
    chown postgres:postgres /var/lib/microshift/patroni/patroni.yml
    chmod 600 /var/lib/microshift/patroni/patroni.yml
"
echo "  Secondary Patroni config written."

echo "  [${SECONDARY_IP}] Starting Patroni (joins primary)..."
run_on "${SECONDARY_IP}" 'systemctl enable --now microshift-patroni'
echo "  Waiting 30s for replication to establish..."
sleep 30

# Now add the partner to the primary's Patroni config and restart
echo "  [${PRIMARY_IP}] Adding partner to primary Patroni config..."
run_on "${PRIMARY_IP}" "
    sed -i '/self_addr:/a\\  partner_addrs:\\n    - ${SECONDARY_IP}:8009' /var/lib/microshift/patroni/patroni.yml
    systemctl restart microshift-patroni
"
sleep 10

echo ""
echo "  === PostgreSQL cluster status ==="
for NODE in "${PRIMARY_IP}" "${SECONDARY_IP}"; do
    PATRONI=$(curl -s --max-time 3 http://${NODE}:8008/patroni 2>/dev/null)
    if [ -n "$PATRONI" ]; then
        echo "  ${NODE}: $(echo $PATRONI | python3 -c "import sys,json; d=json.load(sys.stdin); print(f'state={d[\"state\"]} role={d[\"role\"]}')" 2>/dev/null)"
    else
        echo "  ${NODE}: Patroni not responding"
    fi
done

# ── Step 8b: Create PostgreSQL user and database ───────────────────────────
#
# Patroni's bootstrap.users config doesn't reliably create roles in v4.1.0.
# Create them explicitly on the primary. The secondary gets them via replication.

echo ""
echo "--- Step 8b: Create PostgreSQL role and database ---"

# Find which node is the PG primary
PG_PRIMARY=""
for NODE in "${PRIMARY_IP}" "${SECONDARY_IP}"; do
    ROLE=$(curl -s --max-time 3 http://${NODE}:8008/patroni 2>/dev/null | python3 -c "import sys,json; print(json.load(sys.stdin).get('role',''))" 2>/dev/null)
    if [ "$ROLE" = "primary" ]; then
        PG_PRIMARY="${NODE}"
        break
    fi
done

if [ -z "${PG_PRIMARY}" ]; then
    echo "  WARNING: Could not determine PG primary. Trying ${PRIMARY_IP}..."
    PG_PRIMARY="${PRIMARY_IP}"
fi
echo "  PG primary is ${PG_PRIMARY}"

DB_PW=$(ssh ${SSH_OPTS} "${SSH_USER}@${PG_PRIMARY}" "sudo cat /var/lib/microshift/secrets/postgresql/password")
run_on "${PG_PRIMARY}" "
    export PGPASSWORD=\$(cat /var/lib/microshift/secrets/postgresql/superuser-password)
    # Create role if not exists
    if ! psql -h 127.0.0.1 -U postgres -tAc \"SELECT 1 FROM pg_roles WHERE rolname='microshift'\" | grep -q 1; then
        psql -h 127.0.0.1 -U postgres -c \"CREATE ROLE microshift WITH LOGIN PASSWORD '${DB_PW}' CREATEDB CREATEROLE\"
        echo '  Role microshift created.'
    else
        echo '  Role microshift already exists.'
    fi
    # Create database if not exists
    if ! psql -h 127.0.0.1 -U postgres -tAc \"SELECT 1 FROM pg_database WHERE datname='microshift'\" | grep -q 1; then
        psql -h 127.0.0.1 -U postgres -c \"CREATE DATABASE microshift OWNER microshift\"
        echo '  Database microshift created.'
    else
        echo '  Database microshift already exists.'
    fi
"

# ── Step 9: Generate certificates on primary ───────────────────────────────

echo ""
echo "--- Step 9: Generate certificates (single-node start on primary) ---"
run_on "${PRIMARY_IP}" '
    cat > /etc/microshift/config.d/zzz-init-override.yaml <<INITCFG
twoNode:
  enabled: false
storage:
  backend: etcd
INITCFG
'
echo "  [${PRIMARY_IP}] Starting MicroShift (single-node mode for certs)..."
run_on "${PRIMARY_IP}" 'systemctl reset-failed microshift 2>/dev/null || true; systemctl start microshift'
echo "  Waiting 45 seconds for certificate generation..."
sleep 45
run_on "${PRIMARY_IP}" 'systemctl stop microshift; sleep 3'
run_on "${PRIMARY_IP}" 'rm -f /etc/microshift/config.d/zzz-init-override.yaml'
echo "  Certificates generated."

# ── Step 10: Run init-cluster on primary ───────────────────────────────────

echo ""
echo "--- Step 10: Run init-cluster on primary ---"
run_on "${PRIMARY_IP}" 'microshift init-cluster' 2>&1 | grep -v 'microshift join-cluster' | head -20 || true
TOKEN=$(ssh ${SSH_OPTS} "${SSH_USER}@${PRIMARY_IP}" "sudo cat /var/lib/microshift/join-token 2>/dev/null" || true)
if [[ -z "${TOKEN}" ]]; then
    echo "ERROR: Failed to get join token from primary."
    exit 1
fi
echo "  Join token obtained (${#TOKEN} bytes)"

# ── Step 11: Run join-cluster on secondary ─────────────────────────────────

echo ""
echo "--- Step 11: Run join-cluster on secondary ---"
TOKEN_TMP="/tmp/microshift-join-token.$$"
echo "${TOKEN}" > "${TOKEN_TMP}"
scp ${SSH_OPTS} "${TOKEN_TMP}" "${SSH_USER}@${SECONDARY_IP}:${TOKEN_TMP}"
rm -f "${TOKEN_TMP}"
ssh ${SSH_OPTS} "${SSH_USER}@${SECONDARY_IP}" \
    "sudo microshift join-cluster --token \$(cat ${TOKEN_TMP}) && sudo rm -f ${TOKEN_TMP}" 2>&1 | grep -v '^=== Join Token'
echo "  Join-cluster completed."

# ── Step 11b: Clean stale leaf certs on secondary ──────────────────────────
#
# The join-cluster command writes shared CA certs from the primary, but the
# secondary may have stale leaf certs from a previous run signed by a different
# CA. Delete all leaf certs (subdirectories of signer dirs) so initCerts()
# regenerates them using the shared CAs.

echo ""
echo "--- Step 11b: Clean stale leaf certs on secondary ---"
run_on "${SECONDARY_IP}" '
    CERTS_DIR=/var/lib/microshift/certs
    if [ -d "$CERTS_DIR" ]; then
        # Remove leaf cert subdirectories (but keep the CA cert/key in each signer dir)
        for signer_dir in "$CERTS_DIR"/*/; do
            for leaf_dir in "$signer_dir"/*/; do
                if [ -d "$leaf_dir" ]; then
                    echo "  Removing stale leaf certs: $leaf_dir"
                    rm -rf "$leaf_dir"
                fi
            done
        done
        # Also remove the ca-bundle dir so it gets regenerated
        rm -rf "$CERTS_DIR/ca-bundle"
        # Remove kubeconfigs so they get regenerated with correct certs
        rm -rf /var/lib/microshift/resources
    fi
    echo "  Leaf certs cleaned."
'

# ── Step 12: Start MicroShift on both nodes ────────────────────────────────

echo ""
echo "--- Step 12: Start MicroShift on both nodes ---"
echo "  PostgreSQL is already running via Patroni."
echo "  MicroShift will connect via Kine."
run_on "${PRIMARY_IP}" 'systemctl reset-failed microshift 2>/dev/null || true; systemctl start --no-block microshift' &
run_on "${SECONDARY_IP}" 'systemctl reset-failed microshift 2>/dev/null || true; systemctl start --no-block microshift' &
wait
echo "  Both nodes starting."

# ── Step 13: Fetch kubeconfig for local oc access ─────────────────────────

echo ""
echo "--- Step 13: Fetch kubeconfig for local oc access ---"

KUBECONFIG_REMOTE="/var/lib/microshift/resources/kubeadmin/kubeconfig"
KUBECONFIG_LOCAL="${HOME}/.kube/config"

# Wait briefly then try to pull kubeconfig from the primary node
# (it may not exist yet if MicroShift is still starting)
mkdir -p "${HOME}/.kube"
KUBECONFIG_FETCHED=false
for attempt in 1 2 3; do
    KUBECONFIG_RAW=$(ssh ${SSH_OPTS} "${SSH_USER}@${PRIMARY_IP}" "sudo cat ${KUBECONFIG_REMOTE} 2>/dev/null" || true)
    if [ -n "${KUBECONFIG_RAW}" ]; then
        # Rewrite server URL: localhost -> primary node IP so oc works remotely
        echo "${KUBECONFIG_RAW}" | sed "s|https://127.0.0.1:6443|https://${PRIMARY_IP}:6443|g; s|https://localhost:6443|https://${PRIMARY_IP}:6443|g" > "${KUBECONFIG_LOCAL}"
        chmod 600 "${KUBECONFIG_LOCAL}"
        echo "  Kubeconfig written to ${KUBECONFIG_LOCAL}"
        echo "  API server: https://${PRIMARY_IP}:6443"
        KUBECONFIG_FETCHED=true
        break
    fi
    if [ "${attempt}" -lt 3 ]; then
        echo "  Kubeconfig not available yet (attempt ${attempt}/3), waiting 20s..."
        sleep 20
    fi
done

if [ "${KUBECONFIG_FETCHED}" = false ]; then
    echo "  Kubeconfig not yet available (MicroShift may still be starting)."
    echo "  Fetch it manually once MicroShift is ready:"
    echo "    ssh ${SSH_USER}@${PRIMARY_IP} 'sudo cat ${KUBECONFIG_REMOTE}' | \\"
    echo "      sed 's|https://127.0.0.1:6443|https://${PRIMARY_IP}:6443|' > ${KUBECONFIG_LOCAL}"
fi

# ── Done ───────────────────────────────────────────────────────────────────

echo ""
echo "=== Deployment Complete ==="
echo ""
echo "Wait ~60 seconds for all components to start, then verify:"
echo ""
if [ "${KUBECONFIG_FETCHED}" = true ]; then
    echo "  oc get nodes"
    echo "  oc get pods -A"
    echo "  oc logs -n openshift-kube-apiserver <pod>"
else
    echo "  # Once kubeconfig is fetched:"
    echo "  oc get nodes"
    echo "  oc get pods -A"
fi
echo ""
echo "  # Or directly on a node:"
echo "  ssh ${SSH_USER}@${PRIMARY_IP} 'sudo oc --kubeconfig ${KUBECONFIG_REMOTE} get nodes'"
echo ""
echo "Check Patroni:"
echo "  curl -s http://${PRIMARY_IP}:8008/patroni | python3 -m json.tool"
echo ""
echo "Check VIP:"
echo "  ssh ${SSH_USER}@${PRIMARY_IP} 'ip addr show | grep ${VIP}'"
