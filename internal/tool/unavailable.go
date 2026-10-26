package tool

import (
	"context"
	"fmt"
	"io/fs"
	"os"
)

// Stand-ins installed by [Env.complete] for nil members.
func unavail(what string) error { return fmt.Errorf("%s: %w", what, ErrUnavailable) }

type (
	noPlanner  struct{}
	noStore    struct{}
	noToolsets struct{}
	noFiles    struct{}
)

func (noPlanner) Propose(context.Context, string, any, string, PlanPreview) (string, error) {
	return "", unavail("plan store")
}

func (noPlanner) Claim(context.Context, string, any) error { return ErrPreviewRequired }

func (noStore) Cursor(string, string, string) (string, error)  { return "", unavail("state store") }
func (noStore) SetCursor(string, string, string, string) error { return unavail("state store") }
func (noStore) Load(string, string, any) (bool, error)         { return false, unavail("state store") }
func (noStore) Save(string, string, any) error                 { return unavail("state store") }
func (noStore) Delete(string, string) error                    { return unavail("state store") }

func (noToolsets) List() []ToolsetInfo                       { return nil }
func (noToolsets) Enable(context.Context, ...Toolset) error  { return unavail("toolsets") }
func (noToolsets) Disable(context.Context, ...Toolset) error { return unavail("toolsets") }

func (noFiles) Resolve(string) (string, error)              { return "", unavail("file access") }
func (noFiles) ReadFile(string) ([]byte, error)             { return nil, unavail("file access") }
func (noFiles) WriteFile(string, []byte, fs.FileMode) error { return unavail("file access") }
func (noFiles) Open(string) (*os.File, error)               { return nil, unavail("file access") }
func (noFiles) Stat(string) (fs.FileInfo, error)            { return nil, unavail("file access") }
func (noFiles) WalkDir(string, fs.WalkDirFunc) error        { return unavail("file access") }
