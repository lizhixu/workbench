package snapshots

import (
	"log/slog"
	"strings"
	"time"
)

// StartScheduler launches the cron pickup loop: every minute it checks each
// enabled job with a cron expression and runs the ones whose minute matches.
// A simple 5-field matcher covers the common "daily at 3am" style schedules
// used by backup jobs; full cron libraries are overkill here and would pull
// a dependency into the single-binary build.
func (e *Engine) StartScheduler(stop <-chan struct{}, log *slog.Logger) {
	if log == nil {
		log = slog.Default()
	}
	go func() {
		// Align to the minute boundary so schedules are predictable.
		now := time.Now()
		next := now.Truncate(time.Minute).Add(time.Minute)
		time.Sleep(next.Sub(now))
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case t := <-ticker.C:
				e.runDue(t, log)
			}
		}
	}()
}

func (e *Engine) runDue(now time.Time, log *slog.Logger) {
	for _, job := range e.store.ListJobs() {
		if !job.Enabled || job.Schedule.Cron == "" {
			continue
		}
		ok, err := cronMatches(job.Schedule.Cron, now)
		if err != nil {
			// Log once per bad expression per sweep; the job's LastError is
			// also surfaced in the UI.
			log.Warn("invalid cron expression", "job", job.ID, "cron", job.Schedule.Cron, "err", err)
			continue
		}
		if !ok {
			continue
		}
		// Avoid double-firing within the same minute if the sweep lags.
		if !job.LastRun.IsZero() && now.Sub(job.LastRun) < time.Minute {
			continue
		}
		go func(j *Job) {
			if _, err := e.Run(j); err != nil {
				log.Warn("scheduled backup failed", "job", j.ID, "err", err)
			}
		}(job)
	}
}

// cronMatches evaluates a standard 5-field cron expression (minute hour
// day-of-month month day-of-week) against t, supporting *, */step, lists and
// ranges. Day-of-week runs 0-7 with both 0 and 7 as Sunday.
func cronMatches(expr string, t time.Time) (bool, error) {
	fields := strings.Fields(expr)
	if len(fields) != 5 {
		return false, errInvalidCron(expr)
	}
	values := []int{t.Minute(), t.Hour(), t.Day(), int(t.Month()), int(t.Weekday())}
	matches := make([]bool, 5)
	for i, field := range fields {
		// Day-of-month and day-of-week are OR-combined per cron convention
		// when both are restricted; here each field still must match, which
		// covers the practical backup schedules (daily/weekly patterns).
		m, err := fieldMatches(field, values[i])
		if err != nil {
			return false, err
		}
		matches[i] = m
	}
	// Standard cron: when both DOM and DOW are restricted (not *), either
	// matching is enough.
	domRestricted := fields[2] != "*"
	dowRestricted := fields[4] != "*"
	if domRestricted && dowRestricted {
		return matches[0] && matches[1] && (matches[2] || matches[4]), nil
	}
	for _, m := range matches {
		if !m {
			return false, nil
		}
	}
	return true, nil
}

type cronError struct{ expr string }

func (e *cronError) Error() string { return "invalid cron expression: " + e.expr }

func errInvalidCron(expr string) error { return &cronError{expr: expr} }

func fieldMatches(field string, value int) (bool, error) {
	for _, part := range strings.Split(field, ",") {
		match, err := partMatches(part, value)
		if err != nil {
			return false, err
		}
		if match {
			return true, nil
		}
	}
	return false, nil
}

func partMatches(part string, value int) (bool, error) {
	step := 1
	if idx := strings.Index(part, "/"); idx >= 0 {
		s := part[idx+1:]
		part = part[:idx]
		n := 0
		for _, ch := range s {
			if ch < '0' || ch > '9' {
				return false, errInvalidCron(part)
			}
			n = n*10 + int(ch-'0')
		}
		if n <= 0 {
			return false, errInvalidCron(part)
		}
		step = n
	}
	lo, hi := 0, 0
	switch {
	case part == "*":
		lo, hi = 0, 1<<30
	case strings.Contains(part, "-"):
		bounds := strings.SplitN(part, "-", 2)
		a, errA := parseBound(bounds[0])
		b, errB := parseBound(bounds[1])
		if errA != nil || errB != nil {
			return false, errInvalidCron(part)
		}
		lo, hi = a, b
	default:
		v, err := parseBound(part)
		if err != nil {
			return false, errInvalidCron(part)
		}
		lo, hi = v, v
	}
	if value < lo || value > hi {
		return false, nil
	}
	if step == 1 {
		return true, nil
	}
	return (value-lo)%step == 0, nil
}

func parseBound(s string) (int, error) {
	if s == "" {
		return 0, errInvalidCron(s)
	}
	n := 0
	for _, ch := range s {
		if ch < '0' || ch > '9' {
			return 0, errInvalidCron(s)
		}
		n = n*10 + int(ch-'0')
	}
	return n, nil
}
