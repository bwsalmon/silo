package install

// SilodPermissions are the custom role silod's permissions, exactly as
// install.md "silod's service account" lists them (SV-93), one per line
// and in the page's order, so that a change to the role is a change to
// this list that a reviewer reads. role_test.go pins it to the page's
// text and to what silod must not hold.
//
// Each group is what one part of silod needs:
var SilodPermissions = []string{
	// The facade's instances, and the control VM's own metadata
	// (setMetadata hands certificates to gateway replicas).
	// compute.instances.setServiceAccount is missing on purpose: silod
	// creates instances with no service account, which needs no actAs.
	"compute.instances.create",
	"compute.instances.delete",
	"compute.instances.get",
	"compute.instances.list",
	"compute.instances.start",
	"compute.instances.stop",
	"compute.instances.reset",
	"compute.instances.suspend",
	"compute.instances.resume",
	"compute.instances.attachDisk",
	"compute.instances.detachDisk",
	"compute.instances.setLabels",
	"compute.instances.setMetadata",
	"compute.instances.setTags",
	"compute.instances.setMachineType",
	"compute.instances.setDeletionProtection",
	"compute.instances.getSerialPortOutput",
	"compute.instances.setScheduling",

	// The facade's disks.
	"compute.disks.create",
	"compute.disks.delete",
	"compute.disks.get",
	"compute.disks.list",
	"compute.disks.resize",
	"compute.disks.update",
	"compute.disks.setLabels",
	"compute.disks.use",
	"compute.disks.useReadOnly",
	"compute.disks.createSnapshot",

	// The facade's instance templates, global and regional.
	"compute.instanceTemplates.create",
	"compute.instanceTemplates.delete",
	"compute.instanceTemplates.get",
	"compute.instanceTemplates.list",
	"compute.instanceTemplates.useReadOnly",
	"compute.regionInstanceTemplates.create",
	"compute.regionInstanceTemplates.delete",
	"compute.regionInstanceTemplates.get",
	"compute.regionInstanceTemplates.list",
	"compute.regionInstanceTemplates.useReadOnly",

	// The facade's images and snapshots.
	"compute.images.create",
	"compute.images.delete",
	"compute.images.get",
	"compute.images.list",
	"compute.images.getFromFamily",
	"compute.images.setLabels",
	"compute.images.deprecate",
	"compute.images.useReadOnly",
	"compute.snapshots.create",
	"compute.snapshots.delete",
	"compute.snapshots.get",
	"compute.snapshots.list",
	"compute.snapshots.setLabels",
	"compute.snapshots.useReadOnly",

	// Networking: silos' subnets in the shared VPC, tenants' VPC
	// firewall rules, and the fence (silo-fence). No create or delete of
	// networks or firewall policies: install makes both.
	"compute.subnetworks.create",
	"compute.subnetworks.delete",
	"compute.subnetworks.get",
	"compute.subnetworks.list",
	"compute.subnetworks.update",
	"compute.subnetworks.expandIpCidrRange",
	"compute.subnetworks.use",
	"compute.networks.get",
	"compute.networks.updatePolicy",
	"compute.networks.use",
	"compute.firewalls.create",
	"compute.firewalls.delete",
	"compute.firewalls.get",
	"compute.firewalls.list",
	"compute.firewalls.update",
	"compute.firewallPolicies.get",
	"compute.firewallPolicies.update",

	// Operations, the catalog, and quotas: reconcile and capacity.
	"compute.zoneOperations.get",
	"compute.zoneOperations.list",
	"compute.regionOperations.get",
	"compute.regionOperations.list",
	"compute.globalOperations.get",
	"compute.globalOperations.list",
	"compute.regions.get",
	"compute.regions.list",
	"compute.zones.get",
	"compute.zones.list",
	"compute.machineTypes.get",
	"compute.machineTypes.list",
	"compute.diskTypes.get",
	"compute.diskTypes.list",
	"compute.acceleratorTypes.get",
	"compute.acceleratorTypes.list",
	"compute.projects.get",
	"cloudquotas.quotas.get",
	"cloudquotas.quotas.list",

	// Its database, through the Cloud SQL Go connector with IAM login.
	// Nothing that administers the instance.
	"cloudsql.instances.connect",
	"cloudsql.instances.get",
	"cloudsql.instances.login",
}

// SilodPredefinedRoles are the predefined roles silod's account is
// bound to beside its custom role: writing its logs, and pulling its
// own image from the repository install copies it into.
var SilodPredefinedRoles = []string{
	"roles/logging.logWriter",
	"roles/artifactregistry.reader",
}
