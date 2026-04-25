package openai

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"math"
	"mime/multipart"
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
	defaultImageGenerationSize = "auto"
	defaultImagesMainModel     = "gpt-5.4-mini"
	defaultImagesToolModel     = "gpt-image-2"
	openAIImagesEndpoint       = "/v1/images/generations"
	imageGenerationTimeout     = 10 * time.Minute
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
	h.handleImagesRequest(c, convertImagesGenerationRequestToResponses)
}

// ImagesEdits handles the OpenAI-compatible /v1/images/edits endpoint by
// translating JSON image references into the same Codex image tool path.
func (h *OpenAIAPIHandler) ImagesEdits(c *gin.Context) {
	if strings.Contains(strings.ToLower(c.GetHeader("Content-Type")), "multipart/form-data") {
		h.handleImagesRequestWithConvertedPayload(c, convertImagesEditMultipartRequestToResponses)
		return
	}
	h.handleImagesRequest(c, convertImagesEditRequestToResponses)
}

func (h *OpenAIAPIHandler) handleImagesRequest(c *gin.Context, convert func([]byte) ([]byte, string, string, error)) {
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

	h.executeConvertedImagesRequest(c, rawJSON, convert)
}

func (h *OpenAIAPIHandler) handleImagesRequestWithConvertedPayload(c *gin.Context, convert func(*gin.Context) ([]byte, string, string, error)) {
	responsesJSON, routeModelName, requestedModelName, err := convert(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, handlers.ErrorResponse{
			Error: handlers.ErrorDetail{
				Message: err.Error(),
				Type:    "invalid_request_error",
			},
		})
		return
	}
	h.executeImagesResponsesPayload(c, responsesJSON, routeModelName, requestedModelName)
}

func (h *OpenAIAPIHandler) executeConvertedImagesRequest(c *gin.Context, rawJSON []byte, convert func([]byte) ([]byte, string, string, error)) {
	responsesJSON, routeModelName, requestedModelName, err := convert(rawJSON)
	if err != nil {
		c.JSON(http.StatusBadRequest, handlers.ErrorResponse{
			Error: handlers.ErrorDetail{
				Message: err.Error(),
				Type:    "invalid_request_error",
			},
		})
		return
	}
	h.executeImagesResponsesPayload(c, responsesJSON, routeModelName, requestedModelName)
}

func (h *OpenAIAPIHandler) executeImagesResponsesPayload(c *gin.Context, responsesJSON []byte, routeModelName, requestedModelName string) {
	c.Header("Content-Type", "application/json")
	cliCtx, cliCancel := h.GetContextWithCancel(h, c, context.Background())
	cliCtx = handlers.WithImageGenerationRequest(cliCtx)
	cliCtx = handlers.WithImageGenerationModel(cliCtx, imageToolModelFromResponsesPayload(responsesJSON))
	cliCtx, timeoutCancel := context.WithTimeout(cliCtx, imageGenerationTimeout)
	defer timeoutCancel()
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

func imageToolModelFromResponsesPayload(payload []byte) string {
	tools := gjson.GetBytes(payload, "tools")
	if !tools.Exists() || !tools.IsArray() {
		return defaultImagesToolModel
	}
	for _, tool := range tools.Array() {
		if !strings.EqualFold(strings.TrimSpace(tool.Get("type").String()), "image_generation") {
			continue
		}
		if model := strings.TrimSpace(tool.Get("model").String()); model != "" {
			return model
		}
	}
	return defaultImagesToolModel
}

func convertImagesGenerationRequestToResponses(rawJSON []byte) ([]byte, string, string, error) {
	return convertOpenAIImageRequestToResponses(rawJSON, false)
}

func convertImagesEditRequestToResponses(rawJSON []byte) ([]byte, string, string, error) {
	return convertOpenAIImageRequestToResponses(rawJSON, true)
}

func convertImagesEditMultipartRequestToResponses(c *gin.Context) ([]byte, string, string, error) {
	if err := c.Request.ParseMultipartForm(64 << 20); err != nil {
		return nil, "", "", fmt.Errorf("failed to parse image edit form request: %w", err)
	}
	form := c.Request.MultipartForm
	if form == nil {
		return nil, "", "", fmt.Errorf("image edit form is required")
	}

	raw := []byte(`{"model":"","prompt":"","image":[]}`)
	if values := form.Value; values != nil {
		for _, key := range []string{
			"model",
			"prompt",
			"size",
			"quality",
			"background",
			"output_format",
			"input_fidelity",
			"moderation",
			"output_compression",
			"partial_images",
		} {
			if vals := values[key]; len(vals) > 0 {
				raw, _ = sjson.SetBytes(raw, key, vals[0])
			}
		}
		for _, key := range []string{"image", "image[]"} {
			for _, value := range values[key] {
				if trimmed := strings.TrimSpace(value); trimmed != "" {
					raw, _ = sjson.SetBytes(raw, "image.-1", trimmed)
				}
			}
		}
	}

	if files := form.File; files != nil {
		for _, key := range []string{"image", "image[]"} {
			for _, fileHeader := range files[key] {
				dataURL, err := multipartImageDataURL(fileHeader)
				if err != nil {
					return nil, "", "", err
				}
				raw, _ = sjson.SetBytes(raw, "image.-1", dataURL)
			}
		}
		for fieldName, fileHeaders := range files {
			if !strings.HasPrefix(fieldName, "image[") {
				continue
			}
			for _, fileHeader := range fileHeaders {
				dataURL, err := multipartImageDataURL(fileHeader)
				if err != nil {
					return nil, "", "", err
				}
				raw, _ = sjson.SetBytes(raw, "image.-1", dataURL)
			}
		}
	}

	return convertImagesEditRequestToResponses(raw)
}

func multipartImageDataURL(fileHeader *multipart.FileHeader) (string, error) {
	file, err := fileHeader.Open()
	if err != nil {
		return "", fmt.Errorf("failed to open image file: %w", err)
	}
	defer func() {
		_ = file.Close()
	}()

	data, err := io.ReadAll(file)
	if err != nil {
		return "", fmt.Errorf("failed to read image file: %w", err)
	}
	contentType := http.DetectContentType(data)
	return "data:" + contentType + ";base64," + base64.StdEncoding.EncodeToString(data), nil
}

func convertOpenAIImageRequestToResponses(rawJSON []byte, includeImages bool) ([]byte, string, string, error) {
	root := gjson.ParseBytes(rawJSON)
	if !root.IsObject() {
		return nil, "", "", fmt.Errorf("request body must be a JSON object")
	}

	prompt := strings.TrimSpace(root.Get("prompt").String())
	if prompt == "" {
		return nil, "", "", fmt.Errorf("prompt is required")
	}

	size, ok := normalizeImageGenerationSize(root.Get("size").String())
	if !ok {
		return nil, "", "", fmt.Errorf("size must be auto or WIDTHxHEIGHT with max edge <= 3840, multiples of 16, ratio <= 3:1, and 655360-8294400 total pixels")
	}
	routeModelName := defaultImagesMainModel
	requestedModelName := defaultImagesMainModel

	out := []byte(`{"model":"","input":[],"stream":false,"tool_choice":{"type":"image_generation"},"tools":[]}`)
	out, _ = sjson.SetBytes(out, "model", defaultImagesMainModel)
	action := "generate"
	if includeImages {
		action = "edit"
	}
	tool := buildOpenAIImageTool(root, action, size)
	out, _ = sjson.SetRawBytes(out, "tools.-1", tool)

	if !includeImages {
		out = setImagesResponsesInput(out, prompt, nil)
		return out, routeModelName, requestedModelName, nil
	}

	images := collectOpenAIImageReferences(root)
	if len(images) == 0 {
		return nil, "", "", fmt.Errorf("image is required")
	}
	out = setImagesResponsesInput(out, prompt, images)
	return out, routeModelName, requestedModelName, nil
}

func buildOpenAIImageTool(root gjson.Result, action, size string) []byte {
	tool := []byte(`{"type":"image_generation","action":""}`)
	tool, _ = sjson.SetBytes(tool, "action", action)
	imageModel := strings.TrimSpace(root.Get("model").String())
	if imageModel == "" {
		imageModel = defaultImagesToolModel
	}
	tool, _ = sjson.SetBytes(tool, "model", imageModel)
	if size != "" && size != defaultImageGenerationSize {
		tool, _ = sjson.SetBytes(tool, "size", size)
	}
	for _, field := range []string{"quality", "background", "output_format", "input_fidelity", "moderation"} {
		if v := strings.TrimSpace(root.Get(field).String()); v != "" {
			tool, _ = sjson.SetBytes(tool, field, v)
		}
	}
	for _, field := range []string{"output_compression", "partial_images"} {
		if v := root.Get(field); v.Exists() && v.Type == gjson.Number {
			tool, _ = sjson.SetBytes(tool, field, v.Int())
		}
	}
	if mask := strings.TrimSpace(root.Get("mask.image_url").String()); mask != "" {
		tool, _ = sjson.SetBytes(tool, "input_image_mask.image_url", mask)
	} else if mask := strings.TrimSpace(root.Get("mask.url").String()); mask != "" {
		tool, _ = sjson.SetBytes(tool, "input_image_mask.image_url", mask)
	}
	return tool
}

func setImagesResponsesInput(out []byte, prompt string, images []string) []byte {
	content := []byte(`[]`)
	content, _ = sjson.SetBytes(content, "-1.type", "input_text")
	content, _ = sjson.SetBytes(content, "0.text", prompt)
	for _, imageURL := range images {
		part := []byte(`{"type":"input_image","image_url":""}`)
		part, _ = sjson.SetBytes(part, "image_url", imageURL)
		content, _ = sjson.SetRawBytes(content, "-1", part)
	}

	message := []byte(`{"type":"message","role":"user","content":[]}`)
	message, _ = sjson.SetRawBytes(message, "content", content)
	out, _ = sjson.SetRawBytes(out, "input.-1", message)
	return out
}

func normalizeImageGenerationSize(size string) (string, bool) {
	size = strings.ToLower(strings.TrimSpace(size))
	size = strings.ReplaceAll(size, " ", "")
	if size == "" || size == "auto" {
		return defaultImageGenerationSize, true
	}
	matches := imageGenerationSizePattern.FindStringSubmatch(size)
	if matches == nil {
		return "", false
	}

	width, widthErr := strconv.Atoi(matches[1])
	height, heightErr := strconv.Atoi(matches[2])
	if widthErr != nil || heightErr != nil || width <= 0 || height <= 0 {
		return "", false
	}
	if !validImageGenerationDimensions(width, height) {
		return "", false
	}
	return strconv.Itoa(width) + "x" + strconv.Itoa(height), true
}

func validImageGenerationDimensions(width, height int) bool {
	const (
		maxEdge   = 3840
		minPixels = 655360
		maxPixels = 8294400
	)
	if width > maxEdge || height > maxEdge {
		return false
	}
	if width%16 != 0 || height%16 != 0 {
		return false
	}
	longEdge := math.Max(float64(width), float64(height))
	shortEdge := math.Min(float64(width), float64(height))
	if longEdge/shortEdge > 3 {
		return false
	}
	pixels := width * height
	return pixels >= minPixels && pixels <= maxPixels
}

func collectOpenAIImageReferences(root gjson.Result) []string {
	var out []string
	add := func(value gjson.Result) {
		switch {
		case value.Type == gjson.String:
			if s := strings.TrimSpace(value.String()); s != "" {
				out = append(out, s)
			}
		case value.IsObject():
			for _, path := range []string{"image_url", "url", "file_id"} {
				if s := strings.TrimSpace(value.Get(path).String()); s != "" {
					out = append(out, s)
					return
				}
			}
		}
	}

	image := root.Get("image")
	if image.IsArray() {
		for _, item := range image.Array() {
			add(item)
		}
	} else if image.Exists() {
		add(image)
	}

	images := root.Get("images")
	if images.IsArray() {
		for _, item := range images.Array() {
			add(item)
		}
	} else if images.Exists() {
		add(images)
	}

	return out
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
