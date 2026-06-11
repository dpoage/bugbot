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

	// TypeScript and TSX are DISTINCT grammars in the registry: the tsx grammar
	// parses JSX syntax (`<Foo/>`) that fails to parse under plain TypeScript,
	// silently dropping every definition/reference in a .tsx file. The two share
	// identical tags queries (TSX is a superset of TS for the nodes we capture),
	// so we build the query text once and attach it to each grammar via its own
	// registry sample filename.
	//
	// JavaScript extensions (.js, .jsx, .mjs, .cjs) also map to tsxGrammar for
	// two reasons: (1) the tsx grammar is a superset that parses plain JS and JSX
	// without issue; (2) tsDefQuery captures interface_declaration and
	// type_alias_declaration — TypeScript-only node types — so compiling these
	// queries against the registry's plain javascript grammar would fail at
	// query-compile time. Sharing the tsxGrammar pointer means .tsx/.js/.jsx/
	// .mjs/.cjs files form one navigation family (cross-navigate freely in
	// mixed repos); plain .ts remains its own family via tsGrammar, as before.
	const tsDefQuery = `
(function_declaration name: (identifier) @name) @definition.function
(method_definition name: (property_identifier) @name) @definition.method
(class_declaration name: (type_identifier) @name) @definition.class
(interface_declaration name: (type_identifier) @name) @definition.interface
(type_alias_declaration name: (type_identifier) @name) @definition.type
`
	const tsRefQuery = `
(call_expression function: (identifier) @name) @reference.call
(call_expression function: (member_expression property: (property_identifier) @name)) @reference.call
`

	tsGrammar := &grammar{
		name:     "typescript",
		sample:   "x.ts",
		defQuery: tsDefQuery,
		refQuery: tsRefQuery,
	}

	tsxGrammar := &grammar{
		name:     "tsx",
		sample:   "x.tsx",
		defQuery: tsDefQuery,
		refQuery: tsRefQuery,
	}

	m := map[string]*grammar{
		".go":  goGrammar,
		".py":  pyGrammar,
		".pyi": pyGrammar,
		".ts":  tsGrammar,
		".tsx": tsxGrammar,
		// JS extensions share tsxGrammar — see the comment above tsDefQuery.
		".js":  tsxGrammar,
		".jsx": tsxGrammar,
		".mjs": tsxGrammar,
		".cjs": tsxGrammar,
	}
	return m
}()

// grammarForExt returns the grammar registered for a lowercase extension, or
// nil if the language is not supported by this tier.
func grammarForExt(ext string) *grammar {
	return grammarTable[ext]
}
