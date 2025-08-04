package v1alpha1

import (
	"fmt"
	"github.com/ionos-cloud/cluster-api-provider-proxmox/api/v1alpha2"
	conversion_machinery "k8s.io/apimachinery/pkg/conversion"
	"sigs.k8s.io/controller-runtime/pkg/conversion"
)

// ConvertTo converts this ProxmoxCluster (v1alpha1) to the Hub version (v1alpha2).
func (c *ProxmoxCluster) ConvertTo(dstRaw conversion.Hub) error {
	dst := dstRaw.(*v1alpha2.ProxmoxCluster)
	fmt.Println(dst)
	return nil
}

// ConvertFrom converts the Hub version (v1alpha2) to this ProxmoxCluster (v1alpha1).
func (c *ProxmoxCluster) ConvertFrom(srcRaw conversion.Hub) error {
	src := srcRaw.(*v1alpha2.ProxmoxCluster)
	fmt.Println(src)
	return nil
}

func Convert_v1alpha2_ProxmoxClusterSpec_To_v1alpha1_ProxmoxClusterSpec(in *v1alpha2.ProxmoxClusterSpec, out *ProxmoxClusterSpec, s conversion_machinery.Scope) error {
	out.AllowedNodes = in.AllowedNodes
	out.DNSServers = in.DNSServers
	out.ControlPlaneEndpoint = in.ControlPlaneEndpoint
	out.ExternalManagedControlPlane = in.ExternalManagedControlPlane
	if err := Convert_v1alpha1_IPConfigSpec_To_v1alpha2_IPConfigSpec(out.IPv6Config, in.IPv6Config, s); err != nil {
		return err
	}
	return nil
}
