package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// speechRoundTripFunc 假 transport：测试**零网络**（不用 httptest，不开监听）。
type speechRoundTripFunc func(*http.Request) (*http.Response, error)

func (f speechRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// speechTestClient 构造指向假 transport 的 Client（CN 域，零值 GlobalEnabled）。
func speechTestClient(t *testing.T, handle speechRoundTripFunc) *Client {
	t.Helper()
	return &Client{
		HTTP:          &http.Client{Transport: handle},
		ChatBaseCN:    "https://cn.example",
		BillingBaseCN: "https://cn.example",
	}
}

// speechSSE 包装一段 SSE 文本为 2xx 响应。
func speechSSE(events ...string) *http.Response {
	body := strings.Join(events, "\n") + "\n"
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func speechJSON(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

// TestSpeechRequestShapeAndPCM 断言 TTS 出站契约与回程 PCM 拼接：
//   - 路径 /v2/audio/speech，Accept 声明 text/event-stream（覆盖 ChatHeaders 的 json）；
//   - 请求体四要素 model/stream/speech{input,voice,response_format,sample_rate}；
//   - speech.audio.delta 的 base64 解码后按序追加，speech.audio.done 立即收尾。
func TestSpeechRequestShapeAndPCM(t *testing.T) {
	var gotPath, gotAccept string
	var gotBody map[string]any
	c := speechTestClient(t, func(r *http.Request) (*http.Response, error) {
		gotPath = r.URL.Path
		gotAccept = r.Header.Get("Accept")
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &gotBody); err != nil {
			t.Errorf("出站 body 必须是 JSON: %v", err)
		}
		return speechSSE(
			`data: {"type":"speech.audio.delta","audio":{"data":"AAEC"}}`,
			`data: {"type":"speech.audio.delta","audio":{"data":"AwQF"}}`,
			`data: {"type":"speech.audio.done"}`,
			`data: [DONE]`,
		), nil
	})
	a := &auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}
	pcm, err := c.Speech(context.Background(), a, "你好，世界", "f191_female_cn_tts2.5_flash")
	if err != nil {
		t.Fatalf("Speech: %v", err)
	}
	if gotPath != SpeechPath {
		t.Errorf("path=%q want %q", gotPath, SpeechPath)
	}
	if gotAccept != "text/event-stream" {
		t.Errorf("Accept=%q want text/event-stream", gotAccept)
	}
	if want := []byte{0, 1, 2, 3, 4, 5}; !bytes.Equal(pcm, want) {
		t.Errorf("pcm=%v want %v", pcm, want)
	}
	if gotBody["model"] != TTSModel {
		t.Errorf("model=%v want %v", gotBody["model"], TTSModel)
	}
	if stream, _ := gotBody["stream"].(bool); !stream {
		t.Errorf("stream=%v want true", gotBody["stream"])
	}
	speech, _ := gotBody["speech"].(map[string]any)
	if speech == nil {
		t.Fatalf("缺 speech 对象: %v", gotBody)
	}
	if speech["input"] != "你好，世界" {
		t.Errorf("speech.input=%v", speech["input"])
	}
	if speech["voice"] != "f191_female_cn_tts2.5_flash" {
		t.Errorf("speech.voice=%v", speech["voice"])
	}
	if speech["response_format"] != "pcm" {
		t.Errorf("speech.response_format=%v want pcm", speech["response_format"])
	}
	if rate, _ := speech["sample_rate"].(float64); int(rate) != TTSSampleRate {
		t.Errorf("speech.sample_rate=%v want %d", speech["sample_rate"], TTSSampleRate)
	}
}

// TestSpeechDefaultVoice 空 voice 必须回落官方默认音色（不是空串透传）。
func TestSpeechDefaultVoice(t *testing.T) {
	var voice string
	c := speechTestClient(t, func(r *http.Request) (*http.Response, error) {
		raw, _ := io.ReadAll(r.Body)
		var body struct {
			Speech struct {
				Voice string `json:"voice"`
			} `json:"speech"`
		}
		_ = json.Unmarshal(raw, &body)
		voice = body.Speech.Voice
		return speechSSE(`data: {"type":"speech.audio.done"}`), nil
	})
	a := &auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}
	if _, err := c.Speech(context.Background(), a, "hi", ""); err != nil {
		t.Fatalf("Speech: %v", err)
	}
	if voice != TTSDefaultVoice {
		t.Errorf("voice=%q want %q", voice, TTSDefaultVoice)
	}
}

// TestSpeechStreamErrorFrameToError SSE 里夹的错误帧必须变成带上游原因的 error，
// 不能被咽成 "completed without audio"。
func TestSpeechStreamErrorFrameToError(t *testing.T) {
	c := speechTestClient(t, func(r *http.Request) (*http.Response, error) {
		return speechSSE(
			`data: {"type":"error","error":{"message":"input too long","code":10000}}`,
			`data: [DONE]`,
		), nil
	})
	a := &auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}
	_, err := c.Speech(context.Background(), a, "hi", "")
	if err == nil {
		t.Fatal("错误帧必须返回 error")
	}
	if !strings.Contains(err.Error(), "input too long") {
		t.Errorf("err=%v 应透出上游原因", err)
	}
}

// TestSpeechTruncatedStreamIsError 只有 delta、没有 speech.audio.done 的流是**残缺音频**，
// 必须报错（返回半段 PCM 会让客户端拿到 200 + 乱码录音，issue #277）。
func TestSpeechTruncatedStreamIsError(t *testing.T) {
	c := speechTestClient(t, func(r *http.Request) (*http.Response, error) {
		return speechSSE(`data: {"type":"speech.audio.delta","audio":{"data":"AAEC"}}`), nil
	})
	a := &auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}
	_, err := c.Speech(context.Background(), a, "hi", "")
	if err == nil {
		t.Fatal("截断流必须返回 error")
	}
	if !strings.Contains(err.Error(), "truncated") || !strings.Contains(err.Error(), "3 bytes") {
		t.Errorf("err=%v 应说明截断与已收字节数", err)
	}
}

// TestSpeechEmptyStreamIsError [DONE] 直接收尾（无任何 delta）必须报错。
func TestSpeechEmptyStreamIsError(t *testing.T) {
	c := speechTestClient(t, func(r *http.Request) (*http.Response, error) {
		return speechSSE(`data: [DONE]`), nil
	})
	a := &auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}
	_, err := c.Speech(context.Background(), a, "hi", "")
	if err == nil || !strings.Contains(err.Error(), "without audio") {
		t.Fatalf("err=%v want 'without audio'", err)
	}
}

// TestTranscribeMultipartWire ASR 出站必须是 multipart，字段名 audio 且 boundary 在
// ChatHeaders 之后被重新设回（否则上游只会报 15052）。
func TestTranscribeMultipartWire(t *testing.T) {
	var gotPath, gotCT, field, filename, partCT string
	var payload []byte
	c := speechTestClient(t, func(r *http.Request) (*http.Response, error) {
		gotPath = r.URL.Path
		gotCT = r.Header.Get("Content-Type")
		raw, _ := io.ReadAll(r.Body)
		mt, params, err := mime.ParseMediaType(gotCT)
		if err != nil || mt != "multipart/form-data" {
			t.Errorf("content-type=%q 必须是 multipart/form-data: %v", gotCT, err)
			return speechJSON(http.StatusBadRequest, `{"code":15052}`), nil
		}
		mr := multipart.NewReader(bytes.NewReader(raw), params["boundary"])
		p, err := mr.NextPart()
		if err != nil {
			t.Errorf("解析 multipart: %v", err)
			return speechJSON(http.StatusBadRequest, `{"code":15052}`), nil
		}
		field, filename, partCT = p.FormName(), p.FileName(), p.Header.Get("Content-Type")
		payload, _ = io.ReadAll(p)
		return speechJSON(http.StatusOK, `{"code":0,"msg":"","text":"转写结果"}`), nil
	})
	a := &auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}
	audio := []byte{0x52, 0x49, 0x46, 0x46, 1, 2, 3}
	text, err := c.Transcribe(context.Background(), a, "probe.wav", "audio/wav", audio)
	if err != nil {
		t.Fatalf("Transcribe: %v", err)
	}
	if text != "转写结果" {
		t.Errorf("text=%q", text)
	}
	if gotPath != ASRPath {
		t.Errorf("path=%q want %q", gotPath, ASRPath)
	}
	if field != "audio" {
		t.Errorf("字段名=%q want audio（上游只认这个）", field)
	}
	if filename != "probe.wav" {
		t.Errorf("filename=%q", filename)
	}
	if partCT != "audio/wav" {
		t.Errorf("part content-type=%q", partCT)
	}
	if !bytes.Equal(payload, audio) {
		t.Errorf("音频字节被改动: %v want %v", payload, audio)
	}
}

// TestTranscribeBusinessErrorIsBadParams 上游 502 + 15053 是**请求级**错误
// （这段音频识别不出来，换号必然同样失败），必须归 ErrBadParams。
func TestTranscribeBusinessErrorIsBadParams(t *testing.T) {
	c := speechTestClient(t, func(r *http.Request) (*http.Response, error) {
		return speechJSON(http.StatusBadGateway, `{"code":15053,"msg":"ASR recognition failed"}`), nil
	})
	a := &auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}
	_, err := c.Transcribe(context.Background(), a, "a.pcm", "application/octet-stream", []byte{1, 2})
	if err == nil {
		t.Fatal("业务码非 0 必须报错")
	}
	ue, ok := err.(*Error)
	if !ok {
		t.Fatalf("err 类型=%T want *Error", err)
	}
	if ue.Kind != ErrBadParams {
		t.Errorf("kind=%v want ErrBadParams", ue.Kind)
	}
	if ue.Status != http.StatusBadGateway {
		t.Errorf("status=%d want 502", ue.Status)
	}
	if !strings.Contains(ue.Msg, "15053") {
		t.Errorf("msg=%q 应带业务码", ue.Msg)
	}
}

// TestClassifySpeechErrorForms 语音两族的分类与 hint 口径：
//   - 15053 → ErrBadParams，hint 指向「要 WAV/WebM 容器，裸 PCM 没有头可嗅探」；
//   - 500 + "get voice config failed" → ErrBadParams（音色表是全局配置，与账号无关），
//     hint 指向默认音色。
func TestClassifySpeechErrorForms(t *testing.T) {
	if got := Classify(http.StatusBadGateway, `{"code":15053,"msg":"ASR recognition failed"}`); got != ErrBadParams {
		t.Errorf("15053 kind=%v want ErrBadParams", got)
	}
	hint := GatewayHint(ErrBadParams, `{"code":15053,"msg":"ASR recognition failed"}`, HintContext{})
	if !strings.Contains(hint, "WAV") || !strings.Contains(hint, "15053") {
		t.Errorf("15053 hint=%q 应指向容器格式", hint)
	}

	voiceBody := `{"code":10000,"msg":"get voice config failed: voice x not found in Rainbow"}`
	if got := Classify(http.StatusInternalServerError, voiceBody); got != ErrBadParams {
		t.Errorf("voice config kind=%v want ErrBadParams", got)
	}
	hint = GatewayHint(ErrBadParams, voiceBody, HintContext{})
	if !strings.Contains(hint, TTSDefaultVoice) {
		t.Errorf("voice hint=%q 应给出默认音色 %q", hint, TTSDefaultVoice)
	}
}
