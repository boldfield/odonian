package tuiview

import (
	"fmt"
	"time"
)

// FormatLeaseCountdown formats the lease expiration time as a countdown showing time remaining.
func FormatLeaseCountdown(expiresAt *string, now time.Time) string {
	if expiresAt == nil {
		return "no lease"
	}

	t, err := time.Parse(time.RFC3339Nano, *expiresAt)
	if err != nil {
		return "invalid"
	}

	if now.After(t) {
		return "EXPIRED"
	}

	remaining := t.Sub(now)
	hours := int(remaining.Hours())
	minutes := int(remaining.Minutes()) % 60
	seconds := int(remaining.Seconds()) % 60

	if hours > 0 {
		return fmt.Sprintf("%dh%dm%ds", hours, minutes, seconds)
	}
	if minutes > 0 {
		return fmt.Sprintf("%dm%ds", minutes, seconds)
	}
	return fmt.Sprintf("%ds", seconds)
}

// FormatTime formats a timestamp relative to now, showing elapsed time.
func FormatTime(timestamp string, now time.Time) string {
	if timestamp == "" {
		return "unknown"
	}

	t, err := time.Parse(time.RFC3339Nano, timestamp)
	if err != nil {
		return "unknown"
	}

	diff := now.Sub(t)
	if diff < 0 {
		diff = 0
	}

	if diff < time.Minute {
		return fmt.Sprintf("%ds", int(diff.Seconds()))
	}
	if diff < time.Hour {
		return fmt.Sprintf("%dm", int(diff.Minutes()))
	}
	if diff < 24*time.Hour {
		return fmt.Sprintf("%dh", int(diff.Hours()))
	}

	return fmt.Sprintf("%dd", int(diff.Hours()/24))
}

// FormatChecksDuration renders how long a task has been in checks, from checks_started_at.
func FormatChecksDuration(startedAt *string, now time.Time) string {
	if startedAt == nil {
		return "unknown"
	}

	t, err := time.Parse(time.RFC3339Nano, *startedAt)
	if err != nil {
		return "unknown"
	}

	d := now.Sub(t)
	if d < 0 {
		d = 0
	}

	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
	}
}
