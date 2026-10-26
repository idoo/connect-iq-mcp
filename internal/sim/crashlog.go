package sim

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// GarminDir returns the simulator's GARMIN data dir: on macOS under
// os.TempDir() (= DARWIN_USER_TEMP_DIR); on Linux/Windows the best known
// location (same layout under the temp dir), which may not exist.
func GarminDir() string {
	return filepath.Join(os.TempDir(), "com.garmin.connectiq", "GARMIN")
}

// CrashLogPath returns <GARMIN>/APPS/LOGS/CIQ_LOG.YML.
func CrashLogPath() string {
	return filepath.Join(GarminDir(), "APPS", "LOGS", "CIQ_LOG.YML")
}

// CrashFrame is one stack frame.
type CrashFrame struct {
	PC       string `json:"pc,omitempty"`
	File     string `json:"file,omitempty"`
	Line     int    `json:"line,omitempty"`
	Function string `json:"function,omitempty"`
}

// CrashEntry is one YAML document of CIQ_LOG.YML.
type CrashEntry struct {
	Error   string            `json:"error,omitempty"`
	Details string            `json:"details,omitempty"`
	Time    string            `json:"time,omitempty"`
	App     string            `json:"app,omitempty"`
	PRG     string            `json:"prg,omitempty"`
	Fields  map[string]string `json:"fields,omitempty"`
	Stack   []CrashFrame      `json:"stack"`
}

func unquote(v string) string {
	v = strings.TrimSpace(v)
	if len(v) >= 2 && (v[0] == '\'' && v[len(v)-1] == '\'' || v[0] == '"' && v[len(v)-1] == '"') {
		v = v[1 : len(v)-1]
	}
	return strings.ReplaceAll(v, "''", "'")
}

// ParseCrashLog parses CIQ_LOG.YML content (documents separated by "---",
// top-level "Key: value" pairs and a "Stack:" list of pc/File/Line/Function).
// A leading partial document (from reading only the tail) is dropped when it
// has no Error key. Entries are oldest first.
func ParseCrashLog(s string) []CrashEntry {
	var out []CrashEntry
	var cur *CrashEntry
	var frame *CrashFrame
	flush := func() {
		if cur != nil && (cur.Error != "" || len(cur.Stack) > 0) {
			out = append(out, *cur)
		}
		cur, frame = nil, nil
	}
	for _, raw := range strings.Split(s, "\n") {
		line := strings.TrimRight(raw, "\r ")
		if line == "---" {
			flush()
			cur = &CrashEntry{Fields: map[string]string{}, Stack: []CrashFrame{}}
			continue
		}
		if cur == nil || strings.TrimSpace(line) == "" {
			continue
		}
		indented := strings.HasPrefix(line, " ")
		t := strings.TrimSpace(line)
		if indented {
			if strings.HasPrefix(t, "- ") {
				cur.Stack = append(cur.Stack, CrashFrame{})
				frame = &cur.Stack[len(cur.Stack)-1]
				t = strings.TrimSpace(t[2:])
			}
			if frame == nil {
				continue
			}
			k, v, ok := strings.Cut(t, ":")
			if !ok {
				continue
			}
			v = unquote(v)
			switch k {
			case "pc":
				frame.PC = v
			case "File":
				frame.File = v
			case "Line":
				frame.Line, _ = strconv.Atoi(v)
			case "Function":
				frame.Function = v
			}
			continue
		}
		frame = nil
		k, v, ok := strings.Cut(t, ":")
		if !ok {
			continue
		}
		v = unquote(v)
		switch k {
		case "Error":
			cur.Error = v
		case "Details":
			cur.Details = v
		case "Time":
			cur.Time = v
		case "Appname":
			cur.App = v
		case "Filename":
			cur.PRG = v
		case "Stack":
		default:
			cur.Fields[k] = v
		}
	}
	flush()
	return out
}

// ErrNoCrashLog means CIQ_LOG.YML does not exist (yet).
var ErrNoCrashLog = errors.New("no CIQ_LOG.YML")

// ReadCrashLogTail reads at most max bytes from the end of path.
func ReadCrashLogTail(path string, max int64) (string, int64, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		hint := ""
		if runtime.GOOS != "darwin" {
			hint = " (path on " + runtime.GOOS + " is a best guess)"
		}
		return "", 0, fmt.Errorf("%w at %s%s", ErrNoCrashLog, path, hint)
	}
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return "", 0, err
	}
	off := fi.Size() - max
	if off < 0 {
		off = 0
	}
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		return "", 0, err
	}
	b, err := io.ReadAll(f)
	return string(b), fi.Size(), err
}
