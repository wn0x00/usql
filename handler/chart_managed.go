//go:build managed

package handler

import (
	"context"
	"io"

	"github.com/xo/usql/internal/managedpolicy"
	"github.com/xo/usql/metacmd"
)

// doExecChart is retained as a stub so the core dispatcher has one shape in
// all builds. Managed binaries neither link the renderer nor execute charts.
func (h *Handler) doExecChart(context.Context, io.Writer, metacmd.Option, string, string, bool, []interface{}) error {
	return managedpolicy.FeatureDisabled("chart rendering")
}
