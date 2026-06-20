package infer

import "testing"

func TestMockModel(t *testing.T) {
	m := NewMockModel("test-model")
	if m.ModelName() != "test-model" {
		t.Fatalf("unexpected name %q", m.ModelName())
	}
	out, err := m.Complete("hi")
	if err != nil {
		t.Fatal(err)
	}
	if out != "context: hi" {
		t.Fatalf("unexpected completion %q", out)
	}
	if m.Calls() != 1 {
		t.Fatalf("expected 1 call, got %d", m.Calls())
	}
}

func TestBuild(t *testing.T) {
	none, err := Build("none", "", "", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if none != nil {
		t.Fatal("expected nil model for provider none")
	}

	empty, err := Build("", "", "", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if empty != nil {
		t.Fatal("expected nil model for empty provider")
	}

	mock, err := Build("mock", "m", "", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if mock == nil || mock.ModelName() != "m" {
		t.Fatalf("unexpected mock model %v", mock)
	}

	oa, err := Build("openai", "gpt-4o-mini", "", "", "https://api.openai.com", "OPENAI_API_KEY")
	if err != nil {
		t.Fatal(err)
	}
	if oa == nil {
		t.Fatal("expected openai model")
	}

	gem, err := Build("gemini", "gemini-2.0-flash", "", "", "", "GEMINI_API_KEY")
	if err != nil {
		t.Fatal(err)
	}
	if gem == nil {
		t.Fatal("expected gemini model")
	}

	if _, err := Build("bogus", "", "", "", "", ""); err == nil {
		t.Fatal("expected error for unknown provider")
	}
}
