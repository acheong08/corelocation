package corelocation

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

var (
	ErrNoResults        = errors.New("no usable locations returned")
	ErrInvalidResponse  = errors.New("invalid service response")
	ErrResponseTooLarge = errors.New("service response exceeds size limit")
)

// HTTPError preserves status and Retry-After without exposing response bodies.
type HTTPError struct {
	StatusCode int
	RetryAfter string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("HTTP %d %s", e.StatusCode, http.StatusText(e.StatusCode))
}

type Endpoints struct {
	WLocInternational string
	WLocChina         string
	TileInternational string
	TileChina         string
}

func DefaultEndpoints() Endpoints {
	return Endpoints{
		WLocInternational: "https://gs-loc.apple.com/clls/wloc",
		WLocChina:         "https://gs-loc-cn.apple.com/clls/wloc",
		TileInternational: "https://gspe85-ssl.ls.apple.com/wifi_request_tile",
		TileChina:         "https://gspe85-cn-ssl.ls.apple.com/wifi_request_tile",
	}
}

type Config struct {
	// HTTPClient is copied at construction. Its Transport must support concurrent
	// use. Redirects are always disabled to avoid forwarding location queries.
	HTTPClient *http.Client
	// Region sets first preference, not a hard pin. Pair with DisableFallback to
	// guarantee queries are sent to only one region.
	Region          Region
	DisableFallback bool
	// AttemptTimeout defaults to 10 seconds; the caller's context bounds the
	// entire operation, including both attempts.
	AttemptTimeout time.Duration
	// MaxResponseBytes defaults to 8 MiB, including the WLOC envelope.
	MaxResponseBytes int64
	// Empty endpoint fields use defaults. Overrides are useful for local testing.
	Endpoints Endpoints
}

// Client is immutable after construction and safe for concurrent use.
type Client struct {
	http      *http.Client
	region    Region
	fallback  bool
	timeout   time.Duration
	maxBytes  int64
	endpoints Endpoints
}

func NewClient(cfg Config) (*Client, error) {
	if cfg.Region == "" {
		cfg.Region = Auto
	}
	if cfg.Region != Auto && cfg.Region != International && cfg.Region != China {
		return nil, fmt.Errorf("invalid region %q", cfg.Region)
	}
	if cfg.AttemptTimeout < 0 {
		return nil, fmt.Errorf("attempt timeout must be positive")
	}
	if cfg.AttemptTimeout == 0 {
		cfg.AttemptTimeout = 10 * time.Second
	}
	if cfg.MaxResponseBytes < 0 || cfg.MaxResponseBytes == 1<<63-1 {
		return nil, fmt.Errorf("invalid response size limit")
	}
	if cfg.MaxResponseBytes == 0 {
		cfg.MaxResponseBytes = 8 << 20
	}
	ep := cfg.Endpoints
	defaults := DefaultEndpoints()
	for _, pair := range []struct {
		p   *string
		def string
	}{
		{&ep.WLocInternational, defaults.WLocInternational}, {&ep.WLocChina, defaults.WLocChina},
		{&ep.TileInternational, defaults.TileInternational}, {&ep.TileChina, defaults.TileChina},
	} {
		if *pair.p == "" {
			*pair.p = pair.def
		}
		u, err := url.Parse(*pair.p)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.Fragment != "" {
			return nil, fmt.Errorf("invalid endpoint URL %q", *pair.p)
		}
	}
	hc := &http.Client{}
	if cfg.HTTPClient != nil {
		*hc = *cfg.HTTPClient
	}
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Client{http: hc, region: cfg.Region, fallback: !cfg.DisableFallback, timeout: cfg.AttemptTimeout, maxBytes: cfg.MaxResponseBytes, endpoints: ep}, nil
}

func (c *Client) order(hint *Point, mcc uint32) []Region {
	first := c.region
	if first == Auto {
		first = PreferredRegion(hint, mcc)
	}
	if !c.fallback {
		return []Region{first}
	}
	second := China
	if first == China {
		second = International
	}
	return []Region{first, second}
}
func canFallback(err error) bool {
	if errors.Is(err, ErrNoResults) {
		return true
	}
	var he *HTTPError
	if errors.As(err, &he) {
		return he.StatusCode == 404 || he.StatusCode == 408 || he.StatusCode == 429 || he.StatusCode >= 500
	}
	// Request and body-read failures retain *url.Error or a transport wrapper.
	var te *transportError
	return errors.As(err, &te)
}

type transportError struct{ err error }

func (e *transportError) Error() string { return e.err.Error() }
func (e *transportError) Unwrap() error { return e.err }

func query[T any](ctx context.Context, c *Client, hint *Point, mcc uint32, fetch func(context.Context, Region) ([]T, error)) (Result[T], error) {
	result := Result[T]{Records: make([]T, 0), Attempts: make([]Attempt, 0, 2)}
	var failures []error
	for _, region := range c.order(hint, mcc) {
		if err := ctx.Err(); err != nil {
			return result, errors.Join(append(failures, err)...)
		}
		attemptCtx, cancel := context.WithTimeout(ctx, c.timeout)
		records, err := fetch(attemptCtx, region)
		// The deadline covers decoding/filtering as well as HTTP I/O. A body
		// may finish just before cancellation while its decoded result arrives
		// afterwards; do not report that late result as successful.
		if err == nil && attemptCtx.Err() != nil {
			err = &transportError{attemptCtx.Err()}
		}
		cancel()
		if err == nil && len(records) == 0 {
			err = ErrNoResults
		}
		a := Attempt{Region: region}
		if err != nil {
			a.Error = err.Error()
		}
		result.Attempts = append(result.Attempts, a)
		if err == nil {
			result.Records = records
			result.Region = region
			return result, nil
		}
		failures = append(failures, fmt.Errorf("%s: %w", region, err))
		if ctx.Err() != nil {
			return result, errors.Join(append(failures, ctx.Err())...)
		}
		if !canFallback(err) {
			break
		}
	}
	return result, errors.Join(failures...)
}

func (c *Client) request(ctx context.Context, method, endpoint string, body []byte, tileKey string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Accept-Language", "en-us")
	if tileKey == "" {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Accept-Charset", "utf-8")
		req.Header.Set("User-Agent", "locationd/2890.16.16 CFNetwork/1496.0.7 Darwin/23.5.0")
	} else {
		req.Header.Set("X-tilekey", tileKey)
		req.Header.Set("X-os-version", "17.5.21F79")
		req.Header.Set("User-Agent", "geod/1 CFNetwork/1496.0.7 Darwin/23.5.0")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, &transportError{err}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, &HTTPError{StatusCode: resp.StatusCode, RetryAfter: resp.Header.Get("Retry-After")}
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, c.maxBytes+1))
	if err != nil {
		return nil, &transportError{err}
	}
	if int64(len(data)) > c.maxBytes {
		return nil, ErrResponseTooLarge
	}
	return data, nil
}
