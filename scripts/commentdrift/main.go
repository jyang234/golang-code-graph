// Command commentdrift is an advisory, edit-time nudge against comment drift: it
// flags Go functions whose BODY changed in the staged diff while their doc
// comment — one that makes a CHECKABLE claim (always / never / sorted /
// deterministic / X before Y / …) — did NOT. The moment a maintainer edits a
// function is when they can still confirm the comment holds; catching drift here
// is far cheaper than rediscovering it during a later bug hunt.
//
// It is deliberately a NUDGE, not a gate. It cannot know whether a comment is
// correct, only that code moved under an unchanged assertion, so by default it
// prints its findings and exits 0. Set COMMENTDRIFT_STRICT=1 to make it exit
// non-zero (e.g. to wire a hard check once the signal is trusted). It compares
// the staged index against HEAD, so it is meant to run from a pre-commit hook
// (`make hooks`). The discipline it embodies — comment the WHY, pin the WHAT with
// a test — lives in the repo CLAUDE.md.
package main

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"os"
	"os/exec"
	"strings"
)

// assertionWords are the markers of a checkable claim. A doc comment containing
// none of these is treated as prose (intent/context) and never flagged — that
// keeps the signal high and the false-alarm rate near zero, the same bar the
// project's CI gates hold ("neither lets an AI judge").
var assertionWords = []string{
	"always", "never", "must", "guarantee", "guaranteed", "sorted", "deterministic",
	"byte-identical", "invariant", "idempotent", "exactly", "every", "cannot",
	"in order", "before", "precede", "non-nil", "total order", "race", "stable",
	"deduplicate", "dedupe", "canonical", "fail closed", "fail-closed",
}

type fnInfo struct {
	doc  string // doc comment text (godoc-normalized)
	body string // pretty-printed body, so reformatting alone is not "a change"
}

func main() {
	files, err := stagedGoFiles()
	if err != nil {
		// A git failure must not block a commit on an advisory check.
		fmt.Fprintln(os.Stderr, "commentdrift: skipping ("+err.Error()+")")
		return
	}

	var findings []string
	for _, path := range files {
		staged, err := gitShow(":" + path) // index (what is being committed)
		if err != nil {
			continue
		}
		head, err := gitShow("HEAD:" + path) // last committed version
		if err != nil {
			continue // new file: no prior comment to drift from
		}
		findings = append(findings, findDrift(path, head, staged)...)
	}

	if len(findings) == 0 {
		return
	}
	fmt.Fprintln(os.Stderr, "commentdrift: a function body changed but its asserting doc comment did not —")
	fmt.Fprintln(os.Stderr, "confirm the comment still holds (update it, or pin the claim with a test):")
	for _, f := range findings {
		fmt.Fprintln(os.Stderr, f)
	}
	if os.Getenv("COMMENTDRIFT_STRICT") != "" {
		os.Exit(1)
	}
}

// stagedGoFiles lists added/modified .go files in the index, excluding generated
// and vendored trees where doc comments are not authored here.
func stagedGoFiles() ([]string, error) {
	out, err := run("git", "diff", "--cached", "--name-only", "--diff-filter=AM", "--", "*.go")
	if err != nil {
		return nil, err
	}
	var files []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "vendor/") || strings.HasPrefix(line, "testdata/") {
			continue
		}
		files = append(files, line)
	}
	return files, nil
}

// findDrift reports functions whose body changed between oldSrc and newSrc while
// an asserting doc comment stayed identical — the drift signal. It is pure (no
// git, no I/O) so the rule is unit-testable.
func findDrift(path, oldSrc, newSrc string) []string {
	oldFns := parseFns(path, oldSrc)
	newFns := parseFns(path, newSrc)
	var findings []string
	for key, nf := range newFns {
		of, ok := oldFns[key]
		if !ok {
			continue // newly added function
		}
		if of.body == nf.body {
			continue // body unchanged
		}
		if nf.doc == "" || of.doc != nf.doc {
			continue // no doc, or the doc was updated alongside the body
		}
		if !makesClaim(nf.doc) {
			continue // prose, not a checkable assertion
		}
		findings = append(findings,
			fmt.Sprintf("  %s  %s\n      claim: %q", path, key, firstLine(nf.doc)))
	}
	return findings
}

// parseFns maps each top-level function/method (keyed by receiver type + name) to
// its doc comment and pretty-printed body. A parse error yields an empty map, so
// a half-written file simply produces no findings rather than a spurious one.
func parseFns(path, src string) map[string]fnInfo {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, src, parser.ParseComments)
	if err != nil {
		return nil
	}
	out := map[string]fnInfo{}
	for _, decl := range file.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok || fd.Body == nil {
			continue
		}
		out[fnKey(fd)] = fnInfo{doc: fd.Doc.Text(), body: render(fset, fd.Body)}
	}
	return out
}

func fnKey(fd *ast.FuncDecl) string {
	if fd.Recv != nil && len(fd.Recv.List) > 0 {
		return recvType(fd.Recv.List[0].Type) + "." + fd.Name.Name
	}
	return fd.Name.Name
}

// recvType renders a receiver type to a stable string ("T", "*T") so a method
// keeps the same key across edits.
func recvType(expr ast.Expr) string {
	switch t := expr.(type) {
	case *ast.StarExpr:
		return "*" + recvType(t.X)
	case *ast.Ident:
		return t.Name
	case *ast.IndexExpr: // generic receiver T[P]
		return recvType(t.X)
	case *ast.IndexListExpr:
		return recvType(t.X)
	default:
		return "?"
	}
}

func render(fset *token.FileSet, node ast.Node) string {
	var buf bytes.Buffer
	if err := printer.Fprint(&buf, fset, node); err != nil {
		return ""
	}
	return buf.String()
}

func makesClaim(doc string) bool {
	low := strings.ToLower(doc)
	for _, w := range assertionWords {
		if strings.Contains(low, w) {
			return true
		}
	}
	return false
}

func firstLine(doc string) string {
	if i := strings.IndexByte(doc, '\n'); i >= 0 {
		return strings.TrimSpace(doc[:i])
	}
	return strings.TrimSpace(doc)
}

func gitShow(spec string) (string, error) { return run("git", "show", spec) }

func run(name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	var out, errBuf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errBuf
	if err := cmd.Run(); err != nil {
		return "", err
	}
	return out.String(), nil
}
