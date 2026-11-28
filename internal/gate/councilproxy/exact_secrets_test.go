package councilproxy

import (
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/shadewright-labs/sablecourt/internal/redact"
)

func TestNewLaunchKeyRegistersSigningMaterial(t *testing.T) {
	priv, err := newLaunchKey()
	if err != nil {
		t.Fatalf("newLaunchKey: %v", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatalf("MarshalPKCS8PrivateKey: %v", err)
	}
	scalar, err := priv.Bytes()
	if err != nil {
		t.Fatalf("PrivateKey.Bytes: %v", err)
	}
	dHex := hex.EncodeToString(scalar)
	derB64 := base64.StdEncoding.EncodeToString(der)
	for _, sample := range []string{dHex, derB64} {
		if got := redact.String("k=" + sample); strings.Contains(got, sample) {
			t.Fatalf("launch signing key encoding leaked: %q", got)
		}
	}
}
