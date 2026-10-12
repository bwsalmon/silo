package gce

import (
	"context"
	"net/netip"
	"strconv"
	"strings"

	"github.com/bwsalmon/silo/pkg/agents"
	gceapi "github.com/bwsalmon/silo/pkg/backend/gcp/gce"
	"github.com/bwsalmon/silo/pkg/ledger"
	"github.com/bwsalmon/silo/pkg/store"
	compute "google.golang.org/api/compute/v1"
)

// Instances (gcp-backend.md, "Instances", SV-66). A tenant's instance
// is the real <id>-<name>, its tags prefixed and the silo's own <id>
// tag added, labelled silo=<id> and silo-managed=true, with
// block-project-ssh-keys set, no service accounts and no reservation.
// It charges the price table's instance rows for its provisioning
// model, the running ones (vCPU and memory) only while it isn't
// TERMINATED, as GCE's own usage clock runs (call 6); each new disk it
// creates is charged to the disk itself, so that it outlives the
// instance or goes with it.

func init() {
	for m, h := range map[string]handler{
		"instances.insert":                (*Server).insertInstance,
		"instances.get":                   (*Server).getInstance,
		"instances.list":                  (*Server).listInstances,
		"instances.aggregatedList":        (*Server).aggregatedInstances,
		"instances.delete":                (*Server).deleteInstance,
		"instances.start":                 (*Server).startInstance,
		"instances.stop":                  (*Server).stopInstance,
		"instances.reset":                 (*Server).simpleInstanceOp,
		"instances.suspend":               (*Server).simpleInstanceOp,
		"instances.resume":                (*Server).simpleInstanceOp,
		"instances.attachDisk":            (*Server).attachDisk,
		"instances.detachDisk":            (*Server).detachDisk,
		"instances.setLabels":             (*Server).setInstanceLabels,
		"instances.setMetadata":           (*Server).setInstanceMetadata,
		"instances.setTags":               (*Server).setInstanceTags,
		"instances.setMachineType":        (*Server).setMachineType,
		"instances.setDeletionProtection": (*Server).setDeletionProtection,
		"instances.getSerialPortOutput":   (*Server).getSerialPortOutput,
	} {
		handlers[m] = h
	}
	pricers["instances.insert"] = billPricer(func(s *Server, q *request) (bill, error) {
		p, err := s.planInstance(q)
		return p.bill, err
	})
	pricers["instances.attachDisk"] = billPricer(func(s *Server, q *request) (bill, error) {
		p, err := s.planAttach(q)
		return p.bill, err
	})
	pricers["instances.start"] = billPricer(func(s *Server, q *request) (bill, error) {
		b, _, err := s.planStart(q)
		return b, err
	})
	pricers["instances.setMachineType"] = func(s *Server, q *request, tx store.Tx) ([]ledger.Amount, error) {
		p, err := s.planMachineType(q)
		if err != nil {
			return nil, err
		}
		up, _, err := p.diff(q.ctx(), tx)
		return up, err
	}
}

// refusedMetadata are the instance metadata keys the facade refuses:
// OS Login (gcp-backend.md, "Refused").
var refusedMetadata = map[string]bool{"enable-oslogin": true, "enable-oslogin-2fa": true, "enable-windows-ssh": true}

// blockProjectKeys is the metadata key the facade sets true on every
// instance.
const blockProjectKeys = "block-project-ssh-keys"

// NamePrefixKey is the metadata key the facade sets on every instance
// to the prefix the names in its silo must carry, such as k7f3qa-
// (gcp-backend.md, "Naming", SV-76), so that a guest can learn it
// from the metadata server.
const NamePrefixKey = "silo-name-prefix"

// checkMetadata refuses the OS Login keys, block-project-ssh-keys set
// to anything but true, and silo-name-prefix set to anything but
// prefix.
func checkMetadata(md *compute.Metadata, prefix string) error {
	if md == nil {
		return nil
	}
	for _, it := range md.Items {
		if it == nil {
			continue
		}
		k := strings.ToLower(it.Key)
		if refusedMetadata[k] {
			return forbidden("silo: metadata key %s is not allowed", it.Key)
		}
		if k == blockProjectKeys && (it.Value == nil || !strings.EqualFold(*it.Value, "true")) {
			return forbidden("silo: metadata key %s may only be true", it.Key)
		}
		if k == NamePrefixKey && (it.Value == nil || *it.Value != prefix) {
			return forbidden("silo: metadata key %s is the facade's own; it may only be %s", it.Key, prefix)
		}
	}
	return nil
}

// realMetadata is md with block-project-ssh-keys: true and
// silo-name-prefix: prefix.
func realMetadata(md *compute.Metadata, prefix string) *compute.Metadata {
	out := &compute.Metadata{}
	if md != nil {
		out.Fingerprint = md.Fingerprint
		for _, it := range md.Items {
			if it != nil && strings.ToLower(it.Key) != blockProjectKeys && strings.ToLower(it.Key) != NamePrefixKey {
				out.Items = append(out.Items, it)
			}
		}
	}
	t := "true"
	out.Items = append(out.Items, &compute.MetadataItems{Key: blockProjectKeys, Value: &t},
		&compute.MetadataItems{Key: NamePrefixKey, Value: &prefix})
	return out
}

// realTags are the tenant's tags, which must carry the silo's prefix
// as names do, with the silo's own <id> tag added. field is the
// request field they are in.
func (q *request) realTags(tags *compute.Tags, field string) (*compute.Tags, error) {
	out := &compute.Tags{Items: []string{q.target.ID}}
	if tags != nil {
		out.Fingerprint = tags.Fingerprint
		for i, t := range tags.Items {
			if err := q.n.checkField(field+"["+itoa(i)+"]", t); err != nil {
				return nil, err
			}
			out.Items = append(out.Items, t)
		}
	}
	return out, nil
}

// checkReservation refuses any reservationAffinity but NO_RESERVATION.
func checkReservation(ra *compute.ReservationAffinity) error {
	if ra != nil && ra.ConsumeReservationType != "" && ra.ConsumeReservationType != "NO_RESERVATION" {
		return forbidden("silo: field reservationAffinity.consumeReservationType %s is not allowed: reservations are project-wide, so only NO_RESERVATION is", ra.ConsumeReservationType)
	}
	return nil
}

// machineType resolves an instance's machine type in zone.
func (s *Server) machineType(ctx context.Context, zone, ref string) (*compute.MachineType, error) {
	if ref == "" {
		return nil, invalid("Invalid value for field 'resource.machineType': ''. A machine type is required")
	}
	if r := gceapi.ParseRef(ref); r.Zone != "" && r.Zone != zone {
		return nil, invalid("Invalid value for field 'resource.machineType': '%s'. Machine type must be in zone %s", ref, zone)
	}
	return s.gce.GetMachineType(ctx, s.opts.HostProject, zone, gceapi.LastSegment(ref))
}

func (s *Server) machineTypeLink(zone, name string) string {
	return gceapi.SelfLinkBase + "projects/" + s.opts.HostProject + "/zones/" + zone + "/machineTypes/" + name
}

// nics checks an instance's network interfaces (step 3): network
// default or omitted; subnetwork one of the silo's in the zone's region
// (the only one, if omitted); networkIP in it; every alias range inside
// its ranges. Access configs are refused by the allowlist. A template's
// interface (template set) may name no subnetwork, which the instance
// made from it then does; it can then set no address or alias range.
func (s *Server) nics(q *request, zone string, in []*compute.NetworkInterface, template bool) ([]*compute.NetworkInterface, error) {
	region := gceapi.ZoneRegion(zone)
	if len(in) == 0 {
		return nil, invalid("Invalid value for field 'resource.networkInterfaces': silo: an instance needs a network interface on one of the silo's subnetworks")
	}
	var out []*compute.NetworkInterface
	for i, nic := range in {
		field := "networkInterfaces[" + itoa(i) + "]"
		if nic == nil {
			return nil, invalid("Invalid value for field 'resource.%s'", field)
		}
		if nic.Network != "" {
			if _, err := q.n.inLink(nic.Network, "networks"); err != nil {
				return nil, err
			}
		}
		var sn *compute.Subnetwork
		if nic.Subnetwork == "" && template {
			if nic.NetworkIP != "" || len(nic.AliasIpRanges) > 0 {
				return nil, invalid("Invalid value for field 'resource.%s.subnetwork': ''. silo: a template's interface names its subnetwork to set networkIP or aliasIpRanges", field)
			}
			out = append(out, &compute.NetworkInterface{Network: s.networkLink(), NicType: nic.NicType, StackType: nic.StackType})
			continue
		}
		if nic.Subnetwork == "" {
			l, err := s.ownSubnets(q, region)
			if err != nil {
				return nil, err
			}
			if len(l) != 1 {
				return nil, invalid("Invalid value for field 'resource.%s.subnetwork': ''. silo: name one of the silo's subnetworks in %s", field, region)
			}
			sn = l[0]
		} else {
			r, err := q.ref(nic.Subnetwork, "subnetworks", "resource."+field+".subnetwork")
			if err != nil {
				return nil, err
			}
			if r.Region != "" && r.Region != region {
				return nil, invalid("Invalid value for field 'resource.%s.subnetwork': '%s'. The subnetwork must be in region %s", field, nic.Subnetwork, region)
			}
			nf := notFound("projects/" + q.call.Project + "/regions/" + region + "/subnetworks/" + r.Name)
			sn, err = s.gce.GetSubnetwork(q.ctx(), s.opts.HostProject, region, r.Name)
			if err != nil {
				if gceapi.IsNotFound(err) {
					return nil, nf
				}
				return nil, err
			}
			if !q.ownsSubnet(sn) {
				return nil, nf
			}
		}
		primary, err := netip.ParsePrefix(sn.IpCidrRange)
		if err != nil {
			return nil, err
		}
		if nic.NetworkIP != "" {
			a, err := netip.ParseAddr(nic.NetworkIP)
			if err != nil || !primary.Contains(a) {
				return nil, invalid("Invalid value for field 'resource.%s.networkIP': '%s'. silo: the address must be in the subnetwork's range %s", field, nic.NetworkIP, primary)
			}
		}
		for j, ar := range nic.AliasIpRanges {
			af := field + ".aliasIpRanges[" + itoa(j) + "]"
			if ar == nil {
				continue
			}
			within := primary
			if ar.SubnetworkRangeName != "" {
				found := false
				for _, sr := range sn.SecondaryIpRanges {
					if sr.RangeName == ar.SubnetworkRangeName {
						within, err = netip.ParsePrefix(sr.IpCidrRange)
						found = err == nil
					}
				}
				if !found {
					return nil, invalid("Invalid value for field 'resource.%s.subnetworkRangeName': '%s'. The subnetwork has no such secondary range", af, ar.SubnetworkRangeName)
				}
			}
			if err := aliasInside(ar.IpCidrRange, within); err != nil {
				return nil, invalid("Invalid value for field 'resource.%s.ipCidrRange': '%s'. silo: %v", af, ar.IpCidrRange, err)
			}
		}
		out = append(out, &compute.NetworkInterface{Network: s.networkLink(), Subnetwork: sn.SelfLink,
			NetworkIP: nic.NetworkIP, AliasIpRanges: nic.AliasIpRanges, NicType: nic.NicType, StackType: nic.StackType})
	}
	return out, nil
}

type errText string

func (e errText) Error() string { return string(e) }

// aliasInside checks an alias range, a CIDR, an address or a bare /n
// size, fits inside within.
func aliasInside(s string, within netip.Prefix) error {
	if bits, ok := strings.CutPrefix(s, "/"); ok {
		n, err := strconv.Atoi(bits)
		if err != nil || n < within.Bits() || n > 32 {
			return errText("the range must fit inside " + within.String())
		}
		return nil
	}
	p, err := netip.ParsePrefix(s)
	if err != nil {
		a, aerr := netip.ParseAddr(s)
		if aerr != nil {
			return errText("not an IPv4 range")
		}
		p = netip.PrefixFrom(a, 32)
	}
	if p.Bits() < within.Bits() || !within.Contains(p.Masked().Addr()) {
		return errText("the range must be inside " + within.String())
	}
	return nil
}

// disksPlan is an instance's disks, checked (step 4) and rewritten:
// each new one priced to its own object.
type disksPlan struct {
	disks []*compute.AttachedDisk
	bill  bill
	fresh []string // the new disks' objects
	// typed are the persistent disks with their types, and scratch the
	// local SSD partitions' fields, for the machine-type checks (step 5).
	typed   []typedDisk
	scratch []string
}

// localSSD is the disk type of a local SSD partition.
const localSSD = "local-ssd"

// planDisks checks disks (step 4): each a new disk from an allowed image
// or snapshot, or blank, or one of the silo's own disks in the same
// zone, or, where scratch allows, a local SSD partition. A new disk the
// tenant didn't name is named as GCE would name it: the instance's name
// for the boot disk, and <instance>-<index> for the others, index
// counting from first.
func (s *Server) planDisks(q *request, zone, instance string, in []*compute.AttachedDisk, field string, first int, scratch bool) (disksPlan, error) {
	var p disksPlan
	seen := map[string]bool{}
	for i, ad := range in {
		f := field + "[" + itoa(i) + "]"
		if field == "" {
			f = ""
		}
		at := func(sub string) string {
			if f == "" {
				return sub
			}
			return f + "." + sub
		}
		if ad == nil {
			return p, invalid("Invalid value for field 'resource.%s'", f)
		}
		if ad.Type == "SCRATCH" {
			if !scratch {
				return p, invalid("Invalid value for field 'resource.%s': 'SCRATCH'. Local SSD is attached when the instance is created", at("type"))
			}
			real, err := s.scratchDisk(ad, zone, at)
			if err != nil {
				return p, err
			}
			p.disks = append(p.disks, real)
			p.scratch = append(p.scratch, fieldOr(f, "disk"))
			continue
		}
		if ad.Type != "" && ad.Type != "PERSISTENT" {
			return p, forbidden("silo: field %s %s is not allowed", at("type"), ad.Type)
		}
		real := *ad
		switch {
		case ad.Source != "" && ad.InitializeParams != nil:
			return p, invalid("Invalid value for field 'resource.%s': a disk takes a source or initializeParams, not both", f)
		case ad.Source != "":
			d, err := s.sourceDiskIn(q, ad.Source, zone, "resource."+at("source"))
			if err != nil {
				return p, err
			}
			real.Source = d.SelfLink
			p.typed = append(p.typed, typedDisk{field: f, typ: diskTypeOf(d.Type)})
		case ad.InitializeParams != nil:
			ip := *ad.InitializeParams
			name := ip.DiskName
			if name == "" {
				name = instance
				if !ad.Boot {
					name = instance + "-" + itoa(first+i)
				}
				if err := q.n.checkName(name); err != nil {
					return p, invalid("Invalid value for field 'resource.%s': '%s'. silo: name the disk with diskName; its default name is too long", at("initializeParams.diskName"), name)
				}
			} else if err := q.n.checkField("resource."+at("initializeParams.diskName"), name); err != nil {
				return p, err
			}
			typ, err := diskTypeIn(ip.DiskType, zone, "resource."+at("initializeParams.diskType"))
			if err != nil {
				return p, err
			}
			img, snap, err := s.newDiskSource(q, ip.SourceImage, ip.SourceSnapshot, "resource."+at("initializeParams."))
			if err != nil {
				return p, err
			}
			size, err := s.diskSize(q.ctx(), zone, typ, ip.DiskSizeGb, img, snap)
			if err != nil {
				return p, err
			}
			iops, throughput, err := s.performance(typ, size, ip.ProvisionedIops, ip.ProvisionedThroughput, "resource."+at("initializeParams."))
			if err != nil {
				return p, err
			}
			realName := name
			if seen[realName] {
				return p, errorf(409, ReasonAlreadyExists, "The resource '%s' already exists", q.zonalPath(zone, "disks", name))
			}
			seen[realName] = true
			ip.DiskName, ip.DiskType, ip.DiskSizeGb = realName, s.diskTypeLink(zone, typ), size
			ip.Labels = q.siloLabels(ip.Labels)
			if img != nil {
				ip.SourceImage = img.SelfLink
			}
			if snap != nil {
				ip.SourceSnapshot = snap.SelfLink
			}
			real.InitializeParams = &ip
			p.bill = append(p.bill, priceDisk(zone, realName, typ, size, iops, throughput, at("initializeParams.diskType")))
			p.fresh = append(p.fresh, diskObject(zone, realName))
			p.typed = append(p.typed, typedDisk{field: f, typ: typ})
		default:
			return p, invalid("Invalid value for field 'resource.%s': a disk needs a source or initializeParams", f)
		}
		p.disks = append(p.disks, &real)
	}
	return p, nil
}

// scratchDisk checks a local SSD partition (gcp-backend.md, "Local
// SSD"): type SCRATCH, initializeParams.diskType local-ssd, interface
// NVME or SCSI, and nothing else of a persistent disk's.
func (s *Server) scratchDisk(ad *compute.AttachedDisk, zone string, at func(string) string) (*compute.AttachedDisk, error) {
	if ad.InitializeParams == nil || diskTypeOf(ad.InitializeParams.DiskType) != localSSD {
		return nil, invalid("Invalid value for field 'resource.%s': a SCRATCH disk must be of type %s", at("initializeParams.diskType"), localSSD)
	}
	if _, err := diskTypeIn(ad.InitializeParams.DiskType, zone, "resource."+at("initializeParams.diskType")); err != nil {
		return nil, err
	}
	ip := *ad.InitializeParams
	switch {
	case ad.Boot:
		return nil, invalid("Invalid value for field 'resource.%s': a local SSD can't be the boot disk", at("boot"))
	case ad.Source != "" || ip.SourceImage != "" || ip.SourceSnapshot != "" || ip.DiskName != "":
		return nil, invalid("Invalid value for field 'resource.%s': a local SSD has no name or source", at("initializeParams"))
	case ad.Interface != "" && ad.Interface != "NVME" && ad.Interface != "SCSI":
		return nil, invalid("Invalid value for field 'resource.%s': '%s'. A local SSD's interface is NVME or SCSI", at("interface"), ad.Interface)
	case ip.DiskSizeGb != 0 && ip.DiskSizeGb != s.opts.Compat.LocalSSDPartitionGB:
		return nil, invalid("Invalid value for field 'resource.%s': '%d'. A local SSD partition is %d GB", at("initializeParams.diskSizeGb"), ip.DiskSizeGb, s.opts.Compat.LocalSSDPartitionGB)
	}
	ip.DiskType = s.diskTypeLink(zone, localSSD)
	real := *ad
	real.InitializeParams = &ip
	return &real, nil
}

// planGPUs checks an instance's guestAccelerators in zone (gcp-backend.md,
// "GPUs"): each type offered in the zone, whose region is allowed, in a
// positive count no more than the type's maximum. It returns them by
// type, and rewritten to the real links.
func (s *Server) planGPUs(q *request, zone string, in []*compute.AcceleratorConfig) ([]attachedGPU, []*compute.AcceleratorConfig, error) {
	var gpus []attachedGPU
	var real []*compute.AcceleratorConfig
	for i, a := range in {
		field := "guestAccelerators[" + itoa(i) + "]"
		if a == nil || a.AcceleratorType == "" {
			return nil, nil, invalid("Invalid value for field 'resource.%s.acceleratorType': ''. An accelerator type is required", field)
		}
		r, err := q.ref(a.AcceleratorType, "acceleratorTypes", "resource."+field+".acceleratorType")
		if err != nil {
			return nil, nil, err
		}
		if r.Zone != "" && r.Zone != zone {
			if !q.regions[gceapi.ZoneRegion(r.Zone)] {
				return nil, nil, regionNotAllowed(q.target.Path, "zone "+r.Zone)
			}
			return nil, nil, invalid("Invalid value for field 'resource.%s.acceleratorType': '%s'. The accelerator type must be in zone %s", field, a.AcceleratorType, zone)
		}
		at, err := s.gce.GetAcceleratorType(q.ctx(), s.opts.HostProject, zone, r.Name)
		if err != nil {
			if gceapi.IsNotFound(err) {
				return nil, nil, invalid("Invalid value for field 'resource.%s.acceleratorType': '%s'. Accelerator type %s is not available in zone %s", field, a.AcceleratorType, r.Name, zone)
			}
			return nil, nil, err
		}
		if a.AcceleratorCount <= 0 || (at.MaximumCardsPerInstance > 0 && a.AcceleratorCount > at.MaximumCardsPerInstance) {
			return nil, nil, invalid("Invalid value for field 'resource.%s.acceleratorCount': '%d'. %s takes 1 to %d per instance", field, a.AcceleratorCount, r.Name, at.MaximumCardsPerInstance)
		}
		gpus = append(gpus, attachedGPU{field: field, typ: r.Name, count: a.AcceleratorCount})
		real = append(real, &compute.AcceleratorConfig{AcceleratorType: s.acceleratorTypeLink(zone, r.Name), AcceleratorCount: a.AcceleratorCount})
	}
	return gpus, real, nil
}

func (s *Server) acceleratorTypeLink(zone, name string) string {
	return gceapi.SelfLinkBase + "projects/" + s.opts.HostProject + "/zones/" + zone + "/acceleratorTypes/" + name
}

// gpuExtras are attached GPUs as extras.
func gpuExtras(gpus []attachedGPU, scratch int) extras {
	x := extras{gpus: map[string]int64{}, scratch: int64(scratch)}
	for _, g := range gpus {
		x.gpus[g.typ] += g.count
	}
	return x
}

// instancePlan is an instances.insert, checked and priced.
type instancePlan struct {
	bill  bill
	fresh []string
	real  *compute.Instance
	// template is the real template's link, for an instance made from
	// sourceInstanceTemplate.
	template string
}

// planInstance checks and prices an instances.insert. With
// sourceInstanceTemplate, what it checks is the effective instance:
// the body merged over the template's properties (gcp-backend.md,
// "Instance templates").
func (s *Server) planInstance(q *request) (instancePlan, error) {
	body := q.body
	var tpl *compute.InstanceTemplate
	if src := q.r.URL.Query().Get("sourceInstanceTemplate"); src != "" {
		var err error
		if tpl, err = s.sourceTemplate(q, src, q.call.Zone); err != nil {
			return instancePlan{}, err
		}
		if body, err = q.effective(tpl, body); err != nil {
			return instancePlan{}, err
		}
	}
	var in compute.Instance
	if err := decode(body, &in); err != nil {
		return instancePlan{}, err
	}
	if err := q.n.checkName(in.Name); err != nil {
		return instancePlan{}, err
	}
	zone := q.call.Zone
	// 1. The zone's region is allowed: checkScope has seen to it.
	if in.Zone != "" && gceapi.LastSegment(in.Zone) != zone {
		return instancePlan{}, invalid("Invalid value for field 'resource.zone': '%s'. It must be %s", in.Zone, zone)
	}
	p, err := s.planInstanceIn(q, zone, &in, false)
	if err != nil {
		return instancePlan{}, err
	}
	if tpl != nil {
		p.template = tpl.SelfLink
	}
	return p, nil
}

// planInstanceIn is steps 2 to 7 of instances.insert for in, an
// instance in zone. For a template's properties (template set), an
// interface may leave its subnetwork to the instance.
func (s *Server) planInstanceIn(q *request, zone string, in *compute.Instance, template bool) (instancePlan, error) {
	// 2. Price it: the machine type, by provisioning model, with its
	// GPUs and local SSD, and its new disks, below.
	mt, err := s.machineType(q.ctx(), zone, in.MachineType)
	if err != nil {
		return instancePlan{}, err
	}
	real := in.Name
	gpus, realGPUs, err := s.planGPUs(q, zone, in.GuestAccelerators)
	if err != nil {
		return instancePlan{}, err
	}
	// 3. Network interfaces.
	nics, err := s.nics(q, zone, in.NetworkInterfaces, template)
	if err != nil {
		return instancePlan{}, err
	}
	// 4. Disk sources, and the new disks' prices.
	dp, err := s.planDisks(q, zone, in.Name, in.Disks, "disks", 0, true)
	if err != nil {
		return instancePlan{}, err
	}
	// 5. GPUs, local SSD and the disk types against the machine type.
	if err := s.checkMachine(mt, gpus, in.Scheduling, dp); err != nil {
		return instancePlan{}, err
	}
	b := bill{s.priceInstance(zone, real, mt, gpuExtras(gpus, len(dp.scratch)), instanceVariant(in.Scheduling), false)}
	// 6. The allowlist has refused the fields outside it; these are the
	// values it can't judge.
	if err := checkMetadata(in.Metadata, q.n.prefix()); err != nil {
		return instancePlan{}, err
	}
	tags, err := q.realTags(in.Tags, "resource.tags.items")
	if err != nil {
		return instancePlan{}, err
	}
	if err := checkReservation(in.ReservationAffinity); err != nil {
		return instancePlan{}, err
	}
	// 7. The real instance.
	inst := &compute.Instance{
		Name: real, Description: in.Description, MachineType: s.machineTypeLink(zone, mt.Name),
		Labels: q.siloLabels(in.Labels), Tags: tags, Metadata: realMetadata(in.Metadata, q.n.prefix()),
		NetworkInterfaces: nics, Disks: dp.disks, Scheduling: in.Scheduling, GuestAccelerators: realGPUs,
		ServiceAccounts:         []*compute.ServiceAccount{},
		ReservationAffinity:     &compute.ReservationAffinity{ConsumeReservationType: "NO_RESERVATION"},
		AdvancedMachineFeatures: in.AdvancedMachineFeatures, ShieldedInstanceConfig: in.ShieldedInstanceConfig,
		MinCpuPlatform: in.MinCpuPlatform, DeletionProtection: in.DeletionProtection,
	}
	if a := agentCall(q.ctx()); a != "" {
		// An agent's tool VM, made by silod (agents.md, "Creation").
		inst.Labels[agents.LabelAgent] = a
	}
	return instancePlan{bill: append(b, dp.bill...), fresh: append([]string{instanceObject(zone, real)}, dp.fresh...), real: inst}, nil
}

// checkMachine checks an instance's GPUs, local SSD and disks against
// its machine type (step 5).
func (s *Server) checkMachine(mt *compute.MachineType, gpus []attachedGPU, sc *compute.Scheduling, dp disksPlan) error {
	c := s.opts.Compat
	if err := c.checkGPUs(mt, gpus, sc); err != nil {
		return err
	}
	if err := c.checkLocalSSD(mt, dp.scratch); err != nil {
		return err
	}
	return c.checkDisks(mt, dp.typed)
}

func (s *Server) insertInstance(q *request) (any, error) {
	p, err := s.planInstance(q)
	if err != nil {
		return nil, s.refuse(q, err)
	}
	// 2. Charge, in the transaction that checks, before GCE is called.
	lines, err := s.charge(q, p.bill, p.fresh...)
	if err != nil {
		return nil, s.refuse(q, err)
	}
	op, err := s.gce.InsertInstance(q.ctx(), s.opts.HostProject, q.call.Zone, p.real, p.template)
	if err != nil {
		s.uncharge(q, p.bill, lines)
		return nil, s.refuse(q, err)
	}
	s.audit(q, "OK", nil, q.chargeLines(all(lines)))
	return s.startedWith(q, op, Operation{Object: p.fresh[0], Also: p.fresh[1:]})
}

// ---- reads ----

// instance is inst as the tenant sees it: no service accounts, and the
// facade's labels and tag gone.
func (q *request) instance(inst *compute.Instance) (map[string]any, error) {
	return q.shape("compute#instance", inst)
}

// callInstance is the silo's instance the call names; another silo's,
// or none, is 404, and a name without the silo's prefix 400.
func (s *Server) callInstance(q *request) (*compute.Instance, error) {
	nf := notFound(q.zonalPath(q.call.Zone, "instances", q.call.Name))
	if err := q.n.checkName(q.call.Name); err != nil {
		return nil, err
	}
	inst, err := s.gce.GetInstance(q.ctx(), s.opts.HostProject, q.call.Zone, q.call.Name)
	if err != nil {
		if gceapi.IsNotFound(err) {
			return nil, nf
		}
		return nil, err
	}
	if !q.owns(inst.Name, inst.Labels) {
		return nil, nf
	}
	return inst, nil
}

func (s *Server) getInstance(q *request) (any, error) {
	inst, err := s.callInstance(q)
	if err != nil {
		return nil, err
	}
	return q.instance(inst)
}

func (s *Server) ownInstances(q *request, zone string) ([]*compute.Instance, error) {
	l, err := s.gce.ListInstances(q.ctx(), s.opts.HostProject, zone, gceapi.ListFilter{Labels: map[string]string{"silo": q.target.ID}, NamePrefix: q.n.prefix()})
	if err != nil && !gceapi.IsNotFound(err) {
		return nil, err
	}
	var out []*compute.Instance
	for _, inst := range l {
		if q.owns(inst.Name, inst.Labels) {
			out = append(out, inst)
		}
	}
	return out, nil
}

func (s *Server) listInstances(q *request) (any, error) {
	l, err := s.ownInstances(q, q.call.Zone)
	if err != nil {
		return nil, err
	}
	var items []map[string]any
	for _, inst := range l {
		m, err := q.instance(inst)
		if err != nil {
			return nil, err
		}
		items = append(items, m)
	}
	return s.list(q, "compute#instanceList", items)
}

func (s *Server) aggregatedInstances(q *request) (any, error) {
	zs, err := s.allowedZones(q)
	if err != nil {
		return nil, err
	}
	var items []scoped
	for _, z := range zs {
		l, err := s.ownInstances(q, z.Name)
		if err != nil {
			return nil, err
		}
		for _, inst := range l {
			m, err := q.instance(inst)
			if err != nil {
				return nil, err
			}
			items = append(items, scoped{scope: "zones/" + z.Name, item: m})
		}
	}
	return s.aggregated(q, "compute#instanceAggregatedList", "instances", items)
}

func (s *Server) getSerialPortOutput(q *request) (any, error) {
	inst, err := s.callInstance(q)
	if err != nil {
		return nil, err
	}
	port, start := int64(1), int64(0)
	if v := q.r.URL.Query().Get("port"); v != "" {
		if port, err = strconv.ParseInt(v, 10, 64); err != nil {
			return nil, invalid("Invalid value for parameter 'port': '%s'", v)
		}
	}
	if v := q.r.URL.Query().Get("start"); v != "" {
		if start, err = strconv.ParseInt(v, 10, 64); err != nil {
			return nil, invalid("Invalid value for parameter 'start': '%s'", v)
		}
	}
	out, err := s.gce.GetSerialPortOutput(q.ctx(), s.opts.HostProject, q.call.Zone, inst.Name, port, start)
	if err != nil {
		return nil, err
	}
	return q.shape("compute#serialPortOutput", out)
}

// ---- lifecycle ----

func (s *Server) deleteInstance(q *request) (any, error) {
	inst, err := s.callInstance(q)
	if err != nil {
		return nil, s.refuse(q, err)
	}
	// The disks GCE deletes with it are refunded with it.
	var also []string
	for _, ad := range inst.Disks {
		if ad.AutoDelete && ad.Source != "" {
			also = append(also, diskObject(q.call.Zone, gceapi.LastSegment(ad.Source)))
		}
	}
	op, err := s.gce.DeleteInstance(q.ctx(), s.opts.HostProject, q.call.Zone, inst.Name)
	if err != nil {
		return nil, s.refuse(q, err)
	}
	s.audit(q, "OK", nil, nil)
	return s.startedWith(q, op, Operation{Object: instanceObject(q.call.Zone, inst.Name), Refund: noRefund, Also: also, OnSuccess: []ledger.Amount{}})
}

// planStart prices a start: a stopped instance takes its vCPU and
// memory again, so start is checked like insert. One that isn't
// stopped charges nothing.
func (s *Server) planStart(q *request) (bill, *compute.Instance, error) {
	inst, err := s.callInstance(q)
	if err != nil {
		return nil, nil, err
	}
	if inst.Status != statusTerminated {
		return nil, inst, nil
	}
	mt, err := s.machineType(q.ctx(), q.call.Zone, inst.MachineType)
	if err != nil {
		return nil, nil, err
	}
	p := s.priceInstance(q.call.Zone, inst.Name, mt, extrasOf(inst), instanceVariant(inst.Scheduling), false)
	p.runningOnly = true
	return bill{p}, inst, nil
}

func (s *Server) startInstance(q *request) (any, error) {
	b, inst, err := s.planStart(q)
	if err != nil {
		return nil, s.refuse(q, err)
	}
	lines, err := s.charge(q, b)
	if err != nil {
		return nil, s.refuse(q, err)
	}
	op, err := s.gce.StartInstance(q.ctx(), s.opts.HostProject, q.call.Zone, inst.Name)
	if err != nil {
		s.uncharge(q, b, lines)
		return nil, s.refuse(q, err)
	}
	charged := all(lines)
	s.audit(q, "OK", nil, q.chargeLines(charged))
	return s.started(q, op, instanceObject(q.call.Zone, inst.Name), orNoRefund(charged))
}

// stopInstance gives back the instance's vCPU and memory once it has
// stopped. A suspended instance keeps them, as GCE keeps its memory.
func (s *Server) stopInstance(q *request) (any, error) {
	ctx := q.ctx()
	discard, err := q.queryBool("discardLocalSsd", false)
	if err != nil {
		return nil, s.refuse(q, err)
	}
	inst, err := s.callInstance(q)
	if err != nil {
		return nil, s.refuse(q, err)
	}
	object := instanceObject(q.call.Zone, inst.Name)
	var release []ledger.Amount
	if inst.Status != statusTerminated {
		err = s.st.Tx(ctx, func(tx store.Tx) error {
			running, err := runningClasses(ctx, tx, resInstance, instanceVariant(inst.Scheduling))
			if err != nil {
				return err
			}
			held, err := ledger.Charges(ctx, tx, object)
			for _, h := range held {
				if h.Kind == "gce" && running[h.Class] {
					release = append(release, ledger.Amount{Key: h.Key, Amount: h.Amount})
				}
			}
			return err
		})
		if err != nil {
			return nil, s.refuse(q, err)
		}
	}
	op, err := s.gce.StopInstance(ctx, s.opts.HostProject, q.call.Zone, inst.Name, discard)
	if err != nil {
		return nil, s.refuse(q, err)
	}
	s.audit(q, "OK", nil, nil)
	rec := Operation{Object: object, Refund: noRefund}
	if len(release) > 0 {
		rec.OnSuccess = release
	}
	return s.startedWith(q, op, rec)
}

// simpleInstanceOp is reset, suspend and resume, which change no
// charge: a suspended instance holds what a running one does.
func (s *Server) simpleInstanceOp(q *request) (any, error) {
	inst, err := s.callInstance(q)
	if err != nil {
		return nil, s.refuse(q, err)
	}
	ctx, zone := q.ctx(), q.call.Zone
	var op *compute.Operation
	switch q.call.Method {
	case "reset":
		op, err = s.gce.ResetInstance(ctx, s.opts.HostProject, zone, inst.Name)
	case "suspend":
		op, err = s.gce.SuspendInstance(ctx, s.opts.HostProject, zone, inst.Name)
	default:
		op, err = s.gce.ResumeInstance(ctx, s.opts.HostProject, zone, inst.Name)
	}
	if err != nil {
		return nil, s.refuse(q, err)
	}
	s.audit(q, "OK", nil, nil)
	return s.started(q, op, instanceObject(zone, inst.Name), noRefund)
}

// machineTypePlan is a setMachineType: the instance priced as it is and
// as it would be, in its current state, with the GPUs and local SSD
// each machine type bundles.
type machineTypePlan struct {
	inst     *compute.Instance
	mt       *compute.MachineType
	old, new priced
}

func (s *Server) planMachineType(q *request) (machineTypePlan, error) {
	var in compute.InstancesSetMachineTypeRequest
	if err := decode(q.body, &in); err != nil {
		return machineTypePlan{}, err
	}
	inst, err := s.callInstance(q)
	if err != nil {
		return machineTypePlan{}, err
	}
	zone := q.call.Zone
	mt, err := s.machineType(q.ctx(), zone, in.MachineType)
	if err != nil {
		return machineTypePlan{}, err
	}
	cur, err := s.machineType(q.ctx(), zone, inst.MachineType)
	if err != nil {
		return machineTypePlan{}, err
	}
	// The new machine type is checked against the instance's GPUs and
	// disks as insert checks them (gcp-backend.md, "Instances").
	x := extrasOf(inst)
	var gpus []attachedGPU
	for i, a := range inst.GuestAccelerators {
		if a != nil {
			gpus = append(gpus, attachedGPU{field: "guestAccelerators[" + itoa(i) + "]", typ: gceapi.LastSegment(a.AcceleratorType), count: a.AcceleratorCount})
		}
	}
	var dp disksPlan
	for i, ad := range inst.Disks {
		if ad == nil {
			continue
		}
		field := "disks[" + itoa(i) + "]"
		if ad.Type == "SCRATCH" {
			// A machine type's own local SSD is listed too; it goes with
			// the type, so only an attached partition is checked.
			if bundledLocalSSDs(cur) == 0 {
				dp.scratch = append(dp.scratch, field)
			}
			continue
		}
		if ad.Source == "" {
			continue
		}
		d, err := s.gce.GetDisk(q.ctx(), s.opts.HostProject, zone, gceapi.LastSegment(ad.Source))
		if err != nil {
			return machineTypePlan{}, err
		}
		dp.typed = append(dp.typed, typedDisk{field: field, typ: diskTypeOf(d.Type)})
	}
	if err := s.checkMachine(mt, gpus, inst.Scheduling, dp); err != nil {
		return machineTypePlan{}, err
	}
	variant, stopped := instanceVariant(inst.Scheduling), inst.Status == statusTerminated
	// A bundled type's own local SSD goes with it, so neither price
	// counts it as attached.
	if bundledLocalSSDs(cur) > 0 {
		x.scratch = 0
	}
	return machineTypePlan{inst: inst, mt: mt,
		old: s.priceInstance(zone, inst.Name, cur, x, variant, stopped),
		new: s.priceInstance(zone, inst.Name, mt, x, variant, stopped)}, nil
}

// diff is what the change charges (up) and gives back (down).
func (p machineTypePlan) diff(ctx context.Context, tx store.Tx) (up, down []ledger.Amount, err error) {
	return delta(ctx, tx, p.old, p.new)
}

// setMachineType re-prices the instance: the increase is charged before
// GCE is called, and the decrease given back once it has worked.
func (s *Server) setMachineType(q *request) (any, error) {
	ctx := q.ctx()
	p, err := s.planMachineType(q)
	if err != nil {
		return nil, s.refuse(q, err)
	}
	object := p.old.object
	up, down, err := s.rechargeObject(q, object, func(tx store.Tx) ([]ledger.Amount, []ledger.Amount, error) { return p.diff(ctx, tx) })
	if err != nil {
		return nil, s.refuse(q, err)
	}
	op, err := s.gce.SetMachineType(ctx, s.opts.HostProject, q.call.Zone, p.inst.Name, s.machineTypeLink(q.call.Zone, p.mt.Name))
	if err != nil {
		s.refundObject(q, object, up)
		return nil, s.refuse(q, err)
	}
	s.audit(q, "OK", map[string]any{"machine_type": p.mt.Name}, q.chargeLines(up))
	rec := Operation{Object: object, Refund: orNoRefund(up)}
	if len(down) > 0 {
		rec.OnSuccess = down
	}
	return s.startedWith(q, op, rec)
}

// ---- disks ----

type attachPlan struct {
	inst *compute.Instance
	disk *compute.AttachedDisk
	bill bill
}

func (s *Server) planAttach(q *request) (attachPlan, error) {
	var in compute.AttachedDisk
	if err := decode(q.body, &in); err != nil {
		return attachPlan{}, err
	}
	inst, err := s.callInstance(q)
	if err != nil {
		return attachPlan{}, err
	}
	dp, err := s.planDisks(q, q.call.Zone, q.call.Name, []*compute.AttachedDisk{&in}, "", len(inst.Disks), false)
	if err != nil {
		return attachPlan{}, err
	}
	mt, err := s.machineType(q.ctx(), q.call.Zone, inst.MachineType)
	if err != nil {
		return attachPlan{}, err
	}
	if err := s.opts.Compat.checkDisks(mt, dp.typed); err != nil {
		return attachPlan{}, err
	}
	return attachPlan{inst: inst, disk: dp.disks[0], bill: dp.bill}, nil
}

func (s *Server) attachDisk(q *request) (any, error) {
	p, err := s.planAttach(q)
	if err != nil {
		return nil, s.refuse(q, err)
	}
	var fresh []string
	for _, b := range p.bill {
		fresh = append(fresh, b.object)
	}
	lines, err := s.charge(q, p.bill, fresh...)
	if err != nil {
		return nil, s.refuse(q, err)
	}
	op, err := s.gce.AttachDisk(q.ctx(), s.opts.HostProject, q.call.Zone, p.inst.Name, p.disk)
	if err != nil {
		s.uncharge(q, p.bill, lines)
		return nil, s.refuse(q, err)
	}
	s.audit(q, "OK", nil, q.chargeLines(all(lines)))
	if len(fresh) == 0 {
		return s.started(q, op, instanceObject(q.call.Zone, p.inst.Name), noRefund)
	}
	return s.started(q, op, fresh[0], nil)
}

func (s *Server) detachDisk(q *request) (any, error) {
	inst, err := s.callInstance(q)
	if err != nil {
		return nil, s.refuse(q, err)
	}
	dev := q.r.URL.Query().Get("deviceName")
	if dev == "" {
		return nil, s.refuse(q, invalid("Required parameter 'deviceName' is missing"))
	}
	op, err := s.gce.DetachDisk(q.ctx(), s.opts.HostProject, q.call.Zone, inst.Name, dev)
	if err != nil {
		return nil, s.refuse(q, err)
	}
	s.audit(q, "OK", nil, nil)
	return s.started(q, op, instanceObject(q.call.Zone, inst.Name), noRefund)
}

// ---- setters, rewritten with insert's rules ----

func (s *Server) setInstanceLabels(q *request) (any, error) {
	var in compute.InstancesSetLabelsRequest
	if err := decode(q.body, &in); err != nil {
		return nil, s.refuse(q, err)
	}
	inst, err := s.callInstance(q)
	if err != nil {
		return nil, s.refuse(q, err)
	}
	op, err := s.gce.SetInstanceLabels(q.ctx(), s.opts.HostProject, q.call.Zone, inst.Name,
		&compute.InstancesSetLabelsRequest{Labels: q.siloLabels(in.Labels), LabelFingerprint: in.LabelFingerprint})
	if err != nil {
		return nil, s.refuse(q, err)
	}
	s.audit(q, "OK", nil, nil)
	return s.started(q, op, instanceObject(q.call.Zone, inst.Name), noRefund)
}

func (s *Server) setInstanceMetadata(q *request) (any, error) {
	var in compute.Metadata
	if err := decode(q.body, &in); err != nil {
		return nil, s.refuse(q, err)
	}
	if err := checkMetadata(&in, q.n.prefix()); err != nil {
		return nil, s.refuse(q, err)
	}
	inst, err := s.callInstance(q)
	if err != nil {
		return nil, s.refuse(q, err)
	}
	op, err := s.gce.SetInstanceMetadata(q.ctx(), s.opts.HostProject, q.call.Zone, inst.Name, realMetadata(&in, q.n.prefix()))
	if err != nil {
		return nil, s.refuse(q, err)
	}
	s.audit(q, "OK", nil, nil)
	return s.started(q, op, instanceObject(q.call.Zone, inst.Name), noRefund)
}

func (s *Server) setInstanceTags(q *request) (any, error) {
	if err := checkTags(q.body["items"]); err != nil {
		return nil, s.refuse(q, err)
	}
	var in compute.Tags
	if err := decode(q.body, &in); err != nil {
		return nil, s.refuse(q, err)
	}
	inst, err := s.callInstance(q)
	if err != nil {
		return nil, s.refuse(q, err)
	}
	tags, err := q.realTags(&in, "items")
	if err != nil {
		return nil, s.refuse(q, err)
	}
	op, err := s.gce.SetInstanceTags(q.ctx(), s.opts.HostProject, q.call.Zone, inst.Name, tags)
	if err != nil {
		return nil, s.refuse(q, err)
	}
	s.audit(q, "OK", nil, nil)
	return s.started(q, op, instanceObject(q.call.Zone, inst.Name), noRefund)
}

func (s *Server) setDeletionProtection(q *request) (any, error) {
	protect, err := q.queryBool("deletionProtection", true)
	if err != nil {
		return nil, s.refuse(q, err)
	}
	inst, err := s.callInstance(q)
	if err != nil {
		return nil, s.refuse(q, err)
	}
	op, err := s.gce.SetDeletionProtection(q.ctx(), s.opts.HostProject, q.call.Zone, inst.Name, protect)
	if err != nil {
		return nil, s.refuse(q, err)
	}
	s.audit(q, "OK", nil, nil)
	return s.started(q, op, instanceObject(q.call.Zone, inst.Name), noRefund)
}
