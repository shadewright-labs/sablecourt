package docker

import (
	"strings"
	"testing"

	scruntime "github.com/shadewright-labs/sablecourt/internal/runtime"
)

// TestPreparePromptPrivateKeyRefusal checks that a prompt holding a PEM private
// key header is refused before redaction. The redactor replaces a whole PEM
// block, header included, with one marker, so the check after redaction alone
// would no longer see the header.
func TestPreparePromptPrivateKeyRefusal(t *testing.T) {
	header := "-----BEGIN " + "PRIVATE KEY-----"
	rsaHeader := "-----BEGIN RSA " + "PRIVATE KEY-----"
	footer := "-----END " + "PRIVATE KEY-----"
	cases := []struct {
		name    string
		prompt  string
		refused bool
		want    string
	}{
		{name: "full block", prompt: "review this\n" + header + "\nMIIEfixture\n" + footer + "\n", refused: true},
		{name: "header only", prompt: "here is " + header + "\nsecret", refused: true},
		{name: "rsa label", prompt: "key: " + rsaHeader + " MIIEfixture", refused: true},
		{name: "header mid line", prompt: "please check" + header, refused: true},
		{name: "plain prompt", prompt: "Review the diff and report findings.", want: "Review the diff and report findings."},
		{name: "public key header", prompt: "see -----BEGIN PUBLIC KEY-----\nMIIBfixture", want: "see -----BEGIN PUBLIC KEY-----\nMIIBfixture"},
		{name: "other secret redacted", prompt: "use password=Zq9xK2mV7pLw now", want: "use password=[REDACTED:secret] now"},
		{name: "task token replaced", prompt: "token tok-fixture-123 here", want: "token [REDACTED] here"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := preparePrompt(scruntime.CouncilLaunch{TaskToken: "tok-fixture-123", Prompt: tc.prompt})
			if tc.refused {
				if err == nil || !strings.Contains(err.Error(), "unsafe council prompt") {
					t.Fatalf("preparePrompt(%q) = %q, %v; want unsafe council prompt", tc.prompt, got, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("preparePrompt(%q) error: %v", tc.prompt, err)
			}
			if got != tc.want {
				t.Fatalf("preparePrompt(%q) = %q, want %q", tc.prompt, got, tc.want)
			}
		})
	}
}
