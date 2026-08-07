// Salesforce connector backed by Bulk API 2.0 (with support for incremental
// queries via `SystemModstamp >= :cursor`). OAuth2 client credentials flow is
// used to obtain a short-lived access token.
package connectors

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type salesforceBuilder struct{ cfg Config }

func NewSalesforce(cfg Config) (SourceConnector, error) { return &salesforceBuilder{cfg: cfg}, nil }

func (b *salesforceBuilder) Kind() string { return "salesforce" }

func (b *salesforceBuilder) Open(ctx context.Context, offset Offset) (RowIterator, error) {
	soql, _ := b.cfg.Config["soql"].(string)
	if soql == "" {
		return nil, errors.New("salesforce connector requires 'soql' in config")
	}
	tokUrl, _ := b.cfg.Config["tokenUrl"].(string)
	if tokUrl == "" {
		return nil, errors.New("salesforce connector requires 'tokenUrl'")
	}
	clientID, _ := b.cfg.Config["clientId"].(string)
	instanceURL, _ := b.cfg.Config["instanceUrl"].(string)
	cursorField, _ := b.cfg.Config["cursorField"].(string)
	cursor, _ := b.cfg.Config["cursor"].(string)

	if cursorField != "" && cursor != "" {
		if strings.Contains(soql, "WHERE") {
			soql += fmt.Sprintf(" AND %s >= %s", cursorField, cursor)
		} else {
			soql += fmt.Sprintf(" WHERE %s >= %s", cursorField, cursor)
		}
	}

	token, err := sfOAuthToken(ctx, tokUrl, clientID, b.cfg.Secret)
	if err != nil {
		return nil, err
	}

	jobID, err := sfCreateQueryJob(ctx, instanceURL, token, soql)
	if err != nil {
		return nil, err
	}
	if err := sfWaitJobComplete(ctx, instanceURL, token, jobID); err != nil {
		return nil, err
	}

	return &sfIter{
		instance: instanceURL,
		token:    token,
		jobID:    jobID,
		locator:  offset.SalesforceLocator,
	}, nil
}

type sfIter struct {
	instance  string
	token     string
	jobID     string
	locator   string
	buffered  []map[string]any
	idx       int64
	bufCursor int
	done      bool
}

func (it *sfIter) Next(ctx context.Context) (Row, Offset, bool, error) {
	if it.bufCursor >= len(it.buffered) {
		if it.done {
			return Row{}, Offset{Items: it.idx, SalesforceLocator: ""}, false, nil
		}
		rows, nextLocator, err := sfFetchResults(ctx, it.instance, it.token, it.jobID, it.locator, 2000)
		if err != nil {
			return Row{}, Offset{Items: it.idx, SalesforceLocator: it.locator}, false, err
		}
		if len(rows) == 0 || nextLocator == "null" || nextLocator == "" {
			it.done = true
			if len(rows) == 0 {
				return Row{}, Offset{Items: it.idx}, false, nil
			}
		}
		it.buffered = rows
		it.bufCursor = 0
		it.locator = nextLocator
	}
	row := Row{Index: it.idx, Data: it.buffered[it.bufCursor]}
	it.bufCursor++
	it.idx++
	return row, Offset{Items: it.idx, SalesforceLocator: it.locator}, true, nil
}

func (it *sfIter) Close() error { return nil }

// --- internal HTTP helpers ---

func sfOAuthToken(ctx context.Context, tokenUrl, clientID, clientSecret string) (string, error) {
	form := url.Values{}
	form.Set("grant_type", "client_credentials")
	form.Set("client_id", clientID)
	form.Set("client_secret", clientSecret)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenUrl, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("sf oauth: %s: %s", resp.Status, string(body))
	}
	var payload struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return "", err
	}
	return payload.AccessToken, nil
}

func sfCreateQueryJob(ctx context.Context, instance, token, soql string) (string, error) {
	body, _ := json.Marshal(map[string]any{"operation": "query", "query": soql})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, instance+"/services/data/v58.0/jobs/query", strings.NewReader(string(body)))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("sf create job: %s %s", resp.Status, string(raw))
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	return out.ID, nil
}

func sfWaitJobComplete(ctx context.Context, instance, token, jobID string) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, instance+"/services/data/v58.0/jobs/query/"+jobID, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return err
		}
		var out struct {
			State string `json:"state"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&out)
		_ = resp.Body.Close()
		switch out.State {
		case "JobComplete":
			return nil
		case "Failed", "Aborted":
			return fmt.Errorf("sf job %s: %s", jobID, out.State)
		}
	}
}

func sfFetchResults(ctx context.Context, instance, token, jobID, locator string, limit int) ([]map[string]any, string, error) {
	u := fmt.Sprintf("%s/services/data/v58.0/jobs/query/%s/results?maxRecords=%d", instance, jobID, limit)
	if locator != "" {
		u += "&locator=" + url.QueryEscape(locator)
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	next := resp.Header.Get("Sforce-Locator")
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(resp.Body)
		return nil, next, fmt.Errorf("sf results: %s %s", resp.Status, string(raw))
	}
	rows, err := parseCSVtoMaps(resp.Body)
	return rows, next, err
}
