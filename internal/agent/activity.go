package agent

// ToolActivity describes one tool-call execution. The agent package defines this
// type so the runner can emit structured activity without importing progress
// (the funnel imports both and bridges them at the AgentScope seam).
//
// Phase is "start" (emitted immediately before execution) or "done" (emitted
// immediately after, with Count and Err filled from the result).
//
// Fields are a flat superset; only the relevant subset is populated for each
// tool:
//   - read_file      → File, Line (start offset), EndLine (end offset)
//   - read_symbol    → Symbol, File
//   - grep           → Pattern, File (dir or file target, optional)
//   - find_definition/references/implementations/usages → Symbol, File, Line
//   - list_dir       → File (the directory path)
//   - run_tests      → File (package/dir), Symbol (summary label)
//   - sandbox_exec   → Symbol ("sandbox")
//   - status_note    → Tool="status_note", Symbol (the note text, truncated)
//   - post_lead      → (no extra fields)
//   - unknown        → Tool (name only)
//
// Count is set on Phase="done": for grep it is the hit count; for
// find_references/find_usages it is the reference count; for read_file it is
// the number of lines read. Zero when the tool does not produce a count.
//
// Err is the tool error string on Phase="done", or "" on success.
type ToolActivity struct {
	// Phase is "start" or "done".
	Phase string
	// Tool is the tool name (e.g. "read_file", "grep", "status_note").
	Tool string
	// File is the repo-relative file or directory path, when applicable.
	File string
	// Line is the 1-based start line (read_file, find_*).
	Line int
	// EndLine is the 1-based end line for a read window (read_file only).
	EndLine int
	// Symbol is the symbol name (read_symbol, find_*) or, for status_note,
	// the sanitized note text.
	Symbol string
	// Pattern is the grep regex.
	Pattern string
	// Count is the result count (hits, refs, lines) on Phase="done".
	Count int
	// Err is the tool error string on Phase="done", or "" on success.
	Err string
}
