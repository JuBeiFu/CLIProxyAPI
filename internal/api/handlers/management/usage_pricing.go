package management

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
)

const newAPIPricingTimeout = 20 * time.Second

type usageModelPricesResponse struct {
	NewAPIPricingURL      string                             `json:"new_api_pricing_url,omitempty"`
	NewAPIPricingTokenSet bool                               `json:"new_api_pricing_token_set"`
	LastSyncedAt          string                             `json:"last_synced_at,omitempty"`
	LastSyncSource        string                             `json:"last_sync_source,omitempty"`
	LastSyncModelCount    int                                `json:"last_sync_model_count"`
	LastSyncRequestModels int                                `json:"last_sync_request_models"`
	ModelPrices           map[string]config.UsageModelPrice  `json:"model_prices"`
}

type usageModelPricesPutRequest struct {
	NewAPIPricingURL   *string                            `json:"new_api_pricing_url"`
	NewAPIPricingToken *string                            `json:"new_api_pricing_token"`
	ModelPrices        map[string]config.UsageModelPrice  `json:"model_prices"`
}

type newAPIPricingEnvelope struct {
	Success bool                   `json:"success"`
	Data    []newAPIPricingRecord  `json:"data"`
}

type newAPIPricingRecord struct {
	ModelName        string   `json:"model_name"`
	QuotaType        int      `json:"quota_type"`
	ModelRatio       float64  `json:"model_ratio"`
	ModelPrice       float64  `json:"model_price"`
	CompletionRatio  float64  `json:"completion_ratio"`
	CacheRatio       *float64 `json:"cache_ratio,omitempty"`
	CreateCacheRatio *float64 `json:"create_cache_ratio,omitempty"`
}

func (h *Handler) GetUsageModelPrices(c *gin.Context) {
	if h == nil || h.cfg == nil {
		c.JSON(http.StatusOK, usageModelPricesResponse{ModelPrices: map[string]config.UsageModelPrice{}})
		return
	}
	c.JSON(http.StatusOK, h.buildUsageModelPricesResponse())
}

func (h *Handler) PutUsageModelPrices(c *gin.Context) {
	if h == nil || h.cfg == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "config unavailable"})
		return
	}

	var body usageModelPricesPutRequest
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}

	if body.NewAPIPricingURL != nil {
		h.cfg.UsagePricing.NewAPIPricingURL = strings.TrimSpace(*body.NewAPIPricingURL)
	}
	if body.NewAPIPricingToken != nil {
		h.cfg.UsagePricing.NewAPIPricingToken = strings.TrimSpace(*body.NewAPIPricingToken)
	}

	if body.ModelPrices != nil {
		h.cfg.UsagePricing.ModelPrices = cloneUsageModelPrices(body.ModelPrices)
	}
	h.cfg.SanitizeUsagePricing()

	if !h.persistWithoutResponse() {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to save config"})
		return
	}

	c.JSON(http.StatusOK, h.buildUsageModelPricesResponse())
}

func (h *Handler) SyncUsageModelPrices(c *gin.Context) {
	if h == nil || h.cfg == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "config unavailable"})
		return
	}

	type syncRequest struct {
		URL   string `json:"new_api_pricing_url"`
		Token string `json:"new_api_pricing_token"`
	}

	var body syncRequest
	if err := c.ShouldBindJSON(&body); err != nil && err != io.EOF {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}

	pricingURL := strings.TrimSpace(body.URL)
	if pricingURL == "" {
		pricingURL = strings.TrimSpace(h.cfg.UsagePricing.NewAPIPricingURL)
	}
	if pricingURL == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "new_api_pricing_url is required"})
		return
	}

	token := strings.TrimSpace(body.Token)
	if token == "" {
		token = strings.TrimSpace(h.cfg.UsagePricing.NewAPIPricingToken)
	}

	modelPrices, tokenPricedCount, requestPricedCount, err := fetchNewAPIModelPrices(c.Request.Context(), pricingURL, token)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "sync_failed", "message": err.Error()})
		return
	}

	merged := cloneUsageModelPrices(h.cfg.UsagePricing.ModelPrices)
	for model, price := range modelPrices {
		merged[model] = price
	}

	h.cfg.UsagePricing.NewAPIPricingURL = pricingURL
	h.cfg.UsagePricing.NewAPIPricingToken = token
	h.cfg.UsagePricing.ModelPrices = merged
	h.cfg.UsagePricing.LastSyncedAt = time.Now().UTC().Format(time.RFC3339)
	h.cfg.UsagePricing.LastSyncSource = pricingURL
	h.cfg.UsagePricing.LastSyncModelCount = tokenPricedCount
	h.cfg.UsagePricing.LastSyncRequestModels = requestPricedCount
	h.cfg.SanitizeUsagePricing()

	if !h.persistWithoutResponse() {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to save config"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"status":                   "ok",
		"synced_models":            tokenPricedCount,
		"request_priced_models":    requestPricedCount,
		"usage_pricing":            h.buildUsageModelPricesResponse(),
	})
}

func (h *Handler) buildUsageModelPricesResponse() usageModelPricesResponse {
	pricing := config.UsagePricingConfig{}
	if h != nil && h.cfg != nil {
		pricing = h.cfg.UsagePricing
	}
	modelPrices := cloneUsageModelPrices(pricing.ModelPrices)
	if modelPrices == nil {
		modelPrices = map[string]config.UsageModelPrice{}
	}
	return usageModelPricesResponse{
		NewAPIPricingURL:      pricing.NewAPIPricingURL,
		NewAPIPricingTokenSet: strings.TrimSpace(pricing.NewAPIPricingToken) != "",
		LastSyncedAt:          pricing.LastSyncedAt,
		LastSyncSource:        pricing.LastSyncSource,
		LastSyncModelCount:    pricing.LastSyncModelCount,
		LastSyncRequestModels: pricing.LastSyncRequestModels,
		ModelPrices:           modelPrices,
	}
}

func (h *Handler) persistWithoutResponse() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if err := config.SaveConfigPreserveComments(h.configFilePath, h.cfg); err != nil {
		return false
	}
	return true
}

func cloneUsageModelPrices(in map[string]config.UsageModelPrice) map[string]config.UsageModelPrice {
	if len(in) == 0 {
		return map[string]config.UsageModelPrice{}
	}
	out := make(map[string]config.UsageModelPrice, len(in))
	for model, price := range in {
		name := strings.TrimSpace(model)
		if name == "" {
			continue
		}
		out[name] = price
	}
	return out
}

func fetchNewAPIModelPrices(ctx context.Context, pricingURL, token string) (map[string]config.UsageModelPrice, int, int, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, pricingURL, nil)
	if err != nil {
		return nil, 0, 0, err
	}
	req.Header.Set("Accept", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	client := &http.Client{Timeout: newAPIPricingTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, 0, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return nil, 0, 0, fmt.Errorf("unexpected status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var payload newAPIPricingEnvelope
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, 0, 0, err
	}

	modelPrices := make(map[string]config.UsageModelPrice)
	requestPricedCount := 0
	for _, record := range payload.Data {
		model := strings.TrimSpace(record.ModelName)
		if model == "" {
			continue
		}
		if record.QuotaType == 1 {
			requestPricedCount++
			continue
		}
		prompt := normalizePrice(record.ModelRatio * 2)
		completionRatio := record.CompletionRatio
		if completionRatio <= 0 {
			completionRatio = 1
		}
		cacheRatio := 1.0
		if record.CacheRatio != nil && *record.CacheRatio >= 0 {
			cacheRatio = *record.CacheRatio
		}
		modelPrices[model] = config.UsageModelPrice{
			Prompt:     prompt,
			Completion: normalizePrice(record.ModelRatio * completionRatio * 2),
			Cache:      normalizePrice(record.ModelRatio * cacheRatio * 2),
		}
	}
	return modelPrices, len(modelPrices), requestPricedCount, nil
}

func normalizePrice(value float64) float64 {
	if value < 0 {
		return 0
	}
	return value
}
