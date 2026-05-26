package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/higress-group/proxy-wasm-go-sdk/proxywasm"
	"github.com/higress-group/proxy-wasm-go-sdk/proxywasm/types"
	"github.com/higress-group/wasm-go/pkg/wrapper"
	"github.com/tidwall/gjson"
	"github.com/tidwall/resp"
)

const (
	pluginName         = "llm-tier-router"
	userAPIKeyHeader   = "X-User-API-Key"
	redisKeyCtx        = "redis_key"
	tierCtx            = "selected_tier"
	redisTTLSeconds    = 86400
	redisCallTimeout   = 1000
	requestBodyMaxSize = 8 * 1024 * 1024
)

func main() {}

func init() {
	wrapper.SetCtx(
		pluginName,
		wrapper.ParseConfig(parseConfig),
		wrapper.ProcessRequestHeaders(onHttpRequestHeaders),
		wrapper.ProcessRequestBody(onHttpRequestBody),
		wrapper.ProcessResponseBody(onHttpResponseBody),
	)
}

type Config struct {
	RedisService string              `json:"redis_service"`
	RedisPort    int                 `json:"redis_port"`
	RedisPass    string              `json:"redis_pass"`
	Tiers        []Tier              `json:"tiers"`
	RedisClient  wrapper.RedisClient `json:"-"`
}

type Tier struct {
	MaxToken       int    `json:"max_token"`
	TargetProvider string `json:"target_provider"`
	TargetModel    string `json:"target_model"`
}

func parseConfig(raw gjson.Result, config *Config) error {
	if err := json.Unmarshal([]byte(raw.Raw), config); err != nil {
		return fmt.Errorf("parse config failed: %w", err)
	}

	if config.RedisService == "" {
		return errors.New("redis_service is required")
	}
	if config.RedisPort <= 0 {
		config.RedisPort = 6379
	}
	if len(config.Tiers) == 0 {
		return errors.New("tiers is required")
	}

	for i, tier := range config.Tiers {
		if tier.MaxToken <= 0 {
			return fmt.Errorf("tiers[%d].max_token must be greater than 0", i)
		}
		if strings.TrimSpace(tier.TargetProvider) == "" {
			return fmt.Errorf("tiers[%d].target_provider is required", i)
		}
		if strings.TrimSpace(tier.TargetModel) == "" {
			return fmt.Errorf("tiers[%d].target_model is required", i)
		}
	}

	sort.Slice(config.Tiers, func(i, j int) bool {
		return config.Tiers[i].MaxToken < config.Tiers[j].MaxToken
	})

	client := newRedisClient(*config)
	if err := client.Init("", config.RedisPass, redisCallTimeout); err != nil {
		return fmt.Errorf("init redis failed: %w", err)
	}
	config.RedisClient = client
	return nil
}

func onHttpRequestHeaders(ctx wrapper.HttpContext, config Config) types.Action {
	ctx.SetRequestBodyBufferLimit(requestBodyMaxSize)
	ctx.BufferRequestBody()
	_ = proxywasm.RemoveHttpRequestHeader("content-length")

	// 2. 用户身份来自 New API 透传的 X-User-API-Key，真实 LLM 密钥不会暴露给调用方。
	userKey, _ := proxywasm.GetHttpRequestHeader(userAPIKeyHeader)
	userKey = strings.TrimSpace(userKey)
	if userKey == "" {
		sendJSON(401, `{"error":"缺失 X-User-API-Key 参数"}`)
		return types.HeaderStopAllIterationAndWatermark
	}

	// 3. 按用户 Key 和日期生成 Redis 日累计 Key；日期进入 Key，TTL 负责自动清理。
	redisKey := dailyRedisKey(userKey)
	ctx.SetContext(redisKeyCtx, redisKey)

	client := config.RedisClient
	if err := client.Get(redisKey, func(value resp.Value) {
		if value.Error() != nil {
			proxywasm.LogErrorf("redis GET failed, key=%s, err=%v", redisKey, value.Error())
			sendJSON(503, `{"error":"redis 服务不可用，请稍后重试"}`)
			return
		}

		usedTokens := 0
		if !value.IsNull() {
			usedTokens = value.Integer()
		}

		// 4. 根据当日累计 Token 动态选择阶梯。方案 B 中插件只负责选择 tier，
		// 真实供应商与 Key 池交给 Higress ai-proxy / AI 路由能力处理。
		tier := selectTier(config.Tiers, usedTokens)
		ctx.SetContext(tierCtx, tier)
		applyTierMetadata(tier, usedTokens)

		if err := proxywasm.ResumeHttpRequest(); err != nil {
			proxywasm.LogErrorf("resume request failed: %v", err)
		}
	}); err != nil {
		proxywasm.LogErrorf("dispatch redis GET failed, key=%s, err=%v", redisKey, err)
		sendJSON(503, `{"error":"redis 服务不可用，请稍后重试"}`)
		return types.HeaderStopAllIterationAndBuffer
	}

	return types.HeaderStopAllIterationAndBuffer
}

func onHttpRequestBody(ctx wrapper.HttpContext, config Config, body []byte) types.Action {
	tier, ok := ctx.GetContext(tierCtx).(Tier)
	if !ok || tier.TargetModel == "" || len(body) == 0 {
		return types.DataContinue
	}

	rewrittenBody, err := replaceRequestModel(body, tier.TargetModel)
	if err != nil {
		proxywasm.LogErrorf("replace request model failed: %v", err)
		return types.DataContinue
	}
	if err := proxywasm.ReplaceHttpRequestBody(rewrittenBody); err != nil {
		proxywasm.LogErrorf("replace request body failed: %v", err)
		return types.DataContinue
	}
	return types.DataContinue
}

func onHttpResponseBody(ctx wrapper.HttpContext, config Config, body []byte) types.Action {
	addTokens := totalTokensFromOpenAIResponse(body)
	if addTokens <= 0 {
		return types.ActionContinue
	}

	redisKey := ctx.GetStringContext(redisKeyCtx, "")
	if redisKey == "" {
		return types.ActionContinue
	}

	// 5. Lua 保证 INCRBY 与 EXPIRE 在 Redis 侧原子执行，适合高并发下累加 Token。
	lua := `
local current = redis.call("INCRBY", KEYS[1], ARGV[1])
redis.call("EXPIRE", KEYS[1], ARGV[2])
return current
`

	client := config.RedisClient
	err := client.Eval(
		lua,
		1,
		[]interface{}{redisKey},
		[]interface{}{addTokens, redisTTLSeconds},
		func(value resp.Value) {
			if value.Error() != nil {
				proxywasm.LogErrorf("redis EVAL failed, key=%s, tokens=%d, err=%v", redisKey, addTokens, value.Error())
			}
		},
	)
	if err != nil {
		proxywasm.LogErrorf("dispatch redis EVAL failed, key=%s, tokens=%d, err=%v", redisKey, addTokens, err)
	}

	return types.ActionContinue
}

func newRedisClient(config Config) wrapper.RedisClient {
	clusterName := fmt.Sprintf("outbound|%d||%s", config.RedisPort, config.RedisService)
	return wrapper.NewRedisClusterClient(namedCluster{
		clusterName: clusterName,
		hostName:    config.RedisService,
	})
}

func dailyRedisKey(userKey string) string {
	return fmt.Sprintf("higress:llm:token:%s:%s", time.Now().Format("20060102"), userKey)
}

func selectTier(tiers []Tier, usedTokens int) Tier {
	for _, tier := range tiers {
		if usedTokens < tier.MaxToken {
			return tier
		}
	}
	return tiers[len(tiers)-1]
}

func applyTierMetadata(tier Tier, usedTokens int) {
	headers := map[string]string{
		"X-Higress-Tier-Limit": strconv.Itoa(tier.MaxToken),
		"X-Higress-Used-Token": strconv.Itoa(usedTokens),
	}

	if tier.TargetProvider != "" {
		headers["X-Tier-Provider"] = tier.TargetProvider
	}
	if tier.TargetModel != "" {
		headers["X-Higress-Target-Model"] = tier.TargetModel
	}

	for key, value := range headers {
		if err := proxywasm.ReplaceHttpRequestHeader(key, value); err != nil {
			proxywasm.LogErrorf("replace request header %s failed: %v", key, err)
		}
	}
}

func totalTokensFromOpenAIResponse(body []byte) int {
	var response struct {
		Usage struct {
			TotalTokens int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		return 0
	}
	return response.Usage.TotalTokens
}

func replaceRequestModel(body []byte, model string) ([]byte, error) {
	var request map[string]interface{}
	if err := json.Unmarshal(body, &request); err != nil {
		return nil, err
	}
	request["model"] = model
	return json.Marshal(request)
}

func sendJSON(statusCode uint32, body string) {
	_ = proxywasm.SendHttpResponse(
		statusCode,
		[][2]string{{"content-type", "application/json; charset=utf-8"}},
		[]byte(body),
		-1,
	)
}

type namedCluster struct {
	clusterName string
	hostName    string
}

func (c namedCluster) ClusterName() string {
	return c.clusterName
}

func (c namedCluster) HostName() string {
	return c.hostName
}
