package controller

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"testing"

	"github.com/stretchr/testify/require"

	uyuniv1 "github.com/mborodin/uyuni-operator/api/v1alpha1"
)

// makeArchive builds a gzip'd tar from name->content pairs for tests.
func makeArchive(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, content := range files {
		require.NoError(t, tw.WriteHeader(&tar.Header{
			Name:     name,
			Mode:     0o600,
			Size:     int64(len(content)),
			Typeflag: tar.TypeReg,
		}))
		_, err := tw.Write([]byte(content))
		require.NoError(t, err)
	}
	require.NoError(t, tw.Close())
	require.NoError(t, gz.Close())
	return buf.Bytes()
}

func TestExtractArchive(t *testing.T) {
	t.Run("flat members keyed by base name", func(t *testing.T) {
		b := makeArchive(t, map[string]string{
			"config.yaml": "server: uyuni\n",
			"httpd.yaml":  "server_key: SECRET\n",
			"ssh.yaml":    "proxy_ssh_key: SECRET\n",
		})
		files, err := extractArchive(b)
		require.NoError(t, err)
		require.Len(t, files, 3)
		require.Equal(t, "server: uyuni\n", string(files["config.yaml"]))
		require.Contains(t, files, "httpd.yaml")
		require.Contains(t, files, "ssh.yaml")
	})

	t.Run("nested paths are flattened to base name", func(t *testing.T) {
		b := makeArchive(t, map[string]string{"proxy/config.yaml": "x: 1\n"})
		files, err := extractArchive(b)
		require.NoError(t, err)
		require.Contains(t, files, "config.yaml")
	})

	t.Run("non-gzip input errors", func(t *testing.T) {
		_, err := extractArchive([]byte("not a gzip"))
		require.Error(t, err)
	})

	t.Run("empty archive errors", func(t *testing.T) {
		b := makeArchive(t, map[string]string{})
		_, err := extractArchive(b)
		require.Error(t, err)
	})
}

func TestInlineNonSensitive(t *testing.T) {
	t.Run("inlines only allowlisted members", func(t *testing.T) {
		out := inlineNonSensitive(map[string][]byte{
			"config.yaml": []byte("server: uyuni\n"),
			"httpd.yaml":  []byte("server_key: SECRET\n"),
			"ssh.yaml":    []byte("proxy_ssh_key: SECRET\n"),
		})
		require.Equal(t, "server: uyuni\n", out)
		require.NotContains(t, out, "SECRET")
	})

	t.Run("no allowlisted members yields empty", func(t *testing.T) {
		out := inlineNonSensitive(map[string][]byte{"httpd.yaml": []byte("k: v")})
		require.Equal(t, "", out)
	})
}

func TestProxyInputHash(t *testing.T) {
	base := &uyuniv1.Proxy{Spec: uyuniv1.ProxySpec{
		FQDN: "p.example.com", SSHPort: 8022, MaxCacheMB: 1024, Email: "a@b.c",
	}}
	tlsA := tlsMaterial{crt: "CRT", key: "KEY", rootCA: "CA"}

	h1 := proxyInputHash(base, "srv.example.com", tlsA)
	h2 := proxyInputHash(base, "srv.example.com", tlsA)
	require.Equal(t, h1, h2, "same inputs must hash identically")

	// A changed certificate must change the hash (drives regeneration).
	tlsB := tlsMaterial{crt: "CRT2", key: "KEY", rootCA: "CA"}
	require.NotEqual(t, h1, proxyInputHash(base, "srv.example.com", tlsB))

	// A changed parent server must change the hash.
	require.NotEqual(t, h1, proxyInputHash(base, "other.example.com", tlsA))

	// A changed cache size must change the hash.
	base2 := base.DeepCopy()
	base2.Spec.MaxCacheMB = 2048
	require.NotEqual(t, h1, proxyInputHash(base2, "srv.example.com", tlsA))
}
