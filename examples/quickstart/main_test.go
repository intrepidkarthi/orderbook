package main

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

// TestOutputIsPinned: the quickstart prints what the committed output says.
func TestOutputIsPinned(t *testing.T) {
	var got bytes.Buffer
	run(&got)
	want, err := os.ReadFile("testdata/output.txt")
	if err != nil {
		t.Fatal(err)
	}
	if got.String() != string(want) {
		t.Fatalf("output changed; if intended, regenerate testdata/output.txt and the README block.\ngot:\n%s", got.String())
	}
}

// TestReadmeShowsTheRealOutput: the README's quickstart must quote the program's
// actual output, so the front page cannot drift from what the engine does.
func TestReadmeShowsTheRealOutput(t *testing.T) {
	readme, err := os.ReadFile("../../README.md")
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile("testdata/output.txt")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(readme), "```text\n"+string(want)+"```") {
		t.Fatal("README.md does not show examples/quickstart's output verbatim in a ```text block")
	}
}
