package install_test

import (
	"regexp"
	"strings"
	"testing"

	"github.com/bwsalmon/silo/pkg/silo/install"
)

// silodRoleSpec is install.md "silod's service account" (SV-93), the
// permission list as the page writes it. A change to silod's role
// changes this and install.SilodPermissions together, and both show in
// the diff.
const silodRoleSpec = "" +
	"compute.instances.{create,delete,get,list,start,stop,reset,suspend,resume,attachDisk,detachDisk,setLabels,setMetadata,setTags,setMachineType,setDeletionProtection,getSerialPortOutput,setScheduling}\n" +
	"compute.disks.{create,delete,get,list,resize,update,setLabels,use,useReadOnly,createSnapshot}\n" +
	"compute.instanceTemplates.{create,delete,get,list,useReadOnly}\n" +
	"compute.regionInstanceTemplates.{create,delete,get,list,useReadOnly}\n" +
	"compute.images.{create,delete,get,list,getFromFamily,setLabels,deprecate,useReadOnly}\n" +
	"compute.snapshots.{create,delete,get,list,setLabels,useReadOnly}\n" +
	"compute.subnetworks.{create,delete,get,list,update,expandIpCidrRange,use}\n" +
	"compute.networks.{get,updatePolicy,use}\n" +
	"compute.firewalls.{create,delete,get,list,update}\n" +
	"compute.firewallPolicies.{get,update}\n" +
	"compute.{zoneOperations,regionOperations,globalOperations}.{get,list}\n" +
	"compute.{regions,zones,machineTypes,diskTypes,acceleratorTypes}.{get,list}\n" +
	"compute.projects.get\n" +
	"cloudquotas.quotas.{get,list}\n" +
	"cloudsql.instances.connect\n" +
	"cloudsql.instances.get\n" +
	"cloudsql.instances.login\n"

var braces = regexp.MustCompile(`\{([^}]*)\}`)

// expand expands the first {a,b} in s, and the rest recursively.
func expand(s string) []string {
	m := braces.FindStringSubmatchIndex(s)
	if m == nil {
		return []string{s}
	}
	var out []string
	for _, alt := range strings.Split(s[m[2]:m[3]], ",") {
		out = append(out, expand(s[:m[0]]+alt+s[m[1]:])...)
	}
	return out
}

func TestSilodPermissionsAreThePages(t *testing.T) {
	var want []string
	for _, line := range strings.Fields(silodRoleSpec) {
		want = append(want, expand(line)...)
	}
	got := install.SilodPermissions
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("install.SilodPermissions differs from install.md's list:\n got %d: %v\nwant %d: %v", len(got), got, len(want), want)
	}
	seen := map[string]bool{}
	for _, p := range got {
		if seen[p] {
			t.Errorf("%s is listed twice", p)
		}
		seen[p] = true
	}
	if len(got) != 91 {
		t.Errorf("silod's role has %d permissions; install.md's list has 91", len(got))
	}
}

// What silod must never hold (SV-93): no IAM, DNS, service-account or
// project-metadata permission, no Cloud SQL administration, and no
// setServiceAccount.
func TestSilodRoleHoldsNothingForbidden(t *testing.T) {
	forbidden := []string{"iam.", "resourcemanager.", "dns.", "compute.projects.setCommonInstanceMetadata",
		"compute.instances.setServiceAccount", "compute.instances.setIamPolicy", "compute.firewallPolicies.create",
		"compute.firewallPolicies.delete", "compute.networks.create", "compute.networks.delete", "compute.routers.",
		"compute.addresses.", "serviceusage.", "artifactregistry.", "logging."}
	cloudsql := map[string]bool{"cloudsql.instances.connect": true, "cloudsql.instances.get": true, "cloudsql.instances.login": true}
	for _, p := range install.SilodPermissions {
		for _, f := range forbidden {
			if strings.HasPrefix(p, f) {
				t.Errorf("silod's role holds %s", p)
			}
		}
		if strings.HasPrefix(p, "cloudsql.") && !cloudsql[p] {
			t.Errorf("silod's role holds %s: it may only connect to its database", p)
		}
		if strings.HasSuffix(p, ".setIamPolicy") || strings.HasSuffix(p, ".actAs") {
			t.Errorf("silod's role holds %s", p)
		}
	}
	for _, r := range install.SilodPredefinedRoles {
		if r != "roles/logging.logWriter" && r != "roles/artifactregistry.reader" {
			t.Errorf("silod is bound to %s", r)
		}
	}
}
