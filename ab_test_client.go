package neatlogs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

var errABTransient = errors.New("neatlogs: A/B request temporarily unavailable")

// ABTestConfig connects the A/B control API. Keys must be explicitly registered.
type ABTestConfig struct {
	Endpoint   string
	APIKey     string
	Keys       []string
	HTTPClient *http.Client
}

// ABAssignment is a cached choice. Unavailable settings always select control.
type ABAssignment struct {
	Key               string
	Arm               string
	Params            ABParams
	Reason            string
	Revision          *int
	AllocationVersion *int
}

// ABDecision is a cached choice that has not yet been reported as an agent run.
type ABDecision struct {
	ABAssignment
	UnitID string
}
type ABAssignmentOptions struct {
	Unit     string
	Fallback ABParams
}

// ABTestClient caches settings and queues exposure receipts independently of telemetry.
// Flush telemetry before calling Flush on this client.
type ABTestClient struct {
	mu                     sync.Mutex
	refreshMu              sync.Mutex
	flushMu                sync.Mutex
	endpoint, apiKey       string
	apiPath                string
	onAuthorizationFailure func(string)
	keys                   []string
	http                   *http.Client
	configs                map[string]abConfiguration
	queue                  []abPendingExposure
	seen                   map[string]abSeenExposure
	seenOrder              []string
	rejected               int
	closed, started        bool
	ctx                    context.Context
	cancel                 context.CancelFunc
}

func NewABTestClient(cfg ABTestConfig) (*ABTestClient, error) {
	base, err := url.Parse(cfg.Endpoint)
	if err != nil || base == nil || base.Host == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" || (base.Path != "" && base.Path != "/") || (base.Scheme != "https" && base.Scheme != "http") {
		return nil, errors.New("neatlogs: A/B endpoint must be an HTTPS origin or local HTTP origin")
	}
	if base.Scheme == "http" && !strings.EqualFold(base.Hostname(), "localhost") {
		ip := net.ParseIP(base.Hostname())
		if ip == nil || !ip.IsLoopback() {
			return nil, errors.New("neatlogs: A/B endpoint requires HTTPS except on loopback")
		}
	}
	if strings.TrimSpace(cfg.APIKey) == "" || strings.ContainsAny(cfg.APIKey, "\r\n") || len(cfg.Keys) > 20 {
		return nil, errors.New("neatlogs: A/B credentials or registered keys are invalid")
	}
	keys, known := []string{}, map[string]bool{}
	for _, key := range cfg.Keys {
		if !abKeyPattern.MatchString(key) {
			return nil, errors.New("neatlogs: invalid A/B key")
		}
		if !known[key] {
			keys = append(keys, key)
			known[key] = true
		}
	}
	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{}
	}
	isolated := *httpClient
	isolated.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	base.Path = ""
	ctx, cancel := context.WithCancel(context.Background())
	return &ABTestClient{endpoint: strings.TrimRight(base.String(), "/"), apiPath: "/api/v1/ab-tests", apiKey: strings.TrimSpace(cfg.APIKey), keys: keys, http: &isolated, configs: map[string]abConfiguration{}, seen: map[string]abSeenExposure{}, ctx: ctx, cancel: cancel}, nil
}

// Start fetches settings, then refreshes them and retries queued receipts every minute.
func (c *ABTestClient) Start(ctx context.Context) error {
	err := c.Refresh(ctx)
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.started && !c.closed {
		c.started = true
		go c.poll()
	}
	return err
}
func (c *ABTestClient) poll() {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-c.ctx.Done():
			return
		case <-ticker.C:
			_ = c.Refresh(c.ctx)
			_ = c.Flush(c.ctx)
		}
	}
}
func (c *ABTestClient) Refresh(ctx context.Context) error {
	c.refreshMu.Lock()
	defer c.refreshMu.Unlock()
	var result error
	for _, key := range c.keys {
		c.mu.Lock()
		closed := c.closed
		previous, exists := c.configs[key]
		c.mu.Unlock()
		if closed {
			return nil
		}
		headers := http.Header{}
		if exists {
			headers.Set("If-None-Match", previous.ETag)
		}
		status, responseHeaders, data, err := c.request(ctx, key, "config", nil, headers)
		transient := errors.Is(err, errABTransient) || status == 408 || status == 429 || status >= 500
		var config abConfiguration
		if err == nil {
			if status == http.StatusNotModified && exists {
				config = previous
				config.ExpiresAt = responseHeaders.Get("X-AB-Config-Expires-At")
			} else if status == http.StatusOK {
				err = json.Unmarshal(data, &config)
			} else {
				err = errABConfiguration
			}
		}
		var old *abConfiguration
		if exists {
			old = &previous
		}
		if err == nil {
			err = config.validate(key, old, time.Now())
		}
		c.mu.Lock()
		if err != nil {
			if !transient || !exists || !previous.expires.After(time.Now()) {
				delete(c.configs, key)
			}
			result = errABConfiguration
		} else if !c.closed {
			c.configs[key] = config
		}
		c.mu.Unlock()
	}
	return result
}

// Assign never performs network I/O. Call inside the trace that uses the choice.
func (c *ABTestClient) Assign(ctx context.Context, key string, opts ABAssignmentOptions) ABAssignment {
	decision := c.Choose(key, opts)
	if decision.Reason == "assigned" {
		c.RecordRun(ctx, decision)
	}
	return decision.ABAssignment
}

// Choose reads cached settings without reporting an exposure.
func (c *ABTestClient) Choose(key string, opts ABAssignmentOptions) ABDecision {
	c.mu.Lock()
	config, exists := c.configs[key]
	closed := c.closed
	c.mu.Unlock()
	now := time.Now()
	if closed || !exists || !config.expires.After(now) || !utf8.ValidString(opts.Unit) || abLength(opts.Unit) < 1 || abLength(opts.Unit) > 1024 {
		return ABDecision{ABAssignment: ABAssignment{Key: key, Arm: "a", Params: abCopyParams(opts.Fallback), Reason: "unavailable"}, UnitID: opts.Unit}
	}
	enabled := *config.Enabled && config.ends.After(now)
	index := 0
	if enabled {
		if abBucket(config.Salt, opts.Unit) >= uint32(*config.Arms[0].Weight) {
			index = 1
		}
	} else if config.DefaultArm == "b" {
		index = 1
	}
	arm, reason := config.Arms[index], "default"
	if enabled {
		reason = "assigned"
	}
	revision, allocationVersion := config.Revision, config.AllocationVersion
	return ABDecision{ABAssignment: ABAssignment{Key: key, Arm: arm.Key, Params: abCopyParams(arm.Params), Reason: reason, Revision: &revision, AllocationVersion: &allocationVersion}, UnitID: opts.Unit}
}
func (c *ABTestClient) Close() {
	c.mu.Lock()
	c.closed = true
	c.configs = map[string]abConfiguration{}
	c.mu.Unlock()
	c.cancel()
}
func (c *ABTestClient) request(ctx context.Context, key, operation string, payload []byte, headers http.Header) (int, http.Header, []byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	stop := context.AfterFunc(c.ctx, cancel)
	defer stop()
	method := http.MethodGet
	if payload != nil {
		method = http.MethodPost
	}
	req, err := http.NewRequestWithContext(ctx, method, c.endpoint+c.apiPath+"/"+url.PathEscape(key)+"/"+operation, bytes.NewReader(payload))
	if err != nil {
		return 0, nil, nil, errABConfiguration
	}
	req.Header = headers.Clone()
	if req.Header == nil {
		req.Header = http.Header{}
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Content-Type", "application/json")
	response, err := c.http.Do(req)
	if err != nil {
		return 0, nil, nil, errABTransient
	}
	defer response.Body.Close()
	if (response.StatusCode == 401 || response.StatusCode == 403) && c.onAuthorizationFailure != nil {
		c.onAuthorizationFailure(key)
	}
	if response.StatusCode == http.StatusNotModified || operation == "config" && response.StatusCode != http.StatusOK {
		return response.StatusCode, response.Header, nil, nil
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, 64*1024+1))
	if err != nil {
		return 0, nil, nil, errABTransient
	}
	if len(data) > 64*1024 {
		return 0, nil, nil, errABConfiguration
	}
	return response.StatusCode, response.Header, data, nil
}
