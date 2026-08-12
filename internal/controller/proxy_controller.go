package controller

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	uyuniv1 "github.com/mborodin/uyuni-operator/api/v1alpha1"
	"github.com/mborodin/uyuni-operator/internal/uyuni"
)

// nonSensitiveArchiveFiles is the allowlist of proxy-config archive members
// whose contents are safe to inline into status. Everything else (certs, keys,
// SSH material) lives only in the owned Secret. Safe-by-default: an unknown
// member is never inlined.
var nonSensitiveArchiveFiles = map[string]bool{
	"config.yaml": true,
}

type ProxyReconciler struct {
	client.Client
	Clients uyuni.ClientPool
	Now     func() time.Time
}

// +kubebuilder:rbac:groups=uyuni.uyuni-project.org,resources=proxies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=uyuni.uyuni-project.org,resources=proxies/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=uyuni.uyuni-project.org,resources=proxies/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch;delete

func (r *ProxyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var proxy uyuniv1.Proxy
	if err := r.Get(ctx, req.NamespacedName, &proxy); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if migrateAnnotations(&proxy) {
		return ctrl.Result{}, r.Update(ctx, &proxy)
	}

	// Deletion needs no provider client (the owned Secret is GC'd and there is no
	// Uyuni-side teardown), so handle it before provider resolution — a Proxy
	// must be deletable even when its provider is gone.
	if !proxy.DeletionTimestamp.IsZero() {
		return r.handleDeletion(ctx, &proxy)
	}

	uc, err := r.Clients.For(ctx, toProviderRef(&proxy.Spec.ProviderRef), proxy.Namespace)
	if err != nil {
		return r.fail(ctx, &proxy, "ProviderError", err)
	}

	if ensureFinalizer(&proxy, proxyFinalizer) {
		return ctrl.Result{Requeue: true}, r.Update(ctx, &proxy)
	}

	// Resolve the (optional) TLS material from the referenced Secret. Missing at
	// runtime is a recoverable wait — GitOps may apply the Secret later.
	tls, wait, err := r.resolveTLS(ctx, &proxy)
	if err != nil {
		return r.fail(ctx, &proxy, "ResolveRefs", err)
	}
	if wait != "" {
		setReady(&proxy.Status.Conditions, proxy.Generation, metav1.ConditionFalse, "ReferenceUnavailable", wait)
		if err := r.Status().Update(ctx, &proxy); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	parentServer := proxy.Spec.ParentServer
	if parentServer == "" {
		parentServer = uc.ServerHost()
	}

	inputHash := proxyInputHash(&proxy, parentServer, tls)
	_, forced := proxy.Annotations[uyuniv1.AnnRegenerate]

	// Regeneration rotates the proxy SSH keypair, so gate it: only (re)generate
	// when the resolved inputs change or the operator is explicitly asked to.
	if inputHash != proxy.Status.InputHash || forced {
		archive, err := uc.GenerateProxyContainerConfig(ctx, uyuni.ProxyContainerConfigArgs{
			ProxyName:       proxy.Spec.FQDN,
			ParentServer:    parentServer,
			ProxyPort:       proxy.Spec.SSHPort,
			MaxCacheMB:      proxy.Spec.MaxCacheMB,
			Email:           proxy.Spec.Email,
			RootCA:          tls.rootCA,
			IntermediateCAs: tls.intermediateCAs,
			ProxyCrt:        tls.crt,
			ProxyKey:        tls.key,
		})
		if err != nil {
			return r.fail(ctx, &proxy, "CreateFailed", err)
		}

		files, err := extractArchive(archive)
		if err != nil {
			return r.fail(ctx, &proxy, "CreateFailed", fmt.Errorf("extracting proxy config archive: %w", err))
		}

		secretName := proxy.Status.SecretName
		if secretName == "" {
			secretName = proxy.Name + "-config"
		}
		if err := r.upsertSecret(ctx, &proxy, secretName, files); err != nil {
			return r.fail(ctx, &proxy, "UpdateFailed", fmt.Errorf("writing proxy config Secret: %w", err))
		}

		names := make([]string, 0, len(files))
		for name := range files {
			names = append(names, name)
		}
		sort.Strings(names)

		now := metav1.NewTime(r.now())
		proxy.Status.SecretName = secretName
		proxy.Status.Files = names
		proxy.Status.Config = inlineNonSensitive(files)
		proxy.Status.InputHash = inputHash
		proxy.Status.GeneratedAt = &now
	}

	proxy.Status.ObservedGeneration = proxy.Generation
	setReady(&proxy.Status.Conditions, proxy.Generation, metav1.ConditionTrue, "Reconciled", "")
	if err := r.Status().Update(ctx, &proxy); err != nil {
		return ctrl.Result{}, err
	}

	// Strip the one-shot regenerate annotation only after status is durable.
	if forced {
		delete(proxy.Annotations, uyuniv1.AnnRegenerate)
		if err := r.Update(ctx, &proxy); err != nil {
			return ctrl.Result{}, err
		}
	}

	return ctrl.Result{RequeueAfter: 5 * time.Minute}, nil
}

func (r *ProxyReconciler) handleDeletion(ctx context.Context, proxy *uyuniv1.Proxy) (ctrl.Result, error) {
	if !containsFinalizer(proxy, proxyFinalizer) {
		return ctrl.Result{}, nil
	}
	// No Uyuni-side teardown: proxy.containerConfig has no deregistration API,
	// and the owned config Secret is garbage-collected via its ownerReference.
	removeFinalizer(proxy, proxyFinalizer)
	return ctrl.Result{}, r.Update(ctx, proxy)
}

// tlsMaterial holds the PEM material resolved from the referenced TLS Secret.
type tlsMaterial struct {
	crt             string
	key             string
	rootCA          string
	intermediateCAs []string
}

// resolveTLS reads the proxy certificate from spec.tlsSecretRef. Returns a
// non-empty wait string (not an error) when the Secret does not exist yet, so
// the reconciler can requeue GitOps-style. A nil ref yields empty material
// (the no-cert generation overload).
func (r *ProxyReconciler) resolveTLS(ctx context.Context, proxy *uyuniv1.Proxy) (tlsMaterial, string, error) {
	if proxy.Spec.TLSSecretRef == nil {
		return tlsMaterial{}, "", nil
	}
	var sec corev1.Secret
	key := types.NamespacedName{Namespace: proxy.Namespace, Name: proxy.Spec.TLSSecretRef.Name}
	if err := r.Get(ctx, key, &sec); err != nil {
		if apierrors.IsNotFound(err) {
			return tlsMaterial{}, fmt.Sprintf("TLS secret %q not found yet", proxy.Spec.TLSSecretRef.Name), nil
		}
		return tlsMaterial{}, "", err
	}
	m := tlsMaterial{
		crt:    string(sec.Data["tls.crt"]),
		key:    string(sec.Data["tls.key"]),
		rootCA: string(sec.Data["ca.crt"]),
	}
	if inter := string(sec.Data["ca-intermediate.crt"]); inter != "" {
		m.intermediateCAs = []string{inter}
	}
	if m.crt == "" || m.key == "" {
		return tlsMaterial{}, "", fmt.Errorf("TLS secret %q missing tls.crt or tls.key", proxy.Spec.TLSSecretRef.Name)
	}
	return m, "", nil
}

// upsertSecret creates or updates the operator-owned Secret holding the archive
// files, one key per member. The Proxy owns it (controller ref) so it is GC'd
// on delete.
func (r *ProxyReconciler) upsertSecret(ctx context.Context, proxy *uyuniv1.Proxy, name string, files map[string][]byte) error {
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: proxy.Namespace, Name: name},
	}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, sec, func() error {
		if err := controllerutil.SetControllerReference(proxy, sec, r.Scheme()); err != nil {
			return err
		}
		sec.Type = corev1.SecretTypeOpaque
		sec.Data = files
		return nil
	})
	return err
}

func (r *ProxyReconciler) fail(ctx context.Context, proxy *uyuniv1.Proxy, reason string, err error) (ctrl.Result, error) {
	setReady(&proxy.Status.Conditions, proxy.Generation, metav1.ConditionFalse, reason, err.Error())
	_ = r.Status().Update(ctx, proxy)
	return ctrl.Result{}, err
}

func (r *ProxyReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *ProxyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&uyuniv1.Proxy{}).
		Owns(&corev1.Secret{}).
		Complete(r)
}

// extractArchive decompresses a gzip'd tar and returns each member's bytes
// keyed by its (flattened) base name. The proxy config archive is flat, so the
// base name is a valid Secret key.
func extractArchive(b []byte) (map[string][]byte, error) {
	gz, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		return nil, fmt.Errorf("gzip: %w", err)
	}
	defer gz.Close()

	out := map[string][]byte{}
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("tar: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			return nil, fmt.Errorf("reading %q: %w", hdr.Name, err)
		}
		out[path.Base(hdr.Name)] = data
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("archive contained no regular files")
	}
	return out, nil
}

// inlineNonSensitive concatenates the contents of the allowlisted, non-secret
// archive members for display in status. Order is deterministic.
func inlineNonSensitive(files map[string][]byte) string {
	names := make([]string, 0, len(files))
	for name := range files {
		if nonSensitiveArchiveFiles[name] {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	var sb strings.Builder
	for i, name := range names {
		if i > 0 {
			sb.WriteString("\n")
		}
		if len(names) > 1 {
			sb.WriteString("# " + name + "\n")
		}
		sb.Write(files[name])
	}
	return sb.String()
}

// proxyInputHash hashes every input that affects the generated archive so that
// regeneration happens exactly when an input changes.
func proxyInputHash(proxy *uyuniv1.Proxy, parentServer string, tls tlsMaterial) string {
	h := sha256.New()
	fmt.Fprintf(h, "fqdn=%s\x00server=%s\x00port=%d\x00cache=%d\x00email=%s\x00",
		proxy.Spec.FQDN, parentServer, proxy.Spec.SSHPort, proxy.Spec.MaxCacheMB, proxy.Spec.Email)
	fmt.Fprintf(h, "crt=%s\x00key=%s\x00ca=%s\x00", tls.crt, tls.key, tls.rootCA)
	for _, i := range tls.intermediateCAs {
		fmt.Fprintf(h, "inter=%s\x00", i)
	}
	return hex.EncodeToString(h.Sum(nil))
}
