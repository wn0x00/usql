// Package ipass defines and registers usql's Yingdao iPaaS managed SQL driver.
//
// Group: all
// See: https://github.com/wn0x00/usql
package ipass

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"strings"

	"github.com/xo/dburl"
	"github.com/xo/usql/drivers"
	transport "github.com/xo/usql/drivers/ipass/transport" // DRIVER
)

var aliasPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

func init() {
	dburl.Register(dburl.Scheme{
		Driver:    "ipass",
		Generator: generateDSN,
	})
	sql.Register("ipass", &transport.Driver{})
	drivers.Register("ipass", drivers.Driver{
		LexerName:      "sql",
		UseColumnTypes: true,
		Version: func(context.Context, drivers.DB) (string, error) {
			return "Yingdao iPaaS managed SQL", nil
		},
		User: func(context.Context, drivers.DB) (string, error) {
			return "managed", nil
		},
		Err: func(err error) (string, string) {
			var remote *transport.RemoteError
			if errors.As(err, &remote) {
				return remote.Code, remote.Message
			}
			return "", err.Error()
		},
	})
}

// generateDSN deliberately reduces ipass://ALIAS to the non-secret alias. The
// Adapter address and its short-lived run capability are read only from the
// process environment by the transport package and never become part of a DSN.
func generateDSN(u *dburl.URL) (string, string, error) {
	if u.User != nil || u.Port() != "" || u.RawQuery != "" || u.Fragment != "" {
		return "", "", errors.New("ipass URL only accepts a connection alias")
	}
	if u.Path != "" && u.Path != "/" {
		return "", "", errors.New("ipass URL must use ipass://ALIAS")
	}
	alias := strings.TrimSpace(u.Hostname())
	if !aliasPattern.MatchString(alias) {
		return "", "", errors.New("ipass connection alias must be 1-64 ASCII letters, digits, dots, underscores, or hyphens")
	}
	return alias, "", nil
}
