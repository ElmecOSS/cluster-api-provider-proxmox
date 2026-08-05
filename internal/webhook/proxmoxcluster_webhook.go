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

// Package webhook contains webhooks for the custom resources.
package webhook

import (
	"context"
	"fmt"
	"net/netip"
	"regexp"
	"sort"
	"strings"

	"github.com/pkg/errors"
	"go4.org/netipx"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	infrav1 "github.com/ionos-cloud/cluster-api-provider-proxmox/api/v1alpha2"
)

var _ admission.CustomValidator = &ProxmoxCluster{}

// ProxmoxCluster is a type that implements
// the interfaces from the admission package.
type ProxmoxCluster struct{}

// SetupWebhookWithManager sets up the webhook with the
// custom interfaces.
func (p *ProxmoxCluster) SetupWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr).
		For(&infrav1.ProxmoxCluster{}).
		WithValidator(p).
		WithDefaulter(p).
		Complete()
}

// +kubebuilder:webhook:verbs=create;update,path=/validate-infrastructure-cluster-x-k8s-io-v1alpha2-proxmoxcluster,mutating=false,failurePolicy=fail,matchPolicy=Equivalent,sideEffects=None,groups=infrastructure.cluster.x-k8s.io,resources=proxmoxclusters,versions=v1alpha2,name=validation.proxmoxcluster.infrastructure.cluster.x-k8s.io,admissionReviewVersions=v1
// +kubebuilder:webhook:verbs=create;update,path=/mutate-infrastructure-cluster-x-k8s-io-v1alpha2-proxmoxcluster,mutating=true,failurePolicy=fail,matchPolicy=Equivalent,sideEffects=None,groups=infrastructure.cluster.x-k8s.io,resources=proxmoxclusters,versions=v1alpha2,name=default.proxmoxcluster.infrastructure.cluster.x-k8s.io,admissionReviewVersions=v1

// Default implements the defaulting (mutating) webhook for ProxmoxCluster.
func (p *ProxmoxCluster) Default(_ context.Context, _ runtime.Object) error {
	return nil
}

// ValidateCreate implements the creation validation function.
func (*ProxmoxCluster) ValidateCreate(_ context.Context, obj runtime.Object) (warnings admission.Warnings, err error) {
	cluster, ok := obj.(*infrav1.ProxmoxCluster)
	if !ok {
		return warnings, apierrors.NewBadRequest(fmt.Sprintf("expected a ProxmoxCluster but got %T", obj))
	}

	if hasNoIPPoolConfig(&cluster.Spec) {
		err = errors.New("proxmox cluster must define at least one IP pool config")
		warnings = append(warnings, fmt.Sprintf("proxmox cluster must define at least one IP pool config %s", cluster.GetName()))
		return warnings, err
	}

	if err := validateControlPlaneEndpoint(&cluster.Spec, cluster.GroupVersionKind().GroupKind(), cluster.GetName()); err != nil {
		warnings = append(warnings, fmt.Sprintf("cannot create proxmox cluster %s", cluster.GetName()))
		return warnings, err
	}

	if err := validateZoneCredentials(cluster); err != nil {
		return warnings, err
	}

	return warnings, nil
}

// ValidateDelete implements the deletion validation function.
func (*ProxmoxCluster) ValidateDelete(_ context.Context, _ runtime.Object) (admission.Warnings, error) {
	return nil, nil
}

// ValidateUpdate implements the update validation function.
func (*ProxmoxCluster) ValidateUpdate(_ context.Context, oldObj runtime.Object, newObj runtime.Object) (warnings admission.Warnings, err error) {
	newCluster, ok := newObj.(*infrav1.ProxmoxCluster)
	if !ok {
		return warnings, apierrors.NewBadRequest(fmt.Sprintf("expected a ProxmoxCluster but got %T", newCluster))
	}

	if err := validateControlPlaneEndpoint(&newCluster.Spec, newCluster.GroupVersionKind().GroupKind(), newCluster.GetName()); err != nil {
		warnings = append(warnings, fmt.Sprintf("cannot update proxmox cluster %s", newCluster.GetName()))
		return warnings, err
	}

	if err := validateZoneCredentials(newCluster); err != nil {
		return warnings, err
	}

	if oldCluster, ok := oldObj.(*infrav1.ProxmoxCluster); ok {
		if err := validateZoneRemoval(oldCluster, newCluster); err != nil {
			return warnings, err
		}
		warnings = append(warnings, zoneChangeWarnings(oldCluster, newCluster)...)
	}

	warnings = append(warnings, zoneCredentialsWarnings(newCluster)...)

	return warnings, nil
}

// validateZoneCredentials requires a non-empty secret name on every zone
// credentialsRef and reserves the implicit zone name "default": machines may
// reference it without any zoneConfig entry (it maps to the cluster client
// and the cluster-level IPAM pools), so a later explicit "default" zone
// would silently re-route them and its pools would collide with the
// cluster-level ones in status.inClusterZoneRef.
func validateZoneCredentials(cluster *infrav1.ProxmoxCluster) error {
	for i, zc := range cluster.Spec.ZoneConfigs {
		if ptr.Deref(zc.Zone, "") == "default" {
			return apierrors.NewInvalid(
				cluster.GroupVersionKind().GroupKind(),
				cluster.GetName(),
				field.ErrorList{
					field.Invalid(
						field.NewPath("spec", "zoneConfig").Index(i).Child("zone"),
						"default", `"default" is the reserved name of the implicit zone backed by the cluster-level configuration`),
				})
		}

		if zc.CredentialsRef != nil && zc.CredentialsRef.Name == "" {
			return apierrors.NewInvalid(
				cluster.GroupVersionKind().GroupKind(),
				cluster.GetName(),
				field.ErrorList{
					field.Invalid(
						field.NewPath("spec", "zoneConfig").Index(i).Child("credentialsRef", "name"),
						zc.CredentialsRef.Name, "credentialsRef must name a secret"),
				})
		}
	}

	return nil
}

// zoneCredentialsWarnings warns about cross-namespace zone credentials:
// the controller sets an ownerReference on the secret, and Kubernetes
// garbage collection treats cross-namespace owners as absent, which can
// lead to unexpected secret deletion.
func zoneCredentialsWarnings(cluster *infrav1.ProxmoxCluster) admission.Warnings {
	var warnings admission.Warnings
	for i, zc := range cluster.Spec.ZoneConfigs {
		if zc.CredentialsRef != nil && zc.CredentialsRef.Namespace != "" && zc.CredentialsRef.Namespace != cluster.GetNamespace() {
			warnings = append(warnings, fmt.Sprintf(
				"spec.zoneConfig[%d].credentialsRef points to namespace %q, different from the ProxmoxCluster namespace: the ownerReference set on the secret is cross-namespace, which Kubernetes GC may treat as an absent owner", i, zc.CredentialsRef.Namespace))
		}
	}
	return warnings
}

// validateZoneRemoval denies removing a zone, or changing the identity of
// its credentialsRef, while machines recorded in status.nodeLocations still
// live in that zone. A removed zone wedges its machines (the zone client
// resolves fail-closed, deletion included); a swapped credentialsRef is
// worse — machine operations would be routed to a different Proxmox
// endpoint, where a same-VMID lookup can adopt or destroy an unrelated VM.
func validateZoneRemoval(oldCluster, newCluster *infrav1.ProxmoxCluster) error {
	type zoneState struct {
		exists bool
		creds  client.ObjectKey
		hasRef bool
	}

	credsKey := func(cluster *infrav1.ProxmoxCluster, ref *corev1.SecretReference) (client.ObjectKey, bool) {
		if ref == nil {
			return client.ObjectKey{}, false
		}
		namespace := ref.Namespace
		if namespace == "" {
			namespace = cluster.GetNamespace()
		}
		return client.ObjectKey{Namespace: namespace, Name: ref.Name}, true
	}

	newZones := make(map[string]zoneState, len(newCluster.Spec.ZoneConfigs))
	for _, zc := range newCluster.Spec.ZoneConfigs {
		key, hasRef := credsKey(newCluster, zc.CredentialsRef)
		newZones[ptr.Deref(zc.Zone, "")] = zoneState{exists: true, creds: key, hasRef: hasRef}
	}

	for _, zc := range oldCluster.Spec.ZoneConfigs {
		zoneName := ptr.Deref(zc.Zone, "")
		oldKey, oldHasRef := credsKey(oldCluster, zc.CredentialsRef)

		state := newZones[zoneName]
		var reason string
		switch {
		case !state.exists:
			reason = "cannot remove zone"
		case oldHasRef != state.hasRef || (oldHasRef && oldKey != state.creds):
			reason = "cannot change the credentialsRef of zone"
		default:
			continue
		}

		if machine := firstMachineInZone(newCluster, zoneName); machine != "" {
			return apierrors.NewInvalid(
				newCluster.GroupVersionKind().GroupKind(),
				newCluster.GetName(),
				field.ErrorList{
					field.Forbidden(
						field.NewPath("spec", "zoneConfig"),
						fmt.Sprintf("%s %q: machine %q still placed in this zone (see status.nodeLocations); delete or move those machines first", reason, zoneName, machine)),
				})
		}
	}

	return nil
}

// zoneChangeWarnings returns admission warnings for risky-but-allowed zone
// updates: guarded changes while legacy nodeLocations entries carry no zone
// (they cannot be attributed to a zone, so the deny check is blind to them).
func zoneChangeWarnings(oldCluster, newCluster *infrav1.ProxmoxCluster) admission.Warnings {
	if newCluster.Status.NodeLocations == nil {
		return nil
	}

	nilZoneMachines := 0
	for _, locs := range [][]infrav1.NodeLocation{newCluster.Status.NodeLocations.ControlPlane, newCluster.Status.NodeLocations.Workers} {
		for _, loc := range locs {
			if loc.Zone == nil {
				nilZoneMachines++
			}
		}
	}
	if nilZoneMachines == 0 {
		return nil
	}

	oldZones := zoneConfigFingerprint(oldCluster)
	newZones := zoneConfigFingerprint(newCluster)
	if oldZones == newZones {
		return nil
	}

	return admission.Warnings{fmt.Sprintf(
		"%d machine(s) in status.nodeLocations have no recorded zone; the zone-removal safety check cannot attribute them to a zone — verify none of them lives in a zone you are removing or re-pointing", nilZoneMachines)}
}

func zoneConfigFingerprint(cluster *infrav1.ProxmoxCluster) string {
	parts := make([]string, 0, len(cluster.Spec.ZoneConfigs))
	for _, zc := range cluster.Spec.ZoneConfigs {
		ref := ""
		if zc.CredentialsRef != nil {
			// default the namespace as validateZoneRemoval does, so making
			// the cluster namespace explicit is not flagged as a change.
			namespace := zc.CredentialsRef.Namespace
			if namespace == "" {
				namespace = cluster.GetNamespace()
			}
			ref = namespace + "/" + zc.CredentialsRef.Name
		}
		parts = append(parts, ptr.Deref(zc.Zone, "")+"="+ref)
	}
	sort.Strings(parts)
	return strings.Join(parts, ",")
}

func firstMachineInZone(cluster *infrav1.ProxmoxCluster, zoneName string) string {
	if cluster.Status.NodeLocations == nil {
		return ""
	}

	for _, locs := range [][]infrav1.NodeLocation{cluster.Status.NodeLocations.ControlPlane, cluster.Status.NodeLocations.Workers} {
		for _, loc := range locs {
			if ptr.Deref(loc.Zone, "") == zoneName {
				return loc.Machine.Name
			}
		}
	}

	return ""
}

func validateControlPlaneEndpoint(spec *infrav1.ProxmoxClusterSpec, gk schema.GroupKind, name string) error {
	// Skipping the validation of the Control Plane endpoint in case of externally managed Control Plane:
	// the Cluster API Control Plane provider will eventually provide the LB.
	if ptr.Deref(spec.ExternalManagedControlPlane, false) {
		return nil
	}

	endpoint := spec.ControlPlaneEndpoint.Host

	addr, err := netip.ParseAddr(endpoint)

	/*
	   No further validation is done on hostnames. Checking DNS records
	   incures a lot of complexity. To list a few of the problems:
	    - DNS TTL will lead to incorrect results
	    - IP addresses can be PTR records
	    - Both A and AAAA records would need checking
	    - A record can have multiple entries, each of which need to be checked
	    - A valid record can start with _, but that is not a valid hostname
	    - ...
	   Most importantly, cluster-api does not validate controlPlaneEndpoint
	   at all.
	*/
	match := isHostname(endpoint)
	if match {
		return nil
	}

	if err != nil {
		return apierrors.NewInvalid(
			gk,
			name,
			field.ErrorList{
				field.Invalid(
					field.NewPath("spec", "controlplaneEndpoint"), endpoint, "provided endpoint address is not a valid IP or FQDN"),
			})
	}

	// IPv4
	if spec.IPv4Config != nil {
		set, err := buildSetFromAddresses(spec.IPv4Config.Addresses)
		if err != nil {
			return apierrors.NewInvalid(
				gk,
				name,
				field.ErrorList{
					field.Invalid(
						field.NewPath("spec", "IPv4Config", "addresses"), spec.IPv4Config.Addresses, "provided addresses are not valid IP addresses, ranges or CIDRs"),
				})
		}

		if set.Contains(addr) {
			return apierrors.NewInvalid(
				gk,
				name,
				field.ErrorList{
					field.Invalid(
						field.NewPath("spec", "IPv4Config", "addresses"), spec.IPv4Config.Addresses, "addresses may not contain the endpoint IP"),
				})
		}
	}

	// IPv6
	if spec.IPv6Config != nil {
		set6, err := buildSetFromAddresses(spec.IPv6Config.Addresses)
		if err != nil {
			return apierrors.NewInvalid(
				gk,
				name,
				field.ErrorList{
					field.Invalid(
						field.NewPath("spec", "IPv6Config", "addresses"), spec.IPv6Config.Addresses, "provided addresses are not valid IP addresses, ranges or CIDRs"),
				})
		}

		if set6.Contains(addr) {
			return apierrors.NewInvalid(
				gk,
				name,
				field.ErrorList{
					field.Invalid(
						field.NewPath("spec", "IPv6Config", "addresses"), spec.IPv6Config.Addresses, "addresses may not contain the endpoint IP"),
				})
		}
	}

	return nil
}

func buildSetFromAddresses(addresses []string) (*netipx.IPSet, error) {
	builder := netipx.IPSetBuilder{}

	for _, address := range addresses {
		switch {
		case strings.Contains(address, "-"):
			ipRange, err := netipx.ParseIPRange(address)
			if err != nil {
				return nil, err
			}
			builder.AddRange(ipRange)
		case strings.Contains(address, "/"):
			ipPref, err := netip.ParsePrefix(address)
			if err != nil {
				return nil, err
			}
			builder.AddPrefix(ipPref)
		default:
			ipAddress, err := netip.ParseAddr(address)
			if err != nil {
				return nil, err
			}

			builder.Add(ipAddress)
		}
	}

	set, err := builder.IPSet()
	if err != nil {
		return nil, err
	}

	return set, nil
}

func hasNoIPPoolConfig(spec *infrav1.ProxmoxClusterSpec) bool {
	return spec.IPv4Config == nil && spec.IPv6Config == nil
}

func isHostname(h string) bool {
	// shortname is up to 253 bytes long
	shortname := `([a-z0-9]{1,253}|[a-z0-9][a-z0-9-]{1,251}[a-z0-9])`
	// hostname is optional in a domain
	hostname := `([a-z0-9]{1,63}|[a-z0-9][a-z0-9-]{1,61}[a-z0-9]\.)?`
	domain := `((([a-z0-9]{1,63}|[a-z0-9][a-z0-9-]{1,61}[a-z0-9])\.)+?[a-z]{2,63})`

	// make hostname match case insensitive, match complete string
	hostmatch := `(?i)^(` + shortname + `|` + hostname + domain + `)$`

	match, _ := regexp.Match(hostmatch, []byte(h))
	return match
}
