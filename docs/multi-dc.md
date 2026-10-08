# Multi-datacenter clusters

CAPMOX zones (failure domains) can each be backed by a **different Proxmox
cluster** (a separate API endpoint), so a single CAPI cluster can stretch its
control plane and workers across datacenters / availability zones.

With no zone `credentialsRef` configured, nothing changes: the provider
behaves exactly like the single-endpoint upstream.

## How it works

`ZoneConfigSpec` gains two optional fields:

```yaml
spec:
  zoneConfig:
    - zone: dc1                      # backed by the cluster-level credentials
      dnsServers: [10.1.0.53]
      nodes: [dc1-pve1, dc1-pve2]
      ipv4Config:                    # every zone needs its own pool: zoned
        addresses: [10.1.0.100-10.1.0.120]   # machines draw from it
        prefix: 24
        gateway: 10.1.0.1
      templateSource:
        sourceNode: dc1-pve1
        templateID: 100
    - zone: dc2                      # backed by ANOTHER Proxmox cluster
      dnsServers: [10.2.0.53]
      nodes: [dc2-pve1, dc2-pve2]
      ipv4Config:
        addresses: [10.2.0.100-10.2.0.120]
        prefix: 24
        gateway: 10.2.0.1
      credentialsRef:                # secret keys: url, token, secret
        name: my-cluster-proxmox-credentials-dc2
      templateSource:                # templates cannot be cloned across
        sourceNode: dc2-pve1         # Proxmox clusters: every remote zone
        templateID: 100              # needs its own template source
```

- **`credentialsRef`** — the Proxmox API credentials for the cluster backing
  this zone (same secret format as the cluster-level `credentialsRef`;
  optional keys `insecure` and `root_ca` included). When unset, the zone uses
  the cluster-level client. A zone's credentials always win over the
  controller's env-var credentials.
- **`templateSource`** — total override of the machine's
  `sourceNode`/`templateID`/`templateSelector` for machines placed in this
  zone (never merged). With `templateSelector` instead, tags are resolved on
  the zone's endpoint, so identically-tagged templates per DC also work.
- **`dnsServers`** — now actually honored: machines in the zone get the
  zone's resolvers, falling back to the cluster-level `dnsServers`.

  > **Breaking change for existing multi-zone clusters** — this field was
  > required but silently ignored before: machines always received the
  > cluster-level `dnsServers`. From this release, machines created or
  > rolled in a zone receive the **zone's** `dnsServers`. Review your
  > `zoneConfig[].dnsServers` values (set them equal to the cluster-level
  > list to keep the old behavior) before upgrading.

Machines land in zones via the CAPI failure-domain contract: KubeadmControlPlane
spreads replicas across `status.failureDomains` automatically;
MachineDeployments pin workers with `spec.template.spec.failureDomain`.

Every Proxmox operation of a machine — clone, config, task polling, IP
tagging, power, **delete** — runs on its zone's client. If a zone's client
cannot be resolved (missing zone config, missing secret, unreachable
endpoint) the machine is parked with condition reason `ZoneClientUnavailable`
and is **never** reconciled through another endpoint; deletion keeps the
finalizer and retries. The webhook refuses to remove a zone (or its
`credentialsRef`) while `status.nodeLocations` still records machines in it.

Per-zone endpoint health is surfaced on the ProxmoxCluster as the standalone
`ZonesAvailable` condition and warning events; it deliberately does not
affect cluster readiness. The endpoint of every credentialed zone is
re-probed periodically (every ~5 minutes in steady state, ~2 minutes while a
zone is down), so the condition both detects outages of already-established
clients and recovers on its own. Zone clients are cached across reconciles
and rebuilt on secret changes or probe failures; the **cluster-level**
client is deliberately not cached, preserving upstream's
construction-time liveness check for single-endpoint setups.

The controller tracks the credentials secrets it manages via the
`capmox.cluster.x-k8s.io/managed-credentials-secrets` annotation on the
ProxmoxCluster, so removing or re-pointing a zone `credentialsRef` also
releases the finalizer and ownerRef from the previously referenced secret.

Use `templates/cluster-template-multi-dc.yaml` as a starting point. On top
of the standard template variables (`CONTROL_PLANE_ENDPOINT_IP`,
`NODE_IP_RANGES`, `IP_PREFIX`, `GATEWAY`, `DNS_SERVERS`,
`PROXMOX_URL/TOKEN/SECRET`, `PROXMOX_SOURCENODE`, `TEMPLATE_VMID`,
`KUBERNETES_VERSION`, `VM_SSH_KEYS`, `BRIDGE`, `BOOT_VOLUME_DEVICE`, ...) it
requires:

| Variable | Purpose |
|---|---|
| `DC1_NODE_IP_RANGES` / `DC2_NODE_IP_RANGES` | per-zone IPAM pool addresses (required) |
| `DC2_PROXMOX_URL` / `DC2_PROXMOX_TOKEN` / `DC2_PROXMOX_SECRET` | credentials of the Proxmox cluster backing zone 2 (required) |
| `DC2_PROXMOX_SOURCENODE` / `DC2_TEMPLATE_VMID` | template source in the zone-2 Proxmox cluster (required) |
| `DC1_ZONE` / `DC2_ZONE` | zone names (default `dc1`/`dc2`; never use the reserved name `default`) |
| `DC1_ALLOWED_NODES` / `DC2_ALLOWED_NODES` | Proxmox nodes per zone (default `[]`) |
| `WORKER_FAILURE_DOMAIN` | zone the worker MachineDeployment is pinned to (default: literal `dc1`) |

## Networking flavors

- **Stretched VLAN (recommended starting point)**: one L2 segment spans the
  DCs. A single cluster-level `ipv4Config` pool and the default kube-vip ARP
  setup work unchanged.
- **Routed per-DC subnets**: declare `ipv4Config`/`ipv6Config` and
  `dnsServers` per zone (per-zone IPAM pools already exist), give each DC its
  own MachineDeployment with the right bridge/VLAN in
  `network.networkDevices`, and replace kube-vip ARP with **BGP mode or an
  external load balancer** — L2 ARP cannot announce a VIP across routed
  subnets.

## Stretched control plane: etcd and quorum

- Keep the inter-DC round-trip low (rule of thumb: ≤ 10 ms) or tune etcd
  heartbeat/election timeouts.
- **Two-DC quorum trap**: 3 control-plane replicas over 2 DCs put 2 replicas
  in one DC — losing that DC loses etcd quorum and requires manual recovery.
  Prefer 3 zones (3 DCs, or 2 DCs + a tie-breaker zone), or accept and
  document the manual-recovery runbook.
- A zone endpoint outage does not reshuffle machines: KCP never reassigns
  the failure domain of a pending machine, so a scale-up targeting the dead
  zone waits there. Remediate manually (delete the stuck Machine, or set
  `controlPlaneEligible: false` on the zone and roll).

## VMIDs

VMIDs are unique only *within* one Proxmox cluster. The provider validates
IDs against the zone's endpoint and reserves the VMIDs of all machines of the
CAPI cluster, but it cannot see foreign VMs of the other DC. Recommendation:
give each zone's machine templates **disjoint `vmIDRange`s**. Machine
identity across DCs is safe regardless: `providerID` uses the SMBIOS UUID,
which is globally unique.

## Rolling back to upstream CAPMOX

- **Multi-DC not in use** (no zone `credentialsRef`): the fork is a drop-in
  replacement in both directions; apply upstream CRDs and swap the image.
- **Multi-DC in use**: upstream has one client — after a rollback it would
  reach machines of remote zones on the wrong endpoint. A wrong-endpoint
  lookup can answer "VM does not exist" for a VM that is alive in the other
  DC (finalizer dropped → running VM orphaned) or, with VMID/node-name
  collisions, **delete an unrelated VM**. Therefore, while still on the fork:

  1. Scale remote-zone MachineDeployments to 0 (or repin their
     `failureDomain` to the default zone).
  2. Set `controlPlaneEligible: false` on remote zones and let KCP roll the
     control plane onto the default zone.
  3. Wait for deletions to complete — the fork routes them to the right
     endpoint.
  4. Run `make validate-rollback` (checks `status.nodeLocations`, machine
     failure domains and zone configs); it must pass.
  5. Remove the credentialed `zoneConfig` entries, then swap the controller
     image and apply upstream CRDs (the extra fields are pruned).

  **Never roll back with machines alive in credentialed zones.**
