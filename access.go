package main

// Cloudflare Access JWT validation (fail closed).
// The only ingress to this server is the Cloudflare tunnel, and Access sits in
// front of the hostname at the edge. This middleware proves each request
// actually passed Access by verifying the JWT Cloudflare injects
// (Cf-Access-Jwt-Assertion header / CF_Authorization cookie) against the
// team's public keys, so a misconfigured or removed Access app cannot silently
// expose the chat.

import (
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"
)

type AccessAuth struct {
	team   string
	aud    string
	client *http.Client

	mu    sync.Mutex
	keys  map[string]*rsa.PublicKey
	fetch time.Time
}

type jwk struct {
	Kid string `json:"kid"`
	Kty string `json:"kty"`
	Alg string `json:"alg"`
	N   string `json:"n"`
	E   string `json:"e"`
}

func NewAccessAuth(team, aud string) *AccessAuth {
	return &AccessAuth{team: team, aud: aud, client: &http.Client{Timeout: 15 * time.Second}}
}

func (a *AccessAuth) fetchKeys(force bool) (map[string]*rsa.PublicKey, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !force && len(a.keys) > 0 && time.Since(a.fetch) < 12*time.Hour {
		return a.keys, nil
	}
	url := "https://" + a.team + ".cloudflareaccess.com/cdn-cgi/access/certs"
	resp, err := a.client.Get(url)
	if err != nil {
		return a.keys, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return a.keys, fmt.Errorf("access certs endpoint returned %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return a.keys, err
	}
	var cr struct {
		Keys []jwk `json:"keys"`
	}
	if err := json.Unmarshal(body, &cr); err != nil {
		return a.keys, err
	}
	keys := map[string]*rsa.PublicKey{}
	for _, k := range cr.Keys {
		if k.Alg != "RS256" || k.Kty != "RSA" {
			continue
		}
		nb, err1 := base64.RawURLEncoding.DecodeString(k.N)
		eb, err2 := base64.RawURLEncoding.DecodeString(k.E)
		if err1 != nil || err2 != nil || len(nb) == 0 {
			continue
		}
		e := 0
		for _, b := range eb {
			e = e<<8 | int(b)
		}
		if e == 0 {
			e = 65537
		}
		keys[k.Kid] = &rsa.PublicKey{N: new(big.Int).SetBytes(nb), E: e}
	}
	if len(keys) == 0 {
		return a.keys, errors.New("no usable keys in access certs response")
	}
	a.keys = keys
	a.fetch = time.Now()
	return keys, nil
}

func decodeSeg(seg string, v any) error {
	b, err := base64.RawURLEncoding.DecodeString(seg)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

// Validate checks an Access JWT and returns the authenticated email.
func (a *AccessAuth) Validate(token string) (string, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return "", errors.New("malformed token")
	}
	var hdr struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
	}
	if err := decodeSeg(parts[0], &hdr); err != nil {
		return "", errors.New("bad token header")
	}
	if hdr.Alg != "RS256" {
		return "", fmt.Errorf("unsupported alg %q", hdr.Alg)
	}
	keys, err := a.fetchKeys(false)
	if err != nil {
		return "", err
	}
	pub := keys[hdr.Kid]
	if pub == nil {
		if keys, err = a.fetchKeys(true); err != nil {
			return "", err
		}
		pub = keys[hdr.Kid]
	}
	if pub == nil {
		return "", fmt.Errorf("no key for kid %q", hdr.Kid)
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return "", errors.New("bad token signature encoding")
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(pub, crypto.SHA256, sum[:], sig); err != nil {
		return "", errors.New("invalid token signature")
	}
	var claims struct {
		Iss    string          `json:"iss"`
		Aud    json.RawMessage `json:"aud"`
		Exp    float64         `json:"exp"`
		Email  string          `json:"email"`
		Common string          `json:"common_name"`
	}
	if err := decodeSeg(parts[1], &claims); err != nil {
		return "", errors.New("bad token claims")
	}
	wantIss := "https://" + a.team + ".cloudflareaccess.com"
	if strings.TrimSuffix(claims.Iss, "/") != wantIss {
		return "", fmt.Errorf("unexpected issuer %q", claims.Iss)
	}
	if a.aud == "" || a.aud == "-" {
		return "", errors.New("missing access AUD configuration")
	}
	var auds []string
	if err := json.Unmarshal(claims.Aud, &auds); err != nil {
		var one string
		if err2 := json.Unmarshal(claims.Aud, &one); err2 == nil && one != "" {
			auds = []string{one}
		}
	}
	ok := false
	for _, x := range auds {
		if x == a.aud {
			ok = true
			break
		}
	}
	if !ok {
		return "", fmt.Errorf("token aud %v does not match %q", auds, a.aud)
	}
	if claims.Exp > 0 && time.Now().Add(-time.Minute).After(time.Unix(int64(claims.Exp), 0)) {
		return "", errors.New("token expired")
	}
	email := claims.Email
	if email == "" {
		email = claims.Common
	}
	if email == "" {
		return "", errors.New("token carries no identity")
	}
	return email, nil
}
