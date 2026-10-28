package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

const briefingPurgeFloor = 24 * time.Hour

// PurgeBriefings deletes briefings (and their emptied done groups) created
// before cutoff. Separate from raw-event retention: briefing history may
// outlive the raw log.
func (s *Store) PurgeBriefings(ctx context.Context, now, cutoff time.Time) (int64, error) {
	if now.Sub(cutoff) < briefingPurgeFloor {
		return 0, fmt.Errorf("briefing purge cutoff must be at least %s old", briefingPurgeFloor)
	}
	var affected int64
	err := s.write(ctx, func(tx *sql.Tx) error {
		res, err := tx.Exec(`DELETE FROM briefings WHERE created_at < ?`, nanos(cutoff))
		if err != nil {
			return err
		}
		affected, _ = res.RowsAffected()
		_, err = tx.Exec(`DELETE FROM active_groups WHERE status = 'done' AND group_id NOT IN (SELECT group_id FROM briefings) AND group_id NOT IN (SELECT group_id FROM group_events)`)
		return err
	})
	if err != nil {
		return 0, err
	}
	return affected, nil
}

// SweepCandidates returns unforwarded events still fresh enough to forward
// (signature_timestamp within tolerance-margin); older ones are the caller's
// to mark via MarkStaleSweep.
const purgeFloor = time.Hour

// failedEventFloor keeps failed groups' events for debugging a week.
const failedEventFloor = 7 * 24 * time.Hour

// PurgeRawEvents deletes raw events received before cutoff, but only those
// whose group is done (or that belong to no group). Failed groups' events
// use a longer floor (failedEventFloor, 7 days) and the failed row is
// deleted once its events are gone. Briefings and groups are
// kept: they have their own retention (PurgeBriefings). The cutoff must be at
// least minAge old, so replay/sweep windows are never weakened.
func (s *Store) PurgeRawEvents(ctx context.Context, now, cutoff time.Time, minAge time.Duration) (int64, error) {
	if minAge < purgeFloor {
		minAge = purgeFloor
	}
	if now.Sub(cutoff) < minAge {
		return 0, fmt.Errorf("purge cutoff must be at least %s old", minAge)
	}
	var affected int64
	err := s.write(ctx, func(tx *sql.Tx) error {
		cut := nanos(cutoff)
		if _, err := tx.Exec(`DELETE FROM group_events
			WHERE group_id IN (SELECT group_id FROM active_groups WHERE status = 'done')
			  AND event_id IN (SELECT event_id FROM raw_events WHERE received_at < ?)`, cut); err != nil {
			return err
		}
		// Failed groups keep their events longer (debugging evidence):
		// their effective cutoff is at least failedEventFloor old.
		failedCut := cut
		if latest := nanos(now.Add(-failedEventFloor)); failedCut > latest {
			failedCut = latest
		}
		if _, err := tx.Exec(`DELETE FROM group_events
			WHERE group_id IN (SELECT group_id FROM active_groups WHERE status = 'failed')
			  AND event_id IN (SELECT event_id FROM raw_events WHERE received_at < ?)`, failedCut); err != nil {
			return err
		}
		res, err := tx.Exec(`DELETE FROM raw_events WHERE received_at < ? AND event_id NOT IN (SELECT event_id FROM group_events)`, cut)
		if err != nil {
			return err
		}
		affected, _ = res.RowsAffected()
		// Failed groups are deleted together with their purged events.
		_, err = tx.Exec(`DELETE FROM active_groups WHERE status = 'failed' AND deadline_at < ?
			AND NOT EXISTS (SELECT 1 FROM group_events WHERE group_events.group_id = active_groups.group_id)
			AND NOT EXISTS (SELECT 1 FROM briefings WHERE briefings.group_id = active_groups.group_id)`, failedCut)
		return err
	})
	if err != nil {
		return 0, err
	}
	return affected, nil
}
