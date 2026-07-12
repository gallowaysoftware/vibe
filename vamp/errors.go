package vamp

import (
	internalvamp "github.com/gallowaysoftware/vibe/internal/vamp"
)

// RouterError is the typed error EnsureProfile / EnsureCapability return for
// model-router failures (llama-swap or vibe's own proxy on :9000). Match the
// class with errors.Is against the Err* sentinels, or errors.As to a
// *RouterError for the Model / Detail fields:
//
//	ep, err := vamp.EnsureCapability(ctx, "reasoning")
//	switch {
//	case errors.Is(err, vamp.ErrNotFound):     // fix capabilities.yaml / catalog
//	case errors.Is(err, vamp.ErrCapacity):     // shed load / free VRAM, retry later
//	case errors.Is(err, vamp.ErrStartFailed):  // router took it, model didn't come up
//	case errors.Is(err, vamp.ErrUpstreamDown): // nothing listening on the router port
//	}
type RouterError = internalvamp.RouterError

// RouterErrorCode is the failure class carried by RouterError.Code.
type RouterErrorCode = internalvamp.RouterErrorCode

const (
	RouterStartFailed  = internalvamp.RouterStartFailed
	RouterNotFound     = internalvamp.RouterNotFound
	RouterCapacity     = internalvamp.RouterCapacity
	RouterUpstreamDown = internalvamp.RouterUpstreamDown
)

// Sentinels for errors.Is checks, one per RouterErrorCode.
var (
	ErrStartFailed  = internalvamp.ErrStartFailed
	ErrNotFound     = internalvamp.ErrNotFound
	ErrCapacity     = internalvamp.ErrCapacity
	ErrUpstreamDown = internalvamp.ErrUpstreamDown
)
