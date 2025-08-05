package v1alpha1

import (
	"fmt"
	"github.com/ionos-cloud/cluster-api-provider-proxmox/api/v1alpha2"
	conversion_machinery "k8s.io/apimachinery/pkg/conversion"
	utilconversion "sigs.k8s.io/cluster-api/util/conversion"
	"sigs.k8s.io/controller-runtime/pkg/conversion"
)

// ConvertTo converts this ProxmoxCluster (v1alpha1) to the Hub version (v1alpha2).
func (c *ProxmoxCluster) ConvertTo(dstRaw conversion.Hub) error {
	dst := dstRaw.(*v1alpha2.ProxmoxCluster)
	if err := Convert_v1alpha1_ProxmoxCluster_To_v1alpha2_ProxmoxCluster(c, dst, nil); err != nil {
		return err
	}
	fmt.Println("ConvertTo Test")
	// Manually restore data.
	restored := &v1alpha2.ProxmoxCluster{}
	if ok, err := utilconversion.UnmarshalData(c, restored); err != nil || !ok {
		return err
	}

	return nil
}

// ConvertFrom converts the Hub version (v1alpha2) to this ProxmoxCluster (v1alpha1).
func (c *ProxmoxCluster) ConvertFrom(srcRaw conversion.Hub) error {
	src := srcRaw.(*v1alpha2.ProxmoxCluster)
	if err := Convert_v1alpha2_ProxmoxCluster_To_v1alpha1_ProxmoxCluster(src, c, nil); err != nil {
		return err
	}
	fmt.Println("ConvertFrom Test")

	// Preserve Hub data on down-conversion.
	if err := utilconversion.MarshalData(src, c); err != nil {
		return err
	}
	return nil
}

func Convert_v1alpha2_ProxmoxClusterSpec_To_v1alpha1_ProxmoxClusterSpec(in *v1alpha2.ProxmoxClusterSpec, out *ProxmoxClusterSpec, s conversion_machinery.Scope) error {
	out.AllowedNodes = in.AllowedNodes
	out.DNSServers = in.DNSServers
	out.ControlPlaneEndpoint = in.ControlPlaneEndpoint
	out.ExternalManagedControlPlane = in.ExternalManagedControlPlane
	out.SchedulerHints = (*SchedulerHints)(in.SchedulerHints)
	out.IPv4Config = (*IPConfigSpec)(in.IPv4Config)
	out.IPv6Config = (*IPConfigSpec)(in.IPv6Config)
	out.CredentialsRef = in.CredentialsRef
	// Note: Settings field from v1alpha2 is not supported in v1alpha1 and will be ignored during conversion

	return nil
}
