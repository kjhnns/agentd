package fuel

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func gaugeFixture() Snapshot {
	return Snapshot{Date: "2026-11-08", Budgets: []Budget{
		{MacroState: MacroState{Key: "kcal", Unit: "kcal", Kind: "target", Consumed: 1800, Target: fptr(2600)}},
		{MacroState: MacroState{Key: "fiber_g", Unit: "g", Kind: "floor", Consumed: 12, Target: fptr(30)}},
		{MacroState: MacroState{Key: "protein_g", Unit: "g", Kind: "floor", Consumed: 87, Target: fptr(160)}},
		{MacroState: MacroState{Key: "sat_fat_g", Unit: "g", Kind: "budget", Status: "over", Consumed: 30, Target: fptr(20)}},
		{MacroState: MacroState{Key: "carbs_g", Unit: "g", Kind: "floor", Consumed: 180, Target: fptr(200)}},
		{MacroState: MacroState{Key: "caffeine_mg", Unit: "mg", Kind: "cap", Consumed: 50, Target: fptr(400)}},
	}}
}

func decodeGauges(t *testing.T, raw []byte) image.Image {
	t.Helper()
	im, err := png.Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	return im
}

func gaugePixel(t *testing.T, im image.Image, x, y int, want color.RGBA) {
	t.Helper()
	if got := color.RGBAModel.Convert(im.At(x, y)).(color.RGBA); got != want {
		t.Errorf("pixel (%d,%d) = %v, want %v", x, y, got, want)
	}
}

func TestRenderGauges(t *testing.T) {
	s := gaugeFixture()
	added := map[string]*float64{"protein_g": fptr(27), "sat_fat_g": fptr(5), "carbs_g": fptr(30)}
	raw, err := RenderGauges(s, added)
	if err != nil {
		t.Fatal(err)
	}
	again, err := RenderGauges(s, added)
	if err != nil || !bytes.Equal(raw, again) {
		t.Fatalf("render is not deterministic: %v", err)
	}
	im := decodeGauges(t, raw)
	if im.Bounds() != image.Rect(0, 0, 1080, 658) {
		t.Fatal(im.Bounds())
	}
	white := color.RGBA{255, 255, 255, 255}
	for _, p := range []struct {
		x, y int
		c    color.RGBA
	}{
		{0, 0, white}, {100, 182, gaugePrior}, {421, 182, white},
		{422, 182, white}, {423, 182, white}, {424, 182, gaugeDelta},
		{500, 182, gaugeDelta}, {800, 182, gaugeTrack}, {1015, 166, gaugeInk},
		{64, 170, white}, // rounded track corner
		{500, 282, gaugePrior}, {750, 282, gaugeOver}, {950, 282, gaugeOver},
		{820, 582, gaugeDelta}, // total carbohydrate's meal delta
	} {
		gaugePixel(t, im, p.x, p.y, p.c)
	}
	labels := []string{"Protein", "Sat fat", "Fibre", "Energy", "Carbs"}
	for i, b := range gaugeBudgets(s) {
		if b.Label != labels[i] {
			t.Fatalf("row %d: %s", i, b.Label)
		}
	}
}

func TestGaugesGolden(t *testing.T) {
	// Synthetic round numbers, unrelated to any personal snapshot.
	sample := Snapshot{Date: "2026-11-08", Budgets: []Budget{
		{MacroState: MacroState{Key: "protein_g", Unit: "g", Kind: "floor", Consumed: 80, Target: fptr(150)}},
		{MacroState: MacroState{Key: "sat_fat_g", Unit: "g", Kind: "budget", Consumed: 10, Target: fptr(20)}},
		{MacroState: MacroState{Key: "fiber_g", Unit: "g", Kind: "floor", Consumed: 20, Target: fptr(30)}},
		{MacroState: MacroState{Key: "kcal", Unit: "kcal", Kind: "target", Consumed: 1500, Target: fptr(2500)}, Provisional: true},
		{MacroState: MacroState{Key: "carbs_g", Unit: "g", Kind: "floor", Consumed: 100}},
	}}
	for _, tc := range []struct {
		name     string
		snapshot Snapshot
		added    map[string]*float64
	}{
		{"gauges-sample", sample, nil},
		{"gauges-delta", gaugeFixture(), map[string]*float64{"protein_g": fptr(27), "sat_fat_g": fptr(5), "carbs_g": fptr(30)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := RenderGauges(tc.snapshot, tc.added)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join("testdata", tc.name+".png")
			if os.Getenv("FUEL_GAUGES_GOLDEN_UPDATE") == "1" {
				if err := os.WriteFile(path, raw, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			gotImage, wantImage := decodeGauges(t, raw), decodeGauges(t, want)
			if gotImage.Bounds() != wantImage.Bounds() {
				t.Fatalf("render bounds %v, want %v", gotImage.Bounds(), wantImage.Bounds())
			}
			// Bundled Go fonts avoid host font dependencies. Compare pixels,
			// not PNG compression bytes, allowing at most 2/255 per channel
			// for rasterizer and floating-point rounding across architectures.
			for y := gotImage.Bounds().Min.Y; y < gotImage.Bounds().Max.Y; y++ {
				for x := gotImage.Bounds().Min.X; x < gotImage.Bounds().Max.X; x++ {
					got := color.NRGBAModel.Convert(gotImage.At(x, y)).(color.NRGBA)
					want := color.NRGBAModel.Convert(wantImage.At(x, y)).(color.NRGBA)
					for c, value := range []uint8{got.R, got.G, got.B, got.A} {
						diff := int(value) - int([]uint8{want.R, want.G, want.B, want.A}[c])
						if diff < -2 || diff > 2 {
							t.Fatalf("render differs from %s at (%d,%d): %v, want %v; review before updating with FUEL_GAUGES_GOLDEN_UPDATE=1", path, x, y, got, want)
						}
					}
				}
			}
		})
	}
}

func TestGaugesProvisionalOnlyEnergy(t *testing.T) {
	s := gaugeFixture()
	want, err := RenderGauges(s, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := range s.Budgets {
		if s.Budgets[i].Key != "kcal" {
			s.Budgets[i].Provisional = true
		}
	}
	got, err := RenderGauges(s, nil)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("non-energy provisional flag changed render: %v", err)
	}
}

func TestGaugesNoTargetRow(t *testing.T) {
	s := gaugeFixture()
	s.Budgets[2].Target = nil // Protein is the first rendered row.
	raw, err := RenderGauges(s, nil)
	if err != nil {
		t.Fatal(err)
	}
	im := decodeGauges(t, raw)
	if im.Bounds() != image.Rect(0, 0, 1080, 614) {
		t.Fatal(im.Bounds())
	}
	white := color.RGBA{255, 255, 255, 255}
	for y := 165; y < 177; y++ {
		for x := 64; x < 1016; x++ {
			gaugePixel(t, im, x, y, white)
		}
	}
	// The next row moves up by the omitted bar's 44 pixels.
	gaugePixel(t, im, 100, 238, color.RGBA{110, 110, 110, 255})
}

func TestGaugesBudgetEdges(t *testing.T) {
	for _, tc := range []struct {
		name  string
		b     Budget
		delta *float64
		pixel color.RGBA
		value string
	}{
		{"floor excess", Budget{MacroState: MacroState{Kind: "floor", Consumed: 180, Target: fptr(160), Status: "met"}}, nil, gaugePrior, "180 / 160 g"},
		{"negative correction", Budget{MacroState: MacroState{Kind: "floor", Consumed: 100, Target: fptr(160)}}, fptr(-20), gaugeTrack, "100 / 160 g"},
		{"missing target", Budget{MacroState: MacroState{Consumed: 87}}, fptr(27), color.RGBA{255, 255, 255, 255}, "87 g"},
		{"zero target", Budget{MacroState: MacroState{Kind: "cap", Consumed: 30, Target: fptr(0), Status: "over"}}, nil, gaugeOver, "30 / 0 g cap"},
		{"unknown amount", Budget{MacroState: MacroState{Kind: "floor", Consumed: 87, Target: fptr(160), UnknownRows: 1}}, nil, gaugeTrack, "87+ / 160 g"},
		{"cap", Budget{MacroState: MacroState{Kind: "cap", Consumed: 10, Target: fptr(20)}}, nil, gaugeTrack, "10 / 20 g cap"},
		{"budget", Budget{MacroState: MacroState{Kind: "budget", Consumed: 10, Target: fptr(20)}}, nil, gaugeTrack, "10 / 20 g cap"},
		{"empty day", Budget{MacroState: MacroState{Kind: "floor", Target: fptr(160)}}, nil, gaugeTrack, "0 / 160 g"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.b.Key, tc.b.Unit = "protein_g", "g"
			raw, err := RenderGauges(Snapshot{Budgets: []Budget{tc.b}}, map[string]*float64{"protein_g": tc.delta})
			if err != nil {
				t.Fatal(err)
			}
			im := decodeGauges(t, raw)
			gaugePixel(t, im, 950, 182, tc.pixel)
			if v := gaugeValue(tc.b); v != tc.value {
				t.Fatalf("value %q, want %q", v, tc.value)
			}
		})
	}
}

func TestGaugesInvalidInput(t *testing.T) {
	if _, err := RenderGauges(Snapshot{}, nil); err == nil {
		t.Fatal("empty snapshot accepted")
	}
	for _, v := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		s := gaugeFixture()
		s.Budgets[0].Consumed = v
		if _, err := RenderGauges(s, nil); err == nil {
			t.Fatal("nonfinite amount accepted")
		}
		s = gaugeFixture()
		s.Budgets[0].Target = &v
		if _, err := RenderGauges(s, nil); err == nil {
			t.Fatal("nonfinite target accepted")
		}
	}
}

func TestGaugesRouteAuth(t *testing.T) {
	h := newHarness(t)
	for _, tc := range []struct {
		name, token string
		code        int
	}{
		{"missing", "", http.StatusUnauthorized},
		{"wrong", "wrong", http.StatusUnauthorized},
		{"app", testToken, http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/fuel/gauges.png", nil)
			if tc.token != "" {
				req.Header.Set("Authorization", "Bearer "+tc.token)
			}
			rec := httptest.NewRecorder()
			h.h.ServeHTTP(rec, req)
			if rec.Code != tc.code {
				t.Fatalf("HTTP %d: %s", rec.Code, rec.Body.String())
			}
			if tc.code == http.StatusOK {
				if rec.Header().Get("Content-Type") != "image/png" || rec.Header().Get("Cache-Control") != "no-store" {
					t.Fatal(rec.Header())
				}
				decodeGauges(t, rec.Body.Bytes())
				snap := decode[Snapshot](t, h.get("/fuel/snapshot"))
				want, err := RenderGauges(snap, nil)
				if err != nil || !bytes.Equal(rec.Body.Bytes(), want) {
					t.Fatalf("route differs from renderer: %v", err)
				}
			}
		})
	}
}

func TestTelegramGaugesConfig(t *testing.T) {
	for _, tc := range []struct {
		input string
		want  bool
	}{
		{"", true}, {"telegram_gauges = true", true}, {"telegram_gauges = false", false},
	} {
		cfg, err := ParseDaemonConfig([]byte(tc.input))
		if err != nil || cfg.TelegramGauges != tc.want {
			t.Fatalf("%q: gauges %t, error %v", tc.input, cfg.TelegramGauges, err)
		}
	}
	if _, err := ParseDaemonConfig([]byte(`telegram_gauges = "false"`)); err == nil {
		t.Fatal("quoted bool accepted")
	}
}

func TestGaugesCarbsDelta(t *testing.T) {
	added := addedOf([]Macros{{Carbs: known(250), Protein: known(100)}})
	if added["carbs_g"] == nil || *added["carbs_g"] != 25 {
		t.Fatalf("total carbohydrate delta missing: %v", added)
	}
}
