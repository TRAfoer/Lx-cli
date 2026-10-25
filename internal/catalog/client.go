// Native search adapters are ported from lux-native, Copyright (c) 2026 Xuepoo
// Foter, under the MIT license. See LICENSE in this directory.
package catalog

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const maxResponseBytes = 8 << 20
const userAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 Chrome/130.0 Safari/537.36"

// Client accesses the public platform endpoints. HTTPClient may be replaced by
// callers (including an httptest transport); it is never mutated by an operation.
type Client struct{ HTTPClient *http.Client }

func NewClient() *Client { return &Client{HTTPClient: &http.Client{Timeout: 15 * time.Second}} }

func safeHTTPURL(u *url.URL) bool {
	return u != nil && (u.Scheme == "http" || u.Scheme == "https") && u.Hostname() != "" && u.User == nil
}

func (c *Client) request(ctx context.Context, method, endpoint string, payload any, headers map[string]string) ([]byte, *url.URL, error) {
	u, err := url.Parse(endpoint)
	if err != nil || !safeHTTPURL(u) {
		return nil, nil, errors.New("only HTTP(S) URLs without credentials are allowed")
	}
	var body io.Reader
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return nil, nil, err
		}
		body = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/json, text/plain, */*")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	base := c.HTTPClient
	if base == nil {
		base = NewClient().HTTPClient
	}
	httpClient := *base
	if httpClient.Timeout <= 0 || httpClient.Timeout > 15*time.Second {
		httpClient.Timeout = 15 * time.Second
	}
	previousCheck := httpClient.CheckRedirect
	httpClient.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("redirect limit exceeded (5)")
		}
		if !safeHTTPURL(req.URL) {
			return errors.New("redirect must use HTTP(S) without credentials")
		}
		if previousCheck != nil {
			return previousCheck(req, via)
		}
		return nil
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, nil, fmt.Errorf("platform HTTP status %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return nil, nil, err
	}
	if len(data) > maxResponseBytes {
		return nil, nil, fmt.Errorf("platform response exceeds %d bytes", maxResponseBytes)
	}
	return data, resp.Request.URL, nil
}

func (c *Client) json(ctx context.Context, method, endpoint string, payload any, headers map[string]string) (map[string]any, error) {
	data, _, err := c.request(ctx, method, endpoint, payload, headers)
	if err != nil {
		return nil, err
	}
	return decodeJSON(data)
}

func decodeJSON(data []byte) (map[string]any, error) {
	var result map[string]any
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(&result); err != nil {
		return nil, fmt.Errorf("invalid platform JSON: %w", err)
	}
	if result == nil {
		return nil, errors.New("empty platform JSON")
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return nil, errors.New("unexpected data after platform JSON")
	}
	return result, nil
}

func queryURL(endpoint string, query url.Values) string { return endpoint + "?" + query.Encode() }
func object(value any) map[string]any                   { result, _ := value.(map[string]any); return result }
func list(value any) []any                              { result, _ := value.([]any); return result }
func str(value any) string {
	switch v := value.(type) {
	case string:
		return v
	case json.Number:
		return string(v)
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	case int:
		return strconv.Itoa(v)
	case int64:
		return strconv.FormatInt(v, 10)
	}
	return ""
}
func number(value any) int {
	n, _ := strconv.ParseFloat(str(value), 64)
	if n < 0 || n > 1<<31-1 {
		return 0
	}
	return int(n)
}
func first(values ...any) any {
	for _, value := range values {
		if str(value) != "" {
			return value
		}
	}
	return nil
}
func duration(seconds int) string { return fmt.Sprintf("%02d:%02d", seconds/60, seconds%60) }
func names(value any) string {
	var result []string
	for _, v := range list(value) {
		if name := str(object(v)["name"]); name != "" {
			result = append(result, html.UnescapeString(name))
		}
	}
	return strings.Join(result, "、")
}
func text(value any) string { return html.UnescapeString(str(value)) }
func validID(id string) bool {
	if id == "" || id == "0" {
		return false
	}
	for _, r := range id {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
func apiCode(body map[string]any, success int) error {
	code, ok := body["code"]
	if !ok || str(code) != strconv.Itoa(success) {
		return fmt.Errorf("platform rejected request (code %s): %s", str(code), text(first(body["message"], body["msg"])))
	}
	return nil
}
func rawMetadata(item map[string]any, extra map[string]any) json.RawMessage {
	copy := make(map[string]any, len(item)+len(extra))
	for key, value := range item {
		copy[key] = value
	}
	for key, value := range extra {
		copy[key] = value
	}
	result, _ := json.Marshal(copy)
	return result
}
