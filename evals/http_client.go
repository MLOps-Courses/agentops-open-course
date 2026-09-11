package evals

import (
	"net/http"
	"time"
)

func clientWithoutRedirects(client *http.Client, timeout time.Duration) *http.Client {
	if client == nil {
		return &http.Client{Timeout: timeout, CheckRedirect: refuseHTTPRedirect}
	}
	// A caller can reuse its client elsewhere, so enforce this boundary on a
	// shallow clone without changing the caller-owned redirect policy.
	hardened := *client
	hardened.CheckRedirect = refuseHTTPRedirect
	return &hardened
}

func refuseHTTPRedirect(*http.Request, []*http.Request) error {
	// Redirected 307/308 requests replay POST bodies and can cross trust boundaries.
	return http.ErrUseLastResponse
}
