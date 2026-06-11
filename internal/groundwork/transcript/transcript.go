// Package transcript reads and summarizes the MCP server's --log file
// (calls.jsonl) — the E4 measurement apparatus, and the evidence the MCP
// tiers 2–3 plan-of-record defers to: per-session query counts, the tool and
// service mix, whether agents make cross-service hops mid-session, and how
// often a tool error is followed by a corrected call.
//
// The summary counts usage; it cannot grade value. Whether an agent's
// conclusions cite card facts — E4's qualitative half — stays human-judged,
// and the rendered card says so rather than implying otherwise.
package transcript

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
)

// Entry is one transcript line: a session boundary (Init, written on MCP
// initialize) or one tool call with its resolution. Service is the name of
// the service that answered, "*" for a fleet-wide answer, and absent when
// resolution failed; IsError marks an isError tool result. Lines written by
// servers older than the resolution fields decode fine — both are optional.
type Entry struct {
	Init    bool            `json:"init,omitempty"`
	Call    json.RawMessage `json:"call,omitempty"`
	Service string          `json:"service,omitempty"`
	IsError bool            `json:"isError,omitempty"`
}

// Tool extracts the called tool's name from the raw call params.
func (e Entry) Tool() string {
	var c struct {
		Name string `json:"name"`
	}
	_ = json.Unmarshal(e.Call, &c)
	if c.Name == "" {
		return "(unnamed)"
	}
	return c.Name
}

// Load decodes a transcript, strictly: the format is this toolset's own, so
// a line it does not recognize is corruption (or a future field this reader
// has not been taught), and must fail loudly rather than skew the counts.
func Load(path string) ([]Entry, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	var out []Entry
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 1<<24)
	lineNo := 0
	for sc.Scan() {
		lineNo++
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		dec := json.NewDecoder(bytes.NewReader(line))
		dec.DisallowUnknownFields()
		var e Entry
		if err := dec.Decode(&e); err != nil {
			return nil, fmt.Errorf("transcript: %s:%d: %w", path, lineNo, err)
		}
		if !e.Init && e.Call == nil {
			return nil, fmt.Errorf("transcript: %s:%d: neither an init marker nor a call", path, lineNo)
		}
		out = append(out, e)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// Count is one tool's or service's usage tally.
type Count struct {
	Name   string `json:"name"`
	Calls  int    `json:"calls"`
	Errors int    `json:"errors"`
}

// Summary is the deterministic reading of one transcript.
type Summary struct {
	Sessions              int     `json:"sessions"`
	Calls                 int     `json:"calls"`
	Errors                int     `json:"errors"`
	ErrorsCorrected       int     `json:"errors_corrected"`
	CallsPerSessionMin    int     `json:"calls_per_session_min"`
	CallsPerSessionMedian float64 `json:"calls_per_session_median"`
	CallsPerSessionMax    int     `json:"calls_per_session_max"`
	Tools                 []Count `json:"tools"`
	Services              []Count `json:"services"`
	CrossServiceHops      int     `json:"cross_service_hops"`
	SessionsWithHop       int     `json:"sessions_with_hop"`
}

// fleetLabel and unresolvedLabel name the two non-service resolutions in the
// summary itself, so the JSON and the rendered card agree.
const (
	fleetLabel      = "(fleet-wide)"
	unresolvedLabel = "(unresolved)"
)

// Summarize computes the transcript's reading. Sessions split on init
// markers; calls logged before the first marker (a server that predates the
// marker, or a truncated file) form an implicit leading session. A
// cross-service hop is a call answered by a different concrete service than
// the previous concrete answer in the same session — fleet-wide and
// unresolved calls between them neither make nor break a hop. An error
// counts corrected when the same session's next call of the same tool
// succeeds.
func Summarize(entries []Entry) Summary {
	var sessions [][]Entry
	cur := []Entry{}
	leading := true
	for _, e := range entries {
		if e.Init {
			if !leading || len(cur) > 0 {
				sessions = append(sessions, cur)
			}
			cur, leading = []Entry{}, false
			continue
		}
		cur = append(cur, e)
	}
	if !leading || len(cur) > 0 {
		sessions = append(sessions, cur)
	}

	s := Summary{Sessions: len(sessions)}
	tools, services := map[string]*Count{}, map[string]*Count{}
	tally := func(m map[string]*Count, name string, isErr bool) {
		c := m[name]
		if c == nil {
			c = &Count{Name: name}
			m[name] = c
		}
		c.Calls++
		if isErr {
			c.Errors++
		}
	}
	var perSession []int
	for _, ses := range sessions {
		perSession = append(perSession, len(ses))
		lastConcrete := ""
		hopped := false
		for i, e := range ses {
			s.Calls++
			if e.IsError {
				s.Errors++
				for _, later := range ses[i+1:] {
					if later.Tool() == e.Tool() {
						if !later.IsError {
							s.ErrorsCorrected++
						}
						break
					}
				}
			}
			tally(tools, e.Tool(), e.IsError)
			switch e.Service {
			case "*":
				tally(services, fleetLabel, e.IsError)
			case "":
				tally(services, unresolvedLabel, e.IsError)
			default:
				tally(services, e.Service, e.IsError)
				if lastConcrete != "" && lastConcrete != e.Service {
					s.CrossServiceHops++
					hopped = true
				}
				lastConcrete = e.Service
			}
		}
		if hopped {
			s.SessionsWithHop++
		}
	}
	sort.Ints(perSession)
	if n := len(perSession); n > 0 {
		s.CallsPerSessionMin = perSession[0]
		s.CallsPerSessionMax = perSession[n-1]
		if n%2 == 1 {
			s.CallsPerSessionMedian = float64(perSession[n/2])
		} else {
			s.CallsPerSessionMedian = float64(perSession[n/2-1]+perSession[n/2]) / 2
		}
	}
	s.Tools = freeze(tools)
	s.Services = freeze(services)
	return s
}

func freeze(m map[string]*Count) []Count {
	out := make([]Count, 0, len(m))
	for _, c := range m {
		out = append(out, *c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Render prints the summary card.
func Render(s Summary) string {
	var b strings.Builder
	fmt.Fprintf(&b, "sessions: %d   tool calls: %d   errors: %d (%d corrected by a later same-tool call)\n",
		s.Sessions, s.Calls, s.Errors, s.ErrorsCorrected)
	if s.Sessions > 0 {
		fmt.Fprintf(&b, "calls per session: min %d, median %g, max %d\n",
			s.CallsPerSessionMin, s.CallsPerSessionMedian, s.CallsPerSessionMax)
	}
	if len(s.Tools) > 0 {
		b.WriteString("\ntool                 calls  errors\n")
		for _, c := range s.Tools {
			fmt.Fprintf(&b, "%-20s %5d  %6d\n", c.Name, c.Calls, c.Errors)
		}
	}
	if len(s.Services) > 0 {
		b.WriteString("\nservice                          calls  errors\n")
		for _, c := range s.Services {
			fmt.Fprintf(&b, "%-32s %5d  %6d\n", c.Name, c.Calls, c.Errors)
		}
	}
	fmt.Fprintf(&b, "\ncross-service hops: %d, in %d of %d session(s)\n", s.CrossServiceHops, s.SessionsWithHop, s.Sessions)
	b.WriteString("\nThese counts measure usage, not value: whether conclusions cite card\nfacts (E4's qualitative half) stays human-judged.\n")
	return b.String()
}
