package controller

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// urlCheckTimeout bounds each reachability probe so a hung mirror can't
// stall a reconcile loop.
const urlCheckTimeout = 10 * time.Second

// checkRepoURLReachable does a best-effort reachability probe for a
// Repository's URL. Only http(s) is checked — file:// is local to the
// Uyuni server (not this operator), uln:// is a Uyuni-internal protocol,
// and ftp:// isn't worth a bespoke client for a probe. Those schemes are
// treated as unverifiable and pass silently rather than false-flagging.
func checkRepoURLReachable(ctx context.Context, rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("invalid URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil
	}

	ctx, cancel := context.WithTimeout(ctx, urlCheckTimeout)
	defer cancel()

	client := &http.Client{}

	req, err := http.NewRequestWithContext(ctx, http.MethodHead, rawURL, nil)
	if err != nil {
		return fmt.Errorf("building request: %w", err)
	}
	resp, err := client.Do(req)
	if err == nil {
		_ = resp.Body.Close()
		if resp.StatusCode < 400 {
			return nil
		}
		if resp.StatusCode != http.StatusMethodNotAllowed && resp.StatusCode != http.StatusNotImplemented {
			return fmt.Errorf("HTTP %d", resp.StatusCode)
		}
		// Some servers don't support HEAD; fall through to GET.
	}

	// Retry with GET — either HEAD transport-failed or the server rejected
	// the method. A GET failure here is the one we actually report.
	req, err = http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return fmt.Errorf("building request: %w", err)
	}
	resp, err = client.Do(req)
	if err != nil {
		return fmt.Errorf("request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return nil
}
