// Package media renders post images from a template. The first implementation
// draws a 1080x1080 PNG card with the channel label, headline, source and date.
package media

import (
	"context"
	"crypto/rand"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"time"

	"ai-social-publisher/internal/account"
	"ai-social-publisher/internal/ai"
	"ai-social-publisher/internal/post"

	"golang.org/x/image/font"
	"golang.org/x/image/font/gofont/gobold"
	"golang.org/x/image/font/gofont/goregular"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"
)

// RenderedMedia is the output of the renderer: a local file path plus metadata.
type RenderedMedia struct {
	LocalPath string
	MimeType  string
}

// MediaRenderer renders a post image for a selected variant.
type MediaRenderer interface {
	RenderPostImage(ctx context.Context, variant post.Variant, news ai.NewsCandidate, acct account.Account) (*RenderedMedia, error)
}

// theme holds per-category colors.
type theme struct {
	bg    color.RGBA
	label string
}

var themes = map[string]theme{
	"technology": {bg: color.RGBA{0x10, 0x2A, 0x43, 0xFF}, label: "TEKNOLOJİ"},
	"cinema":     {bg: color.RGBA{0x3B, 0x10, 0x2A, 0xFF}, label: "SİNEMA"},
	"news":       {bg: color.RGBA{0x1A, 0x1A, 0x1A, 0xFF}, label: "HABER"},
	"economy":    {bg: color.RGBA{0x0E, 0x33, 0x22, 0xFF}, label: "EKONOMİ"},
}

const (
	canvasSize = 1080
	margin     = 80
)

// TemplateRenderer draws cards as PNG files into outputDir. It keeps only the
// parsed fonts: an opentype.Face is not safe for concurrent use, so faces are
// created per render (an HTTP preview and a scheduled publish may render at the
// same time).
type TemplateRenderer struct {
	outputDir string
	regular   *opentype.Font
	bold      *opentype.Font
}

// NewTemplateRenderer ensures the output directory exists.
func NewTemplateRenderer(outputDir string) (*TemplateRenderer, error) {
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		return nil, fmt.Errorf("create media output dir: %w", err)
	}
	regular, err := opentype.Parse(goregular.TTF)
	if err != nil {
		return nil, fmt.Errorf("parse regular font: %w", err)
	}
	bold, err := opentype.Parse(gobold.TTF)
	if err != nil {
		return nil, fmt.Errorf("parse bold font: %w", err)
	}
	return &TemplateRenderer{outputDir: outputDir, regular: regular, bold: bold}, nil
}

// faces holds the per-render font faces.
type faces struct {
	label font.Face
	title font.Face
	body  font.Face
	meta  font.Face
}

func (r *TemplateRenderer) newFaces() (faces, error) {
	newFace := func(parsed *opentype.Font, size float64) (font.Face, error) {
		return opentype.NewFace(parsed, &opentype.FaceOptions{Size: size, DPI: 72, Hinting: font.HintingFull})
	}
	var f faces
	var err error
	if f.label, err = newFace(r.bold, 42); err != nil {
		return faces{}, err
	}
	if f.title, err = newFace(r.bold, 52); err != nil {
		return faces{}, err
	}
	if f.body, err = newFace(r.regular, 32); err != nil {
		return faces{}, err
	}
	if f.meta, err = newFace(r.regular, 25); err != nil {
		return faces{}, err
	}
	return f, nil
}

// RenderPostImage produces a 1080x1080 PNG card and returns its local path.
func (r *TemplateRenderer) RenderPostImage(ctx context.Context, variant post.Variant, news ai.NewsCandidate, acct account.Account) (*RenderedMedia, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	fc, err := r.newFaces()
	if err != nil {
		return nil, fmt.Errorf("prepare fonts: %w", err)
	}
	th, ok := themes[acct.Category]
	if !ok {
		th = theme{bg: color.RGBA{0x20, 0x20, 0x20, 0xFF}, label: strings.ToUpper(acct.Category)}
	}

	img := image.NewRGBA(image.Rect(0, 0, canvasSize, canvasSize))
	draw.Draw(img, img.Bounds(), &image.Uniform{th.bg}, image.Point{}, draw.Src)

	// Accent bar at the top.
	accent := color.RGBA{0xFF, 0xC1, 0x07, 0xFF}
	draw.Draw(img, image.Rect(0, 0, canvasSize, 12), &image.Uniform{accent}, image.Point{}, draw.Src)

	white := color.RGBA{0xFF, 0xFF, 0xFF, 0xFF}
	muted := color.RGBA{0xC8, 0xC8, 0xC8, 0xFF}

	y := 140
	// Category label.
	drawText(img, th.label, margin, y, accent, fc.label)
	y += 90

	// Headline (wrapped).
	for _, line := range wrap(news.Title, 34) {
		drawText(img, line, margin, y, white, fc.title)
		y += 64
		if y > canvasSize-360 {
			break
		}
	}

	y += 30
	// Short punchy sub-text from the caption's first line.
	sub := firstLine(variant.Caption)
	for _, line := range wrap(sub, 46) {
		drawText(img, line, margin, y, muted, fc.body)
		y += 43
		if y > canvasSize-200 {
			break
		}
	}

	// Footer: source + date.
	footer := news.Source
	date := news.PublishedAt
	if date.IsZero() {
		date = time.Now()
	}
	footer = strings.TrimSpace(footer + "  -  " + date.Format("02.01.2006"))
	drawText(img, footer, margin, canvasSize-margin, muted, fc.meta)

	// Optional logo area placeholder (top-right box).
	logoBox := image.Rect(canvasSize-margin-120, 60, canvasSize-margin, 180)
	draw.Draw(img, logoBox, &image.Uniform{color.RGBA{0xFF, 0xFF, 0xFF, 0x22}}, image.Point{}, draw.Over)

	// A random component keeps draft URLs unguessable: rendered media is served
	// publicly (Instagram must fetch it) before the operator decides to publish.
	var nonce [8]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nil, fmt.Errorf("generate media file name: %w", err)
	}
	name := fmt.Sprintf("post_%s_v%d_%d_%x.png", acct.Code, variant.VariantNo, time.Now().UnixNano(), nonce)
	outPath := filepath.Join(r.outputDir, name)

	f, err := os.Create(outPath)
	if err != nil {
		return nil, fmt.Errorf("create image file: %w", err)
	}
	if err := png.Encode(f, img); err != nil {
		_ = f.Close()
		_ = os.Remove(outPath)
		return nil, fmt.Errorf("encode png: %w", err)
	}
	// A failed close can mean unflushed data (e.g. disk full); a truncated file
	// must never be reported as a successful render.
	if err := f.Close(); err != nil {
		_ = os.Remove(outPath)
		return nil, fmt.Errorf("close image file: %w", err)
	}

	return &RenderedMedia{LocalPath: outPath, MimeType: "image/png"}, nil
}

func drawText(dst *image.RGBA, s string, x, y int, col color.Color, face font.Face) {
	d := &font.Drawer{
		Dst:  dst,
		Src:  image.NewUniform(col),
		Face: face,
		Dot:  fixed.P(x, y),
	}
	d.DrawString(s)
}

// wrap splits s into lines of at most width runes, breaking on spaces.
func wrap(s string, width int) []string {
	words := strings.Fields(s)
	if len(words) == 0 {
		return nil
	}
	var lines []string
	cur := words[0]
	for _, w := range words[1:] {
		if len([]rune(cur))+1+len([]rune(w)) > width {
			lines = append(lines, cur)
			cur = w
		} else {
			cur += " " + w
		}
	}
	return append(lines, cur)
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
