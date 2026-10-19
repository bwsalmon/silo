package directory

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// MinLostRequeueAfter is the shortest grace a team may give a lost
// running task before the hub requeues it (docs/hub.md §6.3): long
// enough for every node to reconnect after the hub itself restarts,
// which marks every running task lost at once.
const MinLostRequeueAfter = 5 * time.Minute

// Settings are a team's own dispatch and submission settings, which its
// admins set.
type Settings struct {
	// LostRequeueAfter, when set, is how long a running task whose node
	// went away stays lost before the hub puts it back in the queue for
	// any node (docs/hub.md §6.3). Zero, the default, leaves a lost
	// task with its node until the node resumes it or a human requeues
	// it. JSON carries it in seconds.
	LostRequeueAfter time.Duration `json:"-"`
	// AnyoneMayPropose controls whether signed-in users outside the team
	// (or viewers on the team) may submit task proposals to the team or
	// its repositories. True by default when a team has set no settings.
	AnyoneMayPropose bool `json:"-"`
	// RequireApproval controls whether tasks submitted by members or
	// admins of the team are filed as proposed (requiring approval from a
	// different member or admin) rather than queued immediately. False by
	// default.
	RequireApproval bool `json:"-"`
}

type settingsJSON struct {
	LostRequeueAfterSeconds int64 `json:"lost_requeue_after_seconds"`
	AnyoneMayPropose        bool  `json:"anyone_may_propose"`
	RequireApproval         bool  `json:"require_approval"`
}

// MarshalJSON writes the durations in seconds and includes the submission
// policy flags.
func (s Settings) MarshalJSON() ([]byte, error) {
	return json.Marshal(settingsJSON{
		LostRequeueAfterSeconds: int64(s.LostRequeueAfter / time.Second),
		AnyoneMayPropose:        s.AnyoneMayPropose,
		RequireApproval:         s.RequireApproval,
	})
}

// UnmarshalJSON reads the durations in seconds and defaults
// anyone_may_propose to true when omitted from the JSON object.
func (s *Settings) UnmarshalJSON(b []byte) error {
	j := settingsJSON{AnyoneMayPropose: true}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&j); err != nil {
		return err
	}
	s.LostRequeueAfter = time.Duration(j.LostRequeueAfterSeconds) * time.Second
	s.AnyoneMayPropose = j.AnyoneMayPropose
	s.RequireApproval = j.RequireApproval
	return nil
}

func (s Settings) validate() error {
	if d := s.LostRequeueAfter; d != 0 && d < MinLostRequeueAfter {
		return fmt.Errorf("%w lost requeue grace %s: want 0 (off) or at least %s", ErrInvalid, d, MinLostRequeueAfter)
	}
	return nil
}

// TeamSettings returns a team's settings: the defaults for a team that
// has set none.
func (s *Store) TeamSettings(ctx context.Context, team string) (Settings, error) {
	if err := s.teamExists(ctx, s.db, team); err != nil {
		return Settings{}, err
	}
	return s.settings(ctx, team)
}

func (s *Store) settings(ctx context.Context, team string) (Settings, error) {
	var (
		secs             int64
		anyoneMayPropose int
		requireApproval  int
	)
	err := s.db.QueryRowContext(ctx, s.q(`SELECT lost_requeue_seconds, anyone_may_propose, require_approval FROM team_settings WHERE team = ?`), team).Scan(&secs, &anyoneMayPropose, &requireApproval)
	if errors.Is(err, sql.ErrNoRows) {
		return Settings{AnyoneMayPropose: true}, nil
	}
	if err != nil {
		return Settings{}, err
	}
	return Settings{
		LostRequeueAfter: time.Duration(secs) * time.Second,
		AnyoneMayPropose: anyoneMayPropose != 0,
		RequireApproval:  requireApproval != 0,
	}, nil
}

// SetTeamSettings replaces a team's settings.
func (s *Store) SetTeamSettings(ctx context.Context, team string, set Settings) error {
	if err := set.validate(); err != nil {
		return err
	}
	anyone := 0
	if set.AnyoneMayPropose {
		anyone = 1
	}
	reqApp := 0
	if set.RequireApproval {
		reqApp = 1
	}
	return s.tx(ctx, func(tx *sql.Tx) error {
		if err := s.teamExists(ctx, tx, team); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, s.q(`INSERT INTO team_settings (team, lost_requeue_seconds, anyone_may_propose, require_approval) VALUES (?, ?, ?, ?)
			ON CONFLICT (team) DO UPDATE SET
				lost_requeue_seconds = excluded.lost_requeue_seconds,
				anyone_may_propose = excluded.anyone_may_propose,
				require_approval = excluded.require_approval`),
			team, int64(set.LostRequeueAfter/time.Second), anyone, reqApp)
		return err
	})
}

// LostRequeueAfter is the team's Settings.LostRequeueAfter, for the
// dispatcher (tasks.Dispatcher.LostRequeueAfter): zero for a team that
// has not turned it on, or no longer exists.
func (s *Store) LostRequeueAfter(ctx context.Context, team string) (time.Duration, error) {
	set, err := s.settings(ctx, team)
	return set.LostRequeueAfter, err
}

// AllTeamSettings loads every team's explicit settings row from team_settings.
func (s *Store) AllTeamSettings(ctx context.Context) (map[string]Settings, error) {
	if s == nil {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT team, lost_requeue_seconds, anyone_may_propose, require_approval FROM team_settings`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]Settings{}
	for rows.Next() {
		var (
			team             string
			secs             int64
			anyoneMayPropose int
			requireApproval  int
		)
		if err := rows.Scan(&team, &secs, &anyoneMayPropose, &requireApproval); err != nil {
			return nil, err
		}
		out[team] = Settings{
			LostRequeueAfter: time.Duration(secs) * time.Second,
			AnyoneMayPropose: anyoneMayPropose != 0,
			RequireApproval:  requireApproval != 0,
		}
	}
	return out, rows.Err()
}

// TeamSettingsFrom returns team's settings from m, or the default
// (AnyoneMayPropose: true) when m has no entry for team.
func TeamSettingsFrom(m map[string]Settings, team string) Settings {
	if set, ok := m[team]; ok {
		return set
	}
	return Settings{AnyoneMayPropose: true}
}

// Resolved user theme values stored in hub/directory and propagated to
// each deployment's proxied Grain UI.
const (
	ThemeLight = "light"
	ThemeDark  = "dark"
)

// UserPreferences are a signed-in user's persisted Hub preferences:
// their resolved theme ("light" | "dark") and selected Unified Inbox
// deployments.
type UserPreferences struct {
	Theme           string   `json:"theme"`
	InboxNodes      []string `json:"inbox_nodes"`
	InboxConfigured bool     `json:"inbox_configured"`
}

func normalizeTheme(raw string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case ThemeLight:
		return ThemeLight, nil
	case ThemeDark:
		return ThemeDark, nil
	default:
		return "", fmt.Errorf("%w theme %q: want %q or %q", ErrInvalid, raw, ThemeLight, ThemeDark)
	}
}

// UserPreferences returns email's persisted preferences, defaulting to
// ThemeLight and InboxConfigured=false when unset.
func (s *Store) UserPreferences(ctx context.Context, email string) (UserPreferences, error) {
	if s == nil {
		return UserPreferences{Theme: ThemeLight, InboxNodes: []string{}}, nil
	}
	e, err := normEmail(email)
	if err != nil {
		return UserPreferences{Theme: ThemeLight, InboxNodes: []string{}}, err
	}
	var (
		theme      string
		rawNodes   string
		configured int
	)
	err = s.db.QueryRowContext(ctx, s.q(`SELECT theme, inbox_nodes, inbox_configured FROM user_preferences WHERE email = ?`), e).Scan(&theme, &rawNodes, &configured)
	if errors.Is(err, sql.ErrNoRows) {
		return UserPreferences{Theme: ThemeLight, InboxNodes: []string{}, InboxConfigured: false}, nil
	}
	if err != nil {
		return UserPreferences{}, err
	}
	if theme != ThemeDark {
		theme = ThemeLight
	}
	var nodes []string
	if strings.TrimSpace(rawNodes) != "" {
		_ = json.Unmarshal([]byte(rawNodes), &nodes)
	}
	if nodes == nil {
		nodes = []string{}
	}
	return UserPreferences{
		Theme:           theme,
		InboxNodes:      nodes,
		InboxConfigured: configured != 0,
	}, nil
}

// SetUserTheme persists email's resolved theme ("light" or "dark").
func (s *Store) SetUserTheme(ctx context.Context, email, theme string) (string, error) {
	e, err := normEmail(email)
	if err != nil {
		return "", err
	}
	norm, err := normalizeTheme(theme)
	if err != nil {
		return "", err
	}
	_, err = s.db.ExecContext(ctx, s.q(`INSERT INTO user_preferences (email, theme, inbox_nodes, inbox_configured, updated_at) VALUES (?, ?, '[]', 0, ?)
		ON CONFLICT (email) DO UPDATE SET
			theme = excluded.theme,
			updated_at = excluded.updated_at`),
		e, norm, s.now().UnixMilli())
	if err != nil {
		return "", err
	}
	return norm, nil
}

// SetUserInboxNodes persists email's selected Unified Inbox deployment IDs
// and marks InboxConfigured true.
func (s *Store) SetUserInboxNodes(ctx context.Context, email string, nodes []string) ([]string, error) {
	e, err := normEmail(email)
	if err != nil {
		return nil, err
	}
	clean := make([]string, 0, len(nodes))
	seen := map[string]bool{}
	for _, id := range nodes {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		clean = append(clean, id)
	}
	raw, err := json.Marshal(clean)
	if err != nil {
		return nil, err
	}
	_, err = s.db.ExecContext(ctx, s.q(`INSERT INTO user_preferences (email, theme, inbox_nodes, inbox_configured, updated_at) VALUES (?, 'light', ?, 1, ?)
		ON CONFLICT (email) DO UPDATE SET
			inbox_nodes = excluded.inbox_nodes,
			inbox_configured = 1,
			updated_at = excluded.updated_at`),
		e, string(raw), s.now().UnixMilli())
	if err != nil {
		return nil, err
	}
	return clean, nil
}
