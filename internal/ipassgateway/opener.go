package ipassgateway

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/xo/dburl"
	"github.com/xo/usql/drivers"
)

var (
	ErrConnectionStringInvalid = errors.New("connection string is invalid")
	ErrDriverNotAllowed        = errors.New("database driver is not allowed")
	ErrDriverUnavailable       = errors.New("database driver is unavailable")
	driverNamePattern          = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)
	supportedGatewayDrivers    = map[string]struct{}{
		"moderncsqlite": {},
		"mysql":         {},
		"postgres":      {},
		"sqlserver":     {},
	}
)

// DatabaseOpener opens a database using a connection string supplied by the
// trusted iPaaS Action. Implementations must not log or return that string.
type DatabaseOpener interface {
	Open(context.Context, string) (*sql.DB, error)
}

// DBURLOpener resolves dburl connection strings and opens only the configured
// canonical usql drivers. A new *sql.DB is returned for every gateway request;
// callers are responsible for closing it.
type DBURLOpener struct {
	allowed map[string]struct{}
}

// NewDBURLOpener creates a fail-closed opener. The allowlist accepts canonical
// driver names only and every named driver must be linked into the binary.
func NewDBURLOpener(allowedDrivers []string) (*DBURLOpener, error) {
	allowed := make(map[string]struct{}, len(allowedDrivers))
	for _, rawName := range allowedDrivers {
		name := strings.ToLower(strings.TrimSpace(rawName))
		if !driverNamePattern.MatchString(name) {
			return nil, ErrDriverNotAllowed
		}
		if _, supported := supportedGatewayDrivers[name]; !supported {
			return nil, ErrDriverNotAllowed
		}
		if !drivers.Registered(name) {
			return nil, ErrDriverUnavailable
		}
		allowed[name] = struct{}{}
	}
	if len(allowed) == 0 {
		return nil, ErrDriverNotAllowed
	}
	return &DBURLOpener{allowed: allowed}, nil
}

// AllowedDrivers returns a sorted copy suitable for non-secret startup logs.
func (o *DBURLOpener) AllowedDrivers() []string {
	result := make([]string, 0, len(o.allowed))
	for name := range o.allowed {
		result = append(result, name)
	}
	sort.Strings(result)
	return result
}

// Open implements DatabaseOpener.
func (o *DBURLOpener) Open(ctx context.Context, connectionString string) (*sql.DB, error) {
	parsed, err := dburl.Parse(connectionString)
	if err != nil {
		return nil, ErrConnectionStringInvalid
	}
	if _, ok := o.allowed[parsed.Driver]; !ok {
		return nil, ErrDriverNotAllowed
	}
	if !drivers.Registered(parsed.Driver) {
		return nil, ErrDriverUnavailable
	}
	drivers.ForceParams(parsed)
	database, err := drivers.Open(
		ctx,
		parsed,
		func() io.Writer { return io.Discard },
		func() io.Writer { return io.Discard },
	)
	if err != nil {
		return nil, errors.New("database open failed")
	}
	// The gateway is stateless. Keep its per-request pool deliberately small
	// and short-lived so one request cannot retain an iPaaS-managed credential.
	database.SetMaxOpenConns(2)
	database.SetMaxIdleConns(0)
	database.SetConnMaxIdleTime(time.Second)
	return database, nil
}
