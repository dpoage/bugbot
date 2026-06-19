// Package domain holds the small, dependency-free vocabulary types shared
// across bugbot: the confidence Tier, Severity, and Confidence of a finding.
//
// Keeping them in a leaf package — importing only the standard library — lets
// every other package, including store and config (which would otherwise risk
// an import cycle), depend on a single typed source of truth instead of
// re-validating and re-mapping bare ints and strings at each call site. This
// mirrors the discipline of the internal/llm package: a small, dependency-free
// core that callers build on.
//
// Tracked under bugbot-0nc (type-design hardening). The caller migrations that
// replace the duplicated mappings live in bugbot-0nc.2/.3/.4.
package domain
