package services

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/pkg/errors"
	"github.com/urfave/cli"
	"github.com/webtor-io/lazymap"
)

const (
	vaultHostFlag = "vault-host"
	vaultPortFlag = "vault-port"
)

func RegisterVaultFlags(f []cli.Flag) []cli.Flag {
	return append(f,
		cli.StringFlag{
			Name:   vaultHostFlag,
			Usage:  "vault host",
			Value:  "",
			EnvVar: "VAULT_SERVICE_HOST",
		},
		cli.IntFlag{
			Name:   vaultPortFlag,
			Usage:  "http listening port",
			Value:  8080,
			EnvVar: "VAULT_SERVICE_PORT",
		},
	)
}

type Vault struct {
	host      string
	port      int
	cl        *http.Client
	rootCache lazymap.LazyMap[string]
	fileCache lazymap.LazyMap[bool]
}

func NewVault(c *cli.Context, cl *http.Client) *Vault {
	if c.String(vaultHostFlag) == "" {
		return nil
	}
	return &Vault{
		host: c.String(vaultHostFlag),
		port: c.Int(vaultPortFlag),
		cl:   cl,
		rootCache: lazymap.New[string](&lazymap.Config{
			Expire: 60 * time.Second,
		}),
		fileCache: lazymap.New[bool](&lazymap.Config{
			Expire: 60 * time.Second,
		}),
	}
}

// FileURL returns the per-file webseed URL.
func (s *Vault) FileURL(hash, path string) string {
	return fmt.Sprintf("http://%s:%d/webseed/%s/%s", s.host, s.port, hash, path)
}

func (s *Vault) getWebseedURL(ctx context.Context, hash string) (string, error) {
	wsURL := fmt.Sprintf("http://%s:%d/webseed/%s/", s.host, s.port, hash)
	req, err := http.NewRequestWithContext(ctx, "GET", wsURL, nil)
	if err != nil {
		return "", err
	}
	resp, err := s.cl.Do(req)
	if err != nil {
		return "", err
	}
	defer func(Body io.ReadCloser) {
		_ = Body.Close()
	}(resp.Body)
	if resp.StatusCode == http.StatusOK {
		return wsURL, nil
	} else if resp.StatusCode == http.StatusNotFound {
		return "", nil
	} else {
		return "", errors.Errorf("unexpected status code: %d", resp.StatusCode)
	}
}

// GetWebseedURL returns the root webseed URL when the whole torrent is
// stored in vault (200 on /webseed/{hash}/), otherwise empty string.
func (s *Vault) GetWebseedURL(ctx context.Context, hash string) (string, error) {
	return s.rootCache.Get(hash, func() (string, error) {
		return s.getWebseedURL(ctx, hash)
	})
}

func (s *Vault) hasFile(ctx context.Context, hash, path string) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, s.FileURL(hash, path), nil)
	if err != nil {
		return false, err
	}
	resp, err := s.cl.Do(req)
	if err != nil {
		return false, err
	}
	defer func(Body io.ReadCloser) {
		_ = Body.Close()
	}(resp.Body)
	switch resp.StatusCode {
	case http.StatusOK:
		return true, nil
	case http.StatusNotFound:
		return false, nil
	default:
		return false, errors.Errorf("unexpected status code: %d", resp.StatusCode)
	}
}

// HasFile returns true if vault has the specific file stored, even when
// the whole torrent isn't yet. Result is cached for 60s per (hash,path).
func (s *Vault) HasFile(ctx context.Context, hash, path string) (bool, error) {
	return s.fileCache.Get(hash+"/"+path, func() (bool, error) {
		return s.hasFile(ctx, hash, path)
	})
}
