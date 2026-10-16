package directory

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func TestTeamSettings(t *testing.T) {
	eachStore(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		check(t, s.CreateTeam(ctx, "infra", ""))

		// Off until a team admin turns it on.
		set, err := s.TeamSettings(ctx, "infra")
		check(t, err)
		if set.LostRequeueAfter != 0 {
			t.Errorf("default settings %+v, want requeueing off", set)
		}
		check(t, s.SetTeamSettings(ctx, "infra", Settings{LostRequeueAfter: 30 * time.Minute}))
		d, err := s.LostRequeueAfter(ctx, "infra")
		check(t, err)
		if d != 30*time.Minute {
			t.Errorf("LostRequeueAfter = %s, want 30m", d)
		}
		// Replaced, not added to.
		check(t, s.SetTeamSettings(ctx, "infra", Settings{LostRequeueAfter: time.Hour}))
		if set, _ := s.TeamSettings(ctx, "infra"); set.LostRequeueAfter != time.Hour {
			t.Errorf("after a second set: %+v", set)
		}

		if err := s.SetTeamSettings(ctx, "infra", Settings{LostRequeueAfter: time.Minute}); !errors.Is(err, ErrInvalid) {
			t.Errorf("a grace under %s: %v, want ErrInvalid", MinLostRequeueAfter, err)
		}
		if err := s.SetTeamSettings(ctx, "nope", Settings{}); !errors.Is(err, ErrNotFound) {
			t.Errorf("an unknown team: %v, want ErrNotFound", err)
		}
		if _, err := s.TeamSettings(ctx, "nope"); !errors.Is(err, ErrNotFound) {
			t.Errorf("an unknown team's settings: %v, want ErrNotFound", err)
		}

		// A deleted team's settings go with it: a team made again under
		// the same ID starts with the defaults.
		check(t, s.DeleteTeam(ctx, "infra"))
		if d, err := s.LostRequeueAfter(ctx, "infra"); err != nil || d != 0 {
			t.Errorf("a deleted team's grace: %s, %v", d, err)
		}
		check(t, s.CreateTeam(ctx, "infra", ""))
		if set, _ := s.TeamSettings(ctx, "infra"); set.LostRequeueAfter != 0 {
			t.Errorf("a re-made team's settings: %+v", set)
		}
	})
}

func TestSettingsJSON(t *testing.T) {
	b, err := json.Marshal(Settings{LostRequeueAfter: 10 * time.Minute})
	check(t, err)
	if string(b) != `{"lost_requeue_after_seconds":600}` {
		t.Errorf("marshalled %s", b)
	}
	var s Settings
	check(t, json.Unmarshal([]byte(`{"lost_requeue_after_seconds":900}`), &s))
	if s.LostRequeueAfter != 15*time.Minute {
		t.Errorf("unmarshalled %+v", s)
	}
	if err := json.Unmarshal([]byte(`{"lost_requeue_after":"15m"}`), &s); err == nil {
		t.Error("an unknown field was accepted")
	}
}
