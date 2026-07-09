package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

// Site is one code location (file + line) contributing to a finding. The
// primary's own location is always Sites[0]; additional entries come from
// same-root-cause members collapsed into the primary during triage.
type Site struct {
	File string
	Line int
}

// Status enumerates the lifecycle states of a finding.
type Status string

const (
	// StatusOpen means the finding is active and may be reported.
	StatusOpen Status = "open"
	// StatusDismissed means a maintainer rejected it; it is suppressed forever.
	StatusDismissed Status = "dismissed"
	// StatusFixed means the underlying bug has been resolved.
	StatusFixed Status = "fixed"
	// StatusSuperseded means a newer finding replaced this one (e.g. the code
	// moved and a new fingerprint was generated).
	StatusSuperseded Status = "superseded"
)

// NeedsHumanReason is the explicit cause for the NeedsHuman flag. It replaces
// the prior flag-combination inference (NeedsHuman && ReproWitness != "" etc.)
// that downstream code branched on. Exactly one value applies when NeedsHuman
// is true; the zero value means NeedsHuman is false.
type NeedsHumanReason string

const (
	// NeedsHumanReasonNone means the finding does not need human review
	// (NeedsHuman is false). This is the zero value.
	NeedsHumanReasonNone NeedsHumanReason = ""

	// NeedsHumanReasonProverExhausted means the patch-prover used up its attempt
	// budget without finding a minimal fix. The finding is already Tier-1
	// (ReproPath set); a human must inspect the fix candidates.
	NeedsHumanReasonProverExhausted NeedsHumanReason = "prover_exhausted"

	// NeedsHumanReasonBelowQuorum means the verifier survivor fell below the
	// genuine-verdict quorum floor: too few verifier seats actually ruled on it.
	// The finding stays at its original tier (typically T2/T3) with no ReproPath;
	// it may have a ReproWitness bundle from the witness path.
	NeedsHumanReasonBelowQuorum NeedsHumanReason = "below_quorum"
)

// ErrIllegalTransition is returned when a state mutation would produce an
// invariant-violating combination of lifecycle fields.
var ErrIllegalTransition = fmt.Errorf("domain: illegal finding state transition")

// ErrNotFound is returned by store Get-style methods when no row matches.
// It lives in domain so packages that only need the sentinel do not have
// to import the persistence layer.
var ErrNotFound = fmt.Errorf("domain: not found")

// ReproContradictionThreshold is the number of exit-zero (bug-did-not-manifest)
// repro attempts required to set the repro-contradicted signal. Two independent
// attempts, typically across code revisions of the same fingerprint, is the
// minimum meaningful disconfirmation: a single pass could be a transient
// environment issue; two passes suggest the bug is not reliably reproducible.
const ReproContradictionThreshold = 2

// Finding is a single candidate (or confirmed) bug. It is anchored to a code
// version through CommitSHA and FileHash so the daemon can detect when the
// underlying code has changed and re-verification is warranted.
type Finding struct {
	ID          string
	Fingerprint string
	// LocusKey is the lens-independent location identity sha256(normFile, locus):
	// the Fingerprint inputs minus the lens. It still backs IsSuppressed's
	// legacy fallback and RenameFindingIdentity's rewrite, and is a proper
	// subset of the same-file window store.FindingsByFileWindow now queries
	// for triage's durable cross-lens fold (the fold widened past an exact
	// locus_key match so a drifted locus still folds in). Persisted + indexed;
	// empty on pre-migration rows, which simply do not participate until
	// re-upserted.
	LocusKey string
	// DefectKind is the closed taxonomy class this finding belongs to (see
	// DefectKind in identity.go). Empty on pre-v3 rows (migrated v2 findings
	// that predate the defect_kind/subject fields); such rows simply do not
	// participate in kind-gated clustering/durable-fold logic until re-upserted
	// by a fresh scan.
	DefectKind DefectKind
	// Subject is the normalized symbol at fault (see NormalizeSubject). Empty
	// on pre-v3 rows, same caveat as DefectKind.
	Subject       string
	Title         string
	Description   string
	Reasoning     string // the adversarial verification trace
	VerdictDetail string // impact-sweep rationale for severity re-rank (empty = not re-ranked)
	Severity      Severity
	Tier          Tier // T0 fix-witnessed, T1 reproduced, T2 verified, T3 suspected
	Status        Status
	Lens          string
	File          string
	Line          int
	CommitSHA     string
	FileHash      string
	ReproPath     string // empty when no reproduction exists
	// ReproWitness is the bundle path for a non-promoting repro attempt on a
	// below-quorum (NeedsHuman) finding. It mirrors ReproPath in shape (a
	// self-contained artifact directory) but does NOT promote the finding:
	// Tier, ReproPath, and NeedsHuman are all left untouched, and no
	// patch-prover / publish cascade follows. The human reviewer can run the
	// bundle but downstream automation does not — the human gate is preserved.
	// Empty when no witness was recorded. See repro/promote.go::witnessFinding
	// and funnel/repro_hook.go claim-check split. bugbot-w1bh.
	ReproWitness string // empty when no witness repro was attempted
	// FixPatch is the unified diff text produced by the patch-prover stage when a
	// minimal fix candidate was witnessed by the sandbox.  Empty when the prover
	// was not run or found no plausible fix.
	FixPatch string
	// NeedsHuman flags a finding for human review. The cause is recorded
	// explicitly in NeedsHumanReason; use that field instead of flag-combination
	// inference. Both causes exclude the finding from the repro backlog and the
	// patch-prover cascade until a human confirms (bugbot-sw7).
	NeedsHuman bool
	// NeedsHumanReason is the explicit cause for NeedsHuman. Always set when
	// NeedsHuman is true; NeedsHumanReasonNone when NeedsHuman is false.
	// See NeedsHumanReason constants in finding.go.
	NeedsHumanReason NeedsHumanReason
	// CorroboratingLenses are the OTHER lenses that independently reported this
	// same defect and were collapsed into this finding by triage's location-based
	// cross-lens dedup. It excludes the finding's own Lens. Persisted as a
	// comma-separated text column; it is a reporting signal only and does NOT
	// affect the finding's tier or status.
	CorroboratingLenses []string
	// Sites records every code location this finding represents. Sites[0] is the
	// primary's (File, Line). Additional entries are same-root-cause merge sites.
	// Stored as a pipe-separated "file:line" list; empty when no merge occurred.
	Sites []Site
	// Confidence is derived from tier, severity, and corroborating-lens count at
	// read time. It is also written to the DB column for backward compatibility
	// with direct SQL queries, but callers should treat the struct field as the
	// authoritative value — it is always consistent with the other fields.
	Confidence float64
	CreatedAt  time.Time
	UpdatedAt  time.Time
	// SweptAt is the timestamp when UpdateFindingSeverity last recorded a sweep
	// verdict. Zero = not swept (impact-sweep marker; set by UpdateFindingSeverity,
	// preserved across re-upsert when file_hash is unchanged, reset on file_hash change).
	SweptAt time.Time
	// ReproContradicted is true when this finding's repro test has been run
	// >= ReproContradictionThreshold times and exited 0 each time, meaning the
	// bug did not manifest on repeated attempts. This is disconfirming evidence:
	// the automated test cannot reproduce the bug. Derived from
	// repro_attempts.exit_zero_count via a LEFT JOIN at read time; false when
	// no repro_attempts row exists for this fingerprint.
	ReproContradicted bool
	// SupersededBy is the canonical fingerprint this finding was merged into
	// by backlog reconcile (bugbot-ezmx.4), set only when Status ==
	// StatusSuperseded. Empty otherwise. This is the MACHINE-READABLE merge
	// pointer -- callers must key merge logic on this field, never on
	// SupersededReason's prose (repo invariant: machine decisions never key
	// on prose). A live re-discovery of this exact fingerprint by a future
	// scan naturally clears both fields back to empty on upsert (UpsertFinding
	// does not preserve them), the same way a fixed finding's history is left
	// for a fresh scan to overwrite.
	SupersededBy string
	// SupersededReason is a human-readable note explaining why this finding
	// was superseded (e.g. "backlog reconcile: merged into <fp> (dedup
	// arbiter yes)"). Prose only -- never parsed by machine logic; see
	// SupersededBy.
	SupersededReason string
}

// FindingTallies is the aggregate finding picture for the status pane.
type FindingTallies struct {
	// OpenByTier counts StatusOpen findings keyed by tier (0..3).
	OpenByTier map[int]int
	// NeedsHuman counts open findings flagged by the patch-prover for review.
	NeedsHuman int
	// Fixed and Dismissed count terminal-state findings.
	Fixed     int
	Dismissed int
}

// FindingFilter narrows a findings query. Zero-valued fields are not applied.
type FindingFilter struct {
	Status Status // exact status match
	// HasTier, when true, restricts results to the exact Tier value below.
	// When false, Tier is ignored and all tiers are returned. This replaces the
	// prior zero-sentinel convention (Tier==0 meaning "any tier"), which could
	// not express a T0-only query.
	HasTier   bool
	Tier      Tier   // effective only when HasTier is true
	CommitSHA string // findings anchored to a specific commit
	Lens      string
}

// ValidateFindingState enforces the co-variance invariants documented in the
// original audit comment (internal/repro/promote.go:60-65). Call it on any
// Finding before persisting via UpsertFinding; UpsertFinding calls it
// automatically at the chokepoint.
//
// Enforced invariants:
//
//	(a) T0 (fix-witnessed) requires ReproPath (the fix witness bundle implies a
//	    prior reproduction). Witness-only (ReproWitness without ReproPath) cannot
//	    be T0.
//
//	(b) T1 (reproduced) requires ReproPath.
//
//	(c) ReproWitness implies NeedsHuman (a witness bundle is only written for
//	    the below-quorum path; a non-NeedsHuman finding that somehow has a
//	    ReproWitness is a logic error).
//
//	(d) NeedsHuman requires a non-None NeedsHumanReason (the cause must be
//	    recorded explicitly).
//
//	(e) NeedsHumanReason != None implies NeedsHuman (the reason field must not
//	    be set when the flag is clear).
//
//	(f) ProverExhausted requires ReproPath (patch-prover only runs after T1
//	    promotion, which sets ReproPath).
//
//	(g) T0 conflicts with NeedsHuman: a fix-witnessed finding is fully resolved
//	    and must not be flagged for human review.
func ValidateFindingState(f Finding) error {
	tier := int(f.Tier)

	// (g) T0 + NeedsHuman is contradictory.
	if tier == 0 && f.NeedsHuman {
		return fmt.Errorf("%w: T0 (fix-witnessed) and NeedsHuman are mutually exclusive", ErrIllegalTransition)
	}

	// (a) T0 requires ReproPath.
	if tier == 0 && f.ReproPath == "" {
		return fmt.Errorf("%w: T0 requires ReproPath (no fix without a repro)", ErrIllegalTransition)
	}

	// (b) T1 requires ReproPath.
	if tier == 1 && f.ReproPath == "" {
		return fmt.Errorf("%w: T1 (reproduced) requires ReproPath", ErrIllegalTransition)
	}

	// (c) ReproWitness implies NeedsHuman.
	if f.ReproWitness != "" && !f.NeedsHuman {
		return fmt.Errorf("%w: ReproWitness set but NeedsHuman is false (witness path is only for below-quorum findings)", ErrIllegalTransition)
	}

	// (d) NeedsHuman requires a reason.
	if f.NeedsHuman && f.NeedsHumanReason == NeedsHumanReasonNone {
		return fmt.Errorf("%w: NeedsHuman=true requires a NeedsHumanReason", ErrIllegalTransition)
	}

	// (e) NeedsHumanReason != None requires NeedsHuman.
	if f.NeedsHumanReason != NeedsHumanReasonNone && !f.NeedsHuman {
		return fmt.Errorf("%w: NeedsHumanReason=%q set but NeedsHuman is false", ErrIllegalTransition, f.NeedsHumanReason)
	}

	// (f) ProverExhausted requires ReproPath.
	if f.NeedsHumanReason == NeedsHumanReasonProverExhausted && f.ReproPath == "" {
		return fmt.Errorf("%w: NeedsHumanReason=prover_exhausted requires ReproPath (prover runs after T1 promotion)", ErrIllegalTransition)
	}

	return nil
}

// Fingerprint computes the stable, cross-scan dedup identity for a finding. It
// is deliberately independent of the two inputs that drift between scans of the
// same unchanged code: the model-authored title (reworded every run) and the
// raw line (shifts when code above it changes). Drift in either silently minted
// a fresh identity, so a re-discovered bug failed to dedup and was published as
// a duplicate issue.
//
// The identity is the lens (the defect family), the normalized file path, and a
// caller-supplied location anchor (locus): the enclosing symbol when the funnel
// can resolve one, else a line-based fallback (see funnel.LocusResolver). The
// "bugbotFingerprint/v2" version token namespaces the scheme so a future
// algorithm change yields disjoint identities, and matches the SARIF
// partialFingerprints key. Two findings with the same fingerprint are the same
// bug and dedup/upsert onto one row.
func Fingerprint(lens, file, locus string) string {
	normFile := normalizeFilePath(file)
	h := sha256.New()
	_, _ = fmt.Fprintf(h, "bugbotFingerprint/v2\x00%s\x00%s\x00%s", strings.ToLower(lens), normFile, locus)
	return hex.EncodeToString(h.Sum(nil))
}

// LocusKey computes the lens-independent location identity: the same normalized
// file path and caller-supplied locus anchor that Fingerprint uses, but WITHOUT
// the lens. Two findings with the same LocusKey sit at the same enclosing-symbol
// (or line-fallback) anchor regardless of which lens reported them; triage's
// durable cross-lens fold keys on it. The "bugbotLocus/v1" token namespaces the
// scheme independently of the fingerprint version.
func LocusKey(file, locus string) string {
	normFile := normalizeFilePath(file)
	h := sha256.New()
	_, _ = fmt.Fprintf(h, "bugbotLocus/v1\x00%s\x00%s", normFile, locus)
	return hex.EncodeToString(h.Sum(nil))
}
