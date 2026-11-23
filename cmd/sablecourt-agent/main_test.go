package main

import (
	"testing"
)

func TestParseToolDirective(t *testing.T) {
	dir, ok := parseToolDirective(`{"mcp_tool":"sablecourt.list_workloads","args":{}}`)
	if !ok || dir.MCPTool != "sablecourt.list_workloads" {
		t.Fatalf("expected MCP directive, got %+v ok=%v", dir, ok)
	}
	if _, ok := parseToolDirective("Regular assistant response."); ok {
		t.Fatal("expected plain text not to parse as tool directive")
	}
}

func TestExtractTextAllProviders(t *testing.T) {
	if got := extractText("openai", []byte(`{"choices":[{"message":{"content":"hello openai"}}]}`)); got != "hello openai" {
		t.Fatalf("openai extract failed: %q", got)
	}
	if got := extractText("anthropic", []byte(`{"content":[{"text":"hello claude"}]}`)); got != "hello claude" {
		t.Fatalf("anthropic extract failed: %q", got)
	}
	if got := extractText("gemini", []byte(`{"candidates":[{"content":{"parts":[{"text":"hello gemini"}]}}]}`)); got != "hello gemini" {
		t.Fatalf("gemini extract failed: %q", got)
	}
}
