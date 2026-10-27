package sim

import (
	"context"
	"image"
	"image/color"
	"net"
	"strings"
	"testing"
	"time"
)

func TestRingCursorsAndDrops(t *testing.T) {
	r := NewRing(3)
	if l, next, latest, d := r.Since(0, 0); l != nil || next != 0 || latest != 0 || d != 0 {
		t.Fatalf("empty: %v %d %d %d", l, next, latest, d)
	}
	for _, s := range []string{"a", "b", "c", "d", "e"} {
		r.Append("stdout", s)
	}
	l, next, latest, d := r.Since(0, 0)
	if len(l) != 3 || l[0].Text != "c" || l[0].Cursor != 3 || next != 5 || latest != 5 || d != 2 {
		t.Fatalf("since 0: %+v next=%d latest=%d dropped=%d", l, next, latest, d)
	}
	l, next, _, d = r.Since(3, 1)
	if len(l) != 1 || l[0].Text != "d" || next != 4 || d != 0 {
		t.Fatalf("since 3 limit 1: %+v next=%d dropped=%d", l, next, d)
	}
	if l, next, _, _ := r.Since(5, 0); len(l) != 0 || next != 5 {
		t.Fatalf("caught up: %+v next=%d", l, next)
	}
	if got := r.Text(); got != "c\nd\ne\n" {
		t.Fatalf("text %q", got)
	}
}

const transcript = "\x1b[32mExecuting test MyTests.testA...\x1b[0m\n" +
	"DEBUG (19:07): hello\n" +
	"PASS\n" +
	"------------------\n" +
	"Executing test MyTests.testB...\n" +
	"Assertion failed: expected 1 got 2\n" +
	"FAIL\n" +
	"==================\n" +
	"RESULTS\n" +
	"Test:                 Status:\n" +
	"MyTests.testA         PASS\n" +
	"MyTests.testB         FAIL\n" +
	"Ran 2 tests\n" +
	"\n" +
	"FAILED (passed=1, failed=1, errors=0)\n"

func TestParseTestTranscript(t *testing.T) {
	r := ParseTestTranscript(transcript)
	if r.OK || !r.Consistent || r.Ran != 2 || r.Passed != 1 || r.Failed != 1 || len(r.Tests) != 2 {
		t.Fatalf("%+v", r)
	}
	if r.Tests[1].Name != "MyTests.testB" || r.Tests[1].Status != "FAIL" || len(r.Tests[1].Messages) != 1 {
		t.Fatalf("testB %+v", r.Tests[1])
	}
	if len(r.Tests[0].Messages) != 0 {
		t.Fatalf("logger lines must be ignored: %+v", r.Tests[0])
	}
	pass := "Executing test T.a...\nPASS\n=====\nRESULTS\nT.a  PASS\nRan 1 test\nPASSED (passed=1, failed=0, errors=0)\n"
	if r := ParseTestTranscript(pass); !r.OK || !r.Consistent {
		t.Fatalf("pass %+v", r)
	}
	if r := ParseTestTranscript("Executing test T.a...\nPASS\n"); r.OK || r.Consistent {
		t.Fatalf("no summary must not be OK: %+v", r)
	}
}

const crashYML = `  - pc: 0x1
    File: /old.mc
---
Error: Not Enough Arguments Error
Details: 'Failed invoking <symbol>'
Time: 2026-10-26T01:47:09Z
Firmware-Version: '23.16'
Filename: FENIX7
Appname: Star Calendar
Stack: 
  - pc: 0x10004433
    File: /p/WFKLedger.mc
    Line: 131
    Function: historyFromDevice
  - pc: 0x1000d843
    File: /p/Renderer.mc
    Line: 235
    Function: update
---
Error: Watchdog Tripped Error
Appname: Other
Stack: 
`

func TestParseCrashLog(t *testing.T) {
	es := ParseCrashLog(crashYML)
	if len(es) != 2 {
		t.Fatalf("entries %d: %+v", len(es), es)
	}
	e := es[0]
	if e.Error != "Not Enough Arguments Error" || e.Details != "Failed invoking <symbol>" || e.App != "Star Calendar" ||
		e.PRG != "FENIX7" || e.Fields["Firmware-Version"] != "23.16" || len(e.Stack) != 2 {
		t.Fatalf("%+v", e)
	}
	if f := e.Stack[1]; f.File != "/p/Renderer.mc" || f.Line != 235 || f.Function != "update" || f.PC != "0x1000d843" {
		t.Fatalf("frame %+v", f)
	}
	if es[1].Error != "Watchdog Tripped Error" || len(es[1].Stack) != 0 {
		t.Fatalf("%+v", es[1])
	}
}

// synthetic device art: 60x50 opaque patterned bezel with a 20x20 round-ish
// transparent hole at (20,15).
func synthArt() DeviceArt {
	art := image.NewNRGBA(image.Rect(0, 0, 60, 50))
	hole := image.Rect(20, 15, 40, 35)
	for y := range 50 {
		for x := range 60 {
			p := image.Point{x, y}
			dx, dy := float64(x)-29.5, float64(y)-24.5
			if p.In(hole) && dx*dx+dy*dy <= 100 {
				continue // transparent
			}
			art.Set(x, y, color.NRGBA{uint8(x * 4), uint8(y * 5), uint8((x * y) % 256), 255})
		}
	}
	return DeviceArt{Image: art, Display: hole}
}

func TestCropDisplay(t *testing.T) {
	art := synthArt()
	const scale, ox, oy = 2, 7, 28 // artwork drawn at (7,28) points under a title bar
	win := image.NewRGBA(image.Rect(0, 0, (60+14)*scale, (50+40)*scale))
	for y := range win.Bounds().Dy() {
		for x := range win.Bounds().Dx() {
			win.Set(x, y, color.RGBA{200, 200, 200, 255})
		}
	}
	for y := range 50 {
		for x := range 60 {
			c := art.Image.At(x, y)
			if !opaque(c) {
				c = color.RGBA{10, 250, 30, 255} // app content in the hole
			}
			for dy := range scale {
				for dx := range scale {
					win.Set((ox+x)*scale+dx, (oy+y)*scale+dy, c)
				}
			}
		}
	}
	off, cost := FindArtOffset(BoxDownscale(win, scale), art.Image, art.Display)
	if off != (image.Point{ox, oy}) || cost != 0 {
		t.Fatalf("offset %v cost %v", off, cost)
	}
	out, err := CropDisplay(win, scale, art)
	if err != nil {
		t.Fatal(err)
	}
	if out.Bounds().Dx() != 20 || out.Bounds().Dy() != 20 {
		t.Fatalf("size %v", out.Bounds())
	}
	if c := out.RGBAAt(10, 10); c != (color.RGBA{10, 250, 30, 255}) {
		t.Fatalf("centre %v", c)
	}
	if c := out.RGBAAt(0, 0); c != (color.RGBA{0, 0, 0, 255}) {
		t.Fatalf("corner must be masked black, got %v", c)
	}
}

type fakeClock struct{ t time.Time }

func (c *fakeClock) Now() time.Time { return c.t }
func (c *fakeClock) Sleep(ctx context.Context, d time.Duration) error {
	c.t = c.t.Add(d)
	return ctx.Err()
}

func TestWaitReadyFakeListener(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	addr := ln.Addr().String()
	ctx := context.Background()
	clk := &fakeClock{t: time.Unix(0, 0)}
	if err := WaitReady(ctx, clk, Dial, addr, nil, nil, 100*time.Millisecond, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	if el := clk.t.Sub(time.Unix(0, 0)); el < 100*time.Millisecond {
		t.Fatalf("ready before stable window: %v", el)
	}
	// Process dead → never ready.
	clk = &fakeClock{t: time.Unix(0, 0)}
	err = WaitReady(ctx, clk, Dial, addr, func() bool { return false }, nil, 100*time.Millisecond, time.Second)
	if err == nil || !strings.Contains(err.Error(), "not ready") {
		t.Fatalf("dead process: %v", err)
	}
	ln.Close()
	clk = &fakeClock{t: time.Unix(0, 0)}
	if err := WaitClosed(ctx, clk, Dial, addr, time.Second); err != nil {
		t.Fatal(err)
	}
}

func TestSDKOf(t *testing.T) {
	if got := SDKOf("/x/Sdks/sdk-9/bin/ConnectIQ.app/Contents/MacOS/simulator"); got != "/x/Sdks/sdk-9" {
		t.Fatal(got)
	}
}
