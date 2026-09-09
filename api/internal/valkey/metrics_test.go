package valkey_test

import (
	"testing"
	"time"

	"github.com/RostislavDugin/managed-valkey/api/internal/valkey"
)

func TestParseMetricWindowUsesFixedRanges(t *testing.T) {
	now := time.Date(2026, 9, 9, 12, 34, 56, 0, time.FixedZone("test", 3*60*60))
	tests := []struct {
		name     string
		input    valkey.MetricsInput
		duration time.Duration
		step     time.Duration
	}{
		{name: "default", duration: time.Hour, step: time.Minute},
		{name: "5m", input: metricRangeInput("5m"), duration: 5 * time.Minute, step: 10 * time.Second},
		{name: "1h", input: metricRangeInput("1h"), duration: time.Hour, step: time.Minute},
		{name: "24h", input: metricRangeInput("24h"), duration: 24 * time.Hour, step: 5 * time.Minute},
		{name: "7d", input: metricRangeInput("7d"), duration: 7 * 24 * time.Hour, step: 30 * time.Minute},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			window, err := valkey.ParseMetricWindow(testCase.input, now)
			if err != nil {
				t.Fatalf("разобрать окно: %v", err)
			}
			if window.To.Sub(window.From) != testCase.duration || window.Step != testCase.step {
				t.Fatalf("неожиданное окно: %+v", window)
			}
			if !window.To.Equal(now.UTC().Truncate(testCase.step).Add(testCase.step)) {
				t.Fatalf("конец окна %s не выровнен по шагу %s", window.To, testCase.step)
			}
		})
	}
}

func TestParseMetricWindowUsesCustomRange(t *testing.T) {
	tests := []struct {
		name string
		from string
		to   string
		step time.Duration
	}{
		{
			name: "five minutes",
			from: "2026-09-09T09:00:00+03:00",
			to:   "2026-09-09T09:05:00+03:00",
			step: 10 * time.Second,
		},
		{name: "one hour", from: "2026-09-09T06:00:00Z", to: "2026-09-09T07:00:00Z", step: time.Minute},
		{name: "one day", from: "2026-09-08T06:00:00Z", to: "2026-09-09T06:00:00Z", step: 5 * time.Minute},
		{name: "seven days", from: "2026-09-02T06:00:00Z", to: "2026-09-09T06:00:00Z", step: 30 * time.Minute},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			window, err := valkey.ParseMetricWindow(metricCustomInput(testCase.from, testCase.to), time.Time{})
			if err != nil {
				t.Fatalf("разобрать окно: %v", err)
			}
			if window.Step != testCase.step || window.From.Location() != time.UTC || window.To.Location() != time.UTC {
				t.Fatalf("неожиданное окно: %+v", window)
			}
		})
	}
}

func TestParseMetricWindowRejectsInvalidInput(t *testing.T) {
	rangeValue := "5m"
	from := "2026-09-09T06:00:00Z"
	to := "2026-09-09T07:00:00Z"
	tests := []valkey.MetricsInput{
		{Range: stringPointer("unknown")},
		{Range: &rangeValue, From: &from, To: &to},
		{From: &from},
		{To: &to},
		metricCustomInput("invalid", to),
		metricCustomInput(to, from),
		metricCustomInput("2026-09-01T06:00:00Z", to),
	}

	for index, input := range tests {
		if _, err := valkey.ParseMetricWindow(input, time.Now()); err == nil {
			t.Errorf("вариант %d принят: %+v", index, input)
		}
	}
}

func metricRangeInput(value string) valkey.MetricsInput {
	return valkey.MetricsInput{Range: stringPointer(value)}
}

func metricCustomInput(from, to string) valkey.MetricsInput {
	return valkey.MetricsInput{From: stringPointer(from), To: stringPointer(to)}
}

func stringPointer(value string) *string {
	return &value
}
