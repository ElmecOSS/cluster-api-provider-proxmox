package v1alpha1

import (
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

	// Preserve Hub data on down-conversion.
	if err := utilconversion.MarshalData(src, c); err != nil {
		return err
	}
	return nil
}

func Convert_v1alpha2_ProxmoxClusterSpec_To_v1alpha1_ProxmoxClusterSpec(in *v1alpha2.ProxmoxClusterSpec, out *ProxmoxClusterSpec, s conversion_machinery.Scope) error {
	if in.Settings != nil && len(in.Settings.Instances) > 0 && in.Settings.Mode == v1alpha2.DefaultMode {
		out.AllowedNodes = in.Settings.Instances[0].Nodes
	} else {
		out.AllowedNodes = in.AllowedNodes
	}
	out.DNSServers = in.DNSServers
	out.ControlPlaneEndpoint = in.ControlPlaneEndpoint
	out.ExternalManagedControlPlane = in.ExternalManagedControlPlane
	out.SchedulerHints = (*SchedulerHints)(in.SchedulerHints)
	out.IPv4Config = (*IPConfigSpec)(in.IPv4Config)
	out.IPv6Config = (*IPConfigSpec)(in.IPv6Config)
	if in.CloneSpec != nil {
		out.CloneSpec = &ProxmoxClusterCloneSpec{}
		if err := Convert_v1alpha2_ProxmoxClusterCloneSpec_To_v1alpha1_ProxmoxClusterCloneSpec(in.CloneSpec, out.CloneSpec, s); err != nil {
			return err
		}
	}
	out.CredentialsRef = in.CredentialsRef
	// Note: Settings field from v1alpha2 is not supported in v1alpha1 and will be ignored during conversion

	return nil
}

func Convert_v1alpha2_ProxmoxMachineSpec_To_v1alpha1_ProxmoxMachineSpec(in *v1alpha2.ProxmoxMachineSpec, out *ProxmoxMachineSpec, s conversion_machinery.Scope) error {
	out.VirtualMachineCloneSpec.TemplateSelector = (*TemplateSelector)(in.VirtualMachineCloneSpec.TemplateSelector)
	out.VirtualMachineCloneSpec.SourceNode = in.VirtualMachineCloneSpec.SourceNode
	out.VirtualMachineCloneSpec.TemplateID = in.VirtualMachineCloneSpec.TemplateID
	out.VirtualMachineCloneSpec.Description = in.VirtualMachineCloneSpec.Description
	out.VirtualMachineCloneSpec.Format = (*TargetFileStorageFormat)(in.VirtualMachineCloneSpec.Format)
	out.VirtualMachineCloneSpec.Full = in.VirtualMachineCloneSpec.Full
	out.VirtualMachineCloneSpec.Pool = in.VirtualMachineCloneSpec.Pool
	out.VirtualMachineCloneSpec.SnapName = in.VirtualMachineCloneSpec.SnapName
	out.VirtualMachineCloneSpec.Storage = in.VirtualMachineCloneSpec.Storage
	out.VirtualMachineCloneSpec.Target = in.VirtualMachineCloneSpec.Target

	out.ProviderID = in.ProviderID
	out.VirtualMachineID = in.VirtualMachineID
	out.NumSockets = in.NumSockets
	out.NumCores = in.NumCores
	out.MemoryMiB = in.MemoryMiB
	out.Disks.BootVolume = (*DiskSize)(in.Disks.BootVolume)
	out.Network.Default.Bridge = in.Network.Default.Bridge
	out.Network.Default.Model = in.Network.Default.Model
	out.Network.Default.MTU = MTU(in.Network.Default.MTU)
	out.Network.Default.VLAN = in.Network.Default.VLAN
	out.Network.Default.DNSServers = in.Network.Default.DNSServers
	out.Network.Default.IPPoolConfig = IPPoolConfig(in.Network.Default.IPPoolConfig)

	// Convert AdditionalDevices slice
	if in.Network.AdditionalDevices != nil {
		out.Network.AdditionalDevices = make([]AdditionalNetworkDevice, len(in.Network.AdditionalDevices))
		for i := range in.Network.AdditionalDevices {
			if err := Convert_v1alpha2_AdditionalNetworkDevice_To_v1alpha1_AdditionalNetworkDevice(&in.Network.AdditionalDevices[i], &out.Network.AdditionalDevices[i], s); err != nil {
				return err
			}
		}
	}

	// Convert VRFs slice
	if in.Network.VirtualNetworkDevices.VRFs != nil {
		out.Network.VirtualNetworkDevices.VRFs = make([]VRFDevice, len(in.Network.VirtualNetworkDevices.VRFs))
		for i := range in.Network.VirtualNetworkDevices.VRFs {
			if err := Convert_v1alpha2_VRFDevice_To_v1alpha1_VRFDevice(&in.Network.VirtualNetworkDevices.VRFs[i], &out.Network.VirtualNetworkDevices.VRFs[i], s); err != nil {
				return err
			}
		}
	}

	out.VMIDRange = (*VMIDRange)(in.VMIDRange)
	out.Checks = (*ProxmoxMachineChecks)(in.Checks)
	out.MetadataSettings = (*MetadataSettings)(in.MetadataSettings)
	out.AllowedNodes = in.AllowedNodes
	out.Tags = in.Tags

	// instance is not supported in v1alpha1, so we ignore it

	return nil
}

func Convert_v1alpha2_ProxmoxClusterStatus_To_v1alpha1_ProxmoxClusterStatus(in *v1alpha2.ProxmoxClusterStatus, out *ProxmoxClusterStatus, s conversion_machinery.Scope) error {
	out.Ready = in.Ready
	out.InClusterIPPoolRef = in.InClusterIPPoolRef
	if err := Convert_v1alpha2_NodeLocations_To_v1alpha1_NodeLocations(in.NodeLocations, out.NodeLocations, s); err != nil {
		return err
	}
	out.FailureReason = in.FailureReason
	out.FailureMessage = in.FailureMessage
	out.Conditions = in.Conditions

	// Instances field is not supported in v1alpha1, so we ignore it
	return nil
}

func Convert_v1alpha2_NodeLocation_To_v1alpha1_NodeLocation(in *v1alpha2.NodeLocation, out *NodeLocation, s conversion_machinery.Scope) error {
	out.Machine = in.Machine
	out.Node = in.Node

	// The NodeLocation in v1alpha1 does not have an Instance field, so we can ignore it

	return nil
}
