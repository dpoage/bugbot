package report

import (
	"context"
	"fmt"
	"strings"

	"github.com/dpoage/bugbot/internal/domain"
)

// findingLister is the slice of the store API this package needs. Keeping it as
// a small interface (rather than *store.Store) keeps report decoupled and lets
// the daemon, CLI, and tests pass any equivalent provider.
type findingLister interface {
	ListFindings(ctx context.Context, filter domain.FindingFilter) ([]domain.Finding, error)
}

// CollectOpen loads all open findings from the store and wraps them in a Report
// with the supplied metadata, applying canonical ordering. This is the single
// reuse point for `bugbot report emit`, scan, and the daemon: emit current open
// findings through configured sinks.
func CollectOpen(ctx context.Context, l findingLister, meta Metadata) (Report, error) {
	fs, err := l.ListFindings(ctx, domain.FindingFilter{Status: domain.StatusOpen})
	if err != nil {
		return Report{}, fmt.Errorf("report: list open findings: %w", err)
	}
	return New(fs, meta), nil
}

// ErrAmbiguousID is returned by ResolveID when a prefix matches more than one
// finding. The matching ids are included in the error text to aid the user.
type ErrAmbiguousID struct {
	Prefix  string
	Matches []string
}

func (e *ErrAmbiguousID) Error() string {
	return fmt.Sprintf("id prefix %q is ambiguous; matches %d findings: %s",
		e.Prefix, len(e.Matches), strings.Join(e.Matches, ", "))
}

// ResolveID resolves an exact id or an unambiguous id prefix to a single
// finding. It performs a linear scan over all findings, which is fine at
// Bugbot's scale (findings number in the tens to low thousands and live in a
// local SQLite store); if that ever changes, add a prefix index. Resolution
// rules, in order:
//
//  1. An exact id match wins immediately, even if it is also a prefix of others.
//  2. Otherwise, a unique prefix match is returned.
//  3. Zero prefix matches -> domain.ErrNotFound.
//  4. Multiple prefix matches -> *ErrAmbiguousID.
func ResolveID(ctx context.Context, l findingLister, idOrPrefix string) (domain.Finding, error) {
	idOrPrefix = strings.TrimSpace(idOrPrefix)
	if idOrPrefix == "" {
		return domain.Finding{}, fmt.Errorf("report: empty id")
	}

	all, err := l.ListFindings(ctx, domain.FindingFilter{})
	if err != nil {
		return domain.Finding{}, fmt.Errorf("report: list findings: %w", err)
	}

	var matches []domain.Finding
	for _, f := range all {
		if f.ID == idOrPrefix {
			return f, nil // exact match short-circuits ambiguity
		}
		if strings.HasPrefix(f.ID, idOrPrefix) {
			matches = append(matches, f)
		}
	}

	switch len(matches) {
	case 0:
		return domain.Finding{}, domain.ErrNotFound
	case 1:
		return matches[0], nil
	default:
		ids := make([]string, len(matches))
		for i, m := range matches {
			ids[i] = m.ID
		}
		return domain.Finding{}, &ErrAmbiguousID{Prefix: idOrPrefix, Matches: ids}
	}
}
