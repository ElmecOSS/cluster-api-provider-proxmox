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
	"k8s.io/utils/ptr"

	infrav1 "github.com/ionos-cloud/cluster-api-provider-proxmox/api/v1alpha2"
)

func clusterWithZone(zone string, creds *corev1.SecretReference) *infrav1.ProxmoxCluster {
	cluster := validProxmoxCluster("zone-test-cluster")
	cluster.Spec.ZoneConfigs = []infrav1.ZoneConfigSpec{
		{
			Zone:           ptr.To(zone),
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
				Zone:    ptr.To(zone),
			},
		},
	}
}

func TestValidateZoneCredentials(t *testing.T) {
	require.NoError(t, validateZoneCredentials(clusterWithZone("zone-a", nil)))
	require.NoError(t, validateZoneCredentials(clusterWithZone("zone-a", &corev1.SecretReference{Name: "creds"})))

	err := validateZoneCredentials(clusterWithZone("zone-a", &corev1.SecretReference{}))
	require.ErrorContains(t, err, "credentialsRef must name a secret")
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

	// zones without credentialsRef are never blocked (single-endpoint case).
	oldNoCreds := clusterWithZone("zone-a", nil)
	newCluster = oldNoCreds.DeepCopy()
	newCluster.Spec.ZoneConfigs = nil
	placeMachineInZone(newCluster, "machine-1", "zone-a")
	require.NoError(t, validateZoneRemoval(oldNoCreds, newCluster))
}
