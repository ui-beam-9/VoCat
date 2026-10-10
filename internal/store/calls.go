package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	callDirectionIncoming = "incoming"
	callDirectionOutgoing = "outgoing"
	callOutcomeAnswered   = "answered"
	callOutcomeMissed     = "missed"
	callOutcomeFailed     = "failed"
)

// AppendCallHistory persists one finished call and returns the stored row.
func (s *Store) AppendCallHistory(ctx context.Context, entry CallHistoryEntry) (CallHistoryEntry, error) {
	entry.DeviceID = strings.TrimSpace(entry.DeviceID)
	entry.CallID = strings.TrimSpace(entry.CallID)
	entry.Number = strings.TrimSpace(entry.Number)
	entry.Direction = strings.TrimSpace(entry.Direction)
	entry.Outcome = strings.TrimSpace(entry.Outcome)
	if entry.Number == "" {
		return CallHistoryEntry{}, errors.New("call history number is required")
	}
	if entry.Direction != callDirectionIncoming && entry.Direction != callDirectionOutgoing {
		return CallHistoryEntry{}, errors.New("call history direction must be incoming or outgoing")
	}
	if entry.Outcome != callOutcomeAnswered && entry.Outcome != callOutcomeMissed && entry.Outcome != callOutcomeFailed {
		return CallHistoryEntry{}, errors.New("call history outcome must be answered, missed, or failed")
	}
	now := time.Now().UTC()
	if entry.StartedAt.IsZero() {
		entry.StartedAt = now
	}
	if entry.EndedAt.IsZero() {
		entry.EndedAt = now
	}
	if entry.DurationSeconds < 0 {
		entry.DurationSeconds = 0
	}
	// 同一通 IMS 通话只保留一条：先查已有记录，命中则直接返回。
	if entry.CallID != "" {
		if existing, err := s.callHistoryByCallID(ctx, entry.CallID); err == nil {
			return existing, nil
		} else if !errors.Is(err, ErrNotFound) {
			return CallHistoryEntry{}, err
		}
	}
	result, err := s.db.ExecContext(ctx, `
		INSERT INTO call_history (
			call_id, device_id, number, direction, outcome, started_at, ended_at, duration_seconds, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		entry.CallID,
		entry.DeviceID,
		entry.Number,
		entry.Direction,
		entry.Outcome,
		entry.StartedAt.Unix(),
		entry.EndedAt.Unix(),
		entry.DurationSeconds,
		now.Unix(),
	)
	if err != nil {
		// 并发下唯一索引兜底：冲突则回读已有记录。
		if entry.CallID != "" && strings.Contains(err.Error(), "UNIQUE constraint failed") {
			if existing, lookupErr := s.callHistoryByCallID(ctx, entry.CallID); lookupErr == nil {
				return existing, nil
			}
		}
		return CallHistoryEntry{}, fmt.Errorf("append call history for %q: %w", entry.Number, err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return CallHistoryEntry{}, fmt.Errorf("append call history id for %q: %w", entry.Number, err)
	}
	entry.ID = id
	entry.CreatedAt = now
	return entry, nil
}

func (s *Store) callHistoryByCallID(ctx context.Context, callID string) (CallHistoryEntry, error) {
	var entry CallHistoryEntry
	var startedAt, endedAt, createdAt int64
	err := s.db.QueryRowContext(ctx, `
		SELECT id, call_id, device_id, number, direction, outcome, started_at, ended_at, duration_seconds, created_at
		FROM call_history
		WHERE call_id = ?
	`, strings.TrimSpace(callID)).Scan(
		&entry.ID,
		&entry.CallID,
		&entry.DeviceID,
		&entry.Number,
		&entry.Direction,
		&entry.Outcome,
		&startedAt,
		&endedAt,
		&entry.DurationSeconds,
		&createdAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return CallHistoryEntry{}, ErrNotFound
	}
	if err != nil {
		return CallHistoryEntry{}, err
	}
	entry.StartedAt = time.Unix(startedAt, 0).UTC()
	entry.EndedAt = time.Unix(endedAt, 0).UTC()
	entry.CreatedAt = time.Unix(createdAt, 0).UTC()
	return entry, nil
}

// ListCallHistory returns the newest entries first. An empty deviceID lists
// every device; limit is clamped to a sane range.
func (s *Store) ListCallHistory(ctx context.Context, deviceID string, limit int) ([]CallHistoryEntry, error) {
	deviceID = strings.TrimSpace(deviceID)
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	query := `SELECT id, call_id, device_id, number, direction, outcome, started_at, ended_at, duration_seconds, created_at
		FROM call_history`
	args := make([]any, 0, 2)
	if deviceID != "" {
		query += ` WHERE device_id = ?`
		args = append(args, deviceID)
	}
	query += ` ORDER BY started_at DESC, id DESC LIMIT ?`
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list call history: %w", err)
	}
	defer rows.Close()

	entries := make([]CallHistoryEntry, 0)
	for rows.Next() {
		var entry CallHistoryEntry
		var startedAt, endedAt, createdAt int64
		if err := rows.Scan(
			&entry.ID,
			&entry.CallID,
			&entry.DeviceID,
			&entry.Number,
			&entry.Direction,
			&entry.Outcome,
			&startedAt,
			&endedAt,
			&entry.DurationSeconds,
			&createdAt,
		); err != nil {
			return nil, fmt.Errorf("scan call history: %w", err)
		}
		entry.StartedAt = time.Unix(startedAt, 0).UTC()
		entry.EndedAt = time.Unix(endedAt, 0).UTC()
		entry.CreatedAt = time.Unix(createdAt, 0).UTC()
		entries = append(entries, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate call history: %w", err)
	}
	return entries, nil
}

// ClearCallHistory deletes the history. An empty deviceID clears every device.
func (s *Store) ClearCallHistory(ctx context.Context, deviceID string) error {
	deviceID = strings.TrimSpace(deviceID)
	var err error
	if deviceID == "" {
		_, err = s.db.ExecContext(ctx, `DELETE FROM call_history`)
	} else {
		_, err = s.db.ExecContext(ctx, `DELETE FROM call_history WHERE device_id = ?`, deviceID)
	}
	if err != nil {
		return fmt.Errorf("clear call history: %w", err)
	}
	return nil
}

// UpsertContact saves a contact, updating the name when the number already
// exists, and returns the stored row.
func (s *Store) UpsertContact(ctx context.Context, value Contact) (Contact, error) {
	value.Name = strings.TrimSpace(value.Name)
	value.Number = strings.TrimSpace(value.Number)
	if value.Name == "" {
		return Contact{}, errors.New("contact name is required")
	}
	if value.Number == "" {
		return Contact{}, errors.New("contact number is required")
	}
	now := time.Now().UTC()
	createdAt := value.CreatedAt
	if createdAt.IsZero() {
		createdAt = now
	}
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO contacts (name, number, created_at, updated_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(number) DO UPDATE SET
			name = excluded.name,
			updated_at = excluded.updated_at
	`,
		value.Name,
		value.Number,
		createdAt.Unix(),
		now.Unix(),
	); err != nil {
		return Contact{}, fmt.Errorf("upsert contact for %q: %w", value.Number, err)
	}
	return s.contactByNumber(ctx, value.Number)
}

func (s *Store) contactByNumber(ctx context.Context, number string) (Contact, error) {
	var value Contact
	var createdAt, updatedAt int64
	err := s.db.QueryRowContext(ctx, `
		SELECT id, name, number, created_at, updated_at
		FROM contacts
		WHERE number = ?
	`, strings.TrimSpace(number)).Scan(&value.ID, &value.Name, &value.Number, &createdAt, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Contact{}, ErrNotFound
	}
	if err != nil {
		return Contact{}, fmt.Errorf("read contact %q: %w", number, err)
	}
	value.CreatedAt = time.Unix(createdAt, 0).UTC()
	value.UpdatedAt = time.Unix(updatedAt, 0).UTC()
	return value, nil
}

// ListContacts returns the saved address book ordered by name.
func (s *Store) ListContacts(ctx context.Context) ([]Contact, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, name, number, created_at, updated_at
		FROM contacts
		ORDER BY name COLLATE NOCASE ASC, id ASC
	`)
	if err != nil {
		return nil, fmt.Errorf("list contacts: %w", err)
	}
	defer rows.Close()

	values := make([]Contact, 0)
	for rows.Next() {
		var value Contact
		var createdAt, updatedAt int64
		if err := rows.Scan(&value.ID, &value.Name, &value.Number, &createdAt, &updatedAt); err != nil {
			return nil, fmt.Errorf("scan contact: %w", err)
		}
		value.CreatedAt = time.Unix(createdAt, 0).UTC()
		value.UpdatedAt = time.Unix(updatedAt, 0).UTC()
		values = append(values, value)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate contacts: %w", err)
	}
	return values, nil
}
