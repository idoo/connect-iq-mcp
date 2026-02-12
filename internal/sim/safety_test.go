package sim

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestCheckDest(t *testing.T) {
	d := t.TempDir()
	key := filepath.Join(d, "developer_key.der")
	png := filepath.Join(d, "a.png")
	_ = os.WriteFile(key, []byte("K"), 0o600)
	_ = os.WriteFile(png, []byte("P"), 0o600)
	link := filepath.Join(d, "link.png")
	_ = os.Symlink(key, link)
	cases := []struct {
		path      string
		overwrite bool
		ok        bool
	}{
		{key, true, false},
		{filepath.Join(d, "new.png"), false, true},
		{filepath.Join(d, "NEW.PNG"), false, true},
		{png, false, false},
		{png, true, true},
		{link, true, false},
		{filepath.Join(d, "noext"), true, false},
	}
	for _, c := range cases {
		if err := CheckDest(c.path, c.overwrite, ".png"); (err == nil) != c.ok {
			t.Errorf("%s overwrite=%v: %v", c.path, c.overwrite, err)
		}
	}
}

func TestReadCrashLogTailRefusesSymlink(t *testing.T) {
	d := t.TempDir()
	target := filepath.Join(d, "secret")
	_ = os.WriteFile(target, []byte("Error: x\n"), 0o600)
	link := filepath.Join(d, "CIQ_LOG.YML")
	if err := os.Symlink(target, link); err != nil {
		t.Skip(err)
	}
	if _, _, err := ReadCrashLogTail(link, 100); err == nil {
		t.Fatal("symlinked crash log was read")
	}
	if s, _, err := ReadCrashLogTail(target, 100); err != nil || s != "Error: x\n" {
		t.Fatalf("own file: %q %v", s, err)
	}
	if _, _, err := ReadCrashLogTail(filepath.Join(d, "missing"), 100); !errors.Is(err, ErrNoCrashLog) {
		t.Fatalf("missing: %v", err)
	}
}

func TestIsSDKSimulator(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix layout")
	}
	sdk := t.TempDir()
	exe := filepath.Join(sdk, "bin", "ConnectIQ.app", "Contents", "MacOS", "simulator")
	if IsSDKSimulator(exe) {
		t.Fatal("accepted without monkeydo in <sdk>/bin")
	}
	_ = os.MkdirAll(filepath.Join(sdk, "bin"), 0o700)
	_ = os.WriteFile(filepath.Join(sdk, "bin", "monkeydo"), nil, 0o700)
	if !IsSDKSimulator(exe) || !IsSDKSimulator(filepath.Join(sdk, "bin", "simulator")) {
		t.Fatal("SDK simulator not recognised")
	}
	for _, p := range []string{"/usr/bin/simulator", "simulator", "", filepath.Join(sdk, "bin", "other")} {
		if IsSDKSimulator(p) {
			t.Errorf("%q accepted", p)
		}
	}
}

func TestFocusLostMapped(t *testing.T) {
	err := mapAXError([]byte("execution error: Error: focus_lost (-2700)"), errors.New("exit 1"))
	if !errors.Is(err, ErrFocusLost) {
		t.Fatalf("got %v", err)
	}
}
