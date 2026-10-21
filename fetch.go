package main

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/viper"
)

func validateTileURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("tile URL must be a valid HTTP(S) URL")
	}
	query, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return fmt.Errorf("tile URL query must use valid URL encoding: %w", err)
	}
	if host := strings.ToLower(u.Hostname()); host == "tianditu.gov.cn" || strings.HasSuffix(host, ".tianditu.gov.cn") {
		if strings.HasSuffix(strings.ToLower(query.Get("T")), "_c") {
			return fmt.Errorf("Tianditu *_c uses a geographic tile matrix, which is unsupported; use the corresponding *_w Web Mercator layer")
		}
	}
	return nil
}

func (task *Task) fetchTile(ctx context.Context, tileURL string) ([]byte, error) {
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, tileURL, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("User-Agent", task.userAgent)
		if task.referer != "" {
			req.Header.Set("Referer", task.referer)
		} else if host := strings.ToLower(req.URL.Hostname()); host == "tianditu.gov.cn" || strings.HasSuffix(host, ".tianditu.gov.cn") {
			req.Header.Set("Referer", "https://map.tianditu.gov.cn")
		}
		resp, err := task.httpClient.Do(req)
		delay := time.Duration(1<<uint(attempt)) * 250 * time.Millisecond
		retry := err != nil
		if resp != nil {
			if resp.StatusCode == http.StatusOK {
				body, readErr := readTileData(resp.Body)
				resp.Body.Close()
				if readErr == nil && len(body) > 0 {
					return body, nil
				}
				err = readErr
				if err == nil {
					err = fmt.Errorf("empty tile response")
				}
				retry = true
			} else {
				resp.Body.Close()
				err = fmt.Errorf("HTTP status %d", resp.StatusCode)
				retry = resp.StatusCode == 408 || resp.StatusCode == 429 || resp.StatusCode >= 500
				if seconds, parseErr := strconv.Atoi(resp.Header.Get("Retry-After")); parseErr == nil && seconds >= 0 && seconds <= 60 {
					delay = time.Duration(seconds) * time.Second
				} else if until, parseErr := http.ParseTime(resp.Header.Get("Retry-After")); parseErr == nil {
					if wait := time.Until(until); wait > 0 && wait <= time.Minute {
						delay = wait
					}
				}
			}
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if !retry || attempt >= task.retries {
			return nil, err
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

func (task *Task) setupHTTP() error {
	timeout := 30
	if viper.IsSet("task.timeout_seconds") {
		timeout = viper.GetInt("task.timeout_seconds")
	}
	if timeout <= 0 || int64(timeout) > int64((1<<63-1)/time.Second) {
		return fmt.Errorf("task.timeout_seconds must be positive")
	}
	task.retries = viper.GetInt("task.retries")
	if task.retries < 0 || task.retries > 10 {
		return fmt.Errorf("task.retries must be between 0 and 10")
	}
	task.userAgent = viper.GetString("task.user_agent")
	if task.userAgent == "" {
		task.userAgent = "Mozilla/5.0 (compatible; Tiler)"
	}
	task.referer = viper.GetString("task.referer")
	// Use the standard redirect and proxy behavior for all tile providers.
	task.httpClient = &http.Client{Timeout: time.Duration(timeout) * time.Second}
	task.httpClient.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return fmt.Errorf("stopped after 10 redirects")
		}
		if task.referer != "" {
			req.Header.Set("Referer", task.referer)
		}
		return nil
	}
	return nil
}
