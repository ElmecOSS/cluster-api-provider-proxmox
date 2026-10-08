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

package controller

import (
	"context"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/pkg/errors"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/cluster-api/util/conditions"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	ctrlutil "sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	infrav1 "github.com/ionos-cloud/cluster-api-provider-proxmox/api/v1alpha2"
	"github.com/ionos-cloud/cluster-api-provider-proxmox/pkg/kubernetes/ipam"
	capmox "github.com/ionos-cloud/cluster-api-provider-proxmox/pkg/proxmox"
	"github.com/ionos-cloud/cluster-api-provider-proxmox/pkg/proxmox/proxmoxtest"
	"github.com/ionos-cloud/cluster-api-provider-proxmox/pkg/scope"
)

// stubZoneFactory hands out pre-seeded clients per secret name and records
// evictions.
type stubZoneFactory struct {
	clients map[string]capmox.Client
	evicted []string
}

func (f *stubZoneFactory) GetOrCreate(_ context.Context, _ logr.Logger, secret *corev1.Secret) (capmox.Client, error) {
	if c, ok := f.clients[secret.GetName()]; ok {
		return c, nil
	}
	return nil, errors.New("no stub client for secret " + secret.GetName())
}

func (f *stubZoneFactory) Evict(_, name string) {
	f.evicted = append(f.evicted, name)
}

func newClusterControllerFixture(t *testing.T, factory *stubZoneFactory, zones []infrav1.ZoneConfigSpec, secrets ...*corev1.Secret) (*ProxmoxClusterReconciler, *scope.ClusterScope) {
	t.Helper()

	cluster := &clusterv1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: metav1.NamespaceDefault}}
	proxmoxCluster := &infrav1.ProxmoxCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: metav1.NamespaceDefault},
		Spec: infrav1.ProxmoxClusterSpec{
			IPv4Config: &infrav1.IPConfigSpec{
				Addresses: []string{"10.0.0.10-10.0.0.20"},
				Prefix:    24,
				Gateway:   "10.0.0.1",
			},
			DNSServers:  []string{"1.2.3.4"},
			ZoneConfigs: zones,
		},
	}

	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, clusterv1.AddToScheme(scheme))
	require.NoError(t, infrav1.AddToScheme(scheme))

	objects := make([]ctrlclient.Object, 0, 2+len(secrets))
	objects = append(objects, cluster, proxmoxCluster)
	for _, s := range secrets {
		objects = append(objects, s)
	}
	kubeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objects...).
		WithStatusSubresource(&infrav1.ProxmoxCluster{}).
		Build()

	logger := logr.Discard()
	clusterScope, err := scope.NewClusterScope(scope.ClusterScopeParams{
		Client:         kubeClient,
		Logger:         &logger,
		Cluster:        cluster,
		ProxmoxCluster: proxmoxCluster,
		ProxmoxClient:  proxmoxtest.NewMockClient(t),
		ClientFactory:  factory,
		IPAMHelper:     &ipam.Helper{},
	})
	require.NoError(t, err)

	return &ProxmoxClusterReconciler{
		Client:        kubeClient,
		Recorder:      record.NewFakeRecorder(16),
		ClientFactory: factory,
	}, clusterScope
}

func zoneSecret(name string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: metav1.NamespaceDefault},
		Data:       map[string][]byte{"url": []byte("https://dc:8006"), "token": []byte("t"), "secret": []byte("s")},
	}
}

func credentialedZone(zone, secretName string) infrav1.ZoneConfigSpec {
	return infrav1.ZoneConfigSpec{
		Zone:           ptr.To(zone),
		DNSServers:     []string{"1.2.3.4"},
		CredentialsRef: &corev1.SecretReference{Name: secretName},
	}
}

func TestReconcileZoneClients_HealthyProbeAndRateLimit(t *testing.T) {
	zoneClient := proxmoxtest.NewMockClient(t)
	factory := &stubZoneFactory{clients: map[string]capmox.Client{"zone-b-creds": zoneClient}}
	r, clusterScope := newClusterControllerFixture(t, factory,
		[]infrav1.ZoneConfigSpec{credentialedZone("zone-b", "zone-b-creds")}, zoneSecret("zone-b-creds"))

	// exactly one Version call: the second reconcile is inside the rate limit.
	zoneClient.EXPECT().Version(context.Background()).Return(nil, nil).Once()

	requeue := r.reconcileZoneClients(context.Background(), clusterScope)
	require.Equal(t, zoneProbeInterval, requeue)
	cond := conditions.Get(clusterScope.ProxmoxCluster, infrav1.ProxmoxClusterZonesAvailableCondition)
	require.NotNil(t, cond)
	require.Equal(t, metav1.ConditionTrue, cond.Status)

	requeue = r.reconcileZoneClients(context.Background(), clusterScope)
	require.Equal(t, zoneProbeInterval, requeue)
	require.Empty(t, factory.evicted)
}

func TestReconcileZoneClients_ProbeFailureEvictsAndRecovers(t *testing.T) {
	zoneClient := proxmoxtest.NewMockClient(t)
	factory := &stubZoneFactory{clients: map[string]capmox.Client{"zone-b-creds": zoneClient}}
	r, clusterScope := newClusterControllerFixture(t, factory,
		[]infrav1.ZoneConfigSpec{credentialedZone("zone-b", "zone-b-creds")}, zoneSecret("zone-b-creds"))

	zoneClient.EXPECT().Version(context.Background()).Return(nil, errors.New("endpoint down")).Once()

	requeue := r.reconcileZoneClients(context.Background(), clusterScope)
	require.Equal(t, zoneUnreachableRequeue, requeue)
	cond := conditions.Get(clusterScope.ProxmoxCluster, infrav1.ProxmoxClusterZonesAvailableCondition)
	require.Equal(t, metav1.ConditionFalse, cond.Status)
	require.Equal(t, []string{"zone-b-creds"}, factory.evicted)

	// probe-time entry was reset on failure: the next reconcile re-probes
	// immediately and recovers.
	zoneClient.EXPECT().Version(context.Background()).Return(nil, nil).Once()
	requeue = r.reconcileZoneClients(context.Background(), clusterScope)
	require.Equal(t, zoneProbeInterval, requeue)
	cond = conditions.Get(clusterScope.ProxmoxCluster, infrav1.ProxmoxClusterZonesAvailableCondition)
	require.Equal(t, metav1.ConditionTrue, cond.Status)
}

func TestReconcileZoneClients_UnresolvableZoneSecret(t *testing.T) {
	factory := &stubZoneFactory{clients: map[string]capmox.Client{}}
	r, clusterScope := newClusterControllerFixture(t, factory,
		[]infrav1.ZoneConfigSpec{credentialedZone("zone-b", "missing-creds")})

	requeue := r.reconcileZoneClients(context.Background(), clusterScope)
	require.Equal(t, zoneUnreachableRequeue, requeue)
	cond := conditions.Get(clusterScope.ProxmoxCluster, infrav1.ProxmoxClusterZonesAvailableCondition)
	require.Equal(t, metav1.ConditionFalse, cond.Status)
}

func TestReconcileZoneClients_NoCredentialedZones(t *testing.T) {
	factory := &stubZoneFactory{clients: map[string]capmox.Client{}}
	r, clusterScope := newClusterControllerFixture(t, factory,
		[]infrav1.ZoneConfigSpec{{Zone: ptr.To("plain"), DNSServers: []string{"1.2.3.4"}}})

	// pre-set the condition to prove it gets deleted.
	conditions.Set(clusterScope.ProxmoxCluster, metav1.Condition{
		Type:   infrav1.ProxmoxClusterZonesAvailableCondition,
		Status: metav1.ConditionTrue,
		Reason: infrav1.ProxmoxClusterZonesAvailableReason,
	})

	requeue := r.reconcileZoneClients(context.Background(), clusterScope)
	require.Equal(t, time.Duration(0), requeue)
	require.Nil(t, conditions.Get(clusterScope.ProxmoxCluster, infrav1.ProxmoxClusterZonesAvailableCondition))
}

func TestCredentialsSecretLifecycle(t *testing.T) {
	ctx := context.Background()
	factory := &stubZoneFactory{clients: map[string]capmox.Client{}}

	clusterSecret := zoneSecret("cluster-creds")
	oldZoneSecret := zoneSecret("zone-b-creds")
	newZoneSecret := zoneSecret("zone-b-creds-new")

	r, clusterScope := newClusterControllerFixture(t, factory,
		[]infrav1.ZoneConfigSpec{credentialedZone("zone-b", "zone-b-creds")},
		clusterSecret, oldZoneSecret, newZoneSecret)
	clusterScope.ProxmoxCluster.Spec.CredentialsRef = &corev1.SecretReference{Name: "cluster-creds"}

	// first reconcile adopts cluster + zone secrets and tracks them.
	require.NoError(t, r.reconcileNormalCredentialsSecret(ctx, clusterScope))

	got := &corev1.Secret{}
	require.NoError(t, r.Client.Get(ctx, ctrlclient.ObjectKey{Namespace: metav1.NamespaceDefault, Name: "zone-b-creds"}, got))
	require.True(t, ctrlutil.ContainsFinalizer(got, infrav1.SecretFinalizer))
	require.Len(t, got.GetOwnerReferences(), 1)

	annotation := clusterScope.ProxmoxCluster.GetAnnotations()[infrav1.ManagedCredentialsSecretsAnnotation]
	require.Contains(t, annotation, "default/cluster-creds")
	require.Contains(t, annotation, "default/zone-b-creds")

	// re-pointing the zone credentialsRef releases the old secret.
	clusterScope.ProxmoxCluster.Spec.ZoneConfigs[0].CredentialsRef = &corev1.SecretReference{Name: "zone-b-creds-new"}
	require.NoError(t, r.reconcileNormalCredentialsSecret(ctx, clusterScope))

	require.NoError(t, r.Client.Get(ctx, ctrlclient.ObjectKey{Namespace: metav1.NamespaceDefault, Name: "zone-b-creds"}, got))
	require.False(t, ctrlutil.ContainsFinalizer(got, infrav1.SecretFinalizer), "released secret must lose the finalizer")

	annotation = clusterScope.ProxmoxCluster.GetAnnotations()[infrav1.ManagedCredentialsSecretsAnnotation]
	require.NotContains(t, annotation, "default/zone-b-creds,")
	require.Contains(t, annotation, "default/zone-b-creds-new")

	// a missing zone secret is non-fatal and stays tracked (it may carry
	// the finalizer from an earlier success).
	clusterScope.ProxmoxCluster.Spec.ZoneConfigs[0].CredentialsRef = &corev1.SecretReference{Name: "gone-creds"}
	require.NoError(t, r.reconcileNormalCredentialsSecret(ctx, clusterScope))
	annotation = clusterScope.ProxmoxCluster.GetAnnotations()[infrav1.ManagedCredentialsSecretsAnnotation]
	require.Contains(t, annotation, "default/gone-creds")

	// a missing CLUSTER secret is fatal.
	clusterScope.ProxmoxCluster.Spec.CredentialsRef = &corev1.SecretReference{Name: "no-such-cluster-creds"}
	require.Error(t, r.reconcileNormalCredentialsSecret(ctx, clusterScope))

	// deletion releases everything that is still around.
	clusterScope.ProxmoxCluster.Spec.CredentialsRef = &corev1.SecretReference{Name: "cluster-creds"}
	require.NoError(t, r.reconcileDeleteCredentialsSecret(ctx, clusterScope))
	require.NoError(t, r.Client.Get(ctx, ctrlclient.ObjectKey{Namespace: metav1.NamespaceDefault, Name: "zone-b-creds-new"}, got))
	require.False(t, ctrlutil.ContainsFinalizer(got, infrav1.SecretFinalizer))
}

// TestReconcileDelete_InFlightTask_KeepsFinalizer: a machine with an
// in-flight task but no VMID yet must NOT take the never-created shortcut —
// its clone may be materializing on the zone endpoint.
func TestReconcileDelete_InFlightTask_KeepsFinalizer(t *testing.T) {
	r, machineScope := newDeleteTestMachineScope(t, "zone-gone", nil)
	machineScope.ProxmoxMachine.Status.TaskRef = ptr.To("UPID:node1:001")

	_, err := r.reconcileDelete(context.Background(), machineScope)
	require.ErrorContains(t, err, "zone client unavailable")
	require.True(t, ctrlutil.ContainsFinalizer(machineScope.ProxmoxMachine, infrav1.MachineFinalizer))
}
