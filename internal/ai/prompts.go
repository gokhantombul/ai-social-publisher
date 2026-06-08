package ai

import (
	"fmt"
	"strings"
)

// buildScorePrompt builds the dynamic news scoring prompt. The model is asked to
// return strict JSON only.
func buildScorePrompt(news NewsCandidate) string {
	var b strings.Builder
	b.WriteString("Sen bir sosyal medya editör asistanısın. ")
	b.WriteString("Aşağıdaki haberi Türkiye'deki Instagram kitlesi için değerlendir.\n\n")

	b.WriteString("Haber bilgileri:\n")
	fmt.Fprintf(&b, "- Başlık: %s\n", news.Title)
	fmt.Fprintf(&b, "- Özet: %s\n", news.Summary)
	fmt.Fprintf(&b, "- Kaynak: %s\n", news.Source)
	fmt.Fprintf(&b, "- Kategori: %s\n", news.Category)
	b.WriteString("\n")

	b.WriteString("Kurallar:\n")
	b.WriteString("- Sadece geçerli JSON döndür.\n")
	b.WriteString("- Markdown kullanma.\n")
	b.WriteString("- Kod bloğu kullanma.\n")
	b.WriteString("- Açıklama yazma.\n")
	b.WriteString("- Kaynakta olmayan bilgi uydurma.\n")
	b.WriteString("- Türkiye'deki Instagram kitlesi için haberin ilgi çekip çekmeyeceğini değerlendir.\n")
	b.WriteString("- Ekonomi haberlerinde yatırım tavsiyesi verme.\n")
	b.WriteString("- Panik veya manipülatif dil kullanma.\n")
	b.WriteString("- Kesin al/sat yönlendirmesi yapma.\n")
	b.WriteString("- importanceScore ve viralityScore 0-100 arasında olsun.\n\n")

	b.WriteString("JSON formatı:\n")
	b.WriteString(`{
  "importanceScore": 0,
  "viralityScore": 0,
  "accountFit": "technology|cinema|news|economy|skip",
  "shouldNotify": true,
  "riskLevel": "low|medium|high",
  "reason": "kısa açıklama"
}`)
	return b.String()
}

// buildVariantsPrompt builds the dynamic post-generation prompt. The number of
// alternatives is driven entirely by req.VariantCount (never hard-coded).
func buildVariantsPrompt(req GeneratePostVariantsRequest) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Aşağıdaki haber için Instagram'da paylaşılabilecek %d farklı Türkçe post alternatifi üret.\n\n", req.VariantCount)

	b.WriteString("Haber bilgileri:\n")
	fmt.Fprintf(&b, "- Başlık: %s\n", req.News.Title)
	fmt.Fprintf(&b, "- Özet: %s\n", req.News.Summary)
	fmt.Fprintf(&b, "- Kaynak: %s\n", req.News.Source)
	fmt.Fprintf(&b, "- Kategori: %s\n", req.Category)
	if len(req.Styles) > 0 {
		fmt.Fprintf(&b, "- Önerilen tarzlar: %s\n", strings.Join(req.Styles, ", "))
	}
	b.WriteString("\n")

	b.WriteString("Sadece geçerli JSON döndür.\n")
	b.WriteString("Markdown kullanma.\n")
	b.WriteString("Kod bloğu kullanma.\n")
	b.WriteString("Açıklama yazma.\n\n")

	b.WriteString("JSON formatı şu olsun:\n")
	b.WriteString(`{
  "variants": [
    {
      "variantNo": 1,
      "style": "...",
      "caption": "..."
    }
  ]
}` + "\n\n")

	b.WriteString("Kurallar:\n")
	fmt.Fprintf(&b, "- Toplam %d alternatif üret.\n", req.VariantCount)
	b.WriteString("- variantNo 1'den başlasın.\n")
	b.WriteString("- Her alternatif farklı yazım tarzında olsun.\n")
	b.WriteString("- Türkçe yaz.\n")
	b.WriteString("- Kaynakta olmayan bilgi ekleme.\n")
	b.WriteString("- Abartılı clickbait yapma.\n")
	b.WriteString("- En fazla 8 hashtag kullan.\n")
	b.WriteString("- Gereksiz emoji kullanma.\n")
	b.WriteString("- Ekonomi haberlerinde yatırım tavsiyesi verme.\n")
	b.WriteString("- Ekonomi haberlerinde kesin yön tahmini yapma.\n")
	b.WriteString("- Haber kanalı için ciddi ve sade dil kullan.\n")
	b.WriteString("- Sinema için daha sosyal medya uyumlu dil kullan.\n")
	b.WriteString("- Teknoloji için kısa, net ve merak uyandırıcı dil kullan.\n")

	if req.Category == "economy" {
		b.WriteString("- Bu bir ekonomi haberidir: \"al\", \"sat\", \"kaçırma\", \"garanti kazanç\" gibi yönlendirme yapma.\n")
		b.WriteString("- Kaynakta olmayan fiyat, oran veya tarih ekleme.\n")
		b.WriteString("- Gerekirse caption sonunda \"Bu içerik yatırım tavsiyesi değildir.\" ifadesini kullan.\n")
	}

	return b.String()
}

// buildImagePromptPrompt asks the model for a short visual description that can
// drive a template/headline image.
func buildImagePrompt(news NewsCandidate) string {
	var b strings.Builder
	b.WriteString("Aşağıdaki haber için kısa ve net bir görsel sahne açıklaması üret. ")
	b.WriteString("Tek paragraf, Türkçe, en fazla 2 cümle. Sadece açıklama metnini döndür, JSON veya markdown kullanma.\n\n")
	fmt.Fprintf(&b, "Başlık: %s\n", news.Title)
	fmt.Fprintf(&b, "Özet: %s\n", news.Summary)
	return b.String()
}
