package sandbox

import (
	"slices"
	"strings"
	"testing"
)

func TestBuildBwrapArgsSecurityFlags(t *testing.T) {
	args := buildBwrapArgs(bwrapParams{
		workspace: "/tmp/ws",
		network:   "none",
		env:       []string{"FOO=bar", "BAZ=qux"},
		cmd:       []string{"sh", "-c", "echo hi"},
	})

	mustContainSeq(t, args, "--unshare-all")
	mustContainSeq(t, args, "--die-with-parent")
	mustContainSeq(t, args, "--new-session")
	mustContainSeq(t, args, "--clearenv")
	mustContainSeq(t, args, "--setenv", "HOME", "/tmp")
	mustContainSeq(t, args, "--proc", "/proc")
	mustContainSeq(t, args, "--dev", "/dev")
	mustContainSeq(t, args, "--tmpfs", "/tmp")
	mustContainSeq(t, args, "--tmpfs", "/")
	mustContainSeq(t, args, "--bind", "/tmp/ws", workspaceMount)
	mustContainSeq(t, args, "--chdir", workspaceMount)
	mustContainSeq(t, args, "--setenv", "FOO", "bar")
	mustContainSeq(t, args, "--setenv", "BAZ", "qux")

	// The fixed allowlist must be present as read-only binds, and nothing
	// else claiming to be $HOME, /root, or a wholesale /etc.
	for _, host := range fixedROAllowlist {
		mustContainSeq(t, args, "--ro-bind", host, host)
	}
	for i, a := range args {
		if a == "--ro-bind" || a == "--bind" {
			if i+1 >= len(args) {
				continue
			}
			target := args[i+1]
			if target == "/root" || target == "/etc" {
				t.Errorf("must never bind %q wholesale; args=%q", target, args)
			}
			if strings.Contains(target, "/home/") {
				t.Errorf("must never bind a host $HOME path; got %q in args=%q", target, args)
			}
		}
	}

	// Network default ("none") must NOT restore the network namespace.
	if slices.Contains(args, "--share-net") {
		t.Errorf("network=none must not add --share-net; args=%q", args)
	}

	// Command tail must be exactly the spec command, in order, at the end.
	tail := args[len(args)-3:]
	if !slices.Equal(tail, []string{"sh", "-c", "echo hi"}) {
		t.Errorf("command tail = %q, want sh -c 'echo hi'", tail)
	}
}

func TestBuildBwrapArgsNetworkNoneVsHost(t *testing.T) {
	none := buildBwrapArgs(bwrapParams{workspace: "/ws", network: "none", cmd: []string{"true"}})
	if slices.Contains(none, "--share-net") {
		t.Errorf("network=none must not share the net namespace; args=%q", none)
	}
	if slices.Contains(none, "/etc/resolv.conf") {
		t.Errorf("network=none must not bind resolv.conf; args=%q", none)
	}

	host := buildBwrapArgs(bwrapParams{workspace: "/ws", network: "host", cmd: []string{"true"}})
	mustContainSeq(t, host, "--share-net")
	mustContainSeq(t, host, "--ro-bind-try", "/etc/resolv.conf", "/etc/resolv.conf")
}

func TestBuildBwrapArgsWorkspaceIsOnlyWritableBind(t *testing.T) {
	args := buildBwrapArgs(bwrapParams{
		workspace: "/tmp/ws",
		network:   "none",
		cmd:       []string{"true"},
		roMounts:  []ROMount{{HostPath: "/host/modcache", ContainerPath: "/modcache"}},
		rwMounts:  []ROMount{{HostPath: "/host/prefetch", ContainerPath: "/prefetch"}},
	})

	// The RO mount must render via --ro-bind, never --bind.
	mustContainSeq(t, args, "--ro-bind", "/host/modcache", "/modcache")
	// The RW mount (prefetch only) is the one exception permitted to be
	// writable via --bind.
	mustContainSeq(t, args, "--bind", "/host/prefetch", "/prefetch")
	// The workspace itself must be writable.
	mustContainSeq(t, args, "--bind", "/tmp/ws", workspaceMount)

	// Count writable binds: only the workspace and the explicit RWMount.
	writable := 0
	for i, a := range args {
		if a == "--bind" && i+1 < len(args) {
			writable++
		}
	}
	if writable != 2 {
		t.Errorf("expected exactly 2 writable --bind entries (workspace + rwMount), got %d; args=%q", writable, args)
	}
}

func TestBuildBwrapArgsSetupCmds(t *testing.T) {
	args := buildBwrapArgs(bwrapParams{
		workspace: "/ws",
		network:   "none",
		cmd:       []string{"node", "--version"},
		setupCmds: [][]string{{"npm", "ci", "--offline"}},
	})
	shIdx := slices.Index(args, "/bin/sh")
	if shIdx < 0 {
		t.Fatalf("expected /bin/sh in args; got %q", args)
	}
	if args[shIdx+1] != "-c" {
		t.Fatalf("expected -c after /bin/sh; got %q", args)
	}
	script := args[shIdx+2]
	if !strings.Contains(script, "'npm' 'ci' '--offline' || exit 125") {
		t.Errorf("script missing npm cmd: %q", script)
	}
	tail := args[len(args)-2:]
	if !slices.Equal(tail, []string{"node", "--version"}) {
		t.Errorf("original cmd not at tail; got %q", tail)
	}
}

func TestBuildBwrapArgsToolchainBinds(t *testing.T) {
	args := buildBwrapArgs(bwrapParams{
		workspace: "/ws",
		network:   "none",
		cmd:       []string{"true"},
		toolchainBinds: []ROMount{
			{HostPath: "/nix/store/xxx-go", ContainerPath: "/opt/bugbot-toolchains/go", Shared: true},
		},
	})
	mustContainSeq(t, args, "--ro-bind", "/nix/store/xxx-go", "/opt/bugbot-toolchains/go")
}

func TestBuildBwrapArgsMalformedEnvSkipped(t *testing.T) {
	args := buildBwrapArgs(bwrapParams{
		workspace: "/ws",
		network:   "none",
		cmd:       []string{"true"},
		env:       []string{"NOEQUALSSIGN", "GOOD=value"},
	})
	mustContainSeq(t, args, "--setenv", "GOOD", "value")
	if slices.Contains(args, "NOEQUALSSIGN") {
		t.Errorf("malformed env entry must be dropped, not passed through; args=%q", args)
	}
}

func TestValidateBwrapNetwork(t *testing.T) {
	tests := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"", "none", false},
		{"none", "none", false},
		{"host", "host", false},
		{"bridge", "", true},
		{"custom", "", true},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			got, err := validateBwrapNetwork(tc.in)
			if (err != nil) != tc.wantErr {
				t.Fatalf("validateBwrapNetwork(%q) err=%v wantErr=%v", tc.in, err, tc.wantErr)
			}
			if !tc.wantErr && got != tc.want {
				t.Errorf("validateBwrapNetwork(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestValidateBwrapMountsRejectsAllowlistCollision(t *testing.T) {
	if err := validateBwrapMounts([]ROMount{{HostPath: "/host/x", ContainerPath: "/usr"}}, nil); err == nil {
		t.Error("expected error mounting over the fixed /usr allowlist bind")
	}
	if err := validateBwrapMounts([]ROMount{{HostPath: "/host/x", ContainerPath: workspaceMount}}, nil); err == nil {
		t.Error("expected error mounting over /workspace")
	}
	if err := validateBwrapMounts([]ROMount{{HostPath: "/host/x", ContainerPath: "/modcache"}}, nil); err != nil {
		t.Errorf("non-colliding mount should validate cleanly, got %v", err)
	}
}

func TestSplitEnvKV(t *testing.T) {
	key, value, ok := splitEnvKV("FOO=bar")
	if !ok || key != "FOO" || value != "bar" {
		t.Errorf("splitEnvKV(FOO=bar) = %q %q %v", key, value, ok)
	}
	key, value, ok = splitEnvKV("FOO=bar=baz")
	if !ok || key != "FOO" || value != "bar=baz" {
		t.Errorf("splitEnvKV(FOO=bar=baz) = %q %q %v, want split on first =", key, value, ok)
	}
	if _, _, ok := splitEnvKV("NOEQUALS"); ok {
		t.Error("splitEnvKV(NOEQUALS) should report ok=false")
	}
}
