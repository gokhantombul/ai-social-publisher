package approval

import (
	"errors"
	"testing"
	"time"
)

func TestValidateScheduleAt(t *testing.T) {
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	horizon := 30 * 24 * time.Hour

	t.Run("future within horizon is accepted and normalised to UTC", func(t *testing.T) {
		loc := time.FixedZone("UTC+3", 3*3600)
		at := now.Add(2 * time.Hour).In(loc)
		got, err := validateScheduleAt(now, at, horizon)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !got.Equal(at) {
			t.Fatalf("got %v, want equal to %v", got, at)
		}
		if got.Location() != time.UTC {
			t.Fatalf("expected UTC location, got %v", got.Location())
		}
	})

	for _, tc := range []struct {
		name string
		at   time.Time
	}{
		{"past is rejected", now.Add(-time.Minute)},
		{"equal to now is rejected", now},
		{"beyond horizon is rejected", now.Add(horizon + time.Hour)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := validateScheduleAt(now, tc.at, horizon); !errors.Is(err, ErrInvalidSchedule) {
				t.Fatalf("expected ErrInvalidSchedule, got %v", err)
			}
		})
	}
}
