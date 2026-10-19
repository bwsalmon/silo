package directory

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/bwsalmon/silo/hub/dbaccess"
	"github.com/bwsalmon/silo/hub/registry"
	"github.com/bwsalmon/silo/hub/rendezvous"
)

// tokenPrefix starts every node token, so a leaked one is recognisable
// to secret scanners and to the humans reading a log.
const tokenPrefix = "silo-node_"

// tokenShape is exactly what MintNodeToken makes: the prefix, the 8-byte
// ID in hex, and the 32-byte secret in unpadded base64url. Anything else
// is refused before it costs a database query (SA1-L10).
var tokenShape = regexp.MustCompile(`^` + tokenPrefix + `[0-9a-f]{16}_[A-Za-z0-9_-]{43}$`)

// NodeToken is a minted node token's record. The token itself is not
// in it: the store holds only its hash.
type NodeToken struct {
	ID      string     `json:"id"`
	Owner   string     `json:"owner"`
	Node    string     `json:"node"`
	Created time.Time  `json:"created"`
	Revoked *time.Time `json:"revoked,omitempty"`
	// Released is when the token's owner, or a hub admin, gave up the
	// node ID it was for, so that another owner may mint one for it
	// (ReleaseNodeID).
	Released *time.Time `json:"released,omitempty"`
}

func hashToken(tok string) string {
	h := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(h[:])
}

// MintNodeToken mints a token that proves owner, for the node ID node
// alone (docs/hub.md §5.2), and returns it: the only time it is ever
// shown. It revokes owner's earlier tokens for the same node, so
// signing a node in again retires the credential it had.
//
// A node ID belongs to the first owner to mint a token for it, for
// good: another owner's mint is refused (ErrNodeOwner) while any token
// of the first's for it, live or revoked, is unreleased, until they or
// a hub admin release the ID (ReleaseNodeID). A task records only the
// node ID and owner it ran on, and the registry forgets which owner an
// ID was on every hub restart, so an ID that changed hands with its
// owner's tokens revoked would be handed that owner's lost tasks
// (SA1-H1).
func (s *Store) MintNodeToken(ctx context.Context, owner, node string) (string, NodeToken, error) {
	tok, t, _, err := s.mintNodeToken(ctx, owner, node, false)
	return tok, t, err
}

// MintDeploymentToken mints a node token for deployment node. When forceOwner
// is true (the caller is an admin of deployment node or a Hub sys admin), any
// earlier token for node minted by a different admin is automatically revoked
// and released so the caller can mint a replacement token for node.
func (s *Store) MintDeploymentToken(ctx context.Context, owner, node string, forceOwner bool) (string, NodeToken, int64, error) {
	return s.mintNodeToken(ctx, owner, node, forceOwner)
}

// mintNodeToken is MintNodeToken, also saying how many earlier tokens
// it revoked, so the API can end the connection one of them holds.
func (s *Store) mintNodeToken(ctx context.Context, owner, node string, forceOwner bool) (string, NodeToken, int64, error) {
	o, err := normEmail(owner)
	if err != nil {
		return "", NodeToken{}, 0, err
	}
	if _, err := registry.Validate(registry.Node{ID: node}); err != nil {
		return "", NodeToken{}, 0, fmt.Errorf("%w node: %v", ErrInvalid, err)
	}
	var (
		revoked int64
		t       NodeToken
		tok     string
		errSame = errors.New("concurrent mint by same owner")
	)
	for range 32 {
		idb := make([]byte, 8)
		secret := make([]byte, 32)
		rand.Read(idb)
		rand.Read(secret)
		t = NodeToken{ID: hex.EncodeToString(idb), Owner: o, Node: node, Created: s.now().UTC().Truncate(time.Millisecond)}
		tok = tokenPrefix + t.ID + "_" + base64.RawURLEncoding.EncodeToString(secret)
		err = s.tx(ctx, func(tx *sql.Tx) error {
			revoked = 0
			if forceOwner {
				resOther, err := tx.ExecContext(ctx, s.q(`UPDATE node_tokens SET revoked_at = COALESCE(revoked_at, ?), released_at = ? WHERE node = ? AND owner <> ? AND released_at IS NULL`),
					t.Created.UnixMilli(), t.Created.UnixMilli(), node, o)
				if err != nil {
					return err
				}
				if nOther, err := resOther.RowsAffected(); err == nil {
					revoked += nOther
				}
			} else {
				var other string
				err := tx.QueryRowContext(ctx, s.q(`SELECT owner FROM node_tokens WHERE node = ? AND owner <> ? AND released_at IS NULL LIMIT 1`), node, o).Scan(&other)
				if err == nil {
					return fmt.Errorf("%w: %s", ErrNodeOwner, node)
				}
				if !errors.Is(err, sql.ErrNoRows) {
					return err
				}
			}
			res, err := tx.ExecContext(ctx, s.q(`UPDATE node_tokens SET revoked_at = ? WHERE node = ? AND owner = ? AND revoked_at IS NULL`),
				t.Created.UnixMilli(), node, o)
			if err != nil {
				return err
			}
			nOwn, err := res.RowsAffected()
			if err != nil {
				return err
			}
			revoked += nOwn
			_, err = tx.ExecContext(ctx, s.q(`INSERT INTO node_tokens (id, hash, owner, node, created_at) VALUES (?, ?, ?, ?, ?)`),
				t.ID, hashToken(tok), o, node, t.Created.UnixMilli())
			if isUnique(err) {
				return errSame
			}
			return err
		})
		if errors.Is(err, errSame) {
			continue
		}
		if err != nil {
			return "", NodeToken{}, 0, err
		}
		return tok, t, revoked, nil
	}
	return "", NodeToken{}, 0, fmt.Errorf("%w: %s", ErrNodeOwner, node)
}

// VerifyNodeToken is the owner and node a live token proves.
func (s *Store) VerifyNodeToken(ctx context.Context, tok string) (owner, node string, err error) {
	if !tokenShape.MatchString(tok) {
		return "", "", errors.New("directory: not a node token")
	}
	err = dbaccess.Retry(ctx, func() error {
		return s.db.QueryRowContext(ctx, s.q(`SELECT owner, node FROM node_tokens WHERE hash = ? AND revoked_at IS NULL`), hashToken(tok)).Scan(&owner, &node)
	})
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", errors.New("directory: unknown or revoked node token")
	}
	return owner, node, err
}

// Authenticate is rendezvous.Server.Authenticate: a node's bearer node
// token proves its owner and binds it to the node ID it was minted for.
func (s *Store) Authenticate(r *http.Request) (rendezvous.Identity, error) {
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return rendezvous.Identity{}, errors.New("directory: no bearer token")
	}
	owner, node, err := s.VerifyNodeToken(r.Context(), tok)
	if err != nil {
		return rendezvous.Identity{}, err
	}
	return rendezvous.Identity{Owner: owner, Node: node}, nil
}

// NodeTokens lists the tokens minted for owner, or every owner's when
// owner is "", newest first, revoked ones included.
func (s *Store) NodeTokens(ctx context.Context, owner string) ([]NodeToken, error) {
	q := `SELECT id, owner, node, created_at, revoked_at, released_at FROM node_tokens`
	var args []any
	if owner != "" {
		o, _ := normEmail(owner)
		q += ` WHERE owner = ?`
		args = append(args, o)
	}
	var out []NodeToken
	err := dbaccess.Retry(ctx, func() error {
		out = []NodeToken{}
		rows, err := s.db.QueryContext(ctx, s.q(q+` ORDER BY created_at DESC, id`), args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			t, err := scanToken(rows)
			if err != nil {
				return err
			}
			out = append(out, t)
		}
		return rows.Err()
	})
	return out, err
}

// NodeToken is the record of the token id.
func (s *Store) NodeToken(ctx context.Context, id string) (NodeToken, error) {
	var t NodeToken
	err := dbaccess.Retry(ctx, func() error {
		var err error
		t, err = scanToken(s.db.QueryRowContext(ctx, s.q(`SELECT id, owner, node, created_at, revoked_at, released_at FROM node_tokens WHERE id = ?`), id))
		return err
	})
	if errors.Is(err, sql.ErrNoRows) {
		return t, fmt.Errorf("%w: node token %s", ErrNotFound, id)
	}
	return t, err
}

func scanToken(r interface{ Scan(...any) error }) (NodeToken, error) {
	var (
		t                 NodeToken
		created           int64
		revoked, released sql.NullInt64
	)
	if err := r.Scan(&t.ID, &t.Owner, &t.Node, &created, &revoked, &released); err != nil {
		return NodeToken{}, err
	}
	t.Created = time.UnixMilli(created).UTC()
	for _, f := range []struct {
		v  sql.NullInt64
		to **time.Time
	}{{revoked, &t.Revoked}, {released, &t.Released}} {
		if f.v.Valid {
			at := time.UnixMilli(f.v.Int64).UTC()
			*f.to = &at
		}
	}
	return t, nil
}

// RevokeNodeToken revokes the token id: the next handshake that
// presents it is refused. (It does not end a connection the token
// already made; the API disconnects that node too.)
func (s *Store) RevokeNodeToken(ctx context.Context, id string) error {
	var n int64
	err := dbaccess.Retry(ctx, func() error {
		res, err := s.db.ExecContext(ctx, s.q(`UPDATE node_tokens SET revoked_at = ? WHERE id = ? AND revoked_at IS NULL`), s.now().UnixMilli(), id)
		if err != nil {
			return err
		}
		n, _ = res.RowsAffected()
		return nil
	})
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("%w: live node token %s", ErrNotFound, id)
	}
	return nil
}

// ReleaseNodeID gives up owner's claim to node, so that another owner
// may mint a token for it: owner's tokens for it are revoked and marked
// released. An owner of "" is a hub admin releasing the ID from every
// owner. It is ErrNotFound if there was nothing to release.
func (s *Store) ReleaseNodeID(ctx context.Context, owner, node string) error {
	now := s.now().UnixMilli()
	q := `UPDATE node_tokens SET revoked_at = COALESCE(revoked_at, ?), released_at = ? WHERE node = ? AND released_at IS NULL`
	args := []any{now, now, node}
	if owner != "" {
		o, err := normEmail(owner)
		if err != nil {
			return err
		}
		q += ` AND owner = ?`
		args = append(args, o)
	}
	var n int64
	err := dbaccess.Retry(ctx, func() error {
		res, err := s.db.ExecContext(ctx, s.q(q), args...)
		if err != nil {
			return err
		}
		n, _ = res.RowsAffected()
		return nil
	})
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("%w: a node ID %s to release", ErrNotFound, node)
	}
	return nil
}

// RevokeOwnerTokens revokes every live node token owner holds, for node
// only when node is non-empty, and returns the distinct node IDs whose
// tokens it revoked, so the caller can disconnect them. It is used when an
// owner loses the standing their tokens were minted on: removed from the
// accepted users list, or removed from the deployment the token is for.
// The tokens stay unreleased, so the owner's claim to a personal node ID
// is kept (an admin of a deployment may still mint over it).
func (s *Store) RevokeOwnerTokens(ctx context.Context, owner, node string) ([]string, error) {
	o, err := normEmail(strings.TrimPrefix(strings.ToLower(strings.TrimSpace(owner)), UserPrefix))
	if err != nil {
		return nil, err
	}
	where := ` WHERE owner = ? AND revoked_at IS NULL`
	args := []any{o}
	if node != "" {
		where += ` AND node = ?`
		args = append(args, node)
	}
	var nodes []string
	err = s.tx(ctx, func(tx *sql.Tx) error {
		nodes = nil
		rows, err := tx.QueryContext(ctx, s.q(`SELECT DISTINCT node FROM node_tokens`+where), args...)
		if err != nil {
			return err
		}
		for rows.Next() {
			var n string
			if err := rows.Scan(&n); err != nil {
				rows.Close()
				return err
			}
			nodes = append(nodes, n)
		}
		if err := rows.Close(); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, s.q(`UPDATE node_tokens SET revoked_at = ?`+where), append([]any{s.now().UnixMilli()}, args...)...)
		return err
	})
	if err != nil {
		return nil, err
	}
	return nodes, nil
}

// ReleaseNodeTokens revokes and releases every owner's tokens for node,
// returning how many it changed. It is used when the deployment node
// names is deleted, so no former admin's host stays connected as it and
// the name can be created again.
func (s *Store) ReleaseNodeTokens(ctx context.Context, node string) (int64, error) {
	now := s.now().UnixMilli()
	var n int64
	err := dbaccess.Retry(ctx, func() error {
		res, err := s.db.ExecContext(ctx, s.q(`UPDATE node_tokens SET revoked_at = COALESCE(revoked_at, ?), released_at = ? WHERE node = ? AND released_at IS NULL`), now, now, node)
		if err != nil {
			return err
		}
		n, _ = res.RowsAffected()
		return nil
	})
	return n, err
}
