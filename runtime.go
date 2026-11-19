package main

import (
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/reubenmiller/go-c8y/v2/pkg/c8y/api"
	"github.com/reubenmiller/go-c8y/v2/pkg/c8y/api/authentication"
)

// Runtime is the context which go-c8y-cli passes to extension commands in
// environment variables. See
// https://goc8ycli.netlify.app/docs/concepts/extensions/binary-commands/#runtime-context
type Runtime struct {
	// DryRun is set by "c8y --dry tenant-sync ..."
	DryRun bool
	// Insecure is set by "c8y --insecure tenant-sync ..."
	Insecure bool
	// AuthorizationHeader is the Authorization header of the active session.
	// c8y does not pass the session password to extensions, so it is used
	// when the session has no token
	AuthorizationHeader string
}

// RuntimeFromEnv reads the go-c8y-cli runtime context from the environment
func RuntimeFromEnv() Runtime {
	return runtimeFromLookup(os.LookupEnv)
}

func runtimeFromLookup(lookup func(string) (string, bool)) Runtime {
	boolValue := func(key string) bool {
		v, _ := lookup(key)
		b, _ := strconv.ParseBool(strings.TrimSpace(v))
		return b
	}
	header, _ := lookup("C8Y_HEADER_AUTHORIZATION")
	return Runtime{
		DryRun:              boolValue("C8Y_SETTINGS_DEFAULTS_DRY"),
		Insecure:            boolValue("C8Y_SETTINGS_DEFAULTS_INSECURE"),
		AuthorizationHeader: strings.TrimSpace(header),
	}
}

// NewClient creates a Cumulocity client for the active session. The C8Y_*
// session variables are used (token, then username/password); if neither a
// token nor a password is set, the Authorization header passed by c8y is used
func (r Runtime) NewClient(debug bool) (*api.Client, error) {
	host := authentication.HostFromEnvironment()
	if host == "" {
		return nil, fmt.Errorf("no active session: activate one with 'set-session' or 'c8y sessions login' (or set C8Y_HOST)")
	}

	opts := api.ClientOptions{
		BaseURL:            host,
		Auth:               authentication.FromEnvironment(),
		InsecureSkipVerify: r.Insecure,
		Debug:              debug,
	}
	if opts.Auth.Token == "" && opts.Auth.Password == "" && r.AuthorizationHeader != "" {
		opts.Middleware = append(opts.Middleware, authorizationHeaderMiddleware(r.AuthorizationHeader))
	}

	client := api.NewClient(opts)
	if debug {
		client.SetDebug(true)
	}
	return client, nil
}

// authorizationHeaderMiddleware sets a fixed Authorization header on requests
// which do not have one yet (requests with per-tenant credentials keep theirs)
func authorizationHeaderMiddleware(value string) api.Middleware {
	return func(next http.RoundTripper) http.RoundTripper {
		return api.RoundTripperFunc(func(req *http.Request) (*http.Response, error) {
			if req.Header.Get("Authorization") != "" {
				return next.RoundTrip(req)
			}
			clone := req.Clone(req.Context())
			clone.Header.Set("Authorization", value)
			return next.RoundTrip(clone)
		})
	}
}
