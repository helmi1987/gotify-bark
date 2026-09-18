package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// appRefreshMinInterval limits how often an unknown app id triggers a GET /application call.
const appRefreshMinInterval = 30 * time.Second

// appCache maps Gotify application ids to their names.
type appCache struct {
	mu          sync.Mutex
	names       map[uint]string
	lastRefresh time.Time
}

type gotifyApplication struct {
	ID   uint   `json:"id"`
	Name string `json:"name"`
}

// appName returns the Gotify application name for an id, refreshing the cache
// from the Gotify API when the id is unknown (rate limited). Returns "" when unresolvable.
func (c *BarkForwardPlugin) appName(appID uint) string {
	c.apps.mu.Lock()
	defer c.apps.mu.Unlock()

	if name, ok := c.apps.names[appID]; ok {
		return name
	}
	if time.Since(c.apps.lastRefresh) < appRefreshMinInterval {
		return ""
	}
	c.apps.lastRefresh = time.Now()

	names, err := c.fetchApplications()
	if err != nil {
		logf("could not fetch application names from Gotify: %v", err)
		return ""
	}
	c.apps.names = names
	return names[appID]
}

// fetchApplications calls GET /application on the Gotify server with the client token.
func (c *BarkForwardPlugin) fetchApplications() (map[uint]string, error) {
	url := strings.TrimRight(c.config.GotifyHTTPURL, "/") + "/application"
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Gotify-Key", c.config.GotifyClientToken)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return nil, fmt.Errorf("GET %s returned %s: %s", url, resp.Status, strings.TrimSpace(string(body)))
	}

	var apps []gotifyApplication
	if err := json.NewDecoder(resp.Body).Decode(&apps); err != nil {
		return nil, fmt.Errorf("could not decode application list: %w", err)
	}
	names := make(map[uint]string, len(apps))
	for _, a := range apps {
		names[a.ID] = a.Name
	}
	return names, nil
}
