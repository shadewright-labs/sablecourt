package main

import (
	"os"
	"strings"
	"testing"
)

// TestReadTaskInputFromStdin: the input is the first stdin line when the
// runtime says so, never the environment (issue 261).
func TestReadTaskInputFromStdin(t *testing.T) {
	t.Setenv("SABLECOURT_INPUT_JSON", `{"prompt":"from the environment"}`)
	cases := []struct {
		name, flag, stdin string
		want              taskInput
		wantErr           bool
	}{
		{name: "object", flag: "1", stdin: `{"prompt":"p","provider":"openai","context_packet":"c"}` + "\n" + "takeover text\n", want: taskInput{Prompt: "p", Provider: "openai", ContextPacket: "c"}},
		{name: "JSON string is the prompt", flag: "1", stdin: `"line one\nline two"` + "\n", want: taskInput{Prompt: "line one\nline two"}},
		{name: "not JSON is the prompt", flag: "1", stdin: "plain words\n", want: taskInput{Prompt: "plain words"}},
		{name: "empty line", flag: "1", stdin: "\n", want: taskInput{}},
		{name: "no newline is refused", flag: "1", stdin: `{"prompt":"cut`, wantErr: true},
		{name: "empty stdin is refused", flag: "1", stdin: "", wantErr: true},
		{name: "over the cap is refused", flag: "1", stdin: `"` + strings.Repeat("a", maxTaskInputBytes) + `"` + "\n", wantErr: true},
		{name: "no flag ignores stdin and environment", flag: "", stdin: `{"prompt":"p"}` + "\n", want: taskInput{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := readTaskInput(tc.flag, strings.NewReader(tc.stdin))
			if (err != nil) != tc.wantErr || got != tc.want {
				t.Fatalf("readTaskInput = %+v, %v; want %+v, err %t", got, err, tc.want, tc.wantErr)
			}
		})
	}
	if os.Getenv("SABLECOURT_INPUT_JSON") == "" {
		t.Fatal("fixture lost")
	}
}
