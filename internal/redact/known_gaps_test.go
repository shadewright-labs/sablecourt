package redact

import (
	"strings"
	"testing"
)

// gapRow pins one input that main's redactor gets wrong. The answer is known
// by construction: a leak row holds a synthetic secret that should be hidden,
// an over-redaction row holds ordinary text that should come back unchanged.
type gapRow struct {
	// class is the gap class; the docs group the rows by it.
	class string
	name  string
	input string
	// leaks lists the synthetic secret strings of a leak row. The row is a gap
	// while at least one of them survives in the output.
	leaks []string
	// overRedacted marks a row with no secret: the row is a gap while the
	// redactor alters the input.
	overRedacted bool
	// example is the text shown in the docs, with the secret replaced by a
	// placeholder. Rows without one are not used as examples.
	example string
}

// gapClasses lists the gap classes in the order the docs show them. Every row
// of knownGapRows uses one of them.
var gapClasses = []string{
	"token key names",
	"URL userinfo",
	"Basic auth",
	"truncated PEM",
	"JWT",
	"cookies",
	"nested YAML",
	"Slack keys",
	"Stripe keys",
	"other: key names",
	"other: Authorization forms",
	"other: value shapes",
	"other: command-line flags",
	"other: split tokens",
	"other: opaque strings",
	"over-redaction",
}

func knownGapRows() []gapRow {
	return []gapRow{
		{class: "nested YAML", name: "yaml nested secret value", input: "secret:\n  value: hunter2\n", leaks: []string{"hunter2"}, example: "secret:\\n  value: <value>"},
		{class: "nested YAML", name: "yaml nested secret double quoted with spaces", input: "secret:\n  value: \"hunter2 with spaces\"\n", leaks: []string{"hunter2", "with spaces"}},
		{class: "nested YAML", name: "yaml nested secret single quoted", input: "secret:\n  value: 'hunter2-single'\n", leaks: []string{"hunter2-single"}},
		{class: "nested YAML", name: "yaml nested secret with tab indent", input: "secret:\n\tvalue: tabHunter2\n", leaks: []string{"tabHunter2"}},
		{class: "nested YAML", name: "yaml nested secret with CRLF", input: "secret:\r\n  value: crlfHunter2\r\n", leaks: []string{"crlfHunter2"}},
		{class: "nested YAML", name: "yaml nested mixed case keys", input: "Client_Secret:\n  Value: mixedCaseVal99\n", leaks: []string{"mixedCaseVal99"}},
		{class: "nested YAML", name: "yaml nested password one level", input: "password:\n  value: passOneLevel9\n", leaks: []string{"passOneLevel9"}},
		{class: "nested YAML", name: "yaml nested token one level", input: "token:\n  value: tokOneLevel9\n", leaks: []string{"tokOneLevel9"}},
		{class: "nested YAML", name: "yaml nested api_key one level", input: "api_key:\n  value: keyOneLevel9\n", leaks: []string{"keyOneLevel9"}},
		{class: "nested YAML", name: "yaml nested secret two levels under secret", input: "secret:\n  prod:\n    value: deepSecretVal2\n", leaks: []string{"deepSecretVal2"}},
		{class: "nested YAML", name: "yaml nested password two levels under non-secret parent", input: "database:\n  password:\n    value: dbPassDeep2\n  host: db.internal\n", leaks: []string{"dbPassDeep2"}, example: "database:\\n  password:\\n    value: <value>"},
		{class: "nested YAML", name: "yaml nested token two levels", input: "auth:\n  token:\n    primary: tokDeepVal2\n  mode: strict\n", leaks: []string{"tokDeepVal2"}},
		{class: "nested YAML", name: "yaml nested api_key two levels", input: "api_key:\n  staging:\n    raw: keyDeepVal2\nstatus: ok\n", leaks: []string{"keyDeepVal2"}},
		{class: "nested YAML", name: "yaml nested secret block scalar", input: "secret:\n  value: |\n    blockLineSecret1\n    blockLineSecret2\nnext: keep\n", leaks: []string{"blockLineSecret1", "blockLineSecret2"}},
		{class: "nested YAML", name: "H2 json escaped newline nested yaml", input: `{"text":"secret:\n  value: hunter2hunter2\n"}`, leaks: []string{"hunter2hunter2"}, example: "{\"text\":\"secret:\\n  value: <value>\\n\"}"},
		{class: "nested YAML", name: "H2 json escaped newline nested yaml two levels", input: `{"text":"secret:\n  prod:\n    value: deepJsonEsc99\nstatus: ok\n"}`, leaks: []string{"deepJsonEsc99"}},
		{class: "nested YAML", name: "H2 json escaped newline nested yaml no trailing newline", input: `{"text":"secret:\n  value: hunter2end"}`, leaks: []string{"hunter2end"}},
		{class: "other: key names", name: "H3 AWS_SECRET_ACCESS_KEY env", input: "AWS_SECRET_ACCESS_KEY=" + awsSecret, leaks: []string{"wJalrXUtnFEMI", "K7MDENG", "bPxRfiCYFIXTUREKEY"}},
		{class: "other: key names", name: "H3 secret_key kv", input: "secret_key=hunter2hunter2", leaks: []string{"hunter2hunter2"}},
		{class: "other: key names", name: "H3 SECRET_KEY yaml", input: "SECRET_KEY: djangoinsecure123", leaks: []string{"djangoinsecure123"}, example: "SECRET_KEY: <value>"},
		{class: "other: key names", name: "H3 secretKey camelCase", input: "secretKey: abcdefghijkl", leaks: []string{"abcdefghijkl"}},
		{class: "other: key names", name: "H3 private_key kv", input: "private_key: abcdefghijkl", leaks: []string{"abcdefghijkl"}},
		{class: "other: key names", name: "H3 access_key kv", input: "access_key=" + joinParts("abcdefghij", "1234"), leaks: []string{"abcdefghij1234"}},
		{class: "other: key names", name: "H3 signing_key kv", input: "signing_key=" + joinParts("signFixture", "1234"), leaks: []string{"signFixture1234"}},
		{class: "other: key names", name: "H3 pwd kv", input: "pwd=hunter2hunter2", leaks: []string{"hunter2hunter2"}, example: "pwd=<value>"},
		{class: "other: key names", name: "H3 pass kv", input: "pass=hunter2hunter2", leaks: []string{"hunter2hunter2"}},
		{class: "other: key names", name: "H3 credentials kv", input: "credentials: hunter2hunter2", leaks: []string{"hunter2hunter2"}, example: "credentials: <value>"},
		{class: "other: key names", name: "H3 json SecretAccessKey", input: `{"SecretAccessKey":"` + awsSecret + `"}`, leaks: []string{"wJalrXUtnFEMI", "bPxRfiCYFIXTUREKEY"}},
		{class: "other: value shapes", name: "H3 xml password tag", input: "<config><password>hunter2hunter2</password><name>svc</name></config>", leaks: []string{"hunter2hunter2"}},
		{class: "other: command-line flags", name: "H3 cli flag space separated", input: "mycli --password hunter2hunter2 --host db.internal", leaks: []string{"hunter2hunter2"}, example: "mycli --password <value> --host db.internal"},
		{class: "other: command-line flags", name: "H3 cli flag quoted value", input: `mycli --api-key "my cli secret" --verbose`, leaks: []string{"my cli secret"}},
		{class: "other: command-line flags", name: "H3 curl -u basic auth", input: joinParts("curl -", "u admin:hunter2", "hunter2 https://api.invalid/v1"), leaks: []string{"hunter2hunter2", "admin:hunter2hunter2"}, example: joinParts("curl -", "u admin:<value> https://api.invalid/v1")},
		{class: "other: key names", name: "fuzz key at end of line takes next line key", input: "password:\nsecret: " + gapSecret + "\n", leaks: []string{gapSecret}},
		{class: "other: Authorization forms", name: "fuzz bearer header at end of line takes next line header", input: "Authorization: Bearer \nAuthorization: Bearer " + gapSecret + "\n", leaks: []string{gapSecret}},
		{class: "Basic auth", name: "H3 curl --user with intermediate flags", input: joinParts("curl -s -X POST --", "user svc:curl", "Secret99 https://api.invalid/v1"), leaks: []string{"curlSecret99"}},
		{class: "other: value shapes", name: "inline double quoted password with spaces", input: "password: \"my fixture pass\"\n", leaks: []string{"my fixture pass"}},
		{class: "other: value shapes", name: "inline single quoted api_key with spaces", input: "api_key='my fixture key'\n", leaks: []string{"my fixture key"}},
		{class: "token key names", name: "inline token key value", input: "auth_token: " + joinParts("fixtureTok", "Val99") + "\n", leaks: []string{"fixtureTokVal99"}, example: "auth_token: <value>"},
		{class: "other: value shapes", name: "M1 json numeric password", input: `{"password": 12345678901}`, leaks: []string{"12345678901"}, example: "{\"password\": 12345678901}"},
		{class: "other: value shapes", name: "M1 json single-line nested secret object", input: `{"secret": {"value": "hunter2hunter2"}}`, leaks: []string{"hunter2hunter2"}},
		{class: "other: value shapes", name: "M1 json multi-line nested secret object", input: "{\n  \"secret\": {\n    \"value\": \"nestedJsonVal99\"\n  }\n}", leaks: []string{"nestedJsonVal99"}},
		{class: "other: value shapes", name: "M1 json secret array", input: `{"token": ["hunter2hunter2"]}`, leaks: []string{"hunter2hunter2"}},
		{class: "other: value shapes", name: "M1 python single-quoted dict", input: `{'password': 'hunter2hunter2'}`, leaks: []string{"hunter2hunter2"}, example: "{'password': '<value>'}"},
		{class: "other: value shapes", name: "M1 walrus assignment", input: `password := "hunter2hunter2"`, leaks: []string{"hunter2hunter2"}},
		{class: "other: value shapes", name: "M1 fat-arrow assignment", input: `password => "hunter2hunter2"`, leaks: []string{"hunter2hunter2"}},
		{class: "nested YAML", name: "M1 yaml flow mapping", input: `secret: {value: hunter2hunter2}`, leaks: []string{"hunter2hunter2"}},
		{class: "nested YAML", name: "M1 yaml flow sequence", input: `secret: [abcdefgh, ijklmnop]`, leaks: []string{"abcdefgh", "ijklmnop"}},
		{class: "other: value shapes", name: "M1 unquoted value containing comma early", input: `password=ab,cdefghijk`, leaks: []string{"ab,cdefghijk", "cdefghijk"}, example: "password=ab,<rest of value>"},
		{class: "other: value shapes", name: "M1 unquoted value containing comma mid", input: `password=hunter,2secretX`, leaks: []string{"hunter", "2secretX"}},
		{class: "other: value shapes", name: "M1 unquoted value containing semicolon", input: `password=abc;defghijkl`, leaks: []string{"abc;defghijkl", "defghijkl"}},
		{class: "other: value shapes", name: "M1 unquoted value containing apostrophe", input: `password=o'brien12345`, leaks: []string{"o'brien12345", "brien12345"}},
		{class: "other: value shapes", name: "M1 multiline double-quoted password", input: "password=\"Zq9xK2\nmV7pLwXXXXX\"", leaks: []string{"Zq9xK2", "mV7pLwXXXXX"}},
		{class: "other: value shapes", name: "Low single-quoted escaped apostrophe", input: `password='Zq9xK2\'mV7pLw'`, leaks: []string{"Zq9xK2", "mV7pLw"}},
		{class: "nested YAML", name: "M2 block scalar misparse idempotency", input: "secret:\n  secret: |#abcdefg hijklmn\n", leaks: []string{"abcdefg", "hijklmn"}},
		{class: "nested YAML", name: "M2 block scalar hash without space", input: "secret:\n  value: |#hunter2hunter2\n", leaks: []string{"hunter2hunter2"}},
		{class: "nested YAML", name: "M2 block scalar multi-digit value", input: "secret:\n  value: >12345678\n", leaks: []string{"12345678"}},
		{class: "nested YAML", name: "M3 block scalar trailing hash line", input: "private_key: |\n  abcdefgh\n  #tailsecretmaterial\n", leaks: []string{"abcdefgh", "tailsecretmaterial"}},
		{class: "nested YAML", name: "M3 block scalar only hash line", input: "password: |\n  #hunter2hunter2\n", leaks: []string{"hunter2hunter2"}},
		{class: "nested YAML", name: "M4 yaml same-indent list under password", input: "password:\n- hunter2hunter2\n", leaks: []string{"hunter2hunter2"}},
		{class: "nested YAML", name: "M4 yaml plural secrets list", input: "secrets:\n  - hunter2hunter2\n", leaks: []string{"hunter2hunter2"}},
		{class: "nested YAML", name: "M4 yaml non-secret parent with pass child", input: "db:\n  host: x\n  pass: hunter2hunter2\n", leaks: []string{"hunter2hunter2"}},
		{class: "nested YAML", name: "M4 yaml anchor on header", input: "password: &pw\n  hunter2hunter2\n", leaks: []string{"hunter2hunter2"}},
		{class: "nested YAML", name: "M4 yaml explicit tag", input: "password: !!str hunter2hunter2\n", leaks: []string{"hunter2hunter2"}},
		{class: "nested YAML", name: "M4 yaml log-prefixed secret header", input: "2026-01-01T00:00:00Z INFO secret:\n  value: hunter2hunter2\n", leaks: []string{"hunter2hunter2"}},
		{class: "nested YAML", name: "M4 yaml CR-only newlines", input: "secret:\r  value: hunter2hunter2\r", leaks: []string{"hunter2hunter2"}},
		{class: "nested YAML", name: "M4 yaml unicode line separator", input: "secret:\u2028  value: hunter2hunter2\n", leaks: []string{"hunter2hunter2"}},
		{class: "nested YAML", name: "M4 yaml NBSP indentation", input: "secret:\n\u00a0\u00a0value: hunter2hunter2\n", leaks: []string{"hunter2hunter2"}},
		{class: "token key names", name: "M7 url query secret stops at ampersand", input: "https://x.invalid/?token=abcdefgh&next=/home&lang=en", leaks: []string{"abcdefgh"}, example: "https://x.invalid/?token=<value>&next=/home"},
		{class: "URL userinfo", name: "url postgres user and password", input: "dsn=postgres://app:" + "hunter2pass" + "@example.com:5432/x", leaks: []string{"hunter2pass"}, example: "postgres://app:<password>@example.com:5432/x"},
		{class: "URL userinfo", name: "url password only", input: "redis://:" + "onlypass99" + "@cache.internal:6379/0", leaks: []string{"onlypass99"}, example: "redis://:<password>@cache.internal:6379/0"},
		{class: "URL userinfo", name: "url percent-encoded userinfo", input: "https://user:" + "p%40ss%3Aword" + "@api.invalid/v1/items", leaks: []string{"p%40ss%3Aword"}},
		{class: "URL userinfo", name: "url userinfo inside json", input: `{"url":"amqp://svc:` + `brokerPass88` + `@mq.internal:5672/vhost"}`, leaks: []string{"brokerPass88"}},
		{class: "URL userinfo", name: "url userinfo with query and neighbour token", input: "connect postgres://app:" + "urlPass77" + "@example.com/x?sslmode=require token=" + ghp, leaks: []string{"urlPass77", "0123456789", "klmnopqrst"}},
		{class: "URL userinfo", name: "H5 url password containing at sign", input: "postgres://app:p@ssw0rd99@example.com/x", leaks: []string{"p@ssw0rd99", "ssw0rd99"}, example: "postgres://app:p@<rest of password>@example.com/x"},
		{class: "URL userinfo", name: "H5 url password containing slash", input: "redis://:pa/ss1234@example.com", leaks: []string{"pa/ss1234", "ss1234"}},
		{class: "URL userinfo", name: "H5 url password containing hash", input: "postgres://app:pa#ss1234@example.com/x", leaks: []string{"pa#ss1234", "ss1234"}},
		{class: "URL userinfo", name: "H5 url password containing question mark", input: "postgres://app:pa?ss1234@example.com/x", leaks: []string{"pa?ss1234", "ss1234"}},
		{class: "URL userinfo", name: "H5 url password containing apostrophe", input: "postgres://app:pa'ss1234@example.com/x", leaks: []string{"pa'ss1234", "ss1234"}},
		{class: "URL userinfo", name: "H5 url with JSON-escaped slashes", input: `{"u":"https:\/\/user:hunter2pw@example.com\/x"}`, leaks: []string{"hunter2pw"}},
		{class: "Basic auth", name: "authorization basic", input: "Authorization: Basic " + basicB64, leaks: []string{basicB64}, example: "Authorization: Basic <base64>"},
		{class: "Basic auth", name: "authorization basic padded lowercase tab CRLF", input: "authorization:\tbasic\t" + basicPadded + "\r\n", leaks: []string{basicPadded}},
		{class: "Basic auth", name: "proxy-authorization basic", input: "Proxy-Authorization: Basic " + basicB64, leaks: []string{basicB64}, example: "Proxy-Authorization: Basic <base64>"},
		{class: "Basic auth", name: "authorization basic in json header map", input: `{"Authorization": "Basic ` + basicB64 + `"}`, leaks: []string{basicB64}},
		{class: "other: Authorization forms", name: "H4 go map Authorization Bearer", input: "map[Authorization:[Bearer abcdefghijkl]]", leaks: []string{"abcdefghijkl"}},
		{class: "Basic auth", name: "H4 go map Authorization Basic", input: "map[Authorization:[Basic " + basicUserPass + "]]", leaks: []string{basicUserPass}, example: "map[Authorization:[Basic <base64>]]"},
		{class: "other: Authorization forms", name: "H4 json http.Header array Bearer", input: `{"Authorization":["Bearer abcdefghijkl"]}`, leaks: []string{"abcdefghijkl"}, example: "{\"Authorization\":[\"Bearer <value>\"]}"},
		{class: "other: Authorization forms", name: "H4 Authorization Token scheme", input: "Authorization: Token abcdefghijkl", leaks: []string{"abcdefghijkl"}, example: "Authorization: Token <value>"},
		{class: "other: Authorization forms", name: "H4 Authorization ApiKey scheme", input: "Authorization: ApiKey abcdefghijkl", leaks: []string{"abcdefghijkl"}},
		{class: "other: Authorization forms", name: "H4 Authorization Digest scheme", input: `Authorization: Digest username="u", response="0123456789abcdef"`, leaks: []string{"0123456789abcdef"}, example: "Authorization: Digest username=\"u\", response=\"<value>\""},
		{class: "cookies", name: "H4 Cookie header session", input: "Cookie: session=abcdef0123456789", leaks: []string{"abcdef0123456789"}, example: "Cookie: session=<value>"},
		{class: "cookies", name: "H4 Cookie header preference accepted redaction", input: "Cookie: theme=dark", leaks: []string{"theme=dark"}},
		{class: "cookies", name: "H4 Set-Cookie header preserves attributes", input: "Set-Cookie: sid=abcdef0123456789; Path=/; HttpOnly", leaks: []string{"abcdef0123456789"}, example: "Set-Cookie: sid=<value>; Path=/; HttpOnly"},
		{class: "other: Authorization forms", name: "H4 Authorization Bearer NBSP separator", input: "Authorization: Bearer\u00a0abcdefghijkl", leaks: []string{"abcdefghijkl"}},
		{class: "other: Authorization forms", name: "H4 Authorization Bearer with punctuation", input: "Authorization: Bearer abcdefghij!klmnop", leaks: []string{"abcdefghij", "klmnop"}},
		{class: "JWT", name: "jwt without padding", input: "session=" + jwtNoPad + " ok", leaks: []string{"hbGciOiJIUzI1Ni", "zdWIiOiJmaXh0dXJl", "c2lnbmF0dXJlZml4dHVyZTEyMw"}, example: "session=<header>.<payload>.<signature>"},
		{class: "JWT", name: "jwt with padding", input: "cookie: " + jwtPadded + "\r\n", leaks: []string{"hbGciOiJIUzI1Ni", "zdWIiOiJmaXh0dXJl", "c2lnbmF0dXJlZml4dHVyZQ=="}},
		{class: "JWT", name: "jwt inside json", input: `{"id":"` + jwtNoPad + `","status":"ok"}`, leaks: []string{"hbGciOiJIUzI1Ni", "c2lnbmF0dXJlZml4dHVyZTEyMw"}},
		{class: "JWT", name: "M5 unsigned jwt empty signature", input: "tok=" + jwtUnsignedEmptySig, leaks: []string{"hbGciOiJub25l", "zdWIiOiJ4eHh4"}},
		{class: "JWT", name: "M5 two-segment jwt", input: "tok=" + jwtTwoSeg, leaks: []string{"hbGciOiJub25l", "zdWIiOiJ4eHh4"}, example: "tok=<header>.<payload>"},
		{class: "JWT", name: "M5 five-segment jwe", input: "enc=" + jweFiveSeg, leaks: []string{"hbGciOiJkaXI", "ENCKEYENCKEY", "CIPHERTEXTCIPHER", "TAGTAGTAGTAG"}, example: "enc=<five dot-separated segments>"},
		{class: "JWT", name: "M5 jwt standard base64 signature", input: "tok=" + jwtStdB64Sig, leaks: []string{"hbGciOiJIUzI1Ni", "ghijklmnop", "qrstu"}},
		{class: "JWT", name: "M5 jwt line-wrapped signature", input: jwtWrappedSig, leaks: []string{"c2lnbmF0", "dXJlMTIz"}},
		{class: "JWT", name: "M5 jwt dot at start of line", input: jwtWrappedDots, leaks: []string{"hbGciOiJIUzI1Ni", "c2lnbmF0dXJlMTIz"}},
		{class: "JWT", name: "M5 jwt spaces around dots", input: jwtSpacedDots, leaks: []string{"hbGciOiJIUzI1Ni", "c2lnbmF0dXJlMTIz"}},
		{class: "JWT", name: "M5 jwt preceded by word char", input: "jwt_" + jwtNoPad + " 1" + jwtNoPad, leaks: []string{"hbGciOiJIUzI1Ni", "c2lnbmF0dXJlZml4dHVyZTEyMw"}},
		{class: "truncated PEM", name: "pem mismatched end line", input: joinParts("before\n", "-----BEGIN ", "RSA PRIVATE KEY-----\n", pemBody, "\n", "-----END ", "EC PRIVATE KEY-----\n", "trailingSecretBody"), leaks: []string{"fixtureBodyLine1", "trailingSecretBody"}, example: "a BEGIN PRIVATE KEY block closed by an END line of another label"},
		{class: "other: split tokens", name: "provider ghp split by invalid utf8 byte", input: "val=" + ghpSplit, leaks: []string{"0123456789", "abcdefghij", "klmnopqrst"}, example: "a GitHub token with an invalid UTF-8 byte inside"},
		{class: "other: split tokens", name: "provider ghp split inside prefix by invalid utf8 byte", input: "val=" + ghpSplitPrefix, leaks: []string{"0123456789", "abcdefghij", "klmnopqrst"}},
		{class: "other: split tokens", name: "M6 provider ghp split by ANSI CSI", input: "val=" + ghpSplitANSI, leaks: []string{"0123456789", "abcdefghij", "klmnopqrst"}},
		{class: "other: split tokens", name: "M6 provider ghp split by soft hyphen", input: "val=" + ghpSplitSoftHyphen, leaks: []string{"0123456789", "abcdefghij", "klmnopqrst"}},
		{class: "other: split tokens", name: "M6 provider ghp split by ZWSP", input: "val=" + ghpSplitZWSP, leaks: []string{"0123456789", "abcdefghij", "klmnopqrst"}, example: "a GitHub token with a zero-width space inside"},
		{class: "other: split tokens", name: "M6 provider ghp split by ZWSP after 20 chars", input: "val=" + ghpSplitZWSPTail, leaks: []string{"0123456789", "klmnopqrst"}},
		{class: "other: split tokens", name: "M6 provider ghp split by NUL", input: "val=" + ghpSplitNUL, leaks: []string{"0123456789", "abcdefghij", "klmnopqrst"}},
		{class: "other: split tokens", name: "M6 provider ghp split by literal U+FFFD", input: "val=" + ghpSplitFFFD, leaks: []string{"0123456789", "abcdefghij", "klmnopqrst"}},
		{class: "other: split tokens", name: "M6 provider ghp split by CRLF", input: "val=" + ghpSplitCRLF, leaks: []string{"0123456789", "abcdefghij", "klmnopqrst"}},
		{class: "other: split tokens", name: "M6 provider ghp glued to prefix and suffix", input: "MY_" + ghp + "_old token" + ghp, leaks: []string{"0123456789", "abcdefghij", "klmnopqrst"}},
		{class: "other: split tokens", name: "provider op token split by invalid utf8 byte", input: "val=" + opTokSplit, leaks: []string{"0123456789abcdef"}},
		{class: "other: split tokens", name: "Low provider aws AKIA with trailing underscore", input: awsAKIA + "_ID", leaks: []string{"FIXTURE012345678"}, example: "AKIA<16 characters>_ID"},
		{class: "other: split tokens", name: "provider aws key split by invalid utf8 byte", input: "val=" + awsSplit, leaks: []string{"FIXTURE", "012345678"}},
		{class: "Stripe keys", name: "Low provider stripe live key", input: "stripe=" + stripeLive, leaks: []string{"0123456789abcdefghij", "klmnop"}, example: "stripe=sk_live_<24 characters>"},
		{class: "Slack keys", name: "Low provider slack bot token", input: "slack=" + slackBot, leaks: []string{"0123456789", "abcdefghijkl"}, example: "slack=xoxb-<digits>-<digits>-<letters>"},
		{class: "other: split tokens", name: "provider ghp long prefix split before tail by invalid utf8 byte", input: "val=" + joinParts("ghp_", "0123456789abcdefghij", "\xff", "tailSecret99"), leaks: []string{"0123456789", "tailSecret99"}},
		{class: "Basic auth", name: "authorization basic split by invalid utf8 byte", input: "Authorization: Basic " + joinParts("dXNl", "\xff", "cjpmaXh0dXJl"), leaks: []string{"dXNl", "cjpmaXh0dXJl"}},
		{class: "other: value shapes", name: "adjacent quoted secrets on one line", input: `password="first secret val" api_key="second secret val" status=ok`, leaks: []string{"first secret val", "second secret val"}},
		{class: "nested YAML", name: "nested yaml secret then safe sibling then second secret", input: "secret:\n  value: firstYamlSec\nstatus: healthy\ntoken:\n  value: secondYamlSec\n", leaks: []string{"firstYamlSec", "secondYamlSec"}},
		{class: "URL userinfo", name: "N2 url password containing comma", input: "postgres://app:hunter2,pw@example.com/x", leaks: []string{"hunter2", ",pw@example.com"}},
		{class: "URL userinfo", name: "N2 url password containing semicolon", input: "postgres://app:hunter2;pw@example.com/x", leaks: []string{"hunter2", ";pw@example.com"}},
		{class: "URL userinfo", name: "N2 url password containing closing paren", input: "postgres://app:hunter2)pw@example.com/x", leaks: []string{"hunter2", ")pw@example.com"}},
		{class: "URL userinfo", name: "N2 url password containing closing bracket", input: "postgres://app:hunter2]pw@example.com/x", leaks: []string{"hunter2", "]pw@example.com"}},
		{class: "URL userinfo", name: "N2 url password containing closing brace", input: "postgres://app:hunter2}pw@example.com/x", leaks: []string{"hunter2", "}pw@example.com"}},
		{class: "cookies", name: "N3 go map Set-Cookie multiple values", input: "map[Set-Cookie:[sid=abc123; Path=/ refresh=xyz789; Secure]]", leaks: []string{"abc123", "xyz789"}},
		{class: "cookies", name: "N3 json array Set-Cookie multiple values", input: `{"Set-Cookie":["a=b; Path=/","refresh=xyz789"]}`, leaks: []string{"a=b", "xyz789"}},
		{class: "cookies", name: "N6 curl single-quoted Cookie preserves closing quote and url", input: "curl -H 'Cookie: a=b' https://x.invalid", leaks: []string{"a=b"}, example: "curl -H 'Cookie: a=b' https://x.invalid"},
		{class: "cookies", name: "N6 curl double-quoted Cookie preserves closing quote and url", input: `curl -H "Cookie: a=b" https://x.invalid`, leaks: []string{"a=b"}},
		// Rows from the first rewrite's known-gap table that main's redactor
		// also leaks.
		{class: "other: key names", name: "key synonym secret key base", input: "SECRET_KEY_BASE=" + gapSecret, leaks: []string{gapSecret}, example: "SECRET_KEY_BASE=<value>"},
		{class: "other: key names", name: "key synonym encryption key", input: "ENCRYPTION_KEY=" + gapSecret, leaks: []string{gapSecret}},
		{class: "other: key names", name: "key synonym master key", input: "master_key: " + gapSecret, leaks: []string{gapSecret}},
		{class: "other: key names", name: "passphrase key", input: "passphrase: correct horse battery staple", leaks: []string{"correct", "horse battery staple"}},
		{class: "other: value shapes", name: "double escaped json", input: `{"m":"{\\\"password\\\":\\\"` + gapSecret + `\\\"}"}`, leaks: []string{gapSecret}},
		{class: "other: value shapes", name: "short unquoted value", input: "password=abc12", leaks: []string{"abc12"}},
		{class: "other: value shapes", name: "unterminated quote short value", input: `password="abc12`, leaks: []string{"abc12"}},
		{class: "other: value shapes", name: "multiline quoted value stops at colon line", input: "password=\"abc\ndef:ghi\"", leaks: []string{"ghi"}},
		{class: "cookies", name: "folded cookie header", input: "Cookie:\r\n sid=" + gapSecret, leaks: []string{gapSecret}},
		{class: "cookies", name: "logfmt quoted cookie", input: `cookie="sid=` + gapSecret + `"`, leaks: []string{gapSecret}},
		{class: "cookies", name: "curl cookie flag", input: "curl -b sid=" + gapSecret + " https://example.test/", leaks: []string{gapSecret}},
		{class: "cookies", name: "curl long cookie flag", input: "curl --cookie sid=" + gapSecret + " https://example.test/", leaks: []string{gapSecret}},
		{class: "other: Authorization forms", name: "logfmt authorization bearer", input: "authorization=Bearer " + gapSecret, leaks: []string{gapSecret}, example: "authorization=Bearer <value>"},
		{class: "other: Authorization forms", name: "scheme-less authorization", input: "Authorization: " + gapSecret, leaks: []string{gapSecret}, example: "Authorization: <value>"},
		{class: "other: command-line flags", name: "curl proxy-user", input: "curl --proxy-user u:" + gapSecret + " https://example.test/", leaks: []string{gapSecret}, example: "curl --proxy-user u:<value> https://example.test/"},
		{class: "other: split tokens", name: "github token split by crlf after twenty characters", input: joinParts("gh", "p_", "0123456789abcdefghij", "\r\n", "klmnopqrst"), leaks: []string{"klmnopqrst"}},
		{class: "other: split tokens", name: "github token split by crlf after prefix", input: joinParts("gh", "p_", "\r\n", "0123456789abcdefghijkl"), leaks: []string{"0123456789abcdefghijkl"}},
		{class: "other: split tokens", name: "provider token split by ascii whitespace", input: "val=" + joinParts("ghp_", "0123456789", " ", "abcdefghij", "klmnopqrst"), leaks: []string{"abcdefghij"}, example: "a GitHub token with a space inside"},
		{class: "token key names", name: "flow sequence item under a token key", input: "  token: [\n  " + gapSecret + "\n  ]\n", leaks: []string{gapSecret}},
		{class: "URL userinfo", name: "url password containing both at sign and slash", input: "postgres://app:p@ss/word@example.com", leaks: []string{"ss/word"}, example: "postgres://app:p@ss/word@example.com"},
		{class: "other: opaque strings", name: "unlabelled opaque hex secret", input: "deploy " + joinParts("9f8e7d6c5b4a39281716", "05f4e3d2c1b0a9876543"), leaks: []string{"9f8e7d6c5b4a39281716"}, example: "deploy <40 hex characters>"},
		// Over-redaction: ordinary text that main hides. The row stays in the
		// table while the text is altered.
		{class: "over-redaction", name: "typescript interface with token and apiKey types", input: "interface Cfg { token: string; apiKey: string }", overRedacted: true, example: "interface Cfg { token: string; apiKey: string }"},
		{class: "over-redaction", name: "k8s secret volume metadata", input: "secret:\n  secretName: db-creds\n  defaultMode: 0400\n", overRedacted: true, example: "secret:\n  secretName: db-creds\n  defaultMode: 0400"},
		{class: "over-redaction", name: "prose password changed", input: "password: changed", overRedacted: true, example: "password: changed"},
		{class: "over-redaction", name: "bearer authentication failed prose", input: "Authorization: Bearer authentication failed", overRedacted: true, example: "Authorization: Bearer authentication failed"},
		{class: "over-redaction", name: "env reference in password value", input: "password: ${DB_PASSWORD}", overRedacted: true, example: "password: ${DB_PASSWORD}"},
		{class: "over-redaction", name: "secret name reference", input: "existingSecret: postgres-secret", overRedacted: true},
		{class: "over-redaction", name: "private key header with no key after it", input: "grep '" + joinParts("-----BEGIN ", "PRIVATE KEY-----") + "' *.pem\nfile1.pem\n", overRedacted: true, example: "grep '-----BEGIN PRIVATE KEY-----' *.pem"},
	}
}

// TestKnownGaps pins today's behaviour of main's redactor for the gap rows.
// When a gap row starts being hidden, the test fails: move the row to the
// corpus (TestCorpusMustHide or TestCorpusMustKeep) instead of deleting it.
func TestKnownGaps(t *testing.T) {
	for _, tc := range knownGapRows() {
		t.Run(tc.class+"/"+tc.name, func(t *testing.T) {
			got := String(tc.input)
			if tc.overRedacted {
				if got == tc.input {
					t.Errorf("gap closed? %q is no longer altered: move this row to the must-keep corpus", tc.input)
				}
				return
			}
			if len(tc.leaks) == 0 {
				t.Fatalf("row %q has no leaks and is not an over-redaction row", tc.name)
			}
			for _, s := range tc.leaks {
				if strings.Contains(got, s) {
					return
				}
			}
			t.Errorf("gap closed? none of %q survives in %q: move this row to the must-hide corpus", tc.leaks, got)
		})
	}
}

func TestKnownGapRowsShape(t *testing.T) {
	known := map[string]bool{}
	for _, c := range gapClasses {
		known[c] = true
	}
	seen := map[string]bool{}
	used := map[string]int{}
	examples := map[string]int{}
	for _, r := range knownGapRows() {
		if !known[r.class] {
			t.Errorf("row %q has unknown class %q", r.name, r.class)
		}
		key := r.class + "/" + r.name
		if seen[key] {
			t.Errorf("duplicate gap row %q", key)
		}
		seen[key] = true
		for _, n := range r.leaks {
			if !strings.Contains(r.input, n) {
				t.Errorf("row %q leak needle %q does not occur in its input", r.name, n)
			}
		}
		used[r.class]++
		if r.example != "" {
			examples[r.class]++
		}
	}
	for _, c := range gapClasses {
		if used[c] == 0 {
			t.Errorf("class %q has no rows", c)
		}
		if examples[c] < 1 {
			t.Errorf("class %q has no row with an example", c)
		}
	}
}
