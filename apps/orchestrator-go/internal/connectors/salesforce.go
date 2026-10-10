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

	client := b.cfg.httpClient()
	token, err := sfOAuthToken(ctx, client, tokUrl, clientID, b.cfg.Secret)
	if err != nil {
		return nil, err
	}

	it := &sfIter{
		http:     client,
		instance: instanceURL,
		token:    token,
		jobID:    offset.SalesforceJobID,
		locator:  offset.SalesforceLocator,
		skip:     offset.SalesforceSkip,
		idx:      offset.Items,
	}
	if it.jobID == "" {
		// Offsets written before the job id was recorded point into a query
		// job that no longer exists; resuming would silently restart.
		if offset.Items > 0 || offset.SalesforceLocator != "" || offset.SalesforceSkip > 0 {
			return nil, fmt.Errorf("%w: salesforce offset at row %d has no query job", ErrInvalidOffset, offset.Items)
		}
		if it.jobID, err = sfCreateQueryJob(ctx, client, instanceURL, token, soql); err != nil {
			return nil, err
		}
	} else {
		it.ready = offset.Items > 0 || offset.SalesforceLocator != "" || offset.SalesforceSkip > 0
	}
	return it, nil
}

// sfPageSize is the maxRecords requested per Bulk API results page.
var sfPageSize = 2000

// sfIter reads one Bulk API 2.0 query job. Every offset it emits names the
// job, the page being consumed and the rows already taken from that page, so
// a later Open resumes at exactly the next record.
type sfIter struct {
	http     *http.Client
	instance string
	token    string
	jobID    string
	ready    bool

	locator  string // page being consumed; "" is the first page
	next     string // locator of the page after it; "" when it is the last
	skip     int64  // rows of the current page delivered before this iterator
	buffered []map[string]any
	loaded   bool
	cursor   int
	idx      int64
	done     bool
}

func (it *sfIter) offset() Offset {
	o := Offset{Items: it.idx, SalesforceJobID: it.jobID, SalesforceLocator: it.locator, SalesforceSkip: int64(it.cursor)}
	if !it.loaded {
		o.SalesforceSkip = it.skip
	} else if it.cursor >= len(it.buffered) && it.next != "" {
		o.SalesforceLocator, o.SalesforceSkip = it.next, 0
	}
	return o
}

func (it *sfIter) Next(ctx context.Context) (Row, Offset, bool, error) {
	if !it.ready {
		state, err := sfJobState(ctx, it.http, it.instance, it.token, it.jobID)
		if err != nil {
			return Row{}, it.offset(), false, err
		}
		switch state {
		case "JobComplete":
			it.ready = true
		case "Failed", "Aborted":
			return Row{}, it.offset(), false, fmt.Errorf("sf job %s: %s", it.jobID, state)
		default:
			return Row{}, it.offset(), false, ErrSourceNotReady
		}
	}
	for !it.loaded || it.cursor >= len(it.buffered) {
		if it.done {
			return Row{}, it.offset(), false, nil
		}
		if it.loaded {
			if it.next == "" {
				it.done = true
				return Row{}, it.offset(), false, nil
			}
			it.locator, it.skip = it.next, 0
		}
		rows, next, err := sfFetchResults(ctx, it.http, it.instance, it.token, it.jobID, it.locator, sfPageSize)
		if err != nil {
			return Row{}, it.offset(), false, err
		}
		if next == "null" {
			next = ""
		}
		if it.skip > int64(len(rows)) {
			return Row{}, it.offset(), false, fmt.Errorf("%w: page %q has %d rows, offset skips %d",
				ErrInvalidOffset, it.locator, len(rows), it.skip)
		}
		it.buffered, it.next, it.cursor, it.loaded = rows, next, int(it.skip), true
	}
	row := Row{Index: it.idx, Data: it.buffered[it.cursor]}
	it.cursor++
	it.idx++
	return row, it.offset(), true, nil
}

func (it *sfIter) Close() error { return nil }

// --- internal HTTP helpers ---

func sfOAuthToken(ctx context.Context, client *http.Client, tokenUrl, clientID, clientSecret string) (string, error) {
	form := url.Values{}
	form.Set("grant_type", "client_credentials")
	form.Set("client_id", clientID)
	form.Set("client_secret", clientSecret)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenUrl, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := client.Do(req)
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

func sfCreateQueryJob(ctx context.Context, client *http.Client, instance, token, soql string) (string, error) {
	body, _ := json.Marshal(map[string]any{"operation": "query", "query": soql})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, instance+"/services/data/v58.0/jobs/query", strings.NewReader(string(body)))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
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

func sfJobState(ctx context.Context, client *http.Client, instance, token, jobID string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, instance+"/services/data/v58.0/jobs/query/"+url.PathEscape(jobID), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", fmt.Errorf("sf job state: %s %s", resp.Status, string(raw))
	}
	var out struct {
		State string `json:"state"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("sf job state: %w", err)
	}
	return out.State, nil
}

func sfFetchResults(ctx context.Context, client *http.Client, instance, token, jobID, locator string, limit int) ([]map[string]any, string, error) {
	u := fmt.Sprintf("%s/services/data/v58.0/jobs/query/%s/results?maxRecords=%d", instance, url.PathEscape(jobID), limit)
	if locator != "" {
		u += "&locator=" + url.QueryEscape(locator)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := client.Do(req)
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
