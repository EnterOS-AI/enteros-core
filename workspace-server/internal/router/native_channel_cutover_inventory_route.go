package router

import (
	"database/sql"

	"github.com/gin-gonic/gin"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/handlers"
	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/middleware"
)

const nativeChannelCutoverInventoryPath = "/admin/cutovers/native-channels/inventory"

// registerNativeChannelCutoverInventoryRoute owns both the temporary route path
// and its AdminAuth boundary so production Setup and route tests cannot drift.
// Delete this helper with the native subsystem in molecule-core#4267.
func registerNativeChannelCutoverInventoryRoute(routes gin.IRoutes, database *sql.DB) {
	h := handlers.NewNativeChannelCutoverInventoryHandler(database)
	routes.GET(nativeChannelCutoverInventoryPath, middleware.AdminAuth(database), h.Inventory)
}
