package staginge2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
)

// conciergeConfigTabVerdict is the pure result consumed by the tagged live
// staging test. Keeping the classifier untagged puts its fail directions in the
// normal unit-test gate without adding a live-runtime dependency.
type conciergeConfigTabVerdict uint8

const (
	conciergeConfigTabRejected conciergeConfigTabVerdict = iota
	conciergeConfigTabReachable
	conciergeConfigTabExpectedOffline
)

const conciergeSchedulesOfflineResponse = `{"error":"workspace url not registered yet"}`

// classifyConciergeConfigTabResponse keeps the auth sweep fail-closed while
// recognizing the one boot-free schedules result produced before the platform
// runtime registers its callback URL. Status, tab, and compact JSON body must
// all match; every other 5xx remains a server fault.
func classifyConciergeConfigTabResponse(tab string, statusCode int, body string) (conciergeConfigTabVerdict, string) {
	if statusCode == http.StatusUnauthorized || statusCode == http.StatusForbidden {
		return conciergeConfigTabRejected, fmt.Sprintf(
			"config-tab %q rejected the admin token (HTTP %d)", tab, statusCode)
	}

	if exactSchedulesOfflineResponse(body) {
		if tab == "schedules" && statusCode == http.StatusServiceUnavailable {
			return conciergeConfigTabExpectedOffline,
				"schedules is runtime-proxied and the unbooted platform agent has not registered its workspace URL"
		}
		return conciergeConfigTabRejected, fmt.Sprintf(
			"config-tab %q returned the schedules-offline body with unexpected HTTP %d", tab, statusCode)
	}

	if statusCode >= http.StatusInternalServerError {
		return conciergeConfigTabRejected, fmt.Sprintf(
			"config-tab %q returned unexpected server fault HTTP %d: %s", tab, statusCode, body)
	}

	return conciergeConfigTabReachable, fmt.Sprintf(
		"config-tab %q accepted the admin token with HTTP %d", tab, statusCode)
}

func exactSchedulesOfflineResponse(body string) bool {
	var compact bytes.Buffer
	if err := json.Compact(&compact, []byte(body)); err != nil {
		return false
	}
	return compact.String() == conciergeSchedulesOfflineResponse
}
