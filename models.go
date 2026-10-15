package main

// Protocol references and pinned source revisions are recorded in REFERENCES.md.
import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type traeClient struct {
	cfg   config
	http  *http.Client
	mu    sync.Mutex
	cache map[uint64]modelCache
}

type modelCache struct {
	models []string
	at     time.Time
	routes map[string]modelRoute
}

const modelCacheTTL = 5 * time.Minute
const modelCacheCapacity = 8

// Call with t.mu held; both model names and routes expire together.
func (t *traeClient) pruneModelCache(now time.Time) {
	for key, cached := range t.cache {
		if now.Sub(cached.at) >= modelCacheTTL {
			delete(t.cache, key)
		}
	}
}

func apiObjectValue(v any) map[string]any {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	if s, ok := v.(string); ok {
		var m map[string]any
		_ = json.Unmarshal([]byte(s), &m)
		return m
	}
	return nil
}

func apiFirstString(m map[string]any, keys ...string) string {
	for _, k := range keys {
		switch v := m[k].(type) {
		case string:
			if v != "" {
				return v
			}
		case float64:
			return strconv.FormatFloat(v, 'f', -1, 64)
		}
	}
	return ""
}

// The digest identifies a narrow access snapshot without keeping its token
// in cache keys or coupling it to Auth's mutable generation counter.
func snapshotIdentity(c apiCredential) uint64 {
	h := sha256.Sum256([]byte(jsonText(c)))
	return binary.BigEndian.Uint64(h[:8])
}

type modelRoute struct {
	function string
	name     string
	base     string
	most     int
	window   int
}

type modelEntry struct {
	function string
	config   modelConfig
}

type modelDetail struct {
	name      string
	maxTokens int
}

type modelConfig struct {
	name      string
	usage     string
	enabled   bool
	custom    bool
	details   []modelDetail
	devWindow int
	maxWindow int
}

var chatFunctions = []string{"chat_v3", "solo_work_lite", "solo_agent", "solo_agent_lite"}

func (t *traeClient) headers(c apiCredential) http.Header {
	h := http.Header{}
	values := map[string]string{
		"Content-Type":                 "application/json",
		"Accept":                       "text/event-stream",
		"Authorization":                "Cloud-IDE-JWT " + c.Token,
		"X-Cloudide-Token":             c.Token,
		"x-ide-token":                  c.Token,
		"x-uid":                        c.UserID,
		"x-app-id":                     "6eefa01c-1036-4c7e-9ca5-d891f63bfcd8",
		"x-device-id":                  c.DeviceID,
		"x-machine-id":                 c.MachineID,
		"x-request-id":                 newID(""),
		"x-ide-version":                "0.1.69",
		"x-ide-version-code":           "20260917",
		"x-ide-version-type":           "stable",
		"x-app-version":                "0.1.69",
		"x-app-version-code":           "20260917",
		"x-version-code":               "20260917",
		"x-device-brand":               "ASUS TUF Gaming A15 FA507RM_FA507RM",
		"x-device-type":                "windows",
		"x-device-platform":            "windows",
		"x-os-version":                 "Windows 10 Pro",
		"package-type":                 "stable_cn",
		"x-lscbd-aid":                  "787976",
		"x-lscbd-platform":             "windows",
		"x-ss-dp":                      "787976",
		"x-plugin-channel":             "icube-ai",
		"app-version":                  "0.1.69",
		"x-bridge-transport":           "aha",
		"x-ahanet-timeout":             "86400",
		"x-lgw-req-sdk-type":           "3",
		"x-net-sdk-domain-dispatch":    "1",
		"x-ttnet-bypass-decompression": "1",
		"x-ttnet-bypass-cookie":        "0",
		"request-traffic-type":         "prod",
		"User-Agent":                   "Trae/0.1.69",
	}
	for k, v := range values {
		h.Set(k, v)
	}
	return h
}

func (t *traeClient) modelNamesAttempt(ctx context.Context, c apiCredential, g uint64) ([]string, error) {
	t.mu.Lock()
	t.pruneModelCache(time.Now())
	if cached := t.cache[g]; len(cached.models) > 0 {
		names := append([]string(nil), cached.models...)
		t.mu.Unlock()
		return names, nil
	}
	t.mu.Unlock()
	lists := map[string][]modelConfig{}
	root, err := t.modelJSON(ctx, c, "/api/ide/v1/batch_get_detail_param", map[string]any{
		"functions":                 chatFunctions,
		"agent_type":                "",
		"current_config_info":       map[string]any{"config_name": "", "is_custom_model": false},
		"mode_type":                 0,
		"access_type":               0,
		"ab_force_vids":             "",
		"ab_autotest_advanced_mode": 0,
		"show_custom_model":         true,
	})
	if errors.Is(err, errCredentialRejected) {
		return nil, err
	}
	if err == nil {
		for _, group := range records(field(root, "function_configs")) {
			fn := apiFirstString(group, "function")
			if chatFunction(fn) {
				lists[fn] = append(lists[fn], modelConfigs(group["config_info_list"])...)
			}
		}
	}
	hasBatchModels := false
	for _, list := range lists {
		for _, m := range list {
			if m.available() {
				hasBatchModels = true
				break
			}
		}
	}
	if !hasBatchModels {
		var firstErr error
		for _, fn := range chatFunctions {
			one, e := t.modelJSON(ctx, c, "/api/ide/v1/get_detail_param", map[string]any{
				"function":            fn,
				"config_names":        nil,
				"need_prompt":         false,
				"current_config_info": nil,
				"poly_prompt":         true,
				"mode_type":           nil,
				"agent_type":          nil,
			})
			if errors.Is(e, errCredentialRejected) {
				return nil, e
			}
			if e != nil {
				if firstErr == nil {
					firstErr = e
				}
				continue
			}
			lists[fn] = modelConfigs(field(one, "config_info_list"))
		}
		if len(lists) == 0 && firstErr != nil {
			return nil, firstErr
		}
	}
	routes := map[string]modelRoute{}
	entries := map[string][]modelEntry{}
	for _, fn := range chatFunctions {
		for _, m := range lists[fn] {
			if m.available() {
				id := m.name
				entries[id] = append(entries[id], modelEntry{
					function: fn,
					config:   m,
				})
			}
		}
	}
	var names []string
	for id, group := range entries {
		chosen := group[0]
		for _, candidate := range group {
			if candidate.config.detail("__dev") != nil {
				chosen = candidate
				break
			}
		}
		route := modelRoute{
			function: chosen.function,
			base:     id,
		}
		if dev := chosen.config.detail("__dev"); dev != nil {
			route.name = dev.name
			route.most = dev.maxTokens
		}
		routes[id] = route
		names = append(names, id)
		if _, separateModel := entries[id+"-max"]; separateModel {
			continue
		}
		for _, entry := range group {
			if max := entry.config.detail("__max"); max != nil {
				window := 0
				devWindow := 0
				for _, source := range append([]modelEntry{entry}, group...) {
					if window == 0 {
						window = source.config.maxWindow
					}
					if devWindow == 0 {
						devWindow = source.config.devWindow
					}
				}
				if window > devWindow && window > 0 {
					routes[id+"-max"] = modelRoute{
						function: entry.function,
						base:     id,
						name:     max.name,
						most:     max.maxTokens,
						window:   window,
					}
					names = append(names, id+"-max")
					break
				}
			}
		}
	}
	sort.Strings(names)
	if len(names) == 0 {
		return nil, fmt.Errorf("Trae returned no available models")
	}
	t.mu.Lock()
	if t.cache == nil {
		t.cache = make(map[uint64]modelCache)
	}
	now := time.Now()
	t.pruneModelCache(now)
	if _, present := t.cache[g]; !present && len(t.cache) >= modelCacheCapacity {
		var oldest uint64
		var oldestAt time.Time
		for key, cached := range t.cache {
			if oldestAt.IsZero() || cached.at.Before(oldestAt) {
				oldest, oldestAt = key, cached.at
			}
		}
		delete(t.cache, oldest)
	}
	t.cache[g] = modelCache{
		models: append([]string(nil), names...),
		at:     now,
		routes: routes,
	}
	t.mu.Unlock()
	return names, nil
}

func field(m map[string]any, key string) any {
	if v, ok := m[key]; ok {
		return v
	}
	return apiObjectValue(m["data"])[key]
}

func records(v any) []map[string]any {
	xs, _ := v.([]any)
	out := make([]map[string]any, 0, len(xs))
	for _, x := range xs {
		if m := apiObjectValue(x); m != nil {
			out = append(out, m)
		}
	}
	return out
}

func positiveInt(v any) int {
	n, _ := v.(float64)
	if n > 0 && n < 1<<30 {
		return int(n)
	}
	return 0
}

func chatFunction(fn string) bool {
	for _, allowed := range chatFunctions {
		if fn == allowed {
			return true
		}
	}
	return false
}

func modelConfigs(v any) []modelConfig {
	var configs []modelConfig
	for _, raw := range records(v) {
		config := modelConfig{
			name:    apiFirstString(raw, "config_name"),
			usage:   apiFirstString(raw, "usage"),
			enabled: raw["config_switch"] != false,
			custom:  raw["is_custom_model"] == true || apiObjectValue(raw["display_config"])["is_custom_model"] == true,
		}
		window := apiObjectValue(raw["context_window_tokens"])
		config.devWindow = positiveInt(window["dev"])
		config.maxWindow = positiveInt(window["max"])
		for _, detail := range records(raw["model_detail_list"]) {
			config.details = append(config.details, modelDetail{
				name:      apiFirstString(detail, "model_name"),
				maxTokens: positiveInt(detail["max_tokens"]),
			})
		}
		configs = append(configs, config)
	}
	return configs
}

func (m modelConfig) available() bool {
	return m.name != "" &&
		!strings.HasPrefix(strings.ToLower(m.name), "custom_model") &&
		m.enabled && !m.custom &&
		(m.usage == "" || m.usage == "chat_completion")
}

func (m modelConfig) detail(suffix string) *modelDetail {
	for i := range m.details {
		if strings.HasSuffix(m.details[i].name, suffix) {
			return &m.details[i]
		}
	}
	return nil
}

func (t *traeClient) modelJSON(ctx context.Context, c apiCredential, path string, body any) (map[string]any, error) {
	req, e := http.NewRequestWithContext(ctx, "POST", strings.TrimRight(t.cfg.BaseURL, "/")+path, bytes.NewBufferString(jsonText(body)))
	if e != nil {
		return nil, e
	}
	req.Header = t.headers(c)
	req.Header.Set("Accept", "application/json")
	resp, e := t.http.Do(req)
	if e != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("Trae model list connection failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		return nil, errCredentialRejected
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("Trae model list returned HTTP %d", resp.StatusCode)
	}
	var root map[string]any
	if e = json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&root); e != nil {
		return nil, fmt.Errorf("invalid Trae model list response")
	}
	if code := apiFirstString(root, "code"); code != "" && code != "0" {
		if code == "1001" || code == "401" || code == "403" {
			return nil, errCredentialRejected
		}
		return nil, fmt.Errorf("Trae model list business error")
	}
	return root, nil
}

func (t *traeClient) modelNames(ctx context.Context, c apiCredential, g uint64) ([]string, error) {
	names, e := t.modelNamesAttempt(ctx, c, g)
	if errors.Is(e, errCredentialRejected) {
		return nil, errLoginRequired
	}
	return names, e
}
