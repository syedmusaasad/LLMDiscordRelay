package tmuxctl

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestInvariant5_RecipesVersionPinned makes the researched CLI version an
// explicit maintenance gate: adding a keystroke recipe requires recording the
// version against which it was verified .
func TestInvariant5_RecipesVersionPinned(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate recipes test source")
	}
	data, err := os.ReadFile(filepath.Join(filepath.Dir(thisFile), "recipes.go"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(data), "\n")
	for i, line := range lines {
		if !strings.Contains(line, "Recipe =") {
			continue
		}
		start := i - 3
		if start < 0 {
			start = 0
		}
		if !strings.Contains(strings.Join(lines[start:i+1], "\n"), "verified against Claude Code 2.1.319") {
			t.Fatalf("recipe declaration at line %d lacks Claude Code 2.1.319 pin", i+1)
		}
	}
}

func TestRecipes(t *testing.T) {
	eq := func(name string, got, want KeyRecipe) {
		t.Helper()
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("%s = %v, want %v", name, got, want)
		}
	}
	eq("approve", ApprovalApproveRecipe, KeyRecipe{"1"})
	eq("deny", ApprovalDenyRecipe, KeyRecipe{"Escape"})
	eq("trust accept", TrustAcceptRecipe, KeyRecipe{"Down", "Enter"})
	eq("trust deny", TrustDenyRecipe, KeyRecipe{"Escape"})
	for option, want := range map[int]KeyRecipe{
		0: {"Enter"},
		1: {"Down", "Enter"},
		3: {"Down", "Down", "Down", "Enter"},
	} {
		got, err := QuestionRecipe(option)
		if err != nil {
			t.Fatal(err)
		}
		eq("question", got, want)
	}
	if _, err := QuestionRecipe(-1); err == nil {
		t.Error("negative option must be refused")
	}
}
