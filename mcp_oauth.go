// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// MCPAuthKind is what ProbeMCP can decide from one unauthenticated request.
type MCPAuthKind string

const (
	MCPAuthOpen   MCPAuthKind = "open"
	MCPAuthStatic MCPAuthKind = "static"
	MCPAuthOAuth  MCPAuthKind = "oauth"
)

// MCPProbe is the classification of one HTTP MCP URL.
type MCPProbe struct {
	Kind                 MCPAuthKind
	URL                  string
	Status               int
	WWWAuthenticate      string
	ResourceMetadata     string
	AuthorizationServers []string
	Scopes               []string
}

// MCPToken is the result of an owner-present authorization. Claudia
// does not persist it (🎯T42). Refresh-without-the-owner is jevons 🎯T520.
type MCPToken struct {
	AccessToken  string
	RefreshToken string
	TokenType    string
	ExpiresIn    int
}

// AuthorizeMCPArgs drives [AuthorizeMCP]. Nil OpenURL uses the platform
// browser (`open` / `xdg-open`). Tests inject OpenURL and HTTPClient.
type AuthorizeMCPArgs struct {
	URL        string
	Probe      *MCPProbe
	OpenURL    func(string) error
	Client     *http.Client
	ClientName string
}

// ProbeMCP classifies an HTTP MCP endpoint. A 2xx initialize is open. A
// 401 with resource_metadata is oauth. Any other 401 is static-auth.
func ProbeMCP(ctx context.Context, rawURL string) (*MCPProbe, error) {
	return probeMCP(ctx, http.DefaultClient, rawURL)
}

func probeMCP(ctx context.Context, client *http.Client, rawURL string) (*MCPProbe, error) {
	if client == nil {
		client = http.DefaultClient
	}
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return nil, fmt.Errorf("probe mcp: url required")
	}
	body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"claudia","version":"` + Version + `"}}}`
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rawURL, strings.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("probe mcp: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("probe mcp: %w", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	p := &MCPProbe{URL: rawURL, Status: resp.StatusCode, WWWAuthenticate: resp.Header.Get("WWW-Authenticate")}
	meta := parseResourceMetadata(p.WWWAuthenticate)
	p.ResourceMetadata = meta
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		p.Kind = MCPAuthOpen
	case resp.StatusCode == http.StatusUnauthorized && meta != "":
		p.Kind = MCPAuthOAuth
		if prm, err := fetchPRM(ctx, client, meta); err == nil {
			p.AuthorizationServers = prm.AuthorizationServers
			p.Scopes = prm.ScopesSupported
		}
	case resp.StatusCode == http.StatusUnauthorized:
		p.Kind = MCPAuthStatic
	default:
		return p, fmt.Errorf("probe mcp: unexpected status %d", resp.StatusCode)
	}
	return p, nil
}

// AuthorizeMCP runs owner-present PKCE OAuth for an HTTP MCP server.
// It opens the authorization URL in the browser, waits for the local
// redirect, and returns tokens. It does not store them.
func AuthorizeMCP(ctx context.Context, args *AuthorizeMCPArgs) (*MCPToken, error) {
	if args == nil || strings.TrimSpace(args.URL) == "" {
		return nil, fmt.Errorf("authorize mcp: url required")
	}
	client := args.Client
	if client == nil {
		client = http.DefaultClient
	}
	probe := args.Probe
	if probe == nil {
		var err error
		probe, err = probeMCP(ctx, client, args.URL)
		if err != nil {
			return nil, err
		}
	}
	if probe.Kind != MCPAuthOAuth {
		return nil, fmt.Errorf("authorize mcp: probe is %s, not oauth", probe.Kind)
	}
	if probe.ResourceMetadata == "" {
		return nil, fmt.Errorf("authorize mcp: no resource_metadata")
	}
	prm, err := fetchPRM(ctx, client, probe.ResourceMetadata)
	if err != nil {
		return nil, err
	}
	as, err := discoverAuthServer(ctx, client, args.URL, prm)
	if err != nil {
		return nil, err
	}
	if as.AuthorizationEndpoint == "" || as.TokenEndpoint == "" {
		return nil, fmt.Errorf("authorize mcp: AS missing authorize/token")
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("authorize mcp: listen: %w", err)
	}
	redirect := "http://" + ln.Addr().String() + "/callback"

	clientID := ""
	clientSecret := ""
	if as.RegistrationEndpoint != "" {
		reg, err := registerOAuthClient(ctx, client, as.RegistrationEndpoint, redirect, args.ClientName)
		if err != nil {
			ln.Close()
			return nil, fmt.Errorf("authorize mcp: DCR: %w", err)
		}
		clientID = reg.ClientID
		clientSecret = reg.ClientSecret
	}
	if clientID == "" {
		ln.Close()
		return nil, fmt.Errorf("authorize mcp: no client_id (DCR unavailable)")
	}

	verifier, challenge, err := pkceS256()
	if err != nil {
		ln.Close()
		return nil, err
	}
	state, err := randomB64(16)
	if err != nil {
		ln.Close()
		return nil, err
	}

	authURL, err := url.Parse(as.AuthorizationEndpoint)
	if err != nil {
		ln.Close()
		return nil, err
	}
	q := authURL.Query()
	q.Set("response_type", "code")
	q.Set("client_id", clientID)
	q.Set("redirect_uri", redirect)
	q.Set("code_challenge", challenge)
	q.Set("code_challenge_method", "S256")
	q.Set("state", state)
	q.Set("resource", strings.TrimSpace(args.URL))
	if len(prm.ScopesSupported) > 0 {
		q.Set("scope", strings.Join(prm.ScopesSupported, " "))
	}
	authURL.RawQuery = q.Encode()

	codeCh := make(chan string, 1)
	errCh := make(chan error, 1)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("state") != state {
			http.Error(w, "state mismatch", http.StatusBadRequest)
			select {
			case errCh <- fmt.Errorf("authorize mcp: state mismatch"):
			default:
			}
			return
		}
		if e := r.URL.Query().Get("error"); e != "" {
			http.Error(w, e, http.StatusBadRequest)
			select {
			case errCh <- fmt.Errorf("authorize mcp: %s", e):
			default:
			}
			return
		}
		code := r.URL.Query().Get("code")
		if code == "" {
			http.Error(w, "missing code", http.StatusBadRequest)
			select {
			case errCh <- fmt.Errorf("authorize mcp: missing code"):
			default:
			}
			return
		}
		io.WriteString(w, "Authorization complete. You can close this tab.")
		select {
		case codeCh <- code:
		default:
		}
	})}
	go srv.Serve(ln)
	defer func() {
		shctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		srv.Shutdown(shctx)
	}()

	open := args.OpenURL
	if open == nil {
		open = openBrowser
	}
	if err := open(authURL.String()); err != nil {
		return nil, fmt.Errorf("authorize mcp: open browser: %w", err)
	}

	var code string
	select {
	case <-ctx.Done():
		return nil, fmt.Errorf("authorize mcp: %w", ctx.Err())
	case err := <-errCh:
		return nil, err
	case code = <-codeCh:
	}

	return exchangeCode(ctx, client, as.TokenEndpoint, clientID, clientSecret, redirect, code, verifier, args.URL)
}

type prmDoc struct {
	Resource             string   `json:"resource"`
	AuthorizationServers []string `json:"authorization_servers"`
	ScopesSupported      []string `json:"scopes_supported"`
}

type asDoc struct {
	Issuer                string `json:"issuer"`
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
	RegistrationEndpoint  string `json:"registration_endpoint"`
}

type dcrDoc struct {
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
}

func parseResourceMetadata(www string) string {
	// Bearer resource_metadata="https://…", error="invalid_token"
	low := www
	const key = "resource_metadata="
	i := strings.Index(strings.ToLower(low), key)
	if i < 0 {
		return ""
	}
	rest := strings.TrimSpace(www[i+len(key):])
	if rest == "" {
		return ""
	}
	if rest[0] == '"' {
		rest = rest[1:]
		if j := strings.IndexByte(rest, '"'); j >= 0 {
			return rest[:j]
		}
	}
	if j := strings.IndexAny(rest, " ,;"); j >= 0 {
		return rest[:j]
	}
	return rest
}

func fetchPRM(ctx context.Context, client *http.Client, rawURL string) (*prmDoc, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("prm: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("prm: status %d", resp.StatusCode)
	}
	var doc prmDoc
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return nil, fmt.Errorf("prm: %w", err)
	}
	return &doc, nil
}

func discoverAuthServer(ctx context.Context, client *http.Client, mcpURL string, prm *prmDoc) (*asDoc, error) {
	var candidates []string
	for _, as := range prm.AuthorizationServers {
		candidates = append(candidates,
			strings.TrimRight(as, "/")+"/.well-known/oauth-authorization-server",
			wellKnownInsert(as, "oauth-authorization-server"),
			wellKnownInsert(as, "openid-configuration"),
		)
	}
	if u, err := url.Parse(mcpURL); err == nil {
		origin := u.Scheme + "://" + u.Host
		candidates = append(candidates, origin+"/.well-known/oauth-authorization-server")
	}
	var last error
	var fallback *asDoc
	for _, c := range candidates {
		if c == "" {
			continue
		}
		doc, err := fetchAS(ctx, client, c)
		if err != nil {
			last = err
			continue
		}
		if doc.RegistrationEndpoint != "" {
			return doc, nil
		}
		if fallback == nil {
			fallback = doc
		}
	}
	if fallback != nil {
		return fallback, nil
	}
	if last != nil {
		return nil, fmt.Errorf("authorize mcp: AS metadata: %w", last)
	}
	return nil, fmt.Errorf("authorize mcp: no authorization server")
}

func wellKnownInsert(issuer, name string) string {
	u, err := url.Parse(issuer)
	if err != nil || u.Host == "" {
		return ""
	}
	path := strings.TrimSuffix(u.Path, "/")
	u.Path = "/.well-known/" + name + path
	return u.String()
}

func fetchAS(ctx context.Context, client *http.Client, rawURL string) (*asDoc, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d from %s", resp.StatusCode, rawURL)
	}
	var doc asDoc
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return nil, err
	}
	if doc.AuthorizationEndpoint == "" {
		return nil, fmt.Errorf("no authorization_endpoint in %s", rawURL)
	}
	return &doc, nil
}

func registerOAuthClient(ctx context.Context, client *http.Client, endpoint, redirect, name string) (*dcrDoc, error) {
	if name == "" {
		name = "claudia"
	}
	payload, err := json.Marshal(map[string]any{
		"client_name":                name,
		"redirect_uris":              []string{redirect},
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"response_types":             []string{"code"},
		"token_endpoint_auth_method": "none",
	})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(string(payload)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("status %d: %s", resp.StatusCode, truncate(string(b), 200))
	}
	var doc dcrDoc
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, err
	}
	if doc.ClientID == "" {
		return nil, fmt.Errorf("DCR returned no client_id")
	}
	return &doc, nil
}

func exchangeCode(ctx context.Context, client *http.Client, tokenURL, clientID, clientSecret, redirect, code, verifier, resource string) (*MCPToken, error) {
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("redirect_uri", redirect)
	form.Set("client_id", clientID)
	form.Set("code_verifier", verifier)
	if resource != "" {
		form.Set("resource", resource)
	}
	if clientSecret != "" {
		form.Set("client_secret", clientSecret)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("token: status %d: %s", resp.StatusCode, truncate(string(b), 200))
	}
	var out struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		TokenType    string `json:"token_type"`
		ExpiresIn    int    `json:"expires_in"`
	}
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	if out.AccessToken == "" {
		return nil, fmt.Errorf("token: empty access_token")
	}
	return &MCPToken{
		AccessToken:  out.AccessToken,
		RefreshToken: out.RefreshToken,
		TokenType:    out.TokenType,
		ExpiresIn:    out.ExpiresIn,
	}, nil
}

func pkceS256() (verifier, challenge string, err error) {
	verifier, err = randomB64(32)
	if err != nil {
		return "", "", err
	}
	sum := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(sum[:])
	return verifier, challenge, nil
}

func randomB64(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func openBrowser(rawURL string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", rawURL)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", rawURL)
	default:
		cmd = exec.Command("xdg-open", rawURL)
	}
	return cmd.Start()
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
