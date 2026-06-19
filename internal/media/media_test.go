package media

import (
	"context"
	"image/png"
	"os"
	"testing"

	"ai-social-publisher/internal/account"
	"ai-social-publisher/internal/ai"
	"ai-social-publisher/internal/post"
)

func TestRenderPostImageWithTurkishText(t *testing.T) {
	renderer, err := NewTemplateRenderer(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	result, err := renderer.RenderPostImage(context.Background(), post.Variant{
		VariantNo: 1, Caption: "İş dünyası için özgün çözüm",
	}, ai.NewsCandidate{
		Title: "Türkiye'de yeni teknoloji girişimi", Source: "Örnek",
	}, account.Account{Code: "teknoloji", Category: "technology"})
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(result.LocalPath)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	img, err := png.Decode(f)
	if err != nil {
		t.Fatal(err)
	}
	if img.Bounds().Dx() != 1080 || img.Bounds().Dy() != 1080 {
		t.Fatalf("unexpected image bounds: %v", img.Bounds())
	}
}
