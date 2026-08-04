#!/usr/bin/env bash
# Copyright 2023-2026 IONOS Cloud.
# Licensed under the Apache License, Version 2.0.

# validate-rollback.sh — check that a management cluster can be safely rolled
# back from the multi-DC fork to upstream CAPMOX.
#
# Rolling back while machines still live in zones with their own
# credentialsRef is NOT safe: upstream's single client would resolve those
# machines against the wrong Proxmox endpoint. A wrong-endpoint lookup can
# answer "VM does not exist" for a VM that is alive in another datacenter
# (the finalizer is dropped and the VM is orphaned) or, with VMID and node
# name collisions, destroy an unrelated VM.
#
# Usage: hack/validate-rollback.sh [-n <namespace>] (default: all namespaces)

set -euo pipefail

NAMESPACE_ARGS=("--all-namespaces")
while getopts "n:" opt; do
  case "$opt" in
    n) NAMESPACE_ARGS=("-n" "$OPTARG") ;;
    *) echo "usage: $0 [-n <namespace>]" >&2; exit 2 ;;
  esac
done

fail=0

clusters=$(kubectl get proxmoxclusters "${NAMESPACE_ARGS[@]}" -o json)

# Zones that carry their own credentialsRef, per cluster.
credentialed_zones=$(echo "${clusters}" | jq -r '
  .items[]
  | . as $c
  | ($c.spec.zoneConfig // [])[]
  | select(.credentialsRef != null)
  | "\($c.metadata.namespace)/\($c.metadata.name) \(.zone)"')

if [[ -z "${credentialed_zones}" ]]; then
  echo "OK: no zone with its own credentialsRef found; rollback to upstream is drop-in."
  exit 0
fi

echo "Zones with their own credentialsRef:"
echo "${credentialed_zones}" | sed 's/^/  /'

# 1. status.nodeLocations must not reference a credentialed zone.
while read -r cluster zone; do
  machines=$(echo "${clusters}" | jq -r --arg ns "${cluster%%/*}" --arg name "${cluster##*/}" --arg zone "${zone}" '
    .items[]
    | select(.metadata.namespace == $ns and .metadata.name == $name)
    | ((.status.nodeLocations.controlPlane // []) + (.status.nodeLocations.workers // []))[]
    | select(.zone == $zone)
    | .machine.name')
  if [[ -n "${machines}" ]]; then
    echo "FAIL: ${cluster}: machines still placed in credentialed zone \"${zone}\":"
    echo "${machines}" | sed 's/^/    /'
    fail=1
  fi
done <<< "${credentialed_zones}"

# 2. no CAPI Machine may reference a credentialed zone as failureDomain.
while read -r cluster zone; do
  ns="${cluster%%/*}"
  machines=$(kubectl get machines -n "${ns}" -o json | jq -r --arg zone "${zone}" '
    .items[] | select(.spec.failureDomain == $zone) | .metadata.name')
  if [[ -n "${machines}" ]]; then
    echo "FAIL: ${cluster}: CAPI Machines still reference failure domain \"${zone}\":"
    echo "${machines}" | sed 's/^/    /'
    fail=1
  fi
done <<< "${credentialed_zones}"

if [[ "${fail}" -ne 0 ]]; then
  cat >&2 <<'EOF'

Rollback is NOT safe. Before switching to upstream CAPMOX:
  1. Scale remote-zone MachineDeployments to 0 (or repin their failureDomain).
  2. Set controlPlaneEligible: false on remote zones and roll the control
     plane onto the default zone.
  3. Wait for all deletions to finish (the fork routes them correctly).
  4. Re-run this script, then remove the zoneConfig entries and swap the
     controller image and CRDs.
EOF
  exit 1
fi

echo "OK: no machines depend on credentialed zones. Remove those zoneConfig entries, then roll back."
