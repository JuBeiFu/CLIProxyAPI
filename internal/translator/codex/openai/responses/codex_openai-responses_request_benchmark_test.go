package responses

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
)

var benchmarkResponsesRequestSink []byte

func BenchmarkConvertOpenAIResponsesRequestToCodex_LargeTranscript(b *testing.B) {
	raw := buildBenchmarkResponsesRequest(240, true, true, true)
	b.SetBytes(int64(len(raw)))
	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		benchmarkResponsesRequestSink = ConvertOpenAIResponsesRequestToCodex("gpt-5.4", raw, false)
	}
}

func BenchmarkConvertOpenAIResponsesRequestToCodex_LargeTranscriptNoReasoning(b *testing.B) {
	raw := buildBenchmarkResponsesRequest(240, false, true, true)
	b.SetBytes(int64(len(raw)))
	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		benchmarkResponsesRequestSink = ConvertOpenAIResponsesRequestToCodex("gpt-5.4", raw, false)
	}
}

func BenchmarkConvertOpenAIResponsesRequestToCodex_LargeTranscriptNoTools(b *testing.B) {
	raw := buildBenchmarkResponsesRequest(240, true, true, false)
	b.SetBytes(int64(len(raw)))
	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		benchmarkResponsesRequestSink = ConvertOpenAIResponsesRequestToCodex("gpt-5.4", raw, false)
	}
}

func BenchmarkNormalizeCodexBuiltinTools_FunctionHeavy(b *testing.B) {
	raw := buildToolHeavyBenchmarkRequest()
	b.SetBytes(int64(len(raw)))
	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		benchmarkResponsesRequestSink = normalizeCodexBuiltinTools(raw)
	}
}

func BenchmarkConvertOpenAIResponsesRequestToCodex_StringInput(b *testing.B) {
	raw := []byte(`{"model":"gpt-5.4","input":"` + strings.Repeat("find performance regressions ", 128) + `"}`)
	b.SetBytes(int64(len(raw)))
	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		benchmarkResponsesRequestSink = ConvertOpenAIResponsesRequestToCodex("gpt-5.4", raw, false)
	}
}

func buildBenchmarkResponsesRequest(items int, includeReasoning bool, includeSystem bool, includeTools bool) []byte {
	var buf bytes.Buffer
	buf.WriteString(`{"model":"gpt-5.4","input":[`)
	first := true
	for i := 0; i < items; i++ {
		if includeReasoning && i%12 == 0 {
			writeComma(&buf, &first)
			fmt.Fprintf(&buf, `{"type":"reasoning","id":"rs_%06d","summary":[{"type":"summary_text","text":"%s"}]}`, i, strings.Repeat("private chain ", 16))
			writeComma(&buf, &first)
			fmt.Fprintf(&buf, `{"type":"item_reference","id":"rs_%06d"}`, i+1)
		}

		role := "user"
		id := fmt.Sprintf("msg_%06d", i)
		if i%3 == 1 {
			role = "assistant"
			id = fmt.Sprintf("item_unsupported_%06d", i)
		}
		if includeSystem && i%40 == 0 {
			role = "system"
		}

		writeComma(&buf, &first)
		fmt.Fprintf(
			&buf,
			`{"type":"message","id":"%s","role":"%s","content":[{"type":"input_text","text":"%s"}]}`,
			id,
			role,
			strings.Repeat("benchmark transcript payload ", 24),
		)
	}
	buf.WriteByte(']')

	if includeTools {
		buf.WriteString(`,"tools":[`)
		for i := 0; i < 12; i++ {
			if i > 0 {
				buf.WriteByte(',')
			}
			fmt.Fprintf(
				&buf,
				`{"type":"function","function":{"name":"lookup_%02d","description":"lookup helper","parameters":{"type":"object","properties":{"query":{"type":"string"}}},"strict":true}}`,
				i,
			)
		}
		buf.WriteString(`,{"type":"tool_search","execution":"sync","description":"Search installed tools","parameters":{"type":"object","properties":{"query":{"type":"string"}}}}]`)
		buf.WriteString(`,"tool_choice":{"type":"allowed_tools","tools":[{"type":"web_search_preview"},{"type":"web_search_preview_2025_03_11"}]}`)
	}

	buf.WriteString(`,"stream":false,"store":true,"temperature":0.7,"top_p":0.9,"max_output_tokens":4096,"truncation":"disabled","user":"bench-user"}`)
	return buf.Bytes()
}

func buildToolHeavyBenchmarkRequest() []byte {
	var buf bytes.Buffer
	buf.WriteString(`{"tools":[`)
	for i := 0; i < 32; i++ {
		if i > 0 {
			buf.WriteByte(',')
		}
		fmt.Fprintf(
			&buf,
			`{"type":"function","function":{"name":"lookup_%02d","description":"lookup helper","parameters":{"type":"object","properties":{"query":{"type":"string"},"page":{"type":"integer"}},"required":["query"]},"strict":true}}`,
			i,
		)
	}
	buf.WriteString(`,{"type":"tool_search","execution":"sync","description":"Search installed tools","parameters":{"type":"object","properties":{"query":{"type":"string"}}}}],"tool_choice":{"type":"allowed_tools","tools":[{"type":"web_search_preview"},{"type":"web_search_preview_2025_03_11"}]}}`)
	return buf.Bytes()
}

func writeComma(buf *bytes.Buffer, first *bool) {
	if *first {
		*first = false
		return
	}
	buf.WriteByte(',')
}
