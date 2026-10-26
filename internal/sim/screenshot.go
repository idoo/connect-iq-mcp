package sim

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
)

// WindowOwner is the CGWindowList owner name of the simulator.
const WindowOwner = "Connect IQ Device Simulator"

// ErrUnsupported is returned for GUI features not implemented on this OS.
func ErrUnsupported() error { return fmt.Errorf("unsupported_on_%s", runtime.GOOS) }

// ErrScreenLocked means the console is locked; window captures fail.
var ErrScreenLocked = errors.New("the macOS console is locked; screencapture cannot read windows until it is unlocked")

var lockedRE = regexp.MustCompile(`<key>CGSSessionScreenIsLocked</key>\s*<true/>`)

// ScreenLocked reports whether the macOS console is locked.
func ScreenLocked(ctx context.Context) bool {
	if runtime.GOOS != "darwin" {
		return false
	}
	out, err := exec.CommandContext(ctx, "ioreg", "-n", "Root", "-d", "1", "-a").Output()
	return err == nil && lockedRE.Match(out)
}

// Window is a simulator window from CGWindowList.
type Window struct {
	ID       int     `json:"id"`
	Width    float64 `json:"width"`
	Height   float64 `json:"height"`
	OnScreen bool    `json:"onscreen"`
	Title    string  `json:"title,omitempty"`
}

const windowsJXA = `ObjC.import('CoreGraphics');
var l = ObjC.deepUnwrap(ObjC.castRefToObject($.CGWindowListCopyWindowInfo(0, 0)));
var o = [];
for (var i = 0; i < l.length; i++) { var w = l[i];
  if (w.kCGWindowOwnerName !== %q || w.kCGWindowLayer !== 0) continue;
  o.push({id: w.kCGWindowNumber, width: w.kCGWindowBounds.Width, height: w.kCGWindowBounds.Height,
    onscreen: !!w.kCGWindowIsOnscreen, title: w.kCGWindowName || ''}); }
JSON.stringify(o);`

// SimulatorWindow returns the largest simulator window (macOS).
func SimulatorWindow(ctx context.Context) (Window, error) {
	if runtime.GOOS != "darwin" {
		return Window{}, ErrUnsupported()
	}
	out, err := exec.CommandContext(ctx, "osascript", "-l", "JavaScript", "-e", fmt.Sprintf(windowsJXA, WindowOwner)).Output()
	if err != nil {
		return Window{}, fmt.Errorf("list windows: %w", err)
	}
	var ws []Window
	if err := json.Unmarshal(out, &ws); err != nil {
		return Window{}, fmt.Errorf("list windows: %w", err)
	}
	var best Window
	for _, w := range ws {
		if w.Width*w.Height > best.Width*best.Height {
			best = w
		}
	}
	if best.ID == 0 {
		return Window{}, errors.New("no simulator window found (is the simulator running? sim_start)")
	}
	return best, nil
}

// CaptureWindow captures window id to a PNG file (macOS, Screen Recording).
func CaptureWindow(ctx context.Context, id int, path string) error {
	if runtime.GOOS != "darwin" {
		return ErrUnsupported()
	}
	if ScreenLocked(ctx) {
		return ErrScreenLocked
	}
	out, err := exec.CommandContext(ctx, "screencapture", "-x", "-o", "-l", fmt.Sprint(id), path).CombinedOutput()
	if err != nil || len(out) > 0 && strings.Contains(string(out), "could not create image") {
		return fmt.Errorf("screencapture failed (grant Screen Recording to the app running garmin-mcp): %v %s", err, strings.TrimSpace(string(out)))
	}
	if fi, err := os.Stat(path); err != nil || fi.Size() == 0 {
		return errors.New("screencapture wrote no image (grant Screen Recording to the app running garmin-mcp)")
	}
	return nil
}

// DeviceArt is the simulator artwork of one device.
type DeviceArt struct {
	Image   image.Image
	Display image.Rectangle // display hole within Image (points)
}

type simulatorJSON struct {
	Image   string `json:"image"`
	Display struct {
		Location struct {
			X, Y, Width, Height int
		} `json:"location"`
	} `json:"display"`
}

// LoadDeviceArt reads Devices/<id>/simulator.json and its image.
func LoadDeviceArt(devicesDir, id string) (DeviceArt, error) {
	dir := filepath.Join(devicesDir, id)
	b, err := os.ReadFile(filepath.Join(dir, "simulator.json"))
	if err != nil {
		return DeviceArt{}, err
	}
	var sj simulatorJSON
	if err := json.Unmarshal(b, &sj); err != nil {
		return DeviceArt{}, fmt.Errorf("simulator.json: %w", err)
	}
	if sj.Image == "" {
		sj.Image = "device.png"
	}
	f, err := os.Open(filepath.Join(dir, filepath.Base(sj.Image)))
	if err != nil {
		return DeviceArt{}, err
	}
	defer f.Close()
	img, err := png.Decode(f)
	if err != nil {
		return DeviceArt{}, fmt.Errorf("%s: %w", sj.Image, err)
	}
	l := sj.Display.Location
	return DeviceArt{Image: img, Display: image.Rect(l.X, l.Y, l.X+l.Width, l.Y+l.Height)}, nil
}

// BoxDownscale reduces img by an integer factor f averaging f×f blocks.
func BoxDownscale(img image.Image, f int) *image.RGBA {
	b := img.Bounds()
	if f <= 1 {
		out := image.NewRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
		draw.Draw(out, out.Bounds(), img, b.Min, draw.Src)
		return out
	}
	w, h := b.Dx()/f, b.Dy()/f
	out := image.NewRGBA(image.Rect(0, 0, w, h))
	n := uint32(f * f)
	for y := range h {
		for x := range w {
			var r, g, bl, a uint32
			for dy := range f {
				for dx := range f {
					cr, cg, cb, ca := img.At(b.Min.X+x*f+dx, b.Min.Y+y*f+dy).RGBA()
					r, g, bl, a = r+cr, g+cg, bl+cb, a+ca
				}
			}
			out.SetRGBA(x, y, color.RGBA{uint8(r / n >> 8), uint8(g / n >> 8), uint8(bl / n >> 8), uint8(a / n >> 8)})
		}
	}
	return out
}

func opaque(c color.Color) bool { _, _, _, a := c.RGBA(); return a >= 0xF000 }

func diff(a, b color.Color) int {
	ar, ag, ab, _ := a.RGBA()
	br, bg, bb, _ := b.RGBA()
	d := func(x, y uint32) int {
		v := int(x>>8) - int(y>>8)
		if v < 0 {
			return -v
		}
		return v
	}
	return d(ar, br) + d(ag, bg) + d(ab, bb)
}

// FindArtOffset finds where art is drawn inside win (both at the same
// scale) by matching opaque bezel pixels around the display hole. It
// returns the offset and the mean per-pixel colour difference.
func FindArtOffset(win, art image.Image, hole image.Rectangle) (image.Point, float64) {
	ab, wb := art.Bounds(), win.Bounds()
	// Sample opaque art pixels in a band around the hole (the bezel).
	band := hole.Inset(-24).Intersect(ab)
	var pts []image.Point
	step := 1 + (band.Dx()*band.Dy())/600
	i := 0
	for y := band.Min.Y; y < band.Max.Y; y++ {
		for x := band.Min.X; x < band.Max.X; x++ {
			i++
			if i%step != 0 || (image.Point{x, y}).In(hole) || !opaque(art.At(x, y)) {
				continue
			}
			pts = append(pts, image.Point{x, y})
		}
	}
	best, bestCost := image.Point{}, -1.0
	for oy := 0; oy+ab.Dy() <= wb.Dy()+ab.Min.Y && oy <= 400; oy++ {
		for ox := 0; ox+ab.Dx() <= wb.Dx()+ab.Min.X && ox <= 400; ox++ {
			cost := 0
			for _, p := range pts {
				cost += diff(art.At(p.X, p.Y), win.At(wb.Min.X+ox+p.X-ab.Min.X, wb.Min.Y+oy+p.Y-ab.Min.Y))
				if bestCost >= 0 && float64(cost) > bestCost*float64(len(pts)) {
					break
				}
			}
			c := float64(cost) / float64(max(1, len(pts)))
			if bestCost < 0 || c < bestCost {
				best, bestCost = image.Point{ox, oy}, c
			}
		}
	}
	return best, bestCost
}

// CropDisplay turns a window capture at integer scale into the device's
// native display image: downscale, locate the artwork, crop the hole and
// paint pixels outside the (1 px eroded) transparent hole black.
func CropDisplay(win image.Image, scale int, art DeviceArt) (*image.RGBA, error) {
	pts := BoxDownscale(win, scale)
	ab := art.Image.Bounds()
	if pts.Bounds().Dx() < ab.Dx() || pts.Bounds().Dy() < ab.Dy() {
		return nil, fmt.Errorf("window %v smaller than device artwork %v (zoomed simulator window?)", pts.Bounds().Size(), ab.Size())
	}
	off, cost := FindArtOffset(pts, art.Image, art.Display)
	if cost > 60 {
		return nil, fmt.Errorf("could not locate device artwork in the window (mean diff %.0f)", cost)
	}
	h := art.Display
	out := image.NewRGBA(image.Rect(0, 0, h.Dx(), h.Dy()))
	inHole := func(x, y int) bool {
		p := image.Point{x, y}
		return p.In(h) && !opaque(art.Image.At(x, y))
	}
	for y := h.Min.Y; y < h.Max.Y; y++ {
		for x := h.Min.X; x < h.Max.X; x++ {
			c := color.RGBA{0, 0, 0, 255}
			if inHole(x, y) && inHole(x-1, y) && inHole(x+1, y) && inHole(x, y-1) && inHole(x, y+1) {
				r, g, b, _ := pts.At(off.X+x-ab.Min.X, off.Y+y-ab.Min.Y).RGBA()
				c = color.RGBA{uint8(r >> 8), uint8(g >> 8), uint8(b >> 8), 255}
			}
			out.SetRGBA(x-h.Min.X, y-h.Min.Y, c)
		}
	}
	// A rectangular display has no transparent hole edge inside the rect:
	// keep the border pixels instead of eroding them.
	if !opaque(art.Image.At(h.Min.X, h.Min.Y)) {
		for y := h.Min.Y; y < h.Max.Y; y++ {
			for x := h.Min.X; x < h.Max.X; x++ {
				if !opaque(art.Image.At(x, y)) && (x == h.Min.X || y == h.Min.Y || x == h.Max.X-1 || y == h.Max.Y-1) {
					r, g, b, _ := pts.At(off.X+x-ab.Min.X, off.Y+y-ab.Min.Y).RGBA()
					out.SetRGBA(x-h.Min.X, y-h.Min.Y, color.RGBA{uint8(r >> 8), uint8(g >> 8), uint8(b >> 8), 255})
				}
			}
		}
	}
	return out, nil
}

// LastUsedDevice reads LastUsedDevice from <dataRoot>/simulator.ini.
func LastUsedDevice(dataRoot string) string {
	b, err := os.ReadFile(filepath.Join(dataRoot, "simulator.ini"))
	if err != nil {
		return ""
	}
	for _, l := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(l), "LastUsedDevice="); ok {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
