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
}

func hashToken(tok string) string {
	h := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(h[:])
}

// MintNodeToken mints a token that proves owner, for the node ID node
// alone (docs/hub.md §5.2), and returns it: the only time it is ever
// shown. It revokes owner's earlier tokens for the same node, so
// signing a node in again retires the credential it had. A node ID
// another owner holds a live token for is refused (ErrNodeOwner): node
// IDs stay bound to their owner across hub restarts, which the
// in-memory registry alone would forget.
func (s *Store) MintNodeToken(ctx context.Context, owner, node string) (string, NodeToken, error) {
	tok, t, _, err := s.mintNodeToken(ctx, owner, node)
	return tok, t, err
}

// mintNodeToken is MintNodeToken, also saying how many earlier tokens
// it revoked, so the API can end the connection one of them holds.
func (s *Store) mintNodeToken(ctx context.Context, owner, node string) (string, NodeToken, int64, error) {
	o, err := normEmail(owner)
	if err != nil {
		return "", NodeToken{}, 0, err
	}
	if _, err := registry.Validate(registry.Node{ID: node}); err != nil {
		return "", NodeToken{}, 0, fmt.Errorf("%w node: %v", ErrInvalid, err)
	}
	var revoked int64
	idb := make([]byte, 8)
	secret := make([]byte, 32)
	rand.Read(idb)
	rand.Read(secret)
	t := NodeToken{ID: hex.EncodeToString(idb), Owner: o, Node: node, Created: s.now().UTC().Truncate(time.Millisecond)}
	tok := tokenPrefix + t.ID + "_" + base64.RawURLEncoding.EncodeToString(secret)
	err = s.tx(ctx, func(tx *sql.Tx) error {
		var other string
		err := tx.QueryRowContext(ctx, s.q(`SELECT owner FROM node_tokens WHERE node = ? AND owner <> ? AND revoked_at IS NULL LIMIT 1`), node, o).Scan(&other)
		if err == nil {
			return fmt.Errorf("%w: %s", ErrNodeOwner, node)
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		res, err := tx.ExecContext(ctx, s.q(`UPDATE node_tokens SET revoked_at = ? WHERE node = ? AND owner = ? AND revoked_at IS NULL`),
			t.Created.UnixMilli(), node, o)
		if err != nil {
			return err
		}
		if revoked, err = res.RowsAffected(); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, s.q(`INSERT INTO node_tokens (id, hash, owner, node, created_at) VALUES (?, ?, ?, ?, ?)`),
			t.ID, hashToken(tok), o, node, t.Created.UnixMilli())
		if isUnique(err) { // another owner's mint for this node won a race
			return fmt.Errorf("%w: %s", ErrNodeOwner, node)
		}
		return err
	})
	if err != nil {
		return "", NodeToken{}, 0, err
	}
	return tok, t, revoked, nil
}

// VerifyNodeToken is the owner and node a live token proves.
func (s *Store) VerifyNodeToken(ctx context.Context, tok string) (owner, node string, err error) {
	if !tokenShape.MatchString(tok) {
		return "", "", errors.New("directory: not a node token")
	}
	err = s.db.QueryRowContext(ctx, s.q(`SELECT owner, node FROM node_tokens WHERE hash = ? AND revoked_at IS NULL`), hashToken(tok)).Scan(&owner, &node)
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
	q := `SELECT id, owner, node, created_at, revoked_at FROM node_tokens`
	var args []any
	if owner != "" {
		o, _ := normEmail(owner)
		q += ` WHERE owner = ?`
		args = append(args, o)
	}
	rows, err := s.db.QueryContext(ctx, s.q(q+` ORDER BY created_at DESC, id`), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []NodeToken{}
	for rows.Next() {
		var (
			t       NodeToken
			created int64
			revoked sql.NullInt64
		)
		if err := rows.Scan(&t.ID, &t.Owner, &t.Node, &created, &revoked); err != nil {
			return nil, err
		}
		t.Created = time.UnixMilli(created).UTC()
		if revoked.Valid {
			r := time.UnixMilli(revoked.Int64).UTC()
			t.Revoked = &r
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// NodeToken is the record of the token id.
func (s *Store) NodeToken(ctx context.Context, id string) (NodeToken, error) {
	var (
		t       NodeToken
		created int64
		revoked sql.NullInt64
	)
	err := s.db.QueryRowContext(ctx, s.q(`SELECT id, owner, node, created_at, revoked_at FROM node_tokens WHERE id = ?`), id).
		Scan(&t.ID, &t.Owner, &t.Node, &created, &revoked)
	if errors.Is(err, sql.ErrNoRows) {
		return t, fmt.Errorf("%w: node token %s", ErrNotFound, id)
	}
	t.Created = time.UnixMilli(created).UTC()
	if revoked.Valid {
		r := time.UnixMilli(revoked.Int64).UTC()
		t.Revoked = &r
	}
	return t, err
}

// RevokeNodeToken revokes the token id: the next handshake that
// presents it is refused. (It does not end a connection the token
// already made; the API disconnects that node too.)
func (s *Store) RevokeNodeToken(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, s.q(`UPDATE node_tokens SET revoked_at = ? WHERE id = ? AND revoked_at IS NULL`), s.now().UnixMilli(), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: live node token %s", ErrNotFound, id)
	}
	return nil
}
