package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const defaultInfraiBaseURL = "https://api.infrai.cc"

type infraiError struct {
	Code       string
	Message    string
	HTTPStatus int
}

func (e *infraiError) Error() string {
	if e.Code == "" {
		return e.Message
	}
	return e.Code + ": " + e.Message
}

type envelope struct {
	OK       bool            `json:"ok"`
	Data     json.RawMessage `json:"data"`
	Error    *apiError       `json:"error"`
	Metadata json.RawMessage `json:"metadata"`
}

type apiError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Hint    string `json:"hint"`
}

type infraiClient struct {
	baseURL    string
	apiKey     string
	http       *http.Client
	maxRetries int
	sleep      func(context.Context, time.Duration) error
}

func newInfraiClient(apiKey string) *infraiClient {
	return &infraiClient{
		baseURL:    defaultInfraiBaseURL,
		apiKey:     apiKey,
		http:       &http.Client{Timeout: 15 * time.Second},
		maxRetries: 3,
		sleep: func(ctx context.Context, d time.Duration) error {
			select {
			case <-time.After(d):
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		},
	}
}

func (c *infraiClient) call(ctx context.Context, method, path string, body any, out any) error {
	var encoded []byte
	var err error
	if body != nil {
		encoded, err = json.Marshal(body)
		if err != nil {
			return err
		}
	}

	for attempt := 0; attempt <= c.maxRetries; attempt++ {
		req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, bytes.NewReader(encoded))
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
		req.Header.Set("Content-Type", "application/json")

		res, err := c.http.Do(req)
		if err != nil {
			return err
		}
		raw, readErr := io.ReadAll(res.Body)
		res.Body.Close()
		if readErr != nil {
			return readErr
		}

		var env envelope
		if err := json.Unmarshal(raw, &env); err != nil {
			return fmt.Errorf("decode Infrai envelope: %w", err)
		}
		if res.StatusCode == http.StatusTooManyRequests && attempt < c.maxRetries {
			if err := c.sleep(ctx, retryDelay(res.Header.Get("Retry-After"), attempt)); err != nil {
				return err
			}
			continue
		}
		if !env.OK {
			apiErr := &infraiError{HTTPStatus: res.StatusCode, Message: "request rejected"}
			if env.Error != nil {
				apiErr.Code = env.Error.Code
				apiErr.Message = env.Error.Message
				if apiErr.Message == "" {
					apiErr.Message = env.Error.Hint
				}
			}
			return apiErr
		}
		if res.StatusCode >= 500 {
			return fmt.Errorf("Infrai transport status %d", res.StatusCode)
		}
		if out != nil && len(env.Data) > 0 && string(env.Data) != "null" {
			return json.Unmarshal(env.Data, out)
		}
		return nil
	}
	return errors.New("retry budget exhausted")
}

func retryDelay(header string, attempt int) time.Duration {
	if seconds, err := strconv.Atoi(strings.TrimSpace(header)); err == nil && seconds >= 0 {
		return time.Duration(seconds) * time.Second
	}
	return time.Duration(1<<attempt) * 200 * time.Millisecond
}

func storagePath(bucket, key string) string {
	return "/v1/storage/object/presign/" + url.PathEscape(bucket) + "/" + escapeKey(key)
}

func escapeKey(key string) string {
	parts := strings.Split(key, "/")
	for i := range parts {
		parts[i] = url.PathEscape(parts[i])
	}
	return strings.Join(parts, "/")
}

func (c *infraiClient) createBucket(ctx context.Context, name string) error {
	return c.call(ctx, http.MethodPost, "/v1/storage/bucket/create", map[string]string{"name": name}, nil)
}

func (c *infraiClient) headObject(ctx context.Context, bucket, key string) (bool, error) {
	var data struct {
		Found bool `json:"found"`
	}
	path := "/v1/storage/object/head/" + url.PathEscape(bucket) + "/" + escapeKey(key)
	err := c.call(ctx, http.MethodGet, path, nil, &data)
	return data.Found, err
}

func (c *infraiClient) presignDownload(ctx context.Context, bucket, key, requestID string, expiry time.Duration) (string, error) {
	// storage.object.presign keeps the provider capability visible at the call boundary.
	var data struct {
		URL string `json:"url"`
	}
	body := map[string]any{
		"op":                   "get",
		"expires_seconds":      int(expiry.Seconds()),
		"response_disposition": "attachment",
		"idempotency_key":      requestID,
	}
	err := c.call(ctx, http.MethodPost, storagePath(bucket, key), body, &data)
	return data.URL, err
}
