package app

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/juanmonsalveg/homelab-go/config"
)

// healthcheckTimeout stays under the compose healthcheck's 5 s timeout.
const healthcheckTimeout = 3 * time.Second

// healthcheck is the container's healthcheck: distroless has no curl or wget, so the binary checks
// itself (README.md §8). It needs only PORT, so a broken configuration elsewhere still reports
// through /healthz.
func healthcheck(ctx context.Context, name string, getenv func(string) string, stderr io.Writer) int {
	port, err := config.Port(getenv)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "%s: healthcheck: %v\n", name, err)
		return 1
	}

	ctx, cancel := context.WithTimeout(ctx, healthcheckTimeout)
	defer cancel()

	url := "http://" + net.JoinHostPort("127.0.0.1", strconv.Itoa(port)) + "/healthz"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "%s: healthcheck: %v\n", name, err)
		return 1
	}
	client := &http.Client{
		// /healthz never redirects; a redirect means something else answered.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.Do(req)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "%s: healthcheck: %v\n", name, err)
		return 1
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		_, _ = fmt.Fprintf(stderr, "%s: healthcheck: GET %s returned %s\n", name, url, resp.Status)
		return 1
	}
	return 0
}
