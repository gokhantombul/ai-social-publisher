package ai

import "testing"

func TestCleanJSON(t *testing.T) {
	cases := map[string]string{
		`{"a":1}`:                            `{"a":1}`,
		"```json\n{\"a\":1}\n```":            `{"a":1}`,
		"```\n{\"a\":1}\n```":                `{"a":1}`,
		"Here is the result: {\"a\":1} done": `{"a":1}`,
		"prefix [\n1,2\n] suffix":            "[\n1,2\n]",
	}
	for in, want := range cases {
		if got := cleanJSON(in); got != want {
			t.Errorf("cleanJSON(%q)=%q want %q", in, got, want)
		}
	}
}

func TestParseScoreClamps(t *testing.T) {
	raw := `{"importanceScore": 150, "viralityScore": -10, "accountFit": "technology", "shouldNotify": true, "riskLevel": "low", "reason": "x"}`
	score, err := parseScore(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if score.ImportanceScore != 100 {
		t.Errorf("importance not clamped: got %d", score.ImportanceScore)
	}
	if score.ViralityScore != 0 {
		t.Errorf("virality not clamped: got %d", score.ViralityScore)
	}
}

func TestParseScoreMarkdown(t *testing.T) {
	raw := "```json\n{\"importanceScore\":80,\"viralityScore\":70,\"accountFit\":\"news\",\"shouldNotify\":true,\"riskLevel\":\"medium\",\"reason\":\"ok\"}\n```"
	score, err := parseScore(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if score.AccountFit != "news" || score.ImportanceScore != 80 {
		t.Errorf("unexpected score: %+v", score)
	}
}

func TestParseVariantsEnvelope(t *testing.T) {
	raw := `{"variants":[{"variantNo":1,"style":"a","caption":"c1"},{"variantNo":2,"style":"b","caption":"c2"}]}`
	vs, err := parseVariants(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(vs) != 2 {
		t.Fatalf("expected 2 variants, got %d", len(vs))
	}
}

func TestParseVariantsBareArrayAndNumbering(t *testing.T) {
	raw := `[{"style":"a","caption":"c1"},{"style":"b","caption":"c2"}]`
	vs, err := parseVariants(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(vs) != 2 || vs[0].VariantNo != 1 || vs[1].VariantNo != 2 {
		t.Fatalf("variant numbering wrong: %+v", vs)
	}
}

func TestParseVariantsEmpty(t *testing.T) {
	if _, err := parseVariants(`{"variants":[]}`); err == nil {
		t.Error("expected error for empty variants")
	}
}

func TestParseScoreRejectsUnknownEnums(t *testing.T) {
	if _, err := parseScore(`{"importanceScore":80,"viralityScore":70,"accountFit":"other","shouldNotify":true,"riskLevel":"extreme"}`); err == nil {
		t.Fatal("expected invalid enum error")
	}
}
