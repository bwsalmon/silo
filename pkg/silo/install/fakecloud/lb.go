package fakecloud

import (
	"context"

	compute "google.golang.org/api/compute/v1"
)

// The replicas step's objects: kept as given, by region/name (zone/name
// for instance groups).

func getObj[T any](f *Fake, m map[string]*T, k, what string) (*T, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.need("compute.googleapis.com"); err != nil {
		return nil, err
	}
	if v := m[k]; v != nil {
		return cloneJSON(v), nil
	}
	return nil, notFound(what)
}

func insertObj[T any](f *Fake, m map[string]*T, k, what string, v *T) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.need("compute.googleapis.com"); err != nil {
		return err
	}
	if m[k] != nil {
		return conflict(what)
	}
	m[k] = cloneJSON(v)
	return nil
}

func deleteObj[T any](f *Fake, m map[string]*T, k, what string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if m[k] == nil {
		return notFound(what)
	}
	delete(m, k)
	return nil
}

func (x computeExtras) GetHealthCheck(ctx context.Context, project, region, name string) (*compute.HealthCheck, error) {
	return getObj(x.f, x.f.HealthChecks, regional(region, name), "health check "+name)
}

func (x computeExtras) InsertHealthCheck(ctx context.Context, project, region string, hc *compute.HealthCheck) error {
	return insertObj(x.f, x.f.HealthChecks, regional(region, hc.Name), "health check "+hc.Name, hc)
}

func (x computeExtras) DeleteHealthCheck(ctx context.Context, project, region, name string) error {
	return deleteObj(x.f, x.f.HealthChecks, regional(region, name), "health check "+name)
}

func (x computeExtras) GetInstanceGroupManager(ctx context.Context, project, region, name string) (*compute.InstanceGroupManager, error) {
	return getObj(x.f, x.f.MIGs, regional(region, name), "instance group manager "+name)
}

func (x computeExtras) InsertInstanceGroupManager(ctx context.Context, project, region string, m *compute.InstanceGroupManager) error {
	return insertObj(x.f, x.f.MIGs, regional(region, m.Name), "instance group manager "+m.Name, m)
}

func (x computeExtras) PatchInstanceGroupManager(ctx context.Context, project, region string, m *compute.InstanceGroupManager) error {
	x.f.mu.Lock()
	defer x.f.mu.Unlock()
	cur := x.f.MIGs[regional(region, m.Name)]
	if cur == nil {
		return notFound("instance group manager " + m.Name)
	}
	cur.InstanceTemplate, cur.TargetSize = m.InstanceTemplate, m.TargetSize
	cur.UpdatePolicy = cloneJSON(m.UpdatePolicy)
	cur.AutoHealingPolicies = m.AutoHealingPolicies
	return nil
}

func (x computeExtras) DeleteInstanceGroupManager(ctx context.Context, project, region, name string) error {
	return deleteObj(x.f, x.f.MIGs, regional(region, name), "instance group manager "+name)
}

func (x computeExtras) GetBackendService(ctx context.Context, project, region, name string) (*compute.BackendService, error) {
	return getObj(x.f, x.f.BackendServices, regional(region, name), "backend service "+name)
}

func (x computeExtras) InsertBackendService(ctx context.Context, project, region string, b *compute.BackendService) error {
	return insertObj(x.f, x.f.BackendServices, regional(region, b.Name), "backend service "+b.Name, b)
}

func (x computeExtras) PatchBackendService(ctx context.Context, project, region string, b *compute.BackendService) error {
	x.f.mu.Lock()
	defer x.f.mu.Unlock()
	cur := x.f.BackendServices[regional(region, b.Name)]
	if cur == nil {
		return notFound("backend service " + b.Name)
	}
	cur.Backends = cloneJSON(&compute.BackendService{Backends: b.Backends}).Backends
	return nil
}

func (x computeExtras) DeleteBackendService(ctx context.Context, project, region, name string) error {
	return deleteObj(x.f, x.f.BackendServices, regional(region, name), "backend service "+name)
}

func (x computeExtras) GetInstanceGroup(ctx context.Context, project, zone, name string) (*compute.InstanceGroup, error) {
	return getObj(x.f, x.f.InstanceGroups, regional(zone, name), "instance group "+name)
}

func (x computeExtras) InsertInstanceGroup(ctx context.Context, project, zone string, g *compute.InstanceGroup) error {
	return insertObj(x.f, x.f.InstanceGroups, regional(zone, g.Name), "instance group "+g.Name, g)
}

func (x computeExtras) GroupInstances(ctx context.Context, project, zone, name string) ([]string, error) {
	x.f.mu.Lock()
	defer x.f.mu.Unlock()
	k := regional(zone, name)
	if x.f.InstanceGroups[k] == nil {
		return nil, notFound("instance group " + name)
	}
	return append([]string(nil), x.f.GroupMembers[k]...), nil
}

func (x computeExtras) AddGroupInstances(ctx context.Context, project, zone, name string, instances []string) error {
	x.f.mu.Lock()
	defer x.f.mu.Unlock()
	k := regional(zone, name)
	if x.f.InstanceGroups[k] == nil {
		return notFound("instance group " + name)
	}
	x.f.GroupMembers[k] = append(x.f.GroupMembers[k], instances...)
	return nil
}

func (x computeExtras) DeleteInstanceGroup(ctx context.Context, project, zone, name string) error {
	if err := deleteObj(x.f, x.f.InstanceGroups, regional(zone, name), "instance group "+name); err != nil {
		return err
	}
	x.f.mu.Lock()
	delete(x.f.GroupMembers, regional(zone, name))
	x.f.mu.Unlock()
	return nil
}
