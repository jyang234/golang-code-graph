package schemadrift

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// LoadMigrations reads every *.sql file in dir into MigrationFile{Name, SQL}. It
// does NOT filter or order — Check excludes rollbacks and orders by version itself,
// so the result is independent of directory-read order. Subdirectories are ignored.
func LoadMigrations(dir string) ([]MigrationFile, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []MigrationFile
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(strings.ToLower(name), ".sql") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return nil, fmt.Errorf("read migration %s: %w", name, err)
		}
		out = append(out, MigrationFile{Name: name, SQL: string(b)})
	}
	return out, nil
}

// schemaTables replays the forward migrations in version order and applies their
// CREATE/DROP statements, returning the defined-table set, the count of forward
// files replayed, and any fail-closed parse caveats. The set is creates − drops;
// the caller folds in the library-owned tables. Replay order is a total order on
// the file names (numeric version, then repeatable, then other), so the result is
// deterministic regardless of input order.
func schemaTables(files []MigrationFile) (map[string]bool, int, []string) {
	ordered := forwardOrdered(files)
	defined := map[string]bool{}
	var caveats []string
	for _, f := range ordered {
		// Apply DDL in textual order within the file so a create-then-drop (or
		// drop-then-create) of the same table nets out correctly.
		for _, op := range scanDDL(f.SQL) {
			if op.create {
				defined[op.name] = true
			} else {
				delete(defined, op.name)
			}
		}
		if c := renameCaveat(f); c != "" {
			caveats = append(caveats, c)
		}
	}
	return defined, len(ordered), caveats
}

var (
	// versionedRe matches a Flyway versioned migration "V<version>__desc.sql".
	versionedRe = regexp.MustCompile(`^[Vv]([0-9]+(?:[._][0-9]+)*)__`)
	// repeatableRe matches a Flyway repeatable migration "R__desc.sql".
	repeatableRe = regexp.MustCompile(`^[Rr]__`)
)

// forwardOrdered filters out rollback files and returns the forward migrations in
// replay order: versioned (by numeric version), then repeatable, then any other
// .sql, each group broken by name. The ordering is total and name-derived, so it is
// run-independent.
func forwardOrdered(files []MigrationFile) []MigrationFile {
	type keyed struct {
		f   MigrationFile
		cat int // 0 versioned, 1 repeatable, 2 other
		ver []int
	}
	var ks []keyed
	for _, f := range files {
		if isRollback(f.Name) {
			continue
		}
		switch {
		case versionedRe.MatchString(f.Name):
			ks = append(ks, keyed{f: f, cat: 0, ver: parseVersion(f.Name)})
		case repeatableRe.MatchString(f.Name):
			ks = append(ks, keyed{f: f, cat: 1})
		default:
			ks = append(ks, keyed{f: f, cat: 2})
		}
	}
	sort.SliceStable(ks, func(i, j int) bool {
		if ks[i].cat != ks[j].cat {
			return ks[i].cat < ks[j].cat
		}
		if ks[i].cat == 0 {
			if c := compareVersion(ks[i].ver, ks[j].ver); c != 0 {
				return c < 0
			}
		}
		return ks[i].f.Name < ks[j].f.Name
	})
	out := make([]MigrationFile, len(ks))
	for i, k := range ks {
		out[i] = k.f
	}
	return out
}

// isRollback reports whether a file is a rollback (undo) script — excluded from the
// forward replay so its DROP does not apply as if it were forward. It matches the
// documented "*_rollback.sql" convention by SUFFIX, not a loose substring: a
// substring match would wrongly exclude a legitimate FORWARD migration whose
// description merely contains the word (e.g. V12__add_rollback_audit_table.sql),
// dropping its CREATE from the defined-schema set and manufacturing false drift.
func isRollback(name string) bool {
	return strings.HasSuffix(strings.ToLower(name), "_rollback.sql")
}

// parseVersion extracts the numeric version components from a "V<version>__" name,
// e.g. "V3_1__x.sql" → [3, 1]. Non-numeric or absent → nil (sorts first among
// versioned files, which only affects an already-malformed corpus).
func parseVersion(name string) []int {
	m := versionedRe.FindStringSubmatch(name)
	if m == nil {
		return nil
	}
	parts := strings.FieldsFunc(m[1], func(r rune) bool { return r == '.' || r == '_' })
	out := make([]int, 0, len(parts))
	for _, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			return nil
		}
		out = append(out, n)
	}
	return out
}

// compareVersion orders two version component slices, shorter-is-lower on a common
// prefix (V3 before V3.1).
func compareVersion(a, b []int) int {
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] != b[i] {
			if a[i] < b[i] {
				return -1
			}
			return 1
		}
	}
	switch {
	case len(a) < len(b):
		return -1
	case len(a) > len(b):
		return 1
	default:
		return 0
	}
}

// ddlOp is a single table-affecting DDL statement recovered from a migration.
type ddlOp struct {
	create bool // true = CREATE TABLE, false = DROP TABLE
	name   string
}

var (
	// createRe matches "CREATE [GLOBAL|LOCAL] [TEMP|TEMPORARY|UNLOGGED] TABLE
	// [IF NOT EXISTS] <name>", capturing a bare, quoted, or schema-qualified name
	// (cleanName normalizes it). The optional qualifiers matter: an UNLOGGED table is
	// a real persistent table, so missing it would drop it from the defined set and
	// manufacture false drift; TEMP tables are included for the same completeness-
	// favoring reason (a write to one must not read as drift).
	createRe = regexp.MustCompile("(?is)\\bCREATE\\s+(?:(?:GLOBAL|LOCAL)\\s+)?(?:(?:TEMP|TEMPORARY|UNLOGGED)\\s+)?TABLE\\s+(?:IF\\s+NOT\\s+EXISTS\\s+)?(\"[^\"]+\"|`[^`]+`|[a-zA-Z_][\\w$.]*)")
	// dropRe matches "DROP TABLE [IF EXISTS] <list>"; scanDDL runs it per statement
	// (the SQL is split on ';' after stripSQL), so the comma-split list cannot bleed
	// across a statement boundary even when a DROP lacks a trailing semicolon.
	dropRe = regexp.MustCompile(`(?is)\bDROP\s+TABLE\s+(?:IF\s+EXISTS\s+)?([^;]+)`)
	// alterRenameRe detects "ALTER TABLE ... RENAME TO ...", a table rename the
	// create/drop scan cannot follow — surfaced as a fail-closed caveat.
	alterRenameRe = regexp.MustCompile(`(?is)\bALTER\s+TABLE\b.*?\bRENAME\s+TO\b`)
)

// stripSQL removes SQL line comments (-- … EOL), block comments (/* … */), and
// single-quoted string literals (honoring ” escapes), replacing each with a space.
// Without this the DDL regexes would match a CREATE/DROP that appears inside a
// comment or a literal — a phantom CREATE masks real drift (a false "no drift", the
// unsound direction), a phantom DROP manufactures it. Double-quoted text is KEPT: in
// SQL "…" is a quoted IDENTIFIER (a table name), not a string literal, so stripping
// it would lose real table names.
func stripSQL(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		switch {
		case strings.HasPrefix(s[i:], "--"):
			if j := strings.IndexByte(s[i:], '\n'); j >= 0 {
				i += j // keep the newline (written on the next iteration)
			} else {
				i = len(s)
			}
			b.WriteByte(' ')
		case strings.HasPrefix(s[i:], "/*"):
			if j := strings.Index(s[i+2:], "*/"); j >= 0 {
				i += j + 4
			} else {
				i = len(s)
			}
			b.WriteByte(' ')
		case s[i] == '\'':
			i++
			for i < len(s) {
				if s[i] == '\'' {
					if i+1 < len(s) && s[i+1] == '\'' {
						i += 2
						continue
					}
					i++
					break
				}
				i++
			}
			b.WriteByte(' ')
		default:
			b.WriteByte(s[i])
			i++
		}
	}
	return b.String()
}

// scanDDL returns the CREATE/DROP table operations in one migration's SQL, in
// textual order, so the replay nets create-then-drop correctly within a file. It
// strips comments/literals first and scans per ';'-delimited statement so a DROP
// list cannot swallow a following statement.
func scanDDL(sqlText string) []ddlOp {
	var out []ddlOp
	for _, stmt := range strings.Split(stripSQL(sqlText), ";") {
		out = append(out, scanStatement(stmt)...)
	}
	return out
}

// scanStatement recovers the CREATE/DROP table ops within one statement, in
// textual order.
func scanStatement(stmt string) []ddlOp {
	type positioned struct {
		pos int
		op  ddlOp
	}
	var ms []positioned
	for _, idx := range createRe.FindAllStringSubmatchIndex(stmt, -1) {
		name := cleanName(stmt[idx[2]:idx[3]])
		if name != "" {
			ms = append(ms, positioned{pos: idx[0], op: ddlOp{create: true, name: name}})
		}
	}
	for _, idx := range dropRe.FindAllStringSubmatchIndex(stmt, -1) {
		for _, name := range splitDropList(stmt[idx[2]:idx[3]]) {
			if name != "" {
				ms = append(ms, positioned{pos: idx[0], op: ddlOp{create: false, name: name}})
			}
		}
	}
	sort.SliceStable(ms, func(i, j int) bool { return ms[i].pos < ms[j].pos })
	out := make([]ddlOp, 0, len(ms))
	for _, m := range ms {
		out = append(out, m.op)
	}
	return out
}

// splitDropList turns a "DROP TABLE" target list ("a, b CASCADE") into clean names.
// Each comma-separated part's first token is the name; trailing words (CASCADE,
// RESTRICT) are ignored.
func splitDropList(list string) []string {
	var out []string
	for _, part := range strings.Split(list, ",") {
		fields := strings.Fields(part)
		if len(fields) == 0 {
			continue
		}
		out = append(out, cleanName(fields[0]))
	}
	return out
}

// renameCaveat returns a fail-closed disclosure when a migration renames a table:
// the renamed name may not appear as a CREATE, so the scan could miss it. Surfacing
// it is preferable to silently producing (or hiding) drift.
func renameCaveat(f MigrationFile) string {
	if alterRenameRe.MatchString(f.SQL) {
		return fmt.Sprintf("%s: ALTER TABLE ... RENAME — a renamed table may be unseen by the CREATE/DROP scan", f.Name)
	}
	return ""
}

// cleanName normalizes a DDL identifier to match the code-side table name: strip
// surrounding quotes/brackets, take the segment after the last "." (schema
// qualifier), and lower-case. (A dot inside a quoted identifier is a rare literal
// and is treated as a qualifier — a documented limitation.)
func cleanName(raw string) string {
	s := strings.TrimSpace(raw)
	s = strings.Trim(s, "\"`[]")
	if i := strings.LastIndex(s, "."); i >= 0 {
		s = s[i+1:]
	}
	s = strings.Trim(s, "\"`[]")
	return strings.ToLower(strings.TrimSpace(s))
}
