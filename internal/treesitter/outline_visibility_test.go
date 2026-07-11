package treesitter

import (
	"path/filepath"
	"testing"
)

// outlineByName returns the OutlineEntry for symbol name in entries, or the
// zero value if not found.
func outlineByName(entries []OutlineEntry, name string) (OutlineEntry, bool) {
	for _, e := range entries {
		if e.Name == name {
			return e, true
		}
	}
	return OutlineEntry{}, false
}

// TestOutlineVisibility_C_StaticPrivate proves that a C function declared
// with the "static" storage-class specifier gets VisibilityPrivate, while an
// extern-linkage function gets VisibilityPublic.
func TestOutlineVisibility_C_StaticPrivate(t *testing.T) {
	root := writeRepo(t, map[string]string{
		// line 1: static helper → private
		// line 2: extern API   → public
		"util.c": "static int helper(void) { return 0; }\n" +
			"int api_fn(void) { return 1; }\n",
	})
	b := New(root)
	entries, err := b.Outline(filepath.Join(root, "util.c"))
	if err != nil {
		t.Fatalf("Outline: %v", err)
	}

	helper, ok := outlineByName(entries, "helper")
	if !ok {
		t.Fatal("helper not found in outline")
	}
	if helper.Visibility != VisibilityPrivate {
		t.Errorf("helper.Visibility = %q, want private", helper.Visibility)
	}

	apiFn, ok := outlineByName(entries, "api_fn")
	if !ok {
		t.Fatal("api_fn not found in outline")
	}
	if apiFn.Visibility != VisibilityPublic {
		t.Errorf("api_fn.Visibility = %q, want public", apiFn.Visibility)
	}
}

// TestOutlineVisibility_CPP_StructDefaultPublic proves that members of a C++
// struct are VisibilityPublic by default (no access specifier).
func TestOutlineVisibility_CPP_StructDefaultPublic(t *testing.T) {
	root := writeRepo(t, map[string]string{
		"point.cpp": "struct Point {\n" +
			"    int x;\n" +
			"    int getX() { return x; }\n" +
			"};\n",
	})
	b := New(root)
	entries, err := b.Outline(filepath.Join(root, "point.cpp"))
	if err != nil {
		t.Fatalf("Outline: %v", err)
	}

	getX, ok := outlineByName(entries, "getX")
	if !ok {
		t.Fatal("getX not found in outline")
	}
	if getX.Visibility != VisibilityPublic {
		t.Errorf("getX.Visibility = %q, want public (struct default)", getX.Visibility)
	}
}

// TestOutlineVisibility_CPP_ClassDefaultPrivate proves that members of a C++
// class are VisibilityPrivate by default (class keyword → private default).
func TestOutlineVisibility_CPP_ClassDefaultPrivate(t *testing.T) {
	root := writeRepo(t, map[string]string{
		"myclass.cpp": "class MyClass {\n" +
			"    int secret() { return 0; }\n" +
			"};\n",
	})
	b := New(root)
	entries, err := b.Outline(filepath.Join(root, "myclass.cpp"))
	if err != nil {
		t.Fatalf("Outline: %v", err)
	}

	secret, ok := outlineByName(entries, "secret")
	if !ok {
		t.Fatal("secret not found in outline")
	}
	if secret.Visibility != VisibilityPrivate {
		t.Errorf("secret.Visibility = %q, want private (class default)", secret.Visibility)
	}
}

// TestOutlineVisibility_CPP_AccessSpecifierSwitch proves that access-specifier
// lines (public:, private:) correctly flip visibility for subsequent members.
func TestOutlineVisibility_CPP_AccessSpecifierSwitch(t *testing.T) {
	root := writeRepo(t, map[string]string{
		// class default: private
		// line 2: private: (redundant, but explicit)
		// line 3: hidden() → private
		// line 4: public:
		// line 5: open()  → public
		// line 6: private: again
		// line 7: again_hidden() → private
		"api.cpp": "class Api {\n" +
			"private:\n" +
			"    int hidden() { return 0; }\n" +
			"public:\n" +
			"    int open() { return 1; }\n" +
			"private:\n" +
			"    int again_hidden() { return 2; }\n" +
			"};\n",
	})
	b := New(root)
	entries, err := b.Outline(filepath.Join(root, "api.cpp"))
	if err != nil {
		t.Fatalf("Outline: %v", err)
	}

	cases := []struct {
		name string
		want Visibility
	}{
		{"hidden", VisibilityPrivate},
		{"open", VisibilityPublic},
		{"again_hidden", VisibilityPrivate},
	}
	for _, c := range cases {
		e, ok := outlineByName(entries, c.name)
		if !ok {
			t.Errorf("%s: not found in outline", c.name)
			continue
		}
		if e.Visibility != c.want {
			t.Errorf("%s.Visibility = %q, want %q", c.name, e.Visibility, c.want)
		}
	}
}

// TestOutlineVisibility_CPP_AnonNamespacePrivate proves that free functions
// inside an anonymous namespace are VisibilityPrivate.
func TestOutlineVisibility_CPP_AnonNamespacePrivate(t *testing.T) {
	root := writeRepo(t, map[string]string{
		"ns.cpp": "namespace {\n" +
			"int internal_fn() { return 0; }\n" +
			"}\n" +
			"int public_fn() { return 1; }\n",
	})
	b := New(root)
	entries, err := b.Outline(filepath.Join(root, "ns.cpp"))
	if err != nil {
		t.Fatalf("Outline: %v", err)
	}

	internalFn, ok := outlineByName(entries, "internal_fn")
	if !ok {
		t.Fatal("internal_fn not found in outline")
	}
	if internalFn.Visibility != VisibilityPrivate {
		t.Errorf("internal_fn.Visibility = %q, want private (anon namespace)", internalFn.Visibility)
	}

	publicFn, ok := outlineByName(entries, "public_fn")
	if !ok {
		t.Fatal("public_fn not found in outline")
	}
	if publicFn.Visibility != VisibilityPublic {
		t.Errorf("public_fn.Visibility = %q, want public", publicFn.Visibility)
	}
}

// TestOutlineVisibility_CPP_StaticFreeFnPrivate proves that a free function
// with the "static" storage-class specifier at file scope is VisibilityPrivate.
func TestOutlineVisibility_CPP_StaticFreeFnPrivate(t *testing.T) {
	root := writeRepo(t, map[string]string{
		"static_fn.cpp": "static int helper() { return 0; }\n" +
			"int exported() { return 1; }\n",
	})
	b := New(root)
	entries, err := b.Outline(filepath.Join(root, "static_fn.cpp"))
	if err != nil {
		t.Fatalf("Outline: %v", err)
	}

	helper, ok := outlineByName(entries, "helper")
	if !ok {
		t.Fatal("helper not found in outline")
	}
	if helper.Visibility != VisibilityPrivate {
		t.Errorf("helper.Visibility = %q, want private (static free function)", helper.Visibility)
	}

	exported, ok := outlineByName(entries, "exported")
	if !ok {
		t.Fatal("exported not found in outline")
	}
	if exported.Visibility != VisibilityPublic {
		t.Errorf("exported.Visibility = %q, want public", exported.Visibility)
	}
}

// TestOutlineVisibility_Go_Unknown proves that Go files produce VisibilityUnknown
// (visibility is expressed through name casing, not syntax markers).
func TestOutlineVisibility_Go_Unknown(t *testing.T) {
	root := writeRepo(t, map[string]string{
		"pkg.go": "package pkg\n\nfunc Exported() {}\nfunc unexported() {}\n",
	})
	b := New(root)
	entries, err := b.Outline(filepath.Join(root, "pkg.go"))
	if err != nil {
		t.Fatalf("Outline: %v", err)
	}

	for _, e := range entries {
		if e.Visibility != VisibilityUnknown {
			t.Errorf("Go symbol %q: Visibility = %q, want unknown (name heuristic handles Go)", e.Name, e.Visibility)
		}
	}
}
