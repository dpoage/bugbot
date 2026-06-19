//go:build integration

package repro

import (
	"context"
	"testing"
	"time"

	"github.com/dpoage/bugbot/internal/sandbox"
)

// TestVerifySandbox_Integration_GoImage runs a trivial Go smoke against a
// golang image (toolchain present) and expects ok=true.
//
// Requires podman and the golang:1.21 image to be present locally.
func TestVerifySandbox_Integration_GoImage(t *testing.T) {
	dir := t.TempDir()
	if err := writeFileBytes(dir+"/go.mod", []byte("module example.com/x\ngo 1.21\n")); err != nil {
		t.Fatal(err)
	}

	cli, err := sandbox.NewCLI("podman", "docker.io/library/golang:1.21")
	if err != nil {
		t.Skipf("podman unavailable: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	spec := sandbox.Spec{Image: "docker.io/library/golang:1.21"}
	res := sandbox.Resolution{}
	verdict, err := VerifySandbox(ctx, cli, dir, spec, res)
	if err != nil {
		t.Fatalf("VerifySandbox error: %v", err)
	}
	if !verdict.OK {
		t.Errorf("golang:1.21 image: expected ok=true, got category=%q detail=%q",
			verdict.Category, verdict.Detail)
	}
}

// TestVerifySandbox_Integration_ToolchainlessImage runs against debian-slim
// (no Go toolchain) and expects toolchain_missing.
//
// Requires podman and the debian:bookworm-slim image to be present locally.
func TestVerifySandbox_Integration_ToolchainlessImage(t *testing.T) {
	dir := t.TempDir()
	if err := writeFileBytes(dir+"/go.mod", []byte("module example.com/x\ngo 1.21\n")); err != nil {
		t.Fatal(err)
	}

	cli, err := sandbox.NewCLI("podman", "docker.io/library/debian:bookworm-slim")
	if err != nil {
		t.Skipf("podman unavailable: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	spec := sandbox.Spec{Image: "docker.io/library/debian:bookworm-slim"}
	res := sandbox.Resolution{}
	verdict, err := VerifySandbox(ctx, cli, dir, spec, res)
	if err != nil {
		t.Fatalf("VerifySandbox error: %v", err)
	}
	if verdict.OK || verdict.Category != SmokeCategoryToolchainMissing {
		t.Errorf("debian:bookworm-slim: expected toolchain_missing, got ok=%v category=%q detail=%q",
			verdict.OK, verdict.Category, verdict.Detail)
	}
}
