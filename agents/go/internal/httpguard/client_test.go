package httpguard

import (
	"errors"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// The consumers already prove the boundary end to end (kagentinterop, model,
// piiwebhook). What only a package-level test can prove is the contract those
// call sites rely on: the policy holds for every redirect status net/http knows
// how to follow, the caller's own client is never mutated, and a nil client is
// still guarded rather than a panic waiting for the first outbound call.
//
// Real servers on both ends throughout: a followed redirect has to be
// observable as a request arriving somewhere, not inferred from a stub.

func TestNoRedirectsReturnsEveryRedirectInsteadOfFollowingIt(t *testing.T) {
	t.Parallel()

	var reached atomic.Int64
	destination := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		reached.Add(1)
		writer.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(destination.Close)
	// One source for the whole table, with the status named by the request path.
	// A listener per case would be clearer to read and four times the sockets to
	// bind, which is real time on a loaded machine for no extra coverage.
	source := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		status, err := strconv.Atoi(strings.TrimPrefix(request.URL.Path, "/"))
		if err != nil {
			http.Error(writer, "the request path names the redirect status", http.StatusBadRequest)
			return
		}
		writer.Header().Set("Location", destination.URL+"/replayed")
		writer.WriteHeader(status)
	}))
	t.Cleanup(source.Close)

	// 303 rewrites the method, while 307 and 308 replay the original method and
	// body verbatim — the case that would resend a credentialed POST to an
	// attacker-chosen origin, and the reason the refusal lives at the client.
	statuses := []int{
		http.StatusMovedPermanently,
		http.StatusFound,
		http.StatusSeeOther,
		http.StatusTemporaryRedirect,
		http.StatusPermanentRedirect,
	}
	client := NoRedirects(&http.Client{Timeout: 5 * time.Second})
	for _, status := range statuses {
		// Sequential on purpose: the cases share the hit counter, so each of them
		// asserts that nothing has reached the target across the whole table.
		t.Run(http.StatusText(status), func(t *testing.T) {
			response, err := client.Post(
				source.URL+"/"+strconv.Itoa(status), "application/json", strings.NewReader(`{"secret":"x"}`),
			)
			if err != nil {
				t.Fatalf("Post() error = %v, want the redirect response itself", err)
			}
			defer func() { _ = response.Body.Close() }()

			if response.StatusCode != status {
				t.Errorf("Post() status = %d, want the unfollowed %d", response.StatusCode, status)
			}
			if got := response.Header.Get("Location"); got != destination.URL+"/replayed" {
				t.Errorf("Location header = %q, want the redirect target preserved for the caller", got)
			}
			if got := reached.Load(); got != 0 {
				t.Errorf("redirect target received %d request(s), want zero credential or body replays", got)
			}
		})
	}
}

func TestNoRedirectsCopiesTheClientWithoutMutatingIt(t *testing.T) {
	t.Parallel()

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookiejar.New() error = %v, want nil", err)
	}
	transport := &http.Transport{}
	original := &http.Client{Transport: transport, Jar: jar, Timeout: 3 * time.Second}

	guarded := NoRedirects(original)

	if original.CheckRedirect != nil {
		t.Error("NoRedirects() installed its policy on the caller's client, which it may share elsewhere")
	}
	if guarded == original {
		t.Fatal("NoRedirects() returned the caller's client rather than a copy")
	}
	// The copy has to keep the caller's transport, jar, and timeout: a client
	// that silently lost its proxy or its deadline would be a worse bug than the
	// redirect it prevents.
	if guarded.Transport != transport {
		t.Errorf("guarded transport = %v, want the caller's transport preserved", guarded.Transport)
	}
	if guarded.Jar != jar {
		t.Errorf("guarded jar = %v, want the caller's cookie jar preserved", guarded.Jar)
	}
	if guarded.Timeout != original.Timeout {
		t.Errorf("guarded timeout = %v, want %v", guarded.Timeout, original.Timeout)
	}
	if guarded.CheckRedirect == nil {
		t.Fatal("guarded client has no redirect policy")
	}
	if err := guarded.CheckRedirect(nil, nil); !errors.Is(err, http.ErrUseLastResponse) {
		t.Errorf("guarded CheckRedirect() error = %v, want %v", err, http.ErrUseLastResponse)
	}
}

func TestNoRedirectsGuardsANilClient(t *testing.T) {
	t.Parallel()

	var reached atomic.Int64
	destination := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		reached.Add(1)
		writer.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(destination.Close)
	source := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Location", destination.URL+"/replayed")
		writer.WriteHeader(http.StatusFound)
	}))
	t.Cleanup(source.Close)

	// A nil client is what a caller with no timeout to impose passes. It must
	// come back guarded rather than nil, because the alternative is a panic on
	// the first outbound request instead of a refused redirect.
	client := NoRedirects(nil)
	if client == nil {
		t.Fatal("NoRedirects(nil) returned nil")
	}

	response, err := client.Get(source.URL)
	if err != nil {
		t.Fatalf("Get() error = %v, want the redirect response itself", err)
	}
	defer func() { _ = response.Body.Close() }()

	if response.StatusCode != http.StatusFound {
		t.Errorf("Get() status = %d, want the unfollowed %d", response.StatusCode, http.StatusFound)
	}
	if got := reached.Load(); got != 0 {
		t.Errorf("redirect target received %d request(s), want zero", got)
	}
}

func TestRefuseRedirectStopsAtTheFirstHopAndAtEveryLaterOne(t *testing.T) {
	t.Parallel()

	request := httptest.NewRequest(http.MethodGet, "https://redirected.invalid/next", nil)
	via := []*http.Request{
		httptest.NewRequest(http.MethodGet, "https://origin.invalid/first", nil),
		httptest.NewRequest(http.MethodGet, "https://origin.invalid/second", nil),
	}
	for _, chain := range [][]*http.Request{nil, via[:1], via} {
		// ErrUseLastResponse rather than a bespoke error on purpose: net/http
		// treats it as "hand the redirect response back", so the caller sees the
		// 3xx and decides, instead of an opaque transport failure.
		if err := RefuseRedirect(request, chain); !errors.Is(err, http.ErrUseLastResponse) {
			t.Errorf("RefuseRedirect() after %d hop(s) error = %v, want %v", len(chain), err, http.ErrUseLastResponse)
		}
	}
}
