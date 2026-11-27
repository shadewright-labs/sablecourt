package redact

import (
	"strings"
	"testing"
)

func joinParts(parts ...string) string {
	return strings.Join(parts, "")
}

func pemBlock(label, body, sep string, closed bool) string {
	header := joinParts("-----BEGIN ", label, "-----")
	if !closed {
		return header + sep + body
	}
	footer := joinParts("-----END ", label, "-----")
	return header + sep + body + sep + footer
}

func TestCorpusMustHide(t *testing.T) {
	ghp := joinParts("ghp_", "0123456789", "abcdefghij", "klmnopqrst")
	ghpSplit := joinParts("ghp_", "0123456789", "\xff", "abcdefghij", "klmnopqrst")
	ghpSplitPrefix := joinParts("gh", "\xff", "p_", "0123456789", "abcdefghij", "klmnopqrst")
	ghpSplitANSI := joinParts("ghp_", "0123456789", "\x1b[0m", "abcdefghij", "klmnopqrst")
	ghpSplitSoftHyphen := joinParts("ghp_", "0123456789", "\u00ad", "abcdefghij", "klmnopqrst")
	ghpSplitZWSP := joinParts("ghp_", "0123456789", "\u200b", "abcdefghij", "klmnopqrst")
	ghpSplitZWSPTail := joinParts("ghp_", "0123456789abcdefghij", "\u200b", "klmnopqrst")
	ghpSplitNUL := joinParts("ghp_", "0123456789", "\x00", "abcdefghij", "klmnopqrst")
	ghpSplitFFFD := joinParts("ghp_", "0123456789", "\ufffd", "abcdefghij", "klmnopqrst")
	ghpSplitCRLF := joinParts("ghp_", "0123456789", "\r\n", "abcdefghij", "klmnopqrst")
	gho := joinParts("gho_", "0123456789", "abcdefghij", "klmnopqrst")
	ghu := joinParts("ghu_", "0123456789", "abcdefghij", "klmnopqrst")
	ghs := joinParts("ghs_", "0123456789", "abcdefghij", "klmnopqrst")
	ghr := joinParts("ghr_", "0123456789", "abcdefghij", "klmnopqrst")
	ghPat := joinParts("github_", "pat_", "0123456789", "_abcdefghij_klmnop")
	scTask := joinParts("sc_", "task_", "0123456789abcdef", "fedcba9876543210")
	opTok := joinParts("op_", strings.Repeat("0123456789abcdef", 4))
	opTokSplit := joinParts("op_", strings.Repeat("0123456789abcdef", 2), "\xfe", strings.Repeat("0123456789abcdef", 2))
	antKey := joinParts("sk-", "ant-", "api03-", "fixture0123456789abcdef")
	oaiKey := joinParts("sk-", "proj-", "fixture0123456789abcdef")
	oaiLegacy := joinParts("sk-", "fixture0123456789abcdef")
	googKey := joinParts("AI", "za", "SyFixture0123456789abcd")
	awsAKIA := joinParts("AK", "IA", "FIXTURE012345678")
	awsASIA := joinParts("AS", "IA", "FIXTURE012345678")
	awsSplit := joinParts("AK", "IA", "FIXTURE", "\x80", "012345678")
	awsSecret := joinParts("wJalrXUtnFEMI/", "K7MDENG/", "bPxRfiCYFIXTUREKEY")
	stripeLive := joinParts("sk_", "live_", "0123456789abcdefghij", "klmnop")
	slackBot := joinParts("xox", "b-", "0123456789", "-0123456789-abcdefghijkl")

	basicB64 := joinParts("dXNlcjp", "maXh0dXJl")
	basicPadded := joinParts("YXBwOn", "Bhc3N3b3Jk", "==")
	basicUserPass := joinParts("dXNlcjp", "wYXNzd29yZA==")
	bearerVal := joinParts("tok_", "fixture_", "0123456789abcdef")

	jwtNoPad := joinParts(
		"eyJ", "hbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.",
		"eyJ", "zdWIiOiJmaXh0dXJlIn0.",
		"c2lnbmF0dXJlZml4dHVyZTEyMw",
	)
	jwtPadded := joinParts(
		"eyJ", "hbGciOiJIUzI1NiJ9.",
		"eyJ", "zdWIiOiJmaXh0dXJlIn0=.",
		"c2lnbmF0dXJlZml4dHVyZQ==",
	)
	jwtUnsignedEmptySig := joinParts(
		"eyJ", "hbGciOiJub25lIn0.",
		"eyJ", "zdWIiOiJ4eHh4In0.",
	)
	jwtTwoSeg := joinParts(
		"eyJ", "hbGciOiJub25lIn0.",
		"eyJ", "zdWIiOiJ4eHh4In0",
	)
	jweFiveSeg := joinParts(
		"eyJ", "hbGciOiJkaXIifQ.",
		"ENCKEYENCKEY.",
		"IVIVIVIVIV.",
		"CIPHERTEXTCIPHER.",
		"TAGTAGTAGTAG",
	)
	jwtStdB64Sig := joinParts(
		"eyJ", "hbGciOiJIUzI1NiJ9.",
		"eyJ", "zdWIiOiJ4eHh4In0.",
		"abcdef/ghijklmnop+qrstu",
	)
	jwtWrappedSig := joinParts(
		"eyJ", "hbGciOiJIUzI1NiJ9.",
		"eyJ", "zdWIiOiJ4eHh4In0.",
		"c2lnbmF0\ndXJlMTIz",
	)
	jwtWrappedDots := joinParts(
		"eyJ", "hbGciOiJIUzI1NiJ9\n.",
		"eyJ", "zdWIiOiJ4eHh4In0\n.",
		"c2lnbmF0dXJlMTIz",
	)
	jwtSpacedDots := joinParts(
		"eyJ", "hbGciOiJIUzI1NiJ9 . ",
		"eyJ", "zdWIiOiJ4eHh4In0 . ",
		"c2lnbmF0dXJlMTIz",
	)

	pemBody := joinParts("MIIE", "fixtureBodyLine1\n", "fixtureBodyLine2")
	pemBodyCRLF := joinParts("MIIE", "fixtureBodyLine1\r\n", "fixtureBodyLine2")

	cases := []struct {
		name           string
		input          string
		mustNotContain []string
		mustContain    []string
		wantCategory   string
		wantMarker     string
	}{
		// Nested YAML key and value plus near variants.
		{
			name:           "yaml nested secret value",
			input:          "secret:\n  value: hunter2\n",
			mustNotContain: []string{"hunter2"},
			mustContain:    []string{"secret:", "value:"},
			wantMarker:     "[REDACTED:secret]",
		},
		{
			name:           "yaml nested secret double quoted with spaces",
			input:          "secret:\n  value: \"hunter2 with spaces\"\n",
			mustNotContain: []string{"hunter2", "with spaces"},
			mustContain:    []string{"secret:", "value:"},
		},
		{
			name:           "yaml nested secret single quoted",
			input:          "secret:\n  value: 'hunter2-single'\n",
			mustNotContain: []string{"hunter2-single"},
			mustContain:    []string{"secret:", "value:"},
		},
		{
			name:           "yaml nested secret with tab indent",
			input:          "secret:\n\tvalue: tabHunter2\n",
			mustNotContain: []string{"tabHunter2"},
			mustContain:    []string{"secret:", "value:"},
		},
		{
			name:           "yaml nested secret with CRLF",
			input:          "secret:\r\n  value: crlfHunter2\r\n",
			mustNotContain: []string{"crlfHunter2"},
			mustContain:    []string{"secret:", "value:"},
		},
		{
			name:           "yaml nested mixed case keys",
			input:          "Client_Secret:\n  Value: mixedCaseVal99\n",
			mustNotContain: []string{"mixedCaseVal99"},
			mustContain:    []string{"Client_Secret:", "Value:"},
		},
		{
			name:           "yaml nested password one level",
			input:          "password:\n  value: passOneLevel9\n",
			mustNotContain: []string{"passOneLevel9"},
			mustContain:    []string{"password:", "value:"},
		},
		{
			name:           "yaml nested token one level",
			input:          "token:\n  value: tokOneLevel9\n",
			mustNotContain: []string{"tokOneLevel9"},
			mustContain:    []string{"token:", "value:"},
		},
		{
			name:           "yaml nested api_key one level",
			input:          "api_key:\n  value: keyOneLevel9\n",
			mustNotContain: []string{"keyOneLevel9"},
			mustContain:    []string{"api_key:", "value:"},
		},
		{
			name:           "yaml nested secret two levels under secret",
			input:          "secret:\n  prod:\n    value: deepSecretVal2\n",
			mustNotContain: []string{"deepSecretVal2"},
			mustContain:    []string{"secret:", "prod:", "value:"},
		},
		{
			name:           "yaml nested password two levels under non-secret parent",
			input:          "database:\n  password:\n    value: dbPassDeep2\n  host: db.internal\n",
			mustNotContain: []string{"dbPassDeep2"},
			mustContain:    []string{"database:", "password:", "host: db.internal"},
		},
		{
			name:           "yaml nested token two levels",
			input:          "auth:\n  token:\n    primary: tokDeepVal2\n  mode: strict\n",
			mustNotContain: []string{"tokDeepVal2"},
			mustContain:    []string{"auth:", "token:", "mode: strict"},
		},
		{
			name:           "yaml nested api_key two levels",
			input:          "api_key:\n  staging:\n    raw: keyDeepVal2\nstatus: ok\n",
			mustNotContain: []string{"keyDeepVal2"},
			mustContain:    []string{"api_key:", "staging:", "status: ok"},
		},
		{
			name:           "yaml nested secret block scalar",
			input:          "secret:\n  value: |\n    blockLineSecret1\n    blockLineSecret2\nnext: keep\n",
			mustNotContain: []string{"blockLineSecret1", "blockLineSecret2"},
			mustContain:    []string{"next: keep"},
		},
		// H2: nested YAML inside JSON string with escaped newlines.
		{
			name:           "H2 json escaped newline nested yaml",
			input:          `{"text":"secret:\n  value: hunter2hunter2\n"}`,
			mustNotContain: []string{"hunter2hunter2"},
			mustContain:    []string{`"text":"secret:\n  value: `, `\n"}`},
			wantMarker:     "[REDACTED:secret]",
		},
		{
			name:           "H2 json escaped newline nested yaml two levels",
			input:          `{"text":"secret:\n  prod:\n    value: deepJsonEsc99\nstatus: ok\n"}`,
			mustNotContain: []string{"deepJsonEsc99"},
			mustContain:    []string{"status: ok"},
			wantMarker:     "[REDACTED:secret]",
		},
		{
			name:           "H2 json escaped newline nested yaml no trailing newline",
			input:          `{"text":"secret:\n  value: hunter2end"}`,
			mustNotContain: []string{"hunter2end"},
			mustContain:    []string{`"text":"secret:\n  value: `, `"}`},
			wantMarker:     "[REDACTED:secret]",
		},
		// H3: key synonyms, XML, CLI flags, curl -u.
		{
			name:           "H3 AWS_SECRET_ACCESS_KEY env",
			input:          "AWS_SECRET_ACCESS_KEY=" + awsSecret,
			mustNotContain: []string{"wJalrXUtnFEMI", "K7MDENG", "bPxRfiCYFIXTUREKEY"},
			mustContain:    []string{"AWS_SECRET_ACCESS_KEY="},
			wantMarker:     "[REDACTED:secret]",
		},
		{
			name:           "H3 secret_key kv",
			input:          "secret_key=hunter2hunter2",
			mustNotContain: []string{"hunter2hunter2"},
			mustContain:    []string{"secret_key="},
		},
		{
			name:           "H3 SECRET_KEY yaml",
			input:          "SECRET_KEY: djangoinsecure123",
			mustNotContain: []string{"djangoinsecure123"},
			mustContain:    []string{"SECRET_KEY: "},
		},
		{
			name:           "H3 secretKey camelCase",
			input:          "secretKey: abcdefghijkl",
			mustNotContain: []string{"abcdefghijkl"},
			mustContain:    []string{"secretKey: "},
		},
		{
			name:           "H3 private_key kv",
			input:          "private_key: abcdefghijkl",
			mustNotContain: []string{"abcdefghijkl"},
			mustContain:    []string{"private_key: "},
		},
		{
			name:           "H3 access_key kv",
			input:          "access_key=abcdefghij1234",
			mustNotContain: []string{"abcdefghij1234"},
			mustContain:    []string{"access_key="},
		},
		{
			name:           "H3 signing_key kv",
			input:          "signing_key=signFixture1234",
			mustNotContain: []string{"signFixture1234"},
			mustContain:    []string{"signing_key="},
		},
		{
			name:           "H3 pwd kv",
			input:          "pwd=hunter2hunter2",
			mustNotContain: []string{"hunter2hunter2"},
			mustContain:    []string{"pwd="},
		},
		{
			name:           "H3 pass kv",
			input:          "pass=hunter2hunter2",
			mustNotContain: []string{"hunter2hunter2"},
			mustContain:    []string{"pass="},
		},
		{
			name:           "H3 credentials kv",
			input:          "credentials: hunter2hunter2",
			mustNotContain: []string{"hunter2hunter2"},
			mustContain:    []string{"credentials: "},
		},
		{
			name:           "H3 json SecretAccessKey",
			input:          `{"SecretAccessKey":"` + awsSecret + `"}`,
			mustNotContain: []string{"wJalrXUtnFEMI", "bPxRfiCYFIXTUREKEY"},
			mustContain:    []string{`"SecretAccessKey":"`},
		},
		{
			name:           "H3 xml password tag",
			input:          "<config><password>hunter2hunter2</password><name>svc</name></config>",
			mustNotContain: []string{"hunter2hunter2"},
			mustContain:    []string{"<password>", "</password>", "<name>svc</name>"},
		},
		{
			name:           "H3 cli flag space separated",
			input:          "mycli --password hunter2hunter2 --host db.internal",
			mustNotContain: []string{"hunter2hunter2"},
			mustContain:    []string{"--password ", "--host db.internal"},
		},
		{
			name:           "H3 cli flag quoted value",
			input:          `mycli --api-key "my cli secret" --verbose`,
			mustNotContain: []string{"my cli secret"},
			mustContain:    []string{"--api-key ", "--verbose"},
		},
		{
			name:           "H3 curl -u basic auth",
			input:          "curl -u admin:hunter2hunter2 https://api.invalid/v1",
			mustNotContain: []string{"hunter2hunter2", "admin:hunter2hunter2"},
			mustContain:    []string{"curl -u ", "https://api.invalid/v1"},
		},
		{
			name:           "H3 curl --user with intermediate flags",
			input:          "curl -s -X POST --user svc:curlSecret99 https://api.invalid/v1",
			mustNotContain: []string{"curlSecret99"},
			mustContain:    []string{"--user ", "https://api.invalid/v1"},
		},
		// Inline kv variants + H6 regressions + M1 structured values + Low single-quote escape.
		{
			name:           "inline double quoted password with spaces",
			input:          "password: \"my fixture pass\"\n",
			mustNotContain: []string{"my fixture pass"},
			mustContain:    []string{"password:"},
		},
		{
			name:           "inline single quoted api_key with spaces",
			input:          "api_key='my fixture key'\n",
			mustNotContain: []string{"my fixture key"},
			mustContain:    []string{"api_key="},
		},
		{
			name:           "inline token key value",
			input:          "auth_token: fixtureTokVal99\n",
			mustNotContain: []string{"fixtureTokVal99"},
			mustContain:    []string{"auth_token:"},
		},
		{
			name:           "H6 password value on next line",
			input:          "password:\nhunter2hunter2\n",
			mustNotContain: []string{"hunter2hunter2"},
			mustContain:    []string{"password:\n"},
		},
		{
			name:           "H6 unterminated double quote",
			input:          `password="hunter2hunter2`,
			mustNotContain: []string{"hunter2hunter2"},
			mustContain:    []string{`password="`},
		},
		{
			name:           "H6 unterminated single quote",
			input:          "password: 'hunter2hunter2",
			mustNotContain: []string{"hunter2hunter2"},
			mustContain:    []string{"password: '"},
		},
		{
			name:           "M1 json numeric password",
			input:          `{"password": 12345678901}`,
			mustNotContain: []string{"12345678901"},
			mustContain:    []string{`"password": `},
		},
		{
			name:           "M1 json single-line nested secret object",
			input:          `{"secret": {"value": "hunter2hunter2"}}`,
			mustNotContain: []string{"hunter2hunter2"},
			mustContain:    []string{`"secret":`, `"value":`},
		},
		{
			name:           "M1 json multi-line nested secret object",
			input:          "{\n  \"secret\": {\n    \"value\": \"nestedJsonVal99\"\n  }\n}",
			mustNotContain: []string{"nestedJsonVal99"},
			mustContain:    []string{`"secret":`, `"value":`},
		},
		{
			name:           "M1 json secret array",
			input:          `{"token": ["hunter2hunter2"]}`,
			mustNotContain: []string{"hunter2hunter2"},
			mustContain:    []string{`"token":`},
		},
		{
			name:           "M1 python single-quoted dict",
			input:          `{'password': 'hunter2hunter2'}`,
			mustNotContain: []string{"hunter2hunter2"},
			mustContain:    []string{`'password':`},
		},
		{
			name:           "M1 walrus assignment",
			input:          `password := "hunter2hunter2"`,
			mustNotContain: []string{"hunter2hunter2"},
			mustContain:    []string{"password := "},
		},
		{
			name:           "M1 fat-arrow assignment",
			input:          `password => "hunter2hunter2"`,
			mustNotContain: []string{"hunter2hunter2"},
			mustContain:    []string{"password => "},
		},
		{
			name:           "M1 yaml flow mapping",
			input:          `secret: {value: hunter2hunter2}`,
			mustNotContain: []string{"hunter2hunter2"},
			mustContain:    []string{"secret: {value: ", "}"},
		},
		{
			name:           "M1 yaml flow sequence",
			input:          `secret: [abcdefgh, ijklmnop]`,
			mustNotContain: []string{"abcdefgh", "ijklmnop"},
			mustContain:    []string{"secret: [", "]"},
		},
		{
			name:           "M1 unquoted value containing comma early",
			input:          `password=ab,cdefghijk`,
			mustNotContain: []string{"ab,cdefghijk", "cdefghijk"},
			mustContain:    []string{"password="},
		},
		{
			name:           "M1 unquoted value containing comma mid",
			input:          `password=hunter,2secretX`,
			mustNotContain: []string{"hunter", "2secretX"},
			mustContain:    []string{"password="},
		},
		{
			name:           "M1 unquoted value containing semicolon",
			input:          `password=abc;defghijkl`,
			mustNotContain: []string{"abc;defghijkl", "defghijkl"},
			mustContain:    []string{"password="},
		},
		{
			name:           "M1 unquoted value containing apostrophe",
			input:          `password=o'brien12345`,
			mustNotContain: []string{"o'brien12345", "brien12345"},
			mustContain:    []string{"password="},
		},
		{
			name:           "M1 multiline double-quoted password",
			input:          "password=\"Zq9xK2\nmV7pLwXXXXX\"",
			mustNotContain: []string{"Zq9xK2", "mV7pLwXXXXX"},
			mustContain:    []string{`password="`},
		},
		{
			name:           "M1 adversarial unclosed quote followed by second secret",
			input:          "password=\"unclosedSecret\napi_key=\"secondSecretVal\"",
			mustNotContain: []string{"unclosedSecret", "secondSecretVal"},
			mustContain:    []string{`password="`, `api_key="`},
		},
		{
			name:           "Low single-quoted escaped apostrophe",
			input:          `password='Zq9xK2\'mV7pLw'`,
			mustNotContain: []string{"Zq9xK2", "mV7pLw"},
			mustContain:    []string{"password='"},
		},
		// M2, M3, M4: YAML block scalar & shape fixes.
		{
			name:           "M2 block scalar misparse idempotency",
			input:          "secret:\n  secret: |#abcdefg hijklmn\n",
			mustNotContain: []string{"abcdefg", "hijklmn"},
			mustContain:    []string{"secret:"},
		},
		{
			name:           "M2 block scalar hash without space",
			input:          "secret:\n  value: |#hunter2hunter2\n",
			mustNotContain: []string{"hunter2hunter2"},
			mustContain:    []string{"secret:", "value:"},
		},
		{
			name:           "M2 block scalar multi-digit value",
			input:          "secret:\n  value: >12345678\n",
			mustNotContain: []string{"12345678"},
			mustContain:    []string{"secret:", "value:"},
		},
		{
			name:           "M3 block scalar trailing hash line",
			input:          "private_key: |\n  abcdefgh\n  #tailsecretmaterial\n",
			mustNotContain: []string{"abcdefgh", "tailsecretmaterial"},
			mustContain:    []string{"private_key: |"},
		},
		{
			name:           "M3 block scalar only hash line",
			input:          "password: |\n  #hunter2hunter2\n",
			mustNotContain: []string{"hunter2hunter2"},
			mustContain:    []string{"password: |"},
		},
		{
			name:           "M4 yaml same-indent list under password",
			input:          "password:\n- hunter2hunter2\n",
			mustNotContain: []string{"hunter2hunter2"},
			mustContain:    []string{"password:\n- "},
		},
		{
			name:           "M4 yaml plural secrets list",
			input:          "secrets:\n  - hunter2hunter2\n",
			mustNotContain: []string{"hunter2hunter2"},
			mustContain:    []string{"secrets:\n  - "},
		},
		{
			name:           "M4 yaml non-secret parent with pass child",
			input:          "db:\n  host: x\n  pass: hunter2hunter2\n",
			mustNotContain: []string{"hunter2hunter2"},
			mustContain:    []string{"host: x", "pass: "},
		},
		{
			name:           "M4 yaml anchor on header",
			input:          "password: &pw\n  hunter2hunter2\n",
			mustNotContain: []string{"hunter2hunter2"},
			mustContain:    []string{"password: &pw"},
		},
		{
			name:           "M4 yaml explicit tag",
			input:          "password: !!str hunter2hunter2\n",
			mustNotContain: []string{"hunter2hunter2"},
			mustContain:    []string{"password: !!str "},
		},
		{
			name:           "M4 yaml log-prefixed secret header",
			input:          "2026-01-01T00:00:00Z INFO secret:\n  value: hunter2hunter2\n",
			mustNotContain: []string{"hunter2hunter2"},
			mustContain:    []string{"INFO secret:", "value:"},
		},
		{
			name:           "M4 yaml CR-only newlines",
			input:          "secret:\r  value: hunter2hunter2\r",
			mustNotContain: []string{"hunter2hunter2"},
			mustContain:    []string{"secret:", "value:"},
		},
		{
			name:           "M4 yaml unicode line separator",
			input:          "secret:\u2028  value: hunter2hunter2\n",
			mustNotContain: []string{"hunter2hunter2"},
			mustContain:    []string{"secret:", "value:"},
		},
		{
			name:           "M4 yaml NBSP indentation",
			input:          "secret:\n\u00a0\u00a0value: hunter2hunter2\n",
			mustNotContain: []string{"hunter2hunter2"},
			mustContain:    []string{"secret:", "value:"},
		},
		// M7: query parameter stops at &.
		{
			name:           "M7 url query secret stops at ampersand",
			input:          "https://x.invalid/?token=abcdefgh&next=/home&lang=en",
			mustNotContain: []string{"abcdefgh"},
			mustContain:    []string{"?token=[REDACTED:secret]&next=/home&lang=en"},
		},

		// URL with userinfo plus near variants and H5 cases.
		{
			name:           "url postgres user and password",
			input:          "dsn=postgres://app:" + "hunter2pass" + "@example.com:5432/x",
			mustNotContain: []string{"hunter2pass"},
			mustContain:    []string{"postgres://", "@example.com:5432/x"},
			wantCategory:   "url_credential",
			wantMarker:     "[REDACTED:url_credential]",
		},
		{
			name:           "url password only",
			input:          "redis://:" + "onlypass99" + "@cache.internal:6379/0",
			mustNotContain: []string{"onlypass99"},
			mustContain:    []string{"redis://", "@cache.internal:6379/0"},
		},
		{
			name:           "url percent-encoded userinfo",
			input:          "https://user:" + "p%40ss%3Aword" + "@api.invalid/v1/items",
			mustNotContain: []string{"p%40ss%3Aword"},
			mustContain:    []string{"https://", "@api.invalid/v1/items"},
		},
		{
			name:           "url userinfo inside json",
			input:          `{"url":"amqp://svc:` + `brokerPass88` + `@mq.internal:5672/vhost"}`,
			mustNotContain: []string{"brokerPass88"},
			mustContain:    []string{"amqp://", "@mq.internal:5672/vhost"},
		},
		{
			name:           "url userinfo with query and neighbour token",
			input:          "connect postgres://app:" + "urlPass77" + "@example.com/x?sslmode=require token=" + ghp,
			mustNotContain: []string{"urlPass77", "0123456789", "klmnopqrst"},
			mustContain:    []string{"@example.com/x?sslmode=require"},
		},
		{
			name:           "H5 url password containing at sign",
			input:          "postgres://app:p@ssw0rd99@example.com/x",
			mustNotContain: []string{"p@ssw0rd99", "ssw0rd99"},
			mustContain:    []string{"postgres://[REDACTED:url_credential]@example.com/x"},
		},
		{
			name:           "H5 url password containing slash",
			input:          "redis://:pa/ss1234@example.com",
			mustNotContain: []string{"pa/ss1234", "ss1234"},
			mustContain:    []string{"redis://[REDACTED:url_credential]@example.com"},
		},
		{
			name:           "H5 url password containing hash",
			input:          "postgres://app:pa#ss1234@example.com/x",
			mustNotContain: []string{"pa#ss1234", "ss1234"},
			mustContain:    []string{"postgres://[REDACTED:url_credential]@example.com/x"},
		},
		{
			name:           "H5 url password containing question mark",
			input:          "postgres://app:pa?ss1234@example.com/x",
			mustNotContain: []string{"pa?ss1234", "ss1234"},
			mustContain:    []string{"postgres://[REDACTED:url_credential]@example.com/x"},
		},
		{
			name:           "H5 url password containing apostrophe",
			input:          "postgres://app:pa'ss1234@example.com/x",
			mustNotContain: []string{"pa'ss1234", "ss1234"},
			mustContain:    []string{"postgres://[REDACTED:url_credential]@example.com/x"},
		},
		{
			name:           "H5 url with JSON-escaped slashes",
			input:          `{"u":"https:\/\/user:hunter2pw@example.com\/x"}`,
			mustNotContain: []string{"hunter2pw"},
			mustContain:    []string{`https:\/\/[REDACTED:url_credential]@example.com\/x`},
		},

		// Authorization, Proxy-Authorization, and Cookie headers (H4 & H6).
		{
			name:           "authorization basic",
			input:          "Authorization: Basic " + basicB64,
			mustNotContain: []string{basicB64},
			mustContain:    []string{"Authorization: Basic "},
			wantCategory:   "basic_auth_header",
			wantMarker:     "[REDACTED:basic_auth]",
		},
		{
			name:           "authorization basic padded lowercase tab CRLF",
			input:          "authorization:\tbasic\t" + basicPadded + "\r\n",
			mustNotContain: []string{basicPadded},
			mustContain:    []string{"authorization:\tbasic\t"},
		},
		{
			name:           "proxy-authorization basic",
			input:          "Proxy-Authorization: Basic " + basicB64,
			mustNotContain: []string{basicB64},
			mustContain:    []string{"Proxy-Authorization: Basic "},
		},
		{
			name:           "proxy-authorization bearer",
			input:          "Proxy-Authorization: Bearer " + bearerVal,
			mustNotContain: []string{bearerVal},
			mustContain:    []string{"Proxy-Authorization: Bearer "},
			wantCategory:   "bearer_header",
			wantMarker:     "[REDACTED:bearer_token]",
		},
		{
			name:           "authorization bearer mixed case and tab",
			input:          "AUTHORIZATION:\tBearer\t" + bearerVal + "\r\n",
			mustNotContain: []string{bearerVal},
			mustContain:    []string{"AUTHORIZATION:\tBearer\t"},
		},
		{
			name:           "authorization basic in json header map",
			input:          `{"Authorization": "Basic ` + basicB64 + `"}`,
			mustNotContain: []string{basicB64},
			mustContain:    []string{`"Authorization": "Basic `},
		},
		{
			name:           "H4 go map Authorization Bearer",
			input:          "map[Authorization:[Bearer abcdefghijkl]]",
			mustNotContain: []string{"abcdefghijkl"},
			mustContain:    []string{"map[Authorization:[Bearer [REDACTED:bearer_token]]]"},
		},
		{
			name:           "H4 go map Authorization Basic",
			input:          "map[Authorization:[Basic " + basicUserPass + "]]",
			mustNotContain: []string{basicUserPass},
			mustContain:    []string{"map[Authorization:[Basic [REDACTED:basic_auth]]]"},
		},
		{
			name:           "H4 json http.Header array Bearer",
			input:          `{"Authorization":["Bearer abcdefghijkl"]}`,
			mustNotContain: []string{"abcdefghijkl"},
			mustContain:    []string{`{"Authorization":["Bearer [REDACTED:bearer_token]"]}`},
		},
		{
			name:           "H4 Authorization Token scheme",
			input:          "Authorization: Token abcdefghijkl",
			mustNotContain: []string{"abcdefghijkl"},
			mustContain:    []string{"Authorization: Token "},
		},
		{
			name:           "H4 Authorization ApiKey scheme",
			input:          "Authorization: ApiKey abcdefghijkl",
			mustNotContain: []string{"abcdefghijkl"},
			mustContain:    []string{"Authorization: ApiKey "},
		},
		{
			name:           "H4 Authorization Digest scheme",
			input:          `Authorization: Digest username="u", response="0123456789abcdef"`,
			mustNotContain: []string{"0123456789abcdef"},
			mustContain:    []string{"Authorization: Digest "},
		},
		{
			name:           "H4 Cookie header session",
			input:          "Cookie: session=abcdef0123456789",
			mustNotContain: []string{"abcdef0123456789"},
			mustContain:    []string{"Cookie: [REDACTED:cookie]"},
			wantCategory:   "cookie_header",
			wantMarker:     "[REDACTED:cookie]",
		},
		{
			name:           "H4 Cookie header preference accepted redaction",
			input:          "Cookie: theme=dark",
			mustNotContain: []string{"theme=dark"},
			mustContain:    []string{"Cookie: [REDACTED:cookie]"},
		},
		{
			name:           "H4 Set-Cookie header preserves attributes",
			input:          "Set-Cookie: sid=abcdef0123456789; Path=/; HttpOnly",
			mustNotContain: []string{"abcdef0123456789"},
			mustContain:    []string{"Set-Cookie: [REDACTED:cookie]; Path=/; HttpOnly"},
		},
		{
			name:           "H4 Authorization Bearer NBSP separator",
			input:          "Authorization: Bearer\u00a0abcdefghijkl",
			mustNotContain: []string{"abcdefghijkl"},
			mustContain:    []string{"Authorization: Bearer\u00a0[REDACTED:bearer_token]"},
		},
		{
			name:           "H4 Authorization Bearer with punctuation",
			input:          "Authorization: Bearer abcdefghij!klmnop",
			mustNotContain: []string{"abcdefghij", "klmnop"},
			mustContain:    []string{"Authorization: Bearer [REDACTED:bearer_token]"},
		},
		{
			name:           "H6 Authorization folded before scheme",
			input:          "Authorization:\r\n Bearer abcdefghijkl",
			mustNotContain: []string{"abcdefghijkl"},
			mustContain:    []string{"Authorization:\r\n Bearer [REDACTED:bearer_token]"},
		},
		{
			name:           "H6 Authorization folded after scheme",
			input:          "Authorization: Bearer\r\n abcdefghijkl",
			mustNotContain: []string{"abcdefghijkl"},
			mustContain:    []string{"Authorization: Bearer\r\n [REDACTED:bearer_token]"},
		},

		// JWT and JWE tokens (plus M5).
		{
			name:           "jwt without padding",
			input:          "session=" + jwtNoPad + " ok",
			mustNotContain: []string{"hbGciOiJIUzI1Ni", "zdWIiOiJmaXh0dXJl", "c2lnbmF0dXJlZml4dHVyZTEyMw"},
			mustContain:    []string{"session=", " ok"},
			wantCategory:   "jwt",
			wantMarker:     "[REDACTED:jwt]",
		},
		{
			name:           "jwt with padding",
			input:          "cookie: " + jwtPadded + "\r\n",
			mustNotContain: []string{"hbGciOiJIUzI1Ni", "zdWIiOiJmaXh0dXJl", "c2lnbmF0dXJlZml4dHVyZQ=="},
			mustContain:    []string{"cookie: [REDACTED:jwt]\r\n"},
			wantCategory:   "jwt",
		},
		{
			name:           "jwt inside json",
			input:          `{"id":"` + jwtNoPad + `","status":"ok"}`,
			mustNotContain: []string{"hbGciOiJIUzI1Ni", "c2lnbmF0dXJlZml4dHVyZTEyMw"},
			mustContain:    []string{`"status":"ok"`},
		},
		{
			name:           "M5 unsigned jwt empty signature",
			input:          "tok=" + jwtUnsignedEmptySig,
			mustNotContain: []string{"hbGciOiJub25l", "zdWIiOiJ4eHh4"},
			wantCategory:   "jwt",
			wantMarker:     "[REDACTED:jwt]",
		},
		{
			name:           "M5 two-segment jwt",
			input:          "tok=" + jwtTwoSeg,
			mustNotContain: []string{"hbGciOiJub25l", "zdWIiOiJ4eHh4"},
			wantCategory:   "jwt",
		},
		{
			name:           "M5 five-segment jwe",
			input:          "enc=" + jweFiveSeg,
			mustNotContain: []string{"hbGciOiJkaXI", "ENCKEYENCKEY", "CIPHERTEXTCIPHER", "TAGTAGTAGTAG"},
			wantCategory:   "jwt",
		},
		{
			name:           "M5 jwt standard base64 signature",
			input:          "tok=" + jwtStdB64Sig,
			mustNotContain: []string{"hbGciOiJIUzI1Ni", "ghijklmnop", "qrstu"},
			wantCategory:   "jwt",
		},
		{
			name:           "M5 jwt line-wrapped signature",
			input:          jwtWrappedSig,
			mustNotContain: []string{"c2lnbmF0", "dXJlMTIz"},
			wantCategory:   "jwt",
		},
		{
			name:           "M5 jwt dot at start of line",
			input:          jwtWrappedDots,
			mustNotContain: []string{"hbGciOiJIUzI1Ni", "c2lnbmF0dXJlMTIz"},
			wantCategory:   "jwt",
		},
		{
			name:           "M5 jwt spaces around dots",
			input:          jwtSpacedDots,
			mustNotContain: []string{"hbGciOiJIUzI1Ni", "c2lnbmF0dXJlMTIz"},
			wantCategory:   "jwt",
		},
		{
			name:           "M5 jwt preceded by word char",
			input:          "jwt_" + jwtNoPad + " 1" + jwtNoPad,
			mustNotContain: []string{"hbGciOiJIUzI1Ni", "c2lnbmF0dXJlZml4dHVyZTEyMw"},
			wantCategory:   "jwt",
		},

		// PEM private key blocks: RSA, EC, OPENSSH, PKCS8, complete, truncated, and M7 grep line.
		{
			name:           "pem rsa complete",
			input:          "before\n" + pemBlock("RSA PRIVATE KEY", pemBody, "\n", true) + "\nafter",
			mustNotContain: []string{"fixtureBodyLine1", "fixtureBodyLine2"},
			mustContain:    []string{"before\n", "\nafter"},
			wantCategory:   "private_key",
			wantMarker:     "[REDACTED:private_key]",
		},
		{
			name:           "pem ec complete",
			input:          "before\n" + pemBlock("EC PRIVATE KEY", pemBody, "\n", true) + "\nafter",
			mustNotContain: []string{"fixtureBodyLine1"},
			mustContain:    []string{"before\n", "\nafter"},
		},
		{
			name:           "pem openssh complete",
			input:          "before\n" + pemBlock("OPENSSH PRIVATE KEY", pemBody, "\n", true) + "\nafter",
			mustNotContain: []string{"fixtureBodyLine1"},
			mustContain:    []string{"before\n", "\nafter"},
		},
		{
			name:           "pem pkcs8 complete with CRLF",
			input:          "before\r\n" + pemBlock("PRIVATE KEY", pemBodyCRLF, "\r\n", true) + "\r\nafter",
			mustNotContain: []string{"fixtureBodyLine1"},
			mustContain:    []string{"before\r\n", "\r\nafter"},
		},
		{
			name:           "pem rsa truncated no end line",
			input:          "before\n" + pemBlock("RSA PRIVATE KEY", pemBody, "\n", false),
			mustNotContain: []string{"fixtureBodyLine1", "fixtureBodyLine2"},
			mustContain:    []string{"before\n"},
		},
		{
			name:           "pem ec truncated no end line",
			input:          "before\n" + pemBlock("EC PRIVATE KEY", pemBody, "\n", false),
			mustNotContain: []string{"fixtureBodyLine1"},
			mustContain:    []string{"before\n"},
		},
		{
			name:           "pem openssh truncated no end line",
			input:          "before\n" + pemBlock("OPENSSH PRIVATE KEY", pemBody, "\n", false),
			mustNotContain: []string{"fixtureBodyLine1"},
			mustContain:    []string{"before\n"},
		},
		{
			name:           "pem pkcs8 truncated no end line",
			input:          "before\n" + pemBlock("PRIVATE KEY", pemBody, "\n", false),
			mustNotContain: []string{"fixtureBodyLine1"},
			mustContain:    []string{"before\n"},
		},
		{
			name: "pem mismatched end line",
			input: joinParts(
				"before\n",
				"-----BEGIN ", "RSA PRIVATE KEY-----\n",
				pemBody, "\n",
				"-----END ", "EC PRIVATE KEY-----\n",
				"trailingSecretBody",
			),
			mustNotContain: []string{"fixtureBodyLine1", "trailingSecretBody"},
			mustContain:    []string{"before\n"},
		},
		{
			name:           "M7 truncated pem header in grep command preserves following lines",
			input:          "grep '" + joinParts("-----BEGIN ", "PRIVATE KEY-----") + "' *.pem\nfile1.pem\n",
			mustNotContain: []string{"BEGIN PRIVATE KEY"},
			mustContain:    []string{"' *.pem\nfile1.pem\n"},
		},

		// Known provider token prefixes, M6 splits/boundaries, and Low providers.
		{
			name:           "provider ghp token",
			input:          "val=" + ghp,
			mustNotContain: []string{"ghp_", "0123456789", "abcdefghij", "klmnopqrst"},
			wantCategory:   "github_token",
			wantMarker:     "[REDACTED:github_token]",
		},
		{
			name:           "provider ghp split by invalid utf8 byte",
			input:          "val=" + ghpSplit,
			mustNotContain: []string{"0123456789", "abcdefghij", "klmnopqrst"},
		},
		{
			name:           "provider ghp split inside prefix by invalid utf8 byte",
			input:          "val=" + ghpSplitPrefix,
			mustNotContain: []string{"0123456789", "abcdefghij", "klmnopqrst"},
		},
		{
			name:           "M6 provider ghp split by ANSI CSI",
			input:          "val=" + ghpSplitANSI,
			mustNotContain: []string{"0123456789", "abcdefghij", "klmnopqrst"},
			wantCategory:   "github_token",
		},
		{
			name:           "M6 provider ghp split by soft hyphen",
			input:          "val=" + ghpSplitSoftHyphen,
			mustNotContain: []string{"0123456789", "abcdefghij", "klmnopqrst"},
		},
		{
			name:           "M6 provider ghp split by ZWSP",
			input:          "val=" + ghpSplitZWSP,
			mustNotContain: []string{"0123456789", "abcdefghij", "klmnopqrst"},
		},
		{
			name:           "M6 provider ghp split by ZWSP after 20 chars",
			input:          "val=" + ghpSplitZWSPTail,
			mustNotContain: []string{"0123456789", "klmnopqrst"},
		},
		{
			name:           "M6 provider ghp split by NUL",
			input:          "val=" + ghpSplitNUL,
			mustNotContain: []string{"0123456789", "abcdefghij", "klmnopqrst"},
		},
		{
			name:           "M6 provider ghp split by literal U+FFFD",
			input:          "val=" + ghpSplitFFFD,
			mustNotContain: []string{"0123456789", "abcdefghij", "klmnopqrst"},
		},
		{
			name:           "M6 provider ghp split by CRLF",
			input:          "val=" + ghpSplitCRLF,
			mustNotContain: []string{"0123456789", "abcdefghij", "klmnopqrst"},
		},
		{
			name:           "M6 provider ghp glued to prefix and suffix",
			input:          "MY_" + ghp + "_old token" + ghp,
			mustNotContain: []string{"0123456789", "abcdefghij", "klmnopqrst"},
			mustContain:    []string{"MY_", "_old token"},
		},
		{
			name:           "provider gho token",
			input:          "val=" + gho,
			mustNotContain: []string{"0123456789", "klmnopqrst"},
		},
		{
			name:           "provider ghu token",
			input:          "val=" + ghu,
			mustNotContain: []string{"0123456789", "klmnopqrst"},
		},
		{
			name:           "provider ghs token",
			input:          "val=" + ghs,
			mustNotContain: []string{"0123456789", "klmnopqrst"},
		},
		{
			name:           "provider ghr token",
			input:          "val=" + ghr,
			mustNotContain: []string{"0123456789", "klmnopqrst"},
		},
		{
			name:           "provider github_pat token",
			input:          "val=" + ghPat,
			mustNotContain: []string{"0123456789", "klmnop"},
		},
		{
			name:           "provider sc_task token",
			input:          "val=" + scTask,
			mustNotContain: []string{"sc_task_", "0123456789abcdef", "fedcba9876543210"},
			wantCategory:   "sablecourt_token",
			wantMarker:     "[REDACTED:sablecourt_token]",
		},
		{
			name:           "provider op token",
			input:          "val=" + opTok,
			mustNotContain: []string{"0123456789abcdef"},
			wantCategory:   "sablecourt_token",
		},
		{
			name:           "provider op token split by invalid utf8 byte",
			input:          "val=" + opTokSplit,
			mustNotContain: []string{"0123456789abcdef"},
		},
		{
			name:           "provider anthropic key",
			input:          "val=" + antKey,
			mustNotContain: []string{"sk-ant-", "fixture0123456789", "abcdef"},
			wantCategory:   "anthropic_key",
			wantMarker:     "[REDACTED:anthropic_key]",
		},
		{
			name:           "provider openai project key",
			input:          "val=" + oaiKey,
			mustNotContain: []string{"fixture0123456789", "abcdef"},
			wantCategory:   "openai_key",
			wantMarker:     "[REDACTED:openai_key]",
		},
		{
			name:           "provider openai legacy key",
			input:          "val=" + oaiLegacy,
			mustNotContain: []string{"fixture0123456789", "abcdef"},
			wantCategory:   "openai_key",
		},
		{
			name:           "provider google api key",
			input:          "val=" + googKey,
			mustNotContain: []string{"SyFixture0123456789", "abcd"},
			wantCategory:   "google_api_key",
		},
		{
			name:           "provider aws AKIA key",
			input:          "val=" + awsAKIA,
			mustNotContain: []string{"AKIA", "FIXTURE012345678"},
			wantCategory:   "aws_access_key",
			wantMarker:     "[REDACTED:aws_access_key]",
		},
		{
			name:           "provider aws ASIA key",
			input:          "val=" + awsASIA,
			mustNotContain: []string{"ASIA", "FIXTURE012345678"},
			wantCategory:   "aws_access_key",
		},
		{
			name:           "Low provider aws AKIA with trailing underscore",
			input:          awsAKIA + "_ID",
			mustNotContain: []string{"FIXTURE012345678"},
			mustContain:    []string{"_ID"},
			wantCategory:   "aws_access_key",
		},
		{
			name:           "provider aws key split by invalid utf8 byte",
			input:          "val=" + awsSplit,
			mustNotContain: []string{"FIXTURE", "012345678"},
		},
		{
			name:           "Low provider stripe live key",
			input:          "stripe=" + stripeLive,
			mustNotContain: []string{"0123456789abcdefghij", "klmnop"},
			wantCategory:   "stripe_key",
			wantMarker:     "[REDACTED:stripe_key]",
		},
		{
			name:           "Low provider slack bot token",
			input:          "slack=" + slackBot,
			mustNotContain: []string{"0123456789", "abcdefghijkl"},
			wantCategory:   "slack_token",
			wantMarker:     "[REDACTED:slack_token]",
		},
		{
			name:           "provider ghp long prefix split before tail by invalid utf8 byte",
			input:          "val=" + joinParts("ghp_", "0123456789abcdefghij", "\xff", "tailSecret99"),
			mustNotContain: []string{"0123456789", "tailSecret99"},
		},
		{
			name:           "authorization basic split by invalid utf8 byte",
			input:          "Authorization: Basic " + joinParts("dXNl", "\xff", "cjpmaXh0dXJl"),
			mustNotContain: []string{"dXNl", "cjpmaXh0dXJl"},
			mustContain:    []string{"Authorization: Basic "},
		},

		// Adversarial neighbours: extending one match must not hide or skip an adjacent secret.
		{
			name:           "adjacent quoted secrets on one line",
			input:          `password="first secret val" api_key="second secret val" status=ok`,
			mustNotContain: []string{"first secret val", "second secret val"},
			mustContain:    []string{"status=ok"},
		},
		{
			name: "two complete pem blocks preserve text between",
			input: pemBlock("RSA PRIVATE KEY", pemBody, "\n", true) +
				"\nkeep-middle\n" +
				pemBlock("EC PRIVATE KEY", "ecFixtureBody", "\n", true) +
				"\nkeep-end",
			mustNotContain: []string{"fixtureBodyLine1", "ecFixtureBody"},
			mustContain:    []string{"\nkeep-middle\n", "\nkeep-end"},
		},
		{
			name: "complete pem then truncated pem preserve text between",
			input: pemBlock("PRIVATE KEY", pemBody, "\n", true) +
				"\nkeep-between\n" +
				pemBlock("OPENSSH PRIVATE KEY", "unclosedTailSecret", "\n", false),
			mustNotContain: []string{"fixtureBodyLine1", "unclosedTailSecret"},
			mustContain:    []string{"\nkeep-between\n"},
		},
		{
			name:           "nested yaml secret then safe sibling then second secret",
			input:          "secret:\n  value: firstYamlSec\nstatus: healthy\ntoken:\n  value: secondYamlSec\n",
			mustNotContain: []string{"firstYamlSec", "secondYamlSec"},
			mustContain:    []string{"status: healthy"},
		},

		// Review 2 regression cases: N2, N3, N4, N5, and N6 (curl Cookie).
		{
			name:           "N2 url password containing comma",
			input:          "postgres://app:hunter2,pw@example.com/x",
			mustNotContain: []string{"hunter2", "pw@db"},
			mustContain:    []string{"postgres://[REDACTED:url_credential]@example.com/x"},
			wantCategory:   "url_credential",
		},
		{
			name:           "N2 url password containing semicolon",
			input:          "postgres://app:hunter2;pw@example.com/x",
			mustNotContain: []string{"hunter2", "pw@db"},
			mustContain:    []string{"postgres://[REDACTED:url_credential]@example.com/x"},
		},
		{
			name:           "N2 url password containing closing paren",
			input:          "postgres://app:hunter2)pw@example.com/x",
			mustNotContain: []string{"hunter2", "pw@db"},
			mustContain:    []string{"postgres://[REDACTED:url_credential]@example.com/x"},
		},
		{
			name:           "N2 url password containing closing bracket",
			input:          "postgres://app:hunter2]pw@example.com/x",
			mustNotContain: []string{"hunter2", "pw@db"},
			mustContain:    []string{"postgres://[REDACTED:url_credential]@example.com/x"},
		},
		{
			name:           "N2 url password containing closing brace",
			input:          "postgres://app:hunter2}pw@example.com/x",
			mustNotContain: []string{"hunter2", "pw@db"},
			mustContain:    []string{"postgres://[REDACTED:url_credential]@example.com/x"},
		},
		{
			name:           "N3 go map Set-Cookie multiple values",
			input:          "map[Set-Cookie:[sid=abc123; Path=/ refresh=xyz789; Secure]]",
			mustNotContain: []string{"abc123", "xyz789"},
			mustContain:    []string{"map[Set-Cookie:[", "]]"},
			wantCategory:   "cookie_header",
		},
		{
			name:           "N3 json array Set-Cookie multiple values",
			input:          `{"Set-Cookie":["a=b; Path=/","refresh=xyz789"]}`,
			mustNotContain: []string{"a=b", "xyz789"},
			mustContain:    []string{`"Set-Cookie":[`, `]`},
			wantCategory:   "cookie_header",
		},
		{
			name:           "N4 unquoted value containing open paren",
			input:          "DB_PASSWORD=Xy9(kLm2nPq",
			mustNotContain: []string{"Xy9", "kLm2nPq"},
			mustContain:    []string{"DB_PASSWORD=[REDACTED:secret]"},
		},
		{
			name:           "N4 unquoted value containing close paren mid-token",
			input:          "password=Zq9xK2)mV7pLw",
			mustNotContain: []string{"Zq9xK2", "mV7pLw"},
			mustContain:    []string{"password=[REDACTED:secret]"},
		},
		{
			name:           "N4 unquoted value containing close brace mid-token",
			input:          "password=Zq9xK2}mV7pLw",
			mustNotContain: []string{"Zq9xK2", "mV7pLw"},
			mustContain:    []string{"password=[REDACTED:secret]"},
		},
		{
			name:           "N4 unquoted value containing close bracket mid-token",
			input:          "password=Zq9xK2]mV7pLw",
			mustNotContain: []string{"Zq9xK2", "mV7pLw"},
			mustContain:    []string{"password=[REDACTED:secret]"},
		},
		{
			name:           "N5 truncated pem body on header line with space",
			input:          joinParts("-----BEGIN ", "PRIVATE KEY----- ") + "MIIEvQIBADANBgkqhkiG9w0BAQEFAASCBKcw",
			mustNotContain: []string{"MIIEvQIBADANBgkqhkiG9w0BAQEFAASCBKcw"},
			wantCategory:   "private_key",
		},
		{
			name:           "N5 truncated pem body glued to header line",
			input:          joinParts("-----BEGIN ", "PRIVATE KEY-----") + "MIIEvQIBADANBgkqhkiG9w0BAQEFAASCBKcw",
			mustNotContain: []string{"MIIEvQIBADANBgkqhkiG9w0BAQEFAASCBKcw"},
			wantCategory:   "private_key",
		},
		{
			name:           "N5 truncated pem double-escaped newline body",
			input:          `{"msg":"{\"k\":\"` + joinParts("-----BEGIN ", "PRIVATE KEY-----") + `\\nMIIEvQIBADANBgkqhkiG9w0BAQE\"}"}`,
			mustNotContain: []string{"MIIEvQIBADANBgkqhkiG9w0BAQE"},
			mustContain:    []string{`\"}"}`},
			wantCategory:   "private_key",
		},
		{
			name:           "N6 curl single-quoted Cookie preserves closing quote and url",
			input:          "curl -H 'Cookie: a=b' https://x.invalid",
			mustNotContain: []string{"a=b"},
			mustContain:    []string{"curl -H 'Cookie: [REDACTED:cookie]' https://x.invalid"},
			wantCategory:   "cookie_header",
		},
		{
			name:           "N6 curl double-quoted Cookie preserves closing quote and url",
			input:          `curl -H "Cookie: a=b" https://x.invalid`,
			mustNotContain: []string{"a=b"},
			mustContain:    []string{`curl -H "Cookie: [REDACTED:cookie]" https://x.invalid`},
			wantCategory:   "cookie_header",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := Text(tc.input)
			got := res.Text
			if res.Hits == 0 {
				t.Errorf("expected Hits > 0 for %q, got 0", tc.input)
			}
			for _, bad := range tc.mustNotContain {
				if strings.Contains(got, bad) {
					t.Errorf("leaked %q in redacted output %q", bad, got)
				}
			}
			for _, keep := range tc.mustContain {
				if !strings.Contains(got, keep) {
					t.Errorf("expected %q to survive in redacted output %q", keep, got)
				}
			}
			if tc.wantMarker != "" && !strings.Contains(got, tc.wantMarker) {
				t.Errorf("expected marker %q in redacted output %q", tc.wantMarker, got)
			}
			if tc.wantCategory != "" {
				found := false
				for _, c := range res.Categories {
					if c == tc.wantCategory {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("expected category %q in %v", tc.wantCategory, res.Categories)
				}
			}
			if twice := String(got); twice != got {
				t.Errorf("String not idempotent:\nonce:  %q\ntwice: %q", got, twice)
			}
		})
	}
}

func TestCorpusMustKeep(t *testing.T) {
	cases := []struct {
		name  string
		input string
	}{
		{
			name:  "url without userinfo https",
			input: "fetch https://docs.invalid/guide/auth?tab=1#top",
		},
		{
			name:  "url without userinfo postgres",
			input: "connect postgres://db.internal:5432/app?sslmode=require",
		},
		{
			name:  "url with at sign in path not userinfo",
			input: "GET https://registry.invalid:8443/@scope/pkg-1.2.3",
		},
		{
			name:  "M7 ssh url with plain username git",
			input: "ssh://git@example.com/org/repo.git",
		},
		{
			name:  "M7 url followed by comma and email",
			input: "see https://a.invalid,foo@example.com",
		},
		{
			name:  "word secret in prose",
			input: "This secret stays in the vault and never enters logs.",
		},
		{
			name:  "word password and token in prose",
			input: "Rotate the password and refresh the token before Friday.",
		},
		{
			name:  "H3 new key synonyms in prose",
			input: "All tests pass; run pwd, check credentials, access_key, private_key, secret_key and signing_key docs.",
		},
		{
			name:  "H3 non-secret suffix near pass",
			input: "bypass: enabled compass: magnetic pass_count=123456",
		},
		{
			name:  "H3 xml non-secret tag mentioning password",
			input: "<description>rotate password soon</description><count>123456</count>",
		},
		{
			name:  "H3 cli flag followed by another flag",
			input: "mycli --password --help",
		},
		{
			name:  "H3 docker -u uid:gid not curl",
			input: "docker run -u 1000:1000 alpine",
		},
		{
			name:  "M7 H4 Authorization Basic prose",
			input: "Authorization: Basic auth is not supported",
		},
		{
			name:  "H4 Authorization prose word after colon",
			input: "Authorization: required for admin endpoints",
		},
		{
			name:  "H4 Cookie prose without key=value",
			input: "Cookie: disabled in private browsing",
		},
		{
			name:  "M7 invalid token error message",
			input: "invalid token: signature expired",
		},
		{
			name:  "M7 typescript interface with token and apiKey types",
			input: "interface Cfg { token: string; apiKey: string }",
		},
		{
			name:  "M7 code calling os.Getenv",
			input: `token = os.Getenv("TOKEN")`,
		},
		{
			name:  "M7 k8s secret volume metadata",
			input: "secret:\n  secretName: db-creds\n  defaultMode: 0400\n",
		},
		{
			name:  "M7 github actions token input metadata",
			input: "inputs:\n  token:\n    description: 'GitHub token'\n    required: true\n",
		},
		{
			name:  "M7 openapi password schema",
			input: "password:\n  type: string\n  minLength: 8\n",
		},
		{
			name:  "M7 pre-redacted plain REDACTED marker",
			input: "Token: [REDACTED]",
		},
		{
			name:  "yaml value under non-secret parent",
			input: "timeout:\n  value: 30s\n",
		},
		{
			name:  "yaml value two levels under non-secret parent",
			input: "config:\n  retries:\n    value: 5\n  mode: strict\n",
		},
		{
			name:  "base64 image data uri",
			input: "img=data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg==",
		},
		{
			name:  "git commit sha",
			input: "commit a1b2c3d4e5f60718293a4b5c6d7e8f9012345678 merged",
		},
		{
			name:  "uuid v4",
			input: "request_id=550e8400-e29b-41d4-a716-446655440000",
		},
		{
			name:  "dotted identifiers not jwt",
			input: "type pkg.Service.HandleRequest version v1.2.3",
		},
		{
			name:  "eyJ hostname not jwt",
			input: "connect eyJ1234567.service.internal:443",
		},
		{
			name:  "word ending in sk- not openai key",
			input: "id=task-0123456789abcdefghij",
		},
		{
			name:  "pem public key untouched",
			input: pemBlock("PUBLIC KEY", "MFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAEfixture", "\n", true),
		},
		{
			name:  "pem certificate untouched",
			input: pemBlock("CERTIFICATE", "MIICfixtureCertBody", "\n", true),
		},
		{
			name:  "count fields untouched",
			input: `{"api_key_count":123456,"max_tokens":4096,"secret_version":2}`,
		},
		{
			name:  "clean text with invalid utf8 byte",
			input: "status=ok\xffcount=42",
		},
		{
			name:  "pre-redacted url userinfo marker stays intact",
			input: "https://[REDACTED:userinfo]@example.com/x",
		},
		{
			name:  "pre-redacted url_credential marker stays intact",
			input: "https://[REDACTED:url_credential]@example.com/x",
		},
		// Review 2 N6 over-redaction must-keep rows.
		{
			name:  "N6 prose credentials loaded from file",
			input: "credentials: loaded from file",
		},
		{
			name:  "N6 prose password changed",
			input: "password: changed",
		},
		{
			name:  "N6 prose token refreshed",
			input: "token: refreshed",
		},
		{
			name:  "N6 prose pass passed",
			input: "pass: passed",
		},
		{
			name:  "N6 env PWD filesystem path",
			input: "PWD=/opt/runner/work/repo",
		},
		{
			name:  "N6 ipv6 url with email in path",
			input: "https://[2001:db8::1]/u/bob@example.com",
		},
		{
			name:  "N6 k8s imagePullSecrets list item name",
			input: "imagePullSecrets:\n- name: regcred\n",
		},
		{
			name:  "N6 credentials flow mapping metadata",
			input: "credentials: {region: eu, profile: x}",
		},
		{
			name:  "N6 Authorization skipping verification prose",
			input: "Authorization: skipping verification",
		},
		{
			name:  "N6 Authorization Bearer authentication failed prose",
			input: "Authorization: Bearer authentication failed",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := Text(tc.input)
			if res.Text != tc.input || res.Hits != 0 {
				t.Errorf("Text(%q) altered clean input: got %q (hits=%d cats=%v)", tc.input, res.Text, res.Hits, res.Categories)
			}
		})
	}
}

func TestCorpusKnownGaps(t *testing.T) {
	// Pin each documented known gap so that fixing one turns this test red
	// and prompts moving the row into TestCorpusMustHide.
	gaps := []struct {
		name         string
		input        string
		leakedSubstr string
		reason       string
	}{
		{
			name:         "unlabelled opaque hex secret",
			input:        "deploy " + joinParts("9f8e7d6c5b4a39281716", "05f4e3d2c1b0a9876543"),
			leakedSubstr: "9f8e7d6c5b4a39281716",
			reason:       "unlabelled high-entropy strings are indistinguishable from git SHAs, UUIDs, and base64 image data without unacceptable over-redaction",
		},
		{
			name:         "provider token split by ascii whitespace",
			input:        "val=" + joinParts("ghp_", "0123456789", " ", "abcdefghij", "klmnopqrst"),
			leakedSubstr: "abcdefghij",
			reason:       "matching across spaces after a short token prefix would redact ordinary words that follow it",
		},
		{
			name:         "url password containing both at sign and slash",
			input:        "postgres://app:p@ss/word@example.com",
			leakedSubstr: "ss/word",
			reason:       "when @ appears both before and after /, the first @host segment is indistinguishable from a real host followed by an email in the path",
		},
	}

	for _, tc := range gaps {
		t.Run(tc.name, func(t *testing.T) {
			got := String(tc.input)
			if !strings.Contains(got, tc.leakedSubstr) {
				t.Fatalf("known gap %q no longer leaks %q (got %q); move it to TestCorpusMustHide and update docs", tc.name, tc.leakedSubstr, got)
			}
		})
	}
}

func TestCorpusGrammarMustHide(t *testing.T) {
	assertHidden := func(t *testing.T, input string, needles []string, keep []string) {
		t.Helper()
		res := Text(input)
		if res.Hits == 0 {
			t.Fatalf("expected Hits > 0 for %q", input)
		}
		for _, bad := range needles {
			if strings.Contains(res.Text, bad) {
				t.Fatalf("leaked %q in output %q (input %q)", bad, res.Text, input)
			}
		}
		for _, k := range keep {
			if !strings.Contains(res.Text, k) {
				t.Fatalf("expected %q to survive in output %q (input %q)", k, res.Text, input)
			}
		}
		if twice := String(res.Text); twice != res.Text {
			t.Fatalf("not idempotent:\ninput: %q\nonce:  %q\ntwice: %q", input, res.Text, twice)
		}
	}

	t.Run("kv grammar", func(t *testing.T) {
		keys := []string{
			"password", "passwd", "passphrase", "pwd", "pass",
			"secret", "secret_key", "SECRET_KEY", "secretKey",
			"api_key", "api-key", "private_key", "access_key",
			"signing_key", "credentials", "token", "auth_token",
		}
		keyQuotes := []string{"", `"`, `'`}
		seps := []string{"=", ": ", " := ", " => ", ": !!str "}
		valShapes := []struct {
			quote string
			val   string
			need  []string
		}{
			{"", "Zx9kLm2nPq", []string{"Zx9kLm2nPq"}},
			{"", "Xy9(kLm2nPq", []string{"Xy9", "kLm2nPq"}},
			{"", "Zq9xK2)mV7pLw", []string{"Zq9xK2", "mV7pLw"}},
			{"", "Zq9xK2}mV7pLw", []string{"Zq9xK2", "mV7pLw"}},
			{"", "Zq9xK2]mV7pLw", []string{"Zq9xK2", "mV7pLw"}},
			{"", "ab,cdefgh99", []string{"ab,cdefgh99", "cdefgh99"}},
			{"", "ab;cdefgh99", []string{"ab;cdefgh99", "cdefgh99"}},
			{`"`, "secret with spaces 99", []string{"secret with spaces 99"}},
			{`'`, `o\'brienSecret99`, []string{"brienSecret99"}},
		}

		for _, k := range keys {
			for _, kq := range keyQuotes {
				for _, sep := range seps {
					for _, vs := range valShapes {
						input := kq + k + kq + sep + vs.quote + vs.val + vs.quote
						assertHidden(t, input, vs.need, []string{k})
					}
				}
			}
		}
	})

	t.Run("auth header grammar", func(t *testing.T) {
		headers := []string{"Authorization", "Proxy-Authorization", "authorization"}
		schemes := []struct {
			name string
			val  string
			need string
		}{
			{"Bearer", "tokFixture01234567", "tokFixture01234567"},
			{"Basic", joinParts("dXNlcjp", "wYXNzd29yZA=="), "wYXNzd29yZA=="},
			{"Token", "tokFixture01234567", "tokFixture01234567"},
			{"ApiKey", "keyFixture01234567", "keyFixture01234567"},
			{"Digest", `username="u", response="0123456789abcdef"`, "0123456789abcdef"},
		}
		wrappers := []struct {
			wrap func(h, s, v string) string
			keep []string
		}{
			{func(h, s, v string) string { return h + ": " + s + " " + v }, nil},
			{func(h, s, v string) string { return `{"` + h + `":"` + s + " " + v + `"}` }, []string{`"}`}},
			{func(h, s, v string) string { return `{"` + h + `":["` + s + " " + v + `"]}` }, []string{`"]}`}},
			{func(h, s, v string) string { return "map[" + h + ":[" + s + " " + v + "]]" }, []string{"]]"}},
			{func(h, s, v string) string { return "curl -H '" + h + ": " + s + " " + v + "' https://x.invalid" }, []string{"' https://x.invalid"}},
		}

		for _, h := range headers {
			for _, sc := range schemes {
				for _, w := range wrappers {
					if sc.name == "Digest" && strings.Contains(w.wrap(h, sc.name, sc.val), `":"`) {
						// Raw unescaped quotes inside JSON string are invalid JSON; skip that combo.
						continue
					}
					if sc.name == "Digest" && strings.Contains(w.wrap(h, sc.name, sc.val), `":["`) {
						continue
					}
					input := w.wrap(h, sc.name, sc.val)
					assertHidden(t, input, []string{sc.need}, w.keep)
				}
			}
		}
	})

	t.Run("cookie header grammar", func(t *testing.T) {
		headers := []string{"Cookie", "Set-Cookie", "cookie"}
		wrappers := []struct {
			build func(h string) string
			need  []string
			keep  func(h string) []string
		}{
			{
				build: func(h string) string { return h + ": sid=secCookie11; Path=/; HttpOnly" },
				need:  []string{"secCookie11"},
			},
			{
				build: func(h string) string { return "curl -H '" + h + ": sid=secCookie11' https://x.invalid" },
				need:  []string{"secCookie11"},
				keep:  func(string) []string { return []string{"' https://x.invalid"} },
			},
			{
				build: func(h string) string { return `curl -H "` + h + `: sid=secCookie11" https://x.invalid` },
				need:  []string{"secCookie11"},
				keep:  func(string) []string { return []string{`" https://x.invalid`} },
			},
			{
				build: func(h string) string { return "map[" + h + ":[sid=secCookie11; Path=/ refresh=secCookie22; Secure]]" },
				need:  []string{"secCookie11", "secCookie22"},
				keep:  func(h string) []string { return []string{"map[" + h + ":[", "]]"} },
			},
			{
				build: func(h string) string { return `{"` + h + `":["sid=secCookie11; Path=/","refresh=secCookie22"]}` },
				need:  []string{"secCookie11", "secCookie22"},
				keep:  func(h string) []string { return []string{`"` + h + `":[`, `]`} },
			},
			{
				build: func(h string) string {
					return "{\n  \"" + h + "\": [\n    \"sid=secCookie11; Path=/\",\n    \"refresh=secCookie22\"\n  ]\n}"
				},
				need: []string{"secCookie11", "secCookie22"},
				keep: func(h string) []string { return []string{`"` + h + `": [`} },
			},
		}

		for _, h := range headers {
			for _, w := range wrappers {
				var keep []string
				if w.keep != nil {
					keep = w.keep(h)
				}
				assertHidden(t, w.build(h), w.need, keep)
			}
		}
	})

	t.Run("url credential grammar", func(t *testing.T) {
		schemes := []string{"postgres", "redis", "https", "amqp"}
		specials := []string{",", ";", ")", "]", "}", "@", "/", "?", "#", "'"}
		hosts := []string{"db.invalid:5432", "[2001:db8::1]:5432"}

		for _, sch := range schemes {
			for _, sp := range specials {
				for _, host := range hosts {
					pw := "pwA9" + sp + "pwB8"
					input := sch + "://app:" + pw + "@" + host + "/db"
					assertHidden(t, input, []string{"pwA9", "pwB8"}, []string{sch + "://[REDACTED:url_credential]@" + host + "/db"})
				}
			}
		}
	})

	t.Run("escaped newline nested yaml grammar", func(t *testing.T) {
		newlines := []string{`\n`, `\r\n`, `\u000a`, `\u000A`}
		parents := []string{"secret", "password", "token", "api_key"}
		for _, nl := range newlines {
			for _, p := range parents {
				input := `{"text":"` + p + `:` + nl + `  value: escYamlSec99` + nl + `status: ok` + nl + `"}`
				assertHidden(t, input, []string{"escYamlSec99"}, []string{"status: ok"})
			}
		}
	})

	t.Run("truncated pem grammar", func(t *testing.T) {
		labels := []string{"PRIVATE KEY", "RSA PRIVATE KEY", "EC PRIVATE KEY", "OPENSSH PRIVATE KEY"}
		body := "MIIEvQIBADANBgkqhkiG9w0BAQEFAASCBKcw"
		layouts := []struct {
			build func(hdr string) string
			keep  []string
		}{
			{func(hdr string) string { return hdr + "\n" + body }, nil},
			{func(hdr string) string { return hdr + " " + body }, nil},
			{func(hdr string) string { return hdr + body }, nil},
			{func(hdr string) string { return `{"k":"` + hdr + ` ` + body + `"}` }, []string{`"}`}},
			{func(hdr string) string { return `{"k":"` + hdr + `\n` + body + `"}` }, []string{`"}`}},
			{func(hdr string) string { return `{"k":"` + hdr + `\u000a` + body + `"}` }, []string{`"}`}},
			{func(hdr string) string { return `{"m":"{\"k\":\"` + hdr + `\\n` + body + `\"}"}` }, []string{`\"}"}`}},
		}
		for _, lbl := range labels {
			hdr := joinParts("-----BEGIN ", lbl, "-----")
			for _, l := range layouts {
				assertHidden(t, l.build(hdr), []string{body}, l.keep)
			}
		}
	})
}

func TestCorpusGrammarMustKeep(t *testing.T) {
	assertKept := func(t *testing.T, input string) {
		t.Helper()
		res := Text(input)
		if res.Text != input || res.Hits != 0 {
			t.Fatalf("Text(%q) altered clean input: got %q (hits=%d cats=%v)", input, res.Text, res.Hits, res.Categories)
		}
	}

	t.Run("prose status after colon", func(t *testing.T) {
		keys := []string{"credentials", "password", "token", "pass", "secret", "api_key"}
		phrases := []string{
			"loaded from file", "changed", "refreshed", "passed", "rotated",
			"verified", "missing", "required", "disabled", "expired",
			"configured via env", "updating cache", "authentication failed",
		}
		for _, k := range keys {
			for _, ph := range phrases {
				assertKept(t, k+": "+ph)
			}
		}
	})

	t.Run("auth header prose", func(t *testing.T) {
		headers := []string{"Authorization", "Proxy-Authorization"}
		phrases := []string{
			"skipping verification",
			"checking credentials",
			"required for admin endpoints",
			"Bearer authentication failed",
			"Bearer missing token",
			"Basic auth is not supported",
		}
		for _, h := range headers {
			for _, ph := range phrases {
				assertKept(t, h+": "+ph)
			}
		}
	})

	t.Run("config metadata yaml and flow", func(t *testing.T) {
		parents := []string{"secret", "credentials", "imagePullSecrets", "token"}
		pairs := [][2]string{
			{"name", "regcred"},
			{"region", "eu"},
			{"profile", "default"},
			{"secretName", "db-creds"},
			{"defaultMode", "0400"},
			{"type", "string"},
			{"minLength", "8"},
			{"required", "true"},
		}
		for _, p := range parents {
			for _, kv := range pairs {
				assertKept(t, p+":\n  "+kv[0]+": "+kv[1]+"\n")
				assertKept(t, p+":\n- "+kv[0]+": "+kv[1]+"\n")
				assertKept(t, p+": {"+kv[0]+": "+kv[1]+"}")
			}
		}
	})

	t.Run("code calls and filesystem paths", func(t *testing.T) {
		keys := []string{"token", "password", "api_key", "secret"}
		calls := []string{`os.Getenv("TOKEN")`, "getPassword()", "cfg.Get(key)"}
		for _, k := range keys {
			for _, c := range calls {
				assertKept(t, k+" = "+c)
			}
		}
		pathKeys := []string{"PWD", "private_key", "credentials"}
		paths := []string{"/opt/runner/work/repo", "/etc/ssl/private/app.key", "/var/run/secrets/token"}
		for _, k := range pathKeys {
			for _, p := range paths {
				assertKept(t, k+"="+p)
				assertKept(t, k+": "+p)
			}
		}
	})

	t.Run("ipv6 urls without userinfo", func(t *testing.T) {
		for _, sch := range []string{"https", "http"} {
			for _, host := range []string{"[2001:db8::1]", "[2001:db8::1]:8443"} {
				for _, path := range []string{"/u/bob@example.com", "/@scope/pkg"} {
					assertKept(t, sch+"://"+host+path)
				}
			}
		}
	})
}
