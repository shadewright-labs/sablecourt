package redact

import (
	"strings"
	"testing"
)

func FuzzString(f *testing.F) {
	seeds := []string{
		"",
		"ordinary log line without credentials",
		"secret:\n  value: hunter2\nstatus: ok\n",
		"secret:\n  prod:\n    value: \"quoted value\"\n",
		"secret:\r\n\tvalue: |\r\n\t  line1\r\n\t  line2\r\n",
		"secret:\n  secret: |#abcdefg hijklmn\n",
		`{"text":"secret:\n  value: hunter2hunter2\n"}`,
		`{"secret":{"value":"hunter2hunter2"},"token":["abc12345"]}`,
		"secret: {value: hunter2hunter2} secret: [abcdefgh, ijklmnop]",
		"postgres://app:" + "p@ssw0rd99" + "@example.com:5432/x",
		"redis://:" + "pa/ss1234" + "@cache.internal:6379/0",
		`{"u":"https:\/\/user:hunter2pw@example.com\/x"}`,
		"Authorization: Basic " + joinParts("dXNlcjp", "maXh0dXJl"),
		"Proxy-Authorization: Bearer " + joinParts("tok_", "0123456789!tail"),
		`Authorization: Digest username="u", response="0123456789abcdef"`,
		`{"Authorization":["Bearer abcdefghijkl"]}`,
		"map[Authorization:[Bearer abcdefghijkl]]",
		"Cookie: session=abcdef0123456789; theme=dark",
		"Set-Cookie: sid=abcdef0123456789; Path=/; HttpOnly",
		joinParts("eyJ", "hbGciOiJIUzI1NiJ9.", "eyJ", "zdWIiOiIxIn0.", "c2lnbmF0dXJlMTIzNA=="),
		joinParts("eyJ", "hbGciOiJkaXIifQ.", "ENCKEY.", "IVIVIV.", "CIPHER.", "TAGTAG"),
		pemBlock("RSA PRIVATE KEY", "MIIEfixture", "\n", true),
		pemBlock("OPENSSH PRIVATE KEY", "MIIEfixture", "\n", false),
		joinParts("ghp_", "0123456789", "\xff", "abcdefghij", "klmnopqrst"),
		joinParts("ghp_", "0123456789", "\u200b\x1b[0m", "abcdefghij", "klmnopqrst"),
		`{"api_key":"fixture with spaces","status":"ok"}`,
		"a://:x@a://:x@",
		"secret:\n a:1\n b:2\n c\n d\n",
		"status=ok\xffcount=42",
		"<password>hunter2hunter2</password>",
		"curl -u " + joinParts("admin:", "hunter2hunter2") + " https://api.invalid/v1",
		"http://pass='abcdef'@example.com/x",
		"password: \x00abcdef",
		"postgres://app:hunter2,pw@example.com/x",
		"postgres://app:hunter2;pw)123]456}789@example.com/x",
		"https://[2001:db8::1]/u/bob@example.com",
		"map[Set-Cookie:[sid=abc123; Path=/ refresh=xyz789; Secure]]",
		`{"Set-Cookie":["a=b; Path=/","refresh=xyz789"]}`,
		"curl -H 'Cookie: a=b' https://x.invalid",
		"DB_PASSWORD=Xy9(kLm2nPq password=Zq9xK2)mV7pLw",
		joinParts("-----BEGIN ", "PRIVATE KEY----- ") + "MIIEvQIBADANBgkqhkiG9w0BAQE",
		`{"msg":"{\"k\":\"` + joinParts("-----BEGIN ", "PRIVATE KEY-----") + `\\nMIIEvQIBADANBgkqhkiG9w0BAQE\"}"}`,
		"imagePullSecrets:\n- name: regcred\n",
		"credentials: {region: eu, profile: x}",
		"Authorization: skipping verification",
		"Authorization: Bearer authentication failed",
		`{"text":"secret:\u000a  value: hunter2hunter2\u000a"}`,
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, input string) {
		once := String(input)
		maxLen := 6*len(input) + 64
		if len(once) > maxLen {
			t.Fatalf("redacted output grew too much: len(in)=%d len(out)=%d limit=%d", len(input), len(once), maxLen)
		}
		twice := String(once)
		if twice != once {
			t.Fatalf("String is not idempotent:\ninput: %q\nonce:  %q\ntwice: %q", input, once, twice)
		}
	})
}

func FuzzStringNoLeak(f *testing.F) {
	f.Add(uint8(0), "prefix log line", "suffix log line")
	f.Add(uint8(3), `{"a":1}`, "status=ok")
	f.Add(uint8(7), "before\n", "\nafter")

	const needle = "FUZZSECRET9876543210"
	// Only templates that main's redactor hides are listed: the fuzz target
	// asserts that the needle never survives. Shapes it leaves visible are in
	// the known-gap table.
	templates := []func(string) string{
		func(s string) string { return "password=" + s },
		func(s string) string { return `{"api_key":"` + s + `"}` },
		func(s string) string { return "secret: " + s + "\n" },
		func(s string) string { return "Authorization: Bearer " + s },
		func(s string) string { return "ghp_" + s },
		func(s string) string { return "DB_PASSWORD=Xy9(" + s },
		func(s string) string { return "password=Zq9xK2)" + s },
		func(s string) string { return "sk-ant-" + s },
	}

	f.Fuzz(func(t *testing.T, kind uint8, prefix, suffix string) {
		cleanPrefix, cleanSuffix, ok := sanitizeNoLeakParts(prefix, suffix, needle)
		if !ok {
			return
		}

		tmpl := templates[int(kind)%len(templates)]
		// The boundary line stops a prefix that ends in a key or header from
		// reaching the template across the newline; main hides that shape only
		// partly, and the known-gap table records it.
		input := cleanPrefix + "\n---\n" + tmpl(needle) + "\n" + cleanSuffix
		once := String(input)
		if strings.Contains(once, needle) {
			t.Fatalf("leaked secret needle %q in template %d:\ninput: %q\noutput: %q", needle, int(kind)%len(templates), input, once)
		}
		if twice := String(once); twice != once {
			t.Fatalf("String is not idempotent:\ninput: %q\nonce:  %q\ntwice: %q", input, once, twice)
		}
	})
}

// sanitizeNoLeakParts strips the characters that could close or merge the
// template boundary from prefix and suffix. It reports false when the needle
// is present in either cleaned value, checked after the stripping because
// removing characters can join the pieces of a needle.
func sanitizeNoLeakParts(prefix, suffix, needle string) (string, string, bool) {
	clean := func(s string) string {
		for _, drop := range []string{"\"", "'", "\\"} {
			s = strings.ReplaceAll(s, drop, "")
		}
		return s
	}
	cleanPrefix := strings.ReplaceAll(clean(prefix), "-----BEGIN ", "")
	cleanSuffix := clean(suffix)
	if strings.Contains(cleanPrefix, needle) || strings.Contains(cleanSuffix, needle) {
		return "", "", false
	}
	return cleanPrefix, cleanSuffix, true
}

// TestSanitizeNoLeakPartsRejectsJoinedNeedle covers a needle that exists only
// after the stripping: a quote, a backslash or a BEGIN marker inside it.
func TestSanitizeNoLeakPartsRejectsJoinedNeedle(t *testing.T) {
	const needle = "FUZZSECRET9876543210"
	for _, tc := range []struct {
		name           string
		prefix, suffix string
		wantOK         bool
	}{
		{"plain", "log line", "status=ok", true},
		{"raw needle in prefix", "x" + needle, "", false},
		{"quote inside needle in prefix", `FUZZSECRET98"76543210`, "", false},
		{"backslash inside needle in suffix", "", `FUZZ\SECRET9876543210`, false},
		{"apostrophe inside needle in suffix", "", "FUZZ'SECRET9876543210", false},
		{"begin marker inside needle in prefix", "FUZZSECRET98-----BEGIN 76543210", "", false},
	} {
		if _, _, ok := sanitizeNoLeakParts(tc.prefix, tc.suffix, needle); ok != tc.wantOK {
			t.Errorf("%s: ok=%v, want %v", tc.name, ok, tc.wantOK)
		}
	}
}
