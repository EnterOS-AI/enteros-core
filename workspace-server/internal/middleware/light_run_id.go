package middleware

// light_run_id.go — Enter OS Light, contract C3b: every request a Light run
// pod makes to core carries X-Molecule-Run-Id: <the run's Job name>. Core puts
// the run on the request context so the activity rows the request writes carry
// it (activity_logs.run_id), and a run's tool calls and messages can be listed
// per run (GET /workspaces/:id/activity?run_id=).
//
// The header is honoured only on a request authenticated by a workspace token
// (any kind: a run pod holds its agent's API-kind run token): the run belongs
// to that token's workspace. Anything else — no bearer, an admin or org token,
// a malformed value — leaves the request untagged. The middleware never fails
// a request.

import (
	"context"
	"database/sql"
	"regexp"
	"strings"

	"github.com/gin-gonic/gin"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/wsauth"
)

// LightRunIDHeader is the header a run pod's requests carry (C3b).
const LightRunIDHeader = "X-Molecule-Run-Id"

// lightRunIDRe matches the runtime's own sanitisation of MOLECULE_RUN_ID:
// printable ASCII, no space, 1..128 bytes.
var lightRunIDRe = regexp.MustCompile(`^[\x21-\x7e]{1,128}$`)

// ValidLightRunID reports whether s is a run id core accepts.
func ValidLightRunID(s string) bool { return lightRunIDRe.MatchString(s) }

type lightRunCtxKey struct{}

type lightRun struct{ runID, workspaceID string }

// WithLightRun returns ctx carrying the run of workspaceID.
func WithLightRun(ctx context.Context, runID, workspaceID string) context.Context {
	return context.WithValue(ctx, lightRunCtxKey{}, lightRun{runID: runID, workspaceID: workspaceID})
}

// LightRunFromContext returns the run a request was made by, if any.
func LightRunFromContext(ctx context.Context) (runID, workspaceID string, ok bool) {
	v, ok := ctx.Value(lightRunCtxKey{}).(lightRun)
	if !ok {
		return "", "", false
	}
	return v.runID, v.workspaceID, true
}

// LightRunID tags a run pod's requests with their run (see the file comment).
func LightRunID(database *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		raw := c.GetHeader(LightRunIDHeader)
		if raw == "" {
			c.Next()
			return
		}
		runID := strings.TrimSpace(raw)
		tok := wsauth.BearerTokenFromHeader(c.GetHeader("Authorization"))
		if !ValidLightRunID(runID) || tok == "" || database == nil {
			c.Next()
			return
		}
		workspaceID, err := wsauth.WorkspaceFromToken(c.Request.Context(), database, tok)
		if err != nil {
			c.Next()
			return
		}
		c.Request = c.Request.WithContext(WithLightRun(c.Request.Context(), runID, workspaceID))
		c.Next()
	}
}
