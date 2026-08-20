package fsckcmd

// The progress meters, one per phase git shows one on.

import "github.com/wow-look-at-my/git-fixed/internal/progress"

// meterOn starts a meter that draws immediately, or returns nil when nobody
// asked for progress. A nil meter is a working meter that draws nothing, so no
// caller has to ask.
func (r *run) meterOn(title string, total int64) *progress.Meter {
	if !r.o.ShowProgress {
		return nil
	}
	return progress.Start(r.o.Stderr, title, total)
}

// meterDelayed starts one that stays quiet for a second first, for a phase that
// is usually instant.
func (r *run) meterDelayed(title string, total int64) *progress.Meter {
	if !r.o.ShowProgress {
		return nil
	}
	return progress.StartDelayed(r.o.Stderr, title, total)
}
