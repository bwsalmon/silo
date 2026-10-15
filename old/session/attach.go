package session

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/bwsalmon/silo/old/events"
)

// The model is grain's run host (grain pkg/runhost, docs/non-disruptive-
// upgrade.md): the session writes an append-only file, and whoever
// follows it keeps a cursor, so a caller that restarts picks up where it
// was with no gap and no repeat. Grain's cursor is a byte offset; here
// it is an event's seq, which the caller already has.

// ErrNoResult is a followed session whose events end without a result
// event: its process was killed (silo gc cleans up after it).
var ErrNoResult = errors.New("the session's process is gone and its events end without a result")

// EventsPath is session id's events.jsonl, under the state directory so
// that it outlives the session, as the audit log does.
func EventsPath(getenv func(string) string, id string) string {
	return filepath.Join(StateRoot(getenv), id, EventsFile)
}

// List returns the live sessions, whose process is running, oldest
// first: those carrying every label in labels, or all when it is empty.
func List(getenv func(string) string, labels map[string]string) ([]Record, error) {
	entries, err := os.ReadDir(RuntimeRoot(getenv))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	var out []Record
	for _, e := range entries {
		if !e.IsDir() || !ValidID(e.Name()) {
			continue
		}
		rec, err := ReadRecord(filepath.Join(RuntimeRoot(getenv), e.Name()))
		if err != nil || !alive(rec.PID) || rec.Result != nil || !hasLabels(rec.Labels, labels) {
			continue
		}
		out = append(out, rec)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Started.Before(out[j].Started) })
	return out, nil
}

func hasLabels(have, want map[string]string) bool {
	for k, v := range want {
		if got, ok := have[k]; !ok || got != v {
			return false
		}
	}
	return true
}

// FollowPoll is how often Follow looks for more events once it has read
// all there are.
var FollowPoll = 50 * time.Millisecond

// Follow calls fn with each event of session id whose seq is after
// from, in order, replaying what events.jsonl holds and then following
// it as the session goes on, until the result event, which fn also
// gets. A session that has ended is replayed to its result. It returns
// nil after the result, ErrNoResult when the session's process has gone
// without one, fn's error, or ctx's.
func Follow(ctx context.Context, getenv func(string) string, id string, from int64, fn func(events.Event) error) error {
	if !ValidID(id) {
		return fmt.Errorf("%q is not a session id", id)
	}
	// The process to watch: while it runs, more may come.
	pid := 0
	if rec, err := ReadRecord(filepath.Join(RuntimeRoot(getenv), id)); err == nil {
		pid = rec.PID
	}
	f, err := os.Open(EventsPath(getenv, id))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("no session %s: %s does not exist", id, EventsPath(getenv, id))
		}
		return err
	}
	defer f.Close()
	r := bufio.NewReader(f)
	var partial []byte
	last := from
	gone := false
	for {
		line, err := r.ReadBytes('\n')
		if err == nil {
			if len(partial) > 0 {
				line = append(partial, line...)
				partial = nil
			}
			line = bytes.TrimSpace(line)
			if len(line) == 0 {
				continue
			}
			var e events.Event
			if err := events.Unmarshal(line, &e); err != nil {
				return fmt.Errorf("%s: %w", EventsPath(getenv, id), err)
			}
			if e.Seq <= last {
				continue
			}
			last = e.Seq
			if err := fn(e); err != nil {
				return err
			}
			if e.Type == events.TypeResult {
				return nil
			}
			continue
		}
		if !errors.Is(err, io.EOF) {
			return err
		}
		// A line still being written is kept until its newline.
		partial = append(partial, line...)
		if gone {
			return ErrNoResult
		}
		if !alive(pid) {
			// Read once more: the last events may have landed between
			// the read and the check.
			gone = true
			continue
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(FollowPoll):
		}
	}
}
