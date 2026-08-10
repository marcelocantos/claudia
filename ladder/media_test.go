// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package ladder_test

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"strings"
	"testing"

	"github.com/marcelocantos/claudia/ladder"
)

// chromeShot builds a synthetic screenshot: mostly grey window chrome,
// with one small red region in a known place. The judgement the image
// exists to support is "where is the red thing", and a reduction that
// loses that has saved tokens by destroying the point.
func chromeShot(t *testing.T) ([]byte, image.Rectangle) {
	t.Helper()
	const w, h = 800, 600
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			img.Set(x, y, color.RGBA{R: 40, G: 40, B: 40, A: 255})
		}
	}
	region := image.Rect(600, 400, 700, 500)
	for y := region.Min.Y; y < region.Max.Y; y++ {
		for x := region.Min.X; x < region.Max.X; x++ {
			img.Set(x, y, color.RGBA{R: 220, G: 20, B: 20, A: 255})
		}
	}

	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes(), region
}

// reddest returns the position of the most red pixel, scaled to
// fractions of the image, which is the judgement under test.
func reddest(t *testing.T, data []byte) (fx, fy float64) {
	t.Helper()
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	b := img.Bounds()
	var bestX, bestY int
	var best int32
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			r, g, bl, _ := img.At(x, y).RGBA()
			score := int32(r>>8) - int32(g>>8) - int32(bl>>8)
			if score > best {
				best, bestX, bestY = score, x, y
			}
		}
	}
	return float64(bestX-b.Min.X) / float64(b.Dx()), float64(bestY-b.Min.Y) / float64(b.Dy())
}

func TestReducedImageStillSupportsTheJudgement(t *testing.T) {
	src, _ := chromeShot(t)
	wantX, wantY := reddest(t, src)

	red, err := ladder.ReduceImage(src, &ladder.ImageReduction{
		MaxWidth: 200, MaxHeight: 150,
		Ref: ladder.Ref{ID: "shot-1"},
	})
	if err != nil {
		t.Fatalf("ReduceImage: %v", err)
	}

	if red.Width != 200 || red.Height != 150 {
		t.Errorf("reduced to %dx%d, want 200x150", red.Width, red.Height)
	}
	if red.Saved() <= 0 {
		t.Errorf("no saving: %d → %d tokens", red.TokensBefore, red.TokensAfter)
	}

	// The judgement survives: the red region is still in the same place,
	// proportionally.
	gotX, gotY := reddest(t, red.Bytes)
	if abs(gotX-wantX) > 0.05 || abs(gotY-wantY) > 0.05 {
		t.Errorf("the red region moved from (%.2f,%.2f) to (%.2f,%.2f); the reduction destroyed what the image was for",
			wantX, wantY, gotX, gotY)
	}
}

// The box filter's whole justification. A two-pixel highlight is exactly
// what a four-times downscale loses if it samples rather than averages,
// and a thin highlight is often the thing a screenshot was sent to show.
func TestThinHighlightSurvivesDownscale(t *testing.T) {
	const w, h = 800, 600
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			img.Set(x, y, color.RGBA{R: 40, G: 40, B: 40, A: 255})
		}
	}
	// Placed so that a 4:1 nearest-neighbour downscale steps straight
	// over it: it samples x=600 and x=604, and the line is at 601-602.
	for y := range h {
		img.Set(601, y, color.RGBA{R: 220, G: 20, B: 20, A: 255})
		img.Set(602, y, color.RGBA{R: 220, G: 20, B: 20, A: 255})
	}

	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}

	red, err := ladder.ReduceImage(buf.Bytes(), &ladder.ImageReduction{
		MaxWidth: 200, MaxHeight: 150, Ref: ladder.Ref{ID: "highlight"},
	})
	if err != nil {
		t.Fatal(err)
	}

	if score := reddestScore(t, red.Bytes); score <= 0 {
		t.Errorf("the highlight vanished (best redness %d); averaging keeps a thin feature as a tint, sampling drops it", score)
	}
}

func TestCroppingChromeIsWhereTheSavingIs(t *testing.T) {
	src, region := chromeShot(t)

	full, err := ladder.ReduceImage(src, &ladder.ImageReduction{Ref: ladder.Ref{ID: "shot-1"}})
	if err != nil {
		t.Fatal(err)
	}
	if full.Saved() != 0 || len(full.Elisions) != 0 {
		t.Errorf("an untouched image reported a saving or an elision: %+v", full)
	}

	cropped, err := ladder.ReduceImage(src, &ladder.ImageReduction{
		Crop: region, Ref: ladder.Ref{ID: "shot-1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if cropped.Width != region.Dx() || cropped.Height != region.Dy() {
		t.Errorf("cropped to %dx%d, want %dx%d", cropped.Width, cropped.Height, region.Dx(), region.Dy())
	}
	if cropped.TokensAfter >= full.TokensAfter {
		t.Errorf("cropping did not reduce the estimate: %d vs %d", cropped.TokensAfter, full.TokensAfter)
	}

	// A crop outside the image is an error, not a silently empty result.
	if _, err := ladder.ReduceImage(src, &ladder.ImageReduction{
		Crop: image.Rect(5000, 5000, 6000, 6000), Ref: ladder.Ref{ID: "x"},
	}); err == nil {
		t.Error("a crop outside the image was accepted")
	}
}

func TestReductionIsLossyAndSaysSo(t *testing.T) {
	src, region := chromeShot(t)

	red, err := ladder.ReduceImage(src, &ladder.ImageReduction{
		Crop: region, MaxWidth: 50, Ref: ladder.Ref{ID: "shot-1"},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Both losses are declared, and each names how to get the original.
	if len(red.Elisions) != 2 {
		t.Fatalf("elisions = %+v, want a crop and a downscale", red.Elisions)
	}
	for _, e := range red.Elisions {
		if e.Kind != ladder.MarkerElision {
			t.Errorf("elision kind = %q", e.Kind)
		}
		if !strings.Contains(e.Text, "shot-1") {
			t.Errorf("elision does not say how to fetch the original: %q", e.Text)
		}
	}

	// The original stays reachable. A reduction that destroyed its
	// source would make a detail-hungry judgement impossible rather
	// than merely expensive.
	if !bytes.Equal(red.Original.Bytes, src) {
		t.Error("the original was not retained")
	}

	// The losses travel with the request rather than being discoverable
	// only by comparing against the original.
	note := red.Note("reducer")
	if note.Kind != string(ladder.MarkerElision) || note.Value != "shot-1" {
		t.Errorf("note = %+v", note)
	}
	if !strings.Contains(note.Text, "cropped") || !strings.Contains(note.Text, "downscaled") {
		t.Errorf("note does not carry both losses: %q", note.Text)
	}
}

func TestUpscalingIsRefusedBecauseItBuysNothing(t *testing.T) {
	src, region := chromeShot(t)
	red, err := ladder.ReduceImage(src, &ladder.ImageReduction{
		Crop: region, MaxWidth: 10000, MaxHeight: 10000, Ref: ladder.Ref{ID: "x"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if red.Width != region.Dx() || red.Height != region.Dy() {
		t.Errorf("image was enlarged to %dx%d; that costs tokens to add no information", red.Width, red.Height)
	}
}

func TestElidingTextPassesByReference(t *testing.T) {
	body := strings.Repeat("transcript line\n", 500)
	cfg := &ladder.TextReduction{Head: 100, Tail: 100, Ref: ladder.Ref{ID: "transcript-7"}}

	out, m, elided := ladder.ElideText(body, cfg)
	if !elided {
		t.Fatal("a long artifact was not elided")
	}
	if len([]rune(out)) >= len([]rune(body)) {
		t.Error("elision did not shorten the text")
	}
	if !strings.Contains(out, "transcript-7") {
		t.Error("the elided text does not say how to fetch the original")
	}
	if ladder.CountMarkers(out) != 1 {
		t.Errorf("elision marker count = %d, want 1", ladder.CountMarkers(out))
	}
	if m.Kind != ladder.MarkerElision {
		t.Errorf("marker kind = %q", m.Kind)
	}

	// Short text is left alone rather than being marked for nothing.
	short, _, elided := ladder.ElideText("brief", cfg)
	if elided || short != "brief" {
		t.Errorf("short text was elided: %q", short)
	}
}

// reddestScore returns how red the most red pixel is, on the same
// score the judgement uses: positive means genuinely reddish.
func reddestScore(t *testing.T, data []byte) int32 {
	t.Helper()
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	b := img.Bounds()
	var best int32 = -1 << 30
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			r, g, bl, _ := img.At(x, y).RGBA()
			if score := int32(r>>8) - int32(g>>8) - int32(bl>>8); score > best {
				best = score
			}
		}
	}
	return best
}

func abs(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}
