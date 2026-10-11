package fake

import (
	"context"

	"github.com/bwsalmon/silo/pkg/silo/gce"
	compute "google.golang.org/api/compute/v1"
)

func templateScope(region string) string {
	if region == "" {
		return "global"
	}
	return regionScope(region)
}

// InsertInstanceTemplate implements gce.Client: a global template, or a
// regional one when region is set. GCE checks little of a template's
// properties at insert; the instance made from it is checked in full.
// Each counts once against INSTANCE_TEMPLATES.
func (f *Fake) InsertInstanceTemplate(ctx context.Context, project, region string, body *compute.InstanceTemplate) (*compute.Operation, error) {
	p, err := f.lock(ctx, project)
	if err != nil {
		return nil, err
	}
	defer f.unlock()
	if region != "" {
		if err := f.regionExists(region); err != nil {
			return nil, err
		}
	}
	t := clone(body)
	if err := checkName("resource.name", t.Name); err != nil {
		return nil, err
	}
	scope := templateScope(region)
	link := p.link(scope, "instanceTemplates", t.Name)
	if p.templates.get(scope, t.Name) != nil {
		return nil, alreadyExists(link)
	}
	if t.Properties == nil {
		return nil, invalid("resource.properties", "Required field not specified")
	}
	mtName := t.Properties.MachineType
	if mtName == "" || gce.ParseRef(mtName).Collection != "" {
		return nil, invalid("resource.properties.machineType", "Must be the name of a machine type, not a URL: '%s'", mtName)
	}
	if f.catalog.machineTypeAnywhere(mtName) == nil {
		return nil, invalid("resource.properties.machineType", "Machine type '%s' does not exist", mtName)
	}
	if err := checkLabels("resource.properties.labels", t.Properties.Labels); err != nil {
		return nil, err
	}
	t.Kind = "compute#instanceTemplate"
	t.Id = f.nextID()
	t.SelfLink = link
	t.CreationTimestamp = f.timestamp()
	if region != "" {
		t.Region = gce.SelfLinkBase + "projects/" + p.name + "/regions/" + region
	}
	p.templates.put(scope, t.Name, t)
	return f.insertPending(p, "instanceTemplates.insert", link, t.Id, func() {},
		func() {
			if p.templates.get(scope, t.Name) == t {
				p.templates.del(scope, t.Name)
			}
		}), nil
}

func (f *Fake) template(p *state, region, name string) (*compute.InstanceTemplate, error) {
	if region != "" {
		if err := f.regionExists(region); err != nil {
			return nil, err
		}
	}
	t := p.templates.get(templateScope(region), name)
	if t == nil {
		return nil, notFound(p.link(templateScope(region), "instanceTemplates", name))
	}
	return t, nil
}

// GetInstanceTemplate implements gce.Client.
func (f *Fake) GetInstanceTemplate(ctx context.Context, project, region, name string) (*compute.InstanceTemplate, error) {
	p, err := f.lock(ctx, project)
	if err != nil {
		return nil, err
	}
	defer f.unlock()
	t, err := f.template(p, region, name)
	if err != nil {
		return nil, err
	}
	return clone(t), nil
}

// ListInstanceTemplates implements gce.Client. A template has no labels
// of its own (properties.labels are its instances'), so a filter on
// labels matches none; silod lists templates by name prefix.
func (f *Fake) ListInstanceTemplates(ctx context.Context, project, region string, lf gce.ListFilter) ([]*compute.InstanceTemplate, error) {
	p, err := f.lock(ctx, project)
	if err != nil {
		return nil, err
	}
	defer f.unlock()
	if region != "" {
		if err := f.regionExists(region); err != nil {
			return nil, err
		}
	}
	return filtered(p.templates.list(templateScope(region)), lf, func(t *compute.InstanceTemplate) (string, map[string]string) {
		return t.Name, nil
	}), nil
}

// DeleteInstanceTemplate implements gce.Client.
func (f *Fake) DeleteInstanceTemplate(ctx context.Context, project, region, name string) (*compute.Operation, error) {
	p, err := f.lock(ctx, project)
	if err != nil {
		return nil, err
	}
	defer f.unlock()
	t, err := f.template(p, region, name)
	if err != nil {
		return nil, err
	}
	scope := templateScope(region)
	return f.start(p, "instanceTemplates.delete", t.SelfLink, t.Id, func() *opErr {
		if p.templates.get(scope, name) == t {
			p.templates.del(scope, name)
			delete(p.pending, t.SelfLink)
		}
		return nil
	}, nil), nil
}
