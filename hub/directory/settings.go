package directory

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// MinLostRequeueAfter is the shortest grace a team may give a lost
// running task before the hub requeues it (docs/hub.md §6.3): long
// enough for every node to reconnect after the hub itself restarts,
// which marks every running task lost at once.
const MinLostRequeueAfter = 5 * time.Minute

// Settings are a team's own dispatch settings, which its admins set.
type Settings struct {
	// LostRequeueAfter, when set, is how long a running task whose node
	// went away stays lost before the hub puts it back in the queue for
	// any node (docs/hub.md §6.3). Zero, the default, leaves a lost
	// task with its node until the node resumes it or a human requeues
	// it. JSON carries it in seconds.
	LostRequeueAfter time.Duration `json:"-"`
}

type settingsJSON struct {
	LostRequeueAfterSeconds int64 `json:"lost_requeue_after_seconds"`
}

// MarshalJSON writes the durations in seconds.
func (s Settings) MarshalJSON() ([]byte, error) {
	return json.Marshal(settingsJSON{int64(s.LostRequeueAfter / time.Second)})
}

// UnmarshalJSON reads the durations in seconds.
func (s *Settings) UnmarshalJSON(b []byte) error {
	var j settingsJSON
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&j); err != nil {
		return err
	}
	s.LostRequeueAfter = time.Duration(j.LostRequeueAfterSeconds) * time.Second
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
	var secs int64
	err := s.db.QueryRowContext(ctx, s.q(`SELECT lost_requeue_seconds FROM team_settings WHERE team = ?`), team).Scan(&secs)
	if errors.Is(err, sql.ErrNoRows) {
		return Settings{}, nil
	}
	if err != nil {
		return Settings{}, err
	}
	return Settings{LostRequeueAfter: time.Duration(secs) * time.Second}, nil
}

// SetTeamSettings replaces a team's settings.
func (s *Store) SetTeamSettings(ctx context.Context, team string, set Settings) error {
	if err := set.validate(); err != nil {
		return err
	}
	return s.tx(ctx, func(tx *sql.Tx) error {
		if err := s.teamExists(ctx, tx, team); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, s.q(`DELETE FROM team_settings WHERE team = ?`), team); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, s.q(`INSERT INTO team_settings (team, lost_requeue_seconds) VALUES (?, ?)`),
			team, int64(set.LostRequeueAfter/time.Second))
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
