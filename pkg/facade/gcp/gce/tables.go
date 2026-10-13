package gce

import (
	"strings"

	"github.com/bwsalmon/silo/pkg/control"
	"github.com/bwsalmon/silo/pkg/facade"
)

// This file is the facade's allowlists, as data (gcp-backend.md,
// "Default-deny", SV-63): which resource types it knows, which methods
// it serves, which request fields each write may set, and which
// response fields each kind of object shows. The resource tasks that
// follow fill in their rows here; nothing else in the package lists a
// method or a field.

// knownResources are the resource types the facade covers: those with a
// method in pkg/control's table, which is the facade's whole method
// list.
var knownResources = func() map[string]bool {
	out := map[string]bool{}
	for m := range control.FacadeMethods() {
		res, _, _ := strings.Cut(m, ".")
		out[res] = true
	}
	return out
}()

// table is the method table: knownResources, refusedResources and the
// handlers.
var table = facade.Table[handler]{Resources: knownResources, Refused: refusedResources, Methods: handlers}

// refusedResources are GCE resource types the facade knows and
// refuses every method of (gcp-backend.md, "Refused"), with 403 naming
// the method rather than the 404 of a type it doesn't know at all.
var refusedResources = map[string]bool{
	"routes": true, "addresses": true, "globalAddresses": true, "routers": true,
	"vpnGateways": true, "vpnTunnels": true, "targetVpnGateways": true, "externalVpnGateways": true,
	"interconnects": true, "interconnectAttachments": true, "serviceAttachments": true,
	"forwardingRules": true, "globalForwardingRules": true, "backendServices": true, "regionBackendServices": true,
	"targetPools": true, "urlMaps": true, "healthChecks": true, "targetHttpProxies": true, "targetHttpsProxies": true,
	"targetTcpProxies": true, "targetSslProxies": true, "targetInstances": true,
	"machineImages": true, "storagePools": true, "storagePoolTypes": true, "nodeGroups": true, "nodeTemplates": true, "nodeTypes": true,
	"instanceGroupManagers": true, "regionInstanceGroupManagers": true, "instanceGroups": true, "regionInstanceGroups": true,
	"autoscalers": true, "regionAutoscalers": true, "reservations": true, "networkFirewallPolicies": true,
	"firewallPolicies": true, "networkEndpointGroups": true, "packetMirrorings": true, "networkAttachments": true,
	"regionDisks": true, "regionInstances": true,
}

// requestFields is each write method's request allowlist, by JSON path
// from the body's top: "name", "labels" (the whole map),
// "networkInterfaces[].subnetwork". A field outside it that is set to
// anything but its default is refused with 403, naming the field. A
// write method with no row allows an empty body only.
var requestFields = map[string][]string{
	// gcp-backend.md, "Subnetworks" (SV-71). privateIpGoogleAccess is
	// left out, so true is refused and false passes; network, region,
	// stackType and purpose are checked for their one allowed value.
	"subnetworks.insert": {"name", "description", "network", "region", "ipCidrRange",
		"secondaryIpRanges[].rangeName", "secondaryIpRanges[].ipCidrRange", "logConfig", "stackType", "purpose"},
	// A patch carries the subnet's fingerprint, and may repeat the fields
	// it can't change, unchanged.
	"subnetworks.patch": {"name", "description", "network", "region", "ipCidrRange", "fingerprint",
		"secondaryIpRanges[].rangeName", "secondaryIpRanges[].ipCidrRange", "logConfig", "stackType", "purpose"},
	"subnetworks.expandIpCidrRange": {"ipCidrRange"},

	// gcp-backend.md, "Firewall rules" (SV-72): no targetServiceAccounts
	// or sourceServiceAccounts.
	"firewalls.insert": firewallFields,
	"firewalls.patch":  firewallFields,
	"firewalls.update": firewallFields,

	// gcp-backend.md, "Instances" step 6 (SV-66): instanceFields.

	// serviceAccounts, accessConfigs, nodeAffinities or storagePool.
	// guestAccelerators (SV-67) and local SSD disks (SV-68) are checked
	// against the machine type, reservationAffinity for NO_RESERVATION,
	// and metadata for the refused keys.
	"instances.insert":         instanceFields,
	"instances.attachDisk":     attachedDiskFields,
	"instances.setLabels":      {"labels", "labelFingerprint"},
	"instances.setMetadata":    {"kind", "fingerprint", "items[].key", "items[].value"},
	"instances.setTags":        {"items", "fingerprint"},
	"instances.setMachineType": {"machineType"},

	// gcp-backend.md, "Disks" (SV-69): no storagePool. Provisioned
	// performance and accessMode are checked against the type.
	"disks.insert": {"name", "description", "zone", "sizeGb", "type", "sourceImage", "sourceSnapshot", "labels",
		"provisionedIops", "provisionedThroughput", "accessMode"},
	"disks.resize":         {"sizeGb"},
	"disks.update":         {"name", "provisionedIops", "provisionedThroughput", "accessMode"},
	"disks.setLabels":      {"labels", "labelFingerprint"},
	"disks.createSnapshot": {"name", "description", "labels", "storageLocations"},

	// Images: no rawDisk (gcp-backend.md, "Covered").
	"images.insert": {"name", "description", "family", "labels", "sourceDisk", "sourceImage", "sourceSnapshot",
		"storageLocations", "guestOsFeatures", "architecture"},
	"images.setLabels":    {"labels", "labelFingerprint"},
	"images.deprecate":    {"state", "replacement", "deprecated", "obsolete", "deleted"},
	"snapshots.setLabels": {"labels", "labelFingerprint"},

	// gcp-backend.md, "Instance templates" (SV-70): an instance's fields
	// under properties, and no sourceInstance or sourceInstanceParams.
	"instanceTemplates.insert":       templateFields,
	"regionInstanceTemplates.insert": templateFields,
}

// instanceFields are what instances.insert may set: no canIpForward,
// serviceAccounts, accessConfigs, nodeAffinities or storagePool.
// guestAccelerators (SV-67) and local SSD disks (SV-68) are checked
// against the machine type, reservationAffinity for NO_RESERVATION, and
// metadata for the refused keys.
var instanceFields = append([]string{"name", "description", "zone", "machineType", "labels", "tags.items",
	"metadata.items[].key", "metadata.items[].value",
	"networkInterfaces[].network", "networkInterfaces[].subnetwork", "networkInterfaces[].networkIP",
	"networkInterfaces[].aliasIpRanges[].ipCidrRange", "networkInterfaces[].aliasIpRanges[].subnetworkRangeName",
	"networkInterfaces[].nicType", "networkInterfaces[].stackType",
	"scheduling.provisioningModel", "scheduling.preemptible", "scheduling.onHostMaintenance",
	"scheduling.automaticRestart", "scheduling.instanceTerminationAction",
	"advancedMachineFeatures.enableNestedVirtualization", "shieldedInstanceConfig",
	"minCpuPlatform", "deletionProtection", "reservationAffinity.consumeReservationType",
	"guestAccelerators[].acceleratorType", "guestAccelerators[].acceleratorCount"},
	prefixed("disks[].", attachedDiskFields)...)

// templateFields are what an instance template may set: its name,
// description and region, and as its properties what an instance may,
// but for the instance's own name and zone and deletionProtection,
// which a template's properties don't have.
var templateFields = append([]string{"name", "description", "region"},
	prefixed("properties.", without(instanceFields, "name", "zone", "deletionProtection"))...)

func without(paths []string, drop ...string) []string {
	var out []string
	for _, p := range paths {
		if !contains(drop, p) {
			out = append(out, p)
		}
	}
	return out
}

// attachedDiskFields are what a disk attached to an instance may set: a
// persistent disk, new or the silo's own, or a local SSD partition.
var attachedDiskFields = []string{"boot", "autoDelete", "deviceName", "mode", "type", "source", "interface",
	"initializeParams.diskName", "initializeParams.sourceImage", "initializeParams.sourceSnapshot",
	"initializeParams.diskSizeGb", "initializeParams.diskType", "initializeParams.labels", "initializeParams.description",
	"initializeParams.provisionedIops", "initializeParams.provisionedThroughput"}

func prefixed(prefix string, paths []string) []string {
	out := make([]string, len(paths))
	for i, p := range paths {
		out[i] = prefix + p
	}
	return out
}

var firewallFields = []string{"name", "description", "network", "priority", "direction", "sourceRanges",
	"destinationRanges", "sourceTags", "targetTags", "allowed[].IPProtocol", "allowed[].ports",
	"denied[].IPProtocol", "denied[].ports", "disabled", "logConfig"}

// responseFields is each kind's response allowlist, by JSON path. The
// facade builds a response from these fields only, after rewriting
// them; a path naming an object or a list of objects keeps all of it.
var responseFields = map[string][]string{
	"compute#region": {"kind", "id", "creationTimestamp", "name", "description", "status", "zones", "quotas",
		"deprecated", "selfLink", "supportsPzs"},
	"compute#zone": {"kind", "id", "creationTimestamp", "name", "description", "status", "region",
		"availableCpuPlatforms", "deprecated", "selfLink", "supportsPzs"},
	"compute#machineType": {"kind", "id", "creationTimestamp", "name", "description", "guestCpus", "memoryMb",
		"imageSpaceGb", "maximumPersistentDisks", "maximumPersistentDisksSizeGb", "zone", "selfLink",
		"isSharedCpu", "accelerators", "bundledLocalSsds", "architecture", "deprecated"},
	"compute#diskType": {"kind", "id", "creationTimestamp", "name", "description", "validDiskSize", "zone",
		"region", "selfLink", "defaultDiskSizeGb", "deprecated"},
	"compute#acceleratorType": {"kind", "id", "creationTimestamp", "name", "description", "zone", "selfLink",
		"maximumCardsPerInstance", "deprecated"},
	"compute#image": {"kind", "id", "creationTimestamp", "name", "description", "sourceType", "status",
		"archiveSizeBytes", "diskSizeGb", "sourceDisk", "sourceDiskId", "sourceImage", "sourceImageId",
		"sourceSnapshot", "sourceSnapshotId", "licenses", "licenseCodes", "family", "selfLink", "labels",
		"labelFingerprint", "guestOsFeatures", "storageLocations", "architecture", "deprecated"},
	"compute#operation": {"kind", "id", "creationTimestamp", "name", "description", "zone", "region",
		"operationType", "targetLink", "targetId", "status", "statusMessage", "user", "progress",
		"insertTime", "startTime", "endTime", "error", "warnings", "httpErrorStatusCode",
		"httpErrorMessage", "selfLink"},
	"compute#network": {"kind", "id", "creationTimestamp", "name", "description", "autoCreateSubnetworks",
		"subnetworks", "routingConfig", "selfLink", "siloAddressBlocks", "mtu"},
	"compute#subnetwork": {"kind", "id", "creationTimestamp", "name", "description", "network", "ipCidrRange",
		"gatewayAddress", "region", "selfLink", "secondaryIpRanges", "fingerprint", "logConfig", "stackType",
		"purpose", "privateIpGoogleAccess"},
	"compute#firewall": {"kind", "id", "creationTimestamp", "name", "description", "network", "priority",
		"sourceRanges", "destinationRanges", "sourceTags", "targetTags", "allowed", "denied", "direction",
		"logConfig", "disabled", "selfLink"},
	// No serviceAccounts: an instance never has any.
	"compute#instance": {"kind", "id", "creationTimestamp", "name", "description", "tags", "machineType", "status",
		"statusMessage", "zone", "canIpForward", "networkInterfaces", "disks", "metadata", "selfLink", "scheduling",
		"cpuPlatform", "labels", "labelFingerprint", "minCpuPlatform", "guestAccelerators", "startRestricted",
		"deletionProtection", "shieldedInstanceConfig", "advancedMachineFeatures", "fingerprint",
		"lastStartTimestamp", "lastStopTimestamp", "lastSuspendedTimestamp", "reservationAffinity"},
	// properties is shown whole: the facade wrote all of it, and its
	// labels, tags and links are mapped back like an instance's.
	"compute#instanceTemplate": {"kind", "id", "creationTimestamp", "name", "description", "properties", "selfLink", "region"},
	"compute#disk": {"kind", "id", "creationTimestamp", "name", "description", "sizeGb", "zone", "status",
		"sourceSnapshot", "sourceSnapshotId", "sourceImage", "sourceImageId", "type", "licenses", "guestOsFeatures",
		"lastAttachTimestamp", "lastDetachTimestamp", "users", "labels", "labelFingerprint", "physicalBlockSizeBytes",
		"selfLink", "provisionedIops", "provisionedThroughput", "accessMode", "architecture"},
	"compute#snapshot": {"kind", "id", "creationTimestamp", "name", "description", "status", "diskSizeGb",
		"sourceDisk", "sourceDiskId", "storageBytes", "storageBytesStatus", "licenses", "labels",
		"labelFingerprint", "storageLocations", "autoCreated", "architecture", "selfLink"},
	"compute#serialPortOutput": {"kind", "contents", "start", "next", "selfLink"},
	"compute#project": {"kind", "id", "creationTimestamp", "name", "description", "commonInstanceMetadata",
		"quotas", "selfLink", "defaultNetworkTier"},
}
