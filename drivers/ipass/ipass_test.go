package ipass

import (
	"testing"

	"github.com/xo/dburl"
	"github.com/xo/usql/drivers"
)

func TestIPassURL(t *testing.T) {
	u, err := dburl.Parse("ipass://orders-prod")
	if err != nil {
		t.Fatal(err)
	}
	if u.Driver != "ipass" || u.DSN != "orders-prod" {
		t.Fatalf("unexpected parsed URL: driver=%q dsn=%q", u.Driver, u.DSN)
	}
	if !drivers.Registered("ipass") {
		t.Fatal("ipass usql driver was not registered")
	}
}

func TestIPassURLRejectsCredentialLikeComponents(t *testing.T) {
	for _, raw := range []string{
		"ipass://user:password@orders",
		"ipass://orders/database",
		"ipass://orders?authId=other",
		"ipass://orders#fragment",
	} {
		if _, err := dburl.Parse(raw); err == nil {
			t.Errorf("expected %q to be rejected", raw)
		}
	}
}
