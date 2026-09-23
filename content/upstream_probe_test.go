package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadStateIgnoresOversizedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.tsv")
	data := strings.Repeat("x", maxHealthStateFileBytes+1)
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}

	if got := loadState(path); len(got) != 0 {
		t.Fatalf("loadState(%q) returned %d entries for an oversized file", path, len(got))
	}
}

func TestLoadStateRejectsOversizedLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.tsv")
	line := strings.Repeat("a", maxHealthStateLineBytes+1)
	data := line + "\nhttps://ok.example/dns-query\t25.5\t1\n"
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}

	if got := loadState(path); len(got) != 0 {
		t.Fatalf("loadState(%q) returned %d entries after an oversized line", path, len(got))
	}
}

func TestLoadStateParseErrorClearsPartialResults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.tsv")
	valid := "https://ok.example/dns-query\t25.5\t1\n"
	oversized := strings.Repeat("b", maxHealthStateLineBytes+1)
	if err := os.WriteFile(path, []byte(valid+oversized+"\n"), 0600); err != nil {
		t.Fatal(err)
	}

	if got := loadState(path); len(got) != 0 {
		t.Fatalf("loadState(%q) returned %d entries after a scanner parse error", path, len(got))
	}
}

func TestLoadStateBoundsEntries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.tsv")
	var b strings.Builder
	for i := 0; i < maxHealthStateEntries*2; i++ {
		fmt.Fprintf(&b, "https://resolver-%d.example/dns-query\t%.3f\t%d\n", i, float64(i+1), i)
	}
	if err := os.WriteFile(path, []byte(b.String()), 0600); err != nil {
		t.Fatal(err)
	}

	got := loadState(path)
	if len(got) != maxHealthStateEntries {
		t.Fatalf("loadState(%q) returned %d entries, want %d", path, len(got), maxHealthStateEntries)
	}
}

func TestLoadStateRejectsMalformedInput(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.tsv")
	data := strings.Join([]string{
		"https://ok.example/dns-query\t25.5\t1",
		"not-enough-fields",
		"https://bad-ewma.example/dns-query\tnot-a-number\t0",
		"https://bad-failure.example/dns-query\t25.5\t-1",
		"https://nan.example/dns-query\tNaN\t0",
		"https://inf.example/dns-query\t+Inf\t0",
	}, "\n") + "\n"
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}

	got := loadState(path)
	if len(got) != 1 {
		t.Fatalf("loadState(%q) returned %d valid entries, want 1", path, len(got))
	}
	if st, ok := got["https://ok.example/dns-query"]; !ok || st.ewma != 25.5 || st.failures != 1 {
		t.Fatalf("valid state was not restored correctly: %#v", got)
	}
}
