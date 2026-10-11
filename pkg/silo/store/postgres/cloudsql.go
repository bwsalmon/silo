package postgres

import (
	"context"
	"fmt"
	"net"
	"net/url"

	"cloud.google.com/go/cloudsqlconn"
	"github.com/jackc/pgx/v5/pgconn"
)

// CloudSQL names the Cloud SQL database silod opens on a real install
// (docs/silo/state.md "Authentication: IAM, no password", SV-51).
type CloudSQL struct {
	// Instance is the instance's connection name,
	// <project>:<region>:<instance>.
	Instance string
	// Database is the database, silo.
	Database string
	// User is the IAM database user: silod's service account without
	// its .gserviceaccount.com, silod@<project>.iam.
	User string
}

// OpenCloudSQL opens the store on a Cloud SQL instance through the
// Cloud SQL Go connector, with IAM database login and the instance's
// Private Service Connect endpoint. The connector takes the process's
// OAuth token from the metadata server (Application Default
// Credentials) and refreshes it; the token is the password, so none is
// configured anywhere, and the connection is TLS to the instance's
// server certificate. It dials the instance's PSC DNS name, which the
// private zone silo-db resolves to the endpoint silo-db-psc.
//
// opts are applied after the connector's dial function, so a test can
// still replace it.
func OpenCloudSQL(ctx context.Context, c CloudSQL, opts ...Option) (*Store, error) {
	if c.Instance == "" || c.Database == "" || c.User == "" {
		return nil, fmt.Errorf("postgres store: Cloud SQL needs an instance, a database and a user (have %q, %q, %q)",
			c.Instance, c.Database, c.User)
	}
	d, err := cloudsqlconn.NewDialer(ctx,
		cloudsqlconn.WithIAMAuthN(),
		cloudsqlconn.WithDefaultDialOptions(cloudsqlconn.WithPSC()))
	if err != nil {
		return nil, fmt.Errorf("postgres store: the Cloud SQL connector: %w", err)
	}
	s, err := Open(ctx, cloudSQLConnString(c), append([]Option{WithDialFunc(cloudSQLDial(d, c.Instance))}, opts...)...)
	if err != nil {
		d.Close()
		return nil, err
	}
	s.onClose = d.Close
	return s, nil
}

// cloudSQLConnString is the pgx configuration for c: no host worth the
// name (the dial function ignores it), no password, and no TLS of
// pgx's own, since the connector's connection already is TLS.
func cloudSQLConnString(c CloudSQL) string {
	u := url.URL{Scheme: "postgres", User: url.User(c.User), Host: "cloudsql", Path: "/" + c.Database,
		RawQuery: "sslmode=disable"}
	return u.String()
}

// cloudSQLDial is a pgconn.DialFunc that ignores the address it is
// given and dials instance through d.
func cloudSQLDial(d *cloudsqlconn.Dialer, instance string) pgconn.DialFunc {
	return func(ctx context.Context, _, _ string) (net.Conn, error) {
		return d.Dial(ctx, instance)
	}
}
