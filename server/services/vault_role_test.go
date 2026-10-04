package services

import (
	"flag"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/urfave/cli"
)

// TestVaultReadsSkipVaultsCopy: Vault reading a file through a seeder must get
// it from the seeder, not a redirect to the copy it is checking. thp sets
// X-Role from the verified token, vault for Vault's: that role is served here
// (from the cache in this test); any other, an empty one or none is redirected
// as before. Over the wire, so a header name in another case reaches the
// handler as the server canonicalised it.
func TestVaultReadsSkipVaultsCopy(t *testing.T) {
	const (
		hash      = "f07c04a783450dba366e562c253ce56573d7535f"
		path      = "episode.ts"
		body      = "0123456789abcdefghij"
		vaultCopy = "http://s3.invalid/vault-copy"
	)
	// Vault has the file: 200 on HEAD, a presigned redirect on GET.
	vault := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/webseed/"+hash+"/"+path {
			http.NotFound(w, r)
			return
		}
		if r.Method == http.MethodHead {
			return
		}
		http.Redirect(w, r, vaultCopy, http.StatusFound)
	}))
	t.Cleanup(vault.Close)
	addr := vault.Listener.Addr().(*net.TCPAddr)
	fs := flag.NewFlagSet("vault", flag.ContinueOnError)
	fs.String(vaultHostFlag, addr.IP.String(), "")
	fs.Int(vaultPortFlag, addr.Port, "")

	ws := cachedFileSeeder(t, hash, path, body)
	ws.cl = http.DefaultClient
	ws.v = NewVault(cli.NewContext(nil, fs, nil), ws.cl)
	srv := httptest.NewServer(ws)
	t.Cleanup(srv.Close)
	cl := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

	for _, c := range []struct {
		name   string
		header http.Header // keys as written on the wire
		served bool
	}{
		{"no X-Role", nil, false},
		{"empty X-Role", http.Header{"X-Role": {""}}, false},
		{"X-Role free", http.Header{"X-Role": {"free"}}, false},
		{"X-Role vault", http.Header{"X-Role": {"vault"}}, true},
		{"x-role vault", http.Header{"x-role": {"vault"}}, true},
		{"X-ROLE vault", http.Header{"X-ROLE": {"vault"}}, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodGet, srv.URL+"/"+hash+"/"+path+"?download=true", nil)
			if err != nil {
				t.Fatal(err)
			}
			req.Header = c.header
			resp, err := cl.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			got, err := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if err != nil {
				t.Fatal(err)
			}
			if c.served {
				if resp.StatusCode != http.StatusOK || string(got) != body {
					t.Errorf("status %d, Location %q, body %q; want 200 and the file from this seeder",
						resp.StatusCode, resp.Header.Get("Location"), got)
				}
				return
			}
			if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != vaultCopy {
				t.Errorf("status %d, Location %q; want 302 to Vault's copy", resp.StatusCode, resp.Header.Get("Location"))
			}
		})
	}

	// The role skips the redirect and nothing else: ?done, ?warmup and ?stats
	// answer "no torrent needed" for X-Role vault too (here from the cache,
	// which is asked before Vault).
	for q, want := range map[string]int{"done": http.StatusOK, "warmup": http.StatusOK, "stats": http.StatusNotFound} {
		req, err := http.NewRequest(http.MethodGet, srv.URL+"/"+hash+"/"+path+"?"+q, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("X-Role", "vault")
		resp, err := cl.Do(req)
		if err != nil {
			t.Errorf("X-Role vault, ?%s: %v", q, err)
			continue
		}
		_ = resp.Body.Close()
		if resp.StatusCode != want {
			t.Errorf("X-Role vault, ?%s: status %d, want %d", q, resp.StatusCode, want)
		}
	}
}
