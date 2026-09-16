package auth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v4"
)

func TestLocalMiddlewareUsesFixedIdentity(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(LocalMiddleware())
	router.GET("/", func(c *gin.Context) { c.String(http.StatusOK, Identity(c)) })
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.Header.Set("Authorization", "Bearer attacker")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusOK || response.Body.String() != LocalUserID {
		t.Fatalf("status=%d identity=%q", response.Code, response.Body.String())
	}
}

func TestVerifierChecksSignatureIssuerAudienceAndExpiry(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	var issuer string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			json.NewEncoder(w).Encode(map[string]string{"issuer": issuer, "jwks_uri": issuer + "/keys"})
		case "/keys":
			json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{"kid": "test", "kty": "RSA", "alg": "RS256", "n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()), "e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes())}}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	issuer = server.URL
	v, err := NewVerifier(context.Background(), issuer, "agentgo")
	if err != nil {
		t.Fatal(err)
	}
	sign := func(claims jwt.MapClaims) string {
		token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
		token.Header["kid"] = "test"
		raw, err := token.SignedString(key)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	base := jwt.MapClaims{"iss": issuer, "aud": "agentgo", "sub": "alice", "exp": time.Now().Add(time.Minute).Unix()}
	id, err := v.Verify(context.Background(), sign(base))
	if err != nil || id != UserID(issuer, "alice") {
		t.Fatalf("id=%q err=%v", id, err)
	}
	attacker, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	forged := jwt.NewWithClaims(jwt.SigningMethodRS256, base)
	forged.Header["kid"] = "test"
	forgedRaw, err := forged.SignedString(attacker)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := v.Verify(context.Background(), forgedRaw); err == nil {
		t.Fatal("accepted forged signature")
	}
	for _, claims := range []jwt.MapClaims{
		{"iss": "other", "aud": "agentgo", "sub": "alice", "exp": time.Now().Add(time.Minute).Unix()},
		{"iss": issuer, "aud": "other", "sub": "alice", "exp": time.Now().Add(time.Minute).Unix()},
		{"iss": issuer, "aud": "agentgo", "sub": "alice", "exp": time.Now().Add(-time.Minute).Unix()},
		{"iss": issuer, "aud": "agentgo", "sub": "alice"},
	} {
		if _, err := v.Verify(context.Background(), sign(claims)); err == nil {
			t.Fatalf("accepted claims=%v", claims)
		}
	}
}
