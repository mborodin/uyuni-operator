package uyuni

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"sync"
	"time"

	uyuniapi "github.com/uyuni-project/uyuni-tools/shared/api"
)

// Client is a concrete uyuni.API backed by the Uyuni REST JSON API at
// /rhn/manager/api/. It uses uyuni-tools/shared/api for HTTP transport,
// TLS configuration, and pxt-session-cookie session management.
// On 401 responses it re-authenticates transparently before retrying.
//
// The Pool caches one Client per org/provider and hands it out to every
// org-scoped reconciler, so concurrent reconciles (e.g. a cascade delete
// touching ContentProject, ClmEnvironment, SoftwareChannel, SystemGroup,
// ActivationKey at once) share a single session cookie jar. Without mu,
// concurrent 401-triggered re-logins race: one goroutine's fresh session
// cookie gets clobbered by another's in-flight login, producing a storm of
// spurious 401s that never self-resolves. mu serializes every request
// through this client so re-auth can't interleave.
type Client struct {
	mu      sync.Mutex
	conn    *uyuniapi.ConnectionDetails
	http    *uyuniapi.APIClient
	baseURL string // Full server URL for direct HTTP calls

	// rawHTTP backs apiDelete/apiDeleteWithBody, used only by Uyuni's
	// contentmanagement project/environment removal endpoints,
	// which need a real HTTP DELETE that the uyuniapi library doesn't expose.
	// uyuniapi.APIClient.Client is an opaque library type (api.HTTPClient),
	// not a plain *http.Client, so its Transport/cookie jar aren't reachable
	// from here to reuse its session. rawHTTP is a fully self-contained
	// *http.Client with its own cookiejar instead: rawLogin() authenticates
	// directly against /auth/login and the jar then attaches the resulting
	// session cookie to every subsequent request automatically, exactly like
	// the curl -c/-b cookie-jar flow that was used to verify these endpoints
	// work fine when the session cookie is actually sent.
	rawHTTP *http.Client
}

// notFoundError wraps a "not found" response so callers can use IsNotFound.
type notFoundError struct{ msg string }

func (e *notFoundError) Error() string  { return e.msg }
func (e *notFoundError) notFound() bool { return true }

// NewClient creates a Client connected and authenticated to the Uyuni server.
// rawURL is the full server URL (e.g. "https://uyuni.example.com").
// caCert is an optional PEM CA certificate read from the provider's Secret.
func NewClient(rawURL, username, password string, insecure bool, caCert []byte) (*Client, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("parsing provider URL %q: %w", rawURL, err)
	}

	conn := &uyuniapi.ConnectionDetails{
		Server:   parsed.Hostname(),
		User:     username,
		Password: password,
		Insecure: insecure,
	}

	// api.Init configures TLS from a file path; when we have cert bytes we
	// replace the transport after Init so we never touch the filesystem.
	httpClient, err := uyuniapi.Init(conn)
	if err != nil {
		return nil, fmt.Errorf("initializing Uyuni HTTP client: %w", err)
	}
	if len(caCert) > 0 {
		pool, _ := x509.SystemCertPool()
		pool.AppendCertsFromPEM(caCert)
		jar, _ := cookiejar.New(nil)
		httpClient.Client = &http.Client{
			Timeout: time.Minute,
			Jar:     jar, // Important: Cookie jar for session persistence
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{
					RootCAs:            pool,
					InsecureSkipVerify: insecure, //nolint:gosec
				},
			},
		}
	}

	if err := httpClient.Login(); err != nil {
		return nil, fmt.Errorf("authenticating with %s: %w", rawURL, err)
	}

	tlsConfig := &tls.Config{InsecureSkipVerify: insecure} //nolint:gosec
	if len(caCert) > 0 {
		pool, _ := x509.SystemCertPool()
		if pool == nil {
			pool = x509.NewCertPool()
		}
		pool.AppendCertsFromPEM(caCert)
		tlsConfig.RootCAs = pool
	}
	rawJar, _ := cookiejar.New(nil)
	rawHTTP := &http.Client{
		Timeout: time.Minute,
		Jar:     rawJar,
		Transport: &http.Transport{
			TLSClientConfig: tlsConfig,
		},
	}

	c := &Client{conn: conn, http: httpClient, baseURL: strings.TrimSuffix(rawURL, "/"), rawHTTP: rawHTTP}
	if err := c.rawLogin(); err != nil {
		return nil, fmt.Errorf("authenticating raw HTTP client with %s: %w", rawURL, err)
	}
	return c, nil
}

// rawLogin authenticates rawHTTP directly against /auth/login. The resulting
// session cookie lands in rawHTTP's own cookiejar and is then attached
// automatically by the standard library to every subsequent request made
// through rawHTTP, for the same reason curl -c/-b cookie-jar round-trips
// work: a real, jar-backed client, talking to the server with its own
// session, rather than borrowing one from a different client.
func (c *Client) rawLogin() error {
	body, err := json.Marshal(map[string]string{"login": c.conn.User, "password": c.conn.Password})
	if err != nil {
		return fmt.Errorf("marshaling login body: %w", err)
	}
	req, err := http.NewRequest("POST", c.baseURL+"/rhn/manager/api/auth/login", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("creating login request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.rawHTTP.Do(req)
	if err != nil {
		return fmt.Errorf("login request failed: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	var apiResp struct {
		Success bool `json:"success"`
	}
	if err := json.Unmarshal(respBody, &apiResp); err != nil {
		return fmt.Errorf("parsing login response: %w", err)
	}
	if !apiResp.Success {
		return fmt.Errorf("login failed: %s", string(respBody))
	}
	return nil
}

// --- generic re-auth helpers ---

// apiGet calls GET <path> and re-authenticates once on 401 or 403.
// Uyuni returns 403 HTML for both genuine permission denials and expired
// sessions; we try re-auth on either so that a stale cookie is recovered
// transparently. If the retry also fails with 403 the error is reported as-is.
func apiGet[T any](c *Client, path string) (T, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	resp, err := uyuniapi.Get[T](c.http, path)
	if err != nil && needsReauth(err.Error()) {
		if loginErr := c.http.Login(); loginErr != nil {
			var z T
			return z, loginErr
		}
		resp, err = uyuniapi.Get[T](c.http, path)
	}
	if err != nil {
		var z T
		return z, sanitizeHTTPError(err, path)
	}
	if !resp.Success {
		var z T
		return z, fmt.Errorf("%s", resp.Message)
	}
	return resp.Result, nil
}

// apiPost calls POST <path> with JSON body and re-authenticates once on 401 or 403.
func apiPost[T any](c *Client, path string, data map[string]any) (T, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	resp, err := uyuniapi.Post[T](c.http, path, data)
	if err != nil && needsReauth(err.Error()) {
		if loginErr := c.http.Login(); loginErr != nil {
			var z T
			return z, loginErr
		}
		resp, err = uyuniapi.Post[T](c.http, path, data)
	}
	if err != nil {
		var z T
		return z, sanitizeHTTPError(err, path)
	}
	if !resp.Success {
		var z T
		return z, fmt.Errorf("%s", resp.Message)
	}
	return resp.Result, nil
}

// apiDelete calls HTTP DELETE <path> using rawHTTP (see Client.rawHTTP).
func apiDelete(c *Client, path string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	// Ensure we're authenticated
	if err := c.rawLogin(); err != nil {
		return fmt.Errorf("login failed: %w", err)
	}

	// Build full URL
	fullURL := c.baseURL + "/rhn/manager/api/" + path

	// For DELETE, we need to send minimal body. Browser sends full project data but
	// Uyuni API may accept empty object. Try with empty object first.
	bodyData := map[string]any{}
	bodyBytes, err := json.Marshal(bodyData)
	if err != nil {
		return fmt.Errorf("failed to marshal DELETE body: %w", err)
	}

	// Create DELETE request
	req, err := http.NewRequest("DELETE", fullURL, bytes.NewReader(bodyBytes))
	if err != nil {
		return fmt.Errorf("failed to create DELETE request: %w", err)
	}

	// Set required headers (matching browser DELETE request)
	req.Header.Set("Content-Type", "application/json; charset=UTF-8")
	req.Header.Set("Accept", "*/*")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	// Important: Set Accept-Encoding to allow automatic decompression
	req.Header.Set("Accept-Encoding", "gzip, deflate, br")

	// Execute using rawHTTP (its cookiejar carries the session from rawLogin)
	resp, err := c.rawHTTP.Do(req)
	if err != nil {
		// Re-authenticate and retry once
		if needsReauth(err.Error()) {
			if loginErr := c.rawLogin(); loginErr != nil {
				return loginErr
			}
			req2, _ := http.NewRequest("DELETE", fullURL, bytes.NewReader([]byte("{}")))
			req2.Header.Set("Content-Type", "application/json; charset=utf-8")
			req2.Header.Set("Accept", "application/json; charset=utf-8")
			resp, err = c.rawHTTP.Do(req2)
		}
		if err != nil {
			return sanitizeHTTPError(err, path)
		}
	}
	defer resp.Body.Close()

	// Handle 401 Unauthorized - retry with fresh login
	if resp.StatusCode == http.StatusUnauthorized {
		fmt.Printf("DEBUG: Got 401 Unauthorized, retrying with fresh login\n")
		if loginErr := c.rawLogin(); loginErr != nil {
			return loginErr
		}
		req3, _ := http.NewRequest("DELETE", fullURL, bytes.NewReader(bodyBytes))
		req3.Header.Set("Content-Type", "application/json; charset=UTF-8")
		req3.Header.Set("Accept", "*/*")
		req3.Header.Set("X-Requested-With", "XMLHttpRequest")
		req3.Header.Set("Accept-Encoding", "gzip, deflate, br")
		resp, err = c.rawHTTP.Do(req3)
		if err != nil {
			return sanitizeHTTPError(err, path)
		}
		defer resp.Body.Close()
	}

	// Parse response - handle gzip if needed
	var respBody []byte
	if resp.Header.Get("Content-Encoding") == "gzip" {
		gz, err := gzip.NewReader(resp.Body)
		if err != nil {
			return fmt.Errorf("failed to create gzip reader: %w", err)
		}
		defer gz.Close()
		respBody, _ = io.ReadAll(gz)
	} else {
		respBody, _ = io.ReadAll(resp.Body)
	}

	// Log the response for debugging
	if len(respBody) == 0 {
		fmt.Printf("DEBUG: DELETE response body is empty (status %d)\n", resp.StatusCode)
		// Empty response with 200 OK is success
		if resp.StatusCode == http.StatusOK {
			return nil
		}
		return fmt.Errorf("DELETE failed with status %d and empty body", resp.StatusCode)
	}

	fmt.Printf("DEBUG: DELETE response body: %s\n", string(respBody))

	var apiResp struct {
		Success  bool   `json:"success"`
		Message  string `json:"message"`
		Messages []string `json:"messages"`
	}
	if err := json.Unmarshal(respBody, &apiResp); err != nil {
		fmt.Printf("DEBUG: Failed to parse DELETE response: %v\n", err)
		// If we get 200 OK, treat as success even if response can't be parsed
		if resp.StatusCode == http.StatusOK {
			return nil
		}
		return fmt.Errorf("failed to parse DELETE response: %w", err)
	}

	if !apiResp.Success {
		return fmt.Errorf("%s", apiResp.Message)
	}

	return nil
}

// apiDeleteWithBody calls HTTP DELETE with a JSON body (used for environment deletion)
func apiDeleteWithBody(c *Client, path string, bodyData map[string]any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	// Ensure we're authenticated
	if err := c.http.Login(); err != nil {
		return fmt.Errorf("login failed: %w", err)
	}

	// Build full URL
	fullURL := c.baseURL + "/rhn/manager/api/" + path

	// Serialize request body
	bodyBytes, err := json.Marshal(bodyData)
	if err != nil {
		return fmt.Errorf("failed to marshal DELETE body: %w", err)
	}

	// Create DELETE request
	req, err := http.NewRequest("DELETE", fullURL, bytes.NewReader(bodyBytes))
	if err != nil {
		return fmt.Errorf("failed to create DELETE request: %w", err)
	}

	// Set required headers
	req.Header.Set("Content-Type", "application/json; charset=UTF-8")
	req.Header.Set("Accept", "*/*")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("Accept-Encoding", "gzip, deflate, br")

	// Execute using rawHTTP (its cookiejar carries the session from rawLogin)
	resp, err := c.rawHTTP.Do(req)
	if err != nil {
		if needsReauth(err.Error()) {
			if loginErr := c.rawLogin(); loginErr != nil {
				return loginErr
			}
			req2, _ := http.NewRequest("DELETE", fullURL, bytes.NewReader(bodyBytes))
			req2.Header.Set("Content-Type", "application/json; charset=UTF-8")
			req2.Header.Set("Accept", "*/*")
			req2.Header.Set("X-Requested-With", "XMLHttpRequest")
			req2.Header.Set("Accept-Encoding", "gzip, deflate, br")
			resp, err = c.rawHTTP.Do(req2)
		}
		if err != nil {
			return sanitizeHTTPError(err, path)
		}
	}
	defer resp.Body.Close()

	// Handle 401 Unauthorized - retry with fresh login
	if resp.StatusCode == http.StatusUnauthorized {
		fmt.Printf("DEBUG: Got 401 Unauthorized in DELETE with body, retrying with fresh login\n")
		if loginErr := c.rawLogin(); loginErr != nil {
			return loginErr
		}
		req3, _ := http.NewRequest("DELETE", fullURL, bytes.NewReader(bodyBytes))
		req3.Header.Set("Content-Type", "application/json; charset=UTF-8")
		req3.Header.Set("Accept", "*/*")
		req3.Header.Set("X-Requested-With", "XMLHttpRequest")
		req3.Header.Set("Accept-Encoding", "gzip, deflate, br")
		resp, err = c.rawHTTP.Do(req3)
		if err != nil {
			return sanitizeHTTPError(err, path)
		}
		defer resp.Body.Close()
	}

	// Parse response - handle gzip if needed
	var respBody []byte
	if resp.Header.Get("Content-Encoding") == "gzip" {
		gz, err := gzip.NewReader(resp.Body)
		if err != nil {
			return fmt.Errorf("failed to create gzip reader: %w", err)
		}
		defer gz.Close()
		respBody, _ = io.ReadAll(gz)
	} else {
		respBody, _ = io.ReadAll(resp.Body)
	}

	// Log the response for debugging
	if len(respBody) == 0 {
		fmt.Printf("DEBUG: DELETE response body is empty (status %d)\n", resp.StatusCode)
		// Treat 200 OK and 500 errors as success (500 might indicate already deleted)
		if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusInternalServerError {
			return nil
		}
		return fmt.Errorf("DELETE failed with status %d and empty body", resp.StatusCode)
	}

	fmt.Printf("DEBUG: DELETE with body response: %s\n", string(respBody))

	var apiResp struct {
		Success  bool   `json:"success"`
		Message  string `json:"message"`
		Messages []string `json:"messages"`
	}
	if err := json.Unmarshal(respBody, &apiResp); err != nil {
		fmt.Printf("DEBUG: Failed to parse DELETE with body response: %v\n", err)
		if resp.StatusCode == http.StatusOK {
			return nil
		}
		return fmt.Errorf("failed to parse DELETE response: %w", err)
	}

	if !apiResp.Success {
		return fmt.Errorf("%s", apiResp.Message)
	}

	return nil
}

// needsReauth reports whether an HTTP error warrants a re-authentication
// attempt. 401 is the standard unauthenticated signal. Uyuni also returns 403
// with an HTML body when the pxt-session-cookie has expired (rather than 401),
// so we treat 403 the same way and let the retry surface a real permission
// error if re-auth doesn't help. We also match raw HTML in the error body as
// an additional signal for cases where the status code is not in the string.
func needsReauth(errMsg string) bool {
	return strings.Contains(errMsg, "401") ||
		strings.Contains(errMsg, "403") ||
		strings.Contains(errMsg, "<!DOCTYPE") ||
		strings.Contains(errMsg, "<html")
}

// sanitizeHTTPError replaces HTML response bodies in HTTP error messages with
// a concise description. Uyuni returns full HTML pages for 403/404/5xx errors,
// which produce unreadable multi-kilobyte log lines.
func sanitizeHTTPError(err error, path string) error {
	msg := err.Error()
	if strings.Contains(msg, "403") {
		return fmt.Errorf("403 access denied calling %s: verify the Uyuni user has the required roles (e.g. Channel Administrator)", path)
	}
	// Strip any HTML body to keep log lines readable.
	if idx := strings.Index(msg, "<!DOCTYPE"); idx != -1 {
		return fmt.Errorf("%s", strings.TrimSpace(msg[:idx]))
	}
	if idx := strings.Index(msg, "<html"); idx != -1 {
		return fmt.Errorf("%s", strings.TrimSpace(msg[:idx]))
	}
	return err
}

// asNotFound converts an error whose message contains "no such" / "not found"
// / "does not exist" into a *notFoundError, enabling uyuni.IsNotFound.
func asNotFound(err error) error {
	if err == nil {
		return nil
	}
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "no such") ||
		strings.Contains(msg, "not found") ||
		strings.Contains(msg, "does not exist") ||
		strings.Contains(msg, "invalid key") {
		return &notFoundError{msg: err.Error()}
	}
	return err
}

// --- internal wire types (REST JSON shapes) ---

type wireSystem struct {
	ID                 int      `json:"id"`
	Name               string   `json:"name"`
	MinionID           string   `json:"minionid"`
	Hostname           string   `json:"hostname"`
	Description        string   `json:"description"`
	ContactMethod      string   `json:"contact_method"`
	BaseChannelLabel   string   `json:"base_channel_label"`
	ChildChannelLabels []string `json:"child_channel_labels"`
	BaseEntitlement    string   `json:"base_entitlement"`
	LastCheckin        string   `json:"last_checkin"`
}

type wireSystemGroup struct {
	ID          int    `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	SystemCount int    `json:"system_count"`
}

type wireActivationKey struct {
	Key              string   `json:"key"`
	Description      string   `json:"description"`
	BaseChannelLabel string   `json:"base_channel_label"`
	ChildChannels    []string `json:"child_channel_labels"`
	ConfigChannels   []string `json:"config_channel_labels"`
	Entitlements     []string `json:"entitlements"`
	UsageLimit       int      `json:"usage_limit"`
	UniversalDefault bool     `json:"universal_default"`
	Disabled         bool     `json:"disabled"`
	ContactMethod    string   `json:"contact_method"`
	ServerGroupIDs   []int    `json:"serverGroupIds"`
}

type wireChannel struct {
	ID                 int    `json:"id"`
	Label              string `json:"label"`
	Name               string `json:"name"`
	Summary            string `json:"summary"`
	Description        string `json:"description"`
	ArchName           string `json:"arch_name"`
	ParentChannelLabel string `json:"parent_channel_label"`
	ChecksumLabel      string `json:"checksum_label"`
	GPGKeyURL          string `json:"gpg_key_url"`
	GPGKeyID           string `json:"gpg_key_id"`
	GPGKeyFp           string `json:"gpg_key_fp"`
	GPGCheck           bool   `json:"gpg_check"`
	PackageCount       int    `json:"package_count"`
	LastSynced         string `json:"last_synced"`
}

type wireRepo struct {
	ID                int    `json:"id"`
	Label             string `json:"label"`
	SourceURL         string `json:"sourceUrl"`
	Type              string `json:"type"`
	HasSignedMetadata bool   `json:"hasSignedMetadata"`
}

type wireConfigChannelType struct {
	Label string `json:"label"`
}

type wireConfigChannel struct {
	ID                int                    `json:"id"`
	Label             string                 `json:"label"`
	Name              string                 `json:"name"`
	Description       string                 `json:"description"`
	ConfigChannelType wireConfigChannelType  `json:"configChannelType"`
}

type wireConfigFile struct {
	Path        string `json:"path"`
	Type        string `json:"type"`
	Revision    int    `json:"revision"`
	Contents    string `json:"contents"`
	TargetPath  string `json:"target_path"`
	Owner       string `json:"owner"`
	Group       string `json:"group"`
	Permissions int    `json:"permissions"`
	SELinuxCtx  string `json:"selinux_ctx"`
	Macro       bool   `json:"macro"`
	Binary      bool   `json:"binary"`
}

type wireProject struct {
	ID          int    `json:"id"`
	Label       string `json:"label"`
	Name        string `json:"name"`
	Description string `json:"description"`
}

type wireProjectSource struct {
	Channel struct {
		ID    int    `json:"id"`
		Label string `json:"label"`
	} `json:"channel"`
	State string `json:"state"`
}

type wireEnvironment struct {
	ID                       int    `json:"id"`
	Label                    string `json:"label"`
	Name                     string `json:"name"`
	Description              string `json:"description"`
	Version                  int    `json:"version"`
	PreviousEnvironmentLabel string `json:"previous_environment_label"`
	Status                   string `json:"status"`
}

type wireFilter struct {
	ID         int    `json:"id"`
	Name       string `json:"name"`
	EntityType string `json:"entity_type"`
	Rule       string `json:"rule"`
	Criteria   struct {
		Field   string `json:"field"`
		Matcher string `json:"matcher"`
		Value   string `json:"value"`
	} `json:"criteria"`
}

type wireImageStore struct {
	ID    int    `json:"id"`
	Label string `json:"label"`
	URI   string `json:"uri"`
	Type  string `json:"store_type"`
}

type wireImageProfile struct {
	ID            int    `json:"id"`
	Label         string `json:"label"`
	Type          string `json:"imageType"`
	StoreLabel    string `json:"imageStore"`
	ActivationKey string `json:"activationKey"`
	Path          string `json:"path"`
}

type wireImageInfo struct {
	ID           int    `json:"id"`
	Name         string `json:"name"`
	Version      string `json:"version"`
	Revision     int    `json:"revision"`
	BuildStatus  string `json:"build_status"`
	ProfileLabel string `json:"profile_label"`
}

type wireAction struct {
	ID         int    `json:"id"`
	Name       string `json:"name"`
	Type       string `json:"type"`
	Status     string `json:"status"`
	StartedAt  string `json:"started_at"`
	FinishedAt string `json:"finished_at"`
}

type wireActionResult struct {
	ServerID int    `json:"server_id"`
	ActionID int    `json:"action_id"`
	Status   string `json:"status"`
	Result   string `json:"result"`
	ExitCode *int   `json:"exit_code"`
}

// =============================================================================
// Server-level
// =============================================================================

func (c *Client) GetServerVersion(ctx context.Context) (string, error) {
	r, err := apiGet[string](c, "api/getVersion")
	if err != nil {
		return "", err
	}
	return r, nil
}

func (c *Client) GetOrgID(ctx context.Context) (int, error) {
	type orgInfo struct {
		ID int `json:"id"`
	}
	// /user/getDetails returns the current user's info including org ID.
	r, err := apiGet[orgInfo](c, "user/getDetails?login="+url.QueryEscape(c.conn.User))
	if err != nil {
		return 0, err
	}
	return r.ID, nil
}

// =============================================================================
// System
// =============================================================================

func (c *Client) FindSystemByMinionID(ctx context.Context, minionID string) (*SystemDetails, error) {
	// getMinionIdMap returns map[minionID]serverID.
	type minionMap map[string]int
	m, err := apiGet[minionMap](c, "system/getMinionIdMap")
	if err != nil {
		return nil, err
	}
	sid, ok := m[minionID]
	if !ok {
		return nil, &notFoundError{msg: fmt.Sprintf("minion %q not found", minionID)}
	}
	return c.GetSystemDetails(ctx, sid)
}

func (c *Client) FindSystemByMAC(ctx context.Context, mac string) (*SystemDetails, error) {
	// Uyuni has no bulk "list all network devices" call; system.getNetworkDevices
	// is per-system (sessionKey, sid). Walk the visible systems and inspect each
	// one's network devices for a matching hardware address.
	type sysInfo struct {
		ID int `json:"id"`
	}
	type netInfo struct {
		HWAddr string `json:"hardware_address"`
	}
	systems, err := apiGet[[]sysInfo](c, "system/listSystems")
	if err != nil {
		return nil, err
	}
	norm := strings.ToLower(strings.ReplaceAll(mac, "-", ":"))
	for _, s := range systems {
		devices, err := apiGet[[]netInfo](c, fmt.Sprintf("system/getNetworkDevices?sid=%d", s.ID))
		if err != nil {
			continue
		}
		for _, ni := range devices {
			if strings.ToLower(ni.HWAddr) == norm {
				return c.GetSystemDetails(ctx, s.ID)
			}
		}
	}
	return nil, &notFoundError{msg: fmt.Sprintf("system with MAC %q not found", mac)}
}

func (c *Client) CreateSystemProfile(ctx context.Context, name string, data SystemProfileData) (int, error) {
	profileData := map[string]any{}
	if data.HWAddress != "" {
		profileData["hwAddress"] = data.HWAddress
	}
	if data.Hostname != "" {
		profileData["hostname"] = data.Hostname
	}
	id, err := apiPost[int](c, "system/createSystemProfile", map[string]any{
		"systemName": name,
		"data":       profileData,
	})
	if err != nil {
		// Uyuni returns a specific error when the system already exists; surface
		// it as SystemExistsError so callers can adopt the existing system.
		if strings.Contains(strings.ToLower(err.Error()), "already exists") {
			existing, listErr := c.FindSystemByMAC(ctx, data.HWAddress)
			if listErr == nil && existing != nil {
				return 0, &SystemExistsError{IDs: []int{existing.ID}}
			}
		}
		return 0, err
	}
	return id, nil
}

func (c *Client) GetSystemDetails(ctx context.Context, serverID int) (*SystemDetails, error) {
	r, err := apiGet[wireSystem](c, fmt.Sprintf("system/getDetails?sid=%d", serverID))
	if err != nil {
		return nil, asNotFound(err)
	}
	return wireSystemToDetails(&r), nil
}

func (c *Client) SetSystemDetails(ctx context.Context, serverID int, d SystemDetailsUpdate) error {
	details := map[string]any{}
	if d.Description != "" {
		details["description"] = d.Description
	}
	if d.ContactMethod != "" {
		details["contact_method"] = d.ContactMethod
	}
	_, err := apiPost[any](c, "system/setDetails", map[string]any{
		"sid":     serverID,
		"details": details,
	})
	return err
}

func (c *Client) ListSystemConfigChannels(ctx context.Context, serverID int) ([]string, error) {
	type wireCCInfo struct {
		Label string `json:"label"`
	}
	list, err := apiGet[[]wireCCInfo](c, fmt.Sprintf("system/config/listChannels?sid=%d", serverID))
	if err != nil {
		return nil, err
	}
	labels := make([]string, len(list))
	for i, cc := range list {
		labels[i] = cc.Label
	}
	return labels, nil
}

func (c *Client) SetSystemConfigChannels(ctx context.Context, serverID int, channelLabels []string) error {
	_, err := apiPost[any](c, "system/config/setChannels", map[string]any{
		"sids":                 []int{serverID},
		"configChannelLabels":  channelLabels,
	})
	return err
}

func (c *Client) ProvisionSystem(ctx context.Context, serverID int, profile string, earliest time.Time) (int, error) {
	actionID, err := apiPost[int](c, "system/provisionSystem", map[string]any{
		"sid":          serverID,
		"profileName":  profile,
		"earliestDate": earliest.UTC().Format(time.RFC3339),
	})
	return actionID, err
}

func (c *Client) DeleteSystem(ctx context.Context, serverID int) error {
	_, err := apiPost[any](c, "system/deleteSystem", map[string]any{
		"sid":           serverID,
		"clean_up_type": "NONE",
	})
	return asNotFound(err)
}

func (c *Client) GetCustomInfo(ctx context.Context, serverID int) (map[string]string, error) {
	type keyVal struct {
		Key   string `json:"key"`
		Value string `json:"value"`
	}
	list, err := apiGet[[]keyVal](c, fmt.Sprintf("system/getCustomValues?sid=%d", serverID))
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(list))
	for _, kv := range list {
		out[kv.Key] = kv.Value
	}
	return out, nil
}

func (c *Client) SetCustomInfo(ctx context.Context, serverID int, kv map[string]string) error {
	_, err := apiPost[any](c, "system/setCustomValues", map[string]any{
		"sid":    serverID,
		"values": kv,
	})
	return err
}

func (c *Client) DeleteCustomInfo(ctx context.Context, serverID int, keys []string) error {
	_, err := apiPost[any](c, "system/deleteCustomValues", map[string]any{
		"sid":  serverID,
		"keys": keys,
	})
	return err
}

func (c *Client) ScheduleChangeChannels(ctx context.Context, serverID int, base string, children []string, earliest time.Time) (int, error) {
	type resp struct {
		ActionID int `json:"action_id"`
	}
	r, err := apiPost[resp](c, "system/scheduleChangeChannels", map[string]any{
		"sid":                 serverID,
		"base_channel":        base,
		"child_channels":      children,
		"earliest_occurrence": earliest.Format(time.RFC3339),
	})
	if err != nil {
		return 0, err
	}
	return r.ActionID, nil
}

func (c *Client) SetBaseChannel(ctx context.Context, serverID int, label string) error {
	_, err := apiPost[any](c, "system/setBaseChannel", map[string]any{
		"sid":          serverID,
		"channelLabel": label,
	})
	return err
}

func (c *Client) SetChildChannels(ctx context.Context, serverID int, labels []string) error {
	_, err := apiPost[any](c, "system/setChildChannels", map[string]any{
		"sid":                serverID,
		"channelIdsOrLabels": labels,
	})
	return err
}

func (c *Client) ListEntitlements(ctx context.Context, serverID int) ([]string, error) {
	return apiGet[[]string](c, fmt.Sprintf("system/getEntitlements?sid=%d", serverID))
}

func (c *Client) AddEntitlements(ctx context.Context, serverID int, addons []string) (int, error) {
	return apiPost[int](c, "system/addEntitlements", map[string]any{
		"sid":          serverID,
		"entitlements": addons,
	})
}

func (c *Client) RemoveEntitlements(ctx context.Context, serverID int, addons []string) error {
	_, err := apiPost[any](c, "system/removeEntitlements", map[string]any{
		"sid":          serverID,
		"entitlements": addons,
	})
	return err
}

func wireSystemToDetails(w *wireSystem) *SystemDetails {
	return &SystemDetails{
		ID:                 w.ID,
		Name:               w.Name,
		MinionID:           w.MinionID,
		Hostname:           w.Hostname,
		Description:        w.Description,
		ContactMethod:      w.ContactMethod,
		BaseChannelLabel:   w.BaseChannelLabel,
		ChildChannelLabels: w.ChildChannelLabels,
		BaseEntitlement:    w.BaseEntitlement,
	}
}

// =============================================================================
// SystemGroup
// =============================================================================

func (c *Client) CreateSystemGroup(ctx context.Context, name, description string) (*SystemGroupDetails, error) {
	r, err := apiPost[wireSystemGroup](c, "systemgroup/create", map[string]any{
		"name":        name,
		"description": description,
	})
	if err != nil {
		return nil, err
	}
	return wireGroupToDetails(&r), nil
}

func (c *Client) GetSystemGroup(ctx context.Context, name string) (*SystemGroupDetails, error) {
	r, err := apiGet[wireSystemGroup](c, "systemgroup/getDetails?systemGroupName="+url.QueryEscape(name))
	if err != nil {
		return nil, asNotFound(err)
	}
	return wireGroupToDetails(&r), nil
}

func (c *Client) UpdateSystemGroupDescription(ctx context.Context, name, description string) error {
	_, err := apiPost[any](c, "systemgroup/update", map[string]any{
		"systemGroupName": name,
		"description":     description,
	})
	return err
}

func (c *Client) DeleteSystemGroup(ctx context.Context, name string) error {
	_, err := apiPost[any](c, "systemgroup/delete", map[string]any{
		"systemGroupName": name,
	})
	return asNotFound(err)
}

func (c *Client) ListSystemsInGroup(ctx context.Context, name string) ([]int, error) {
	type sys struct {
		ID int `json:"id"`
	}
	list, err := apiGet[[]sys](c, "systemgroup/listSystems?systemGroupName="+url.QueryEscape(name))
	if err != nil {
		return nil, err
	}
	out := make([]int, len(list))
	for i, s := range list {
		out[i] = s.ID
	}
	return out, nil
}

func (c *Client) AddSystemsToGroup(ctx context.Context, name string, serverIDs []int) error {
	_, err := apiPost[any](c, "systemgroup/addOrRemoveSystems", map[string]any{
		"systemGroupName": name,
		"serverIds":       serverIDs,
		"add":             true,
	})
	return err
}

func (c *Client) RemoveSystemsFromGroup(ctx context.Context, name string, serverIDs []int) error {
	_, err := apiPost[any](c, "systemgroup/addOrRemoveSystems", map[string]any{
		"systemGroupName": name,
		"serverIds":       serverIDs,
		"add":             false,
	})
	return err
}

func (c *Client) SubscribeGroupToConfigChannel(ctx context.Context, groupName, channelLabel string) error {
	_, err := apiPost[any](c, "systemgroup/subscribeConfigChannel", map[string]any{
		"systemGroupName":     groupName,
		"configChannelLabels": []string{channelLabel},
	})
	return err
}

func (c *Client) UnsubscribeGroupFromConfigChannel(ctx context.Context, groupName, channelLabel string) error {
	_, err := apiPost[any](c, "systemgroup/unsubscribeConfigChannel", map[string]any{
		"systemGroupName":     groupName,
		"configChannelLabels": []string{channelLabel},
	})
	return err
}

func wireGroupToDetails(w *wireSystemGroup) *SystemGroupDetails {
	return &SystemGroupDetails{
		ID:          w.ID,
		Name:        w.Name,
		Description: w.Description,
		SystemCount: w.SystemCount,
	}
}

// =============================================================================
// ActivationKey
// =============================================================================

func (c *Client) CreateActivationKey(ctx context.Context, in ActivationKeyDetails) (string, error) {
	// activationkey.create returns the new key as a bare string. It has two
	// overloads (with/without usageLimit); omit usageLimit to hit the unlimited
	// overload, which is what UsageLimit==0 means in this operator's model.
	entitlements := in.Entitlements
	if entitlements == nil {
		entitlements = []string{}
	}
	params := map[string]any{
		"key":              in.Key,
		"description":      in.Description,
		"baseChannelLabel": in.BaseChannelLabel,
		"entitlements":     entitlements,
		"universalDefault": in.UniversalDefault,
	}
	if in.UsageLimit > 0 {
		params["usageLimit"] = in.UsageLimit
	}
	key, err := apiPost[string](c, "activationkey/create", params)
	if err != nil {
		return "", err
	}
	return key, nil
}

func (c *Client) GetActivationKey(ctx context.Context, key string) (*ActivationKeyDetails, error) {
	r, err := apiGet[wireActivationKey](c, "activationkey/getDetails?key="+url.QueryEscape(key))
	if err != nil {
		return nil, asNotFound(err)
	}
	return wireAKToDetails(&r), nil
}

func (c *Client) DeleteActivationKey(ctx context.Context, key string) error {
	_, err := apiPost[any](c, "activationkey/delete", map[string]any{
		"key": key,
	})
	return asNotFound(err)
}

func (c *Client) SetActivationKeyDetails(ctx context.Context, key string, d ActivationKeyDetails) error {
	// setDetails takes (key, struct details) with snake_case members — not the
	// flat camelCase params that create uses.
	details := map[string]any{
		"description":        d.Description,
		"base_channel_label": d.BaseChannelLabel,
		"universal_default":  d.UniversalDefault,
	}
	if d.UsageLimit > 0 {
		details["usage_limit"] = d.UsageLimit
	} else {
		details["unlimited_usage_limit"] = true
	}
	if d.ContactMethod != "" {
		details["contact_method"] = d.ContactMethod
	}
	_, err := apiPost[any](c, "activationkey/setDetails", map[string]any{
		"key":     key,
		"details": details,
	})
	return err
}

func (c *Client) AddActivationKeyEntitlements(ctx context.Context, key string, entitlements []string) error {
	_, err := apiPost[any](c, "activationkey/addEntitlements", map[string]any{
		"key":          key,
		"entitlements": entitlements,
	})
	return err
}

func (c *Client) RemoveActivationKeyEntitlements(ctx context.Context, key string, entitlements []string) error {
	_, err := apiPost[any](c, "activationkey/removeEntitlements", map[string]any{
		"key":          key,
		"entitlements": entitlements,
	})
	return err
}

func (c *Client) AddChildChannels(ctx context.Context, key string, labels []string) error {
	_, err := apiPost[any](c, "activationkey/addChildChannels", map[string]any{
		"key":               key,
		"childChannelLabels": labels,
	})
	return err
}

func (c *Client) RemoveChildChannels(ctx context.Context, key string, labels []string) error {
	_, err := apiPost[any](c, "activationkey/removeChildChannels", map[string]any{
		"key":               key,
		"childChannelLabels": labels,
	})
	return err
}

func (c *Client) SetActivationKeyConfigChannels(ctx context.Context, key string, channelLabels []string) error {
	_, err := apiPost[any](c, "activationkey/setConfigChannels", map[string]any{
		"keys":               []string{key},
		"configChannelLabels": channelLabels,
	})
	return err
}

func (c *Client) SetActivationKeyGroups(ctx context.Context, key string, groupIDs []int) error {
	// Replace all groups: first remove existing, then add desired.
	existing, err := c.GetActivationKey(ctx, key)
	if err != nil {
		return err
	}
	if len(existing.ServerGroupIDs) > 0 {
		if _, err := apiPost[any](c, "activationkey/removeServerGroups", map[string]any{
			"key":              key,
			"serverGroupIds": existing.ServerGroupIDs,
		}); err != nil {
			return err
		}
	}
	if len(groupIDs) > 0 {
		if _, err := apiPost[any](c, "activationkey/addServerGroups", map[string]any{
			"key":              key,
			"serverGroupIds": groupIDs,
		}); err != nil {
			return err
		}
	}
	return nil
}

func wireAKToDetails(w *wireActivationKey) *ActivationKeyDetails {
	return &ActivationKeyDetails{
		Key:              w.Key,
		Description:      w.Description,
		BaseChannelLabel: w.BaseChannelLabel,
		ChildChannels:    w.ChildChannels,
		ConfigChannels:   w.ConfigChannels,
		Entitlements:     w.Entitlements,
		UsageLimit:       w.UsageLimit,
		UniversalDefault: w.UniversalDefault,
		Disabled:         w.Disabled,
		ContactMethod:    w.ContactMethod,
		ServerGroupIDs:   w.ServerGroupIDs,
	}
}

// =============================================================================
// Software channels & repos
// =============================================================================

func (c *Client) CreateChannel(ctx context.Context, spec ChannelDetails) error {
	_, err := apiPost[any](c, "channel/software/create", map[string]any{
		"label":        spec.Label,
		"name":         spec.Name,
		"summary":      spec.Summary,
		"archLabel":    spec.ArchName,
		"parentLabel":  spec.ParentChannelLabel,
		"checksumType": spec.ChecksumLabel,
		"gpgKey": map[string]any{
			"url":         spec.GPGKeyURL,
			"id":          spec.GPGKeyID,
			"fingerprint": spec.GPGKeyFp,
		},
		"gpgCheck": spec.GPGCheck,
	})
	return err
}

func (c *Client) GetChannel(ctx context.Context, label string) (*ChannelDetails, error) {
	r, err := apiGet[wireChannel](c, "channel/software/getDetails?channelLabel="+url.QueryEscape(label))
	if err != nil {
		return nil, asNotFound(err)
	}
	return wireChannelToDetails(&r), nil
}

func (c *Client) SetChannelDetails(ctx context.Context, id int, d ChannelDetails) error {
	_, err := apiPost[any](c, "channel/software/setDetails", map[string]any{
		"channelId": id,
		"details": map[string]any{
			"name":           d.Name,
			"summary":        d.Summary,
			"description":    d.Description,
			"gpg_key_url":    d.GPGKeyURL,
			"gpg_key_id":     d.GPGKeyID,
			"gpg_key_fp":     d.GPGKeyFp,
			"gpg_check":      d.GPGCheck,
			"checksum_label": d.ChecksumLabel,
		},
	})
	return err
}

func (c *Client) DeleteChannel(ctx context.Context, label string) error {
	_, err := apiPost[any](c, "channel/software/delete", map[string]any{
		"channelLabel": label,
	})
	return asNotFound(err)
}

func (c *Client) SetChannelGloballySubscribable(ctx context.Context, label string, subscribable bool) error {
	_, err := apiPost[any](c, "channel/software/setGloballySubscribable", map[string]any{
		"channelLabel": label,
		"subscribable": subscribable,
	})
	return err
}

func (c *Client) ListChannelRepos(ctx context.Context, label string) ([]string, error) {
	type repoRef struct {
		Label string `json:"label"`
	}
	list, err := apiGet[[]repoRef](c, "channel/software/listChannelRepos?channelLabel="+url.QueryEscape(label))
	if err != nil {
		return nil, err
	}
	out := make([]string, len(list))
	for i, r := range list {
		out[i] = r.Label
	}
	return out, nil
}

func (c *Client) AssociateRepo(ctx context.Context, channelLabel, repoLabel string) error {
	_, err := apiPost[any](c, "channel/software/associateRepo", map[string]any{
		"channelLabel": channelLabel,
		"repoLabel":    repoLabel,
	})
	return err
}

func (c *Client) DisassociateRepo(ctx context.Context, channelLabel, repoLabel string) error {
	_, err := apiPost[any](c, "channel/software/disassociateRepo", map[string]any{
		"channelLabel": channelLabel,
		"repoLabel":    repoLabel,
	})
	return err
}

func (c *Client) SetRepoSyncSchedule(ctx context.Context, channelLabel, quartzCron string) error {
	_, err := apiPost[any](c, "channel/software/syncRepo", map[string]any{
		"channelLabel": channelLabel,
		"cronExpr":     quartzCron,
	})
	return err
}

func (c *Client) SyncChannelNow(ctx context.Context, channelLabel string) error {
	_, err := apiPost[any](c, "channel/software/syncRepo", map[string]any{
		"channelLabel": channelLabel,
	})
	return err
}

func (c *Client) CreateRepo(ctx context.Context, r RepoDetails, sslCa, sslCert, sslKey string) (*RepoDetails, error) {
	payload := map[string]any{
		"label": r.Label,
		"type":  r.Type,
		"url":   r.URL,
	}
	if sslCa != "" || sslCert != "" || sslKey != "" {
		payload["sslCaCert"]  = sslCa
		payload["sslCliCert"] = sslCert
		payload["sslCliKey"]  = sslKey
	}
	created, err := apiPost[wireRepo](c, "channel/software/createRepo", payload)
	if err != nil {
		return nil, err
	}
	return wireRepoToDetails(&created), nil
}

func (c *Client) GetRepo(ctx context.Context, label string) (*RepoDetails, error) {
	r, err := apiGet[wireRepo](c, "channel/software/getRepoDetails?repoLabel="+url.QueryEscape(label))
	if err != nil {
		return nil, asNotFound(err)
	}
	return wireRepoToDetails(&r), nil
}

func (c *Client) UpdateRepoURL(ctx context.Context, label, repoURL string) error {
	_, err := apiPost[any](c, "channel/software/updateRepoUrl", map[string]any{
		"label": label,
		"url":   repoURL,
	})
	return err
}

func (c *Client) DeleteRepo(ctx context.Context, label string) error {
	_, err := apiPost[any](c, "channel/software/removeRepo", map[string]any{
		"label": label,
	})
	return asNotFound(err)
}

func wireChannelToDetails(w *wireChannel) *ChannelDetails {
	return &ChannelDetails{
		ID:                 w.ID,
		Label:              w.Label,
		Name:               w.Name,
		Summary:            w.Summary,
		Description:        w.Description,
		ArchName:           w.ArchName,
		ParentChannelLabel: w.ParentChannelLabel,
		ChecksumLabel:      w.ChecksumLabel,
		GPGKeyURL:          w.GPGKeyURL,
		GPGKeyID:           w.GPGKeyID,
		GPGKeyFp:           w.GPGKeyFp,
		GPGCheck:           w.GPGCheck,
		PackageCount:       w.PackageCount,
		LastSynced:         w.LastSynced,
	}
}

func wireRepoToDetails(w *wireRepo) *RepoDetails {
	return &RepoDetails{
		ID:                w.ID,
		Label:             w.Label,
		URL:               w.SourceURL,
		Type:              w.Type,
		HasSignedMetadata: w.HasSignedMetadata,
	}
}

// =============================================================================
// Config channels & files
// =============================================================================

func (c *Client) CreateConfigChannel(ctx context.Context, label, name, description, chanType string) (*ConfigChannelDetails, error) {
	r, err := apiPost[wireConfigChannel](c, "configchannel/create", map[string]any{
		"label":       label,
		"name":        name,
		"description": description,
		"type":        chanType,
	})
	if err != nil {
		return nil, err
	}
	return wireConfigChanToDetails(&r), nil
}

func (c *Client) GetConfigChannel(ctx context.Context, label string) (*ConfigChannelDetails, error) {
	r, err := apiGet[wireConfigChannel](c, "configchannel/getDetails?label="+url.QueryEscape(label))
	if err != nil {
		return nil, asNotFound(err)
	}
	return wireConfigChanToDetails(&r), nil
}

func (c *Client) UpdateConfigChannel(ctx context.Context, label, name, description string) error {
	_, err := apiPost[any](c, "configchannel/update", map[string]any{
		"label":       label,
		"name":        name,
		"description": description,
	})
	return err
}

func (c *Client) DeleteConfigChannel(ctx context.Context, label string) error {
	_, err := apiPost[any](c, "configchannel/deleteChannels", map[string]any{
		"labels": []string{label},
	})
	return asNotFound(err)
}

func (c *Client) ListConfigFiles(ctx context.Context, channelLabel string) ([]ConfigFileDetails, error) {
	list, err := apiGet[[]wireConfigFile](c, "configchannel/listFiles?label="+url.QueryEscape(channelLabel))
	if err != nil {
		return nil, err
	}
	out := make([]ConfigFileDetails, len(list))
	for i, f := range list {
		out[i] = *wireConfigFileToDetails(&f)
	}
	return out, nil
}

func (c *Client) GetConfigFile(ctx context.Context, channelLabel, path string) (*ConfigFileDetails, error) {
	r, err := apiPost[wireConfigFile](c, "configchannel/getFileRevision", map[string]any{
		"label":    channelLabel,
		"filename": path,
		"revision": 0, // 0 means latest
	})
	if err != nil {
		return nil, asNotFound(err)
	}
	return wireConfigFileToDetails(&r), nil
}

func (c *Client) CreateOrUpdateConfigFile(ctx context.Context, channelLabel string, f ConfigFileUpsert) (*ConfigFileDetails, error) {
	pathInfo := map[string]any{
		"contents":    f.Contents,
		"owner":       f.Owner,
		"group":       f.Group,
		"permissions": f.Permissions,
	}
	if f.SELinuxCtx != "" {
		pathInfo["selinux_ctx"] = f.SELinuxCtx
	}
	if f.Macro {
		pathInfo["macro"] = f.Macro
	}
	if f.TargetPath != "" {
		pathInfo["target_path"] = f.TargetPath
	}

	payload := map[string]any{
		"label":    channelLabel,
		"path":     f.Path,
		"isDir":    false,
		"pathInfo": pathInfo,
	}

	r, err := apiPost[wireConfigFile](c, "configchannel/createOrUpdatePath", payload)
	if err != nil {
		return nil, err
	}
	return wireConfigFileToDetails(&r), nil
}

func (c *Client) DeleteConfigFile(ctx context.Context, channelLabel, path string) error {
	_, err := apiPost[any](c, "configchannel/deleteFiles", map[string]any{
		"label":     channelLabel,
		"filenames": []string{path},
	})
	return asNotFound(err)
}

func wireConfigChanToDetails(w *wireConfigChannel) *ConfigChannelDetails {
	return &ConfigChannelDetails{
		ID:          w.ID,
		Label:       w.Label,
		Name:        w.Name,
		Description: w.Description,
		Type:        w.ConfigChannelType.Label,
	}
}

func wireConfigFileToDetails(w *wireConfigFile) *ConfigFileDetails {
	return &ConfigFileDetails{
		Path:        w.Path,
		Type:        w.Type,
		Revision:    w.Revision,
		Contents:    w.Contents,
		TargetPath:  w.TargetPath,
		Owner:       w.Owner,
		Group:       w.Group,
		Permissions: fmt.Sprintf("0%o", w.Permissions),
		SELinuxCtx:  w.SELinuxCtx,
		Macro:       w.Macro,
		Binary:      w.Binary,
	}
}

// =============================================================================
// Content management
// =============================================================================

func (c *Client) CreateProject(ctx context.Context, label, name, description string) (*ProjectDetails, error) {
	r, err := apiPost[wireProject](c, "contentmanagement/projects", map[string]any{
		"properties": map[string]any{
			"label":           label,
			"name":            name,
			"description":     description,
			"historyEntries":  []any{},
		},
		"errors": map[string]any{},
	})
	if err != nil {
		return nil, err
	}
	return wireProjectToDetails(&r), nil
}

func (c *Client) LookupProject(ctx context.Context, label string) (*ProjectDetails, error) {
	r, err := apiGet[wireProject](c, "contentmanagement/projects/"+url.QueryEscape(label))
	if err != nil {
		return nil, asNotFound(err)
	}
	return wireProjectToDetails(&r), nil
}

func (c *Client) UpdateProject(ctx context.Context, label, name, description string) error {
	_, err := apiPost[any](c, "contentmanagement/updateProject", map[string]any{
		"projectLabel": label,
		"props": map[string]any{
			"name":        name,
			"description": description,
		},
	})
	return err
}

func (c *Client) RemoveProject(ctx context.Context, label string) error {
	// Probe first: Uyuni's DELETE on an already-removed project returns an
	// opaque 401/500 with an empty body instead of 404, so apiDelete can't
	// distinguish "already gone" from a real failure. LookupProject's GET
	// does return a proper "does not exist" message we can classify.
	if _, err := c.LookupProject(ctx, label); IsNotFound(err) {
		return err
	}
	return apiDelete(c, "contentmanagement/projects/"+url.QueryEscape(label))
}

func (c *Client) ListProjectSources(ctx context.Context, projectLabel string) ([]ProjectSource, error) {
	list, err := apiGet[[]wireProjectSource](c, "contentmanagement/listProjectSources?project_label="+url.QueryEscape(projectLabel))
	if err != nil {
		return nil, err
	}
	out := make([]ProjectSource, len(list))
	for i, s := range list {
		out[i] = ProjectSource{
			State: s.State,
		}
		out[i].Channel.ID = s.Channel.ID
		out[i].Channel.Label = s.Channel.Label
	}
	return out, nil
}

func (c *Client) AttachSource(ctx context.Context, projectLabel, channelLabel string) error {
	_, err := apiPost[any](c, "contentmanagement/attachSource", map[string]any{
		"projectLabel": projectLabel,
		"sourceType":   "SW_CHANNEL",
		"sourceLabel":  channelLabel,
	})
	return err
}

func (c *Client) DetachSource(ctx context.Context, projectLabel, channelLabel string) error {
	_, err := apiPost[any](c, "contentmanagement/detachSource", map[string]any{
		"projectLabel": projectLabel,
		"sourceType":   "SW_CHANNEL",
		"sourceLabel":  channelLabel,
	})
	return err
}

func (c *Client) ListProjectEnvironments(ctx context.Context, projectLabel string) ([]ProjectEnvironmentInfo, error) {
	list, err := apiGet[[]wireEnvironment](c, "contentmanagement/listProjectEnvironments?project_label="+url.QueryEscape(projectLabel))
	if err != nil {
		return nil, err
	}
	out := make([]ProjectEnvironmentInfo, len(list))
	for i, e := range list {
		out[i] = ProjectEnvironmentInfo{
			ID:                       e.ID,
			Label:                    e.Label,
			Name:                     e.Name,
			Description:              e.Description,
			Version:                  e.Version,
			PreviousEnvironmentLabel: e.PreviousEnvironmentLabel,
			Status:                   e.Status,
		}
	}
	return out, nil
}

func (c *Client) CreateEnvironment(ctx context.Context, projectLabel, label, name, description, predecessor string) error {
	payload := map[string]any{
		"projectLabel": projectLabel,
		"label":        label,
		"name":         name,
		"description":  description,
	}
	if predecessor != "" {
		payload["predecessorLabel"] = predecessor
	}
	_, err := apiPost[any](c, "contentmanagement/projects/"+url.QueryEscape(projectLabel)+"/environments", payload)
	return err
}

func (c *Client) UpdateEnvironment(ctx context.Context, projectLabel, envLabel, name, description string) error {
	_, err := apiPost[any](c, "contentmanagement/projects/"+url.QueryEscape(projectLabel)+"/environments", map[string]any{
		"projectLabel": projectLabel,
		"label":        envLabel,
		"name":         name,
		"description":  description,
	})
	return err
}

func (c *Client) RemoveEnvironment(ctx context.Context, projectLabel, envLabel, name, description string) error {
	fmt.Printf("DEBUG: RemoveEnvironment called for project=%s, env=%s\n", projectLabel, envLabel)

	// Probe first: same opaque-error problem as RemoveProject. If the parent
	// project is already gone, the environment is gone with it. If the
	// project exists but no longer lists this environment, it was already
	// removed (e.g. a prior reconcile succeeded but the status update lost
	// the race). Either way, skip the DELETE call that Uyuni can't answer
	// cleanly for a missing entity.
	if _, err := c.LookupProject(ctx, projectLabel); IsNotFound(err) {
		return err
	} else if err == nil {
		envs, listErr := c.ListProjectEnvironments(ctx, projectLabel)
		if listErr == nil {
			found := false
			for _, e := range envs {
				if e.Label == envLabel {
					found = true
					break
				}
			}
			if !found {
				return &notFoundError{msg: fmt.Sprintf("environment %q not found in project %q", envLabel, projectLabel)}
			}
		}
	}

	// Build environment object for DELETE request using provided fields
	// We use reasonable defaults for fields not available from the controller
	path := "contentmanagement/projects/" + url.QueryEscape(projectLabel) + "/environments"
	payload := map[string]any{
		"projectLabel": projectLabel,
		"label":        envLabel,
		"name":         name,
		"description":  description,
		"version":      0,
		"status":       nil,
		"builtTime":    nil,
		"hasProfiles":  false,
	}

	fmt.Printf("DEBUG: Sending DELETE request for env=%s with payload: %+v\n", envLabel, payload)
	err := apiDeleteWithBody(c, path, payload)
	if err != nil {
		fmt.Printf("DEBUG: DELETE failed: %v\n", err)
	} else {
		fmt.Printf("DEBUG: Environment %s deleted successfully\n", envLabel)
	}
	return err
}

func (c *Client) ListFilters(ctx context.Context) ([]FilterDetails, error) {
	list, err := apiGet[[]wireFilter](c, "contentmanagement/listFilters")
	if err != nil {
		return nil, err
	}
	return wireFiltersToDetails(list), nil
}

func (c *Client) CreateFilter(ctx context.Context, name, entityType, rule string, criteria FilterCriteriaWire) (*FilterDetails, error) {
	r, err := apiPost[wireFilter](c, "contentmanagement/createFilter", map[string]any{
		"name":       name,
		"entityType": entityType,
		"rule":       rule,
		"criteria": map[string]any{
			"field":   criteria.Field,
			"matcher": criteria.Matcher,
			"value":   criteria.Value,
		},
	})
	if err != nil {
		return nil, err
	}
	d := wireFilterToDetails(&r)
	return &d, nil
}

func (c *Client) UpdateFilter(ctx context.Context, id int, name, rule string, criteria FilterCriteriaWire) error {
	_, err := apiPost[any](c, "contentmanagement/updateFilter", map[string]any{
		"id":   id,
		"name": name,
		"rule": rule,
		"criteria": map[string]any{
			"field":   criteria.Field,
			"matcher": criteria.Matcher,
			"value":   criteria.Value,
		},
	})
	return err
}

func (c *Client) RemoveFilter(ctx context.Context, id int) error {
	_, err := apiPost[any](c, "contentmanagement/removeFilter", map[string]any{
		"id": id,
	})
	return asNotFound(err)
}

func (c *Client) AttachFilter(ctx context.Context, projectLabel string, id int) error {
	_, err := apiPost[any](c, "contentmanagement/attachFilter", map[string]any{
		"projectLabel": projectLabel,
		"filterId":     id,
	})
	return err
}

func (c *Client) DetachFilter(ctx context.Context, projectLabel string, id int) error {
	_, err := apiPost[any](c, "contentmanagement/detachFilter", map[string]any{
		"projectLabel": projectLabel,
		"filterId":     id,
	})
	return err
}

func (c *Client) BuildProject(ctx context.Context, projectLabel, message string) error {
	_, err := apiPost[any](c, "contentmanagement/buildProject", map[string]any{
		"projectLabel": projectLabel,
		"message":      message,
	})
	return err
}

func (c *Client) PromoteProject(ctx context.Context, projectLabel, envLabel string) error {
	_, err := apiPost[any](c, "contentmanagement/promoteProject", map[string]any{
		"projectLabel": projectLabel,
		"envLabel":     envLabel,
	})
	return err
}

func wireProjectToDetails(w *wireProject) *ProjectDetails {
	return &ProjectDetails{
		ID:          w.ID,
		Label:       w.Label,
		Name:        w.Name,
		Description: w.Description,
	}
}

func wireFilterToDetails(w *wireFilter) FilterDetails {
	return FilterDetails{
		ID:         w.ID,
		Name:       w.Name,
		EntityType: w.EntityType,
		Rule:       w.Rule,
		Criteria: FilterCriteriaWire{
			Field:   w.Criteria.Field,
			Matcher: w.Criteria.Matcher,
			Value:   w.Criteria.Value,
		},
	}
}

func wireFiltersToDetails(list []wireFilter) []FilterDetails {
	out := make([]FilterDetails, len(list))
	for i, f := range list {
		out[i] = wireFilterToDetails(&f)
	}
	return out
}

// =============================================================================
// Image stores / profiles
// =============================================================================

func (c *Client) CreateImageStore(ctx context.Context, label, storeType, uri, user, pass string) error {
	_, err := apiPost[any](c, "imagestore/create", map[string]any{
		"label":      label,
		"store_type": storeType,
		"uri":        uri,
		"username":   user,
		"password":   pass,
	})
	return err
}

func (c *Client) GetImageStore(ctx context.Context, label string) (*ImageStoreDetails, error) {
	r, err := apiGet[wireImageStore](c, "imagestore/getDetails?label="+url.QueryEscape(label))
	if err != nil {
		return nil, asNotFound(err)
	}
	return &ImageStoreDetails{
		ID:    r.ID,
		Label: r.Label,
		URI:   r.URI,
		Type:  r.Type,
	}, nil
}

func (c *Client) UpdateImageStore(ctx context.Context, label, uri string) error {
	_, err := apiPost[any](c, "imagestore/update", map[string]any{
		"label": label,
		"uri":   uri,
	})
	return err
}

func (c *Client) DeleteImageStore(ctx context.Context, label string) error {
	_, err := apiPost[any](c, "imagestore/delete", map[string]any{
		"label": label,
	})
	return asNotFound(err)
}

func (c *Client) CreateImageProfile(ctx context.Context, p ImageProfileDetails, customInfo map[string]string) error {
	// image.profile.create(label, type, storeLabel, path, activationKey).
	// It has no custom-info parameter; custom values are set separately below.
	if _, err := apiPost[any](c, "image/profile/create", map[string]any{
		"label":         p.Label,
		"type":          p.Type,
		"storeLabel":    p.StoreLabel,
		"path":          p.SourcePath,
		"activationKey": p.ActivationKey,
	}); err != nil {
		return err
	}
	if len(customInfo) > 0 {
		if _, err := apiPost[any](c, "image/profile/setCustomValues", map[string]any{
			"label":  p.Label,
			"values": customInfo,
		}); err != nil {
			return err
		}
	}
	return nil
}

func (c *Client) GetImageProfile(ctx context.Context, label string) (*ImageProfileDetails, error) {
	r, err := apiGet[wireImageProfile](c, "image/profile/getDetails?label="+url.QueryEscape(label))
	if err != nil {
		return nil, asNotFound(err)
	}
	return &ImageProfileDetails{
		ID:            r.ID,
		Label:         r.Label,
		Type:          r.Type,
		StoreLabel:    r.StoreLabel,
		ActivationKey: r.ActivationKey,
		SourcePath:    r.Path,
	}, nil
}

func (c *Client) UpdateImageProfile(ctx context.Context, label string, details map[string]any) error {
	// image.profile.setDetails(label, details{storeLabel, path, activationKey}).
	_, err := apiPost[any](c, "image/profile/setDetails", map[string]any{
		"label":   label,
		"details": details,
	})
	return err
}

func (c *Client) DeleteImageProfile(ctx context.Context, label string) error {
	_, err := apiPost[any](c, "image/profile/delete", map[string]any{
		"label": label,
	})
	return asNotFound(err)
}

func (c *Client) ScheduleImageBuild(ctx context.Context, profileLabel, version string, buildHostID int) (int, error) {
	type resp struct {
		ActionID int `json:"action_id"`
	}
	r, err := apiPost[resp](c, "image/scheduleImageBuild", map[string]any{
		"profile_label": profileLabel,
		"version":       version,
		"build_host_id": buildHostID,
	})
	if err != nil {
		return 0, err
	}
	return r.ActionID, nil
}

func (c *Client) ListImagesForProfile(ctx context.Context, profileLabel string) ([]ImageInfo, error) {
	list, err := apiGet[[]wireImageInfo](c, "image/listImages?profile_label="+url.QueryEscape(profileLabel))
	if err != nil {
		return nil, err
	}
	out := make([]ImageInfo, len(list))
	for i, img := range list {
		out[i] = ImageInfo{
			ID:           img.ID,
			Name:         img.Name,
			Version:      img.Version,
			Revision:     img.Revision,
			BuildStatus:  img.BuildStatus,
			ProfileLabel: img.ProfileLabel,
		}
	}
	return out, nil
}

// =============================================================================
// Scheduled actions (tasks)
// =============================================================================

func (c *Client) ScheduleHighstate(ctx context.Context, serverIDs []int, earliest time.Time, test bool) (int, error) {
	type resp struct {
		ActionID int `json:"action_id"`
	}
	r, err := apiPost[resp](c, "system/scheduleApplyHighstate", map[string]any{
		"sid":                 serverIDs,
		"earliest_occurrence": earliest.Format(time.RFC3339),
		"test":                test,
	})
	if err != nil {
		return 0, err
	}
	return r.ActionID, nil
}

func (c *Client) ScheduleRemoteCommand(ctx context.Context, serverIDs []int, earliest time.Time, command, user, group string, timeoutSeconds int) (int, error) {
	type resp struct {
		ActionID int `json:"action_id"`
	}
	r, err := apiPost[resp](c, "system/scheduleScriptRun", map[string]any{
		"sids":                serverIDs,
		"username":            user,
		"groupname":           group,
		"timeout":             timeoutSeconds,
		"script":              command,
		"earliest_occurrence": earliest.Format(time.RFC3339),
	})
	if err != nil {
		return 0, err
	}
	return r.ActionID, nil
}

func (c *Client) ScheduleReboot(ctx context.Context, serverIDs []int, earliest time.Time) ([]int, error) {
	type resp struct {
		ActionIDs []int `json:"action_ids"`
	}
	r, err := apiPost[resp](c, "system/scheduleReboot", map[string]any{
		"sids":                serverIDs,
		"earliest_occurrence": earliest.Format(time.RFC3339),
	})
	if err != nil {
		return nil, err
	}
	return r.ActionIDs, nil
}

func (c *Client) ScheduleApplyPatches(ctx context.Context, serverIDs []int, earliest time.Time, advisoryNames []string) ([]int, error) {
	type resp struct {
		ActionIDs []int `json:"action_ids"`
	}
	r, err := apiPost[resp](c, "system/scheduleApplyErrata", map[string]any{
		"sids":                serverIDs,
		"errata_names":        advisoryNames,
		"earliest_occurrence": earliest.Format(time.RFC3339),
	})
	if err != nil {
		return nil, err
	}
	return r.ActionIDs, nil
}

func (c *Client) ScheduleApplyConfigChannels(ctx context.Context, serverIDs []int, earliest time.Time) (int, error) {
	type resp struct {
		ActionID int `json:"action_id"`
	}
	r, err := apiPost[resp](c, "system/scheduleApplyConfigChannel", map[string]any{
		"sids":                serverIDs,
		"earliest_occurrence": earliest.Format(time.RFC3339),
	})
	if err != nil {
		return 0, err
	}
	return r.ActionID, nil
}

func (c *Client) GetActionDetails(ctx context.Context, actionID int) (*ScheduledAction, error) {
	r, err := apiGet[wireAction](c, fmt.Sprintf("schedule/getScheduledActionDetails?action_id=%d", actionID))
	if err != nil {
		return nil, asNotFound(err)
	}
	started, _ := time.Parse(time.RFC3339, r.StartedAt)
	finished, _ := time.Parse(time.RFC3339, r.FinishedAt)
	return &ScheduledAction{
		ID:         r.ID,
		Name:       r.Name,
		Type:       r.Type,
		Status:     r.Status,
		StartedAt:  started,
		FinishedAt: finished,
	}, nil
}

func (c *Client) GetActionResults(ctx context.Context, actionID int) ([]SystemActionResult, error) {
	list, err := apiGet[[]wireActionResult](c, fmt.Sprintf("schedule/listCompletedSystems?action_id=%d", actionID))
	if err != nil {
		return nil, err
	}
	out := make([]SystemActionResult, len(list))
	for i, ar := range list {
		out[i] = SystemActionResult{
			ServerID: ar.ServerID,
			ActionID: ar.ActionID,
			Status:   ar.Status,
			Result:   ar.Result,
			ExitCode: ar.ExitCode,
		}
	}
	return out, nil
}

func (c *Client) CancelAction(ctx context.Context, actionID int) error {
	_, err := apiPost[any](c, "schedule/cancelActions", map[string]any{
		"action_ids": []int{actionID},
	})
	return err
}

// =============================================================================
// Organization management (satellite admin)
// =============================================================================

type wireOrg struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

func (c *Client) CreateOrganization(ctx context.Context, name, adminLogin, adminPass, adminFirstName, adminLastName, adminEmail string) (int, error) {
	r, err := apiPost[wireOrg](c, "org/create", map[string]any{
		"orgName":       name,
		"adminLogin":    adminLogin,
		"adminPassword": adminPass,
		"prefix":        "Mr.",
		"firstName":     adminFirstName,
		"lastName":      adminLastName,
		"email":         adminEmail,
		"usePamAuth":    false,
	})
	if err != nil {
		return 0, err
	}
	return r.ID, nil
}

func (c *Client) GetOrganizationByID(ctx context.Context, id int) (*OrgDetails, error) {
	r, err := apiGet[wireOrg](c, fmt.Sprintf("org/getDetails?orgId=%d", id))
	if err != nil {
		return nil, asNotFound(err)
	}
	return &OrgDetails{ID: r.ID, Name: r.Name}, nil
}

func (c *Client) GetOrganizationByName(ctx context.Context, name string) (*OrgDetails, error) {
	r, err := apiGet[wireOrg](c, "org/getDetails?name="+url.QueryEscape(name))
	if err != nil {
		return nil, asNotFound(err)
	}
	return &OrgDetails{ID: r.ID, Name: r.Name}, nil
}

func (c *Client) UpdateOrganizationName(ctx context.Context, id int, name string) error {
	_, err := apiPost[any](c, "org/updateName", map[string]any{
		"orgId": id,
		"name":  name,
	})
	return err
}

func (c *Client) DeleteOrganization(ctx context.Context, id int) error {
	_, err := apiPost[any](c, "org/delete", map[string]any{
		"orgId": id,
	})
	return asNotFound(err)
}

// =============================================================================
// Autoinstall — kickstart.tree (distribution)
// =============================================================================

type wireDistribution struct {
	ID                int    `json:"id"`
	Label             string `json:"label"`
	BasePath          string `json:"base_path"`
	ChannelLabel      string `json:"channel_label"`
	InstallType       string `json:"install_type"`
	KernelOptions     string `json:"kernel_options"`
	PostKernelOptions string `json:"post_kernel_options"`
}

func wireDistToDetails(w *wireDistribution) *DistributionDetails {
	return &DistributionDetails{
		ID:                w.ID,
		Label:             w.Label,
		BasePath:          w.BasePath,
		ChannelLabel:      w.ChannelLabel,
		InstallType:       w.InstallType,
		KernelOptions:     w.KernelOptions,
		PostKernelOptions: w.PostKernelOptions,
	}
}

func (c *Client) CreateDistribution(ctx context.Context, d DistributionDetails) error {
	_, err := apiPost[any](c, "kickstart/tree/create", map[string]any{
		"treelabel":          d.Label,
		"basepath":           d.BasePath,
		"channel_label":      d.ChannelLabel,
		"installtype_label":  d.InstallType,
		"kernel_options":     d.KernelOptions,
		"post_kernel_options": d.PostKernelOptions,
	})
	return err
}

func (c *Client) GetDistribution(ctx context.Context, label string) (*DistributionDetails, error) {
	r, err := apiGet[wireDistribution](c, "kickstart/tree/getDetails?treeLabel="+url.QueryEscape(label))
	if err != nil {
		return nil, asNotFound(err)
	}
	return wireDistToDetails(&r), nil
}

func (c *Client) UpdateDistribution(ctx context.Context, label string, d DistributionDetails) error {
	_, err := apiPost[any](c, "kickstart/tree/update", map[string]any{
		"treelabel":          label,
		"basepath":           d.BasePath,
		"channel_label":      d.ChannelLabel,
		"installtype_label":  d.InstallType,
		"kernel_options":     d.KernelOptions,
		"post_kernel_options": d.PostKernelOptions,
	})
	return err
}

func (c *Client) DeleteDistribution(ctx context.Context, label string) error {
	_, err := apiPost[any](c, "kickstart/tree/delete", map[string]any{
		"treelabel": label,
	})
	return asNotFound(err)
}

// =============================================================================
// Autoinstall — kickstart / kickstart.profile
// =============================================================================

type wireProfile struct {
	Label              string `json:"label"`
	VirtualizationType string `json:"virtualization_type"`
	TreeLabel          string `json:"tree_label"`
	UpdateType         string `json:"update_type"`
}

type wireProfileScript struct {
	ScriptID    int    `json:"script_id"`
	Contents    string `json:"contents"`
	Interpreter string `json:"interpreter"`
	ScriptType  string `json:"script_type"` // "pre" | "post"
	Chroot      bool   `json:"chroot"`
	Template    bool   `json:"template"`
	ErrorOnFail bool   `json:"error_on_fail"`
}

// scriptNamePrefix is the sentinel embedded at the start of script contents to
// carry the reconcile name through Uyuni's API, which has no separate name field.
const scriptNamePrefix = "#ks-name:"

func encodeScriptName(name, contents string) string {
	return scriptNamePrefix + name + "\n" + contents
}

func decodeScriptName(encoded string) (name, contents string) {
	if after, ok := strings.CutPrefix(encoded, scriptNamePrefix); ok {
		nl := strings.IndexByte(after, '\n')
		if nl >= 0 {
			return after[:nl], after[nl+1:]
		}
		return after, ""
	}
	return "", encoded
}

func (c *Client) CreateProfile(ctx context.Context, args ProfileCreateArgs) error {
	_, err := apiPost[any](c, "kickstart/createProfile", map[string]any{
		"profile_label":       args.Label,
		"vm_type":             args.VirtualizationType,
		"kickstart_host":      args.KickstartHost,
		"kickstart_tree_label": args.TreeLabel,
		"download_url":        "",
		"root_password":       args.RootPassword,
	})
	return err
}

func (c *Client) ImportProfile(ctx context.Context, args ProfileImportArgs) error {
	_, err := apiPost[any](c, "kickstart/importFile", map[string]any{
		"profile_label":       args.Label,
		"virtualization_type": "none",
		"kickstart_host":      args.KickstartHost,
		"kickstart_tree_label": args.TreeLabel,
		"file_contents":       args.Contents,
	})
	return err
}

func (c *Client) GetProfile(ctx context.Context, label string) (*ProfileDetails, error) {
	r, err := apiGet[wireProfile](c, "kickstart/getDetails?ksLabel="+url.QueryEscape(label))
	if err != nil {
		return nil, asNotFound(err)
	}
	return &ProfileDetails{
		Label:              r.Label,
		VirtualizationType: r.VirtualizationType,
		TreeLabel:          r.TreeLabel,
		UpdateType:         r.UpdateType,
	}, nil
}

func (c *Client) DeleteProfile(ctx context.Context, label string) error {
	_, err := apiPost[any](c, "kickstart/deleteProfile", map[string]any{
		"ksLabel": label,
	})
	return asNotFound(err)
}

func (c *Client) SetProfileChildChannels(ctx context.Context, label string, channelLabels []string) error {
	_, err := apiPost[any](c, "kickstart/profile/software/setSoftwareList", map[string]any{
		"ksLabel":      label,
		"channels":     channelLabels,
		"upgradeable":  false,
	})
	return err
}

func (c *Client) GetProfileChildChannels(ctx context.Context, label string) ([]string, error) {
	type chanRef struct {
		Label string `json:"label"`
	}
	list, err := apiGet[[]chanRef](c, "kickstart/profile/software/getSoftwareList?ksLabel="+url.QueryEscape(label))
	if err != nil {
		return nil, err
	}
	out := make([]string, len(list))
	for i, cr := range list {
		out[i] = cr.Label
	}
	return out, nil
}

func (c *Client) SetProfileVariables(ctx context.Context, label string, vars map[string]string) error {
	_, err := apiPost[any](c, "kickstart/profile/setVariables", map[string]any{
		"ksLabel":   label,
		"variables": vars,
	})
	return err
}

func (c *Client) GetProfileVariables(ctx context.Context, label string) (map[string]string, error) {
	r, err := apiGet[map[string]string](c, "kickstart/profile/getVariables?ksLabel="+url.QueryEscape(label))
	if err != nil {
		return nil, err
	}
	return r, nil
}

func (c *Client) SetProfileUpdateType(ctx context.Context, label, updateType string) error {
	_, err := apiPost[any](c, "kickstart/profile/setAdvancedOptions", map[string]any{
		"ksLabel": label,
		"options": []map[string]any{
			{"name": "update_type", "enabled": true, "value": updateType},
		},
	})
	return err
}

func (c *Client) SetProfileCfgPreservation(ctx context.Context, label string, preserve bool) error {
	_, err := apiPost[any](c, "kickstart/profile/setAdvancedOptions", map[string]any{
		"ksLabel": label,
		"options": []map[string]any{
			{"name": "preserveFiles", "enabled": preserve},
		},
	})
	return err
}

func (c *Client) AddProfileScript(ctx context.Context, label string, s ProfileScript) (int, error) {
	r, err := apiPost[wireProfileScript](c, "kickstart/profile/script/addScript", map[string]any{
		"ksLabel":      label,
		"contents":     encodeScriptName(s.Name, s.Contents),
		"interpreter":  s.Interpreter,
		"script_type":  s.Type,
		"chroot":       s.Chroot,
		"template":     s.Template,
		"errorOnFail":  s.ErrorOnFail,
	})
	if err != nil {
		return 0, err
	}
	return r.ScriptID, nil
}

func (c *Client) ListProfileScripts(ctx context.Context, label string) ([]ProfileScript, error) {
	list, err := apiGet[[]wireProfileScript](c, "kickstart/profile/script/listScripts?ksLabel="+url.QueryEscape(label))
	if err != nil {
		return nil, err
	}
	out := make([]ProfileScript, len(list))
	for i, ws := range list {
		name, contents := decodeScriptName(ws.Contents)
		out[i] = ProfileScript{
			ID:          ws.ScriptID,
			Name:        name,
			Contents:    contents,
			Interpreter: ws.Interpreter,
			Type:        ws.ScriptType,
			Chroot:      ws.Chroot,
			Template:    ws.Template,
			ErrorOnFail: ws.ErrorOnFail,
		}
	}
	return out, nil
}

func (c *Client) RemoveProfileScript(ctx context.Context, label string, scriptID int) error {
	_, err := apiPost[any](c, "kickstart/profile/script/removeScript", map[string]any{
		"ksLabel":   label,
		"script_id": scriptID,
	})
	return err
}
