package openai

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	. "github.com/router-for-me/CLIProxyAPI/v6/internal/constant"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/api/handlers"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	defaultImageGenerationSize = "1024x1024"
	imageGenerationRouteModel  = "gpt-draw-1024x1024"
	openAIImagesEndpoint       = "/v1/images/generations"
)

var imageGenerationSizePattern = regexp.MustCompile(`^(\d+)x(\d+)$`)

func isOpenAIImageGenerationModel(modelName string) bool {
	switch strings.ToLower(strings.TrimSpace(modelName)) {
	case "gpt-image-1", "gpt-image-2":
		return true
	default:
		return false
	}
}

func writeImageGenerationEndpointError(c *gin.Context, modelName string) {
	c.JSON(http.StatusBadRequest, handlers.ErrorResponse{
		Error: handlers.ErrorDetail{
			Message: fmt.Sprintf("model %s is an image generation model; use %s", modelName, openAIImagesEndpoint),
			Type:    "invalid_request_error",
			Code:    "endpoint_not_supported",
		},
	})
}

// ImagesGenerations handles the OpenAI-compatible /v1/images/generations endpoint
// by routing the request through the existing Codex Responses image tool path.
func (h *OpenAIAPIHandler) ImagesGenerations(c *gin.Context) {
	rawJSON, err := c.GetRawData()
	if err != nil {
		c.JSON(http.StatusBadRequest, handlers.ErrorResponse{
			Error: handlers.ErrorDetail{
				Message: fmt.Sprintf("Invalid request: %v", err),
				Type:    "invalid_request_error",
			},
		})
		return
	}

	responsesJSON, routeModelName, requestedModelName, err := convertImagesGenerationRequestToResponses(rawJSON)
	if err != nil {
		c.JSON(http.StatusBadRequest, handlers.ErrorResponse{
			Error: handlers.ErrorDetail{
				Message: err.Error(),
				Type:    "invalid_request_error",
			},
		})
		return
	}

	c.Header("Content-Type", "application/json")
	cliCtx, cliCancel := h.GetContextWithCancel(h, c, context.Background())
	stopKeepAlive := h.StartNonStreamingKeepAlive(c, cliCtx)
	resp, upstreamHeaders, errMsg := h.ExecuteWithAuthManagerRequestedModel(cliCtx, OpenaiResponse, routeModelName, responsesJSON, "", requestedModelName)
	stopKeepAlive()
	if errMsg != nil {
		h.WriteErrorResponse(c, errMsg)
		cliCancel(errMsg.Error)
		return
	}

	imageResp, err := convertResponsesImageToOpenAIImage(resp)
	if err != nil {
		c.JSON(http.StatusBadGateway, handlers.ErrorResponse{
			Error: handlers.ErrorDetail{
				Message: err.Error(),
				Type:    "server_error",
			},
		})
		cliCancel(err)
		return
	}

	handlers.WriteUpstreamHeaders(c.Writer.Header(), upstreamHeaders)
	_, _ = c.Writer.Write(imageResp)
	cliCancel(imageResp)
}

func convertImagesGenerationRequestToResponses(rawJSON []byte) ([]byte, string, string, error) {
	root := gjson.ParseBytes(rawJSON)
	if !root.IsObject() {
		return nil, "", "", fmt.Errorf("request body must be a JSON object")
	}

	prompt := strings.TrimSpace(root.Get("prompt").String())
	if prompt == "" {
		return nil, "", "", fmt.Errorf("prompt is required")
	}

	size := normalizeImageGenerationSize(root.Get("size").String())
	routeModelName := imageGenerationRouteModel
	requestedModelName := "gpt-draw-" + size

	out := []byte(`{"model":"","input":"","stream":false}`)
	out, _ = sjson.SetBytes(out, "model", requestedModelName)
	out, _ = sjson.SetBytes(out, "input", prompt)

	return out, routeModelName, requestedModelName, nil
}

func normalizeImageGenerationSize(size string) string {
	size = strings.ToLower(strings.TrimSpace(size))
	size = strings.ReplaceAll(size, " ", "")
	matches := imageGenerationSizePattern.FindStringSubmatch(size)
	if matches == nil {
		return defaultImageGenerationSize
	}

	width, widthErr := strconv.Atoi(matches[1])
	height, heightErr := strconv.Atoi(matches[2])
	if widthErr != nil || heightErr != nil || width <= 0 || height <= 0 {
		return defaultImageGenerationSize
	}
	switch {
	case width > height:
		return "1536x1024"
	case height > width:
		return "1024x1536"
	default:
		return defaultImageGenerationSize
	}
}

func convertResponsesImageToOpenAIImage(rawJSON []byte) ([]byte, error) {
	root := gjson.ParseBytes(rawJSON)
	if !root.IsObject() {
		return nil, fmt.Errorf("upstream image response is not a JSON object")
	}

	output := root.Get("output")
	if !output.IsArray() {
		return nil, fmt.Errorf("upstream image response missing output")
	}

	out := []byte(`{"created":0,"data":[]}`)
	if created := root.Get("created_at"); created.Exists() {
		out, _ = sjson.SetBytes(out, "created", created.Int())
	} else {
		out, _ = sjson.SetBytes(out, "created", time.Now().Unix())
	}

	for _, item := range output.Array() {
		if item.Get("type").String() != "image_generation_call" {
			continue
		}
		result := item.Get("result").String()
		if strings.TrimSpace(result) == "" {
			continue
		}
		imageData := []byte(`{"b64_json":""}`)
		imageData, _ = sjson.SetBytes(imageData, "b64_json", result)
		out, _ = sjson.SetRawBytes(out, "data.-1", imageData)
	}

	if len(gjson.GetBytes(out, "data").Array()) == 0 {
		return nil, fmt.Errorf("upstream image response contained no image data")
	}

	if usage := root.Get("usage"); usage.Exists() && usage.IsObject() {
		out, _ = sjson.SetRawBytes(out, "usage", []byte(usage.Raw))
		out = normalizeResponsesUsageForOpenAIImages(out)
	}

	return out, nil
}

func normalizeResponsesUsageForOpenAIImages(rawJSON []byte) []byte {
	usage := gjson.GetBytes(rawJSON, "usage")
	if !usage.IsObject() {
		return rawJSON
	}

	out := bytes.Clone(rawJSON)
	inputTokens := usage.Get("input_tokens")
	outputTokens := usage.Get("output_tokens")
	totalTokens := usage.Get("total_tokens")
	if totalTokens.Exists() {
		out, _ = sjson.SetBytes(out, "usage.total_tokens", totalTokens.Int())
	} else if inputTokens.Exists() || outputTokens.Exists() {
		out, _ = sjson.SetBytes(out, "usage.total_tokens", inputTokens.Int()+outputTokens.Int())
	}
	if cachedTokens := usage.Get("input_tokens_details.cached_tokens"); cachedTokens.Exists() {
		out, _ = sjson.SetBytes(out, "usage.prompt_tokens_details.cached_tokens", cachedTokens.Int())
	}
	if reasoningTokens := usage.Get("output_tokens_details.reasoning_tokens"); reasoningTokens.Exists() {
		out, _ = sjson.SetBytes(out, "usage.completion_tokens_details.reasoning_tokens", reasoningTokens.Int())
	}
	return out
}
