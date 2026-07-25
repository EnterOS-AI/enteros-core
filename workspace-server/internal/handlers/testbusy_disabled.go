//go:build !e2e_busy_inject

package handlers

import "github.com/gin-gonic/gin"

// RegisterTestBusyRoutes is the PRODUCTION no-op: without the e2e_busy_inject
// build tag, no test-busy route is registered and the endpoint 404s like any
// unknown path. testBusyActiveTasksHook is left nil, so the heartbeat handler
// persists the runtime-reported active_tasks verbatim. This file is what every
// shipped tenant image compiles, so the test-only busy-inject lever does not
// exist in production at all — see testbusy.go for the safety rationale.
func RegisterTestBusyRoutes(_ gin.IRoutes) {}
