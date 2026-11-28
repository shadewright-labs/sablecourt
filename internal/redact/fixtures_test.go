package redact

import "strings"

// Fixtures shared by the corpus and the known-gap table. Every secret is
// synthetic and built from parts, so no scanner sees a whole token.
var (
	gapSecret          = joinParts("Zq9x", "K2mV", "7pLw")
	awsTail            = joinParts("AKIA", "FIXTURE", "012345678")
	ghp                = joinParts("ghp_", "0123456789", "abcdefghij", "klmnopqrst")
	ghpSplit           = joinParts("ghp_", "0123456789", "\xff", "abcdefghij", "klmnopqrst")
	ghpSplitPrefix     = joinParts("gh", "\xff", "p_", "0123456789", "abcdefghij", "klmnopqrst")
	ghpSplitANSI       = joinParts("ghp_", "0123456789", "\x1b[0m", "abcdefghij", "klmnopqrst")
	ghpSplitSoftHyphen = joinParts("ghp_", "0123456789", "\u00ad", "abcdefghij", "klmnopqrst")
	ghpSplitZWSP       = joinParts("ghp_", "0123456789", "\u200b", "abcdefghij", "klmnopqrst")
	ghpSplitZWSPTail   = joinParts("ghp_", "0123456789abcdefghij", "\u200b", "klmnopqrst")
	ghpSplitNUL        = joinParts("ghp_", "0123456789", "\x00", "abcdefghij", "klmnopqrst")
	ghpSplitFFFD       = joinParts("ghp_", "0123456789", "\ufffd", "abcdefghij", "klmnopqrst")
	ghpSplitCRLF       = joinParts("ghp_", "0123456789", "\r\n", "abcdefghij", "klmnopqrst")
	gho                = joinParts("gho_", "0123456789", "abcdefghij", "klmnopqrst")
	ghu                = joinParts("ghu_", "0123456789", "abcdefghij", "klmnopqrst")
	ghs                = joinParts("ghs_", "0123456789", "abcdefghij", "klmnopqrst")
	ghr                = joinParts("ghr_", "0123456789", "abcdefghij", "klmnopqrst")
	ghPat              = joinParts("github_", "pat_", "0123456789", "_abcdefghij_klmnop")
	scTask             = joinParts("sc_", "task_", "0123456789abcdef", "fedcba9876543210")
	opTok              = joinParts("op_", strings.Repeat("0123456789abcdef", 4))
	opTokSplit         = joinParts("op_", strings.Repeat("0123456789abcdef", 2), "\xfe", strings.Repeat("0123456789abcdef", 2))
	antKey             = joinParts("sk-", "ant-", "api03-", "fixture0123", "456789abcdef")
	oaiKey             = joinParts("sk-", "proj-", "fixture0123456789abcdef")
	oaiLegacy          = joinParts("sk-", "fixture0123456789abcdef")
	googKey            = joinParts("AI", "za", "SyFixture0123456789abcd")
	awsAKIA            = joinParts("AK", "IA", "FIXTURE012345678")
	awsASIA            = joinParts("AS", "IA", "FIXTURE012345678")
	awsSplit           = joinParts("AK", "IA", "FIXTURE", "\x80", "012345678")
	awsSecret          = joinParts("wJalrXUtnFEMI/", "K7MDENG/", "bPxRfiCYFIXTUREKEY")
	stripeLive         = joinParts("sk_", "live_", "0123456789abcdefghij", "klmnop")
	slackBot           = joinParts("xox", "b-", "0123456789", "-0123456789-abcdefghijkl")

	basicB64      = joinParts("dXNlcjp", "maXh0dXJl")
	basicPadded   = joinParts("YXBwOn", "Bhc3N3b3Jk", "==")
	basicUserPass = joinParts("dXNlcjp", "wYXNzd29yZA==")
	bearerVal     = joinParts("tok_", "fixture_", "0123456789abcdef")

	jwtNoPad = joinParts(
		"eyJ", "hbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.",
		"eyJ", "zdWIiOiJmaXh0dXJlIn0.",
		"c2lnbmF0dXJlZml4dHVyZTEyMw",
	)
	jwtPadded = joinParts(
		"eyJ", "hbGciOiJIUzI1NiJ9.",
		"eyJ", "zdWIiOiJmaXh0dXJlIn0=.",
		"c2lnbmF0dXJlZml4dHVyZQ==",
	)
	jwtUnsignedEmptySig = joinParts(
		"eyJ", "hbGciOiJub25lIn0.",
		"eyJ", "zdWIiOiJ4eHh4In0.",
	)
	jwtTwoSeg = joinParts(
		"eyJ", "hbGciOiJub25lIn0.",
		"eyJ", "zdWIiOiJ4eHh4In0",
	)
	jweFiveSeg = joinParts(
		"eyJ", "hbGciOiJkaXIifQ.",
		"ENCKEYENCKEY.",
		"IVIVIVIVIV.",
		"CIPHERTEXTCIPHER.",
		"TAGTAGTAGTAG",
	)
	jwtStdB64Sig = joinParts(
		"eyJ", "hbGciOiJIUzI1NiJ9.",
		"eyJ", "zdWIiOiJ4eHh4In0.",
		"abcdef/ghijklmnop+qrstu",
	)
	jwtWrappedSig = joinParts(
		"eyJ", "hbGciOiJIUzI1NiJ9.",
		"eyJ", "zdWIiOiJ4eHh4In0.",
		"c2lnbmF0\ndXJlMTIz",
	)
	jwtWrappedDots = joinParts(
		"eyJ", "hbGciOiJIUzI1NiJ9\n.",
		"eyJ", "zdWIiOiJ4eHh4In0\n.",
		"c2lnbmF0dXJlMTIz",
	)
	jwtSpacedDots = joinParts(
		"eyJ", "hbGciOiJIUzI1NiJ9 . ",
		"eyJ", "zdWIiOiJ4eHh4In0 . ",
		"c2lnbmF0dXJlMTIz",
	)

	pemBody     = joinParts("MIIE", "fixtureBodyLine1\n", "fixtureBodyLine2")
	pemBodyCRLF = joinParts("MIIE", "fixtureBodyLine1\r\n", "fixtureBodyLine2")
)
