// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package controlplane

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/marcelocantos/claudia"
	"github.com/marcelocantos/sqlpipe/go/sqlpipe"
)

func planTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

// ReadPlanUsage reads only this owner's file under its writer lock.
func (p *plane) ReadPlanUsage() ([]claudia.PlanUsageObservation, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.db == nil {
		return nil, fmt.Errorf("control plane: closed")
	}
	return readPlanUsage(p.db)
}

func readPlanUsage(db *sqlpipe.Database) ([]claudia.PlanUsageObservation, error) {
	result, err := db.Query(`SELECT provider, status, reason, attempted_at, success_at, next_attempt_at, refusals, reading_json FROM claudia_plan_usage ORDER BY provider`)
	if err != nil {
		return nil, err
	}
	observations := make([]claudia.PlanUsageObservation, 0, len(result.Rows))
	for _, row := range result.Rows {
		if len(row) != 8 {
			return nil, fmt.Errorf("control plane: malformed plan usage row")
		}
		strings := make([]string, 8)
		for _, i := range []int{0, 1, 2, 3, 4, 5, 7} {
			value, ok := row[i].(string)
			if !ok {
				return nil, fmt.Errorf("control plane: malformed plan usage column %d", i)
			}
			strings[i] = value
		}
		refusals, ok := row[6].(int64)
		if !ok || refusals < 0 {
			return nil, fmt.Errorf("control plane: malformed plan refusal count")
		}
		observation := claudia.PlanUsageObservation{Provider: claudia.Provider(strings[0]), Status: claudia.PlanUsageStatus(strings[1]), Reason: strings[2], Refusals: int(refusals)}
		for i, destination := range map[int]*time.Time{3: &observation.AttemptedAt, 4: &observation.SuccessAt, 5: &observation.NextAttemptAt} {
			if strings[i] == "" {
				continue
			}
			parsed, err := time.Parse(time.RFC3339Nano, strings[i])
			if err != nil {
				return nil, fmt.Errorf("control plane: malformed plan time: %w", err)
			}
			*destination = parsed
		}
		if err := json.Unmarshal([]byte(strings[7]), &observation.Reading); err != nil {
			return nil, fmt.Errorf("control plane: malformed plan reading: %w", err)
		}
		observations = append(observations, observation)
	}
	return observations, nil
}

// ReservePlanUsage durably reserves a provider before any network request.
// A crashed process therefore cannot reset pacing by reopening the file.
func (p *plane) ReservePlanUsage(provider claudia.Provider, now, next time.Time) (previous claudia.PlanUsageObservation, allowed bool, err error) {
	err = p.withDB(func(db *sqlpipe.Database) error {
		rows, err := readPlanUsage(db)
		if err != nil {
			return err
		}
		previous.Provider = provider
		for _, row := range rows {
			if row.Provider == provider {
				previous = row
				break
			}
		}
		if now.Before(previous.NextAttemptAt) {
			return nil
		}
		pending := previous
		if pending.Status == "" {
			pending.Status, pending.Reason = claudia.PlanUsageUnavailable, "usage poll in progress"
		}
		pending.AttemptedAt, pending.NextAttemptAt = now, next
		if err := writePlanUsage(db, pending); err != nil {
			return err
		}
		allowed = true
		return nil
	})
	return
}

// CompletePlanUsage commits the polling outcome, last success and history
// together. A failure never writes a history point or retimestamps a success.
func (p *plane) CompletePlanUsage(observation claudia.PlanUsageObservation) error {
	return p.CommitFixturePlanUsage([]claudia.PlanUsageObservation{observation})
}

// CommitFixturePlanUsage writes an entire fixture generation in one owner transaction.
// The same commit path serves ordinary poll completions and history publication.
func (p *plane) CommitFixturePlanUsage(rows []claudia.PlanUsageObservation) error {
	return p.withDB(func(db *sqlpipe.Database) (err error) {
		if err = db.Exec("BEGIN IMMEDIATE"); err != nil {
			return err
		}
		defer func() {
			if err != nil {
				_ = db.Exec("ROLLBACK")
			}
		}()
		for _, observation := range rows {
			if err = writePlanUsage(db, observation); err != nil {
				return err
			}
			if observation.Status == claudia.PlanUsageAvailable && observation.Reading != nil {
				reading := *observation.Reading
				reading.RawBody = ""
				body, marshalErr := json.Marshal(reading)
				if marshalErr != nil {
					return marshalErr
				}
				err = ExecOwned(db, WriterClaudia, PlanUsageHistoryTable, `INSERT INTO claudia_plan_usage_history (provider, observed_at, reading_json) VALUES (?, ?, ?)
    ON CONFLICT(provider, observed_at) DO UPDATE SET reading_json = excluded.reading_json`, string(observation.Provider), planTime(observation.SuccessAt), string(body))
				if err != nil {
					return err
				}
			}
		}
		return db.Exec("COMMIT")
	})
}

func writePlanUsage(db *sqlpipe.Database, observation claudia.PlanUsageObservation) error {
	if observation.Provider == "" {
		return fmt.Errorf("control plane: plan usage missing provider")
	}
	reading := observation.Reading
	if reading != nil {
		copy := *reading
		copy.RawBody = ""
		reading = &copy
	}
	body, err := json.Marshal(reading)
	if err != nil {
		return err
	}
	return ExecOwned(db, WriterClaudia, PlanUsageTable, `INSERT INTO claudia_plan_usage
  (provider, status, reason, attempted_at, success_at, next_attempt_at, refusals, reading_json)
  VALUES (?, ?, ?, ?, ?, ?, ?, ?)
  ON CONFLICT(provider) DO UPDATE SET status = excluded.status, reason = excluded.reason,
  attempted_at = excluded.attempted_at, success_at = excluded.success_at,
  next_attempt_at = excluded.next_attempt_at, refusals = excluded.refusals, reading_json = excluded.reading_json`,
		string(observation.Provider), string(observation.Status), observation.Reason, planTime(observation.AttemptedAt), planTime(observation.SuccessAt), planTime(observation.NextAttemptAt), observation.Refusals, string(body))
}
