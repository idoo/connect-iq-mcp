package sim

import (
	"regexp"
	"strconv"
	"strings"
)

// TestCase is one unit test outcome.
type TestCase struct {
	Name     string   `json:"name"`
	Status   string   `json:"status"` // PASS | FAIL | ERROR
	Messages []string `json:"messages,omitempty"`
}

// TestReport is a parsed `monkeydo -t` transcript. monkeydo's exit code is
// unreliable, so OK is derived from the transcript only.
type TestReport struct {
	OK         bool       `json:"ok"`
	Tests      []TestCase `json:"tests"`
	Ran        int        `json:"ran"`
	Passed     int        `json:"passed"`
	Failed     int        `json:"failed"`
	Errors     int        `json:"errors"`
	Summary    string     `json:"summary,omitempty"`
	Consistent bool       `json:"consistent"`
}

var (
	ansiRE     = regexp.MustCompile(`\x1b\[[0-9;?]*[A-Za-z]`)
	execRE     = regexp.MustCompile(`^Executing test (.+?)\.\.\.$`)
	loggerRE   = regexp.MustCompile(`^(DEBUG|INFO|WARNING|ERROR) \(\d{1,2}:\d{2}\):`)
	rowRE      = regexp.MustCompile(`^(.+?)\s+(PASS|FAIL|ERROR)$`)
	ranRE      = regexp.MustCompile(`^Ran (\d+) tests?`)
	summaryRE  = regexp.MustCompile(`^(PASSED|FAILED) \(passed=(\d+), failed=(\d+), errors=(\d+)\)`)
	equalsRE   = regexp.MustCompile(`^=+$`)
	dashesRE   = regexp.MustCompile(`^-+$`)
	statusLine = map[string]bool{"PASS": true, "FAIL": true, "ERROR": true}
)

// ParseTestTranscript parses monkeydo -t output (blocks, RESULTS table,
// summary) and cross-checks the three.
func ParseTestTranscript(out string) TestReport {
	var r TestReport
	var cur *TestCase
	results := false
	table := map[string]string{}
	var order []string
	summary := false
	for _, raw := range strings.Split(ansiRE.ReplaceAllString(out, ""), "\n") {
		line := strings.TrimSpace(strings.TrimRight(raw, "\r"))
		if line == "" || dashesRE.MatchString(line) {
			continue
		}
		if equalsRE.MatchString(line) {
			results, cur = true, nil
			continue
		}
		if m := summaryRE.FindStringSubmatch(line); m != nil {
			summary = true
			r.Summary = line
			r.Passed, _ = strconv.Atoi(m[2])
			r.Failed, _ = strconv.Atoi(m[3])
			r.Errors, _ = strconv.Atoi(m[4])
			continue
		}
		if m := ranRE.FindStringSubmatch(line); m != nil {
			r.Ran, _ = strconv.Atoi(m[1])
			continue
		}
		if results {
			if line == "RESULTS" || strings.HasPrefix(line, "Test:") {
				continue
			}
			if m := rowRE.FindStringSubmatch(line); m != nil {
				if _, ok := table[m[1]]; !ok {
					order = append(order, m[1])
				}
				table[m[1]] = m[2]
			}
			continue
		}
		if m := execRE.FindStringSubmatch(line); m != nil {
			r.Tests = append(r.Tests, TestCase{Name: m[1]})
			cur = &r.Tests[len(r.Tests)-1]
			continue
		}
		if cur == nil {
			continue
		}
		if statusLine[line] {
			cur.Status = line
			cur = nil
			continue
		}
		if !loggerRE.MatchString(line) {
			cur.Messages = append(cur.Messages, line)
		}
	}
	// Merge the table: fill missing statuses and tests only listed there.
	consistent := true
	idx := map[string]int{}
	for i, t := range r.Tests {
		idx[t.Name] = i
	}
	for _, name := range order {
		st := table[name]
		if i, ok := idx[name]; ok {
			if r.Tests[i].Status == "" {
				r.Tests[i].Status = st
			} else if r.Tests[i].Status != st {
				consistent = false
			}
		} else {
			r.Tests = append(r.Tests, TestCase{Name: name, Status: st})
		}
	}
	var p, f, e int
	for _, t := range r.Tests {
		switch t.Status {
		case "PASS":
			p++
		case "FAIL":
			f++
		case "ERROR":
			e++
		default:
			consistent = false
		}
	}
	if summary {
		if p != r.Passed || f != r.Failed || e != r.Errors {
			consistent = false
		}
	} else {
		r.Passed, r.Failed, r.Errors = p, f, e
		consistent = false
	}
	if r.Ran != 0 && r.Ran != len(r.Tests) {
		consistent = false
	}
	r.Consistent = consistent
	r.OK = summary && strings.HasPrefix(r.Summary, "PASSED") && r.Failed == 0 && r.Errors == 0 && f == 0 && e == 0 && len(r.Tests) > 0
	if r.Tests == nil {
		r.Tests = []TestCase{}
	}
	return r
}
