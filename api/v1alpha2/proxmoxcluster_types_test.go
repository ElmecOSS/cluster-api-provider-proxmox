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

package v1alpha2

import (
	"context"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ipamicv1 "sigs.k8s.io/cluster-api-ipam-provider-in-cluster/api/v1alpha2"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestUpdateNodeLocation(t *testing.T) {
	zone1 := "zone1"

	cl := ProxmoxCluster{
		Status: ProxmoxClusterStatus{},
	}

	res := cl.UpdateNodeLocation("new", "n1", &zone1, false)
	require.NotNil(t, cl.Status.NodeLocations)
	require.Len(t, cl.Status.NodeLocations.Workers, 1)
	require.True(t, res)
	require.Equal(t, zone1, *cl.Status.NodeLocations.Workers[0].Zone)

	locs := &NodeLocations{
		Workers: []NodeLocation{
			{
				Machine: corev1.LocalObjectReference{Name: "m1"},
				Node:    "n1",
				Zone:    &zone1,
			},
			{
				Machine: corev1.LocalObjectReference{Name: "m2"},
				Node:    "n2",
			},
			{
				Machine: corev1.LocalObjectReference{Name: "m3"},
				Node:    "n3",
			},
		},
	}

	cl.Status.NodeLocations = locs

	res = cl.UpdateNodeLocation("m1", "n2", &zone1, false)
	require.True(t, res)
	require.Len(t, cl.Status.NodeLocations.Workers, 3)
	require.Equal(t, cl.Status.NodeLocations.Workers[0].Node, "n2")
	require.Equal(t, zone1, *cl.Status.NodeLocations.Workers[0].Zone)

	res = cl.UpdateNodeLocation("m4", "n4", nil, false)
	require.True(t, res)
	require.Len(t, cl.Status.NodeLocations.Workers, 4)
	require.Equal(t, cl.Status.NodeLocations.Workers[3].Node, "n4")

	res = cl.UpdateNodeLocation("m2", "n2", nil, false)
	require.False(t, res)
	require.Len(t, cl.Status.NodeLocations.Workers, 4)

	// a missing zone is repaired even when the node did not change.
	res = cl.UpdateNodeLocation("m2", "n2", &zone1, false)
	require.True(t, res)
	require.Equal(t, zone1, *cl.Status.NodeLocations.Workers[1].Zone)
}

func defaultCluster() *ProxmoxCluster {
	return &ProxmoxCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-cluster",
			Namespace: metav1.NamespaceDefault,
		},
		Spec: ProxmoxClusterSpec{
			IPv4Config: &IPConfigSpec{
				Addresses: []string{"10.0.0.0/24"},
				Prefix:    24,
				Gateway:   "10.0.0.254",
				Metric:    new(int32(123)),
			},
			DNSServers: []string{"1.2.3.4"},
		},
	}
}

var _ = Describe("ProxmoxCluster Test", func() {
	AfterEach(func() {
		err := k8sClient.Delete(context.Background(), defaultCluster())
		Expect(client.IgnoreNotFound(err)).To(Succeed())
	})

	Context("ClusterPort", func() {
		It("Should not allow ports higher than 65535", func() {
			dc := defaultCluster()
			dc.Spec.ControlPlaneEndpoint = APIEndpoint{
				Port: 65536,
			}
			Expect(k8sClient.Create(context.Background(), dc)).Should(MatchError(ContainSubstring("should be less than or equal to 65535")))
		})

		It("Should not allow negative ports", func() {
			dc := defaultCluster()
			dc.Spec.ControlPlaneEndpoint = APIEndpoint{
				Port: -1,
			}
			Expect(k8sClient.Create(context.Background(), dc)).Should(MatchError(ContainSubstring("should be greater than or equal to 1")))
		})

		It("Should not allow port 0", func() {
			dc := defaultCluster()
			dc.Spec.ControlPlaneEndpoint = APIEndpoint{
				Host: "example.com",
				Port: 0,
			}
			Expect(k8sClient.Create(context.Background(), dc)).Should(MatchError(ContainSubstring("port must be within 1-65535")))
		})
	})

	Context("IPv4Config", func() {
		It("Should not allow empty addresses", func() {
			dc := defaultCluster()
			dc.Spec.IPv4Config.Addresses = []string{}

			Expect(k8sClient.Create(context.Background(), dc)).Should(MatchError(ContainSubstring("spec.ipv4Config.addresses: Required value")))
		})

		It("Should not allow prefix higher than 128", func() {
			dc := defaultCluster()
			dc.Spec.IPv4Config.Prefix = 129

			Expect(k8sClient.Create(context.Background(), dc)).Should(MatchError(ContainSubstring("should be less than or equal to 128")))
		})

		It("Should not allow empty ip config", func() {
			dc := defaultCluster()
			dc.Spec.IPv6Config = nil
			dc.Spec.IPv4Config = nil
			Expect(k8sClient.Create(context.Background(), dc)).Should(MatchError(ContainSubstring("at least one ip config must be set")))
		})
	})

	It("Should not allow empty DNS servers", func() {
		dc := defaultCluster()
		dc.Spec.DNSServers = []string{}

		Expect(k8sClient.Create(context.Background(), dc)).Should(MatchError(ContainSubstring("spec.dnsServers: Required value")))
	})

	It("Should allow creating valid clusters", func() {
		Expect(k8sClient.Create(context.Background(), defaultCluster())).To(Succeed())
	})

	Context("IPv6Config", func() {
		It("Should not allow empty addresses", func() {
			dc := defaultCluster()
			dc.Spec.IPv6Config = &IPConfigSpec{
				Addresses: []string{},
				Prefix:    0,
			}
			Expect(k8sClient.Create(context.Background(), dc)).Should(MatchError(ContainSubstring("spec.ipv6Config.addresses: Required value")))
		})

		It("Should not allow prefix higher than 128", func() {
			dc := defaultCluster()
			dc.Spec.IPv6Config = &IPConfigSpec{
				Addresses: []string{},
				Prefix:    129,
			}

			Expect(k8sClient.Create(context.Background(), dc)).Should(MatchError(ContainSubstring("should be less than or equal to 128")))
		})
	})
})

func TestRemoveNodeLocation(t *testing.T) {
	cl := ProxmoxCluster{
		Status: ProxmoxClusterStatus{NodeLocations: &NodeLocations{
			Workers: []NodeLocation{
				{
					Machine: corev1.LocalObjectReference{Name: "m1"},
					Node:    "n1",
				},
				{
					Machine: corev1.LocalObjectReference{Name: "m2"},
					Node:    "n2",
				},
				{
					Machine: corev1.LocalObjectReference{Name: "m3"},
					Node:    "n3",
				},
			},
		}},
	}

	cl.RemoveNodeLocation("m1", false)
	require.NotNil(t, cl.Status.NodeLocations)
	require.Len(t, cl.Status.NodeLocations.Workers, 2)

	cl.RemoveNodeLocation("m1", false)
	require.Len(t, cl.Status.NodeLocations.Workers, 2)
	require.Equal(t, cl.Status.NodeLocations.Workers[0].Node, "n2")

	cl.UpdateNodeLocation("m4", "n4", nil, true)
	require.Len(t, cl.Status.NodeLocations.ControlPlane, 1)

	cl.RemoveNodeLocation("m4", true)
	require.Len(t, cl.Status.NodeLocations.ControlPlane, 0)
}

func TestGetZoneNodes(t *testing.T) {
	cl := &ProxmoxCluster{
		Spec: ProxmoxClusterSpec{
			ZoneConfigs: []ZoneConfigSpec{
				{
					Zone:  new("zone-a"),
					Nodes: []string{"pve1", "pve2"},
				},
				{
					Zone: new("zone-b"),
					// No nodes explicitly set.
				},
			},
		},
	}

	// Zone found with nodes.
	nodes := cl.GetZoneNodes("zone-a")
	require.Equal(t, []string{"pve1", "pve2"}, nodes)

	// Zone found without nodes.
	nodes = cl.GetZoneNodes("zone-b")
	require.Nil(t, nodes)

	// Zone not found.
	nodes = cl.GetZoneNodes("zone-c")
	require.Nil(t, nodes)

	// Empty ZoneConfigs.
	empty := &ProxmoxCluster{}
	nodes = empty.GetZoneNodes("anything")
	require.Nil(t, nodes)
}

func TestSetInClusterIPPoolRef(t *testing.T) {
	cl := defaultCluster()

	cl.SetInClusterIPPoolRef(nil)
	require.Nil(t, cl.Status.InClusterIPPoolRef)

	pool := &ipamicv1.InClusterIPPool{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test",
			Namespace: metav1.NamespaceDefault,
		},
		Spec: ipamicv1.InClusterIPPoolSpec{
			Addresses: []string{"10.10.10.2/24"},
			Prefix:    24,
			Gateway:   "10.10.10.1",
		},
	}

	cl.SetInClusterIPPoolRef(pool)
	require.Equal(t, cl.Status.InClusterIPPoolRef[0].Name, pool.GetName())

	cl.SetInClusterIPPoolRef(pool)
	require.Equal(t, cl.Status.InClusterIPPoolRef[0].Name, pool.GetName())
}

func zonePool(name, zone, family string) *ipamicv1.InClusterIPPool {
	pool := &ipamicv1.InClusterIPPool{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   metav1.NamespaceDefault,
			Annotations: map[string]string{ProxmoxIPFamilyAnnotation: family},
		},
	}
	if zone != "" {
		pool.Labels = map[string]string{ProxmoxZoneLabel: zone}
	}

	return pool
}

func TestAddInClusterZoneRef(t *testing.T) {
	cl := defaultCluster()

	// default zone (no zone label) plus two labelled zones, both IP families each.
	cl.AddInClusterZoneRef(zonePool("test-v4-icip", "", IPv4Type))
	cl.AddInClusterZoneRef(zonePool("test-zone1-v4-icip", "zone1", IPv4Type))
	cl.AddInClusterZoneRef(zonePool("test-zone1-v6-icip", "zone1", IPv6Type))
	cl.AddInClusterZoneRef(zonePool("test-zone2-v4-icip", "zone2", IPv4Type))
	cl.AddInClusterZoneRef(zonePool("test-zone2-v6-icip", "zone2", IPv6Type))

	require.Len(t, cl.Status.InClusterZoneRef, 3)

	byZone := make(map[string]InClusterZoneRef)
	for _, ref := range cl.Status.InClusterZoneRef {
		byZone[*ref.Zone] = ref
	}

	require.Equal(t, "test-v4-icip", byZone["default"].InClusterIPPoolRefV4.Name)
	require.Nil(t, byZone["default"].InClusterIPPoolRefV6)
	require.Equal(t, "test-zone1-v4-icip", byZone["zone1"].InClusterIPPoolRefV4.Name)
	require.Equal(t, "test-zone1-v6-icip", byZone["zone1"].InClusterIPPoolRefV6.Name)
	require.Equal(t, "test-zone2-v4-icip", byZone["zone2"].InClusterIPPoolRefV4.Name)
	require.Equal(t, "test-zone2-v6-icip", byZone["zone2"].InClusterIPPoolRefV6.Name)

	// updating an existing zone's pool ref must not append a new entry.
	cl.AddInClusterZoneRef(zonePool("test-zone1-v4-icip-new", "zone1", IPv4Type))
	require.Len(t, cl.Status.InClusterZoneRef, 3)
}
