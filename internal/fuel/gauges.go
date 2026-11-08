package fuel

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"math"
	"net/http"
	"time"

	"golang.org/x/image/font"
	"golang.org/x/image/font/gofont/gobold"
	"golang.org/x/image/font/gofont/goregular"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"
)

// The layout uses a 540 point canvas at 2x. All coordinates below are pixels.
const (
	gaugeWidth      = 1080
	gaugeRowGap     = 28
	gaugeTextHeight = 28
	gaugeBarHeight  = 44 // From the value baseline to the bottom of the bar.
)

var (
	gaugeInk   = color.RGBA{0x18, 0x18, 0x18, 0xff}
	gaugePrior = color.RGBA{110, 110, 110, 255}
	gaugeDelta = color.RGBA{28, 28, 30, 255}
	gaugeTrack = color.RGBA{229, 229, 234, 255}
	gaugeOver  = color.RGBA{0x8a, 0x4b, 0x4b, 0xff}
	gaugeMuted = color.RGBA{142, 142, 147, 255}
)

// gaugeBudgets selects nutrition budgets in the same order on every client.
func gaugeBudgets(s Snapshot) []Budget {
	var out []Budget
	for _, row := range []struct{ key, label string }{
		{"protein_g", "Protein"}, {"sat_fat_g", "Sat fat"}, {"fiber_g", "Fibre"},
		{"kcal", "Energy"}, {"carbs_g", "Carbs"},
	} {
		for _, b := range s.Budgets {
			if b.Key == row.key {
				b.Label = row.label
				out = append(out, b)
				break
			}
		}
	}
	return out
}

// RenderGauges renders a deterministic, light-theme PNG from a day's snapshot.
// added contains this meal's deltas by macro key, in the snapshot's units.
// Negative corrections reduce the total but never draw a positive meal segment.
// A nil delta is unknown. Floors above target are not warnings.
func RenderGauges(s Snapshot, added map[string]*float64) ([]byte, error) {
	rows := gaugeBudgets(s)
	if len(rows) == 0 {
		return nil, fmt.Errorf("gauges: no nutrition budgets")
	}
	face := func(data []byte, size float64) (font.Face, error) {
		f, err := opentype.Parse(data)
		if err != nil {
			return nil, err
		}
		return opentype.NewFace(f, &opentype.FaceOptions{Size: size, DPI: 144, Hinting: font.HintingFull})
	}
	regular, err := face(goregular.TTF, 16)
	if err != nil {
		return nil, fmt.Errorf("gauges font: %w", err)
	}
	defer regular.Close()
	bold, err := face(gobold.TTF, 18)
	if err != nil {
		return nil, fmt.Errorf("gauges font: %w", err)
	}
	defer bold.Close()
	small, err := face(goregular.TTF, 12)
	if err != nil {
		return nil, fmt.Errorf("gauges font: %w", err)
	}
	defer small.Close()
	height := 150 + 64 + (len(rows)-1)*(gaugeTextHeight+gaugeRowGap)
	for _, b := range rows {
		if b.Target != nil {
			height += gaugeBarHeight
		}
	}
	im := image.NewRGBA(image.Rect(0, 0, gaugeWidth, height))
	draw.Draw(im, im.Bounds(), image.White, image.Point{}, draw.Src)
	text := func(f font.Face, x, y int, value string, ink color.RGBA) {
		d := font.Drawer{Dst: im, Src: image.NewUniform(ink), Face: f, Dot: fixed.P(x, y)}
		d.DrawString(value)
	}
	text(bold, 64, 76, "Today", gaugeInk)
	date := s.Date
	if d, err := time.Parse("2006-01-02", date); err == nil {
		date = d.Format("Mon 2 Jan")
	}
	text(regular, gaugeWidth-64-font.MeasureString(regular, date).Ceil(), 76, date, gaugeInk)
	y := 150
	for _, b := range rows {
		if !gaugeFinite(b.Consumed) || b.Target != nil && !gaugeFinite(*b.Target) {
			return nil, fmt.Errorf("gauges: invalid %s value", b.Key)
		}
		text(bold, 64, y, b.Label, gaugeInk)
		value := gaugeValue(b)
		right := gaugeWidth - 64
		if b.Key == "kcal" && b.Provisional {
			suffix := "provisional"
			x := right - font.MeasureString(small, suffix).Ceil()
			text(small, x, y, suffix, gaugeMuted)
			right = x - 12
		}
		text(regular, right-font.MeasureString(regular, value).Ceil(), y, value, gaugeInk)
		if b.Target != nil {
			gaugeBar(im, image.Rect(64, y+20, gaugeWidth-64, y+gaugeBarHeight), b, added[b.Key])
			y += gaugeBarHeight
		}
		y += gaugeTextHeight + gaugeRowGap
	}
	var out bytes.Buffer
	if err := png.Encode(&out, im); err != nil {
		return nil, fmt.Errorf("gauges PNG: %w", err)
	}
	return out.Bytes(), nil
}

func gaugeFinite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

func gaugeValue(b Budget) string {
	c := fmtNum(tgRound(b.Consumed))
	if b.UnknownRows > 0 {
		c += "+"
	}
	if b.Target == nil {
		return c + " " + b.Unit
	}
	value := c + " / " + fmtNum(*b.Target) + " " + b.Unit
	if b.Kind == "cap" || b.Kind == "budget" {
		value += " cap"
	}
	return value
}

func gaugeBar(im *image.RGBA, r image.Rectangle, b Budget, delta *float64) {
	consumed := math.Max(0, b.Consumed)
	target := 0.0
	if b.Target != nil {
		target = math.Max(0, *b.Target)
	}
	scale := math.Max(1, math.Max(target, consumed))
	width := float64(r.Dx())
	end, mark := width*consumed/scale, width*target/scale
	before := end
	if delta != nil && gaugeFinite(*delta) && *delta > 0 {
		before = width * math.Max(0, consumed-*delta) / scale
	}
	warn := b.Kind != "floor" && (b.Kind == "cap" || b.Kind == "budget" || b.Status == "over")
	radius := float64(r.Dy()) / 2
	for y := r.Min.Y; y < r.Max.Y; y++ {
		for x := r.Min.X; x < r.Max.X; x++ {
			p := float64(x-r.Min.X) + 0.5
			c := gaugeTrack
			if b.Target != nil && p < end {
				c = gaugePrior
				if p >= before {
					c = gaugeDelta
				}
				if warn && p >= mark {
					c = gaugeOver
				}
				// The 1.5 point (3 pixel) gap belongs to the meal segment.
				if before > 0 && before < end && p >= before && p < math.Min(end, before+3) {
					c = color.RGBA{255, 255, 255, 255}
				}
			}
			// Antialiased capsule edges. Segments share the same clipped track.
			cx := math.Max(radius, math.Min(width-radius, p))
			d := math.Hypot(p-cx, float64(y-r.Min.Y)+0.5-radius)
			a := math.Max(0, math.Min(1, radius+0.5-d))
			blend := func(v uint8) uint8 { return uint8(math.Round(255 + a*(float64(v)-255))) }
			im.SetRGBA(x, y, color.RGBA{blend(c.R), blend(c.G), blend(c.B), 255})
		}
	}
	if b.Target != nil {
		x := r.Min.X + int(math.Round(mark))
		x = min(r.Max.X-1, max(r.Min.X+1, x))
		draw.Draw(im, image.Rect(x-1, r.Min.Y-5, x+1, r.Max.Y+5), image.NewUniform(gaugeInk), image.Point{}, draw.Src)
	}
}

func (s *Service) handleGauges(w http.ResponseWriter, r *http.Request) {
	date := s.today()
	s.freshen(r.Context(), date)
	s.ensureHistory(r.Context(), date)
	snap, err := s.snapshotFor(date)
	if err != nil {
		writeErr(w, errf(http.StatusServiceUnavailable, "targets_invalid", false, "targets invalid"))
		return
	}
	raw, err := RenderGauges(snap, nil)
	if err != nil {
		writeErr(w, errf(http.StatusInternalServerError, "render_failed", false, "could not render gauges"))
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(raw)
}
