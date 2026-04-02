//go:build !cgo
// +build !cgo

package sqlite

import (
	"context"
	"database/sql"
	"errors"

	"github.com/k3s-io/kine/pkg/drivers"
	"github.com/k3s-io/kine/pkg/server"
)

var errNoCgo = errors.New("this binary is built without CGO, sqlite is disabled")

func New(ctx context.Context, cfg drivers.Config) (bool, server.Backend, error) {
	return false, nil, errNoCgo
}

func NewVariant(driverName string, cfg drivers.Config) (server.Backend, error) {
	return nil, errNoCgo
}

func setup(db *sql.DB) error {
	return errNoCgo
}
