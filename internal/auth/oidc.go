package auth

import (
	"context"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v4"
)

const identityKey = "authenticated_user_id"

const LocalUserID = "local-user"

// LocalMiddleware assigns one shared identity when OIDC is disabled.
func LocalMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		SetIdentity(c, LocalUserID)
		c.Next()
	}
}

type Verifier struct {
	issuer, audience, jwksURL string
	client                    *http.Client
	mu                        sync.RWMutex
	keys                      map[string]*rsa.PublicKey
}

func NewVerifier(ctx context.Context, issuer, audience string) (*Verifier, error) {
	if issuer == "" || audience == "" {
		return nil, errors.New("OIDC issuer 和 audience 必须配置")
	}
	issuerURL, err := url.Parse(issuer)
	if err != nil || issuerURL.Host == "" || (issuerURL.Scheme != "https" && !(issuerURL.Scheme == "http" && (issuerURL.Hostname() == "localhost" || issuerURL.Hostname() == "127.0.0.1"))) {
		return nil, errors.New("OIDC issuer 必须使用 HTTPS（本机开发环境除外）")
	}
	v := &Verifier{issuer: issuer, audience: audience, client: &http.Client{Timeout: 5 * time.Second}}
	var document struct {
		Issuer  string `json:"issuer"`
		JWKSURI string `json:"jwks_uri"`
	}
	if err := v.getJSON(ctx, strings.TrimRight(issuer, "/")+"/.well-known/openid-configuration", &document); err != nil {
		return nil, fmt.Errorf("读取 OIDC discovery 失败: %w", err)
	}
	if document.Issuer != issuer || document.JWKSURI == "" {
		return nil, errors.New("OIDC discovery issuer 不一致或缺少 jwks_uri")
	}
	jwksURL, err := url.Parse(document.JWKSURI)
	if err != nil || (jwksURL.Scheme != "https" && !(issuerURL.Scheme == "http" && jwksURL.Host == issuerURL.Host)) {
		return nil, errors.New("OIDC jwks_uri 必须使用 HTTPS")
	}
	v.jwksURL = document.JWKSURI
	if err := v.refresh(ctx); err != nil {
		return nil, err
	}
	return v, nil
}

func (v *Verifier) getJSON(ctx context.Context, url string, target any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := v.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(target)
}

func (v *Verifier) refresh(ctx context.Context) error {
	var data struct {
		Keys []struct{ KID, KTY, ALG, Use, N, E string } `json:"keys"`
	}
	if err := v.getJSON(ctx, v.jwksURL, &data); err != nil {
		return fmt.Errorf("读取 JWKS 失败: %w", err)
	}
	keys := make(map[string]*rsa.PublicKey)
	for _, key := range data.Keys {
		if key.KTY != "RSA" || (key.ALG != "" && key.ALG != "RS256") || (key.Use != "" && key.Use != "sig") || key.KID == "" {
			continue
		}
		n, nerr := base64.RawURLEncoding.DecodeString(key.N)
		e, eerr := base64.RawURLEncoding.DecodeString(key.E)
		if nerr != nil || eerr != nil || len(n) < 256 || len(e) == 0 || len(e) > 4 {
			continue
		}
		exponent := new(big.Int).SetBytes(e).Int64()
		if exponent < 3 || exponent > 1<<31-1 {
			continue
		}
		keys[key.KID] = &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(exponent)}
	}
	if len(keys) == 0 {
		return errors.New("JWKS 中没有可用的 RS256 公钥")
	}
	v.mu.Lock()
	v.keys = keys
	v.mu.Unlock()
	return nil
}

func (v *Verifier) Verify(ctx context.Context, tokenString string) (string, error) {
	claims := jwt.MapClaims{}
	token, err := jwt.NewParser(jwt.WithValidMethods([]string{"RS256"})).ParseWithClaims(tokenString, claims, func(token *jwt.Token) (any, error) {
		kid, ok := token.Header["kid"].(string)
		if !ok || kid == "" {
			return nil, errors.New("JWT 缺少 kid")
		}
		v.mu.RLock()
		key := v.keys[kid]
		v.mu.RUnlock()
		if key == nil {
			if err := v.refresh(ctx); err != nil {
				return nil, err
			}
			v.mu.RLock()
			key = v.keys[kid]
			v.mu.RUnlock()
		}
		if key == nil {
			return nil, errors.New("未知 JWT kid")
		}
		return key, nil
	})
	if err != nil || !token.Valid || !claims.VerifyIssuer(v.issuer, true) || !claims.VerifyAudience(v.audience, true) || !claims.VerifyExpiresAt(time.Now().Unix(), true) {
		return "", errors.New("无效的访问令牌")
	}
	sub, ok := claims["sub"].(string)
	if !ok || sub == "" || len(sub) > 512 {
		return "", errors.New("访问令牌缺少 sub")
	}
	return UserID(v.issuer, sub), nil
}

func (v *Verifier) Middleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		auth := c.GetHeader("Authorization")
		if !strings.HasPrefix(auth, "Bearer ") {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "缺少 Bearer token"})
			return
		}
		id, err := v.Verify(c.Request.Context(), strings.TrimSpace(strings.TrimPrefix(auth, "Bearer ")))
		if err != nil {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "无效的访问令牌"})
			return
		}
		c.Set(identityKey, id)
		c.Next()
	}
}

func Identity(c *gin.Context) string {
	value, _ := c.Get(identityKey)
	id, _ := value.(string)
	return id
}

// SetIdentity lets trusted middleware install a verified identity.
func SetIdentity(c *gin.Context, id string) { c.Set(identityKey, id) }

func UserID(issuer, subject string) string {
	sum := sha256.Sum256([]byte(issuer + "\x00" + subject))
	return hex.EncodeToString(sum[:16])
}
