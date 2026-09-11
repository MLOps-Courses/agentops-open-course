// Package httpguard defines the outbound HTTP redirect boundary shared by
// clients that carry credentials or private request bodies.
package httpguard

import "net/http"

// NoRedirects returns a shallow copy that refuses every HTTP redirect.
//
// A copy preserves the caller's transport, cookie jar, and timeout without
// mutating a client it may share elsewhere. Refusing at the client boundary is
// required because 307 and 308 replay POST bodies, while authentication
// wrappers can reapply credentials after net/http strips them.
func NoRedirects(client *http.Client) *http.Client {
	if client == nil {
		client = &http.Client{}
	}
	guarded := *client
	guarded.CheckRedirect = RefuseRedirect
	return &guarded
}

// RefuseRedirect keeps the redirect response at its source origin.
func RefuseRedirect(*http.Request, []*http.Request) error {
	return http.ErrUseLastResponse
}
