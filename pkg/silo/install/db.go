package install

import (
	"context"
	"crypto/rand"
	"fmt"
	"sort"
	"strings"
	"time"
)

// The database's objects (state.md "The Cloud SQL instance" and
// "Connectivity", SV-47 to SV-49).
const (
	// RoleLabel and RoleDB are the second label the instance is found
	// by, beside silo-install.
	RoleLabel = "silo-role"
	RoleDB    = "db"
	// RestoredLabel is set on an instance install adopts with
	// --db-instance, until the migrate step has set silod's reconcile
	// to report-only on it (state.md "Backups and recovery", SV-54).
	RestoredLabel = "silo-restored"
	// SchemaLabel is the schema version the migrate step last left the
	// database at. Install can't reach the database itself, so this is
	// how a later run knows whether there is anything to migrate.
	SchemaLabel = "silo-schema"

	DBInstancePrefix = "silo-db-"
	DBName           = "silo"
	DBEndpoint       = "silo-db-psc"
	DBZone           = "silo-db"

	// DBDiskGB is the instance's starting disk; automatic increase
	// grows it, and install never shrinks it.
	DBDiskGB = 10
	// DBLogDays is how many days of transaction logs PITR keeps.
	DBLogDays = 7
	// iamAuthFlag is the database flag IAM database login needs.
	iamAuthFlag = "cloudsql.iam_authentication"
	// SuperuserRole is the database role the IAM user is created with,
	// which owns databases the Admin API creates.
	SuperuserRole = "cloudsqlsuperuser"
)

// DBUser is silod's IAM database user: its service account's email
// without .gserviceaccount.com.
func DBUser(project string) string { return "silod@" + project + ".iam" }

// newSuffix is four random lower-case letters and digits, for the
// instance's name (Cloud SQL keeps a deleted instance's name for about
// a week, so the name can't be fixed).
func newSuffix() string {
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, 4)
	rand.Read(b)
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return string(b)
}

// ours reports whether in carries this install's database labels.
func (e *Env) ours(in *SQLInstance) bool {
	return in.Labels[LabelKey] == e.ID && in.Labels[RoleLabel] == RoleDB
}

// findDB is the instance install uses: --db-instance's, by name, or else
// the one carrying this install's labels. others are any other
// instances carrying them.
func findDB(env *Env, list []SQLInstance) (db *SQLInstance, others []SQLInstance) {
	name := env.Config.DBInstance
	for i := range list {
		in := list[i]
		switch {
		case name != "" && in.Name == name:
			db = &in
		case name == "" && db == nil && env.ours(&in):
			db = &in
		case env.ours(&in):
			others = append(others, in)
		}
	}
	return db, others
}

// observeDB lists the project's instances and finds install's. on is
// false while sqladmin isn't enabled.
func observeDB(ctx context.Context, env *Env) (on bool, db *SQLInstance, others []SQLInstance, err error) {
	if on, err = env.Enabled(ctx, "sqladmin.googleapis.com"); err != nil || !on {
		return on, nil, nil, err
	}
	list, err := env.Cloud.SQLAdmin.ListInstances(ctx, env.Config.Project)
	if err != nil {
		return true, nil, nil, err
	}
	db, others = findDB(env, list)
	return true, db, others, nil
}

// DBInstance is step 2a, db-instance: it starts creating the Cloud SQL
// instance and doesn't wait for it, so creation overlaps the steps
// after it. On an existing instance it changes the settings install
// owns in place; with --db-instance it adopts that instance.
type DBInstance struct{ Env *Env }

// DBInstanceObserved is install's instance, if there is one, and any
// other instance carrying its labels.
type DBInstanceObserved struct {
	SQLEnabled bool          `json:"sqlEnabled"`
	Instance   *SQLInstance  `json:"instance,omitempty"`
	Others     []SQLInstance `json:"others,omitempty"`
}

func (s *DBInstance) Name() string          { return "db-instance" }
func (s *DBInstance) NewObserved() Observed { return &DBInstanceObserved{} }

func (s *DBInstance) Observe(ctx context.Context) (Observed, error) {
	on, db, others, err := observeDB(ctx, s.Env)
	if err != nil {
		return nil, err
	}
	return &DBInstanceObserved{SQLEnabled: on, Instance: db, Others: others}, nil
}

func availability(ha bool) string {
	if ha {
		return "REGIONAL"
	}
	return "ZONAL"
}

// want is cur with every setting install owns set from the flags, its
// labels and flags merged with install's, or a new instance's settings
// if cur is nil.
func (s *DBInstance) want(cur *SQLInstance, name string) *SQLInstance {
	cfg := &s.Env.Config
	day, hour, _ := MaintenanceWindow(cfg.DBMaintenanceWindow)
	w := &SQLInstance{}
	if cur != nil {
		cp := *cur
		w = &cp
	}
	w.Name, w.Region, w.DatabaseVersion = name, cfg.HomeRegion(), cfg.DBVersion
	w.Edition, w.Tier, w.Availability = "ENTERPRISE", cfg.DBTier, availability(cfg.DBHA)
	w.DiskType, w.AutoResize = "PD_SSD", true
	if w.DiskSizeGB < DBDiskGB {
		w.DiskSizeGB = DBDiskGB
	}
	w.Backups, w.BackupRetention = true, int64(cfg.DBBackupRetention)
	w.PITR, w.LogRetentionDays = true, DBLogDays
	w.DeletionProtection = true
	w.PublicIP, w.PrivateNetwork = false, ""
	w.PSC, w.PSCConsumers = true, []string{cfg.Project}
	w.MaintenanceDay, w.MaintenanceHour = int64(day), int64(hour)
	w.Labels = merge(w.Labels, map[string]string{LabelKey: s.Env.ID, RoleLabel: RoleDB})
	w.Flags = merge(w.Flags, map[string]string{iamAuthFlag: "on"})
	return w
}

func merge(a, b map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		out[k] = v
	}
	return out
}

// pgMajor is POSTGRES_<n>'s n, or 0.
func pgMajor(v string) int {
	var n int
	if _, err := fmt.Sscanf(v, "POSTGRES_%d", &n); err != nil {
		return 0
	}
	return n
}

// differences names each setting install owns on have that differs from
// want, and whether one of them restarts the instance.
func differences(have, want *SQLInstance) (diffs []string, restarts bool) {
	d := func(what string, h, w any) {
		if fmt.Sprint(h) != fmt.Sprint(w) {
			diffs = append(diffs, fmt.Sprintf("%s %v, not %v", what, w, h))
		}
	}
	if have.Tier != want.Tier || have.Availability != want.Availability {
		restarts = true
	}
	d("tier", have.Tier, want.Tier)
	d("availability", have.Availability, want.Availability)
	if have.DatabaseVersion != want.DatabaseVersion {
		diffs = append(diffs, fmt.Sprintf("%s, upgraded from %s", want.DatabaseVersion, have.DatabaseVersion))
		restarts = true
	}
	d("edition", have.Edition, want.Edition)
	d("disk", have.DiskType, want.DiskType)
	if have.DiskSizeGB < DBDiskGB {
		d("disk size", have.DiskSizeGB, want.DiskSizeGB)
	}
	d("storage auto-increase", have.AutoResize, want.AutoResize)
	d("backups", have.Backups, want.Backups)
	d("backup retention", have.BackupRetention, want.BackupRetention)
	d("point-in-time recovery", have.PITR, want.PITR)
	d("log retention days", have.LogRetentionDays, want.LogRetentionDays)
	d("deletion protection", have.DeletionProtection, want.DeletionProtection)
	d("public IP", have.PublicIP, want.PublicIP)
	if have.PrivateNetwork != "" {
		diffs = append(diffs, "no private services access, not "+have.PrivateNetwork)
	}
	d("PSC", have.PSC, want.PSC)
	d("PSC consumers", strings.Join(have.PSCConsumers, ","), strings.Join(want.PSCConsumers, ","))
	d(iamAuthFlag, have.Flags[iamAuthFlag], want.Flags[iamAuthFlag])
	if have.MaintenanceDay != want.MaintenanceDay || have.MaintenanceHour != want.MaintenanceHour {
		diffs = append(diffs, fmt.Sprintf("maintenance window %s, not %s", window(want), window(have)))
	}
	return diffs, restarts
}

// window is an instance's maintenance window as the flag writes it.
func window(in *SQLInstance) string {
	if in.MaintenanceDay < 1 || in.MaintenanceDay > 7 {
		return "any"
	}
	return fmt.Sprintf("%s:%02d:00", days[in.MaintenanceDay-1], in.MaintenanceHour)
}

// describe is the instance's settings as the plan shows them.
func (s *DBInstance) describe() string {
	cfg := &s.Env.Config
	return fmt.Sprintf("%s %s %s %s", cfg.DBVersion, cfg.DBTier, strings.ToLower(availability(cfg.DBHA)), cfg.HomeRegion())
}

func (s *DBInstance) Plan(o Observed) []Change {
	obs := o.(*DBInstanceObserved)
	cfg := &s.Env.Config
	in := obs.Instance
	if in == nil {
		if cfg.DBInstance != "" && obs.SQLEnabled {
			return []Change{{Action: ActionFail, Object: cfg.DBInstance, Detail: "doesn't exist",
				Why: "--db-instance adopts an instance that exists", Fix: "check the name: gcloud sql instances list --project " + cfg.Project}}
		}
		return []Change{{Action: ActionCreate, Object: DBInstancePrefix + "<suffix>",
			Detail:   s.describe() + "\n(takes several minutes; install continues meanwhile)",
			Estimate: 10 * time.Second}}
	}
	if in.Region != cfg.HomeRegion() {
		return []Change{{Action: ActionFail, Object: in.Name, Detail: "is in " + in.Region + ", not the home region " + cfg.HomeRegion(),
			Why: "silod reaches its database through an endpoint in the home region", Fix: "restore it into a new instance in " + cfg.HomeRegion()}}
	}
	if !strings.HasPrefix(in.DatabaseVersion, "POSTGRES_") {
		return []Change{{Action: ActionFail, Object: in.Name, Detail: "is " + in.DatabaseVersion + ", not PostgreSQL",
			Why: "silod's schema is PostgreSQL", Fix: "adopt a PostgreSQL instance"}}
	}
	if pgMajor(in.DatabaseVersion) > pgMajor(cfg.DBVersion) {
		return []Change{{Action: ActionFail, Object: in.Name, Detail: fmt.Sprintf("is %s, newer than --db-version %s", in.DatabaseVersion, cfg.DBVersion),
			Why: "Cloud SQL's major versions only go up", Fix: "pass --db-version " + in.DatabaseVersion}}
	}
	var out []Change
	adopting := !s.Env.ours(in)
	for _, other := range obs.Others {
		out = append(out, Change{Action: ActionUpdate, Object: other.Name,
			Detail: "no longer this install's database: its labels are removed, and it is kept", Estimate: 30 * time.Second})
	}
	if in.State == "PENDING_CREATE" {
		return append(out, Change{Action: ActionOK, Object: in.Name, Detail: "being created"})
	}
	diffs, restarts := differences(in, s.want(in, in.Name))
	if adopting {
		diffs = append([]string{"adopted: labelled as this install's database; silod's reconcile starts report-only"}, diffs...)
	}
	if len(diffs) > 0 {
		detail := strings.Join(diffs, "; ")
		if restarts {
			detail += " (restarts the instance)"
		}
		est := 30 * time.Second
		if restarts {
			est = 5 * time.Minute
		}
		return append(out, Change{Action: ActionUpdate, Object: in.Name, Detail: detail, Estimate: est, Waiting: "Cloud SQL"})
	}
	if len(out) == 0 {
		return []Change{{Action: ActionOK, Object: in.Name, Detail: s.describe()}}
	}
	return out
}

// PlanRemove keeps the instance: it is all that remains of the tree's
// grants, policies and audit (install.md "silo uninstall").
func (s *DBInstance) PlanRemove(o Observed) []Change {
	obs := o.(*DBInstanceObserved)
	if obs.Instance == nil || !s.Env.ours(obs.Instance) {
		return []Change{{Action: ActionOK, Detail: "no database instance"}}
	}
	return []Change{{Action: ActionKeep, Object: obs.Instance.Name,
		Detail: "kept, with its deletion protection and backups (--delete-data deletes it)"}}
}

func (s *DBInstance) Apply(ctx context.Context, changes []Change) error {
	sql := s.Env.Cloud.SQLAdmin
	project := s.Env.Config.Project
	for _, ch := range changes {
		switch ch.Action {
		case ActionCreate:
			suffix := newSuffix
			if s.Env.NewSuffix != nil {
				suffix = s.Env.NewSuffix
			}
			w := s.want(nil, DBInstancePrefix+suffix())
			w.Labels[SchemaLabel] = "0"
			if err := sql.InsertInstance(ctx, project, w); err != nil {
				return fmt.Errorf("creating %s: %w", w.Name, err)
			}
		case ActionUpdate:
			in, err := sql.GetInstance(ctx, project, ch.Object)
			if err != nil {
				return err
			}
			var w *SQLInstance
			switch {
			case in.Name == s.Env.Config.DBInstance || s.Env.Config.DBInstance == "" && s.Env.ours(in):
				adopting := !s.Env.ours(in)
				w = s.want(in, in.Name)
				if adopting {
					w.Labels[RestoredLabel] = "pending"
				}
			default:
				// An earlier database of this install's, replaced by the
				// one adopted: no longer found by its labels, and kept.
				w = in
				w.Labels = merge(in.Labels, nil)
				delete(w.Labels, LabelKey)
				delete(w.Labels, RoleLabel)
				delete(w.Labels, SchemaLabel)
			}
			if err := sql.PatchInstance(ctx, project, w); err != nil {
				return fmt.Errorf("updating %s: %w", in.Name, err)
			}
		}
	}
	return nil
}

// DB is step 11a, db: it waits for the instance, then creates the
// database silo and silod's IAM database user, silod@<project>.iam,
// with the cloudsqlsuperuser role, which owns databases the Admin API
// creates (state.md "Authentication: IAM, no password", SV-51). No
// password is set for anyone: not for silod, which logs in with its
// OAuth token, and not for the built-in postgres user, which therefore
// can't log in at all.
type DB struct{ Env *Env }

// DBObserved is the instance's state, databases and users; the last two
// only once it is running.
type DBObserved struct {
	SQLEnabled bool      `json:"sqlEnabled"`
	Instance   string    `json:"instance,omitempty"`
	State      string    `json:"state,omitempty"`
	Databases  []string  `json:"databases,omitempty"`
	Users      []SQLUser `json:"users,omitempty"`
}

func (s *DB) Name() string          { return "db" }
func (s *DB) NewObserved() Observed { return &DBObserved{} }

func (s *DB) Observe(ctx context.Context) (Observed, error) {
	on, in, _, err := observeDB(ctx, s.Env)
	obs := &DBObserved{SQLEnabled: on}
	if err != nil || in == nil {
		return obs, err
	}
	obs.Instance, obs.State = in.Name, in.State
	if in.State != "RUNNABLE" {
		return obs, nil
	}
	sql, project := s.Env.Cloud.SQLAdmin, s.Env.Config.Project
	if obs.Databases, err = sql.ListDatabases(ctx, project, in.Name); err != nil {
		return nil, err
	}
	sort.Strings(obs.Databases)
	if obs.Users, err = sql.ListUsers(ctx, project, in.Name); err != nil {
		return nil, err
	}
	sort.Slice(obs.Users, func(i, j int) bool { return obs.Users[i].Name < obs.Users[j].Name })
	return obs, nil
}

func (s *DB) Plan(o Observed) []Change {
	obs := o.(*DBObserved)
	user := DBUser(s.Env.Config.Project)
	name := obs.Instance
	if name == "" {
		name = DBInstancePrefix + "<suffix>"
	}
	if obs.State != "RUNNABLE" {
		c := Change{Action: ActionCreate, Detail: "database " + DBName + ", IAM user " + user}
		if obs.State != "" {
			c.Detail += "\n(waits for " + name + ", " + strings.ToLower(obs.State) + ")"
		}
		c.Estimate, c.Waiting = 10*time.Minute, "Cloud SQL"
		return []Change{c}
	}
	var out []Change
	if !contains(obs.Databases, DBName) {
		out = append(out, Change{Action: ActionCreate, Detail: "database " + DBName + " in " + name, Estimate: 20 * time.Second})
	}
	found := false
	for _, u := range obs.Users {
		if u.Name != user {
			continue
		}
		found = true
		if u.Type != "CLOUD_IAM_SERVICE_ACCOUNT" {
			out = append(out, Change{Action: ActionFail, Object: user, Detail: "exists as a " + u.Type + " user, not an IAM service account",
				Why: "silod logs in with IAM, never a password",
				Fix: fmt.Sprintf("gcloud sql users delete %s --instance %s --project %s, then install again", user, name, s.Env.Config.Project)})
		}
	}
	if !found {
		out = append(out, Change{Action: ActionCreate,
			Detail: "IAM user " + user + ", " + SuperuserRole + " (owns " + DBName + "), no password", Estimate: 20 * time.Second})
	}
	if len(out) == 0 {
		return []Change{{Action: ActionOK, Detail: "database " + DBName + ", IAM user " + user}}
	}
	return out
}

// RefusedRoleError is the Admin API refusing to create the IAM user with
// the cloudsqlsuperuser role (open question 19). Install doesn't fall
// back to a password.
type RefusedRoleError struct {
	User string
	Err  error
}

func (e *RefusedRoleError) Error() string {
	return fmt.Sprintf("Cloud SQL refused to create the IAM user %s with the %s role: %v. "+
		"Install makes silod the owner of its database only through the Admin API, and won't fall back to "+
		"a password for the built-in postgres user (open question 19); nothing was created with a password",
		e.User, SuperuserRole, e.Err)
}

func (e *RefusedRoleError) Unwrap() error { return e.Err }

func (s *DB) Apply(ctx context.Context, changes []Change) error {
	sql, project := s.Env.Cloud.SQLAdmin, s.Env.Config.Project
	_, in, _, err := observeDB(ctx, s.Env)
	if err != nil {
		return err
	}
	if in == nil {
		return fmt.Errorf("no database instance: the db-instance step creates it")
	}
	name := in.Name
	if in, err = sql.WaitInstance(ctx, project, name); err != nil {
		return fmt.Errorf("waiting for %s: %w", name, err)
	}
	user := DBUser(project)
	// Observed again now that the instance is running: the plan may
	// have been made while it was being created.
	dbs, err := sql.ListDatabases(ctx, project, in.Name)
	if err != nil {
		return err
	}
	if !contains(dbs, DBName) {
		if err := sql.InsertDatabase(ctx, project, in.Name, DBName); err != nil && !IsAlreadyExists(err) {
			return fmt.Errorf("creating database %s: %w", DBName, err)
		}
	}
	users, err := sql.ListUsers(ctx, project, in.Name)
	if err != nil {
		return err
	}
	for _, u := range users {
		if u.Name == user {
			return nil
		}
	}
	err = sql.InsertUser(ctx, project, in.Name, &SQLUser{Name: user, Type: "CLOUD_IAM_SERVICE_ACCOUNT", DatabaseRoles: []string{SuperuserRole}})
	if err != nil && !IsAlreadyExists(err) {
		return &RefusedRoleError{User: user, Err: err}
	}
	return nil
}
