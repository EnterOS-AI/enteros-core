package push

import (
	"context"
	"database/sql"
	"log"
	"time"
)

// Notifier sends push notifications for agent messages.
type Notifier struct {
	repo   *Repo
	sender *Sender
}

// NewNotifier creates a Notifier.
func NewNotifier(db *sql.DB, sender *Sender) *Notifier {
	return &Notifier{
		repo:   NewRepo(db),
		sender: sender,
	}
}

// NotifyAgentMessage sends a push notification to all registered devices for a
// workspace when an agent sends a message. It runs asynchronously (fire-and-
// forget) so the caller's WebSocket broadcast is never blocked.
func (n *Notifier) NotifyAgentMessage(ctx context.Context, workspaceID, workspaceName, message string) {
	if n == nil || n.sender == nil {
		return
	}

	// Capture values for the goroutine.
	wsID := workspaceID
	wsName := workspaceName
	msg := message

	go func() {
		// Use a fresh context with timeout so a slow Expo API doesn't
		// leak the caller's context deadline.
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()

		tokens, err := n.repo.GetTokens(ctx, wsID)
		if err != nil {
			log.Printf("push: failed to get tokens for workspace %s: %v", wsID, err)
			return
		}
		if len(tokens) == 0 {
			return
		}

		// Expo accepts batches of up to ~100 messages; we cap lower to stay
		// well under the limit.
		const batchSize = 50
		for i := 0; i < len(tokens); i += batchSize {
			end := i + batchSize
			if end > len(tokens) {
				end = len(tokens)
			}

			batch := tokens[i:end]
			messages := make([]Message, 0, len(batch))
			for _, t := range batch {
				messages = append(messages, Message{
					To:    t.Token,
					Title: wsName,
					Body:  truncate(msg, 100),
					Data: map[string]string{
						"type":          "agent_message",
						"workspaceId":   wsID,
						"workspaceSlug": "", // populated by caller if available
					},
					Sound:    "default",
					Priority: "high",
				})
			}

			results, err := n.sender.Send(ctx, messages)
			if err != nil {
				log.Printf("push: send failed for workspace %s: %v", wsID, err)
				continue
			}

			// Remove invalid tokens.
			for j, r := range results {
				if ShouldRemoveToken(r) {
					if delErr := n.repo.DeleteToken(ctx, wsID, batch[j].Token); delErr != nil {
						log.Printf("push: failed to delete invalid token for workspace %s: %v", wsID, delErr)
					}
				}
			}
		}
	}()
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}
