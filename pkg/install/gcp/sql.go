package gcp

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/bwsalmon/silo/pkg/install"
	compute "google.golang.org/api/compute/v1"
	sqladmin "google.golang.org/api/sqladmin/v1"
)

// fromSQL is the part of a Cloud SQL instance install reads.
func fromSQL(in *sqladmin.DatabaseInstance) *install.SQLInstance {
	si := &install.SQLInstance{Name: in.Name, Region: in.Region, DatabaseVersion: in.DatabaseVersion, State: in.State,
		ServiceAttachment: in.PscServiceAttachmentLink, DNSName: in.DnsName, ConnectionName: in.ConnectionName}
	st := in.Settings
	if st == nil {
		return si
	}
	si.Labels, si.Edition, si.Tier, si.Availability = st.UserLabels, st.Edition, st.Tier, st.AvailabilityType
	si.DiskType, si.DiskSizeGB = st.DataDiskType, st.DataDiskSizeGb
	si.AutoResize = st.StorageAutoResize != nil && *st.StorageAutoResize
	si.DeletionProtection = st.DeletionProtectionEnabled
	if b := st.BackupConfiguration; b != nil {
		si.Backups, si.PITR, si.LogRetentionDays = b.Enabled, b.PointInTimeRecoveryEnabled, b.TransactionLogRetentionDays
		if b.BackupRetentionSettings != nil {
			si.BackupRetention = b.BackupRetentionSettings.RetainedBackups
		}
	}
	if ip := st.IpConfiguration; ip != nil {
		si.PublicIP, si.PrivateNetwork = ip.Ipv4Enabled, ip.PrivateNetwork
		if ip.PscConfig != nil {
			si.PSC, si.PSCConsumers = ip.PscConfig.PscEnabled, ip.PscConfig.AllowedConsumerProjects
		}
	}
	for _, f := range st.DatabaseFlags {
		if si.Flags == nil {
			si.Flags = map[string]string{}
		}
		si.Flags[f.Name] = f.Value
	}
	if w := st.MaintenanceWindow; w != nil {
		si.MaintenanceDay, si.MaintenanceHour = w.Day, w.Hour
	}
	return si
}

// toSQL is in as a create or a patch: every setting install owns, sent
// even where it is false or zero.
func toSQL(in *install.SQLInstance) *sqladmin.DatabaseInstance {
	auto := in.AutoResize
	var flags []*sqladmin.DatabaseFlags
	names := make([]string, 0, len(in.Flags))
	for k := range in.Flags {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		flags = append(flags, &sqladmin.DatabaseFlags{Name: k, Value: in.Flags[k]})
	}
	return &sqladmin.DatabaseInstance{
		Name: in.Name, Region: in.Region, DatabaseVersion: in.DatabaseVersion,
		Settings: &sqladmin.Settings{
			Edition: in.Edition, Tier: in.Tier, AvailabilityType: in.Availability,
			DataDiskType: in.DiskType, DataDiskSizeGb: in.DiskSizeGB, StorageAutoResize: &auto,
			BackupConfiguration: &sqladmin.BackupConfiguration{
				Enabled: in.Backups, PointInTimeRecoveryEnabled: in.PITR, TransactionLogRetentionDays: in.LogRetentionDays,
				BackupRetentionSettings: &sqladmin.BackupRetentionSettings{RetainedBackups: in.BackupRetention, RetentionUnit: "COUNT"},
				ForceSendFields:         []string{"Enabled", "PointInTimeRecoveryEnabled"},
			},
			DeletionProtectionEnabled: in.DeletionProtection,
			IpConfiguration: &sqladmin.IpConfiguration{
				Ipv4Enabled: in.PublicIP, PrivateNetwork: in.PrivateNetwork,
				PscConfig: &sqladmin.PscConfig{PscEnabled: in.PSC, AllowedConsumerProjects: in.PSCConsumers,
					ForceSendFields: []string{"PscEnabled"}},
				ForceSendFields: []string{"Ipv4Enabled"},
			},
			DatabaseFlags:     flags,
			MaintenanceWindow: &sqladmin.MaintenanceWindow{Day: in.MaintenanceDay, Hour: in.MaintenanceHour, ForceSendFields: []string{"Hour"}},
			UserLabels:        in.Labels,
			ForceSendFields:   []string{"DeletionProtectionEnabled", "UserLabels"},
		},
	}
}

// waitOp waits for a Cloud SQL operation.
func (c SQLAdmin) waitOp(ctx context.Context, project string, op *sqladmin.Operation, err error) error {
	for err == nil && op.Status != "DONE" {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(3 * time.Second):
		}
		op, err = c.s.Operations.Get(project, op.Name).Context(ctx).Do()
	}
	if err != nil {
		return err
	}
	if op.Error != nil && len(op.Error.Errors) > 0 {
		e := op.Error.Errors[0]
		return fmt.Errorf("%s %s: %s: %s", op.OperationType, op.TargetId, e.Code, e.Message)
	}
	return nil
}

func (c SQLAdmin) GetInstance(ctx context.Context, project, name string) (*install.SQLInstance, error) {
	in, err := c.s.Instances.Get(project, name).Context(ctx).Do()
	if err != nil {
		return nil, err
	}
	return fromSQL(in), nil
}

// InsertInstance starts the create and doesn't wait for it. No
// rootPassword is sent, so the built-in postgres user has none.
func (c SQLAdmin) InsertInstance(ctx context.Context, project string, in *install.SQLInstance) error {
	_, err := c.s.Instances.Insert(project, toSQL(in)).Context(ctx).Do()
	return err
}

func (c SQLAdmin) PatchInstance(ctx context.Context, project string, in *install.SQLInstance) error {
	if err := c.idle(ctx, project, in.Name); err != nil {
		return err
	}
	op, err := c.s.Instances.Patch(project, in.Name, toSQL(in)).Context(ctx).Do()
	return c.waitOp(ctx, project, op, err)
}

// SetDeletionProtection patches settings.deletionProtectionEnabled
// alone: the patch sends no other field.
func (c SQLAdmin) SetDeletionProtection(ctx context.Context, project, name string, on bool) error {
	if err := c.idle(ctx, project, name); err != nil {
		return err
	}
	patch := &sqladmin.DatabaseInstance{Settings: &sqladmin.Settings{
		DeletionProtectionEnabled: on, ForceSendFields: []string{"DeletionProtectionEnabled"}}}
	op, err := c.s.Instances.Patch(project, name, patch).Context(ctx).Do()
	return c.waitOp(ctx, project, op, err)
}

// DeleteInstance deletes the instance with a final backup kept for
// finalBackupDays days.
func (c SQLAdmin) DeleteInstance(ctx context.Context, project, name string, finalBackupDays int64) error {
	if err := c.idle(ctx, project, name); err != nil {
		return err
	}
	op, err := c.s.Instances.Delete(project, name).EnableFinalBackup(true).FinalBackupTtlDays(finalBackupDays).
		FinalBackupDescription("silo uninstall --delete-data").Context(ctx).Do()
	return c.waitOp(ctx, project, op, err)
}

// idle waits until no operation is running on the instance: Cloud SQL
// refuses a second one.
func (c SQLAdmin) idle(ctx context.Context, project, name string) error {
	for {
		r, err := c.s.Operations.List(project).Instance(name).MaxResults(5).Context(ctx).Do()
		if err != nil {
			return err
		}
		busy := false
		for _, op := range r.Items {
			busy = busy || op.Status == "PENDING" || op.Status == "RUNNING"
		}
		if !busy {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
}

func (c SQLAdmin) WaitInstance(ctx context.Context, project, name string) (*install.SQLInstance, error) {
	for {
		in, err := c.GetInstance(ctx, project, name)
		if err != nil {
			return nil, err
		}
		switch in.State {
		case "RUNNABLE":
			if err := c.idle(ctx, project, name); err != nil {
				return nil, err
			}
			return c.GetInstance(ctx, project, name)
		case "FAILED", "SUSPENDED", "MAINTENANCE_FAILED":
			return nil, fmt.Errorf("instance %s is %s", name, in.State)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(10 * time.Second):
		}
	}
}

func (c SQLAdmin) ListDatabases(ctx context.Context, project, instance string) ([]string, error) {
	r, err := c.s.Databases.List(project, instance).Context(ctx).Do()
	if err != nil {
		return nil, err
	}
	var out []string
	for _, d := range r.Items {
		out = append(out, d.Name)
	}
	return out, nil
}

func (c SQLAdmin) InsertDatabase(ctx context.Context, project, instance, name string) error {
	op, err := c.s.Databases.Insert(project, instance, &sqladmin.Database{Name: name, Instance: instance, Project: project}).Context(ctx).Do()
	return c.waitOp(ctx, project, op, err)
}

func (c SQLAdmin) ListUsers(ctx context.Context, project, instance string) ([]install.SQLUser, error) {
	r, err := c.s.Users.List(project, instance).Context(ctx).Do()
	if err != nil {
		return nil, err
	}
	var out []install.SQLUser
	for _, u := range r.Items {
		out = append(out, install.SQLUser{Name: u.Name, Type: u.Type})
	}
	return out, nil
}

// InsertUser sends no password: an IAM user logs in with its OAuth
// token.
func (c SQLAdmin) InsertUser(ctx context.Context, project, instance string, u *install.SQLUser) error {
	op, err := c.s.Users.Insert(project, instance, &sqladmin.User{Name: u.Name, Type: u.Type,
		DatabaseRoles: u.DatabaseRoles}).Context(ctx).Do()
	return c.waitOp(ctx, project, op, err)
}

func (c ComputeExtras) GetForwardingRule(ctx context.Context, project, region, name string) (*compute.ForwardingRule, error) {
	return c.s.ForwardingRules.Get(project, region, name).Context(ctx).Do()
}

func (c ComputeExtras) InsertForwardingRule(ctx context.Context, project, region string, r *compute.ForwardingRule) error {
	op, err := c.s.ForwardingRules.Insert(project, region, r).Context(ctx).Do()
	return c.wait(ctx, project, op, err)
}

func (c ComputeExtras) DeleteForwardingRule(ctx context.Context, project, region, name string) error {
	op, err := c.s.ForwardingRules.Delete(project, region, name).Context(ctx).Do()
	return c.wait(ctx, project, op, err)
}

func (c ComputeExtras) GuestAttribute(ctx context.Context, project, zone, instance, key string) (string, error) {
	g, err := c.s.Instances.GetGuestAttributes(project, zone, instance).QueryPath(key).Context(ctx).Do()
	if err != nil {
		return "", err
	}
	if g.QueryValue != nil {
		for _, it := range g.QueryValue.Items {
			if it.Namespace+"/"+it.Key == key {
				return it.Value, nil
			}
		}
	}
	return g.VariableValue, nil
}
