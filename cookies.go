package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/doug-martin/goqu/v9"
	"github.com/labstack/echo/v4"
)

const (
	maxCookiesBytes = 1 << 20
	// maxCookies keeps one insert under SQLite's limit on bound variables
	maxCookies     = 2000
	netscapeHeader = "# Netscape HTTP Cookie File"
	httpOnlyPrefix = "#HttpOnly_"
)

// Cookie is one browser cookie, as yt-dlp needs it to act as a signed-in user.
type Cookie struct {
	Domain   string `db:"domain"`
	Name     string `db:"key"`
	Value    string `db:"value"`
	Path     string `db:"path"`
	Secure   bool   `db:"secure"`
	HTTPOnly bool   `db:"http_only"`
	// Expires is a Unix time; 0 marks a session cookie
	Expires int64 `db:"expires"`
}

// parseNetscapeCookies reads the text of a Netscape cookies.txt: one cookie per line with the tab-separated fields
// domain, subdomain flag, path, secure flag, expiry, name and value. Blank lines and comments are skipped; a domain
// with the "#HttpOnly_" prefix marks an HTTP-only cookie.
func parseNetscapeCookies(text string) ([]Cookie, error) {
	var cookies []Cookie
	for number, line := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		httpOnly := false
		if rest, found := strings.CutPrefix(line, httpOnlyPrefix); found {
			line, httpOnly = rest, true
		} else if strings.HasPrefix(line, "#") {
			continue
		}

		fields := strings.Split(line, "\t")
		if len(fields) == 6 {
			// an empty value loses its trailing tab in some exports
			fields = append(fields, "")
		}
		if len(fields) != 7 {
			return nil, fmt.Errorf("line %d: expected 7 tab-separated fields, found %d", number+1, len(fields))
		}
		secure, err := parseNetscapeBool(fields[3])
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", number+1, err)
		}
		expires, err := strconv.ParseInt(fields[4], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("line %d: expiry %q is not a number", number+1, fields[4])
		}
		if fields[0] == "" || fields[5] == "" {
			return nil, fmt.Errorf("line %d: domain and name are required", number+1)
		}
		cookies = append(cookies, Cookie{
			Domain: fields[0], Path: fields[2], Secure: secure, HTTPOnly: httpOnly,
			Expires: expires, Name: fields[5], Value: fields[6],
		})
	}
	if len(cookies) == 0 {
		return nil, fmt.Errorf("no cookies found")
	}
	return cookies, nil
}

func parseNetscapeBool(value string) (bool, error) {
	switch strings.ToUpper(value) {
	case "TRUE":
		return true, nil
	case "FALSE":
		return false, nil
	}
	return false, fmt.Errorf("%q should be TRUE or FALSE", value)
}

func netscapeBool(value bool) string {
	if value {
		return "TRUE"
	}
	return "FALSE"
}

// renderNetscapeCookies writes cookies in the format yt-dlp reads.
func renderNetscapeCookies(cookies []Cookie) string {
	var text strings.Builder
	text.WriteString(netscapeHeader + "\n")
	for _, cookie := range cookies {
		domain := cookie.Domain
		if cookie.HTTPOnly {
			domain = httpOnlyPrefix + domain
		}
		// a leading dot means the cookie also applies to subdomains
		fmt.Fprintf(&text, "%s\t%s\t%s\t%s\t%d\t%s\t%s\n",
			domain, netscapeBool(strings.HasPrefix(cookie.Domain, ".")), cookie.Path, netscapeBool(cookie.Secure),
			cookie.Expires, cookie.Name, cookie.Value)
	}
	return text.String()
}

// UpsertCookies stores the cookies in one statement, replacing the value of any with the same domain and name.
// Others stay.
func (r *Repository) UpsertCookies(ctx context.Context, cookies []Cookie) error {
	if len(cookies) == 0 {
		return nil
	}
	rows := make([]any, len(cookies))
	for position, cookie := range cookies {
		rows[position] = goqu.Record{
			"domain": cookie.Domain, "key": cookie.Name, "value": cookie.Value, "path": cookie.Path,
			"secure": cookie.Secure, "http_only": cookie.HTTPOnly, "expires": cookie.Expires,
		}
	}
	_, err := r.db.Insert("cookies").
		Rows(rows...).
		OnConflict(goqu.DoUpdate(`domain, "key"`, goqu.Record{
			"value":     goqu.L("excluded.value"),
			"path":      goqu.L("excluded.path"),
			"secure":    goqu.L("excluded.secure"),
			"http_only": goqu.L("excluded.http_only"),
			"expires":   goqu.L("excluded.expires"),
		})).
		Executor().
		ExecContext(ctx)
	if err != nil {
		return fmt.Errorf("failed to save cookies: %w", err)
	}
	return nil
}

// ListCookies returns every stored cookie.
func (r *Repository) ListCookies(ctx context.Context) ([]Cookie, error) {
	var cookies []Cookie
	err := r.db.From("cookies").
		Select("domain", "key", "value", "path", "secure", "http_only", "expires").
		Order(goqu.C("domain").Asc(), goqu.C("key").Asc()).
		ScanStructsContext(ctx, &cookies)
	if err != nil {
		return nil, fmt.Errorf("failed to query cookies: %w", err)
	}
	return cookies, nil
}

// DeleteCookies removes every stored cookie.
func (r *Repository) DeleteCookies(ctx context.Context) error {
	if _, err := r.db.Delete("cookies").Executor().ExecContext(ctx); err != nil {
		return fmt.Errorf("failed to delete cookies: %w", err)
	}
	return nil
}

// writeCookieFile renders the stored cookies to a private temporary file for yt-dlp, which writes back to the file
// it is given. It returns "" when there are no cookies. Call cleanup when done.
func writeCookieFile(ctx context.Context, repo *Repository) (path string, cleanup func(), err error) {
	cookies, err := repo.ListCookies(ctx)
	if err != nil || len(cookies) == 0 {
		return "", func() {}, err
	}
	file, err := os.CreateTemp("", "subbed-cookies-*.txt") // created with mode 0600
	if err != nil {
		return "", func() {}, err
	}
	cleanup = func() { os.Remove(file.Name()) }
	if _, err := file.WriteString(renderNetscapeCookies(cookies)); err != nil {
		file.Close()
		cleanup()
		return "", func() {}, err
	}
	if err := file.Close(); err != nil {
		cleanup()
		return "", func() {}, err
	}
	return file.Name(), cleanup, nil
}

type cookieSummary struct {
	Count int `json:"count"`
	// Domains lists where cookies exist, without leading dots. Values are never sent back.
	Domains []string `json:"domains"`
}

func summarizeCookies(cookies []Cookie) cookieSummary {
	seen := map[string]bool{}
	domains := []string{}
	for _, cookie := range cookies {
		if domain := strings.TrimPrefix(cookie.Domain, "."); !seen[domain] {
			seen[domain] = true
			domains = append(domains, domain)
		}
	}
	sort.Strings(domains)
	return cookieSummary{Count: len(cookies), Domains: domains}
}

func getCookies(repo *Repository) echo.HandlerFunc {
	return func(c echo.Context) error {
		cookies, err := repo.ListCookies(c.Request().Context())
		if err != nil {
			return err
		}
		return c.JSON(http.StatusOK, summarizeCookies(cookies))
	}
}

// saveCookies takes the text of a Netscape cookies.txt and upserts its cookies.
func saveCookies(repo *Repository) echo.HandlerFunc {
	return func(c echo.Context) error {
		var req struct {
			Cookies string `json:"cookies"`
		}
		if err := c.Bind(&req); err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, "Invalid request")
		}
		if len(req.Cookies) > maxCookiesBytes {
			return echo.NewHTTPError(http.StatusRequestEntityTooLarge, "Cookies are too large")
		}
		cookies, err := parseNetscapeCookies(req.Cookies)
		if err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, "Invalid cookies: "+err.Error())
		}
		if len(cookies) > maxCookies {
			return echo.NewHTTPError(http.StatusBadRequest, "Too many cookies")
		}
		ctx := c.Request().Context()
		if err := repo.UpsertCookies(ctx, cookies); err != nil {
			return err
		}
		stored, err := repo.ListCookies(ctx)
		if err != nil {
			return err
		}
		slog.Info("Saved cookies", "imported", len(cookies), "stored", len(stored))
		return c.JSON(http.StatusOK, summarizeCookies(stored))
	}
}

func deleteCookies(repo *Repository) echo.HandlerFunc {
	return func(c echo.Context) error {
		if err := repo.DeleteCookies(c.Request().Context()); err != nil {
			return err
		}
		slog.Info("Deleted cookies")
		return c.JSON(http.StatusOK, cookieSummary{Domains: []string{}})
	}
}
