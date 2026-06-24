package push

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

const expoPushAPI = "https://exp.host/--/api/v2/push/send"

// Message is one Expo push notification.
type Message struct {
	To       string            `json:"to"`
	Title    string            `json:"title,omitempty"`
	Body     string            `json:"body,omitempty"`
	Data     map[string]string `json:"data,omitempty"`
	Sound    string            `json:"sound,omitempty"`
	Priority string            `json:"priority,omitempty"`
}

// Sender delivers push notifications via the Expo Push Service.
type Sender struct {
	apiURL     string
	httpClient *http.Client
	expoToken  string // optional Expo access token for authenticated requests
}

// NewSender creates a Sender. expoToken may be empty for unauthenticated
// requests (sufficient for most use cases).
func NewSender(expoToken string) *Sender {
	return &Sender{
		apiURL: expoPushAPI,
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
		},
		expoToken: expoToken,
	}
}

// SendResult is the per-recipient status from Expo.
type SendResult struct {
	Status  string `json:"status"`
	ID      string `json:"id"`
	Message string `json:"message,omitempty"`
	Details struct {
		Error string `json:"error,omitempty"`
	} `json:"details,omitempty"`
}

// expoResponse is the wrapper shape returned by the Expo API.
type expoResponse struct {
	Data []SendResult `json:"data"`
}

// Send fires a batch of push messages. It returns a slice of results in the
// same order as the input, plus an error only when the HTTP call itself fails.
// Callers should inspect each result's Status field for per-message errors
// (e.g. "DeviceNotRegistered" → token should be deleted).
func (s *Sender) Send(ctx context.Context, messages []Message) ([]SendResult, error) {
	if len(messages) == 0 {
		return nil, nil
	}

	body, err := json.Marshal(messages)
	if err != nil {
		return nil, fmt.Errorf("push: marshal: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.apiURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("push: new request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if s.expoToken != "" {
		req.Header.Set("Authorization", "Bearer "+s.expoToken)
	}

	res, err := s.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("push: post: %w", err)
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("push: expo returned %d", res.StatusCode)
	}

	var resp expoResponse
	if err := json.NewDecoder(res.Body).Decode(&resp); err != nil {
		return nil, fmt.Errorf("push: decode: %w", err)
	}
	return resp.Data, nil
}

// ShouldRemoveToken reports whether a SendResult indicates the token is no
// longer valid and should be deleted from the database.
func ShouldRemoveToken(r SendResult) bool {
	return r.Status == "error" && r.Details.Error == "DeviceNotRegistered"
}
