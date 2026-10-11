package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// PhoneLocation returns the cached location for one number.
func (s *Store) PhoneLocation(ctx context.Context, number string) (PhoneLocation, error) {
	number = strings.TrimSpace(number)
	var value PhoneLocation
	var updatedAt int64
	err := s.db.QueryRowContext(ctx, `
		SELECT number, province, city, sp, area_code, updated_at
		FROM phone_locations
		WHERE number = ?
	`, number).Scan(
		&value.Number, &value.Province, &value.City, &value.Sp, &value.AreaCode, &updatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return PhoneLocation{}, ErrNotFound
	}
	if err != nil {
		return PhoneLocation{}, fmt.Errorf("read phone location %q: %w", number, err)
	}
	value.UpdatedAt = time.Unix(updatedAt, 0).UTC()
	return value, nil
}

// PhoneLocations returns cached locations for a set of numbers. Unknown numbers
// are simply absent from the result.
func (s *Store) PhoneLocations(ctx context.Context, numbers []string) (map[string]PhoneLocation, error) {
	result := make(map[string]PhoneLocation, len(numbers))
	unique := make([]string, 0, len(numbers))
	seen := make(map[string]struct{}, len(numbers))
	for _, number := range numbers {
		number = strings.TrimSpace(number)
		if number == "" {
			continue
		}
		if _, ok := seen[number]; ok {
			continue
		}
		seen[number] = struct{}{}
		unique = append(unique, number)
	}
	if len(unique) == 0 {
		return result, nil
	}
	placeholders := make([]string, len(unique))
	args := make([]any, len(unique))
	for index, number := range unique {
		placeholders[index] = "?"
		args[index] = number
	}
	query := `SELECT number, province, city, sp, area_code, updated_at
		FROM phone_locations WHERE number IN (` + strings.Join(placeholders, ",") + `)`
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list phone locations: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var value PhoneLocation
		var updatedAt int64
		if err := rows.Scan(&value.Number, &value.Province, &value.City, &value.Sp, &value.AreaCode, &updatedAt); err != nil {
			return nil, fmt.Errorf("scan phone location: %w", err)
		}
		value.UpdatedAt = time.Unix(updatedAt, 0).UTC()
		result[value.Number] = value
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate phone locations: %w", err)
	}
	return result, nil
}

// UpsertPhoneLocation persists a number's location, overwriting an existing row.
func (s *Store) UpsertPhoneLocation(ctx context.Context, value PhoneLocation) error {
	value.Number = strings.TrimSpace(value.Number)
	if value.Number == "" {
		return errors.New("phone location number is required")
	}
	if value.Province == "" && value.City == "" && value.Sp == "" {
		return errors.New("phone location has no data")
	}
	updatedAt := value.UpdatedAt
	if updatedAt.IsZero() {
		updatedAt = time.Now().UTC()
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO phone_locations (number, province, city, sp, area_code, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(number) DO UPDATE SET
			province = excluded.province,
			city = excluded.city,
			sp = excluded.sp,
			area_code = excluded.area_code,
			updated_at = excluded.updated_at
	`,
		value.Number, value.Province, value.City, value.Sp, value.AreaCode, updatedAt.Unix(),
	)
	if err != nil {
		return fmt.Errorf("upsert phone location for %q: %w", value.Number, err)
	}
	return nil
}
