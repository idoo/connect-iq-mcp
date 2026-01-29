package ciqsdk

import (
	"archive/zip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// JungleError refuses a jungle that would make monkeyc read a file outside
// the root. Tool callers report it as invalid arguments.
type JungleError struct{ msg string }

func (e *JungleError) Error() string { return e.msg }

func jungleErr(format string, a ...any) error {
	return &JungleError{msg: fmt.Sprintf(format, a...)}
}

const jungleRule = "jungle paths must be relative and resolve, through any symlinks, inside the root %s (absolute, drive-letter, UNC and ~ paths are refused); move the files inside the root or drop the entry"

// Bounds on what CheckJungle reads.
const (
	maxJungleBytes  = 1 << 20
	maxJungleDepth  = 4
	maxCheckEntries = 100000
	maxBarrelFiles  = 10000
)

// implicitPaths are the files and folders monkeyc reads from the jungle's
// directory without the jungle naming them (default source, resources and
// manifest); "-" marks a prefix for qualified folders (resources-fenix7).
var implicitPaths = []string{"manifest.xml", "source", "source-", "resources", "resources-"}

// CheckJungle refuses a build whose jungle (default <projectDir>/monkey.jungle)
// could make monkeyc read anything outside root (empty root means
// projectDir). Values are read the way monkeyc reads them: one pair of
// surrounding quotes is stripped from each ;-element, `\` is a separator and
// $(...) references are expanded. Each must be relative and resolve, with
// symlinks followed physically (so link/.. is the link target's parent),
// inside root; `..` is fine while it stays there. Folders it names and the
// default ones are scanned for symlinks that leave root, and every *.jungle
// inside a .barrel on barrelPath must stay inside the barrel.
func CheckJungle(root, projectDir, jungle string) error {
	if root == "" {
		root = projectDir
	}
	if jungle == "" {
		jungle = "monkey.jungle"
	}
	if strings.Contains(jungle, ";") || strings.Contains(jungle, "$(") {
		return jungleErr("jungle path %s contains ';' or '$(', which monkeyc reads as a list or a variable; rename it", jungle)
	}
	if !filepath.IsAbs(jungle) {
		jungle = filepath.Join(projectDir, jungle)
	}
	rr, err := filepath.EvalSymlinks(root)
	if err != nil {
		return jungleErr("root %s: %v", root, err)
	}
	c := &confiner{root: rr, seen: map[string]bool{}}
	jr, err := filepath.EvalSymlinks(jungle)
	if errors.Is(err, fs.ErrNotExist) {
		// monkeyc fails on a missing jungle; only its defaults could be read.
		dir, err := filepath.EvalSymlinks(filepath.Dir(jungle))
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil || !c.inside(dir) {
			return jungleErr("jungle %s is outside the root %s; pass a jungle inside it", jungle, root)
		}
		return c.implicit(dir, jungle)
	}
	if err != nil {
		return jungleErr("jungle %s: %v", jungle, err)
	}
	if !c.inside(jr) {
		return jungleErr("jungle %s is outside the root %s; pass a jungle inside it", jungle, root)
	}
	if err := c.implicit(filepath.Dir(jr), jungle); err != nil {
		return err
	}
	return c.jungleFile(jr, jungle, 0)
}

type confiner struct {
	root    string // symlinks resolved
	seen    map[string]bool
	entries int
}

func (c *confiner) rule() string { return fmt.Sprintf(jungleRule, c.root) }

// inside reports whether the resolved path p is root or below it.
func (c *confiner) inside(p string) bool {
	rel, err := filepath.Rel(c.root, p)
	return err == nil && !filepath.IsAbs(rel) && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// normalize turns one jungle element into the relative slash path monkeyc
// reads, or says why it is refused.
func normalize(v string) (string, string) {
	if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
		v = v[1 : len(v)-1]
	}
	v = strings.ReplaceAll(v, `\`, "/")
	switch {
	case strings.ContainsAny(v, `"'`):
		return "", "has a quote monkeyc would not strip"
	case strings.HasPrefix(v, "/") || filepath.IsAbs(v):
		return "", "is an absolute or UNC path"
	case len(v) >= 2 && v[1] == ':':
		return "", "starts with a drive letter"
	case strings.HasPrefix(v, "~"):
		return "", "starts with ~"
	}
	return v, ""
}

func hasDotDot(rel string) bool {
	for _, seg := range strings.Split(rel, "/") {
		if seg == ".." {
			return true
		}
	}
	return false
}

// resolveRel resolves dir/rel physically (no lexical Clean first, so a
// symlink before .. is followed). A missing path resolves its longest
// existing prefix; the rest must then have no "..". It returns the resolved
// path ("" when missing) and whether it stays inside root.
func (c *confiner) resolveRel(dir, rel string) (string, bool, error) {
	comps := strings.Split(rel, "/")
	join := func(n int) string {
		if n == 0 {
			return dir
		}
		return dir + string(filepath.Separator) + strings.Join(comps[:n], string(filepath.Separator))
	}
	r, err := filepath.EvalSymlinks(join(len(comps)))
	if err == nil {
		return r, c.inside(r), nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return "", false, err
	}
	for n := len(comps) - 1; n >= 0; n-- {
		p, err := filepath.EvalSymlinks(join(n))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", false, err
		}
		return "", c.inside(p) && !hasDotDot(strings.Join(comps[n:], "/")), nil
	}
	return "", false, nil
}

func (c *confiner) jungleFile(path, label string, depth int) error {
	if depth > maxJungleDepth {
		return jungleErr("jungle %s: jungle files nested too deep", label)
	}
	data, err := readCapped(path)
	if err != nil {
		return jungleErr("jungle %s: %v", label, err)
	}
	paths, err := JunglePaths(data)
	if err != nil {
		return jungleErr("jungle %s: %v; %s", label, err, c.rule())
	}
	dir := filepath.Dir(path)
	for _, jp := range paths {
		rel, why := normalize(jp.Path)
		if why != "" {
			return jungleErr("jungle %s: %s = %s %s; %s", label, jp.Key, jp.Path, why, c.rule())
		}
		r, ok, err := c.resolveRel(dir, rel)
		if err != nil {
			return jungleErr("jungle %s: %s = %s: %v", label, jp.Key, jp.Path, err)
		}
		if !ok {
			return jungleErr("jungle %s: %s = %s resolves outside the root; %s", label, jp.Key, jp.Path, c.rule())
		}
		if r == "" {
			continue // nothing there for monkeyc to read
		}
		if err := c.target(r, label, jp, depth); err != nil {
			return err
		}
	}
	return nil
}

// target checks what a jungle value names once resolved inside the root.
func (c *confiner) target(path, label string, jp JunglePath, depth int) error {
	st, err := os.Stat(path)
	if err != nil {
		return jungleErr("jungle %s: %s = %s: %v", label, jp.Key, jp.Path, err)
	}
	if st.IsDir() {
		return c.walk(path, label, jp)
	}
	switch strings.ToLower(filepath.Ext(path)) {
	case ".jungle":
		return c.jungleFile(path, label+" -> "+jp.Path, depth+1)
	case ".barrel":
		return c.barrel(path, label, jp)
	}
	return nil
}

// walk refuses a folder holding a symlink that leaves the root, and checks
// barrels in it; symlinked folders inside the root are walked too.
func (c *confiner) walk(dir, label string, jp JunglePath) error {
	if c.seen[dir] {
		return nil
	}
	c.seen[dir] = true
	barrels := strings.EqualFold(lastKeyPart(jp.Key), "barrelPath")
	return filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return jungleErr("jungle %s: %s = %s: %v", label, jp.Key, jp.Path, err)
		}
		if c.entries++; c.entries > maxCheckEntries {
			return jungleErr("jungle %s: more than %d files under the folders it names; too many to check", label, maxCheckEntries)
		}
		if d.Type()&fs.ModeSymlink != 0 {
			r, err := filepath.EvalSymlinks(p)
			if errors.Is(err, fs.ErrNotExist) {
				return nil // dangling: nothing to read
			}
			if err != nil {
				return jungleErr("jungle %s: %s = %s: %v", label, jp.Key, jp.Path, err)
			}
			if !c.inside(r) {
				return jungleErr("jungle %s: %s = %s holds the symlink %s, which points outside the root; %s", label, jp.Key, jp.Path, p, c.rule())
			}
			st, err := os.Stat(r)
			if err != nil {
				return jungleErr("jungle %s: %s: %v", label, p, err)
			}
			if st.IsDir() {
				return c.walk(r, label, jp)
			}
			p = r
		} else if d.IsDir() {
			return nil
		}
		if barrels && strings.EqualFold(filepath.Ext(p), ".barrel") {
			return c.barrel(p, label, jp)
		}
		return nil
	})
}

// barrel checks every *.jungle inside a .barrel (a zip). Its paths are
// relative to the unpacked barrel, so they must stay inside it: the
// element rules plus no "..". An unreadable barrel is refused.
func (c *confiner) barrel(p, label string, jp JunglePath) error {
	zr, err := zip.OpenReader(p)
	if err != nil {
		return jungleErr("jungle %s: %s = %s: barrel %s cannot be read (%v); only valid barrels inside the root can be built", label, jp.Key, jp.Path, p, err)
	}
	defer zr.Close()
	if len(zr.File) > maxBarrelFiles {
		return jungleErr("jungle %s: barrel %s has more than %d files", label, p, maxBarrelFiles)
	}
	for _, f := range zr.File {
		if !strings.EqualFold(filepath.Ext(f.Name), ".jungle") {
			continue
		}
		data, err := readZipCapped(f)
		if err != nil {
			return jungleErr("jungle %s: barrel %s: %s cannot be read (%v)", label, p, f.Name, err)
		}
		inner := p + ":" + f.Name
		paths, err := JunglePaths(data)
		if err != nil {
			return jungleErr("jungle %s: %v", inner, err)
		}
		for _, bp := range paths {
			rel, why := normalize(bp.Path)
			if why == "" && hasDotDot(rel) {
				why = "leaves the barrel with .."
			}
			if why != "" {
				return jungleErr("jungle %s: %s = %s %s; paths inside a barrel must stay inside it", inner, bp.Key, bp.Path, why)
			}
		}
	}
	return nil
}

// implicit checks the default folders and manifest beside the jungle.
func (c *confiner) implicit(dir, label string) error {
	ents, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return jungleErr("jungle %s: %v", label, err)
	}
	for _, e := range ents {
		name := e.Name()
		match := false
		for _, n := range implicitPaths {
			if strings.HasSuffix(n, "-") && strings.HasPrefix(name, n) || name == n {
				match = true
				break
			}
		}
		if !match {
			continue
		}
		p := filepath.Join(dir, name)
		r, err := filepath.EvalSymlinks(p)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return jungleErr("jungle %s: %s: %v", label, p, err)
		}
		if !c.inside(r) {
			return jungleErr("jungle %s: the default %s is a symlink pointing outside the root; %s", label, p, c.rule())
		}
		if st, err := os.Stat(r); err == nil && st.IsDir() {
			if err := c.walk(r, label, JunglePath{Key: "default", Path: name}); err != nil {
				return err
			}
		}
	}
	return nil
}

func lastKeyPart(key string) string {
	if i := strings.LastIndexByte(key, '.'); i >= 0 {
		return key[i+1:]
	}
	return key
}

func readCapped(p string) ([]byte, error) {
	f, err := os.Open(p) //nolint:gosec // G304: p is a jungle path already resolved inside the root
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return capRead(f)
}

func readZipCapped(f *zip.File) ([]byte, error) {
	r, err := f.Open()
	if err != nil {
		return nil, err
	}
	defer r.Close()
	return capRead(r)
}

func capRead(r io.Reader) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, maxJungleBytes+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxJungleBytes {
		return nil, fmt.Errorf("larger than %d bytes", maxJungleBytes)
	}
	return b, nil
}
