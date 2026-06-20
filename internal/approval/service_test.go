package approval

import (
	"context"
	"strings"
	"testing"
)

func TestUpdateVariantCaptionRejectsInvalidLength(t *testing.T) {
	service := &Service{}
	for _, caption := range []string{"   ", strings.Repeat("ğ", 2201)} {
		if err := service.UpdateVariantCaption(context.Background(), 1, 1, caption); err == nil {
			t.Fatalf("expected caption %q to be rejected", caption[:min(len(caption), 12)])
		}
	}
}
