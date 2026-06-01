package main

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func fixtureDir() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..", "testdata", "fixtures", "loansvc")
}

// silenceStdout redirects os.Stdout for the duration of the test so a command's
// JSON output does not pollute the test log.
func silenceStdout(t *testing.T) {
	t.Helper()
	orig := os.Stdout
	devnull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = devnull
	t.Cleanup(func() {
		os.Stdout = orig
		_ = devnull.Close()
	})
}

func TestRunVersion(t *testing.T) {
	silenceStdout(t)
	if err := run([]string{"version"}); err != nil {
		t.Fatal(err)
	}
}

func TestRunUnknownSubcommand(t *testing.T) {
	if err := run([]string{"frobnicate"}); err == nil {
		t.Fatal("expected an error for an unknown subcommand")
	}
}

// TestRunBoundaryCheckCurrent verifies the gate passes against the fixture's
// committed contract.
func TestRunBoundaryCheckCurrent(t *testing.T) {
	silenceStdout(t)
	if err := run([]string{"boundary", "--check", fixtureDir()}); err != nil {
		t.Fatalf("boundary --check on a current fixture should pass: %v", err)
	}
}

func TestRunGraph(t *testing.T) {
	silenceStdout(t)
	if err := run([]string{"graph", "--entry", "POST /loan-application", fixtureDir()}); err != nil {
		t.Fatalf("graph: %v", err)
	}
}

// TestRunBoundaryCheckStale verifies the gate fails when no contract is committed.
func TestRunBoundaryCheckStale(t *testing.T) {
	silenceStdout(t)
	t.Setenv("GOWORK", "off")
	dir := t.TempDir()
	write(t, dir, "go.mod", "module svc\n\ngo 1.24\n")
	write(t, dir, ".flowmap.yaml", "version: 1\nservice: svc\nclassify:\n  busPublish: [\"svc/bus#Publish\"]\n")
	write(t, dir, "bus/bus.go", "package bus\nfunc Publish(event string) {}\n")
	write(t, dir, "main.go", "package main\nimport \"svc/bus\"\nfunc main() { bus.Publish(\"x\") }\n")

	if err := run([]string{"boundary", "--check", dir}); err == nil {
		t.Fatal("boundary --check with no committed contract should fail")
	}
	// Writing it, then checking, should pass.
	if err := run([]string{"boundary", dir}); err != nil {
		t.Fatalf("boundary write: %v", err)
	}
	if err := run([]string{"boundary", "--check", dir}); err != nil {
		t.Fatalf("boundary --check after write should pass: %v", err)
	}
}

func write(t *testing.T, dir, rel, content string) {
	t.Helper()
	p := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestRunDiffIdentical(t *testing.T) {
	silenceStdout(t)
	g := filepath.Join(fixtureDir(), "flows", "testdata", "flows", "post_loan_application.golden.json")
	if err := run([]string{"diff", g, g}); err != nil {
		t.Fatalf("identical traces should diff cleanly, got: %v", err)
	}
}

func TestRunDiffDiffers(t *testing.T) {
	silenceStdout(t)
	dir := filepath.Join(fixtureDir(), "flows", "testdata", "flows")
	a := filepath.Join(dir, "post_loan_application.golden.json")
	b := filepath.Join(dir, "consume_payment_settled.golden.json")
	if err := run([]string{"diff", a, b}); err == nil {
		t.Fatal("expected a non-nil error (non-zero exit) when traces differ")
	}
}

func TestRunDiffBadArgs(t *testing.T) {
	if err := run([]string{"diff", "only-one.json"}); err == nil {
		t.Fatal("expected an error when not given exactly two files")
	}
}
