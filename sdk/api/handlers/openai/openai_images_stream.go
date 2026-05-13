package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	. "github.com/router-for-me/CLIProxyAPI/v6/internal/constant"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/api/handlers"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

type openAIImageResultConfig struct {
	background   string
	outputFormat string
	quality      string
	size         string
}

type openAIImageFinalResult struct {
	result        string
	background    string
	outputFormat  string
	quality       string
	size          string
	revisedPrompt string
}

type openAIImageStreamMode string

const (
	openAIImageStreamModeGenerate openAIImageStreamMode = "image_generation"
	openAIImageStreamModeEdit     openAIImageStreamMode = "image_edit"
)

type openAIImageStreamBridge struct {
	mode              openAIImageStreamMode
	config            openAIImageResultConfig
	pending           []byte
	createdAt         int64
	usageRaw          []byte
	finalItems        []openAIImageFinalResult
	responseCompleted bool
	sentDone          bool
}

func (h *OpenAIAPIHandler) executeImagesStreamingResponsesPayload(c *gin.Context, responsesJSON []byte, routeModelName, requestedModelName string) {
	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		c.JSON(http.StatusInternalServerError, handlers.ErrorResponse{
			Error: handlers.ErrorDetail{
				Message: "Streaming not supported",
				Type:    "server_error",
			},
		})
		return
	}

	cliCtx, cliCancel := h.GetContextWithCancel(h, c, context.Background())
	cliCtx = handlers.WithImageGenerationRequest(cliCtx)
	cliCtx = handlers.WithImageGenerationModel(cliCtx, imageToolModelFromResponsesPayload(responsesJSON))
	cliCtx, timeoutCancel := context.WithTimeout(cliCtx, imageGenerationTimeout)
	defer timeoutCancel()

	candidates := buildImagesMainModelCandidates(routeModelName, requestedModelName)
	for index, candidate := range candidates {
		attemptPayload := setImagesResponsesMainModel(responsesJSON, candidate.routeModelName)
		dataChan, upstreamHeaders, errChan := h.ExecuteStreamWithAuthManagerRequestedModelPrecommit(
			cliCtx,
			c,
			flusher,
			OpenaiResponse,
			candidate.routeModelName,
			attemptPayload,
			"",
			candidate.requestedModelName,
		)

		for {
			select {
			case <-c.Request.Context().Done():
				cliCancel(c.Request.Context().Err())
				return
			case errMsg, ok := <-errChan:
				if !ok {
					errChan = nil
					continue
				}
				if errMsg == nil {
					continue
				}
				if index < len(candidates)-1 && shouldFallbackImagesMainModel(errMsg) {
					goto nextCandidate
				}
				if c.Writer.Written() {
					handlers.PrepareEventStreamHeaders(c, upstreamHeaders)
					writeOpenAIImageStreamError(c.Writer, errMsg)
					flusher.Flush()
				} else {
					h.WriteErrorResponse(c, errMsg)
				}
				cliCancel(errMsg.Error)
				return
			case chunk, ok := <-dataChan:
				handlers.PrepareEventStreamHeaders(c, upstreamHeaders)
				if !ok {
					bridge := newOpenAIImageStreamBridge(attemptPayload)
					bridge.Flush(c.Writer)
					bridge.emitDone(c.Writer)
					flusher.Flush()
					cliCancel(nil)
					return
				}

				bridge := newOpenAIImageStreamBridge(attemptPayload)
				bridge.WriteChunk(c.Writer, chunk)
				flusher.Flush()
				h.ForwardStream(c, flusher, func(err error) { cliCancel(err) }, dataChan, errChan, handlers.StreamForwardOptions{
					WriteChunk: func(chunk []byte) {
						bridge.WriteChunk(c.Writer, chunk)
					},
					WriteTerminalError: func(errMsg *interfaces.ErrorMessage) {
						bridge.Flush(c.Writer)
						writeOpenAIImageStreamError(c.Writer, errMsg)
					},
					WriteDone: func() {
						bridge.Flush(c.Writer)
						bridge.emitDone(c.Writer)
					},
				})
				return
			}
		}

	nextCandidate:
	}

	cliCancel(nil)
}

func newOpenAIImageStreamBridge(requestPayload []byte) *openAIImageStreamBridge {
	root := gjson.ParseBytes(requestPayload)
	tool := findFirstImageGenerationTool(root.Get("tools"))
	action := strings.TrimSpace(tool.Get("action").String())
	config := resolveOpenAIImageResultConfig(root)
	if strings.TrimSpace(config.background) == "" {
		config.background = defaultImageGenerationSize
	}
	if strings.TrimSpace(config.quality) == "" {
		config.quality = defaultImageGenerationSize
	}
	if strings.TrimSpace(config.size) == "" {
		config.size = defaultImageGenerationSize
	}
	if strings.TrimSpace(config.outputFormat) == "" {
		config.outputFormat = "png"
	}
	return &openAIImageStreamBridge{
		mode:   imageStreamModeFromAction(action),
		config: config,
	}
}

func (b *openAIImageStreamBridge) WriteChunk(w io.Writer, chunk []byte) {
	if len(chunk) == 0 {
		return
	}
	if responsesSSENeedsLineBreak(b.pending, chunk) {
		b.pending = append(b.pending, '\n')
	}
	b.pending = append(b.pending, chunk...)
	for {
		frameLen := responsesSSEFrameLen(b.pending)
		if frameLen == 0 {
			break
		}
		frame := bytes.Clone(b.pending[:frameLen])
		copy(b.pending, b.pending[frameLen:])
		b.pending = b.pending[:len(b.pending)-frameLen]
		b.handleFrame(w, frame)
	}
	if len(bytes.TrimSpace(b.pending)) == 0 {
		b.pending = b.pending[:0]
		return
	}
	if !responsesSSECanEmitWithoutDelimiter(b.pending) {
		return
	}
	frame := bytes.Clone(b.pending)
	b.pending = b.pending[:0]
	b.handleFrame(w, frame)
}

func (b *openAIImageStreamBridge) Flush(w io.Writer) {
	if len(b.pending) > 0 {
		if responsesSSECanEmitWithoutDelimiter(b.pending) {
			frame := bytes.Clone(b.pending)
			b.pending = b.pending[:0]
			b.handleFrame(w, frame)
		} else {
			b.pending = b.pending[:0]
		}
	}
	b.emitFinalItems(w)
}

func (b *openAIImageStreamBridge) emitDone(w io.Writer) {
	if w == nil || b.sentDone {
		return
	}
	writeOpenAIImageSSEData(w, nil)
	b.sentDone = true
}

func (b *openAIImageStreamBridge) handleFrame(w io.Writer, frame []byte) {
	data := openAIImageSSEData(frame)
	if len(data) == 0 {
		return
	}
	if bytes.Equal(data, []byte("[DONE]")) {
		b.emitDone(w)
		return
	}
	if !json.Valid(data) {
		return
	}

	payload := gjson.ParseBytes(data)
	eventType := strings.TrimSpace(payload.Get("type").String())
	switch eventType {
	case "response.created":
		if createdAt := payload.Get("response.created_at").Int(); createdAt > 0 {
			b.createdAt = createdAt
		}
	case "response.image_generation_call.partial_image":
		if createdAt := payload.Get("created_at").Int(); createdAt > 0 {
			b.createdAt = createdAt
		}
		b.emitPartialImage(w, payload)
	case "response.output_item.done":
		item := payload.Get("item")
		if item.Get("type").String() != "image_generation_call" {
			return
		}
		b.finalItems = append(b.finalItems, openAIImageFinalResult{
			result:        strings.TrimSpace(item.Get("result").String()),
			background:    strings.TrimSpace(item.Get("background").String()),
			outputFormat:  strings.TrimSpace(item.Get("output_format").String()),
			quality:       strings.TrimSpace(item.Get("quality").String()),
			size:          strings.TrimSpace(item.Get("size").String()),
			revisedPrompt: strings.TrimSpace(item.Get("revised_prompt").String()),
		})
		if b.responseCompleted {
			b.emitFinalItems(w)
		}
	case "response.completed":
		response := payload.Get("response")
		if createdAt := response.Get("created_at").Int(); createdAt > 0 {
			b.createdAt = createdAt
		}
		if usage := response.Get("usage"); usage.Exists() && usage.IsObject() {
			b.usageRaw = append([]byte(nil), usage.Raw...)
		}
		b.config = mergeOpenAIImageResultConfig(b.config, resolveOpenAIImageResultConfig(response))
		b.responseCompleted = true
		b.emitFinalItems(w)
	}
}

func (b *openAIImageStreamBridge) emitPartialImage(w io.Writer, payload gjson.Result) {
	imageData := strings.TrimSpace(payload.Get("partial_image_b64").String())
	if imageData == "" {
		return
	}
	out := []byte(`{"type":"","b64_json":"","created_at":0,"background":"","output_format":"","partial_image_index":0,"quality":"","size":""}`)
	eventType := string(b.mode) + ".partial_image"
	out, _ = sjson.SetBytes(out, "type", eventType)
	out, _ = sjson.SetBytes(out, "b64_json", imageData)
	out, _ = sjson.SetBytes(out, "created_at", b.eventCreatedAt())
	out, _ = sjson.SetBytes(out, "background", defaultImageConfigValue(b.config.background, defaultImageGenerationSize))
	out, _ = sjson.SetBytes(out, "output_format", defaultImageConfigValue(b.config.outputFormat, "png"))
	out, _ = sjson.SetBytes(out, "partial_image_index", payload.Get("partial_image_index").Int())
	out, _ = sjson.SetBytes(out, "quality", defaultImageConfigValue(b.config.quality, defaultImageGenerationSize))
	out, _ = sjson.SetBytes(out, "size", defaultImageConfigValue(b.config.size, defaultImageGenerationSize))
	writeOpenAIImageSSEEvent(w, eventType, out)
}

func (b *openAIImageStreamBridge) emitFinalItems(w io.Writer) {
	if len(b.finalItems) == 0 {
		return
	}
	for _, item := range b.finalItems {
		if strings.TrimSpace(item.result) == "" {
			continue
		}
		out := []byte(`{"type":"","b64_json":"","created_at":0,"background":"","output_format":"","quality":"","size":""}`)
		eventType := string(b.mode) + ".completed"
		out, _ = sjson.SetBytes(out, "type", eventType)
		out, _ = sjson.SetBytes(out, "b64_json", item.result)
		out, _ = sjson.SetBytes(out, "created_at", b.eventCreatedAt())
		out, _ = sjson.SetBytes(out, "background", defaultImageConfigValue(firstNonEmpty(item.background, b.config.background), defaultImageGenerationSize))
		out, _ = sjson.SetBytes(out, "output_format", defaultImageConfigValue(firstNonEmpty(item.outputFormat, b.config.outputFormat), "png"))
		out, _ = sjson.SetBytes(out, "quality", defaultImageConfigValue(firstNonEmpty(item.quality, b.config.quality), defaultImageGenerationSize))
		out, _ = sjson.SetBytes(out, "size", defaultImageConfigValue(firstNonEmpty(item.size, b.config.size), defaultImageGenerationSize))
		if len(b.usageRaw) > 0 && json.Valid(b.usageRaw) {
			out, _ = sjson.SetRawBytes(out, "usage", b.usageRaw)
		}
		writeOpenAIImageSSEEvent(w, eventType, out)
	}
	b.finalItems = nil
}

func (b *openAIImageStreamBridge) eventCreatedAt() int64 {
	if b.createdAt > 0 {
		return b.createdAt
	}
	return time.Now().Unix()
}

func writeOpenAIImageStreamError(w io.Writer, errMsg *interfaces.ErrorMessage) {
	if w == nil {
		return
	}
	status := http.StatusInternalServerError
	if errMsg != nil && errMsg.StatusCode > 0 {
		status = errMsg.StatusCode
	}
	errText := http.StatusText(status)
	if errMsg != nil && errMsg.Error != nil {
		if text := strings.TrimSpace(errMsg.Error.Error()); text != "" {
			errText = text
		}
	}
	body := handlers.BuildErrorResponseBody(status, errText)
	writeResponsesSSEChunk(w, []byte("event: error\ndata: "+string(body)+"\n\n"))
}

func writeOpenAIImageSSEEvent(w io.Writer, eventType string, data []byte) {
	if w == nil || len(data) == 0 {
		return
	}
	writeResponsesSSEChunk(w, []byte("event: "+eventType+"\ndata: "+string(data)+"\n\n"))
}

func writeOpenAIImageSSEData(w io.Writer, data []byte) {
	if w == nil {
		return
	}
	if len(data) == 0 {
		writeResponsesSSEChunk(w, []byte("data: [DONE]\n\n"))
		return
	}
	writeResponsesSSEChunk(w, []byte("data: "+string(data)+"\n\n"))
}

func openAIImageSSEData(frame []byte) []byte {
	var parts [][]byte
	for _, line := range bytes.Split(frame, []byte("\n")) {
		line = bytes.TrimSpace(bytes.TrimSuffix(line, []byte("\r")))
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		parts = append(parts, bytes.TrimSpace(line[len("data:"):]))
	}
	switch len(parts) {
	case 0:
		return nil
	case 1:
		return parts[0]
	default:
		return bytes.Join(parts, []byte("\n"))
	}
}

func resolveOpenAIImageResultConfig(root gjson.Result) openAIImageResultConfig {
	config := openAIImageResultConfig{}
	config = mergeOpenAIImageResultConfig(config, imageConfigFromResult(findFirstImageGenerationTool(root.Get("tools"))))
	output := root.Get("output")
	if output.IsArray() {
		for _, item := range output.Array() {
			if item.Get("type").String() != "image_generation_call" {
				continue
			}
			config = mergeOpenAIImageResultConfig(config, imageConfigFromResult(item))
			break
		}
	}
	return config
}

func imageConfigFromResult(result gjson.Result) openAIImageResultConfig {
	if !result.Exists() || !result.IsObject() {
		return openAIImageResultConfig{}
	}
	return openAIImageResultConfig{
		background:   strings.TrimSpace(result.Get("background").String()),
		outputFormat: strings.TrimSpace(result.Get("output_format").String()),
		quality:      strings.TrimSpace(result.Get("quality").String()),
		size:         strings.TrimSpace(result.Get("size").String()),
	}
}

func mergeOpenAIImageResultConfig(base, override openAIImageResultConfig) openAIImageResultConfig {
	if strings.TrimSpace(override.background) != "" {
		base.background = strings.TrimSpace(override.background)
	}
	if strings.TrimSpace(override.outputFormat) != "" {
		base.outputFormat = strings.TrimSpace(override.outputFormat)
	}
	if strings.TrimSpace(override.quality) != "" {
		base.quality = strings.TrimSpace(override.quality)
	}
	if strings.TrimSpace(override.size) != "" {
		base.size = strings.TrimSpace(override.size)
	}
	return base
}

func normalizeResolvedImageConfigValue(value string) string {
	value = strings.TrimSpace(value)
	if strings.EqualFold(value, "auto") {
		return ""
	}
	return value
}

func findFirstImageGenerationTool(tools gjson.Result) gjson.Result {
	if !tools.Exists() || !tools.IsArray() {
		return gjson.Result{}
	}
	for _, tool := range tools.Array() {
		if strings.EqualFold(strings.TrimSpace(tool.Get("type").String()), "image_generation") {
			return tool
		}
	}
	return gjson.Result{}
}

func imageStreamModeFromAction(action string) openAIImageStreamMode {
	if strings.EqualFold(strings.TrimSpace(action), "edit") {
		return openAIImageStreamModeEdit
	}
	return openAIImageStreamModeGenerate
}

func defaultImageConfigValue(value, fallback string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return fallback
	}
	return value
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
