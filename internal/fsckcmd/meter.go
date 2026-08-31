package fsckcmd

// The progress meters, matching the phases git draws a meter on.

import "github.com/wow-look-at-my/git-fixed/internal/progress"

// meterOn starts a meter that draws immediately, or returns nil when nobody asked for progress.
func (r *run) meterOn(title string, total int64) *progress.Meter {
	if !r.o.ShowProgress {
		return nil
	}
	return progress.Start(r.o.Stderr, title, total)
}

// meterDelayed starts a meter that stays quiet briefly before it draws, for a phase that
// is usually instant.
func (r *run) meterDelayed(title string, total int64) *progress.Meter {
	if !r.o.ShowProgress {
		return nil
	}
	return progress.StartDelayed(r.o.Stderr, title, total)
}
