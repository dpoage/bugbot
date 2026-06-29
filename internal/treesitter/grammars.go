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

	// C and C++ are DISTINCT grammars in the registry: the cpp grammar adds
	// class_specifier, qualified_identifier (for Class::method), and
	// template_declaration that do not exist in the plain c grammar. Compiling
	// a query referencing those node types against the c grammar fails at
	// query-compile time (the same class of bug that required separate ts/tsx
	// grammars). Therefore cGrammar and cppGrammar have SEPARATE query texts.
	//
	// Node types confirmed present/absent via lang.SymbolByName probing:
	//
	//   cGrammar (present):  function_definition, function_declarator,
	//     pointer_declarator, identifier, field_identifier, type_identifier,
	//     struct_specifier, enum_specifier, union_specifier, type_definition,
	//     call_expression, field_expression
	//   cGrammar (absent):  class_specifier, qualified_identifier,
	//     template_declaration, template_function
	//
	//   cppGrammar adds:    class_specifier, qualified_identifier,
	//     template_declaration, template_function
	//
	// Captured by cGrammar:
	//   - function_definition with a direct (identifier) declarator, e.g. void f()
	//   - function_definition with a pointer_declarator, e.g. int *f()
	//   - typedef name (type_definition → type_identifier)
	//   - struct/enum/union tag names
	//
	// NOT captured by cGrammar (acceptable limitations):
	//   - Functions returning function pointers: int (*signal(...))(int) —
	//     the nested function_declarator is not matched.
	//   - K&R-style function definitions (no function_declarator child).
	//
	// Captured by cppGrammar (all of the above plus):
	//   - class_specifier name (class Foo { ... })
	//   - in-class method definitions: the declarator is a field_identifier
	//   - out-of-class definitions (Foo::bar): declarator is qualified_identifier
	//   - template functions: matched via the underlying function_definition;
	//     the NameRange points to the function name (e.g. line of "T add(...)"),
	//     not the "template <...>" line above it — this is a known limitation.
	//   - refQuery extends C's ref query with qualified_identifier callees
	//     (e.g. Foo::bar() call sites).
	//
	// NOT captured by cppGrammar:
	//   - Constructors and destructors (no separate node type; they appear as
	//     function_definitions whose name is a type_identifier or ~identifier;
	//     excluded to avoid false positives, not needed for navigation to named
	//     methods).
	//   - Operator overloads.
	//
	// .h maps to cGrammar: extLang already classifies .h as C (C++ headers
	// conventionally use .hpp/.hh/.hxx). .h files in C++ projects that are
	// actually C++ headers will parse but may miss C++-only constructs — a
	// known gap, acceptable given .h is predominantly a C extension.
	cGrammar := &grammar{
		name:   "c",
		sample: "x.c",
		defQuery: `
(function_definition
  declarator: (function_declarator
    declarator: (identifier) @name)) @definition.function
(function_definition
  declarator: (pointer_declarator
    declarator: (function_declarator
      declarator: (identifier) @name))) @definition.function
(type_definition
  declarator: (type_identifier) @name) @definition.type
(struct_specifier
  name: (type_identifier) @name) @definition.type
(enum_specifier
  name: (type_identifier) @name) @definition.type
(union_specifier
  name: (type_identifier) @name) @definition.type
`,
		refQuery: `
(call_expression
  function: (identifier) @name) @reference.call
(call_expression
  function: (field_expression
    field: (field_identifier) @name)) @reference.call
`,
	}

	cppGrammar := &grammar{
		name:   "cpp",
		sample: "x.cpp",
		defQuery: `
(function_definition
  declarator: (function_declarator
    declarator: (identifier) @name)) @definition.function
(function_definition
  declarator: (function_declarator
    declarator: (qualified_identifier
      name: (identifier) @name))) @definition.function
(function_definition
  declarator: (function_declarator
    declarator: (field_identifier) @name)) @definition.function
(function_definition
  declarator: (pointer_declarator
    declarator: (function_declarator
      declarator: (identifier) @name))) @definition.function
(class_specifier
  name: (type_identifier) @name) @definition.class
(struct_specifier
  name: (type_identifier) @name) @definition.type
(enum_specifier
  name: (type_identifier) @name) @definition.type
(union_specifier
  name: (type_identifier) @name) @definition.type
(type_definition
  declarator: (type_identifier) @name) @definition.type
`,
		refQuery: `
(call_expression
  function: (identifier) @name) @reference.call
(call_expression
  function: (field_expression
    field: (field_identifier) @name)) @reference.call
(call_expression
  function: (qualified_identifier
    name: (identifier) @name)) @reference.call
`,
	}
	// csharpGrammar covers C# (registry name `c_sharp`, extension `.cs`). The
	// grammar follows the conventional node shapes for definitions and the
	// C-style invocation_expression for references. Probed via the C# grammar's
	// own highlight query (which uses the same node names) and confirmed
	// against a real parse of the test fixture below.
	//
	// Captured by csharpGrammar:
	//   - class_declaration name (e.g. `class Greeter { ... }`)
	//   - interface_declaration name (`interface IFoo { ... }`)
	//   - struct_declaration name (`struct Point { ... }`)
	//   - enum_declaration name (`enum Color { ... }`)
	//   - record_declaration name (both `record R(...);` and `record R { }`)
	//   - method_declaration name (the conventional `name:` field shape)
	//   - local_function_statement name (e.g. `int Add(int a, int b) { ... }`
	//     inside a method body)
	//   - constructor_declaration name (e.g. `public Greeter() { ... }`)
	//   - invocation_expression on an identifier callee (e.g. `Format(who)`)
	//   - invocation_expression on a member-access callee (e.g. `obj.Format(who)`)
	//
	// class/interface/struct/enum/record all expose the type name via a `name:`
	// field, used here for precision (a bare `(identifier)` would match any
	// direct identifier child).
	//
	// Known limitations (acceptable for a syntactic fallback tier):
	//   - Properties, fields, events, indexers, operators, destructors are not
	//     captured as definitions — the C# grammar models them with distinct
	//     node types (property_declaration, field_declaration, etc.) that
	//     would each need their own probe. The bead scopes this change to the
	//     load-bearing C# shapes (class/interface/struct/enum/record/method/
	//     ctor/local function + identifier/member-access call refs).
	//   - Generic methods/classes: the type parameter list appears before the
	//     name identifier, but the `name:` field still captures the identifier
	//     itself.
	//   - Member-access calls capture the FINAL segment, so `obj.Method()`,
	//     `A.B.Method()`, and `System.Console.WriteLine()` all resolve to the
	//     trailing identifier (the outer member_access_expression's `name:`).
	csharpGrammar := &grammar{
		name:   "c_sharp",
		sample: "x.cs",
		defQuery: `
(class_declaration name: (identifier) @name) @definition.class
(interface_declaration name: (identifier) @name) @definition.interface
(struct_declaration name: (identifier) @name) @definition.type
(enum_declaration name: (identifier) @name) @definition.type
(record_declaration name: (identifier) @name) @definition.type
(method_declaration name: (identifier) @name) @definition.method
(local_function_statement name: (identifier) @name) @definition.function
(constructor_declaration name: (identifier) @name) @definition.method
`,
		refQuery: `
(invocation_expression function: (identifier) @name) @reference.call
(invocation_expression function: (member_access_expression name: (identifier) @name)) @reference.call
`,
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
		// C extensions: .c and .h use cGrammar. .h is classified as C by extLang;
		// C++ projects typically use .hpp/.hh/.hxx for C++ headers.
		".c": cGrammar,
		".h": cGrammar,
		// C++ extensions use cppGrammar. See the comment above cGrammar for why
		// C and C++ must be distinct grammar entries.
		".cc":  cppGrammar,
		".cpp": cppGrammar,
		".cxx": cppGrammar,
		".hpp": cppGrammar,
		".hh":  cppGrammar,
		".hxx": cppGrammar,
		// C# uses csharpGrammar. .cs is classified as C# by extLang
		// (internal/ingest/lang.go) and by the LSP server config.
		".cs": csharpGrammar,
	}
	return m
}()

// grammarForExt returns the grammar registered for a lowercase extension, or
// nil if the language is not supported by this tier.
func grammarForExt(ext string) *grammar {
	return grammarTable[ext]
}
