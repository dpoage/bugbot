package ingest

import (
	"context"
	"testing"
)

// TestPackageImporters_DirectEdge pins the core contract: a package that
// directly imports another shows up in the imported package's importer list,
// and a package that does NOT import another does not show up in its
// importer list. The test reuses the testRepo harness from ingest_test.go
// (newTestRepo / write / commit / open) to build a small fixture with the
// module name "github.com/dpoage/bugbot", matching the production prefix so
// suffix matching resolves correctly.
func TestPackageImporters_DirectEdge(t *testing.T) {
	r := newTestRepo(t)
	// Package a imports package b. Package c is unrelated.
	r.write("a/a.go", "package a\n\nimport \"github.com/dpoage/bugbot/b\"\n\nfunc Use() { b.B() }\n")
	r.write("b/b.go", "package b\n\nfunc B() {}\n")
	r.write("c/c.go", "package c\n\nfunc C() {}\n")
	r.commit("init")

	repo := r.open()
	snap, err := repo.Snapshot(context.Background(), ScanFilter{})
	if err != nil {
		t.Fatal(err)
	}

	importers, err := repo.PackageImporters(context.Background(), snap)
	if err != nil {
		t.Fatalf("PackageImporters: %v", err)
	}

	// importers["b"] must include "a" — a imports b.
	gotA := importers["b"]
	if !contains(gotA, "a") {
		t.Errorf("importers[\"b\"] = %v, want it to contain \"a\"", gotA)
	}
	// importers["a"] must NOT include "b" — b does not import a.
	gotB := importers["a"]
	if contains(gotB, "b") {
		t.Errorf("importers[\"a\"] = %v, want it to NOT contain \"b\"", gotB)
	}
	// c is unrelated to anything; it must not appear as a key in importers
	// for the existing packages (it has no incoming edges to record).
	if contains(importers["a"], "c") {
		t.Errorf("importers[\"a\"] = %v, want it to NOT contain \"c\"", importers["a"])
	}
	if contains(importers["b"], "c") {
		t.Errorf("importers[\"b\"] = %v, want it to NOT contain \"c\"", importers["b"])
	}
}

// TestPackageImporters_OmitsSelfEdge verifies that a package is not recorded
// as an importer of itself. Internal cross-file imports within one package
// are an implementation detail and would be misleading in the importers
// map.
func TestPackageImporters_OmitsSelfEdge(t *testing.T) {
	r := newTestRepo(t)
	// Two files in the same package; the second imports a sibling. The
	// package should not appear as an importer of itself.
	r.write("p/p1.go", "package p\n\nfunc One() int { return 1 }\n")
	r.write("p/p2.go", "package p\n\nimport \"github.com/dpoage/bugbot/p\"\n\nfunc Two() int { return One() }\n")
	r.commit("init")

	repo := r.open()
	snap, err := repo.Snapshot(context.Background(), ScanFilter{})
	if err != nil {
		t.Fatal(err)
	}

	importers, err := repo.PackageImporters(context.Background(), snap)
	if err != nil {
		t.Fatalf("PackageImporters: %v", err)
	}
	if contains(importers["p"], "p") {
		t.Errorf("importers[\"p\"] = %v, want self-edge to be omitted", importers["p"])
	}
}

// TestPackageImporters_NilSnapshotDoesNotPanic pins the empty-input
// semantics: callers (the cartographer) hand PackageImporters the
// snapshot they have; a nil snapshot must not panic. The exact return
// value (nil map vs empty map) is left to the implementation — the
// contract only requires "no panic, no error".
func TestPackageImporters_NilSnapshotDoesNotPanic(t *testing.T) {
	r := newTestRepo(t)
	repo := r.open()

	importers, err := repo.PackageImporters(context.Background(), nil)
	if err != nil {
		t.Fatalf("PackageImporters(nil): %v", err)
	}
	if len(importers) != 0 {
		t.Errorf("importers = %v, want empty", importers)
	}
}
