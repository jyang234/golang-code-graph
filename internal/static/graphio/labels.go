package graphio

import (
	"strings"

	"golang.org/x/tools/go/ssa"

	"github.com/jyang234/golang-code-graph/internal/static/features"
)

// dynamicLabel is the sentinel for a boundary argument the labeler could not read
// off a compile-time constant. It is the single source of truth shared by the
// label producers here and the consumers that must NOT treat an unreadable
// boundary as a concretely-named effect (see committedEffect).
const dynamicLabel = "<dynamic>"

// eventLabel is the published event name, or dynamicLabel if not constant.
func eventLabel(site ssa.CallInstruction) string {
	args := features.StringArgs(site)
	if len(args) >= 1 {
		if s, ok := features.ConstString(args[0]); ok {
			return s
		}
	}
	return dynamicLabel
}

// httpLabel is "peer method route" for a constant outbound call, else dynamicLabel.
func httpLabel(site ssa.CallInstruction) string {
	args := features.StringArgs(site)
	if len(args) >= 3 {
		p, ok1 := features.ConstString(args[0])
		m, ok2 := features.ConstString(args[1])
		r, ok3 := features.ConstString(args[2])
		if ok1 && ok2 && ok3 {
			return p + " " + m + " " + r
		}
	}
	return dynamicLabel
}

// dbLabel is the SQL operation and table ("SELECT applicants"), derived from the
// statement constant; it falls back to the DB method name. This is a light view
// label only — the behavioral pipeline owns canonical SQL normalization.
func dbLabel(site ssa.CallInstruction) string {
	args := features.StringArgs(site)
	if len(args) >= 1 {
		if stmt, ok := features.ConstString(args[0]); ok {
			if op, table := sqlOpTable(stmt); op != "" {
				if table != "" {
					return op + " " + table
				}
				return op
			}
		}
	}
	if c := site.Common().StaticCallee(); c != nil {
		return c.Name()
	}
	return "call"
}

// sqlOpTable extracts the leading SQL operation and primary table from a
// statement. It is a deliberately light heuristic.
func sqlOpTable(stmt string) (op, table string) {
	fields := strings.Fields(stmt)
	if len(fields) == 0 {
		return "", ""
	}
	op = strings.ToUpper(fields[0])
	switch op {
	case "SELECT", "DELETE":
		table = wordAfter(fields, "FROM")
	case "INSERT":
		table = wordAfter(fields, "INTO")
	case "UPDATE":
		if len(fields) >= 2 {
			table = trimSQL(fields[1])
		}
	default:
		return op, ""
	}
	return op, table
}

func wordAfter(fields []string, kw string) string {
	for i, f := range fields {
		if strings.EqualFold(f, kw) && i+1 < len(fields) {
			return trimSQL(fields[i+1])
		}
	}
	return ""
}

func trimSQL(s string) string { return strings.Trim(s, "(),;\"`") }
