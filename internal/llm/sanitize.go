package llm

import "regexp"

// leadingThinkRE matches a single <think>...</think> or <thinking>...</thinking>
// block anchored at the start of the (whitespace-trimmed) text. It is
// case-insensitive ((?i)), spans newlines ((?s) makes . match '\n'), and is
// non-greedy so it stops at the first closing tag rather than swallowing a
// payload that legitimately contains the literal "</think>" in a string value.
//
// Stripping is anchored at the leading edge and repeated, so only think blocks
// that PRECEDE the payload are removed; a "<think>" appearing inside the body
// is left untouched. This is the documented limitation: think blocks must wrap
// the payload, not be embedded within it.
var leadingThinkRE = regexp.MustCompile(`(?is)^\s*<think(?:ing)?>.*?</think(?:ing)?>`)

// unclosedThinkRE matches an UNCLOSED trailing think tag: an opening
// <think>/<thinking> with no closing tag, running to end of input. Reasoning
// models truncated mid-thought emit this. Anchored at the leading edge (after
// closed leading blocks are removed) so it only strips a think span that
// precedes — or replaces — the payload, never one embedded inside the body.
var unclosedThinkRE = regexp.MustCompile(`(?is)^\s*<think(?:ing)?>.*$`)

// StripThinkBlocks removes reasoning-model think spans that wrap the visible
// answer. Real reasoning models (e.g. MiniMax-M3) emit one or more
// "<think>...</think>" blocks inline in message content before the actual
// answer; consumers that want the answer alone — RunJSON parsing a JSON
// payload, the cartographer storing a package summary — must drop them first
// or they pollute (or, on truncation, replace) the result.
//
// It strips repeatedly from the leading edge: multiple consecutive closed
// blocks are all removed, then a single unclosed trailing <think> (a truncated
// thought) is dropped. It deliberately does NOT strip blocks embedded inside
// the body, so a payload that legitimately contains the literal "<think>"
// survives intact. The result is NOT whitespace-trimmed; callers trim as
// needed.
func StripThinkBlocks(s string) string {
	out := s
	for {
		stripped := leadingThinkRE.ReplaceAllString(out, "")
		if stripped == out {
			break
		}
		out = stripped
	}
	// After all closed leading blocks are gone, drop a single unclosed trailing
	// think span (truncation). Only fires when an opening tag still leads the
	// remaining text, so a well-formed payload is never touched.
	out = unclosedThinkRE.ReplaceAllString(out, "")
	return out
}
