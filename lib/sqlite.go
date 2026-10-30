package lib

import (
	"database/sql"
	"database/sql/driver"
	"fmt"
	"net/url"
	"strings"

	"modernc.org/sqlite"
)

// Keep the existing driver name in configurations and sqlx queries. Importing
// lib registers the pure Go driver for all four rssmix components.
func init() { sql.Register("sqlite3", &nativeSQLiteDriver{}) }

type nativeSQLiteDriver struct{ driver sqlite.Driver }

func (d *nativeSQLiteDriver) Open(name string) (driver.Conn, error) {
	name, err := sqliteDSN(name)
	if err != nil {
		return nil, err
	}
	return d.driver.Open(name)
}

// Preserve go-sqlite3's 5-second busy timeout on every physical connection.
// Explicit modernc _pragma settings take precedence. The old _busy_timeout
// and _timeout aliases are translated rather than silently ignored.
func sqliteDSN(name string) (string, error) {
	base, query := name, ""
	if i := strings.IndexByte(name, '?'); i >= 0 {
		base, query = name[:i], name[i+1:]
	}
	params, err := url.ParseQuery(query)
	if err != nil {
		return "", fmt.Errorf("invalid SQLite DSN: %w", err)
	}
	timeout := "5000"
	if v := params.Get("_busy_timeout"); v != "" {
		timeout = v
	}
	if v := params.Get("_timeout"); v != "" {
		timeout = v
	}
	// The timeout is injected as SQL only after strict integer validation.
	for _, c := range timeout {
		if c < '0' || c > '9' {
			return "", fmt.Errorf("invalid SQLite busy timeout %q", timeout)
		}
	}
	params.Del("_busy_timeout")
	params.Del("_timeout")
	pragmas := params["_pragma"]
	explicitTimeout := false
	for _, pragma := range pragmas {
		if strings.Contains(strings.ToLower(pragma), "busy_timeout") {
			explicitTimeout = true
		}
	}
	if !explicitTimeout {
		params["_pragma"] = append(pragmas, "busy_timeout("+timeout+")")
	}
	return base + "?" + params.Encode(), nil
}
