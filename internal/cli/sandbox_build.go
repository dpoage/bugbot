package cli

import (
	"context"
	"embed"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/template"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/dpoage/bugbot/internal/config"
	"github.com/dpoage/bugbot/internal/ingest"
	"github.com/dpoage/bugbot/internal/sandbox"
)

//go:embed templates/sandbox_build.Dockerfile.tmpl templates/sandbox_build.build.sh.tmpl
var sandboxBuildTemplates embed.FS

const (
	// defaultBazelVersion is baked when the target repo has no .bazelversion.
	// It tracks the version proven in the aethon reference recipe.
	defaultBazelVersion = "8.4.2"

	// Container-side paths baked into the image. They are fixed (not user
	// flags) so the system /etc/bazel.bazelrc and the build mounts agree.
	sandboxVendorMount    = "/vendor"
	sandboxRepoCacheMount = "/bazel-repo-cache"
	sandboxDiskCacheMount = "/bazel-cache"
)

// sandboxWarmTestArgs is the canonical deterministic Bazel test command run to
// warm the disk cache (shared contract, epic bugbot-2tl). It MUST match the
// argv the repro/verify pipeline runs so the warmed cache actually hits at run
// time: build only test targets + deps (so host-incompatible targets like an
// arm64-only //docker:image are skipped) and surface failing-test output.
var sandboxWarmTestArgs = []string{"bazel", "test", "--build_tests_only", "--test_output=errors", "//..."}

// sandboxBuildFacts is the fully-resolved input to the Dockerfile/build.sh
// templates. Every field is a substituted fact so the templates carry no
// detection logic — the command resolves all defaults up front and the
// templates are pure text/template renders.
type sandboxBuildFacts struct {
	RepoDir       string // absolute path to the target repository
	RepoName      string // filepath.Base(RepoDir)
	OutDir        string // where Dockerfile + build.sh are written (--out)
	BaseImage     string // FROM line; from prime's recommendImage
	BazelVersion  string // pinned Bazel version baked into the image
	ImageTag      string // final committed image tag (--image)
	VendorDir     string // host `bazel vendor` tree (--vendor-dir)
	RepoCachePath string // host repository cache; "" => build.sh resolves it
	Runtime       string // container runtime default for build.sh (podman|docker)

	// Container-side paths (template substitution).
	VendorMount    string
	RepoCacheMount string
	DiskCacheMount string

	// WarmTest is the offline test command joined to a single string for the
	// build.sh warm-cache step.
	WarmTest string
}

// newSandboxCmd returns the `bugbot sandbox` parent command. Subcommands group
// everything that produces or manages the container image Bugbot uses for
// offline (network=none) verify/reproduce runs.
func newSandboxCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "sandbox",
		Short: "Build and manage Bugbot sandbox images",
		Long: `sandbox groups commands that produce the container image Bugbot uses for
offline (network=none) verify/reproduce runs.`,
		Args: cobra.NoArgs,
	}
	cmd.AddCommand(newSandboxBuildCmd())
	return cmd
}

// newSandboxBuildCmd returns the `bugbot sandbox build` subcommand. It
// generalizes a hand-built Bazel sandbox recipe into bugbot tooling: it
// scaffolds a parameterized Dockerfile + build.sh that bake a repo's vendored
// external deps and prefetched repository cache into a base layer, then warm
// the disk cache via an offline `bazel test` and commit it as the final image's
// writable layer. Without --run it only scaffolds and prints; with --run it
// orchestrates the real build.
func newSandboxBuildCmd() *cobra.Command {
	var (
		target       string
		outDir       string
		imageTag     string
		bazelVersion string
		vendorDir    string
		doRun        bool
	)

	cmd := &cobra.Command{
		Use:   "build",
		Short: "Scaffold (and optionally build) an offline Bazel sandbox image",
		Long: `build generalizes a hand-built Bazel sandbox recipe into bugbot tooling so a
Bazel monorepo gets a correct offline (network=none) sandbox image without
hand-rolling one.

It renders a parameterized Dockerfile + build.sh into --out. The two-phase
recipe they encode:

  1. Base layer: bake this repo's vendored external deps (the ` + "`bazel vendor`" + ` tree)
     and prefetched content-addressed repository cache into the image. They are
     baked rather than mounted because Bazel's vendor mode and disk cache need a
     writable surface, which Bugbot's read-only mounts cannot provide.
  2. Warm layer: run ` + "`bazel test --build_tests_only //...`" + ` once under
     network=none to prove the image builds + tests fully offline AND to warm
     the disk cache, then commit it as the final image's writable layer. Every
     Bugbot run then starts warm through the per-run container overlay.

Without --run it scaffolds + prints next steps only (it never shells out). With
--run it orchestrates vendor -> build -> warm run -> commit. It also refreshes
sandbox.image in the target's bugbot.yaml to the built tag (other keys
preserved).`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			repoDir, err := filepath.Abs(target)
			if err != nil {
				return fmt.Errorf("resolve target %q: %w", target, err)
			}

			// 1. Detect build systems; require Bazel.
			systems := ingest.DetectBuildSystems(repoDir)
			if !containsBuildSystem(systems, ingest.BuildSystemBazel) {
				return nonBazelSandboxError(systems)
			}

			// 2. Gather facts (resolve every default).
			facts := gatherSandboxBuildFacts(repoDir, outDir, imageTag, bazelVersion, vendorDir, systems)

			w := cmd.OutOrStdout()

			// 3 + 4. Render templates, write scaffold, print next steps. No exec.
			if err := scaffoldSandboxBuild(w, facts); err != nil {
				return err
			}

			// 5. Refresh sandbox.image in <repo>/bugbot.yaml (preserve other keys).
			if err := refreshSandboxImage(w, repoDir, facts.ImageTag); err != nil {
				return err
			}

			// 6. WITHOUT --run we are done (scaffold-only never shells out). WITH
			// --run, hand off to the orchestration seam — a package var so unit
			// tests can prove the scaffold-only path never reaches it.
			if doRun {
				return execOrchestration(cmd.Context(), w, facts)
			}
			return nil
		},
	}

	addTargetFlag(cmd, &target)
	cmd.Flags().StringVar(&outDir, "out", "",
		"output directory for the scaffold (default <target>/bugbot-sandbox)")
	cmd.Flags().StringVar(&imageTag, "image", "",
		"final image tag (default localhost/<repo>-bugbot-sandbox:latest)")
	cmd.Flags().StringVar(&bazelVersion, "bazel-version", "",
		"Bazel version to bake (default <target>/.bazelversion if present, else "+defaultBazelVersion+")")
	cmd.Flags().StringVar(&vendorDir, "vendor-dir", "",
		"host bazel vendor dir (default $HOME/.cache/<repo>-bugbot/vendor)")
	cmd.Flags().BoolVar(&doRun, "run", false,
		"actually build the image (vendor -> build -> warm run -> commit); without it, scaffold + print only")

	return cmd
}

// gatherSandboxBuildFacts resolves every command default into a complete fact
// set. It is pure aside from filesystem probes (recommendImage/.bazelversion/
// runtime detection), so the rendered output is deterministic given the repo.
func gatherSandboxBuildFacts(repoDir, outDir, imageTag, bazelVersion, vendorDir string, systems []ingest.BuildSystem) sandboxBuildFacts {
	repoName := filepath.Base(repoDir)

	if outDir == "" {
		outDir = filepath.Join(repoDir, "bugbot-sandbox")
	} else if abs, err := filepath.Abs(outDir); err == nil {
		outDir = abs
	}
	if imageTag == "" {
		imageTag = fmt.Sprintf("localhost/%s-bugbot-sandbox:latest", repoName)
	}
	if bazelVersion == "" {
		bazelVersion = detectBazelVersion(repoDir)
	}
	if vendorDir == "" {
		vendorDir = filepath.Join(sandboxHomeDir(), ".cache", repoName+"-bugbot", "vendor")
	}

	// Use only the image value from prime's recommendation, not its note text.
	baseImage, _ := recommendImage(systems, goModVersion(repoDir))

	runtime, ok := sandbox.Detect()
	if !ok {
		runtime = "podman"
	}

	return sandboxBuildFacts{
		RepoDir:        repoDir,
		RepoName:       repoName,
		OutDir:         outDir,
		BaseImage:      baseImage,
		BazelVersion:   bazelVersion,
		ImageTag:       imageTag,
		VendorDir:      vendorDir,
		RepoCachePath:  "", // build.sh resolves it via `bazelisk info repository_cache`
		Runtime:        runtime,
		VendorMount:    sandboxVendorMount,
		RepoCacheMount: sandboxRepoCacheMount,
		DiskCacheMount: sandboxDiskCacheMount,
		WarmTest:       strings.Join(sandboxWarmTestArgs, " "),
	}
}

// scaffoldSandboxBuild renders both templates, writes them into f.OutDir, and
// prints next steps. It performs NO exec — this is the entire scaffold-only
// code path.
func scaffoldSandboxBuild(w io.Writer, f sandboxBuildFacts) error {
	if err := os.MkdirAll(f.OutDir, 0o755); err != nil {
		return fmt.Errorf("create out dir %s: %w", f.OutDir, err)
	}

	dockerfile, err := renderSandboxTemplate("templates/sandbox_build.Dockerfile.tmpl", f)
	if err != nil {
		return err
	}
	buildSh, err := renderSandboxTemplate("templates/sandbox_build.build.sh.tmpl", f)
	if err != nil {
		return err
	}

	dockerfilePath := filepath.Join(f.OutDir, "Dockerfile")
	buildShPath := filepath.Join(f.OutDir, "build.sh")
	if err := os.WriteFile(dockerfilePath, []byte(dockerfile), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", dockerfilePath, err)
	}
	if err := os.WriteFile(buildShPath, []byte(buildSh), 0o755); err != nil {
		return fmt.Errorf("write %s: %w", buildShPath, err)
	}

	printSandboxNextSteps(w, f, dockerfilePath, buildShPath)
	return nil
}

// renderSandboxTemplate reads and renders one embedded template against f. It
// parses the content directly (rather than ParseFS) to avoid template-naming
// surprises, and errors on any unknown field so a typo never ships a
// `<no value>` placeholder into a generated file.
func renderSandboxTemplate(name string, f sandboxBuildFacts) (string, error) {
	content, err := sandboxBuildTemplates.ReadFile(name)
	if err != nil {
		return "", fmt.Errorf("read template %s: %w", name, err)
	}
	tmpl, err := template.New(filepath.Base(name)).Option("missingkey=error").Parse(string(content))
	if err != nil {
		return "", fmt.Errorf("parse template %s: %w", name, err)
	}
	var buf strings.Builder
	if err := tmpl.Execute(&buf, f); err != nil {
		return "", fmt.Errorf("render template %s: %w", name, err)
	}
	return buf.String(), nil
}

// printSandboxNextSteps prints the resolved facts and the manual workflow.
func printSandboxNextSteps(w io.Writer, f sandboxBuildFacts, dockerfilePath, buildShPath string) {
	_, _ = fmt.Fprintf(w, "Wrote offline Bazel sandbox scaffold:\n")
	_, _ = fmt.Fprintf(w, "  %s\n", dockerfilePath)
	_, _ = fmt.Fprintf(w, "  %s\n\n", buildShPath)

	_, _ = fmt.Fprintf(w, "Facts:\n")
	_, _ = fmt.Fprintf(w, "  base image : %s\n", f.BaseImage)
	_, _ = fmt.Fprintf(w, "  bazel      : %s\n", f.BazelVersion)
	_, _ = fmt.Fprintf(w, "  image tag  : %s\n", f.ImageTag)
	_, _ = fmt.Fprintf(w, "  vendor dir : %s\n\n", f.VendorDir)

	_, _ = fmt.Fprintf(w, "Next steps:\n")
	_, _ = fmt.Fprintf(w, "  1. Vendor this repo's external deps (online, once):\n")
	_, _ = fmt.Fprintf(w, "       bazel vendor --vendor_dir=%s //...\n", f.VendorDir)
	_, _ = fmt.Fprintf(w, "  2. Build + warm the image offline, then commit it:\n")
	_, _ = fmt.Fprintf(w, "       %s\n", buildShPath)
	_, _ = fmt.Fprintf(w, "  3. The image is committed as %s and wired into bugbot.yaml's\n", f.ImageTag)
	_, _ = fmt.Fprintf(w, "     sandbox.image (refreshed below). Re-run with --run to perform\n")
	_, _ = fmt.Fprintf(w, "     steps 1-2 automatically.\n\n")
}

// refreshSandboxImage rewrites only sandbox.image in <repoDir>/bugbot.yaml to
// imageTag, preserving every other key. It reuses the design_sandbox.go YAML
// helpers (spliceYAMLKey + unifiedDiff) but splices into the nested sandbox
// mapping so sibling keys (cpus, memory_mb, network, ...) survive. When the
// file is absent it prints a note and skips.
func refreshSandboxImage(w io.Writer, repoDir, imageTag string) error {
	path := filepath.Join(repoDir, config.DefaultFileName)

	existing, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		_, _ = fmt.Fprintf(w,
			"%s not found — skipping sandbox.image refresh; set sandbox.image: %s by hand.\n",
			path, imageTag)
		return nil
	}
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}

	var root yaml.Node
	if err := yaml.Unmarshal(existing, &root); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}

	doc := &root
	if doc.Kind == yaml.DocumentNode && len(doc.Content) > 0 {
		doc = doc.Content[0]
	}
	if doc.Kind != yaml.MappingNode {
		return fmt.Errorf("%s: top-level YAML is not a mapping", path)
	}

	imageVal := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: imageTag}

	var sandboxNode *yaml.Node
	for i := 0; i+1 < len(doc.Content); i += 2 {
		if doc.Content[i].Value == "sandbox" {
			sandboxNode = doc.Content[i+1]
			break
		}
	}
	switch {
	case sandboxNode == nil:
		// No sandbox block — create one carrying just image.
		sandboxMap := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		if _, err := spliceYAMLKey(sandboxMap, "image", imageVal); err != nil {
			return fmt.Errorf("splice image key: %w", err)
		}
		if _, err := spliceYAMLKey(&root, "sandbox", sandboxMap); err != nil {
			return fmt.Errorf("splice sandbox key: %w", err)
		}
	case sandboxNode.Kind != yaml.MappingNode:
		return fmt.Errorf("%s: sandbox is not a mapping", path)
	default:
		if _, err := spliceYAMLKey(sandboxNode, "image", imageVal); err != nil {
			return fmt.Errorf("splice image key: %w", err)
		}
	}

	out, err := yaml.Marshal(&root)
	if err != nil {
		return fmt.Errorf("marshal %s: %w", path, err)
	}

	diff := unifiedDiff(string(existing), string(out), path)
	if diff == "" {
		_, _ = fmt.Fprintf(w, "sandbox.image already %s in %s (no change).\n", imageTag, path)
		return nil
	}
	_, _ = fmt.Fprintln(w, diff)
	return os.WriteFile(path, out, 0o644)
}

// execOrchestration is the real, network-touching build path. It is a package
// var so unit tests can install a spy and prove the scaffold-only path never
// reaches it; the default is exercised only by the integration-tagged
// end-to-end test.
var execOrchestration = defaultExecOrchestration

// defaultExecOrchestration runs vendor -> build -> warm run (network=none) ->
// commit. Steps 2-4 are delegated to the generated build.sh (the single source
// of truth for the recipe); only the optional online vendor step runs here.
func defaultExecOrchestration(ctx context.Context, w io.Writer, f sandboxBuildFacts) error {
	if !dirExists(f.VendorDir) {
		_, _ = fmt.Fprintf(w, ">> vendoring external deps into %s (online)\n", f.VendorDir)
		vendor := exec.CommandContext(ctx, "bazel", "vendor", "--vendor_dir="+f.VendorDir, "//...")
		vendor.Dir = f.RepoDir
		vendor.Stdout = w
		vendor.Stderr = w
		if err := vendor.Run(); err != nil {
			return fmt.Errorf("bazel vendor: %w", err)
		}
	}

	sh := exec.CommandContext(ctx, "bash", filepath.Join(f.OutDir, "build.sh"))
	sh.Dir = f.RepoDir
	sh.Stdout = w
	sh.Stderr = w
	sh.Env = append(os.Environ(),
		"IMAGE="+f.ImageTag,
		"RUNTIME="+f.Runtime,
		"REPO="+f.RepoDir,
		"VENDOR_DIR="+f.VendorDir,
	)
	if err := sh.Run(); err != nil {
		return fmt.Errorf("build.sh: %w", err)
	}
	return nil
}

// nonBazelSandboxError reports that the target is not a Bazel repo and points
// the user at the dependency-strategy alternatives.
func nonBazelSandboxError(systems []ingest.BuildSystem) error {
	if len(systems) == 0 {
		return fmt.Errorf("sandbox build currently supports Bazel repos; no Bazel workspace marker " +
			"(MODULE.bazel/WORKSPACE) found in the target — non-Bazel ecosystems use " +
			"sandbox.dep_strategy off/host/fetch instead")
	}
	names := make([]string, 0, len(systems))
	for _, s := range systems {
		names = append(names, string(s))
	}
	return fmt.Errorf("sandbox build currently supports Bazel repos; detected %s — "+
		"use sandbox.dep_strategy off/host/fetch for those ecosystems",
		strings.Join(names, ", "))
}

// detectBazelVersion reads <repoDir>/.bazelversion (first line), falling back to
// defaultBazelVersion when it is absent or empty.
func detectBazelVersion(repoDir string) string {
	data, err := os.ReadFile(filepath.Join(repoDir, ".bazelversion"))
	if err != nil {
		return defaultBazelVersion
	}
	v := strings.TrimSpace(string(data))
	if v == "" {
		return defaultBazelVersion
	}
	if i := strings.IndexByte(v, '\n'); i >= 0 {
		v = strings.TrimSpace(v[:i])
	}
	return v
}

// sandboxHomeDir resolves the user's home directory for the default vendor dir.
func sandboxHomeDir() string {
	if h, err := os.UserHomeDir(); err == nil && h != "" {
		return h
	}
	return os.Getenv("HOME")
}

// dirExists reports whether path exists and is a directory.
func dirExists(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.IsDir()
}
