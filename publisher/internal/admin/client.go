package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/TyrEamon/xrw-album/publisher/internal/model"
)

type Client struct {
	baseURL string
	token   string
	client  *http.Client
}

type CheckResult struct {
	OK              bool   `json:"ok"`
	Status          string `json:"status"`
	SourceUpdatedAt string `json:"source_updated_at"`
	ExpectedCount   int    `json:"expected_count"`
	PublishedCount  int    `json:"published_count"`
}

func New(baseURL, token string, timeout time.Duration) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   token,
		client:  &http.Client{Timeout: timeout},
	}
}

func (c *Client) Enabled() bool {
	return c != nil && c.baseURL != ""
}

func (c *Client) Check(ctx context.Context, albumID string) (CheckResult, error) {
	endpoint := c.baseURL + "/check?id=" + url.QueryEscape(albumID)
	var result CheckResult
	if err := c.request(ctx, http.MethodGet, endpoint, nil, &result); err != nil {
		return CheckResult{}, err
	}
	return result, nil
}

func (c *Client) Publish(ctx context.Context, payload model.PublishPayload) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	var result struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := c.request(ctx, http.MethodPost, c.baseURL+"/publish", data, &result); err != nil {
		return err
	}
	if !result.OK {
		return fmt.Errorf("publish rejected: %s", result.Error)
	}
	return nil
}

func (c *Client) request(ctx context.Context, method, endpoint string, body []byte, target any) error {
	request, err := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+c.token)
	request.Header.Set("Accept", "application/json")
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := c.client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 2<<20))
	if err != nil {
		return err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("admin API HTTP %d: %s", response.StatusCode, strings.TrimSpace(string(data)))
	}
	if err := json.Unmarshal(data, target); err != nil {
		return fmt.Errorf("decode admin API response: %w", err)
	}
	return nil
}
