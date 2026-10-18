package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
)

const (
	sessionCookie = "subbed_session"
	sessionTTL    = 30 * 24 * time.Hour
	loginPath     = "/login"
)

// sessions issues and checks signed session cookies. A token is "<expiry unix>.<hex hmac>": nothing is stored on the
// server. The signing key is derived from the credentials, so changing the username or password signs everyone out.
type sessions struct {
	creds Credentials
	key   []byte
	now   func() time.Time
}

func newSessions(creds Credentials) *sessions {
	mac := hmac.New(sha256.New, []byte(creds.Password))
	mac.Write([]byte("subbed-session:" + creds.Username))
	return &sessions{creds: creds, key: mac.Sum(nil), now: time.Now}
}

func (s *sessions) sign(expiry string) string {
	mac := hmac.New(sha256.New, s.key)
	mac.Write([]byte(expiry))
	return hex.EncodeToString(mac.Sum(nil))
}

func (s *sessions) issue() (token string, expires time.Time) {
	expires = s.now().Add(sessionTTL)
	expiry := strconv.FormatInt(expires.Unix(), 10)
	return expiry + "." + s.sign(expiry), expires
}

func (s *sessions) valid(token string) bool {
	expiry, signature, ok := strings.Cut(token, ".")
	if !ok || !hmac.Equal([]byte(signature), []byte(s.sign(expiry))) {
		return false
	}
	unix, err := strconv.ParseInt(expiry, 10, 64)
	return err == nil && s.now().Unix() < unix
}

func (s *sessions) signedIn(c echo.Context) bool {
	cookie, err := c.Cookie(sessionCookie)
	return err == nil && s.valid(cookie.Value)
}

// checkCredentials compares both fields in constant time and without short-circuiting.
func (s *sessions) checkCredentials(username, password string) bool {
	userMatch := subtle.ConstantTimeCompare([]byte(username), []byte(s.creds.Username))
	passwordMatch := subtle.ConstantTimeCompare([]byte(password), []byte(s.creds.Password))
	return userMatch&passwordMatch == 1
}

func (s *sessions) setCookie(c echo.Context, value string, expires time.Time) {
	c.SetCookie(&http.Cookie{
		Name:     sessionCookie,
		Value:    value,
		Path:     "/",
		Expires:  expires,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		// behind a TLS-terminating proxy (ngrok, a reverse proxy) the scheme comes from X-Forwarded-Proto
		Secure: c.Scheme() == "https",
	})
}

// requireSession lets signed-in requests through. Others get a redirect to the login page (for pages) or a 401.
func (s *sessions) requireSession(redirect bool) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			if s.signedIn(c) {
				return next(c)
			}
			if redirect {
				return c.Redirect(http.StatusFound, loginPath)
			}
			return echo.NewHTTPError(http.StatusUnauthorized, "Sign in required")
		}
	}
}

// loginRateLimit slows down password guessing: a burst of 10 attempts per client, then one every 5 seconds.
func loginRateLimit() echo.MiddlewareFunc {
	return middleware.RateLimiterWithConfig(middleware.RateLimiterConfig{
		Store: middleware.NewRateLimiterMemoryStoreWithConfig(middleware.RateLimiterMemoryStoreConfig{
			Rate:      0.2,
			Burst:     10,
			ExpiresIn: 10 * time.Minute,
		}),
		DenyHandler: func(c echo.Context, _ string, _ error) error {
			return echo.NewHTTPError(http.StatusTooManyRequests, "Too many attempts, try again in a moment")
		},
	})
}

type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

func (s *sessions) handleLogin(c echo.Context) error {
	var request loginRequest
	if err := c.Bind(&request); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "Invalid request")
	}
	if !s.checkCredentials(request.Username, request.Password) {
		return echo.NewHTTPError(http.StatusUnauthorized, "Invalid username or password")
	}
	token, expires := s.issue()
	s.setCookie(c, token, expires)
	return c.NoContent(http.StatusNoContent)
}

func (s *sessions) handleLogout(c echo.Context) error {
	s.setCookie(c, "", time.Unix(0, 0))
	return c.NoContent(http.StatusNoContent)
}

// loginPage serves the login page, or sends people who are already signed in to the admin page.
func (s *sessions) loginPage(page echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		if s.signedIn(c) {
			return c.Redirect(http.StatusFound, "/admin")
		}
		return page(c)
	}
}
