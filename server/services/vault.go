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
	lazymap.LazyMap[string]
	host string
	port int
	cl   *http.Client
}

func NewVault(c *cli.Context, cl *http.Client) *Vault {
	if c.String(vaultHostFlag) == "" {
		return nil
	}
	return &Vault{
		LazyMap: lazymap.New[string](&lazymap.Config{
			Expire: 60 * time.Second,
		}),
		host: c.String(vaultHostFlag),
		port: c.Int(vaultPortFlag),
		cl:   cl,
	}
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

func (s *Vault) GetWebseedURL(ctx context.Context, hash string) (string, error) {
	return s.Get(hash, func() (string, error) {
		return s.getWebseedURL(ctx, hash)
	})
}
