package daemon

import (
	"context"

	"github.com/dpoage/bugbot/internal/funnel"
	"github.com/dpoage/bugbot/internal/ingest"
)

// renamesSince returns the file renames git detected between two commits, or
// nil if lastSeen is empty/equal to head or the diff fails. Mirrors
// changedPathsSince's best-effort failure handling: a diff error just means no
// renames are processed this cycle, not a fatal cycle failure — the targeted
// scan still runs against the raw changed-file set.
func renamesSince(ctx context.Context, repo *ingest.Repo, lastSeen, head string) []ingest.Change {
	if lastSeen == "" || head == "" || lastSeen == head {
		return nil
	}
	changes, err := repo.ChangedFiles(ctx, lastSeen, head)
	if err != nil {
		return nil
	}
	var renames []ingest.Change
	for _, c := range changes {
		if c.Kind == ingest.ChangeRenamed {
			renames = append(renames, c)
		}
	}
	return renames
}

// applyRenames rewrites stored finding/suppression identity for every rename
// git detected between lastSeen and head (store.RenameFindingIdentity), so a
// moved file's open findings and dismissals survive under its new path
// instead of resurfacing as an unrelated "new" finding — or a suppressed one
// resurrecting as open — at the next scan (bugbot-ezmx.6). Best-effort per
// rename: one failing rewrite is logged and does not block the others or the
// rest of the cycle. Idempotent: store.RenameFindingIdentity is a no-op once a
// rename has already been applied, so replaying the same commit range after an
// interrupted cycle (bugbot-r4x3) is safe.
func (d *Daemon) applyRenames(ctx context.Context, lastSeen, head string) {
	renames := renamesSince(ctx, d.repo, lastSeen, head)
	if len(renames) == 0 {
		return
	}
	resolve := funnel.NewLocusResolver(d.repo.Root()).Resolve
	for _, r := range renames {
		n, err := d.store.RenameFindingIdentity(ctx, r.OldPath, r.Path, resolve)
		if err != nil {
			d.log.Error("daemon: rename identity rewrite failed", "old", r.OldPath, "new", r.Path, "err", err)
			continue
		}
		if n > 0 {
			d.log.Info("daemon: rewrote finding identity for rename", "old", r.OldPath, "new", r.Path, "count", n)
		}
	}
}
