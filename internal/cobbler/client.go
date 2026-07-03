package cobbler

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
)

// Client talks to Cobbler's XMLRPC API at {server}/cobbler_api. Reads
// (get_*/find_*) need no authentication; writes (new/modify/save/remove) take a
// token obtained via login with the UyuniProvider credentials — this requires
// cobbler's redhat_management_permissive setting to be enabled, otherwise
// per-user login is refused (the user still needs config_admin or org_admin).
type Client struct {
	url  string
	user string
	pass string
	http *http.Client

	mu    sync.Mutex
	token string
}

// New builds a Cobbler client. serverURL is the Uyuni base URL (e.g.
// https://uyuni.example.test); httpClient carries the TLS configuration.
func New(serverURL, user, pass string, httpClient *http.Client) *Client {
	return &Client{
		url:  strings.TrimSuffix(serverURL, "/") + "/cobbler_api",
		user: user,
		pass: pass,
		http: httpClient,
	}
}

func (c *Client) call(ctx context.Context, method string, args ...any) (any, error) {
	body, err := encodeRequest(method, args...)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "text/xml")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("cobbler %s: %w", method, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("cobbler %s: HTTP %d", method, resp.StatusCode)
	}
	return decodeResponse(data)
}

func (c *Client) ensureToken(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != "" {
		return c.token, nil
	}
	res, err := c.call(ctx, "login", c.user, c.pass)
	if err != nil {
		return "", fmt.Errorf("cobbler login: %w", err)
	}
	tok, _ := res.(string)
	if tok == "" {
		return "", fmt.Errorf("cobbler login returned an empty token")
	}
	c.token = tok
	return tok, nil
}

func (c *Client) resetToken() {
	c.mu.Lock()
	c.token = ""
	c.mu.Unlock()
}

// isAuthFault reports whether err is a token/login fault that a re-login may fix.
func isAuthFault(err error) bool {
	var f *FaultError
	if errors.As(err, &f) {
		msg := strings.ToLower(f.Message)
		return strings.Contains(msg, "token") || strings.Contains(msg, "login") || strings.Contains(msg, "invalid")
	}
	return false
}

// --- reads (unauthenticated) ---

// GetSystem returns the system's fields and whether it exists. Cobbler returns a
// "~"/empty marker (a plain string, not a struct) for a missing item.
func (c *Client) GetSystem(ctx context.Context, name string) (map[string]any, bool, error) {
	return c.getItem(ctx, "get_system", name)
}

func (c *Client) GetProfile(ctx context.Context, name string) (map[string]any, bool, error) {
	return c.getItem(ctx, "get_profile", name)
}

func (c *Client) GetDistro(ctx context.Context, name string) (map[string]any, bool, error) {
	return c.getItem(ctx, "get_distro", name)
}

func (c *Client) getItem(ctx context.Context, method, name string) (map[string]any, bool, error) {
	res, err := c.call(ctx, method, name)
	if err != nil {
		return nil, false, err
	}
	m, ok := res.(map[string]any)
	if !ok {
		return nil, false, nil // "~" marker -> not found
	}
	return m, true, nil
}

// --- system writes ---

// SystemInterface is one network interface of a cobbler system record.
type SystemInterface struct {
	Name    string
	MAC     string
	IP      string
	DNSName string
}

// SystemSpec is the desired state of a cobbler system record.
type SystemSpec struct {
	Name            string
	Profile         string
	Netboot         bool
	AutoinstallMeta map[string]string
	Interfaces      []SystemInterface
	Server          string // proxy/server override; "" leaves it unset
	Comment         string
}

// UpsertSystem creates or updates a cobbler system record and returns its uid.
func (c *Client) UpsertSystem(ctx context.Context, s SystemSpec) (string, error) {
	uid, err := c.upsertSystem(ctx, s)
	if isAuthFault(err) {
		c.resetToken()
		uid, err = c.upsertSystem(ctx, s)
	}
	return uid, err
}

func (c *Client) upsertSystem(ctx context.Context, s SystemSpec) (string, error) {
	token, err := c.ensureToken(ctx)
	if err != nil {
		return "", err
	}
	handle, err := c.itemHandle(ctx, "system", s.Name, token)
	if err != nil {
		return "", err
	}
	set := func(field string, value any) error {
		if _, e := c.call(ctx, "modify_system", handle, field, value, token); e != nil {
			return fmt.Errorf("modify_system %s: %w", field, e)
		}
		return nil
	}
	if err := set("profile", s.Profile); err != nil {
		return "", err
	}
	if err := set("netboot_enabled", s.Netboot); err != nil {
		return "", err
	}
	if err := set("comment", s.Comment); err != nil {
		return "", err
	}
	if len(s.AutoinstallMeta) > 0 {
		meta := make(map[string]any, len(s.AutoinstallMeta))
		for k, v := range s.AutoinstallMeta {
			meta[k] = v
		}
		if err := set("autoinstall_meta", meta); err != nil {
			return "", err
		}
	}
	if s.Server != "" {
		if err := set("server", s.Server); err != nil {
			return "", err
		}
	}
	if len(s.Interfaces) > 0 {
		iface := map[string]any{}
		for _, nic := range s.Interfaces {
			if nic.MAC != "" {
				iface["macaddress-"+nic.Name] = nic.MAC
			}
			if nic.IP != "" {
				iface["ipaddress-"+nic.Name] = nic.IP
			}
			if nic.DNSName != "" {
				iface["dnsname-"+nic.Name] = nic.DNSName
			}
		}
		if err := set("modify_interface", iface); err != nil {
			return "", err
		}
	}
	if _, err := c.call(ctx, "save_system", handle, token); err != nil {
		return "", fmt.Errorf("save_system: %w", err)
	}
	m, found, err := c.GetSystem(ctx, s.Name)
	if err != nil || !found {
		return "", err
	}
	uid, _ := m["uid"].(string)
	return uid, nil
}

// itemHandle returns an edit handle for an existing cobbler item of the given
// kind ("system"/"profile"/"distro"), creating a new one (with its name set)
// when it doesn't exist yet.
func (c *Client) itemHandle(ctx context.Context, kind, name, token string) (string, error) {
	_, found, err := c.getItem(ctx, "get_"+kind, name)
	if err != nil {
		return "", err
	}
	if found {
		res, err := c.call(ctx, "get_"+kind+"_handle", name, token)
		if err != nil {
			return "", err
		}
		h, _ := res.(string)
		return h, nil
	}
	res, err := c.call(ctx, "new_"+kind, token)
	if err != nil {
		return "", err
	}
	h, _ := res.(string)
	if _, err := c.call(ctx, "modify_"+kind, h, "name", name, token); err != nil {
		return "", err
	}
	return h, nil
}

// RemoveSystem deletes a cobbler system record. Missing records are not an error.
func (c *Client) RemoveSystem(ctx context.Context, name string) error {
	return c.removeItem(ctx, "system", name)
}

func (c *Client) removeItem(ctx context.Context, kind, name string) error {
	err := c.removeItemOnce(ctx, kind, name)
	if isAuthFault(err) {
		c.resetToken()
		err = c.removeItemOnce(ctx, kind, name)
	}
	return err
}

func (c *Client) removeItemOnce(ctx context.Context, kind, name string) error {
	_, found, err := c.getItem(ctx, "get_"+kind, name)
	if err != nil || !found {
		return err
	}
	token, err := c.ensureToken(ctx)
	if err != nil {
		return err
	}
	_, err = c.call(ctx, "remove_"+kind, name, token)
	return err
}

// --- distro / profile writes ---

// DistroSpec is the desired state of a cobbler distribution.
type DistroSpec struct {
	Name            string
	Kernel          string
	Initrd          string
	Breed           string
	OSVersion       string
	Arch            string
	KernelOptions   string
	AutoinstallMeta map[string]string
}

// UpsertDistro creates or updates a cobbler distribution and returns its uid.
func (c *Client) UpsertDistro(ctx context.Context, d DistroSpec) (string, error) {
	fields := map[string]any{}
	putString(fields, "kernel", d.Kernel)
	putString(fields, "initrd", d.Initrd)
	putString(fields, "breed", d.Breed)
	putString(fields, "os_version", d.OSVersion)
	putString(fields, "arch", d.Arch)
	putString(fields, "kernel_options", d.KernelOptions)
	if len(d.AutoinstallMeta) > 0 {
		fields["autoinstall_meta"] = toAnyMap(d.AutoinstallMeta)
	}
	return c.upsertItem(ctx, "distro", d.Name, fields)
}

// ProfileSpec is the desired state of a cobbler profile.
type ProfileSpec struct {
	Name            string
	Distro          string
	Autoinstall     string
	KernelOptions   string
	AutoinstallMeta map[string]string
	EnableMenu      *bool
}

// UpsertProfile creates or updates a cobbler profile and returns its uid.
func (c *Client) UpsertProfile(ctx context.Context, p ProfileSpec) (string, error) {
	fields := map[string]any{}
	putString(fields, "distro", p.Distro)
	putString(fields, "autoinstall", p.Autoinstall)
	putString(fields, "kernel_options", p.KernelOptions)
	if len(p.AutoinstallMeta) > 0 {
		fields["autoinstall_meta"] = toAnyMap(p.AutoinstallMeta)
	}
	if p.EnableMenu != nil {
		fields["enable_menu"] = *p.EnableMenu
	}
	return c.upsertItem(ctx, "profile", p.Name, fields)
}

// RemoveDistro / RemoveProfile delete cobbler items; missing items are no-ops.
func (c *Client) RemoveDistro(ctx context.Context, name string) error {
	return c.removeItem(ctx, "distro", name)
}
func (c *Client) RemoveProfile(ctx context.Context, name string) error {
	return c.removeItem(ctx, "profile", name)
}

// upsertItem creates/updates a cobbler item by setting each field, then saving.
func (c *Client) upsertItem(ctx context.Context, kind, name string, fields map[string]any) (string, error) {
	uid, err := c.upsertItemOnce(ctx, kind, name, fields)
	if isAuthFault(err) {
		c.resetToken()
		uid, err = c.upsertItemOnce(ctx, kind, name, fields)
	}
	return uid, err
}

func (c *Client) upsertItemOnce(ctx context.Context, kind, name string, fields map[string]any) (string, error) {
	token, err := c.ensureToken(ctx)
	if err != nil {
		return "", err
	}
	handle, err := c.itemHandle(ctx, kind, name, token)
	if err != nil {
		return "", err
	}
	for k, v := range fields {
		if _, err := c.call(ctx, "modify_"+kind, handle, k, v, token); err != nil {
			return "", fmt.Errorf("modify_%s %s: %w", kind, k, err)
		}
	}
	if _, err := c.call(ctx, "save_"+kind, handle, token); err != nil {
		return "", fmt.Errorf("save_%s: %w", kind, err)
	}
	m, found, err := c.getItem(ctx, "get_"+kind, name)
	if err != nil || !found {
		return "", err
	}
	uid, _ := m["uid"].(string)
	return uid, nil
}

func putString(m map[string]any, key, val string) {
	if val != "" {
		m[key] = val
	}
}

func toAnyMap(m map[string]string) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// ItemAutoinstallMeta extracts the autoinstall metadata map from a read item
// (get_system/get_profile), preferring the current autoinstall_meta field and
// falling back to the legacy ks_meta.
func ItemAutoinstallMeta(item map[string]any) map[string]string {
	for _, key := range []string{"autoinstall_meta", "ks_meta"} {
		if raw, ok := item[key].(map[string]any); ok && len(raw) > 0 {
			out := make(map[string]string, len(raw))
			for k, v := range raw {
				if s, ok := v.(string); ok {
					out[k] = s
				}
			}
			return out
		}
	}
	return map[string]string{}
}
