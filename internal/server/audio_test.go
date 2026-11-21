package server

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/reqlog"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// audioTestUpstream 假上游（零网络）。handler 与 upstream 必须在同一进程内，
// 故直接给 Client 装 roundTripFunc 而不是起 httptest 监听。
func audioTestUpstream(handle roundTripFunc) *upstream.Client {
	return &upstream.Client{
		HTTP:          &http.Client{Transport: handle},
		ChatBaseCN:    "https://cn.example",
		BillingBaseCN: "https://cn.example",
	}
}

// audioTestHandler 单账号池 + 假上游；tts/asr 控制两个开关，log 为 nil 时不记账。
func audioTestHandler(tts, asr bool, handle roundTripFunc, log *reqlog.Recorder) *Handler {
	return NewHandler(Config{
		Pool:       testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream:   audioTestUpstream(handle),
		RequestLog: log,
		TTSEnabled: tts,
		ASREnabled: asr,
	})
}

// speechSSEResponse 把若干 PCM 分片包装成上游 SSE（base64 + done + [DONE]）。
func speechSSEResponse(chunks ...[]byte) *http.Response {
	var b strings.Builder
	for _, c := range chunks {
		fmt.Fprintf(&b, "data: {\"type\":\"speech.audio.delta\",\"audio\":{\"data\":%q}}\n",
			base64.StdEncoding.EncodeToString(c))
	}
	b.WriteString("data: {\"type\":\"speech.audio.done\"}\n")
	b.WriteString("data: [DONE]\n")
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(b.String())),
	}
}

func jsonResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

// TestSpeechWAVHeader WAV 封装契约：44 字节 RIFF 头逐字段正确（24kHz / 单声道 /
// 16-bit s16le），data 段长度等于 PCM 字节数，且 PCM 原样跟随。
//
// 这是**变异验证点**：把 bit depth 字段（偏移 34）写成 8/32 必须让本用例变红。
func TestSpeechWAVHeader(t *testing.T) {
	pcm := []byte{0x01, 0x02, 0x03, 0x04, 0x05}
	h := audioTestHandler(true, false, func(r *http.Request) (*http.Response, error) {
		return speechSSEResponse(pcm[:2], pcm[2:]), nil
	}, nil)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/audio/speech",
		strings.NewReader(`{"model":"tts-1","input":"你好"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "audio/wav" {
		t.Errorf("content-type=%q want audio/wav", ct)
	}
	body := rec.Body.Bytes()
	if len(body) != 44+len(pcm) {
		t.Fatalf("body=%d bytes want %d（44 字节头 + %d PCM）", len(body), 44+len(pcm), len(pcm))
	}
	// 变异验证点：PCM 拼接少一段 → 这里的长度与逐字节比较同时变红。
	if !bytes.Equal(body[44:], pcm) {
		t.Errorf("PCM 区被改动: %v want %v", body[44:], pcm)
	}
	if got := rec.Header().Get("Content-Length"); got != fmt.Sprint(len(pcm)+44) {
		t.Errorf("content-length=%q", got)
	}
	le := binary.LittleEndian
	if string(body[0:4]) != "RIFF" || string(body[8:16]) != "WAVEfmt " || string(body[36:40]) != "data" {
		t.Fatalf("RIFF 魔数错位: %q %q %q", body[0:4], body[8:16], body[36:40])
	}
	if got := le.Uint32(body[4:8]); got != uint32(36+len(pcm)) {
		t.Errorf("RIFF size=%d want %d", got, 36+len(pcm))
	}
	if got := le.Uint32(body[16:20]); got != 16 {
		t.Errorf("fmt 块长度=%d want 16", got)
	}
	if got := le.Uint16(body[20:22]); got != 1 {
		t.Errorf("audioFormat=%d want 1 (PCM)", got)
	}
	if got := le.Uint16(body[22:24]); got != 1 {
		t.Errorf("声道数=%d want 1", got)
	}
	if got := le.Uint32(body[24:28]); got != 24000 {
		t.Errorf("采样率=%d want 24000", got)
	}
	if got := le.Uint32(body[28:32]); got != 24000*1*16/8 {
		t.Errorf("byteRate=%d want %d", got, 24000*1*16/8)
	}
	if got := le.Uint16(body[32:34]); got != 2 {
		t.Errorf("blockAlign=%d want 2", got)
	}
	// 变异验证点：这里写错 bit depth 字段必须变红。
	if got := le.Uint16(body[34:36]); got != 16 {
		t.Errorf("bit depth=%d want 16", got)
	}
	if got := le.Uint32(body[40:44]); got != uint32(len(pcm)) {
		t.Errorf("data 段长度=%d want %d", got, len(pcm))
	}
}

// TestSpeechOutboundRequest 出站只看固定契约：model 一律 hunyuan-tts-2.5（客户端填的
// tts-1 / cn:tts-1 只用于取 realm，不转发），response_format 恒 pcm，sample_rate 24000。
func TestSpeechOutboundRequest(t *testing.T) {
	var path, accept string
	var body map[string]any
	h := audioTestHandler(true, false, func(r *http.Request) (*http.Response, error) {
		path, accept = r.URL.Path, r.Header.Get("Accept")
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Errorf("出站 body 非 JSON: %v", err)
		}
		return speechSSEResponse([]byte{1}), nil
	}, nil)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/audio/speech",
		strings.NewReader(`{"model":"cn:tts-1","voice":"custom_voice","input":"  测试文本  "}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if path != "/v2/audio/speech" {
		t.Errorf("上游路径=%q", path)
	}
	if accept != "text/event-stream" {
		t.Errorf("Accept=%q want text/event-stream", accept)
	}
	if body["model"] != upstream.TTSModel {
		t.Errorf("model=%v want %v", body["model"], upstream.TTSModel)
	}
	if stream, _ := body["stream"].(bool); !stream {
		t.Errorf("stream=%v want true", body["stream"])
	}
	speech, _ := body["speech"].(map[string]any)
	if speech["input"] != "测试文本" {
		t.Errorf("speech.input=%v（首尾空白应被裁掉）", speech["input"])
	}
	if speech["voice"] != "custom_voice" {
		t.Errorf("speech.voice=%v", speech["voice"])
	}
	if speech["response_format"] != "pcm" {
		t.Errorf("speech.response_format=%v want pcm", speech["response_format"])
	}
	if rate, _ := speech["sample_rate"].(float64); int(rate) != upstream.TTSSampleRate {
		t.Errorf("speech.sample_rate=%v", speech["sample_rate"])
	}
}

// TestSpeechResponseFormatPCM response_format=pcm 时不套 WAV 头，直接回裸 PCM。
func TestSpeechResponseFormatPCM(t *testing.T) {
	pcm := []byte{9, 8, 7}
	h := audioTestHandler(true, false, func(r *http.Request) (*http.Response, error) {
		return speechSSEResponse(pcm), nil
	}, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/audio/speech",
		strings.NewReader(`{"input":"hi","response_format":"pcm"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "audio/pcm" {
		t.Errorf("content-type=%q want audio/pcm", ct)
	}
	if !bytes.Equal(rec.Body.Bytes(), pcm) {
		t.Errorf("body=%v want %v", rec.Body.Bytes(), pcm)
	}
}

// TestSpeechDisabledForbidden 开关默认关闭 ⇒ 403 + tts_disabled，且**不打上游**。
//
// 这是**变异验证点**：去掉 handler 里的开关判断必须让本用例变红。
func TestSpeechDisabledForbidden(t *testing.T) {
	calls := 0
	h := audioTestHandler(false, false, func(r *http.Request) (*http.Response, error) {
		calls++
		return speechSSEResponse([]byte{1}), nil
	}, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/audio/speech",
		strings.NewReader(`{"input":"hi"}`)))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("code=%d want 403", rec.Code)
	}
	if calls != 0 {
		t.Errorf("关闭时不得打上游，实际 %d 次", calls)
	}
	var env struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("错误信封非 JSON: %v body=%s", err, rec.Body)
	}
	if env.Error.Code != "tts_disabled" {
		t.Errorf("code=%q want tts_disabled", env.Error.Code)
	}
	if !strings.Contains(env.Error.Message, "upstream.tts_enabled") {
		t.Errorf("message=%q 应点名开关 upstream.tts_enabled", env.Error.Message)
	}
}

// TestSpeechRequestLevelErrorFailsFast 「get voice config failed」是请求级错误
// （音色表是全局配置，与账号无关）：只试一次、不轮转，并给出默认音色 hint。
func TestSpeechRequestLevelErrorFailsFast(t *testing.T) {
	calls := 0
	h := audioTestHandler(true, false, func(r *http.Request) (*http.Response, error) {
		calls++
		return jsonResponse(http.StatusInternalServerError,
			`{"code":10000,"msg":"get voice config failed: voice x not found in Rainbow"}`), nil
	}, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/audio/speech",
		strings.NewReader(`{"input":"hi","voice":"x"}`)))
	if calls != 1 {
		t.Errorf("请求级错误不应换号，上游调用=%d want 1", calls)
	}
	if rec.Code < 400 {
		t.Fatalf("code=%d want >=400 body=%s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), upstream.TTSDefaultVoice) {
		t.Errorf("body=%s 应带默认音色 hint", rec.Body)
	}
}

// multipartAudio 构造 OpenAI 形态的上传体；field 决定用 file（OpenAI SDK）还是 audio。
func multipartAudio(t *testing.T, field, filename string, data []byte) (*bytes.Buffer, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	part, err := mw.CreateFormFile(field, filename)
	if err != nil {
		t.Fatalf("multipart: %v", err)
	}
	if _, err := part.Write(data); err != nil {
		t.Fatalf("multipart write: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("multipart close: %v", err)
	}
	return &buf, mw.FormDataContentType()
}

// multipartAudioTyped 构造带显式 Content-Type 的上传体（对齐真实客户端：
// OpenAI SDK 会带 audio/wav，某些工具有时不带、靠扩展名补）。
func multipartAudioTyped(t *testing.T, field, filename, contentType string, data []byte) (*bytes.Buffer, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	h := textproto.MIMEHeader{}
	h.Set("Content-Disposition", fmt.Sprintf(`form-data; name=%q; filename=%q`, field, filename))
	if contentType != "" {
		h.Set("Content-Type", contentType)
	}
	part, err := mw.CreatePart(h)
	if err != nil {
		t.Fatalf("multipart part: %v", err)
	}
	if _, err := part.Write(data); err != nil {
		t.Fatalf("multipart write: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("multipart close: %v", err)
	}
	return &buf, mw.FormDataContentType()
}

// TestTranscriptionsForwardsAudioField 入站字段 file（OpenAI）→ 上游固定字段 audio，
// 音频字节原样转发（不转码），回 OpenAI 的 {"text":...}。
func TestTranscriptionsForwardsAudioField(t *testing.T) {
	audio := []byte{0x52, 0x49, 0x46, 0x46, 0x00, 0x01, 0x02}
	var gotField, gotName, gotType string
	var gotAudio []byte
	h := audioTestHandler(false, true, func(r *http.Request) (*http.Response, error) {
		mr, err := r.MultipartReader()
		if err != nil {
			t.Errorf("上游请求不是 multipart: %v", err)
			return jsonResponse(400, `{"code":15052}`), nil
		}
		p, err := mr.NextPart()
		if err != nil {
			t.Errorf("读 multipart: %v", err)
			return jsonResponse(400, `{"code":15052}`), nil
		}
		gotField, gotName, gotType = p.FormName(), p.FileName(), p.Header.Get("Content-Type")
		gotAudio, _ = io.ReadAll(p)
		return jsonResponse(200, `{"code":0,"msg":"","text":"识别结果"}`), nil
	}, nil)

	body, ct := multipartAudioTyped(t, "file", "probe.wav", "audio/wav", audio)
	req := httptest.NewRequest("POST", "/v1/audio/transcriptions", body)
	req.Header.Set("Content-Type", ct)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if gotField != "audio" {
		t.Errorf("上游字段名=%q want audio", gotField)
	}
	if gotName != "probe.wav" || gotType != "audio/wav" {
		t.Errorf("上游 filename=%q content-type=%q", gotName, gotType)
	}
	if !bytes.Equal(gotAudio, audio) {
		t.Errorf("音频被改动: %v want %v", gotAudio, audio)
	}
	var out struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || out.Text != "识别结果" {
		t.Errorf("回程=%s err=%v", rec.Body, err)
	}
}

// multipartWithFormat 构造一份带 response_format 字段 + 音频文件的 multipart 体。
func multipartWithFormat(t *testing.T, format string) (*bytes.Buffer, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if format != "" {
		if err := mw.WriteField("response_format", format); err != nil {
			t.Fatalf("write field: %v", err)
		}
	}
	part, err := mw.CreateFormFile("file", "a.wav")
	if err != nil {
		t.Fatalf("create file: %v", err)
	}
	if _, err := part.Write([]byte{1, 2, 3}); err != nil {
		t.Fatalf("write file: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return &buf, mw.FormDataContentType()
}

// TestTranscriptionsFillsMissingMime 客户端不带 Content-Type 时按扩展名补 mime
// （上游按 mime 判格式，缺了会报 15052/15053 一类的解析失败）。
func TestTranscriptionsFillsMissingMime(t *testing.T) {
	var gotType string
	h := audioTestHandler(false, true, func(r *http.Request) (*http.Response, error) {
		mr, err := r.MultipartReader()
		if err != nil {
			t.Errorf("上游请求不是 multipart: %v", err)
			return jsonResponse(400, `{"code":15052}`), nil
		}
		p, err := mr.NextPart()
		if err != nil {
			t.Errorf("读 multipart: %v", err)
			return jsonResponse(400, `{"code":15052}`), nil
		}
		gotType = p.Header.Get("Content-Type")
		return jsonResponse(200, `{"code":0,"text":"ok"}`), nil
	}, nil)
	body, ct := multipartAudioTyped(t, "file", "probe.wav", "", []byte{1, 2, 3})
	req := httptest.NewRequest("POST", "/v1/audio/transcriptions", body)
	req.Header.Set("Content-Type", ct)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if !strings.Contains(gotType, "audio/wav") && !strings.Contains(gotType, "audio/x-wav") {
		t.Errorf("上游 part content-type=%q，应由 .wav 扩展名补出音频 mime", gotType)
	}
}

// TestTranscriptionsResponseFormat text 返回纯文本；verbose_json（要时间戳）明确 400，
// 且不打上游。
func TestTranscriptionsResponseFormat(t *testing.T) {
	h := audioTestHandler(false, true, func(r *http.Request) (*http.Response, error) {
		return jsonResponse(200, `{"code":0,"text":"hi there"}`), nil
	}, nil)
	body, ct := multipartWithFormat(t, "text")
	req := httptest.NewRequest("POST", "/v1/audio/transcriptions", body)
	req.Header.Set("Content-Type", ct)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("text: code=%d body=%s", rec.Code, rec.Body)
	}
	if rec.Body.String() != "hi there" {
		t.Errorf("text 回程=%q want 纯文本", rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/plain") {
		t.Errorf("content-type=%q", got)
	}

	calls := 0
	h2 := audioTestHandler(false, true, func(r *http.Request) (*http.Response, error) {
		calls++
		return jsonResponse(200, `{"code":0,"text":"x"}`), nil
	}, nil)
	body2, ct2 := multipartWithFormat(t, "verbose_json")
	req2 := httptest.NewRequest("POST", "/v1/audio/transcriptions", body2)
	req2.Header.Set("Content-Type", ct2)
	rec2 := httptest.NewRecorder()
	h2.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusBadRequest {
		t.Errorf("verbose_json: code=%d want 400 body=%s", rec2.Code, rec2.Body)
	}
	if calls != 0 {
		t.Errorf("参数错误不得打上游，实际 %d 次", calls)
	}
}

// TestTranscriptions15053Hint 上游 502 + 15053 是请求级错误：一次即止 + WAV hint。
func TestTranscriptions15053Hint(t *testing.T) {
	calls := 0
	h := audioTestHandler(false, true, func(r *http.Request) (*http.Response, error) {
		calls++
		return jsonResponse(http.StatusBadGateway, `{"code":15053,"msg":"ASR recognition failed"}`), nil
	}, nil)
	body, ct := multipartAudio(t, "file", "a.pcm", []byte{1, 2, 3})
	req := httptest.NewRequest("POST", "/v1/audio/transcriptions", body)
	req.Header.Set("Content-Type", ct)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("code=%d want 502 body=%s", rec.Code, rec.Body)
	}
	if calls != 1 {
		t.Errorf("15053 是请求级错误，不应换号：calls=%d", calls)
	}
	if !strings.Contains(rec.Body.String(), "WAV") || !strings.Contains(rec.Body.String(), "15053") {
		t.Errorf("body=%s 应带 15053 的容器格式 hint", rec.Body)
	}
}

// TestTranscriptionsDisabledForbidden 开关默认关闭 ⇒ 403 + asr_disabled，不打上游。
func TestTranscriptionsDisabledForbidden(t *testing.T) {
	calls := 0
	h := audioTestHandler(false, false, func(r *http.Request) (*http.Response, error) {
		calls++
		return jsonResponse(200, `{"code":0,"text":"x"}`), nil
	}, nil)
	body, ct := multipartAudio(t, "file", "a.wav", []byte{1})
	req := httptest.NewRequest("POST", "/v1/audio/transcriptions", body)
	req.Header.Set("Content-Type", ct)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("code=%d want 403", rec.Code)
	}
	if calls != 0 {
		t.Errorf("关闭时不得打上游，实际 %d 次", calls)
	}
	if !strings.Contains(rec.Body.String(), "asr_disabled") {
		t.Errorf("body=%s 应带 asr_disabled", rec.Body)
	}
}

// TestTranscriptionsTooLarge 音频超过 3MB 上游硬上限：入口 413，不打上游。
func TestTranscriptionsTooLarge(t *testing.T) {
	calls := 0
	h := audioTestHandler(false, true, func(r *http.Request) (*http.Response, error) {
		calls++
		return jsonResponse(200, `{"code":0}`), nil
	}, nil)
	body, ct := multipartAudio(t, "file", "big.wav", make([]byte, maxAudioBytes+1))
	req := httptest.NewRequest("POST", "/v1/audio/transcriptions", body)
	req.Header.Set("Content-Type", ct)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("code=%d want 413 body=%s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "audio_too_large") {
		t.Errorf("body=%s 应带 audio_too_large", rec.Body)
	}
	if calls != 0 {
		t.Errorf("超限不得打上游，实际 %d 次", calls)
	}
}

// countingReader 统计实际被读取的字节数（断言「读盘之前就钳住」）。
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// zeroReader 无限零字节流：模拟一个 64MB 的超大上传体，不真正分配 64MB 内存。
type zeroReader struct{ remaining int64 }

func (z *zeroReader) Read(p []byte) (int, error) {
	if z.remaining <= 0 {
		return 0, io.EOF
	}
	if int64(len(p)) > z.remaining {
		p = p[:z.remaining]
	}
	for i := range p {
		p[i] = 0
	}
	z.remaining -= int64(len(p))
	return len(p), nil
}

// TestTranscriptionsBodyClampedBeforeSpool 64MB 原始体必须在落盘/吃满内存之前被
// MaxBytesReader 钳住：实际读取字节数 ≤ 3MB + 64KiB + 1，且回 413。
func TestTranscriptionsBodyClampedBeforeSpool(t *testing.T) {
	h := audioTestHandler(false, true, func(r *http.Request) (*http.Response, error) {
		return jsonResponse(200, `{"code":0}`), nil
	}, nil)
	const boundary = "audio-clamp-boundary"
	prefix := "--" + boundary + "\r\n" +
		`Content-Disposition: form-data; name="file"; filename="big.wav"` + "\r\n" +
		"Content-Type: audio/wav\r\n\r\n"
	cr := &countingReader{r: io.MultiReader(strings.NewReader(prefix), &zeroReader{remaining: 64 << 20})}
	req := httptest.NewRequest("POST", "/v1/audio/transcriptions", cr)
	req.Header.Set("Content-Type", "multipart/form-data; boundary="+boundary)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("code=%d want 413 body=%s", rec.Code, rec.Body)
	}
	if limit := int64(maxAudioBytes + audioBodySlack + 1); cr.n > limit {
		t.Errorf("读取了 %d 字节，超过钳制上限 %d（读盘前必须已截断）", cr.n, limit)
	}
}

// TestSpeechLedgerRow 成功请求必须落请求台账：path / 账号 / 模型 / 结果。
func TestSpeechLedgerRow(t *testing.T) {
	log := reqlog.New(reqlog.Config{})
	h := audioTestHandler(true, false, func(r *http.Request) (*http.Response, error) {
		return speechSSEResponse([]byte{1, 2}), nil
	}, log)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/audio/speech",
		strings.NewReader(`{"input":"hi"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	recent := log.Snapshot().Recent
	if len(recent) != 1 {
		t.Fatalf("台账行数=%d want 1", len(recent))
	}
	e := recent[0]
	if e.Path != "/v1/audio/speech" {
		t.Errorf("path=%q", e.Path)
	}
	if e.Model != upstream.TTSModel {
		t.Errorf("model=%q want %q", e.Model, upstream.TTSModel)
	}
	if e.Account == "" {
		t.Errorf("账号列不得为空")
	}
	if e.Status != http.StatusOK || e.Outcome != reqlog.OutcomeSuccess {
		t.Errorf("status=%d outcome=%q", e.Status, e.Outcome)
	}
}

// TestTranscriptionsFailureLedger 失败请求也要落台账（status 与 outcome 一致），
// 否则面板里 ASR 的失败完全不可见。
func TestTranscriptionsFailureLedger(t *testing.T) {
	log := reqlog.New(reqlog.Config{})
	h := audioTestHandler(false, true, func(r *http.Request) (*http.Response, error) {
		return jsonResponse(http.StatusBadGateway, `{"msg":"boom"}`), nil
	}, log)
	body, ct := multipartAudio(t, "file", "a.wav", []byte{1, 2, 3})
	req := httptest.NewRequest("POST", "/v1/audio/transcriptions", body)
	req.Header.Set("Content-Type", ct)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("code=%d want 502 body=%s", rec.Code, rec.Body)
	}
	recent := log.Snapshot().Recent
	if len(recent) != 1 {
		t.Fatalf("台账行数=%d want 1", len(recent))
	}
	if recent[0].Status != http.StatusBadGateway || recent[0].Outcome != reqlog.OutcomeHTTPError {
		t.Errorf("status=%d outcome=%q", recent[0].Status, recent[0].Outcome)
	}
}
