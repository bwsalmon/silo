package gce

// GCE quota metrics behind the classes of docs/silo/allocators.md
// "Classes" and "Global counts". Regional metrics are reported by
// regions.get (some only by Cloud Quotas); project metrics by
// projects.get.
const (
	MetricCPUS                  = "CPUS"
	MetricPreemptibleCPUS       = "PREEMPTIBLE_CPUS"
	MetricInstances             = "INSTANCES"
	MetricDisksTotalGB          = "DISKS_TOTAL_GB"
	MetricSSDTotalGB            = "SSD_TOTAL_GB"
	MetricLocalSSDTotalGB       = "LOCAL_SSD_TOTAL_GB"
	MetricPreemptibleLocalSSDGB = "PREEMPTIBLE_LOCAL_SSD_GB"
	MetricPDExtremeIOPS         = "PD_EXTREME_TOTAL_PROVISIONED_IOPS"
	MetricHDBTotalGB            = "HDB_TOTAL_GB"
	MetricHDBTotalIOPS          = "HDB_TOTAL_IOPS"
	MetricHDBTotalThroughput    = "HDB_TOTAL_THROUGHPUT"
	MetricHDXTotalGB            = "HDX_TOTAL_GB"
	MetricHDXTotalIOPS          = "HDX_TOTAL_IOPS"
	MetricHDTTotalGB            = "HDT_TOTAL_GB"
	MetricHDTTotalThroughput    = "HDT_TOTAL_THROUGHPUT"
	MetricHDMLTotalGB           = "HDML_TOTAL_GB"
	MetricHDMLTotalThroughput   = "HDML_TOTAL_THROUGHPUT"

	MetricInstanceTemplates = "INSTANCE_TEMPLATES"
	MetricGPUsAllRegions    = "GPUS_ALL_REGIONS"
	MetricSubnetworks       = "SUBNETWORKS"
	MetricFirewalls         = "FIREWALLS"
	MetricNetworks          = "NETWORKS"
	MetricImages            = "IMAGES"
	MetricSnapshots         = "SNAPSHOTS"
)

// PreemptiblePrefix turns a regional metric into its Spot variant:
// NVIDIA_L4_GPUS into PREEMPTIBLE_NVIDIA_L4_GPUS.
const PreemptiblePrefix = "PREEMPTIBLE_"

// CloudQuotasService is the Cloud Quotas service name of Compute.
const CloudQuotasService = "compute.googleapis.com"
