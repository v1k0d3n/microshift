# Phase 11: NVIDIA DGX OS (ARM64) Platform Port

## Status: PLANNING

## Goal

Port the MicroShift 2-node HA deployment to NVIDIA DGX OS running on the
GB10 (Grace Blackwell) platform. This involves adapting the packaging,
security, and deployment layers — the Go binaries and core architecture
require no changes.

## Target Platform

| Property | Value |
|----------|-------|
| Platform | NVIDIA GB10 (Project Spark) |
| CPU | NVIDIA Grace (ARM64 / aarch64) |
| GPU | NVIDIA Blackwell |
| OS | NVIDIA DGX OS |
| Base | Derived from a Debian-family distribution with custom kernel and drivers |
| Security framework | AppArmor (default) |
| Package manager | apt / dpkg |
| Container runtime (stock) | containerd (NVIDIA-configured) |

## Scope

### In Scope
1. AppArmor profiles equivalent to the existing SELinux policy
2. Deployment script adaptation for DGX OS (package names, service names, paths)
3. CRI-O installation and integration alongside NVIDIA container runtime
4. Verification that statically-linked ARM64 binaries run on DGX OS kernel
5. GPU workload validation (NVIDIA device plugin / GPU operator on MicroShift)

### Out of Scope
- Changes to MicroShift Go source code (none expected)
- Upstream packaging (RPM spec, SELinux policy module) — left intact
- x86_64 / amd64 DGX OS support (GB10 is ARM64 only)

## Analysis: Why No Codebase Changes Are Needed

The MicroShift binary is **SELinux-agnostic at runtime**. All SELinux
enforcement is handled externally:

| Layer | SELinux mechanism | DGX OS equivalent |
|-------|------------------|-------------------|
| Binary execution | `kubelet_exec_t` file context | AppArmor profile or no restriction |
| Data directories | `container_var_lib_t` context | AppArmor profile for path access |
| Config directories | `kubernetes_file_t` context | AppArmor profile for path access |
| Backup dir creation | `.te` filetrans_pattern | AppArmor path rules |
| Container security | CRI-O + kernel enforcement | CRI-O + kernel enforcement (same) |
| Pod SCCs | Declarative YAML (kernel-enforced) | Declarative YAML (kernel-enforced) |

The Go binary imports `opencontainers/selinux` only as an **indirect**
dependency (via Kubernetes libraries). It never calls SELinux APIs directly.

## Implementation Plan

### Step 1: AppArmor Profiles

Create AppArmor profiles equivalent to the existing SELinux policy.

**Existing SELinux policy (what we're translating):**

`packaging/selinux/microshift.fc` defines file contexts:
- `/usr/bin/microshift`, `/usr/bin/microshift-etcd` → `kubelet_exec_t` (executable by systemd)
- `/var/lib/microshift(/.*)?` → `container_var_lib_t` (container data)
- `/var/lib/microshift-backups(/.*)?` → `container_var_lib_t`
- `/var/lib/microshift.saved(/.*)?` → `container_var_lib_t`
- `/etc/microshift(/.*)?` → `kubernetes_file_t` (config)
- `/usr/lib/microshift(/.*)?` → `kubernetes_file_t`

`packaging/selinux/microshift.te` defines process rules:
- `kubelet_t` creating `microshift-backups` or `microshift.saved` dirs under
  `var_lib_t` → auto-label as `container_var_lib_t`

**AppArmor profiles to create:**

```
packaging/apparmor/
├── usr.bin.microshift          # Profile for microshift binary
├── usr.bin.microshift-etcd     # Profile for microshift-etcd binary
├── usr.bin.microshift-kine     # Profile for microshift-kine binary (new for 2-node)
└── README.md                   # Explanation of profiles and how to install
```

Each profile grants:
- Read access to `/etc/microshift/**`
- Read/write access to `/var/lib/microshift/**`, `/var/lib/microshift-backups/**`, `/var/lib/microshift.saved/**`
- Read access to `/usr/lib/microshift/**`
- Network access (tcp, udp, raw)
- Capability grants: `net_admin`, `net_bind_service`, `sys_admin`, `chown`, `dac_override`, `fowner`, `kill`, `setuid`, `setgid`, `sys_ptrace`, `sys_resource`
- Signal handling for child process management
- Access to container runtime sockets, cgroup filesystem, proc, sys

### Step 2: DGX OS Deployment Script

Create `scripts/deploy-two-node-dgx.sh` (or parameterize the existing script)
that handles DGX OS differences:

| Concern | Fedora/RHEL (`deploy-two-node.sh`) | DGX OS adaptation |
|---------|-------------------------------------|-------------------|
| Package install | `dnf install cri-o openvswitch...` | `apt-get install cri-o openvswitch-switch...` |
| Package check | `rpm -q $pkg` | `dpkg -l $pkg` |
| SELinux setup | `semanage fcontext` + `restorecon` | `apparmor_parser -r` to load profiles |
| Policy tools | `policycoreutils-python-utils` | `apparmor-utils` |
| CRI-O config | Fedora package defaults | Needs apt repo setup + NVIDIA runtime hook |
| OVN packages | `ovn-host ovn-central` | `ovn-host ovn-central` (verify names) |
| Firewall | `firewall-cmd` (firewalld) | `ufw` or `iptables` (check DGX OS default) |
| Service names | `crio`, `openvswitch` | `crio`, `openvswitch-switch` (verify) |

### Step 3: CRI-O + NVIDIA Container Runtime Integration

DGX OS ships with containerd pre-configured for GPU workloads. MicroShift
requires CRI-O. The approach:

1. Install CRI-O from the CRI-O packaging project (has ARM64 apt packages)
2. Configure CRI-O to use `nvidia-container-runtime` as a runtime handler:
   ```toml
   [crio.runtime.runtimes.nvidia]
   runtime_path = "/usr/bin/nvidia-container-runtime"
   runtime_type = "oci"
   ```
3. Alternatively, use NVIDIA CDI (Container Device Interface) with CRI-O's
   native CDI support — this is the modern approach and avoids the legacy
   runtime hook
4. Verify containerd can coexist (different socket paths) or disable it

### Step 4: ARM64 Binary Verification

- Verify the statically-linked ARM64 binaries execute on DGX OS kernel
- Check for any kernel module dependencies (openvswitch, etc.)
- Verify Patroni + PostgreSQL ARM64 packages are available
- Test Kine → PostgreSQL connectivity on DGX OS

### Step 5: GPU Workload Validation

Once the 2-node HA cluster is running on two GB10 nodes:

1. Deploy NVIDIA GPU Operator or device plugin via MicroShift
2. Verify GPU resources appear on nodes (`nvidia.com/gpu`)
3. Run a test inference workload across both nodes
4. Test GPU workload failover: kill the node running inference,
   verify rescheduling to the surviving node

## Risks and Mitigations

| Risk | Impact | Mitigation |
|------|--------|------------|
| DGX OS kernel missing openvswitch module | OVN-Kubernetes CNI won't function | Check `modprobe openvswitch`; if missing, build module or use alternative CNI |
| CRI-O conflicts with containerd | Socket/resource contention | Disable containerd if not needed; or run on separate sockets |
| AppArmor too restrictive | MicroShift fails at runtime | Start in complain mode (`aa-complain`), audit logs, then tighten |
| DGX OS immutable/locked rootfs | Can't install packages | Check filesystem layout; use overlay or writable partitions |
| NVIDIA runtime hook not compatible with CRI-O | GPU containers fail | Use CDI approach instead of legacy runtime hook |
| ARM64 PostgreSQL performance | Patroni/PG overhead on Grace CPU | Grace is a high-performance ARM64 — unlikely to be an issue |

## File Inventory (Planned)

| File | Action | Description |
|------|--------|-------------|
| `packaging/apparmor/usr.bin.microshift` | CREATE | AppArmor profile for microshift |
| `packaging/apparmor/usr.bin.microshift-etcd` | CREATE | AppArmor profile for microshift-etcd |
| `packaging/apparmor/usr.bin.microshift-kine` | CREATE | AppArmor profile for microshift-kine |
| `packaging/apparmor/README.md` | CREATE | Installation and usage instructions |
| `scripts/deploy-two-node-dgx.sh` | CREATE | DGX OS deployment script |
| `.claude/plans/14-dgx-os-arm64-port.md` | CREATE | This plan |

## Open Questions

1. **DGX OS version**: Which DGX OS release is running on the GB10? Package
   availability may vary.
2. **Rootfs mutability**: Is the DGX OS rootfs writable, or does it use an
   immutable image model?
3. **Firewall**: Does DGX OS use `ufw`, `iptables`, `nftables`, or none?
4. **AppArmor mode**: Is AppArmor enforcing by default on DGX OS, or permissive/disabled?
5. **containerd dependency**: Do any DGX OS GPU management services depend on
   containerd being active?
6. **OVS kernel module**: Is openvswitch built into or available for the DGX OS kernel?
7. **Network topology**: Same flat network as demo-1/demo-2, or different
   for the GB10 nodes?
