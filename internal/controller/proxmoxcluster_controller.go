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

// Package controller implements controller types.
package controller

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	kerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	clusterutil "sigs.k8s.io/cluster-api/util"
	"sigs.k8s.io/cluster-api/util/annotations"
	"sigs.k8s.io/cluster-api/util/conditions"
	"sigs.k8s.io/cluster-api/util/patch"
	"sigs.k8s.io/cluster-api/util/predicates"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlutil "sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	infrav1 "github.com/ionos-cloud/cluster-api-provider-proxmox/api/v1alpha2"
	"github.com/ionos-cloud/cluster-api-provider-proxmox/pkg/consts"
	"github.com/ionos-cloud/cluster-api-provider-proxmox/pkg/kubernetes/ipam"
	"github.com/ionos-cloud/cluster-api-provider-proxmox/pkg/proxmox"
	"github.com/ionos-cloud/cluster-api-provider-proxmox/pkg/proxmox/clientfactory"
	"github.com/ionos-cloud/cluster-api-provider-proxmox/pkg/scope"
)

const (
	// ControlPlaneEndpointPort default API server port.
	ControlPlaneEndpointPort = 6443
)

// ProxmoxClusterReconciler reconciles a ProxmoxCluster object.
type ProxmoxClusterReconciler struct {
	client.Client
	Scheme        *runtime.Scheme
	Recorder      record.EventRecorder
	ProxmoxClient proxmox.Client
	ClientFactory clientfactory.Factory

	// zoneProbeTimes rate-limits the per-zone endpoint liveness probe;
	// keyed by "<namespace>/<cluster>/<zone>", values are time.Time.
	zoneProbeTimes sync.Map
}

// SetupWithManager sets up the controller with the Manager.
func (r *ProxmoxClusterReconciler) SetupWithManager(ctx context.Context, mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&infrav1.ProxmoxCluster{}).
		WithEventFilter(predicates.ResourceNotPaused(r.Scheme, ctrl.LoggerFrom(ctx))).
		Watches(&clusterv1.Cluster{},
			handler.EnqueueRequestsFromMapFunc(clusterutil.ClusterToInfrastructureMapFunc(ctx, infrav1.GroupVersion.WithKind(infrav1.ProxmoxClusterKind), mgr.GetClient(), &infrav1.ProxmoxCluster{})),
			builder.WithPredicates(predicates.ClusterUnpaused(r.Scheme, ctrl.LoggerFrom(ctx)))).
		WithEventFilter(predicates.ResourceIsNotExternallyManaged(r.Scheme, ctrl.LoggerFrom(ctx))).
		Complete(r)
}

// +kubebuilder:rbac:groups=core,resources=secrets,verbs=get;list;watch;patch
// +kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=proxmoxclusters,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=proxmoxclusters/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=proxmoxclusters/finalizers,verbs=update

// +kubebuilder:rbac:groups=cluster.x-k8s.io,resources=clusters;clusters/status,verbs=get;list;watch;patch

// +kubebuilder:rbac:groups=ipam.cluster.x-k8s.io,resources=inclusterippools,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=ipam.cluster.x-k8s.io,resources=globalinclusterippools,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=ipam.cluster.x-k8s.io,resources=ipaddresses,verbs=get;list;watch
// +kubebuilder:rbac:groups=ipam.cluster.x-k8s.io,resources=ipaddressclaims,verbs=get;list;watch;create;update;delete

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.14.4/pkg/reconcile
func (r *ProxmoxClusterReconciler) Reconcile(ctx context.Context, req ctrl.Request) (_ ctrl.Result, reterr error) {
	logger := log.FromContext(ctx)

	proxmoxCluster := &infrav1.ProxmoxCluster{}
	if err := r.Client.Get(ctx, req.NamespacedName, proxmoxCluster); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// Get owner cluster
	cluster, err := clusterutil.GetOwnerCluster(ctx, r.Client, proxmoxCluster.ObjectMeta)
	if err != nil {
		return ctrl.Result{}, err
	}
	if cluster == nil {
		logger.Info("Waiting for Cluster Controller to set OwnerRef on ProxmoxCluster")
		return ctrl.Result{}, nil
	}

	logger = logger.WithValues("cluster", klog.KObj(cluster))
	ctx = ctrl.LoggerInto(ctx, logger)

	if annotations.IsPaused(cluster, proxmoxCluster) {
		logger.Info("ProxmoxCluster or owning Cluster is marked as paused, not reconciling")

		return ctrl.Result{}, nil
	}

	// Create the scope.
	clusterScope, err := scope.NewClusterScope(scope.ClusterScopeParams{
		Client:         r.Client,
		Logger:         &logger,
		Cluster:        cluster,
		ProxmoxCluster: proxmoxCluster,
		ControllerName: "proxmoxcluster",
		ProxmoxClient:  r.ProxmoxClient,
		ClientFactory:  r.ClientFactory,
		IPAMHelper:     ipam.NewHelper(r.Client, proxmoxCluster.DeepCopy()),
	})
	if err != nil {
		return reconcile.Result{}, errors.Errorf("failed to create scope: %+v", err)
	}

	// Always close the scope when exiting this function so we can persist any ProxmoxCluster changes.
	defer func() {
		if err := clusterScope.Close(); err != nil && reterr == nil {
			reterr = err
		}
	}()

	// Handle deleted clusters
	if !proxmoxCluster.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, clusterScope)
	}

	// Handle non-deleted clusters
	return r.reconcileNormal(ctx, clusterScope)
}

func (r *ProxmoxClusterReconciler) reconcileDelete(ctx context.Context, clusterScope *scope.ClusterScope) (reconcile.Result, error) {
	// We want to prevent deletion unless the owning cluster was flagged for deletion.
	if clusterScope.Cluster.DeletionTimestamp.IsZero() {
		clusterScope.Error(errors.New("deletion was requested but owning cluster wasn't deleted"), "Unable to delete ProxmoxCluster")
		// We stop reconciling here. It will be triggered again once the owning cluster was deleted.
		return reconcile.Result{}, nil
	}

	clusterScope.Logger.V(4).Info("Reconciling ProxmoxCluster delete")
	// Deletion usually should be triggered through the deletion of the owning cluster.
	// If the ProxmoxCluster was also flagged for deletion (e.g. deletion using the manifest file)
	// we should only allow to remove the finalizer when there are no ProxmoxMachines left.
	machines, err := clusterScope.ListProxmoxMachinesForCluster(ctx)
	if err != nil {
		return reconcile.Result{}, errors.Wrapf(err, "could not retrieve proxmox machines for cluster %q", clusterScope.InfraClusterName())
	}

	// Requeue if there are one or more machines left.
	if len(machines) > 0 {
		clusterScope.Info("waiting for machines to be deleted", "remaining", len(machines))
		return ctrl.Result{RequeueAfter: infrav1.DefaultReconcilerRequeue}, nil
	}

	if err := r.reconcileDeleteCredentialsSecret(ctx, clusterScope); err != nil {
		return reconcile.Result{}, err
	}

	clusterScope.Info("cluster deleted successfully")
	ctrlutil.RemoveFinalizer(clusterScope.ProxmoxCluster, infrav1.ClusterFinalizer)
	return ctrl.Result{}, nil
}

func (r *ProxmoxClusterReconciler) reconcileNormal(ctx context.Context, clusterScope *scope.ClusterScope) (reconcile.Result, error) {
	clusterScope.Logger.Info("Reconciling ProxmoxCluster")

	// If the ProxmoxCluster doesn't have our finalizer, add it.
	ctrlutil.AddFinalizer(clusterScope.ProxmoxCluster, infrav1.ClusterFinalizer)

	if ptr.Deref(clusterScope.ProxmoxCluster.Spec.ExternalManagedControlPlane, false) {
		if clusterScope.ProxmoxCluster.Spec.ControlPlaneEndpoint.IsZero() {
			clusterScope.Logger.Info("ProxmoxCluster is not ready, missing or waiting for a ControlPlaneEndpoint")

			conditions.Set(clusterScope.ProxmoxCluster, metav1.Condition{
				Type:    infrav1.ProxmoxClusterProxmoxAvailableCondition,
				Status:  metav1.ConditionFalse,
				Reason:  infrav1.ProxmoxClusterProxmoxAvailableMissingControlPlaneEndpointReason,
				Message: "The ProxmoxCluster is missing or waiting for a ControlPlaneEndpoint",
			})

			return ctrl.Result{RequeueAfter: 200 * time.Millisecond}, nil
		}
	}

	res, err := r.reconcileIPAM(ctx, clusterScope)
	if err != nil {
		return ctrl.Result{}, err
	}

	if !res.IsZero() {
		return res, nil
	}

	r.reconcileFailureDomains(clusterScope)

	zoneRequeue := r.reconcileZoneClients(ctx, clusterScope)

	if err := r.reconcileNormalCredentialsSecret(ctx, clusterScope); err != nil {
		reason := infrav1.ProxmoxClusterProxmoxAvailableProxmoxUnreachableReason
		if apierrors.IsNotFound(err) {
			reason = infrav1.ProxmoxClusterProxmoxAvailableCredentialsNotFoundReason
		}
		conditions.Set(clusterScope.ProxmoxCluster, metav1.Condition{
			Type:    infrav1.ProxmoxClusterProxmoxAvailableCondition,
			Status:  metav1.ConditionFalse,
			Reason:  reason,
			Message: err.Error(),
		})
		return reconcile.Result{}, err
	}

	conditions.Set(clusterScope.ProxmoxCluster, metav1.Condition{
		Type:   infrav1.ProxmoxClusterProxmoxAvailableCondition,
		Status: metav1.ConditionTrue,
		Reason: clusterv1.ProvisionedReason,
	})

	clusterScope.SetReady()

	// re-run the zone endpoint probe periodically so ZonesAvailable both
	// detects outages of cached clients and recovers.
	return ctrl.Result{RequeueAfter: zoneRequeue}, nil
}

func (r *ProxmoxClusterReconciler) reconcileIPAM(ctx context.Context, clusterScope *scope.ClusterScope) (reconcile.Result, error) {
	if err := clusterScope.IPAMHelper.CreateOrUpdateInClusterIPPool(ctx); err != nil {
		if errors.Is(err, ipam.ErrMissingAddresses) {
			clusterScope.Info("Missing addresses in cluster IPAM config, not reconciling")
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	proxmoxCluster := clusterScope.ProxmoxCluster
	ipPools := []string{}
	if proxmoxCluster.Spec.IPv4Config != nil {
		ipPools = append(ipPools, ipam.InClusterPoolFormat(proxmoxCluster, nil, infrav1.IPv4Format))
	}
	if proxmoxCluster.Spec.IPv6Config != nil {
		ipPools = append(ipPools, ipam.InClusterPoolFormat(proxmoxCluster, nil, infrav1.IPv6Format))
	}
	for _, zone := range proxmoxCluster.Spec.ZoneConfigs {
		if zone.IPv4Config != nil {
			ipPools = append(ipPools, ipam.InClusterPoolFormat(proxmoxCluster, zone.Zone, infrav1.IPv4Format))
		}
		if zone.IPv6Config != nil {
			ipPools = append(ipPools, ipam.InClusterPoolFormat(proxmoxCluster, zone.Zone, infrav1.IPv6Format))
		}
	}

	for _, poolName := range ipPools {
		pool, err := clusterScope.IPAMHelper.GetIPPool(ctx, corev1.TypedLocalObjectReference{
			APIGroup: consts.GetIPAMInClusterAPIGroup(),
			Name:     poolName,
			Kind:     consts.GetInClusterIPPoolKind(),
		})
		if err != nil {
			if apierrors.IsNotFound(err) {
				return ctrl.Result{RequeueAfter: infrav1.DefaultReconcilerRequeue}, nil
			}

			return ctrl.Result{}, err
		}
		clusterScope.ProxmoxCluster.SetInClusterIPPoolRef(pool)
	}

	return reconcile.Result{}, nil
}

// reconcileFailureDomains populates Status.FailureDomains from ZoneConfigs.
// When no zones are configured, FailureDomains is set to nil, preserving
// backwards compatibility (KCP will not attempt failure domain placement).
func (r *ProxmoxClusterReconciler) reconcileFailureDomains(clusterScope *scope.ClusterScope) {
	pc := clusterScope.ProxmoxCluster
	if len(pc.Spec.ZoneConfigs) == 0 {
		pc.Status.FailureDomains = nil
		return
	}

	faildoms := make([]clusterv1.FailureDomain, 0, len(pc.Spec.ZoneConfigs))
	for _, zc := range pc.Spec.ZoneConfigs {
		zoneName := ptr.Deref(zc.Zone, "")
		if zoneName == "" {
			continue
		}

		faildoms = append(faildoms, clusterv1.FailureDomain{
			Name:         zoneName,
			ControlPlane: new(ptr.Deref(zc.ControlPlaneEligible, true)),
		})
	}

	sort.Slice(faildoms, func(i, j int) bool { return faildoms[i].Name < faildoms[j].Name })
	pc.Status.FailureDomains = faildoms
}

// zoneProbeInterval rate-limits the live Version probe per zone endpoint,
// and is the steady-state requeue interval of clusters with credentialed
// zones. zoneUnreachableRequeue retries faster while a zone is down.
const (
	zoneProbeInterval      = 5 * time.Minute
	zoneUnreachableRequeue = 2 * time.Minute
)

// reconcileZoneClients checks the Proxmox endpoint of every zone that
// carries its own credentialsRef and surfaces the result as the standalone
// ZonesAvailable condition plus warning events. Resolution goes through the
// client factory cache; a rate-limited live Version probe catches endpoints
// that died after the client was cached, and eviction makes the next
// resolution rebuild from scratch. Failures never block reconciliation:
// machines in healthy zones must keep reconciling, and the machine
// controller already gates per-machine on the zone client.
//
// The returned duration is the requeue interval keeping the probe (and its
// recovery) running; zero when the cluster has no credentialed zones.
func (r *ProxmoxClusterReconciler) reconcileZoneClients(ctx context.Context, clusterScope *scope.ClusterScope) time.Duration {
	var probed, unreachable []string

	for _, zc := range clusterScope.ProxmoxCluster.Spec.ZoneConfigs {
		if zc.CredentialsRef == nil {
			continue
		}

		zoneName := ptr.Deref(zc.Zone, "")
		probed = append(probed, zoneName)

		zoneClient, err := clusterScope.ClientForZone(ctx, zc.Zone)
		if err != nil {
			unreachable = append(unreachable, zoneName)
			r.Recorder.Eventf(clusterScope.ProxmoxCluster, corev1.EventTypeWarning, infrav1.ProxmoxClusterZonesAvailableZoneUnreachableReason,
				"Proxmox endpoint for zone %q is unavailable: %v", zoneName, err)
			continue
		}

		// live probe, rate-limited per zone: the cached client was only
		// verified reachable at construction time.
		probeKey := clusterScope.ProxmoxCluster.Namespace + "/" + clusterScope.ProxmoxCluster.Name + "/" + zoneName
		if last, ok := r.zoneProbeTimes.Load(probeKey); ok && time.Since(last.(time.Time)) < zoneProbeInterval {
			continue
		}

		if _, err := zoneClient.Version(ctx); err != nil {
			unreachable = append(unreachable, zoneName)
			r.Recorder.Eventf(clusterScope.ProxmoxCluster, corev1.EventTypeWarning, infrav1.ProxmoxClusterZonesAvailableZoneUnreachableReason,
				"Proxmox endpoint for zone %q failed the liveness probe: %v", zoneName, err)

			// evict the cached client so the next resolution rebuilds it
			// (and re-verifies the endpoint) from scratch.
			namespace := zc.CredentialsRef.Namespace
			if namespace == "" {
				namespace = clusterScope.ProxmoxCluster.Namespace
			}
			if r.ClientFactory != nil {
				r.ClientFactory.Evict(namespace, zc.CredentialsRef.Name)
			}
			r.zoneProbeTimes.Delete(probeKey)
			continue
		}

		r.zoneProbeTimes.Store(probeKey, time.Now())
	}

	if len(probed) == 0 {
		conditions.Delete(clusterScope.ProxmoxCluster, infrav1.ProxmoxClusterZonesAvailableCondition)
		return 0
	}

	if len(unreachable) > 0 {
		conditions.Set(clusterScope.ProxmoxCluster, metav1.Condition{
			Type:    infrav1.ProxmoxClusterZonesAvailableCondition,
			Status:  metav1.ConditionFalse,
			Reason:  infrav1.ProxmoxClusterZonesAvailableZoneUnreachableReason,
			Message: fmt.Sprintf("Proxmox endpoint unavailable for zones: %s", strings.Join(unreachable, ", ")),
		})
		return zoneUnreachableRequeue
	}

	conditions.Set(clusterScope.ProxmoxCluster, metav1.Condition{
		Type:   infrav1.ProxmoxClusterZonesAvailableCondition,
		Status: metav1.ConditionTrue,
		Reason: infrav1.ProxmoxClusterZonesAvailableReason,
	})
	return zoneProbeInterval
}

func (r *ProxmoxClusterReconciler) reconcileNormalCredentialsSecret(ctx context.Context, clusterScope *scope.ClusterScope) error {
	proxmoxCluster := clusterScope.ProxmoxCluster

	current := credentialsSecretKeys(proxmoxCluster)
	currentSet := make(map[client.ObjectKey]struct{}, len(current))
	for _, key := range current {
		currentSet[key] = struct{}{}
	}

	// Release secrets we adopted earlier that are no longer referenced
	// (a zone credentialsRef was removed or now names a different secret),
	// so ownerRef and finalizer never outlive the reference. Keys we fail
	// to release stay tracked and are retried next reconcile.
	managed := managedSecretKeys(proxmoxCluster)
	for _, key := range managed {
		if _, stillReferenced := currentSet[key]; stillReferenced {
			continue
		}
		if err := r.releaseCredentialsSecret(ctx, proxmoxCluster, key); err != nil {
			currentSet[key] = struct{}{} // keep tracking, retry later
			r.Recorder.Eventf(proxmoxCluster, corev1.EventTypeWarning, "CredentialsSecretReleaseFailed",
				"failed to release credentials secret %s: %v", key, err)
		}
	}

	clusterKey, hasClusterKey := clusterCredentialsSecretKey(proxmoxCluster)

	for _, secretKey := range current {
		if err := r.adoptCredentialsSecret(ctx, proxmoxCluster, secretKey); err != nil {
			// The cluster-level secret keeps its historical semantics: its
			// absence fails the cluster. Zone secret problems must never
			// block the cluster (or the other zones); they already surface
			// through the ZonesAvailable condition and its events.
			if hasClusterKey && secretKey == clusterKey {
				setManagedSecretKeys(proxmoxCluster, currentSet)
				return err
			}
			delete(currentSet, secretKey)
			r.Recorder.Eventf(proxmoxCluster, corev1.EventTypeWarning, "CredentialsSecretAdoptionFailed",
				"failed to adopt zone credentials secret %s: %v", secretKey, err)
		}
	}

	setManagedSecretKeys(proxmoxCluster, currentSet)
	return nil
}

func (r *ProxmoxClusterReconciler) reconcileDeleteCredentialsSecret(ctx context.Context, clusterScope *scope.ClusterScope) error {
	proxmoxCluster := clusterScope.ProxmoxCluster

	// Release everything: currently referenced secrets plus any stale
	// tracked ones. Attempt all of them before reporting an error so one
	// broken secret does not shield the others from cleanup.
	keys := credentialsSecretKeys(proxmoxCluster)
	seen := make(map[client.ObjectKey]struct{}, len(keys))
	for _, key := range keys {
		seen[key] = struct{}{}
	}
	for _, key := range managedSecretKeys(proxmoxCluster) {
		if _, ok := seen[key]; !ok {
			keys = append(keys, key)
		}
	}

	var errs []error
	for _, secretKey := range keys {
		if err := r.releaseCredentialsSecret(ctx, proxmoxCluster, secretKey); err != nil {
			errs = append(errs, errors.Wrapf(err, "secret %s", secretKey))
		}
	}

	return kerrors.NewAggregate(errs)
}

// adoptCredentialsSecret marks a credentials secret as used by this
// ProxmoxCluster: ownerRef for garbage collection plus the SecretFinalizer.
func (r *ProxmoxClusterReconciler) adoptCredentialsSecret(ctx context.Context, proxmoxCluster *infrav1.ProxmoxCluster, secretKey client.ObjectKey) error {
	secret := &corev1.Secret{}
	if err := r.Client.Get(ctx, secretKey, secret); err != nil {
		return err
	}

	helper, err := patch.NewHelper(secret, r.Client)
	if err != nil {
		return err
	}

	// Ensure the ProxmoxCluster is an owner and that the APIVersion is up-to-date.
	secret.SetOwnerReferences(clusterutil.EnsureOwnerRef(secret.GetOwnerReferences(),
		metav1.OwnerReference{
			APIVersion: infrav1.GroupVersion.String(),
			Kind:       "ProxmoxCluster",
			Name:       proxmoxCluster.Name,
			UID:        proxmoxCluster.UID,
		},
	))

	// Ensure the finalizer is added.
	if !ctrlutil.ContainsFinalizer(secret, infrav1.SecretFinalizer) {
		ctrlutil.AddFinalizer(secret, infrav1.SecretFinalizer)
	}

	return helper.Patch(ctx, secret)
}

// releaseCredentialsSecret undoes adoptCredentialsSecret, respecting other
// ProxmoxCluster owners of a shared secret. A missing secret is fine.
func (r *ProxmoxClusterReconciler) releaseCredentialsSecret(ctx context.Context, proxmoxCluster *infrav1.ProxmoxCluster, secretKey client.ObjectKey) error {
	logger := ctrl.LoggerFrom(ctx)

	secret := &corev1.Secret{}
	if err := r.Client.Get(ctx, secretKey, secret); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}

	helper, err := patch.NewHelper(secret, r.Client)
	if err != nil {
		return err
	}

	ownerRef := metav1.OwnerReference{
		APIVersion: infrav1.GroupVersion.String(),
		Kind:       "ProxmoxCluster",
		Name:       proxmoxCluster.Name,
		UID:        proxmoxCluster.UID,
	}

	if len(secret.GetOwnerReferences()) > 1 {
		// Remove the ProxmoxCluster from the OwnerRef.
		secret.SetOwnerReferences(clusterutil.RemoveOwnerRef(secret.GetOwnerReferences(), ownerRef))
	} else if clusterutil.HasOwnerRef(secret.GetOwnerReferences(), ownerRef) && ctrlutil.ContainsFinalizer(secret, infrav1.SecretFinalizer) {
		// There is only one OwnerRef, the current ProxmoxCluster. Remove the Finalizer (if present).
		logger.Info(fmt.Sprintf("Removing finalizer %s", infrav1.SecretFinalizer), "Secret", klog.KObj(secret))
		ctrlutil.RemoveFinalizer(secret, infrav1.SecretFinalizer)
	}

	return helper.Patch(ctx, secret)
}

// managedSecretKeys returns the credentials secrets this ProxmoxCluster
// adopted, as tracked by the managed-credentials annotation.
func managedSecretKeys(proxmoxCluster *infrav1.ProxmoxCluster) []client.ObjectKey {
	value := proxmoxCluster.GetAnnotations()[infrav1.ManagedCredentialsSecretsAnnotation]
	if value == "" {
		return nil
	}

	var keys []client.ObjectKey
	for _, item := range strings.Split(value, ",") {
		namespace, name, found := strings.Cut(item, "/")
		if !found || namespace == "" || name == "" {
			continue
		}
		keys = append(keys, client.ObjectKey{Namespace: namespace, Name: name})
	}

	return keys
}

// setManagedSecretKeys records the adopted credentials secrets on the
// managed-credentials annotation; the scope patch persists it.
func setManagedSecretKeys(proxmoxCluster *infrav1.ProxmoxCluster, keys map[client.ObjectKey]struct{}) {
	items := make([]string, 0, len(keys))
	for key := range keys {
		items = append(items, key.Namespace+"/"+key.Name)
	}
	sort.Strings(items)

	annotations := proxmoxCluster.GetAnnotations()
	if len(items) == 0 {
		delete(annotations, infrav1.ManagedCredentialsSecretsAnnotation)
		return
	}
	if annotations == nil {
		annotations = map[string]string{}
	}
	annotations[infrav1.ManagedCredentialsSecretsAnnotation] = strings.Join(items, ",")
	proxmoxCluster.SetAnnotations(annotations)
}

// clusterCredentialsSecretKey returns the key of the cluster-level
// credentials secret, if a credentialsRef is set.
func clusterCredentialsSecretKey(proxmoxCluster *infrav1.ProxmoxCluster) (client.ObjectKey, bool) {
	ref := proxmoxCluster.Spec.CredentialsRef
	if ref == nil || ref.Name == "" {
		return client.ObjectKey{}, false
	}
	namespace := ref.Namespace
	if len(namespace) == 0 {
		namespace = proxmoxCluster.GetNamespace()
	}
	return client.ObjectKey{Namespace: namespace, Name: ref.Name}, true
}

// credentialsSecretKeys returns the deduplicated object keys of every
// credentials secret referenced by the ProxmoxCluster: the cluster-level
// credentialsRef plus all zone-level ones. References without a namespace
// default to the ProxmoxCluster namespace.
func credentialsSecretKeys(proxmoxCluster *infrav1.ProxmoxCluster) []client.ObjectKey {
	if proxmoxCluster == nil {
		return nil
	}

	var keys []client.ObjectKey
	seen := make(map[client.ObjectKey]struct{})

	add := func(ref *corev1.SecretReference) {
		if ref == nil || ref.Name == "" {
			return
		}
		namespace := ref.Namespace
		if len(namespace) == 0 {
			namespace = proxmoxCluster.GetNamespace()
		}
		key := client.ObjectKey{Namespace: namespace, Name: ref.Name}
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		keys = append(keys, key)
	}

	add(proxmoxCluster.Spec.CredentialsRef)
	for _, zc := range proxmoxCluster.Spec.ZoneConfigs {
		add(zc.CredentialsRef)
	}

	return keys
}
