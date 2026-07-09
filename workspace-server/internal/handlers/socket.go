package handlers

import (
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"log"
	"net/http"
	"os"
	"strings"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/db"
	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/metrics"
	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/middleware"
	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/orgtoken"
	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/ws"
	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/wsauth"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		// In production, validate against CORS_ORIGINS. In dev, allow all.
		origins := os.Getenv("CORS_ORIGINS")
		if origins == "" {
			return true // dev mode — no restriction
		}
		origin := r.Header.Get("Origin")
		for _, allowed := range strings.Split(origins, ",") {
			if strings.EqualFold(strings.TrimSpace(allowed), origin) {
				return true
			}
		}
		return false
	},
}

type SocketHandler struct {
	hub *ws.Hub
}

const (
	// Browsers cannot attach Authorization to a WebSocket constructor. Canvas
	// therefore hex-encodes its existing admin bearer into an offered protocol.
	// The server consumes this credential for the handshake but never selects or
	// echoes the protocol back to the client.
	websocketAuthProtocolPrefix = "molecule-auth."
	websocketAuthProtocolMaxHex = 8192
)

var errInvalidWebSocketCredential = errors.New("invalid WebSocket credential")

func NewSocketHandler(hub *ws.Hub) *SocketHandler {
	return &SocketHandler{hub: hub}
}

// HandleConnect handles the authenticated WebSocket upgrade at GET /ws.
// Privileged canvas clients omit X-Workspace-ID and receive the global stream;
// workspace agents provide X-Workspace-ID and receive only events permitted by
// CanCommunicate. Both paths authenticate before the protocol upgrade.
func (h *SocketHandler) HandleConnect(c *gin.Context) {
	workspaceID := c.GetHeader("X-Workspace-ID")
	if !authenticateWebSocket(c, workspaceID) {
		return
	}

	conn, err := upgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		log.Printf("WebSocket upgrade error: %v", err)
		return
	}

	client := &ws.Client{
		Conn:        conn,
		WorkspaceID: workspaceID,
		Send:        make(chan []byte, 256),
	}

	h.hub.Register <- client
	metrics.TrackWSConnect()

	// Wrap WritePump and ReadPump so the gauge is decremented exactly once
	// when the client's write goroutine exits (WritePump owns conn lifetime).
	// goAsync-exempt (RFC internal#524 Layer 2.2): WebSocket pumps live
	// for the duration of the client connection (minutes-hours), not a
	// single request. Wrapping them in globalGoAsync would block every
	// test's t.Cleanup until every connected WS client disconnects. No
	// db.DB access in either pump.
	go func() {
		ws.WritePump(client)
		metrics.TrackWSDisconnect()
	}()
	go ws.ReadPump(client, h.hub)
}

// authenticateWebSocket protects the two different trust surfaces behind
// GET /ws. A workspace-scoped stream requires the token for that exact
// workspace. The global canvas stream requires a verified CP tenant-member
// session, ADMIN_TOKEN, or an org-scoped token; a workspace token never grants
// access to every event.
func authenticateWebSocket(c *gin.Context, workspaceID string) bool {
	token, err := websocketBearerToken(c)
	if err != nil {
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid WebSocket credential"})
		return false
	}

	ctx := c.Request.Context()
	if workspaceID != "" {
		if token == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "missing workspace auth token"})
			return false
		}
		if err := wsauth.ValidateToken(ctx, db.DB, workspaceID, token); err != nil {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid workspace auth token"})
			return false
		}
		return true
	}

	if middleware.IsVerifiedCanvasSession(c) {
		return true
	}
	if token == "" {
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "canvas auth required"})
		return false
	}

	adminSecret := os.Getenv("ADMIN_TOKEN")
	if adminSecret != "" && subtle.ConstantTimeCompare([]byte(token), []byte(adminSecret)) == 1 {
		return true
	}

	if _, _, _, err := orgtoken.Validate(ctx, db.DB, token, orgtoken.AuditLogRequestContextFromGin(c), "", false); err == nil {
		return true
	} else if !errors.Is(err, orgtoken.ErrInvalidToken) {
		log.Printf("WebSocket org-token validation failed: %v", err)
		c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{
			"error": "platform datastore unavailable — retry shortly",
			"code":  "platform_unavailable",
		})
		return false
	}

	c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid canvas auth token"})
	return false
}

// websocketBearerToken resolves a bearer from either Authorization (native
// clients) or the browser-compatible molecule-auth.<hex> subprotocol. Invalid,
// duplicate, or conflicting credentials are rejected instead of falling back
// to another authentication path.
func websocketBearerToken(c *gin.Context) (string, error) {
	authorizationValues := c.Request.Header.Values("Authorization")
	if len(authorizationValues) > 1 {
		return "", errInvalidWebSocketCredential
	}
	authorization := ""
	if len(authorizationValues) == 1 {
		authorization = authorizationValues[0]
	}
	headerToken := wsauth.BearerTokenFromHeader(authorization)
	if authorization != "" && headerToken == "" {
		return "", errInvalidWebSocketCredential
	}

	protocolToken := ""
	for _, protocolHeader := range c.Request.Header.Values("Sec-WebSocket-Protocol") {
		for _, offered := range strings.Split(protocolHeader, ",") {
			offered = strings.TrimSpace(offered)
			if !strings.HasPrefix(offered, websocketAuthProtocolPrefix) {
				continue
			}
			if protocolToken != "" {
				return "", errInvalidWebSocketCredential
			}
			encoded := strings.TrimPrefix(offered, websocketAuthProtocolPrefix)
			if encoded == "" || len(encoded) > websocketAuthProtocolMaxHex || len(encoded)%2 != 0 {
				return "", errInvalidWebSocketCredential
			}
			decoded, err := hex.DecodeString(encoded)
			if err != nil || len(decoded) == 0 {
				return "", errInvalidWebSocketCredential
			}
			protocolToken = string(decoded)
		}
	}

	if headerToken != "" && protocolToken != "" {
		if subtle.ConstantTimeCompare([]byte(headerToken), []byte(protocolToken)) != 1 {
			return "", errInvalidWebSocketCredential
		}
	}
	if headerToken != "" {
		return headerToken, nil
	}
	return protocolToken, nil
}
