package github

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestJWTIsSignedByTheAppKey(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "app.pem")
	os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}), 0o600)

	jwt, err := (&Client{AppID: 42, KeyFile: path}).jwt()
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		t.Fatalf("jwt: %s", jwt)
	}
	enc := base64.RawURLEncoding
	sig, _ := enc.DecodeString(parts[2])
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, sum[:], sig); err != nil {
		t.Fatalf("signature: %v", err)
	}
	var claims struct {
		Iss string `json:"iss"`
		Iat int64  `json:"iat"`
		Exp int64  `json:"exp"`
	}
	data, _ := enc.DecodeString(parts[1])
	json.Unmarshal(data, &claims)
	if claims.Iss != "42" || claims.Exp-claims.Iat > 600 {
		t.Errorf("claims: %+v", claims)
	}
}

func TestLatestKeepsEachChecksLastRun(t *testing.T) {
	got := Latest([]CheckRun{
		{ID: 5, Name: "test", Conclusion: "success"},
		{ID: 2, Name: "lint", Conclusion: "failure"},
		{ID: 3, Name: "test", Conclusion: "failure"},
		{ID: 9, Name: "lint", Conclusion: "success"},
	})
	if len(got) != 2 || got[0].ID != 5 || got[1].ID != 9 {
		t.Errorf("latest: %+v", got)
	}
}
