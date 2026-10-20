package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

const (
	defaultListenAddr = "127.0.0.1:50021"
	defaultUtauURL    = "http://127.0.0.1:8080"
	defaultRenderer   = "utautts-world-phrase"
	maxRequestBytes   = 1 << 20   // 1 MiB
	maxAudioBytes     = 128 << 20 // 128 MiB
	maxErrorBodyBytes = 4 << 10   // 4 KiB
)

type Config struct {
	ListenAddr         string
	UtauURL            string
	Renderer           string
	IntonationStrength int
}

type Voicebank struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type VoicebankResponse struct {
	Voicebanks []Voicebank `json:"voicebanks"`
}

type Speaker struct {
	Name        string  `json:"name"`
	SpeakerUUID string  `json:"speaker_uuid"`
	Styles      []Style `json:"styles"`
	Version     string  `json:"version"`
}

type Style struct {
	Name string `json:"name"`
	ID   int    `json:"id"`
}

// AudioQuery はYMM4/VOICEVOXとの互換性を確認するための簡易モデル。
// UtauTTSへ送る文章は kana を使用する。VOICEVOXの完全な音素解析結果ではない。
type AudioQuery struct {
	AccentPhrases      []any   `json:"accent_phrases"`
	SpeedScale         float64 `json:"speedScale"`
	PitchScale         float64 `json:"pitchScale"`
	IntonationScale    float64 `json:"intonationScale"`
	VolumeScale        float64 `json:"volumeScale"`
	PrePhonemeLength   float64 `json:"prePhonemeLength"`
	PostPhonemeLength  float64 `json:"postPhonemeLength"`
	OutputSamplingRate int     `json:"outputSamplingRate"`
	OutputStereo       bool    `json:"outputStereo"`
	Kana               string  `json:"kana"`
}

// Mora はVOICEVOX互換レスポンス用の最小モーラ情報。
// UtauTTSの公開HTTP APIからVOICEVOX形式のアクセント解析結果は取得できないため、
// /accent_phrases では入力文字を保持する互換用データを返す。
type Mora struct {
	Text            string   `json:"text"`
	Consonant       *string  `json:"consonant"`
	ConsonantLength *float64 `json:"consonant_length"`
	Vowel           string   `json:"vowel"`
	VowelLength     float64  `json:"vowel_length"`
	Pitch           float64  `json:"pitch"`
}

type AccentPhrase struct {
	Moras           []Mora `json:"moras"`
	Accent          int    `json:"accent"`
	PauseMora       *Mora  `json:"pause_mora"`
	IsInterrogative bool   `json:"is_interrogative"`
}

type SynthesisRequest struct {
	Text               string `json:"text"`
	VoicebankID        string `json:"voicebank_id"`
	Renderer           string `json:"renderer"`
	IntonationStrength int    `json:"intonation_strength"`
}

type App struct {
	cfg    Config
	client *http.Client
	logger *log.Logger
}

var requestCounter uint64

func main() {
	logger := log.New(os.Stdout, "", log.LstdFlags|log.Lmicroseconds)

	cfg, err := loadConfig()
	if err != nil {
		logger.Fatal(err)
	}

	app := &App{
		cfg:    cfg,
		logger: logger,
		client: newHTTPClient(),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/docs", app.docsHandler)
	mux.HandleFunc("/openapi.json", app.openapiHandler)
	mux.HandleFunc("/version", app.versionHandler)
	mux.HandleFunc("/is_initialized", app.isInitializedHandler)
	mux.HandleFunc("/is_initialized_speaker", app.isInitializedSpeakerHandler)
	mux.HandleFunc("/initialize_speaker", app.initializeSpeakerHandler)
	mux.HandleFunc("/speakers", app.speakersHandler)
	mux.HandleFunc("/audio_query", app.audioQueryHandler)
	mux.HandleFunc("/accent_phrases", app.accentPhrasesHandler)
	mux.HandleFunc("/synthesis", app.synthesisHandler)
	mux.HandleFunc("/", app.notFoundHandler)

	server := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           app.loggingMiddleware(app.recoveryMiddleware(mux)),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    1 << 20,
		// WriteTimeoutは長い音声合成レスポンスを途中で切らないよう設定しない。
	}

	logger.Printf("起動: http://%s", cfg.ListenAddr)
	logger.Printf("UtauTTS API: %s", cfg.UtauURL)
	logger.Printf("Renderer: %s / IntonationStrength: %d",
		cfg.Renderer, cfg.IntonationStrength)

	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Fatal(err)
	}
}

func loadConfig() (Config, error) {
	cfg := Config{
		ListenAddr:         envOr("PROXY_LISTEN_ADDR", defaultListenAddr),
		UtauURL:            strings.TrimRight(envOr("UTAUTTS_URL", defaultUtauURL), "/"),
		Renderer:           envOr("UTAUTTS_RENDERER", defaultRenderer),
		IntonationStrength: 4,
	}

	if raw := os.Getenv("UTAUTTS_INTONATION_STRENGTH"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 || n > 10 {
			return Config{}, fmt.Errorf("UTAUTTS_INTONATION_STRENGTH は0〜10の整数にしてください")
		}
		cfg.IntonationStrength = n
	}

	u, err := url.Parse(cfg.UtauURL)
	if err != nil || u.Scheme == "" || u.Host == "" ||
		(u.Scheme != "http" && u.Scheme != "https") {
		return Config{}, fmt.Errorf("UTAUTTS_URL が不正です: %q", cfg.UtauURL)
	}
	if strings.TrimSpace(cfg.Renderer) == "" {
		return Config{}, errors.New("UTAUTTS_RENDERER は空にできません")
	}
	return cfg, nil
}

func envOr(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

func newHTTPClient() *http.Client {
	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   10,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
	}
	return &http.Client{
		Transport: transport,
		Timeout:   3 * time.Minute,
	}
}

type statusWriter struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (w *statusWriter) WriteHeader(status int) {
	if w.status != 0 {
		return
	}
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *statusWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	n, err := w.ResponseWriter.Write(p)
	w.bytes += n
	return n, err
}

func (a *App) loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := atomic.AddUint64(&requestCounter, 1)
		r.Header.Set("X-Request-ID", strconv.FormatUint(id, 10))
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w}
		sw.Header().Set("X-Request-ID", strconv.FormatUint(id, 10))

		next.ServeHTTP(sw, r)

		status := sw.status
		if status == 0 {
			status = http.StatusOK
		}
		a.logger.Printf("HTTP id=%d method=%s path=%s status=%d bytes=%d duration=%s remote=%s",
			id, r.Method, r.URL.Path, status, sw.bytes, time.Since(start).Round(time.Millisecond), r.RemoteAddr)
	})
}

func (a *App) recoveryMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if recovered := recover(); recovered != nil {
				a.logger.Printf("PANIC method=%s path=%s value=%v", r.Method, r.URL.Path, recovered)
				http.Error(w, "internal server error", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func (a *App) docsHandler(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = io.WriteString(w, `<!doctype html>
<html lang="ja"><meta charset="utf-8"><title>UtauTTS VOICEVOX compatibility proxy</title>
<h1>UtauTTS VOICEVOX compatibility proxy</h1>
<p>これはYMM4接続用の互換プロキシです。VOICEVOXエンジンの全APIを実装しているわけではありません。</p>
<ul>
<li>GET /speakers</li><li>GET /version</li><li>GET /is_initialized</li>
<li>GET /is_initialized_speaker?speaker=0</li><li>POST /initialize_speaker?speaker=0</li>
<li>POST /audio_query?text=こんにちは&amp;speaker=0</li><li>POST /accent_phrases?text=こんにちは&amp;speaker=0</li><li>POST /synthesis?speaker=0</li>
</ul></html>`)
}

func (a *App) openapiHandler(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	writeJSON(w, map[string]any{
		"openapi": "3.0.0",
		"info": map[string]string{
			"title":   "UtauTTS VOICEVOX compatibility proxy",
			"version": "0.2.0",
		},
		"paths": map[string]any{
			"/speakers":       map[string]any{"get": map[string]string{"summary": "話者一覧"}},
			"/audio_query":    map[string]any{"post": map[string]string{"summary": "簡易AudioQuery生成"}},
			"/accent_phrases": map[string]any{"post": map[string]string{"summary": "VOICEVOX互換アクセント句レスポンス（簡易）"}},
			"/synthesis":      map[string]any{"post": map[string]string{"summary": "UtauTTS音声合成"}},
		},
	})
}

func (a *App) versionHandler(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	writeJSON(w, map[string]string{"version": "0.2.0"})
}

func (a *App) isInitializedHandler(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.cfg.UtauURL+"/api/health", nil)
	if err != nil {
		a.writeError(w, http.StatusInternalServerError, "health_request_creation_failed", err.Error())
		return
	}
	resp, err := a.client.Do(req)
	if err != nil {
		a.logger.Printf("UPSTREAM health error: %v", err)
		writeJSON(w, false)
		return
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxErrorBodyBytes))
	writeJSON(w, resp.StatusCode >= 200 && resp.StatusCode < 300)
}

func (a *App) isInitializedSpeakerHandler(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	speakerID, err := parseSpeakerID(r)
	if err != nil {
		writeJSON(w, false)
		return
	}
	banks, err := a.loadVoicebanks(r.Context())
	if err != nil {
		a.logger.Printf("VOICEBANKS error: %v", err)
		a.writeError(w, http.StatusBadGateway, "voicebank_lookup_failed", err.Error())
		return
	}
	writeJSON(w, speakerID < len(banks))
}

func (a *App) initializeSpeakerHandler(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	speakerID, err := parseSpeakerID(r)
	if err != nil {
		a.writeError(w, http.StatusBadRequest, "invalid_speaker", err.Error())
		return
	}
	banks, err := a.loadVoicebanks(r.Context())
	if err != nil {
		a.logger.Printf("VOICEBANKS error: %v", err)
		a.writeError(w, http.StatusBadGateway, "voicebank_lookup_failed", err.Error())
		return
	}
	if speakerID >= len(banks) {
		a.writeError(w, http.StatusBadRequest, "unknown_speaker", fmt.Sprintf("speaker=%d", speakerID))
		return
	}
	// UtauTTSにはVOICEVOXのような話者ロードAPIがないため、
	// 音源IDの存在を確認したうえで204を返す。
	a.logger.Printf("SPEAKER initialized proxy-only speaker=%d voicebank_id=%q",
		speakerID, banks[speakerID].ID)
	w.WriteHeader(http.StatusNoContent)
}

func (a *App) speakersHandler(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	banks, err := a.loadVoicebanks(r.Context())
	if err != nil {
		a.logger.Printf("VOICEBANKS error: %v", err)
		a.writeError(w, http.StatusBadGateway, "voicebank_lookup_failed", err.Error())
		return
	}

	speakers := make([]Speaker, 0, len(banks))
	for i, bank := range banks {
		if strings.TrimSpace(bank.ID) == "" {
			a.logger.Printf("VOICEBANKS warning: index=%d has empty id", i)
		}
		speakers = append(speakers, Speaker{
			Name:        bank.Name,
			SpeakerUUID: stableUUID(bank.ID, i),
			Styles:      []Style{{Name: "ノーマル", ID: i}},
			Version:     "1.0.0",
		})
	}
	writeJSON(w, speakers)
}

func (a *App) audioQueryHandler(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	text := r.URL.Query().Get("text")
	speakerID, err := parseSpeakerID(r)
	if err != nil {
		a.writeError(w, http.StatusBadRequest, "invalid_speaker", err.Error())
		return
	}

	// YMM4が起動時に空文字で問い合わせるケースがあるため、
	// audio_queryでは空文字を400にせず、空のクエリを返す。
	// 実際の音声合成では空文字を拒否する。
	query := AudioQuery{
		AccentPhrases:      []any{},
		SpeedScale:         1.0,
		PitchScale:         0.0,
		IntonationScale:    1.0,
		VolumeScale:        1.0,
		PrePhonemeLength:   0.1,
		PostPhonemeLength:  0.1,
		OutputSamplingRate: 24000,
		OutputStereo:       false,
		Kana:               text,
	}
	a.logger.Printf("AUDIO_QUERY speaker=%d text_len=%d text=%q",
		speakerID, len([]rune(text)), text)
	writeJSON(w, query)
}

// accentPhrasesHandler はYMM4が呼び出すVOICEVOX互換エンドポイント。
// 注意: UtauTTSの公開APIにVOICEVOX形式のアクセント句解析APIはないため、
// ここでは入力文字を失わないための簡易プレースホルダーを返す。
// 実際の音声合成は /synthesis から元のテキストをUtauTTSへ渡して行う。
func (a *App) accentPhrasesHandler(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	text := r.URL.Query().Get("text")
	speakerID, err := parseSpeakerID(r)
	if err != nil {
		a.writeError(w, http.StatusBadRequest, "invalid_speaker", err.Error())
		return
	}
	if strings.TrimSpace(text) == "" {
		a.logger.Printf("ACCENT_PHRASES speaker=%d text_len=0 result=empty", speakerID)
		writeJSON(w, []AccentPhrase{})
		return
	}

	// VOICEVOXのMora形式を保ちつつ、各Unicode文字を1単位として返す。
	// 音韻・アクセントを解析しているわけではないため、ピッチは0、長さは0.1秒の仮値。
	runes := []rune(text)
	moras := make([]Mora, 0, len(runes))
	for _, ch := range runes {
		if ch == ' ' || ch == '\t' || ch == '\n' || ch == '\r' {
			continue
		}
		vowel := vowelForKana(ch)
		moras = append(moras, Mora{
			Text:        string(ch),
			Consonant:   nil,
			Vowel:       vowel,
			VowelLength: 0.1,
			Pitch:       0,
		})
	}
	if len(moras) == 0 {
		writeJSON(w, []AccentPhrase{})
		return
	}
	phrase := AccentPhrase{
		Moras:           moras,
		Accent:          len(moras),
		PauseMora:       nil,
		IsInterrogative: strings.HasSuffix(strings.TrimSpace(text), "？") || strings.HasSuffix(strings.TrimSpace(text), "?"),
	}
	a.logger.Printf("ACCENT_PHRASES speaker=%d text_len=%d moras=%d approximate=true text=%q",
		speakerID, len(runes), len(moras), text)
	writeJSON(w, []AccentPhrase{phrase})
}

// kanaの母音を推定する簡易テーブル。漢字など読みを判定できない文字は "a" を返す。
// この値は互換レスポンス用で、正確な発音解析には使用しない。
func vowelForKana(ch rune) string {
	c, ok := katakanaToHiragana(ch)
	if !ok {
		return "a"
	}
	groups := map[string]string{
		"あかさたなはまやらわがざだばぱぁゃ": "a",
		"いきしちにひみりぎじぢびぴぃ":    "i",
		"うくすつぬふむゆるぐずづぶぷぅゅ":  "u",
		"えけせてねへめれげぜでべぺぇ":    "e",
		"おこそとのほもよろをごぞどぼぽぉょ": "o",
	}
	for chars, vowel := range groups {
		if strings.ContainsRune(chars, c) {
			return vowel
		}
	}
	return "a"
}

func katakanaToHiragana(ch rune) (rune, bool) {
	if ch >= 'ぁ' && ch <= 'ゖ' {
		return ch, true
	}
	if ch >= 'ァ' && ch <= 'ヶ' {
		return ch - 0x60, true
	}
	return 0, false
}

func (a *App) synthesisHandler(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	speakerID, err := parseSpeakerID(r)
	if err != nil {
		a.writeError(w, http.StatusBadRequest, "invalid_speaker", err.Error())
		return
	}

	var query AudioQuery
	decoder := json.NewDecoder(io.LimitReader(r.Body, maxRequestBytes+1))
	if err := decoder.Decode(&query); err != nil {
		a.writeError(w, http.StatusBadRequest, "invalid_audio_query_json", err.Error())
		return
	}
	// VOICEVOX互換クライアントによっては kana が省略されることがある。
	text := strings.TrimSpace(query.Kana)
	if text == "" {
		a.writeError(w, http.StatusBadRequest, "empty_text", "AudioQuery.kana is empty")
		return
	}

	banks, err := a.loadVoicebanks(r.Context())
	if err != nil {
		a.logger.Printf("VOICEBANKS error: %v", err)
		a.writeError(w, http.StatusBadGateway, "voicebank_lookup_failed", err.Error())
		return
	}
	if speakerID >= len(banks) {
		a.writeError(w, http.StatusBadRequest, "unknown_speaker", fmt.Sprintf("speaker=%d", speakerID))
		return
	}
	bank := banks[speakerID]
	if strings.TrimSpace(bank.ID) == "" {
		a.writeError(w, http.StatusBadGateway, "empty_voicebank_id",
			fmt.Sprintf("voicebank at index %d has no id", speakerID))
		return
	}

	payload := SynthesisRequest{
		Text:               text,
		VoicebankID:        bank.ID,
		Renderer:           a.cfg.Renderer,
		IntonationStrength: a.cfg.IntonationStrength,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		a.writeError(w, http.StatusInternalServerError, "json_encode_failed", err.Error())
		return
	}

	endpoint := a.cfg.UtauURL + "/api/synthesize/audio"
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		a.writeError(w, http.StatusInternalServerError, "upstream_request_creation_failed", err.Error())
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "audio/wav, application/octet-stream")

	start := time.Now()
	a.logger.Printf("SYNTHESIS start request_id=%s speaker=%d voicebank_id=%q renderer=%q text_len=%d text=%q",
		r.Header.Get("X-Request-ID"), speakerID, bank.ID, payload.Renderer, len([]rune(text)), text)

	resp, err := a.client.Do(req)
	if err != nil {
		a.logger.Printf("SYNTHESIS upstream_error duration=%s err=%v", time.Since(start).Round(time.Millisecond), err)
		a.writeError(w, http.StatusBadGateway, "utautts_request_failed", err.Error())
		return
	}
	defer resp.Body.Close()

	a.logger.Printf("SYNTHESIS upstream_response status=%s content_type=%q content_length=%q duration=%s",
		resp.Status, resp.Header.Get("Content-Type"), resp.Header.Get("Content-Length"),
		time.Since(start).Round(time.Millisecond))

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes))
		a.logger.Printf("SYNTHESIS upstream_error_body=%q", string(detail))
		a.writeError(w, http.StatusBadGateway, "utautts_synthesis_failed",
			fmt.Sprintf("UtauTTS returned %s: %s", resp.Status, strings.TrimSpace(string(detail))))
		return
	}

	// 先に上限付きで全データを読み、空データやWAVでない応答を検出する。
	audio, err := io.ReadAll(io.LimitReader(resp.Body, maxAudioBytes+1))
	if err != nil {
		a.writeError(w, http.StatusBadGateway, "audio_read_failed", err.Error())
		return
	}
	if len(audio) > maxAudioBytes {
		a.writeError(w, http.StatusBadGateway, "audio_too_large",
			fmt.Sprintf("audio response exceeds %d bytes", maxAudioBytes))
		return
	}
	if err := validateWAV(audio); err != nil {
		a.logger.Printf("SYNTHESIS invalid_audio bytes=%d err=%v", len(audio), err)
		a.writeError(w, http.StatusBadGateway, "invalid_wav", err.Error())
		return
	}

	w.Header().Set("Content-Type", "audio/wav")
	w.Header().Set("Content-Length", strconv.Itoa(len(audio)))
	w.Header().Set("Content-Disposition", "inline")
	w.Header().Set("X-Audio-Bytes", strconv.Itoa(len(audio)))
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(audio); err != nil {
		a.logger.Printf("SYNTHESIS client_write_error err=%v", err)
		return
	}
	a.logger.Printf("SYNTHESIS success bytes=%d duration=%s", len(audio), time.Since(start).Round(time.Millisecond))
}

func validateWAV(data []byte) error {
	if len(data) < 12 {
		return fmt.Errorf("WAV response too short: %d bytes", len(data))
	}
	if string(data[0:4]) != "RIFF" || string(data[8:12]) != "WAVE" {
		return fmt.Errorf("response does not begin with RIFF....WAVE; first bytes=%q", data[:min(len(data), 16)])
	}
	return nil
}

func (a *App) loadVoicebanks(ctx context.Context) ([]Voicebank, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.cfg.UtauURL+"/api/voicebanks", nil)
	if err != nil {
		return nil, err
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET /api/voicebanks: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes))
		return nil, fmt.Errorf("GET /api/voicebanks returned %s: %s",
			resp.Status, strings.TrimSpace(string(body)))
	}
	var result VoicebankResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxRequestBytes)).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode /api/voicebanks: %w", err)
	}
	if result.Voicebanks == nil {
		return nil, errors.New("/api/voicebanks response has no 'voicebanks' array")
	}
	// IDが返る順番に依存しないよう、音源IDでソートして話者番号を安定させる。
	sort.SliceStable(result.Voicebanks, func(i, j int) bool {
		return result.Voicebanks[i].ID < result.Voicebanks[j].ID
	})
	return result.Voicebanks, nil
}

func parseSpeakerID(r *http.Request) (int, error) {
	raw := r.URL.Query().Get("speaker")
	if raw == "" {
		return 0, errors.New("speaker query parameter is required")
	}
	id, err := strconv.Atoi(raw)
	if err != nil || id < 0 {
		return 0, fmt.Errorf("speaker must be a non-negative integer: %q", raw)
	}
	return id, nil
}

func stableUUID(id string, index int) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s#%d", id, index)))
	// UUID形式の見た目を維持しつつ、同じIDとindexから同じ値を生成する。
	b := sum[:16]
	b[6] = (b[6] & 0x0f) | 0x50
	b[8] = (b[8] & 0x3f) | 0x80
	hexValue := hex.EncodeToString(b)
	return fmt.Sprintf("%s-%s-%s-%s-%s",
		hexValue[0:8], hexValue[8:12], hexValue[12:16], hexValue[16:20], hexValue[20:32])
}

func (a *App) notFoundHandler(w http.ResponseWriter, r *http.Request) {
	a.writeError(w, http.StatusNotFound, "not_found", "unknown endpoint: "+r.URL.Path)
}

func requireMethod(w http.ResponseWriter, r *http.Request, method string) bool {
	if r.Method == method {
		return true
	}
	w.Header().Set("Allow", method)
	http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	return false
}

func (a *App) writeError(w http.ResponseWriter, status int, code, message string) {
	a.logger.Printf("ERROR status=%d code=%s message=%q", status, code, message)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"error":   code,
		"message": message,
	})
}

func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		log.Printf("JSON response encode failed: %v", err)
	}
}
