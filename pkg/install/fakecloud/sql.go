package fakecloud

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/bwsalmon/silo/pkg/backend/gcp/gce"
	"github.com/bwsalmon/silo/pkg/install"
	compute "google.golang.org/api/compute/v1"
	"google.golang.org/api/googleapi"
)

// sqlInstance is the index of the instance name in SQLInstances, or -1.
// The caller holds mu.
func (f *Fake) sqlInstance(name string) int {
	for i := range f.SQLInstances {
		if f.SQLInstances[i].Name == name {
			return i
		}
	}
	return -1
}

func copyInstance(in install.SQLInstance) *install.SQLInstance {
	return cloneJSON(&in)
}

func (s sqlAdmin) GetInstance(ctx context.Context, project, name string) (*install.SQLInstance, error) {
	s.f.mu.Lock()
	defer s.f.mu.Unlock()
	if err := s.f.need("sqladmin.googleapis.com"); err != nil {
		return nil, err
	}
	i := s.f.sqlInstance(name)
	if i < 0 {
		return nil, notFound("instance " + name)
	}
	return copyInstance(s.f.SQLInstances[i]), nil
}

// InsertInstance makes the instance PENDING_CREATE, with the attachment,
// DNS name and connection name Cloud SQL would give it, the built-in
// postgres user and database, and no password. WaitInstance finishes
// creating it.
func (s sqlAdmin) InsertInstance(ctx context.Context, project string, in *install.SQLInstance) error {
	s.f.mu.Lock()
	defer s.f.mu.Unlock()
	if err := s.f.need("sqladmin.googleapis.com"); err != nil {
		return err
	}
	if s.f.sqlInstance(in.Name) >= 0 {
		return conflict("instance " + in.Name)
	}
	cp := copyInstance(*in)
	cp.State = "PENDING_CREATE"
	cp.ServiceAttachment = fmt.Sprintf("projects/sql-producer-%s/regions/%s/serviceAttachments/a-%s", in.Name, in.Region, in.Name)
	cp.DNSName = fmt.Sprintf("%s.%s.sql.goog.", strings.TrimPrefix(in.Name, install.DBInstancePrefix), in.Region)
	cp.ConnectionName = project + ":" + in.Region + ":" + in.Name
	s.f.SQLInstances = append(s.f.SQLInstances, *cp)
	s.f.Databases[in.Name] = []string{"postgres"}
	s.f.Users[in.Name] = []install.SQLUser{{Name: "postgres", Type: "BUILT_IN"}}
	return nil
}

func (s sqlAdmin) PatchInstance(ctx context.Context, project string, in *install.SQLInstance) error {
	s.f.mu.Lock()
	defer s.f.mu.Unlock()
	if err := s.f.need("sqladmin.googleapis.com"); err != nil {
		return err
	}
	i := s.f.sqlInstance(in.Name)
	if i < 0 {
		return notFound("instance " + in.Name)
	}
	cur := s.f.SQLInstances[i]
	next := *copyInstance(*in)
	if cur.DeletionProtection != next.DeletionProtection {
		s.f.ProtectionChanges = append(s.f.ProtectionChanges, fmt.Sprintf("%s=%t", in.Name, next.DeletionProtection))
	}
	// Read only.
	next.State, next.Region = cur.State, cur.Region
	next.ServiceAttachment, next.DNSName, next.ConnectionName = cur.ServiceAttachment, cur.DNSName, cur.ConnectionName
	s.f.SQLInstances[i] = next
	return nil
}

func (s sqlAdmin) SetDeletionProtection(ctx context.Context, project, name string, on bool) error {
	s.f.mu.Lock()
	defer s.f.mu.Unlock()
	if err := s.f.need("sqladmin.googleapis.com"); err != nil {
		return err
	}
	i := s.f.sqlInstance(name)
	if i < 0 {
		return notFound("instance " + name)
	}
	if s.f.SQLInstances[i].DeletionProtection != on {
		s.f.ProtectionChanges = append(s.f.ProtectionChanges, fmt.Sprintf("%s=%t", name, on))
	}
	s.f.SQLInstances[i].DeletionProtection = on
	return nil
}

// DeleteInstance refuses an instance with deletion protection, as Cloud
// SQL does, and records the final backup in FinalBackups.
func (s sqlAdmin) DeleteInstance(ctx context.Context, project, name string, finalBackupDays int64) error {
	s.f.mu.Lock()
	defer s.f.mu.Unlock()
	if err := s.f.need("sqladmin.googleapis.com"); err != nil {
		return err
	}
	i := s.f.sqlInstance(name)
	if i < 0 {
		return notFound("instance " + name)
	}
	if s.f.SQLInstances[i].DeletionProtection {
		return &googleapi.Error{Code: http.StatusBadRequest, Message: "The instance is protected. Please disable the deletion protection and try again."}
	}
	s.f.SQLInstances = append(s.f.SQLInstances[:i], s.f.SQLInstances[i+1:]...)
	delete(s.f.Databases, name)
	delete(s.f.Users, name)
	s.f.FinalBackups[name] = finalBackupDays
	return nil
}

func (s sqlAdmin) WaitInstance(ctx context.Context, project, name string) (*install.SQLInstance, error) {
	s.f.mu.Lock()
	defer s.f.mu.Unlock()
	if err := s.f.need("sqladmin.googleapis.com"); err != nil {
		return nil, err
	}
	i := s.f.sqlInstance(name)
	if i < 0 {
		return nil, notFound("instance " + name)
	}
	s.f.SQLInstances[i].State = "RUNNABLE"
	return copyInstance(s.f.SQLInstances[i]), nil
}

func (s sqlAdmin) running(name string) error {
	if err := s.f.need("sqladmin.googleapis.com"); err != nil {
		return err
	}
	i := s.f.sqlInstance(name)
	if i < 0 {
		return notFound("instance " + name)
	}
	if s.f.SQLInstances[i].State != "RUNNABLE" {
		return &googleapi.Error{Code: http.StatusBadRequest, Message: "Invalid request since instance is not running."}
	}
	return nil
}

func (s sqlAdmin) ListDatabases(ctx context.Context, project, instance string) ([]string, error) {
	s.f.mu.Lock()
	defer s.f.mu.Unlock()
	if err := s.running(instance); err != nil {
		return nil, err
	}
	return append([]string(nil), s.f.Databases[instance]...), nil
}

func (s sqlAdmin) InsertDatabase(ctx context.Context, project, instance, name string) error {
	s.f.mu.Lock()
	defer s.f.mu.Unlock()
	if err := s.running(instance); err != nil {
		return err
	}
	for _, d := range s.f.Databases[instance] {
		if d == name {
			return conflict("database " + name)
		}
	}
	s.f.Databases[instance] = append(s.f.Databases[instance], name)
	return nil
}

func (s sqlAdmin) ListUsers(ctx context.Context, project, instance string) ([]install.SQLUser, error) {
	s.f.mu.Lock()
	defer s.f.mu.Unlock()
	if err := s.running(instance); err != nil {
		return nil, err
	}
	var out []install.SQLUser
	for _, u := range s.f.Users[instance] {
		// users.list doesn't report database roles.
		out = append(out, install.SQLUser{Name: u.Name, Type: u.Type})
	}
	return out, nil
}

func (s sqlAdmin) InsertUser(ctx context.Context, project, instance string, u *install.SQLUser) error {
	s.f.mu.Lock()
	defer s.f.mu.Unlock()
	if err := s.running(instance); err != nil {
		return err
	}
	if s.f.RefuseRoles && len(u.DatabaseRoles) > 0 {
		return badRequest("Invalid request: databaseRoles is not supported for this user type.")
	}
	for _, have := range s.f.Users[instance] {
		if have.Name == u.Name {
			return conflict("user " + u.Name)
		}
	}
	s.f.Users[instance] = append(s.f.Users[instance], *cloneJSON(u))
	return nil
}

func (x computeExtras) GetForwardingRule(ctx context.Context, project, region, name string) (*compute.ForwardingRule, error) {
	if err := noRegion(region); err != nil {
		return nil, err
	}
	x.f.mu.Lock()
	defer x.f.mu.Unlock()
	if err := x.f.need("compute.googleapis.com"); err != nil {
		return nil, err
	}
	r := x.f.ForwardingRules[regional(region, name)]
	if r == nil {
		return nil, notFound("forwarding rule " + name)
	}
	return cloneJSON(r), nil
}

// InsertForwardingRule needs the address it names to exist, as GCE
// does, and takes its IP.
func (x computeExtras) InsertForwardingRule(ctx context.Context, project, region string, r *compute.ForwardingRule) error {
	x.f.mu.Lock()
	defer x.f.mu.Unlock()
	if err := x.f.need("compute.googleapis.com"); err != nil {
		return err
	}
	k := regional(region, r.Name)
	if x.f.ForwardingRules[k] != nil {
		return conflict("forwarding rule " + r.Name)
	}
	a := x.f.Addresses[regional(region, gce.LastSegment(r.IPAddress))]
	if a == nil {
		return badRequest("The resource '" + r.IPAddress + "' was not found")
	}
	if r.Target == "" && r.BackendService == "" {
		return badRequest("target or backendService is required")
	}
	cp := cloneJSON(r)
	cp.IPAddress = a.Address
	x.f.ForwardingRules[k] = cp
	return nil
}

func (x computeExtras) DeleteForwardingRule(ctx context.Context, project, region, name string) error {
	x.f.mu.Lock()
	defer x.f.mu.Unlock()
	k := regional(region, name)
	if x.f.ForwardingRules[k] == nil {
		return notFound("forwarding rule " + name)
	}
	delete(x.f.ForwardingRules, k)
	return nil
}

// GuestAttribute stands in for silod migrate on silo-migrate: the VM
// reports MigrateResult, or "ok <its container's --to>".
func (x computeExtras) GuestAttribute(ctx context.Context, project, zone, instance, key string) (string, error) {
	vm, err := x.f.Compute.GetInstance(ctx, project, zone, instance)
	if err != nil {
		return "", err
	}
	if instance != install.MigrateVM || key != install.MigrateAttribute {
		return "", notFound("guest attribute " + key)
	}
	x.f.mu.Lock()
	defer x.f.mu.Unlock()
	if x.f.MigrateResult != "" {
		return x.f.MigrateResult, nil
	}
	if vm.Metadata != nil {
		for _, it := range vm.Metadata.Items {
			if it.Key != "gce-container-declaration" || it.Value == nil {
				continue
			}
			lines := strings.Split(*it.Value, "\n")
			for i, l := range lines {
				if strings.TrimSpace(l) == `- "--to"` && i+1 < len(lines) {
					return "ok " + strings.Trim(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(lines[i+1]), "-")), `"`), nil
				}
			}
		}
	}
	return "", notFound("guest attribute " + key)
}
