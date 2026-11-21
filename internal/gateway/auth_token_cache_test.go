package gateway

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/eimsound/lazy-observe/internal/meta"
	"github.com/gofiber/fiber/v3"
)

// tokenCacheAliasingRounds is sized so that the pre-fix bug (a borrowed
// Authorization header used as the cache key, matched with roughly 1/128
// probability per lookup) fails with overwhelming probability: about 16
// expected hits, so P(no hit) is on the order of 1e-7.
const tokenCacheAliasingRounds = 2000

type staticProjectTokens map[string]meta.ProjectInfo

func (s staticProjectTokens) AuthenticateProjectAPIToken(_ context.Context, token string) (meta.ProjectInfo, error) {
	p, ok := s[token]
	if !ok {
		return meta.ProjectInfo{}, meta.ErrInvalidProjectAPIToken
	}
	return p, nil
}

func (staticProjectTokens) Revision() uint64 { return 1 }

// tokenCacheProbeApp wires a fresh Authenticator into the same Fiber
// configuration the gateway serves with, so both defenses are exercised: the
// Authenticator owning its cache keys and the app not lending request memory.
func tokenCacheProbeApp(tokens staticProjectTokens) *fiber.App {
	auth := NewAuthenticator(tokens)
	app := newFiberApp(defaultReadTimeout, defaultIdleTimeout, nil, nil)
	app.Post("/ingest", auth.Middleware, func(c fiber.Ctx) error {
		return c.SendString(currentProject(c).Slug)
	})
	return app
}

func tokenCacheProbe(t *testing.T, app *fiber.App, token string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/ingest", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(body)
}

// A cached project token must never resolve a later request carrying a
// different project's token: Fiber reuses the request buffers, so a borrowed
// cache key would silently turn into the next request's token.
func TestAuthenticatorTokenCacheDoesNotResolveAnotherProject(t *testing.T) {
	misresolved := 0
	for round := range tokenCacheAliasingRounds {
		first := fmt.Sprintf("lop_regression_%024d", 2*round)
		second := fmt.Sprintf("lop_regression_%024d", 2*round+1)
		app := tokenCacheProbeApp(staticProjectTokens{
			first:  {ID: 1, Slug: "default"},
			second: {ID: 2, Slug: "p2"},
		})
		if status, slug := tokenCacheProbe(t, app, first); status != http.StatusOK || slug != "default" {
			t.Fatalf("round %d: first token status=%d project=%q", round, status, slug)
		}
		if status, slug := tokenCacheProbe(t, app, second); status != http.StatusOK || slug != "p2" {
			misresolved++
		}
	}
	if misresolved != 0 {
		t.Fatalf("project token resolved to another project in %d/%d rounds", misresolved, tokenCacheAliasingRounds)
	}
}

// An unknown token must be rejected even right after a valid token was cached
// from the same reused request buffers.
func TestAuthenticatorTokenCacheRejectsUnknownTokenAfterCaching(t *testing.T) {
	accepted := 0
	for round := range tokenCacheAliasingRounds {
		valid := fmt.Sprintf("lop_regression_%024d", round)
		unknown := fmt.Sprintf("lop_unknown_%027d", round)
		app := tokenCacheProbeApp(staticProjectTokens{valid: {ID: 1, Slug: "default"}})
		if status, slug := tokenCacheProbe(t, app, valid); status != http.StatusOK || slug != "default" {
			t.Fatalf("round %d: valid token status=%d project=%q", round, status, slug)
		}
		if status, _ := tokenCacheProbe(t, app, unknown); status != http.StatusUnauthorized {
			accepted++
		}
	}
	if accepted != 0 {
		t.Fatalf("unknown token was authenticated in %d/%d rounds", accepted, tokenCacheAliasingRounds)
	}
}
