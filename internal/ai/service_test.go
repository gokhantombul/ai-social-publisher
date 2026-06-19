package ai

import (
	"context"
	"io"
	"log/slog"
	"testing"
)

type fakeProvider struct {
	name     string
	variants []PostVariant
}

func (p fakeProvider) Name() string                     { return p.name }
func (p fakeProvider) IsAvailable(context.Context) bool { return true }
func (p fakeProvider) ScoreNews(context.Context, NewsCandidate) (*NewsScore, error) {
	return &NewsScore{}, nil
}
func (p fakeProvider) GeneratePostVariants(context.Context, GeneratePostVariantsRequest) ([]PostVariant, error) {
	return p.variants, nil
}

func TestGeneratePostVariantsFallsBackOnWrongCount(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	service := NewService(logger,
		fakeProvider{name: "wrong", variants: []PostVariant{{Caption: "one"}}},
		fakeProvider{name: "right", variants: []PostVariant{{Caption: "one"}, {Caption: "two"}}},
	)
	result, err := service.GeneratePostVariants(context.Background(), GeneratePostVariantsRequest{VariantCount: 2})
	if err != nil {
		t.Fatal(err)
	}
	if result.Provider != "right" || len(result.Variants) != 2 {
		t.Fatalf("unexpected result: %+v", result)
	}
}
