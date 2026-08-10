// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package ladder

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	_ "image/jpeg" // decode support
	"image/png"
	"strings"
)

// Ref is a handle to the full artifact a reduction came from.
//
// Reduction is lossy, so the original must stay reachable: a judgement
// that turns out to need the detail can fetch it, and a reduction that
// destroyed its source would have made that impossible rather than
// merely expensive.
type Ref struct {
	// ID is how a consumer fetches the original. Claudia does not
	// interpret it; whether it names a file, a blob store key or a
	// transcript offset is the consumer's business.
	ID string
	// Bytes is the original, retained when the consumer wants it held
	// in memory rather than fetched.
	Bytes []byte
}

// Reduction is a payload made cheaper, plus an honest account of what
// that cost.
type Reduction struct {
	// Bytes is the reduced payload.
	Bytes []byte

	// Original refers back to what this came from.
	Original Ref

	Width, Height                 int
	OriginalWidth, OriginalHeight int

	// Elisions record what was dropped, as markers on the out-of-band
	// channel. Reduction is LOSSY AND SAYS SO: a pipeline that quietly
	// discarded the anomalous corner of a screenshot would have made a
	// judgement call with no escalation and no record.
	Elisions []Marker

	// TokensBefore and TokensAfter are estimates, not measurements. A
	// consumer that needs the real number counts it at the provider.
	TokensBefore, TokensAfter int
}

// Saved reports the estimated token reduction.
func (r *Reduction) Saved() int { return r.TokensBefore - r.TokensAfter }

// Note renders the elisions as a request note, so what was dropped
// travels with the request rather than being discoverable only by
// comparing against the original.
func (r *Reduction) Note(layer string) Note {
	parts := make([]string, 0, len(r.Elisions))
	for _, e := range r.Elisions {
		parts = append(parts, e.Text)
	}
	return Note{
		Layer: layer,
		Kind:  string(MarkerElision),
		Text:  strings.Join(parts, "; "),
		Value: r.Original.ID,
	}
}

// EstimateImageTokens approximates what an image costs a model, using
// the width×height/750 rule Anthropic documents for Claude.
//
// It is an ESTIMATE and is labelled as one everywhere it surfaces.
// Reporting a modelled saving as a measured one is the kind of number
// that quietly becomes a headline.
func EstimateImageTokens(w, h int) int {
	if w <= 0 || h <= 0 {
		return 0
	}
	return (w * h) / 750
}

// ImageReduction configures [ReduceImage].
type ImageReduction struct {
	// Crop is the region of interest. The zero rectangle keeps
	// everything.
	//
	// A cockpit screenshot that is ninety percent window chrome should
	// not cost full-resolution tokens for the chrome.
	Crop image.Rectangle

	// MaxWidth and MaxHeight bound the result, preserving aspect ratio.
	// Zero means unbounded in that dimension.
	MaxWidth, MaxHeight int

	// Ref identifies the original so it stays fetchable.
	Ref Ref
}

// ReduceImage crops and downscales an image before it costs tokens.
//
// Every step here is DETERMINISTIC. Cropping, downscaling and measuring
// need no inference at all, so nothing in this path wakes a model to
// make a payload cheaper — which would be self-defeating.
func ReduceImage(src []byte, cfg *ImageReduction) (*Reduction, error) {
	if cfg == nil {
		return nil, fmt.Errorf("ladder: ReduceImage needs a config")
	}
	img, _, err := image.Decode(bytes.NewReader(src))
	if err != nil {
		return nil, fmt.Errorf("ladder: decode image: %w", err)
	}

	full := img.Bounds()
	out := &Reduction{
		OriginalWidth:  full.Dx(),
		OriginalHeight: full.Dy(),
		TokensBefore:   EstimateImageTokens(full.Dx(), full.Dy()),
		Original:       cfg.Ref,
	}
	if out.Original.Bytes == nil {
		out.Original.Bytes = src
	}

	region := full
	if !cfg.Crop.Empty() {
		region = cfg.Crop.Intersect(full)
		if region.Empty() {
			return nil, fmt.Errorf("ladder: crop %v lies outside the image %v", cfg.Crop, full)
		}
		if region != full {
			out.Elisions = append(out.Elisions, Marker{
				Kind: MarkerElision,
				Text: fmt.Sprintf("cropped to %dx%d at (%d,%d) from %dx%d; fetch %s",
					region.Dx(), region.Dy(), region.Min.X, region.Min.Y, full.Dx(), full.Dy(), cfg.Ref.ID),
			})
		}
	}

	cropped := image.NewRGBA(image.Rect(0, 0, region.Dx(), region.Dy()))
	draw.Draw(cropped, cropped.Bounds(), img, region.Min, draw.Src)

	w, h := scaledTo(region.Dx(), region.Dy(), cfg.MaxWidth, cfg.MaxHeight)
	result := image.Image(cropped)
	if w != region.Dx() || h != region.Dy() {
		result = boxDownscale(cropped, w, h)
		out.Elisions = append(out.Elisions, Marker{
			Kind: MarkerElision,
			Text: fmt.Sprintf("downscaled %dx%d to %dx%d; fetch %s", region.Dx(), region.Dy(), w, h, cfg.Ref.ID),
		})
	}

	var buf bytes.Buffer
	if err := png.Encode(&buf, result); err != nil {
		return nil, fmt.Errorf("ladder: encode reduced image: %w", err)
	}

	out.Bytes = buf.Bytes()
	out.Width, out.Height = w, h
	out.TokensAfter = EstimateImageTokens(w, h)
	return out, nil
}

// scaledTo fits w×h inside the bounds, preserving aspect ratio and never
// enlarging. Upscaling would cost tokens to add no information.
func scaledTo(w, h, maxW, maxH int) (int, int) {
	sw, sh := w, h
	if maxW > 0 && sw > maxW {
		sh = sh * maxW / sw
		sw = maxW
	}
	if maxH > 0 && sh > maxH {
		sw = sw * maxH / sh
		sh = maxH
	}
	return max(sw, 1), max(sh, 1)
}

// boxDownscale averages each source region into one destination pixel.
//
// A box filter rather than nearest-neighbour because the point is to
// preserve what a judgement was made on: dropping three of every four
// pixels can lose a thin highlight entirely, while averaging keeps it as
// a tint.
func boxDownscale(src *image.RGBA, w, h int) *image.RGBA {
	b := src.Bounds()
	dst := image.NewRGBA(image.Rect(0, 0, w, h))

	for y := range h {
		y0 := b.Min.Y + y*b.Dy()/h
		y1 := b.Min.Y + (y+1)*b.Dy()/h
		if y1 <= y0 {
			y1 = y0 + 1
		}
		for x := range w {
			x0 := b.Min.X + x*b.Dx()/w
			x1 := b.Min.X + (x+1)*b.Dx()/w
			if x1 <= x0 {
				x1 = x0 + 1
			}

			var r, g, bl, a, n uint32
			for sy := y0; sy < y1; sy++ {
				for sx := x0; sx < x1; sx++ {
					sr, sg, sb, sa := src.At(sx, sy).RGBA()
					r += sr
					g += sg
					bl += sb
					a += sa
					n++
				}
			}
			if n == 0 {
				continue
			}
			dst.Set(x, y, color.RGBA{
				R: uint8(r / n >> 8),
				G: uint8(g / n >> 8),
				B: uint8(bl / n >> 8),
				A: uint8(a / n >> 8),
			})
		}
	}
	return dst
}

// TextReduction configures [ElideText].
type TextReduction struct {
	// Head and Tail are how many runes to keep from each end. The
	// middle of a long artifact is usually the least load-bearing part,
	// and both ends usually matter.
	Head, Tail int
	// Ref identifies the original.
	Ref Ref
}

// ElideText shortens bulk text, recording what went and how to get it.
//
// This is how "pass by reference rather than by inclusion" is expressed
// on the wire: a transcript costs a summary and a pointer unless a rung
// actually needs the text.
func ElideText(s string, cfg *TextReduction) (string, Marker, bool) {
	if cfg == nil {
		return s, Marker{}, false
	}
	runes := []rune(s)
	keep := cfg.Head + cfg.Tail
	if keep <= 0 || len(runes) <= keep {
		return s, Marker{}, false
	}

	dropped := len(runes) - keep
	m := Marker{
		Kind: MarkerElision,
		Text: fmt.Sprintf("%d characters omitted; fetch %s", dropped, cfg.Ref.ID),
	}
	return string(runes[:cfg.Head]) + " " + m.String() + " " + string(runes[len(runes)-cfg.Tail:]), m, true
}
