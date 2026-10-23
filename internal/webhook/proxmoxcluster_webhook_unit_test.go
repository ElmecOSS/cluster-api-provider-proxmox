/*
Copyright 2023-2026 IONOS Cloud.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package webhook

import (
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"

	infrav1 "github.com/ionos-cloud/cluster-api-provider-proxmox/api/v1alpha2"
)

func clusterWithZone(zone string, creds *corev1.SecretReference) *infrav1.ProxmoxCluster {
	cluster := validProxmoxCluster("zone-test-cluster")
	cluster.Spec.ZoneConfigs = []infrav1.ZoneConfigSpec{
		{
			Zone:           new(zone),
			DNSServers:     []string{"8.8.8.8"},
			CredentialsRef: creds,
		},
	}

	return &cluster
}

func placeMachineInZone(cluster *infrav1.ProxmoxCluster, machine, zone string) {
	cluster.Status.NodeLocations = &infrav1.NodeLocations{
		Workers: []infrav1.NodeLocation{
			{
				Machine: corev1.LocalObjectReference{Name: machine},
				Node:    "pve1",
				Zone:    new(zone),
			},
		},
	}
}

func TestValidateZoneCredentials(t *testing.T) {
	require.NoError(t, validateZoneCredentials(clusterWithZone("zone-a", nil)))
	require.NoError(t, validateZoneCredentials(clusterWithZone("zone-a", &corev1.SecretReference{Name: "creds"})))

	err := validateZoneCredentials(clusterWithZone("zone-a", &corev1.SecretReference{}))
	require.ErrorContains(t, err, "credentialsRef must name a secret")

	// "default" is the implicit zone backed by the cluster-level config:
	// an explicit zoneConfig would silently re-route machines already
	// referencing it and collide with the cluster-level IPAM pools.
	err = validateZoneCredentials(clusterWithZone("default", nil))
	require.ErrorContains(t, err, "reserved name")
}

func TestValidateZoneNodes(t *testing.T) {
	zonedCluster := func(zones ...infrav1.ZoneConfigSpec) *infrav1.ProxmoxCluster {
		cluster := validProxmoxCluster("zone-nodes-cluster")
		cluster.Spec.ZoneConfigs = zones
		return &cluster
	}
	creds := func(name string) *corev1.SecretReference {
		return &corev1.SecretReference{Name: name, Namespace: "default"}
	}

	t.Run("own endpoint without nodes is fine when there is nothing to inherit", func(t *testing.T) {
		// No cluster-level allowedNodes means the scheduler never runs: the
		// machine lands on its template's node, which is on the zone endpoint.
		cluster := zonedCluster(infrav1.ZoneConfigSpec{Zone: ptr.To("zone-a"), CredentialsRef: creds("creds-a")})
		cluster.Spec.AllowedNodes = nil
		require.NoError(t, validateZoneNodes(cluster))
	})

	t.Run("own endpoint without nodes inherits a foreign node list", func(t *testing.T) {
		cluster := zonedCluster(infrav1.ZoneConfigSpec{Zone: ptr.To("zone-a"), CredentialsRef: creds("creds-a")})
		cluster.Spec.AllowedNodes = []string{"pve1", "pve2"}
		require.ErrorContains(t, validateZoneNodes(cluster), "must list its nodes when spec.allowedNodes is set")
	})

	t.Run("same node name in zones on different endpoints is allowed", func(t *testing.T) {
		// Node names are only unique within one Proxmox cluster.
		cluster := zonedCluster(
			infrav1.ZoneConfigSpec{Zone: ptr.To("zone-a"), Nodes: []string{"pve1"}, CredentialsRef: creds("creds-a")},
			infrav1.ZoneConfigSpec{Zone: ptr.To("zone-b"), Nodes: []string{"pve1"}, CredentialsRef: creds("creds-b")},
		)
		require.NoError(t, validateZoneNodes(cluster))
	})

	t.Run("same node in two zones on the same endpoint is rejected", func(t *testing.T) {
		// One host cannot be in two failure domains.
		cluster := zonedCluster(
			infrav1.ZoneConfigSpec{Zone: ptr.To("zone-a"), Nodes: []string{"pve1", "pve2"}},
			infrav1.ZoneConfigSpec{Zone: ptr.To("zone-b"), Nodes: []string{"pve2", "pve3"}},
		)
		err := validateZoneNodes(cluster)
		require.ErrorContains(t, err, "pve2")
		require.ErrorContains(t, err, "already belongs to zone \"zone-a\"")
	})

	t.Run("same node in two zones sharing one credentialsRef is rejected", func(t *testing.T) {
		cluster := zonedCluster(
			infrav1.ZoneConfigSpec{Zone: ptr.To("zone-a"), Nodes: []string{"pve1"}, CredentialsRef: creds("shared")},
			infrav1.ZoneConfigSpec{Zone: ptr.To("zone-b"), Nodes: []string{"pve1"}, CredentialsRef: creds("shared")},
		)
		require.ErrorContains(t, validateZoneNodes(cluster), "served by the same Proxmox endpoint")
	})

	t.Run("disjoint node lists on the cluster endpoint are fine", func(t *testing.T) {
		cluster := zonedCluster(
			infrav1.ZoneConfigSpec{Zone: ptr.To("zone-a"), Nodes: []string{"pve1", "pve2"}},
			infrav1.ZoneConfigSpec{Zone: ptr.To("zone-b"), Nodes: []string{"pve3"}},
		)
		require.NoError(t, validateZoneNodes(cluster))
	})
}

func TestZoneSchedulerHintsWarnings(t *testing.T) {
	withHints := func(clusterHints, zoneHints *infrav1.SchedulerHints) *infrav1.ProxmoxCluster {
		cluster := validProxmoxCluster("zone-hints-cluster")
		cluster.Spec.SchedulerHints = clusterHints
		cluster.Spec.ZoneConfigs = []infrav1.ZoneConfigSpec{
			{Zone: ptr.To("zone-a"), DNSServers: []string{"8.8.8.8"}, SchedulerHints: zoneHints},
		}
		return &cluster
	}

	t.Run("a partial override silently drops the cluster values", func(t *testing.T) {
		cluster := withHints(
			&infrav1.SchedulerHints{
				MemoryAdjustment: ptr.To(int64(120)),
				CPUAdjustment:    ptr.To(int64(300)),
			},
			&infrav1.SchedulerHints{MemoryAdjustment: ptr.To(int64(250))},
		)
		warnings := zoneSchedulerHintsWarnings(cluster)
		require.Len(t, warnings, 1)
		require.Contains(t, warnings[0], "cpuAdjustment")
		require.NotContains(t, warnings[0], "memoryAdjustment")
	})

	t.Run("a complete override warns about nothing", func(t *testing.T) {
		hints := &infrav1.SchedulerHints{MemoryAdjustment: ptr.To(int64(120))}
		zone := &infrav1.SchedulerHints{MemoryAdjustment: ptr.To(int64(250))}
		require.Empty(t, zoneSchedulerHintsWarnings(withHints(hints, zone)))
	})

	t.Run("no zone override, nothing to warn about", func(t *testing.T) {
		hints := &infrav1.SchedulerHints{MemoryAdjustment: ptr.To(int64(120))}
		require.Empty(t, zoneSchedulerHintsWarnings(withHints(hints, nil)))
	})

	t.Run("no cluster hints, there is nothing to lose", func(t *testing.T) {
		zone := &infrav1.SchedulerHints{MemoryAdjustment: ptr.To(int64(250))}
		require.Empty(t, zoneSchedulerHintsWarnings(withHints(nil, zone)))
	})
}

func TestValidateZoneRemoval_DeniesWithLiveMachines(t *testing.T) {
	oldCluster := clusterWithZone("zone-a", &corev1.SecretReference{Name: "creds-a"})

	// zone removed entirely.
	newCluster := oldCluster.DeepCopy()
	newCluster.Spec.ZoneConfigs = nil
	placeMachineInZone(newCluster, "machine-1", "zone-a")

	err := validateZoneRemoval(oldCluster, newCluster)
	require.ErrorContains(t, err, `cannot remove zone "zone-a"`)
	require.ErrorContains(t, err, "machine-1")

	// credentialsRef removed while the zone stays.
	newCluster = oldCluster.DeepCopy()
	newCluster.Spec.ZoneConfigs[0].CredentialsRef = nil
	placeMachineInZone(newCluster, "machine-2", "zone-a")

	err = validateZoneRemoval(oldCluster, newCluster)
	require.ErrorContains(t, err, `cannot change the credentialsRef of zone "zone-a"`)

	// credentialsRef swapped to a different secret (= different endpoint).
	newCluster = oldCluster.DeepCopy()
	newCluster.Spec.ZoneConfigs[0].CredentialsRef = &corev1.SecretReference{Name: "creds-other"}
	placeMachineInZone(newCluster, "machine-3", "zone-a")

	err = validateZoneRemoval(oldCluster, newCluster)
	require.ErrorContains(t, err, `cannot change the credentialsRef of zone "zone-a"`)

	// credentialsRef added to a zone whose machines were created through
	// the cluster client.
	oldNoCreds := clusterWithZone("zone-a", nil)
	newCluster = oldNoCreds.DeepCopy()
	newCluster.Spec.ZoneConfigs[0].CredentialsRef = &corev1.SecretReference{Name: "creds-new"}
	placeMachineInZone(newCluster, "machine-4", "zone-a")

	err = validateZoneRemoval(oldNoCreds, newCluster)
	require.ErrorContains(t, err, `cannot change the credentialsRef of zone "zone-a"`)

	// removing a zone WITHOUT credentialsRef with live machines also wedges
	// them (fail-closed client resolution): denied.
	newCluster = oldNoCreds.DeepCopy()
	newCluster.Spec.ZoneConfigs = nil
	placeMachineInZone(newCluster, "machine-5", "zone-a")

	err = validateZoneRemoval(oldNoCreds, newCluster)
	require.ErrorContains(t, err, `cannot remove zone "zone-a"`)
}

func TestValidateZoneRemoval_AllowsSafeChanges(t *testing.T) {
	oldCluster := clusterWithZone("zone-a", &corev1.SecretReference{Name: "creds-a"})

	// no machines in the zone: removal is fine.
	newCluster := oldCluster.DeepCopy()
	newCluster.Spec.ZoneConfigs = nil
	require.NoError(t, validateZoneRemoval(oldCluster, newCluster))

	// machines in another zone don't block.
	newCluster = oldCluster.DeepCopy()
	newCluster.Spec.ZoneConfigs = nil
	placeMachineInZone(newCluster, "machine-1", "zone-b")
	require.NoError(t, validateZoneRemoval(oldCluster, newCluster))

	// keeping the zone and its credentials is always fine, even with machines.
	newCluster = oldCluster.DeepCopy()
	placeMachineInZone(newCluster, "machine-1", "zone-a")
	require.NoError(t, validateZoneRemoval(oldCluster, newCluster))

	// changing unrelated zone fields (nodes, DNS) with machines is fine.
	newCluster = oldCluster.DeepCopy()
	newCluster.Spec.ZoneConfigs[0].Nodes = []string{"pve9"}
	placeMachineInZone(newCluster, "machine-1", "zone-a")
	require.NoError(t, validateZoneRemoval(oldCluster, newCluster))

	// removing a credential-less zone without machines is fine.
	oldNoCreds := clusterWithZone("zone-a", nil)
	newCluster = oldNoCreds.DeepCopy()
	newCluster.Spec.ZoneConfigs = nil
	require.NoError(t, validateZoneRemoval(oldNoCreds, newCluster))
}

func TestZoneChangeWarnings(t *testing.T) {
	oldCluster := clusterWithZone("zone-a", &corev1.SecretReference{Name: "creds-a"})

	// legacy nodeLocations without zone + a guarded zone change → warning.
	newCluster := oldCluster.DeepCopy()
	newCluster.Spec.ZoneConfigs = nil
	newCluster.Status.NodeLocations = &infrav1.NodeLocations{
		Workers: []infrav1.NodeLocation{{
			Machine: corev1.LocalObjectReference{Name: "legacy"},
			Node:    "pve1",
		}},
	}
	warnings := zoneChangeWarnings(oldCluster, newCluster)
	require.Len(t, warnings, 1)
	require.Contains(t, warnings[0], "no recorded zone")

	// no zone change → no warning even with legacy entries.
	same := oldCluster.DeepCopy()
	same.Status.NodeLocations = newCluster.Status.NodeLocations
	require.Empty(t, zoneChangeWarnings(oldCluster, same))

	// zone change but all locations carry a zone → no warning.
	newCluster.Status.NodeLocations.Workers[0].Zone = new("zone-b")
	require.Empty(t, zoneChangeWarnings(oldCluster, newCluster))
}

func TestZoneCredentialsWarnings(t *testing.T) {
	cluster := clusterWithZone("zone-a", &corev1.SecretReference{Name: "creds", Namespace: "elsewhere"})
	warnings := zoneCredentialsWarnings(cluster)
	require.Len(t, warnings, 1)
	require.Contains(t, warnings[0], "cross-namespace")

	require.Empty(t, zoneCredentialsWarnings(clusterWithZone("zone-a", &corev1.SecretReference{Name: "creds"})))
}
