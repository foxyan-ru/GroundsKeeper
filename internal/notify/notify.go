// Package notify sends alerts to a human fallback contact when Groundskeeper
// needs a person to know something happened — most importantly, when a
// post-merge health check fails and an automatic rollback was performed (or
// attempted) on an orphaned repo with no active maintainer to otherwise
// notice.
package notify

import "context"

// Notifier delivers a plain-text message to whatever fallback contact
// channel a target repository is configured with.
type Notifier interface {
	Notify(ctx context.Context, message string) error
}

// MultiNotifier fans a single Notify call out to several Notifiers (e.g.
// Slack plus email), continuing to attempt the rest even if one fails, and
// returning a combined error only if every one of them failed.
type MultiNotifier struct {
	Notifiers []Notifier
}

// Notify implements Notifier by delivering message via every configured
// Notifier.
func (m *MultiNotifier) Notify(ctx context.Context, message string) error {
	var errs []error
	for _, n := range m.Notifiers {
		if err := n.Notify(ctx, message); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) == 0 {
		return nil
	}
	if len(errs) == len(m.Notifiers) {
		return &allFailedError{errs: errs}
	}
	// At least one Notifier succeeded; a partial failure is logged by the
	// caller rather than surfaced as a hard error, since the message did
	// get through to a human somewhere.
	return nil
}

// allFailedError reports that every configured Notifier failed.
type allFailedError struct {
	errs []error
}

func (e *allFailedError) Error() string {
	msg := "notify: all notifiers failed:"
	for _, err := range e.errs {
		msg += " [" + err.Error() + "]"
	}
	return msg
}

func (e *allFailedError) Unwrap() []error {
	return e.errs
}
