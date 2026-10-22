package server

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/asilluron/lcars-api/internal/adminui"
	"github.com/asilluron/lcars-api/internal/devices"
	"github.com/asilluron/lcars-api/internal/supabase"
)

// AdminAuth signs admins in and checks their sessions; *supabase.Client
// implements it.
type AdminAuth interface {
	SignIn(ctx context.Context, email, password string) (supabase.Session, error)
	User(ctx context.Context, accessToken string) (supabase.User, error)
}

var errAdminDisabled = &HTTPError{
	Code: http.StatusServiceUnavailable,
	Msg:  "the admin API is disabled: set supabase.url and supabase.secret_key in the config file",
}

// requireDevice makes next answer 401 unless the request has a valid device
// token as its Basic auth credentials. With no store, devices don't
// authenticate and next is returned as-is.
func requireDevice(store *devices.Store, next http.Handler) http.Handler {
	if store == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if user, password, ok := r.BasicAuth(); ok && store.Check(user, password) {
			next.ServeHTTP(w, r)
			return
		}
		w.Header().Set("WWW-Authenticate", `Basic realm="lcars", charset="UTF-8"`)
		writeError(w, http.StatusUnauthorized,
			"a device token is required: send its username and password as HTTP Basic auth")
	})
}

type signInRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

type signInResponse struct {
	AccessToken string    `json:"access_token"`
	ExpiresAt   time.Time `json:"expires_at"`
	Email       string    `json:"email"`
}

// adminSignIn exchanges an admin's email and password for an access token,
// which the admin page sends as a Bearer token to the other admin endpoints.
func adminSignIn(logger *slog.Logger, deps Deps) handlerFunc {
	return func(w http.ResponseWriter, r *http.Request) error {
		if deps.Devices == nil {
			return errAdminDisabled
		}
		var req signInRequest
		if err := decodeJSON(w, r, &req); err != nil {
			return err
		}
		if req.Email == "" || req.Password == "" {
			return &HTTPError{Code: http.StatusBadRequest, Msg: "email and password are required"}
		}
		s, err := deps.Admins.SignIn(r.Context(), req.Email, req.Password)
		switch {
		case errors.Is(err, supabase.ErrInvalidCredentials):
			logger.WarnContext(r.Context(), "admin sign-in failed")
			return &HTTPError{Code: http.StatusUnauthorized, Msg: "invalid email or password"}
		case errors.Is(err, supabase.ErrRateLimited):
			return &HTTPError{Code: http.StatusTooManyRequests, Msg: "too many sign-in attempts; try again later"}
		case err != nil:
			return authUnavailable(r.Context(), logger, err)
		}
		logger.InfoContext(r.Context(), "admin signed in", "user_id", s.User.ID)
		return writeJSON(w, http.StatusOK, signInResponse{
			AccessToken: s.AccessToken,
			ExpiresAt:   time.Unix(s.ExpiresAt, 0).UTC(),
			Email:       s.User.Email,
		})
	}
}

// adminHandlerFunc is a handlerFunc for a signed-in admin.
type adminHandlerFunc func(w http.ResponseWriter, r *http.Request, admin supabase.User) error

// requireAdmin answers 401 unless the request has a valid admin access token
// as its Bearer token, and otherwise calls h with the admin.
func requireAdmin(logger *slog.Logger, deps Deps, h adminHandlerFunc) handlerFunc {
	return func(w http.ResponseWriter, r *http.Request) error {
		if deps.Devices == nil {
			return errAdminDisabled
		}
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || token == "" {
			w.Header().Set("WWW-Authenticate", `Bearer realm="lcars-admin"`)
			return &HTTPError{Code: http.StatusUnauthorized, Msg: "sign in first"}
		}
		admin, err := deps.Admins.User(r.Context(), token)
		if errors.Is(err, supabase.ErrInvalidCredentials) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="lcars-admin", error="invalid_token"`)
			return &HTTPError{Code: http.StatusUnauthorized, Msg: "your session has expired; sign in again"}
		}
		if err != nil {
			return authUnavailable(r.Context(), logger, err)
		}
		return h(w, r, admin)
	}
}

// authUnavailable logs a failure to reach Supabase and returns a generic 503.
func authUnavailable(ctx context.Context, logger *slog.Logger, err error) error {
	logger.ErrorContext(ctx, "supabase request failed", "err", err)
	return &HTTPError{Code: http.StatusServiceUnavailable, Msg: "the admin database is unavailable; try again later"}
}

type deviceToken struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
	// Secret is set only in the response that creates the token.
	Secret string `json:"secret,omitempty"`
}

func newDeviceToken(t devices.Token) deviceToken {
	return deviceToken{ID: t.ID, Name: t.Name, CreatedAt: t.CreatedAt.UTC()}
}

func listTokens(logger *slog.Logger, store *devices.Store) adminHandlerFunc {
	return func(w http.ResponseWriter, r *http.Request, _ supabase.User) error {
		tokens, err := store.List(r.Context())
		if err != nil {
			return authUnavailable(r.Context(), logger, err)
		}
		out := make([]deviceToken, len(tokens))
		for i, t := range tokens {
			out[i] = newDeviceToken(t)
		}
		return writeJSON(w, http.StatusOK, map[string][]deviceToken{"tokens": out})
	}
}

func createToken(logger *slog.Logger, store *devices.Store) adminHandlerFunc {
	return func(w http.ResponseWriter, r *http.Request, admin supabase.User) error {
		var req struct {
			Name string `json:"name"`
		}
		if err := decodeJSON(w, r, &req); err != nil {
			return err
		}
		t, secret, err := store.Create(r.Context(), req.Name, admin.ID)
		if errors.Is(err, devices.ErrInvalidName) {
			return &HTTPError{Code: http.StatusBadRequest, Msg: `invalid "name": ` + err.Error()}
		}
		if err != nil {
			return authUnavailable(r.Context(), logger, err)
		}
		out := newDeviceToken(t)
		out.Secret = secret
		w.Header().Set("Cache-Control", "no-store")
		return writeJSON(w, http.StatusCreated, out)
	}
}

func revokeToken(logger *slog.Logger, store *devices.Store) adminHandlerFunc {
	return func(w http.ResponseWriter, r *http.Request, _ supabase.User) error {
		err := store.Revoke(r.Context(), r.PathValue("id"))
		if errors.Is(err, devices.ErrNotFound) {
			return &HTTPError{Code: http.StatusNotFound, Msg: "no such device token"}
		}
		if err != nil {
			return authUnavailable(r.Context(), logger, err)
		}
		w.WriteHeader(http.StatusNoContent)
		return nil
	}
}

// maxBodyBytes is the largest JSON request body accepted.
const maxBodyBytes = 64 << 10

// decodeJSON decodes the JSON request body into v, rejecting anything else
// with a 400 (or 415 for the wrong content type).
func decodeJSON(w http.ResponseWriter, r *http.Request, v any) error {
	if ct := r.Header.Get("Content-Type"); ct != "application/json" && !strings.HasPrefix(ct, "application/json;") {
		return &HTTPError{Code: http.StatusUnsupportedMediaType, Msg: "send a JSON body with Content-Type: application/json"}
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return &HTTPError{Code: http.StatusBadRequest, Msg: "invalid JSON body"}
	}
	return nil
}

// adminPage serves the admin page's files. index.html and the hashed assets
// it loads are all it has; there are no directory listings.
func adminPage(files fs.FS, built bool) handlerFunc {
	return func(w http.ResponseWriter, r *http.Request) error {
		h := w.Header()
		h.Set("Content-Security-Policy",
			"default-src 'self'; img-src 'self' data:; object-src 'none'; base-uri 'none'; frame-ancestors 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		if !built {
			h.Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, err := w.Write([]byte("The admin page is not built into this binary. Run `make admin-ui`, then rebuild.\n"))
			return err
		}

		name := strings.TrimPrefix(r.URL.Path, "/admin/")
		if name == "" {
			name = "index.html"
		}
		if strings.HasSuffix(name, "/") || name != path.Clean(name) {
			return &HTTPError{Code: http.StatusNotFound, Msg: "Not Found"}
		}
		if _, err := fs.Stat(files, name); err != nil {
			return &HTTPError{Code: http.StatusNotFound, Msg: "Not Found"}
		}
		if strings.HasPrefix(name, "assets/") {
			// Vite puts a content hash in every asset's name.
			h.Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			h.Set("Cache-Control", "no-cache")
		}
		// name is clean and exists in files (the embedded build), which can't
		// reach anything else.
		http.ServeFileFS(w, r, files, name) //nolint:gosec // see above

		return nil
	}
}

// adminFiles is the admin page built into the binary.
var adminFiles, adminBuilt = adminui.FS(), adminui.Built()
