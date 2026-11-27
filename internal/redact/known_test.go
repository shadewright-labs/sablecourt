package redact

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func synthSecret(parts ...string) string {
	return strings.Join(parts, "")
}

func TestRegistryMinimumLength(t *testing.T) {
	reg := NewRegistry()
	for _, short := range []string{"", "a", "ok", "true", "1234567", "       ", "  short  ", "\t\nabc\r\n"} {
		reg.Register(short)
	}
	for _, input := range []string{"ok", "true", "1234567", "short", "        "} {
		res := reg.RedactText(input)
		if res.Hits != 0 || res.Text != input {
			t.Fatalf("short value %q was unexpectedly redacted: %+v", input, res)
		}
	}

	exactMin := synthSecret("min", "8", "byte")
	reg.Register(exactMin)
	res := reg.RedactText("value=" + exactMin + "!")
	if res.Hits != 1 || strings.Contains(res.Text, exactMin) || !slices.Contains(res.Categories, KnownSecretCategory) {
		t.Fatalf("expected 8-byte secret to redact, got %+v", res)
	}
}

func TestRegistryRedactsAllForms(t *testing.T) {
	opaque := synthSecret("opaque", "_", "cred", "_", "alpha9912")
	special := synthSecret(`sec"ret`, `\val`, `<>&/`, "mid", "'end")
	multiline := synthSecret("lineOne", "_secret", "\n", "lineTwo", "_tail")

	reg := NewRegistry()
	reg.Register(opaque, special, multiline)

	jsonSpecial, err := json.Marshal(special)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	var noHTMLBuf bytes.Buffer
	enc := json.NewEncoder(&noHTMLBuf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(special); err != nil {
		t.Fatalf("json.Encode: %v", err)
	}
	jsonNoHTML := strings.TrimSuffix(noHTMLBuf.String(), "\n")

	cases := []struct {
		name   string
		input  string
		forbid []string
	}{
		{
			name:   "opaque raw",
			input:  "before " + opaque + " after",
			forbid: []string{opaque, "alpha9912"},
		},
		{
			name:   "opaque quoted",
			input:  `quoted=` + strconv.Quote(opaque),
			forbid: []string{opaque, "alpha9912"},
		},
		{
			name:   "opaque json",
			input:  `{"v":"` + opaque + `"}`,
			forbid: []string{opaque, "alpha9912"},
		},
		{
			name:   "opaque split by LF",
			input:  "x=" + opaque[:7] + "\n" + opaque[7:] + "!",
			forbid: []string{opaque[:7], opaque[7:]},
		},
		{
			name:   "opaque split by CRLF",
			input:  "x=" + opaque[:5] + "\r\n" + opaque[5:12] + "\r\n" + opaque[12:] + "!",
			forbid: []string{opaque[:5], opaque[5:12], opaque[12:]},
		},
		{
			name:   "special raw",
			input:  "raw:" + special + ":end",
			forbid: []string{special, `sec"ret`, `<>&/`},
		},
		{
			name:   "special go quoted",
			input:  "q:" + strconv.Quote(special),
			forbid: []string{`sec\"ret`, `\\val`},
		},
		{
			name:   "special json escaped",
			input:  `{"item":` + string(jsonSpecial) + `}`,
			forbid: []string{`sec\"ret`, `\u003c\u003e\u0026`},
		},
		{
			name:   "special json no html escape",
			input:  `{"item":` + jsonNoHTML + `}`,
			forbid: []string{`sec\"ret`, `<>&/`},
		},
		{
			name:   "special json escaped and split by CRLF",
			input:  `{"item":` + string(jsonSpecial[:8]) + "\r\n" + string(jsonSpecial[8:]) + `}`,
			forbid: []string{`sec\"ret`, `\u003c\u003e\u0026`},
		},
		{
			name:   "multiline secret with CRLF",
			input:  "hdr\r\n" + strings.ReplaceAll(multiline, "\n", "\r\n") + "\r\nftr",
			forbid: []string{"lineOne_secret", "lineTwo_tail"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := reg.RedactText(tc.input)
			if res.Hits == 0 || !slices.Contains(res.Categories, KnownSecretCategory) {
				t.Fatalf("expected known_secret hit, got %+v", res)
			}
			if !strings.Contains(res.Text, KnownSecretMarker) {
				t.Fatalf("expected marker %q in %q", KnownSecretMarker, res.Text)
			}
			if !reg.ContainsSensitive(tc.input) {
				t.Fatalf("ContainsSensitive(%q) = false, want true", tc.input)
			}
			for _, f := range tc.forbid {
				if strings.Contains(res.Text, f) {
					t.Fatalf("leaked %q in %q", f, res.Text)
				}
			}
		})
	}
}

func TestRegistryRunsBeforePatternFilter(t *testing.T) {
	tok := synthSecret("sc_", "task_", "0123456789abcdef0123456789abcdef")
	reg := NewRegistry()
	reg.Register(tok)

	res := reg.RedactText("lease " + tok + " end")
	if res.Text != "lease "+KnownSecretMarker+" end" {
		t.Fatalf("exact match should replace before pattern filter, got %q", res.Text)
	}
	if !slices.Equal(res.Categories, []string{KnownSecretCategory}) {
		t.Fatalf("categories = %v, want [%s]", res.Categories, KnownSecretCategory)
	}
}

func TestRegistryRegisterBytes(t *testing.T) {
	raw := []byte("0123456789abcdef0123456789abcdef")
	reg := NewRegistry()
	reg.RegisterBytes(raw)

	forms := []string{
		string(raw),
		hex.EncodeToString(raw),
		strings.ToUpper(hex.EncodeToString(raw)),
		base64.StdEncoding.EncodeToString(raw),
		base64.RawStdEncoding.EncodeToString(raw),
		base64.URLEncoding.EncodeToString(raw),
		base64.RawURLEncoding.EncodeToString(raw),
	}
	for _, f := range forms {
		got := reg.RedactString("k=" + f + ";")
		if strings.Contains(got, f) || !strings.Contains(got, KnownSecretMarker) {
			t.Fatalf("RegisterBytes failed to redact encoding %q -> %q", f, got)
		}
	}
}

func TestRegistryNeverExposesValues(t *testing.T) {
	secret := synthSecret("never", "_log_", "this_value_77")
	reg := NewRegistry()
	reg.Register(secret)

	if strings.Contains(fmt.Sprint(reg), secret) {
		t.Fatal("fmt.Sprint exposed secret")
	}
	if strings.Contains(fmt.Sprintf("%#v", reg), secret) {
		t.Fatal("fmt GoString exposed secret")
	}
	jb, err := json.Marshal(reg)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	if strings.Contains(string(jb), secret) {
		t.Fatalf("json.Marshal exposed secret: %s", jb)
	}

	// A Stream buffers the start of a possible match; formatting it, or a
	// struct holding it, must not print those bytes.
	st := reg.NewStream()
	held := secret[:len(secret)-2]
	if out := st.Write([]byte("pre "+held), false); strings.Contains(string(out), held) {
		t.Fatalf("stream emitted a held prefix: %q", out)
	}
	holder := struct{ S *Stream }{st}
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x"} {
		got := fmt.Sprintf(verb, holder)
		if strings.Contains(got, held) || strings.Contains(got, fmt.Sprintf("%x", held)) || strings.Contains(got, fmt.Sprint([]byte(held))) {
			t.Fatalf("formatting a Stream with %s exposed buffered bytes: %s", verb, got)
		}
	}
}

func TestRegistryStreamAcrossAllSplitsAndCRLF(t *testing.T) {
	s1 := synthSecret("stream", "_", "secret", "_", "alpha123")
	s2 := s1 + synthSecret("_ext", "ended99")
	reg := NewRegistry()
	reg.Register(s1, s2)

	inputs := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "raw secret",
			input: "before " + s1 + " after",
			want:  "before " + KnownSecretMarker + " after",
		},
		{
			name:  "longer prefix extension secret",
			input: "before " + s2 + " after",
			want:  "before " + KnownSecretMarker + " after",
		},
		{
			name:  "split by CRLF",
			input: "before " + s1[:8] + "\r\n" + s1[8:] + " after",
			want:  "before " + KnownSecretMarker + " after",
		},
		{
			name:  "clean text without newline passes immediately",
			input: "Continue? [y/N] ",
			want:  "Continue? [y/N] ",
		},
	}

	for _, tc := range inputs {
		t.Run(tc.name, func(t *testing.T) {
			for cut := range len(tc.input) + 1 {
				st := reg.NewStream()
				out := append(st.Write([]byte(tc.input[:cut]), false), st.Write([]byte(tc.input[cut:]), false)...)
				out = append(out, st.Write(nil, true)...)
				if string(out) != tc.want {
					t.Fatalf("cut=%d got %q, want %q", cut, out, tc.want)
				}
			}
			// Byte-by-byte streaming.
			st := reg.NewStream()
			var out []byte
			for i := range len(tc.input) {
				out = append(out, st.Write([]byte{tc.input[i]}, false)...)
			}
			out = append(out, st.Write(nil, true)...)
			if string(out) != tc.want {
				t.Fatalf("byte-by-byte got %q, want %q", out, tc.want)
			}
		})
	}

	// Non-matching prompt emits immediately without waiting for EOF.
	st := reg.NewStream()
	if live := string(st.Write([]byte("Continue? "), false)); live != "Continue? " {
		t.Fatalf("expected immediate passthrough for clean text, got %q", live)
	}
}

func TestRegistryConcurrentRegisterAndRedact(t *testing.T) {
	reg := NewRegistry()
	var wg sync.WaitGroup
	for g := range 8 {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for i := range 25 {
				val := fmt.Sprintf("conc_secret_%02d_%02d_payload", id, i)
				reg.Register(val)
				if got := reg.RedactString("x=" + val); strings.Contains(got, val) {
					t.Errorf("concurrent redact missed %q -> %q", val, got)
				}
				st := reg.NewStream()
				chunk1 := st.Write([]byte("a "+val[:10]+"\r\n"), false)
				chunk2 := st.Write([]byte(val[10:]+" b"), true)
				if got := string(append(chunk1, chunk2...)); strings.Contains(got, val[10:]) {
					t.Errorf("concurrent stream missed %q -> %q", val, got)
				}
			}
		}(g)
	}
	wg.Wait()
}

// Task tokens are minted for every task the daemon runs. Registering each one
// forever would grow the registry and the per-mint automaton rebuild without
// bound, so a value registered with an expiry is dropped once it is past its
// expiry plus ExpiredSecretRetention.
func TestRegistryDropsExpiredValues(t *testing.T) {
	now := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	reg := NewRegistry()
	reg.now = func() time.Time { return now }

	permanent := synthSecret("perm", "_operator_", "value_0001")
	reg.Register(permanent)
	expiring := synthSecret("sc_", "lease_", "expiring_0002")
	reg.RegisterUntil(now.Add(time.Hour), expiring)
	pinned := synthSecret("pin", "_both_", "value_0003")
	reg.Register(pinned)
	reg.RegisterUntil(now.Add(time.Minute), pinned)

	for _, v := range []string{permanent, expiring, pinned} {
		if got := reg.RedactString("x " + v); strings.Contains(got, v) {
			t.Fatalf("value not redacted before expiry: %q", got)
		}
	}

	// Within the retention window the expired value still redacts.
	now = now.Add(time.Hour + ExpiredSecretRetention/2)
	reg.Register(synthSecret("trigger", "_prune_", "0004"))
	if got := reg.RedactString("x " + expiring); strings.Contains(got, expiring) {
		t.Fatalf("value dropped inside retention window: %q", got)
	}

	// Past expiry plus retention, the next registration prunes it.
	now = now.Add(ExpiredSecretRetention)
	reg.Register(synthSecret("trigger", "_prune_", "0005"))
	if got := reg.RedactString("x " + expiring); !strings.Contains(got, expiring) {
		t.Fatalf("expired value still registered: %q", got)
	}
	for _, v := range []string{permanent, pinned} {
		if got := reg.RedactString("x " + v); strings.Contains(got, v) {
			t.Fatalf("permanent value dropped: %q", got)
		}
	}
	if n := reg.Len(); n != 4 {
		t.Fatalf("registry holds %d values, want 4", n)
	}
}

func TestRegistryBoundedUnderTokenChurn(t *testing.T) {
	now := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	reg := NewRegistry()
	reg.now = func() time.Time { return now }
	for i := range 500 {
		reg.RegisterUntil(now.Add(time.Hour), fmt.Sprintf("sc_task_churn_%064d", i))
		now = now.Add(time.Minute)
	}
	// Only tokens expiring within the last ExpiredSecretRetention survive.
	limit := int((time.Hour+ExpiredSecretRetention)/time.Minute) + 1
	if n := reg.Len(); n > limit {
		t.Fatalf("registry holds %d values after churn, want at most %d", n, limit)
	}
}

// A heartbeat renews a task lease past its minted expiry. The token must stay
// registered while the renewed lease is live, so ExtendOwned moves the expiry
// of every value registered for that owner.
func TestRegistryExtendOwnedKeepsRenewedLeaseRegistered(t *testing.T) {
	now := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	reg := NewRegistry()
	reg.now = func() time.Time { return now }

	token := synthSecret("sc_", "lease_", "renewed_0001")
	other := synthSecret("sc_", "lease_", "other_0002")
	reg.RegisterOwnedUntil("task-1", now.Add(time.Hour), token)
	reg.RegisterOwnedUntil("task-2", now.Add(time.Hour), other)

	// Renewals every few minutes keep task-1 alive for three hours.
	for range 36 {
		now = now.Add(5 * time.Minute)
		reg.ExtendOwned("task-1", now.Add(time.Hour))
	}
	reg.Register(synthSecret("trigger", "_prune_", "0003"))
	if got := reg.RedactString("x " + token); strings.Contains(got, token) {
		t.Fatalf("renewed lease token was pruned while live: %q", got)
	}
	if got := reg.RedactString("x " + other); !strings.Contains(got, other) {
		t.Fatalf("task-2 token should be pruned after its retention: %q", got)
	}

	// Once renewals stop, the token is dropped after its last renewed expiry
	// plus the retention, like any other expiring value.
	lastExpiry := now.Add(time.Hour)
	now = lastExpiry.Add(ExpiredSecretRetention - time.Minute)
	reg.Register(synthSecret("trigger", "_prune_", "0004"))
	if got := reg.RedactString("x " + token); strings.Contains(got, token) {
		t.Fatalf("renewed lease token was pruned before its retention ended: %q", got)
	}
	now = lastExpiry.Add(ExpiredSecretRetention + time.Minute)
	reg.Register(synthSecret("trigger", "_prune_", "0005"))
	if got := reg.RedactString("x " + token); !strings.Contains(got, token) {
		t.Fatalf("renewed lease token outlived its renewed expiry and retention: %q", got)
	}
}

// RedactExact must not touch text that holds no registered value, even when
// pattern rules would rewrite it.
func TestRegistryRedactExactLeavesOtherTextAlone(t *testing.T) {
	reg := NewRegistry()
	held := synthSecret("exact", "_only_", "value_0042")
	reg.Register(held)
	in := `{"body":"password=hunter2-value\"}","x":"` + held + `"}`
	got := reg.RedactExact(in)
	want := strings.Replace(in, held, KnownSecretMarker, 1)
	if got != want {
		t.Fatalf("RedactExact = %q, want %q", got, want)
	}
}
