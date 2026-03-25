#!/bin/bash
# MicroShift 2-Node HA: API server health check for keepalived.
# Returns 0 (healthy) if the local kube-apiserver is responding.
# Used by keepalived to decide VIP ownership.

# Check kube-apiserver /healthz endpoint
if curl -sk --max-time 2 https://localhost:6443/healthz 2>/dev/null | grep -q "ok"; then
    exit 0
fi

exit 1
