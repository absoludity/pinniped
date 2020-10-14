// Copyright 2020 the Pinniped contributors. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

// Package login implements a CLI OIDC login flow.
package login

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/coreos/go-oidc"
	"github.com/pkg/browser"
	"golang.org/x/oauth2"

	"go.pinniped.dev/internal/httputil/httperr"
	"go.pinniped.dev/internal/httputil/securityheader"
	"go.pinniped.dev/internal/oidcclient/nonce"
	"go.pinniped.dev/internal/oidcclient/pkce"
	"go.pinniped.dev/internal/oidcclient/state"
)

type handlerState struct {
	// Basic parameters.
	ctx      context.Context
	issuer   string
	clientID string
	scopes   []string

	// Parameters of the localhost listener.
	listenAddr   string
	callbackPath string

	// Generated parameters of a login flow.
	idTokenVerifier *oidc.IDTokenVerifier
	oauth2Config    *oauth2.Config
	state           state.State
	nonce           nonce.Nonce
	pkce            pkce.Code

	// External calls for things.
	generateState func() (state.State, error)
	generatePKCE  func() (pkce.Code, error)
	generateNonce func() (nonce.Nonce, error)
	openURL       func(string) error

	callbacks chan callbackResult
}

type callbackResult struct {
	token *Token
	err   error
}

type Token struct {
	*oauth2.Token
	IDToken       string    `json:"id_token"`
	IDTokenExpiry time.Time `json:"id_token_expiry"`
}

// Option is an optional configuration for Run().
type Option func(*handlerState) error

// WithContext specifies a specific context.Context under which to perform the login. If this option is not specified,
// login happens under context.Background().
func WithContext(ctx context.Context) Option {
	return func(h *handlerState) error {
		h.ctx = ctx
		return nil
	}
}

// WithListenPort specifies a TCP listen port on localhost, which will be used for the redirect_uri and to handle the
// authorization code callback. By default, a random high port will be chosen which requires the authorization server
// to support wildcard port numbers as described by https://tools.ietf.org/html/rfc8252:
//    The authorization server MUST allow any port to be specified at the
//    time of the request for loopback IP redirect URIs, to accommodate
//    clients that obtain an available ephemeral port from the operating
//    system at the time of the request.
func WithListenPort(port uint16) Option {
	return func(h *handlerState) error {
		h.listenAddr = fmt.Sprintf("localhost:%d", port)
		return nil
	}
}

// WithScopes sets the OAuth2 scopes to request during login. If not specified, it defaults to
// "offline_access openid email profile".
func WithScopes(scopes []string) Option {
	return func(h *handlerState) error {
		h.scopes = scopes
		return nil
	}
}

// WithBrowserOpen overrides the default "open browser" functionality with a custom callback. If not specified,
// an implementation using https://github.com/pkg/browser will be used by default.
func WithBrowserOpen(openURL func(url string) error) Option {
	return func(h *handlerState) error {
		h.openURL = openURL
		return nil
	}
}

// Run an OAuth2/OIDC authorization code login using a localhost listener.
func Run(issuer string, clientID string, opts ...Option) (*Token, error) {
	h := handlerState{
		issuer:       issuer,
		clientID:     clientID,
		listenAddr:   "localhost:0",
		scopes:       []string{"offline_access", "openid", "email", "profile"},
		callbackPath: "/callback",
		ctx:          context.Background(),
		callbacks:    make(chan callbackResult),

		// Default implementations of external dependencies (to be mocked in tests).
		generateState: state.Generate,
		generateNonce: nonce.Generate,
		generatePKCE:  pkce.Generate,
		openURL:       browser.OpenURL,
	}
	for _, opt := range opts {
		if err := opt(&h); err != nil {
			return nil, err
		}
	}

	// Always set a long, but non-infinite timeout for this operation.
	ctx, cancel := context.WithTimeout(h.ctx, 10*time.Minute)
	defer cancel()
	h.ctx = ctx

	// Initialize login parameters.
	var err error
	h.state, err = h.generateState()
	if err != nil {
		return nil, err
	}
	h.nonce, err = h.generateNonce()
	if err != nil {
		return nil, err
	}
	h.pkce, err = h.generatePKCE()
	if err != nil {
		return nil, err
	}

	// Perform OIDC discovery.
	provider, err := oidc.NewProvider(h.ctx, h.issuer)
	if err != nil {
		return nil, fmt.Errorf("could not perform OIDC discovery for %q: %w", h.issuer, err)
	}
	h.idTokenVerifier = provider.Verifier(&oidc.Config{ClientID: h.clientID})

	// Open a TCP listener.
	listener, err := net.Listen("tcp", h.listenAddr)
	if err != nil {
		return nil, fmt.Errorf("could not open callback listener: %w", err)
	}

	// Build an OAuth2 configuration based on the OIDC discovery data and our callback endpoint.
	h.oauth2Config = &oauth2.Config{
		ClientID: h.clientID,
		Endpoint: provider.Endpoint(),
		RedirectURL: (&url.URL{
			Scheme: "http",
			Host:   listener.Addr().String(),
			Path:   h.callbackPath,
		}).String(),
		Scopes: h.scopes,
	}

	// Start a callback server in a background goroutine.
	mux := http.NewServeMux()
	mux.Handle(h.callbackPath, httperr.HandlerFunc(h.handleAuthCodeCallback))
	srv := http.Server{
		Handler:     securityheader.Wrap(mux),
		BaseContext: func(_ net.Listener) context.Context { return h.ctx },
	}
	go func() { _ = srv.Serve(listener) }()
	defer func() {
		// Gracefully shut down the server, allowing up to 5 seconds for
		// clients to receive any in-flight responses.
		shutdownCtx, cancel := context.WithTimeout(h.ctx, 1*time.Second)
		_ = srv.Shutdown(shutdownCtx)
		cancel()
	}()

	// Open the authorize URL in the users browser.
	authorizeURL := h.oauth2Config.AuthCodeURL(
		h.state.String(),
		oauth2.AccessTypeOffline,
		h.nonce.Param(),
		h.pkce.Challenge(),
		h.pkce.Method(),
	)
	if err := h.openURL(authorizeURL); err != nil {
		return nil, fmt.Errorf("could not open browser: %w", err)
	}

	// Wait for either the callback or a timeout.
	select {
	case <-h.ctx.Done():
		return nil, fmt.Errorf("timed out waiting for token callback: %w", h.ctx.Err())
	case callback := <-h.callbacks:
		if callback.err != nil {
			return nil, fmt.Errorf("error handling callback: %w", callback.err)
		}
		return callback.token, nil
	}
}

func (h *handlerState) handleAuthCodeCallback(w http.ResponseWriter, r *http.Request) (err error) {
	// If we return an error, also report it back over the channel to the main CLI thread.
	defer func() {
		if err != nil {
			h.callbacks <- callbackResult{err: err}
		}
	}()

	// Return HTTP 405 for anything that's not a GET.
	if r.Method != http.MethodGet {
		return httperr.Newf(http.StatusMethodNotAllowed, "wanted GET")
	}

	// Validate OAuth2 state and fail if it's incorrect (to block CSRF).
	params := r.URL.Query()
	if err := h.state.Validate(params.Get("state")); err != nil {
		return httperr.New(http.StatusForbidden, "missing or invalid state parameter")
	}

	// Check for error response parameters.
	if errorParam := params.Get("error"); errorParam != "" {
		return httperr.Newf(http.StatusBadRequest, "login failed with code %q", errorParam)
	}

	// Exchange the authorization code for access, ID, and refresh tokens.
	oauth2Tok, err := h.oauth2Config.Exchange(r.Context(), params.Get("code"), h.pkce.Verifier())
	if err != nil {
		return httperr.Wrap(http.StatusBadRequest, "could not complete code exchange", err)
	}

	// Perform required validations on the returned ID token.
	idTok, hasIDTok := oauth2Tok.Extra("id_token").(string)
	if !hasIDTok {
		return httperr.New(http.StatusBadRequest, "received response missing ID token")
	}
	validated, err := h.idTokenVerifier.Verify(r.Context(), idTok)
	if err != nil {
		return httperr.Wrap(http.StatusBadRequest, "received invalid ID token", err)
	}
	if validated.AccessTokenHash != "" {
		if err := validated.VerifyAccessToken(oauth2Tok.AccessToken); err != nil {
			return httperr.Wrap(http.StatusBadRequest, "received invalid ID token", err)
		}
	}
	if err := h.nonce.Validate(validated); err != nil {
		return httperr.Wrap(http.StatusBadRequest, "received ID token with invalid nonce", err)
	}

	h.callbacks <- callbackResult{token: &Token{
		Token:         oauth2Tok,
		IDToken:       idTok,
		IDTokenExpiry: validated.Expiry,
	}}
	_, _ = w.Write([]byte("you have been logged in and may now close this tab"))
	return nil
}
