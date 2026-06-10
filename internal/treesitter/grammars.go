package treesitter

// grammar describes one language's tree-sitter tags queries. Adding a language
// is data, not code: append a grammar entry keyed by file extension. The
// queries follow the tree-sitter tags convention — a @name capture for the
// symbol identifier and a @definition.X / @reference.X capture for the kind.
//
// The reference query is what makes this tier better than grep: because it
// matches AST nodes (call expressions, member accesses), a symbol name that
// merely appears inside a comment or string literal is never captured.
type grammar struct {
	// name is the gotreesitter registry language name (used to load the
	// grammar via a representative filename).
	name string
	// sample is a filename with this language's extension, used to look the
	// grammar up in the registry (the registry is keyed by filename).
	sample string
	// defQuery captures definitions: functions, methods, types, etc.
	defQuery string
	// refQuery captures references: call sites and member accesses. Kept
	// separate from defQuery so a single Tagger run can be scoped to one or the
	// other without re-filtering by kind.
	refQuery string
}

// grammarTable maps a lowercase file extension (with leading dot) to its
// grammar. Several extensions may share a grammar (e.g. .ts/.tsx, .py/.pyi).
var grammarTable = func() map[string]*grammar {
	goGrammar := &grammar{
		name:   "go",
		sample: "x.go",
		defQuery: `
(function_declaration name: (identifier) @name) @definition.function
(method_declaration name: (field_identifier) @name) @definition.method
(type_spec name: (type_identifier) @name) @definition.type
(const_spec name: (identifier) @name) @definition.constant
(var_spec name: (identifier) @name) @definition.variable
`,
		refQuery: `
(call_expression function: (identifier) @name) @reference.call
(call_expression function: (selector_expression field: (field_identifier) @name)) @reference.call
`,
	}

	pyGrammar := &grammar{
		name:   "python",
		sample: "x.py",
		defQuery: `
(function_definition name: (identifier) @name) @definition.function
(class_definition name: (identifier) @name) @definition.class
`,
		refQuery: `
(call (identifier) @name) @reference.call
(call (attribute attribute: (identifier) @name)) @reference.call
`,
	}

	tsGrammar := &grammar{
		name:   "typescript",
		sample: "x.ts",
		defQuery: `
(function_declaration name: (identifier) @name) @definition.function
(method_definition name: (property_identifier) @name) @definition.method
(class_declaration name: (type_identifier) @name) @definition.class
(interface_declaration name: (type_identifier) @name) @definition.interface
(type_alias_declaration name: (type_identifier) @name) @definition.type
`,
		refQuery: `
(call_expression function: (identifier) @name) @reference.call
(call_expression function: (member_expression property: (property_identifier) @name)) @reference.call
`,
	}

	m := map[string]*grammar{
		".go":  goGrammar,
		".py":  pyGrammar,
		".pyi": pyGrammar,
		".ts":  tsGrammar,
		".tsx": tsGrammar,
	}
	return m
}()

// grammarForExt returns the grammar registered for a lowercase extension, or
// nil if the language is not supported by this tier.
func grammarForExt(ext string) *grammar {
	return grammarTable[ext]
}
