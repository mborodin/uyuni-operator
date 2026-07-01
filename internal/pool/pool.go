// Package pool provides a concrete uyuni.ClientPool implementation that
// resolves UyuniProvider CRs from the Kubernetes API and caches the
// resulting Uyuni API clients.
package pool

import (
	"context"
	"fmt"
	"sync"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	uyuniv1 "github.com/mborodin/uyuni-operator/api/v1alpha1"
	"github.com/mborodin/uyuni-operator/internal/uyuni"
)

type entry struct {
	api   uyuni.API
	orgID int
}

type orgEntry struct {
	api          uyuni.API
	providerName string // non-empty when the org has no own credentials and shares the provider client
}

// Pool is a concrete uyuni.ClientPool. It lazily builds API clients from
// UyuniProvider CRs (provider-level) and Organization CRs (org-level),
// caching both. Invalidate/InvalidateOrg evict entries so the next call
// rebuilds from fresh credentials.
type Pool struct {
	client     client.Client
	operatorNS string

	mu       sync.RWMutex
	cache    map[string]entry    // keyed by provider name
	orgCache map[string]orgEntry // keyed by "namespace/name"
}

// New returns an initialised Pool. operatorNS is the namespace where
// credential Secrets are expected to live.
func New(c client.Client, operatorNS string) *Pool {
	return &Pool{
		client:     c,
		operatorNS: operatorNS,
		cache:      make(map[string]entry),
		orgCache:   make(map[string]orgEntry),
	}
}

// For resolves ref to a ready uyuni.API. A nil ref (or empty Name) triggers
// a lookup for the default provider (spec.isDefault=true).
func (p *Pool) For(ctx context.Context, ref *uyuni.LocalObjectRef, requestNamespace string) (uyuni.API, error) {
	name, err := p.resolveName(ctx, ref)
	if err != nil {
		return nil, err
	}

	p.mu.RLock()
	e, ok := p.cache[name]
	p.mu.RUnlock()
	if ok {
		return e.api, nil
	}

	return p.build(ctx, name)
}

func (p *Pool) resolveName(ctx context.Context, ref *uyuni.LocalObjectRef) (string, error) {
	if ref != nil && ref.Name != "" {
		return ref.Name, nil
	}
	var list uyuniv1.UyuniProviderList
	if err := p.client.List(ctx, &list); err != nil {
		return "", fmt.Errorf("listing UyuniProviders: %w", err)
	}
	for _, prov := range list.Items {
		if prov.Spec.IsDefault {
			return prov.Name, nil
		}
	}
	return "", fmt.Errorf("no default UyuniProvider found; set spec.isDefault=true on one provider")
}

// build reads the UyuniProvider CR and its credentials Secret, then
// creates and caches an API client.
func (p *Pool) build(ctx context.Context, name string) (uyuni.API, error) {
	var prov uyuniv1.UyuniProvider
	if err := p.client.Get(ctx, types.NamespacedName{Name: name}, &prov); err != nil {
		return nil, fmt.Errorf("getting UyuniProvider %q: %w", name, err)
	}

	secretNS := p.operatorNS
	if prov.Spec.CredentialsSecretRef.Namespace != "" {
		secretNS = prov.Spec.CredentialsSecretRef.Namespace
	}
	var secret corev1.Secret
	if err := p.client.Get(ctx, types.NamespacedName{
		Namespace: secretNS,
		Name:      prov.Spec.CredentialsSecretRef.Name,
	}, &secret); err != nil {
		return nil, fmt.Errorf("reading credentials secret for provider %q: %w", name, err)
	}

	username := string(secret.Data["username"])
	password := string(secret.Data["password"])
	if username == "" || password == "" {
		return nil, fmt.Errorf("credentials secret for provider %q must contain non-empty 'username' and 'password' keys", name)
	}

	// Optional CA certificate from a separate Secret.
	var caCert []byte
	if prov.Spec.CACertSecretRef != nil {
		caNS := p.operatorNS
		if prov.Spec.CACertSecretRef.Namespace != "" {
			caNS = prov.Spec.CACertSecretRef.Namespace
		}
		var caSecret corev1.Secret
		if err := p.client.Get(ctx, types.NamespacedName{
			Namespace: caNS,
			Name:      prov.Spec.CACertSecretRef.Name,
		}, &caSecret); err != nil {
			return nil, fmt.Errorf("reading CA cert secret for provider %q: %w", name, err)
		}
		caCert = caSecret.Data["ca.crt"]
	}

	c, err := uyuni.NewClient(prov.Spec.URL, username, password, prov.Spec.InsecureSkipVerify, caCert)
	if err != nil {
		return nil, fmt.Errorf("connecting to provider %q: %w", name, err)
	}

	// Fetch and cache the org ID so Pool.OrgID can return it without an
	// extra API call.
	orgID, orgErr := c.GetOrgID(ctx)
	if orgErr != nil {
		orgID = 0
	}

	p.mu.Lock()
	p.cache[name] = entry{api: c, orgID: orgID}
	p.mu.Unlock()

	return c, nil
}

// ForOrganization resolves orgName/orgNamespace to a ready uyuni.API.
// It reads the Organization CR to find the UyuniProvider (for server URL
// and TLS), then uses the org's credentialsSecretRef if set, otherwise
// falls back to the provider's satellite admin credentials.
func (p *Pool) ForOrganization(ctx context.Context, orgName string, orgNamespace string) (uyuni.API, error) {
	if orgName == "" {
		return nil, fmt.Errorf("organizationRef is required")
	}
	key := orgNamespace + "/" + orgName

	p.mu.RLock()
	if e, ok := p.orgCache[key]; ok {
		p.mu.RUnlock()
		return e.api, nil
	}
	p.mu.RUnlock()

	return p.buildForOrg(ctx, orgName, orgNamespace, key)
}

func (p *Pool) buildForOrg(ctx context.Context, orgName, orgNamespace, key string) (uyuni.API, error) {
	var org uyuniv1.Organization
	if err := p.client.Get(ctx, types.NamespacedName{Namespace: orgNamespace, Name: orgName}, &org); err != nil {
		if !apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("getting Organization %q/%q: %w", orgNamespace, orgName, err)
		}
		// Fallback: match by spec.name (Uyuni org display name) so callers can
		// use either the CR's metadata.name or the human-readable org name.
		var list uyuniv1.OrganizationList
		if listErr := p.client.List(ctx, &list, client.InNamespace(orgNamespace)); listErr != nil {
			return nil, fmt.Errorf("getting Organization %q/%q: %w", orgNamespace, orgName, err)
		}
		found := false
		for i := range list.Items {
			if list.Items[i].Spec.Name == orgName {
				org = list.Items[i]
				found = true
				break
			}
		}
		if !found {
			return nil, fmt.Errorf("getting Organization %q/%q: %w", orgNamespace, orgName, err)
		}
	}

	// If no org-specific credentials, reuse the provider client.
	if org.Spec.CredentialsSecretRef == nil {
		api, err := p.For(ctx, &uyuni.LocalObjectRef{Name: org.Spec.ProviderRef.Name}, orgNamespace)
		if err == nil {
			p.mu.Lock()
			p.orgCache[key] = orgEntry{api: api, providerName: org.Spec.ProviderRef.Name}
			p.mu.Unlock()
			return api, nil
		}
		// UyuniProvider CR may already be gone (e.g. deleted concurrently by
		// Crossplane while this org's dependents are still cleaning up).
		// Fall back to building directly from the org's provider snapshot,
		// using the provider's (satellite admin) credentials snapshot too.
		return p.buildFromSnapshot(ctx, &org, org.Status.ProviderCredentialsSecretRef, orgName, key)
	}

	providerURL, insecureSkipVerify, caCertSecretRef, err := p.resolveProviderDetails(ctx, &org)
	if err != nil {
		return nil, err
	}

	var caCert []byte
	if caCertSecretRef != nil {
		var caSecret corev1.Secret
		if err := p.client.Get(ctx, types.NamespacedName{Namespace: caCertSecretRef.Namespace, Name: caCertSecretRef.Name}, &caSecret); err != nil {
			return nil, fmt.Errorf("reading CA cert secret for Organization %q: %w", orgName, err)
		}
		caCert = caSecret.Data["ca.crt"]
	}

	// Org-specific credentials: build a separate client.
	var secret corev1.Secret
	if err := p.client.Get(ctx, types.NamespacedName{
		Namespace: orgNamespace,
		Name:      org.Spec.CredentialsSecretRef.Name,
	}, &secret); err != nil {
		return nil, fmt.Errorf("reading org credentials secret for Organization %q: %w", orgName, err)
	}
	username := string(secret.Data["username"])
	password := string(secret.Data["password"])
	if username == "" || password == "" {
		return nil, fmt.Errorf("org credentials secret for Organization %q must contain non-empty 'username' and 'password' keys", orgName)
	}

	c, err := uyuni.NewClient(providerURL, username, password, insecureSkipVerify, caCert)
	if err != nil {
		return nil, fmt.Errorf("connecting for Organization %q: %w", orgName, err)
	}

	p.mu.Lock()
	p.orgCache[key] = orgEntry{api: c}
	p.mu.Unlock()
	return c, nil
}

// resolveProviderDetails returns the org's UyuniProvider connection details,
// preferring the live UyuniProvider CR but falling back to org.Status's
// provider snapshot when the CR is gone (see Organization controller's
// snapshotProvider). Without this fallback, every org-scoped resource
// controller gets stuck forever if Crossplane deletes the UyuniProvider
// concurrently with the Organization, before dependents finish cleanup.
func (p *Pool) resolveProviderDetails(ctx context.Context, org *uyuniv1.Organization) (url string, insecureSkipVerify bool, caCertSecretRef *corev1.SecretReference, err error) {
	var prov uyuniv1.UyuniProvider
	if err := p.client.Get(ctx, types.NamespacedName{Name: org.Spec.ProviderRef.Name}, &prov); err == nil {
		return prov.Spec.URL, prov.Spec.InsecureSkipVerify, prov.Spec.CACertSecretRef, nil
	}

	if org.Status.ProviderURL != "" {
		return org.Status.ProviderURL, org.Status.ProviderInsecureSkipVerify, org.Status.ProviderCACertSecretRef, nil
	}

	return "", false, nil, fmt.Errorf("getting UyuniProvider %q for Organization %q: not found, and Organization has no provider snapshot", org.Spec.ProviderRef.Name, org.Name)
}

// buildFromSnapshot builds a client for an org with no org-specific
// credentials (shared provider client), using org.Status's provider
// snapshot — including the provider's own (satellite admin) credentials
// secret — since the live UyuniProvider CR is gone.
func (p *Pool) buildFromSnapshot(ctx context.Context, org *uyuniv1.Organization, credRef *corev1.SecretReference, orgName, key string) (uyuni.API, error) {
	if org.Status.ProviderURL == "" || credRef == nil {
		return nil, fmt.Errorf("getting UyuniProvider %q for Organization %q: not found, and Organization has no provider snapshot", org.Spec.ProviderRef.Name, orgName)
	}

	var secret corev1.Secret
	if err := p.client.Get(ctx, types.NamespacedName{Namespace: credRef.Namespace, Name: credRef.Name}, &secret); err != nil {
		return nil, fmt.Errorf("reading provider credentials secret for Organization %q: %w", orgName, err)
	}
	username := string(secret.Data["username"])
	password := string(secret.Data["password"])
	if username == "" || password == "" {
		return nil, fmt.Errorf("provider credentials secret for Organization %q must contain non-empty 'username' and 'password' keys", orgName)
	}

	var caCert []byte
	if caRef := org.Status.ProviderCACertSecretRef; caRef != nil {
		var caSecret corev1.Secret
		if err := p.client.Get(ctx, types.NamespacedName{Namespace: caRef.Namespace, Name: caRef.Name}, &caSecret); err != nil {
			return nil, fmt.Errorf("reading CA cert secret for Organization %q: %w", orgName, err)
		}
		caCert = caSecret.Data["ca.crt"]
	}

	c, err := uyuni.NewClient(org.Status.ProviderURL, username, password, org.Status.ProviderInsecureSkipVerify, caCert)
	if err != nil {
		return nil, fmt.Errorf("connecting for Organization %q: %w", orgName, err)
	}

	p.mu.Lock()
	p.orgCache[key] = orgEntry{api: c}
	p.mu.Unlock()
	return c, nil
}

// Invalidate evicts the cached client for providerName so the next For call
// recreates it. Org cache entries that share the provider client (no own
// credentials) are also evicted so they rebuild against the fresh client.
func (p *Pool) Invalidate(providerName string) {
	p.mu.Lock()
	delete(p.cache, providerName)
	for k, e := range p.orgCache {
		if e.providerName == providerName {
			delete(p.orgCache, k)
		}
	}
	p.mu.Unlock()
}

// InvalidateOrg evicts the cached org client. orgKey is "namespace/name".
// Called by OrganizationReconciler when credentials change.
func (p *Pool) InvalidateOrg(orgKey string) {
	p.mu.Lock()
	delete(p.orgCache, orgKey)
	p.mu.Unlock()
}

// OrgID returns the cached Uyuni org ID for providerName.
func (p *Pool) OrgID(providerName string) (int, bool) {
	p.mu.RLock()
	e, ok := p.cache[providerName]
	p.mu.RUnlock()
	return e.orgID, ok
}
