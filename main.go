package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	listenAddr = "127.0.0.1:50021"
	utauURL    = "http://127.0.0.1:8080"
)

// UtauTTSから受け取る音源情報
type Voicebank struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type VoicebankResponse struct {
	Voicebanks []Voicebank `json:"voicebanks"`
}

// VOICEVOX互換の話者情報
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

// VOICEVOX AudioQueryの簡易版
type AudioQuery struct {
	AccentPhrases      []any   `json:"accentPhrases"`
	SpeedScale         float64 `json:"speedScale"`
	PitchScale         float64 `json:"pitchScale"`
	IntonationScale    float64 `json:"intonationScale"`
	VolumeScale        float64 `json:"volumeScale"`
	PrePhonemeLength   float64 `json:"prePhonemeLength"`
	PostPhonemeLength  float64 `json:"postPhonemeLength"`
	OutputSamplingRate int     `json:"outputSamplingRate"`
	OutputStereo       bool    `json:"outputStereo"`

	// 互換性検証用。元の文章を保持する
	Kana string `json:"kana"`
}

type SynthesisRequest struct {
	Text        string `json:"text"`
	VoicebankID string `json:"voicebank_id"`
	Renderer    string `json:"renderer"`
}

var client = &http.Client{
	Timeout: 3 * time.Minute,
}

// UtauTTSのAPIへGETリクエストを送る
func getUtauJSON(path string, target any) error {
	resp, err := client.Get(utauURL + path)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("UtauTTS returned HTTP %d", resp.StatusCode)
	}

	return json.NewDecoder(resp.Body).Decode(target)
}

// UtauTTSの音源一覧を取得
func loadVoicebanks() ([]Voicebank, error) {
	var result VoicebankResponse
	err := getUtauJSON("/api/voicebanks", &result)
	return result.Voicebanks, err
}

// VOICEVOXの話者一覧API
func speakersHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	banks, err := loadVoicebanks()
	if err != nil {
		http.Error(w, "UtauTTS voicebank lookup failed",
			http.StatusBadGateway)
		return
	}

	speakers := make([]Speaker, 0, len(banks))

	for i, bank := range banks {
		// IDは一覧の順番に割り当てる。
		// 本番版ではIDと音源の対応を安定させること。
		speakers = append(speakers, Speaker{
			Name: bank.Name,
			SpeakerUUID: fmt.Sprintf(
				"00000000-0000-4000-8000-%012d", i+1),
			Styles: []Style{
				{Name: "ノーマル", ID: i},
			},
			Version: "1.0.0",
		})
	}

	writeJSON(w, speakers)
}

// VOICEVOXの /audio_query
func audioQueryHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	text := r.URL.Query().Get("text")
	if strings.TrimSpace(text) == "" {
		http.Error(w, "text is required", http.StatusBadRequest)
		return
	}

	// テキストをAudioQueryのkanaに保持する簡易方式
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

	writeJSON(w, query)
}

// VOICEVOXの /synthesis
func synthesisHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	speakerID, err := strconv.Atoi(r.URL.Query().Get("speaker"))
	if err != nil || speakerID < 0 {
		http.Error(w, "invalid speaker", http.StatusBadRequest)
		return
	}

	// AudioQuery JSONを読み取る
	var query AudioQuery
	if err := json.NewDecoder(
		io.LimitReader(r.Body, 1<<20),
	).Decode(&query); err != nil {
		http.Error(w, "invalid AudioQuery JSON", http.StatusBadRequest)
		return
	}

	if strings.TrimSpace(query.Kana) == "" {
		http.Error(w, "AudioQuery.kana is empty",
			http.StatusBadRequest)
		return
	}

	// 話者IDをUtauTTSのvoicebank_idに変換
	banks, err := loadVoicebanks()
	if err != nil {
		http.Error(w, "UtauTTS is unavailable",
			http.StatusBadGateway)
		return
	}

	if speakerID >= len(banks) {
		http.Error(w, "unknown speaker",
			http.StatusBadRequest)
		return
	}

	payload := SynthesisRequest{
		Text:        query.Kana,
		VoicebankID: banks[speakerID].ID,
		Renderer:    "utautts-world-phrase",
	}

	body, err := json.Marshal(payload)
	if err != nil {
		http.Error(w, "JSON encoding failed",
			http.StatusInternalServerError)
		return
	}

	// UtauTTSへ合成リクエストを送る
	endpoint := utauURL + "/api/synthesize/audio"
	req, err := http.NewRequestWithContext(
		r.Context(), http.MethodPost,
		endpoint, bytes.NewReader(body),
	)
	if err != nil {
		http.Error(w, "request creation failed",
			http.StatusInternalServerError)
		return
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		http.Error(w, "UtauTTS request failed",
			http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		http.Error(w, "UtauTTS synthesis failed",
			http.StatusBadGateway)
		return
	}

	// WAVをそのままYMM4側へ返す
	w.Header().Set("Content-Type", "audio/wav")
	w.Header().Set("Content-Disposition", "inline")
	w.WriteHeader(http.StatusOK)

	if _, err := io.Copy(w, resp.Body); err != nil {
		log.Printf("WAV response write failed: %v", err)
	}
}

// JSONレスポンスを返す共通関数
func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		log.Printf("JSON response failed: %v", err)
	}
}

// VOICEVOXの話者初期化状態を返す
func initializedSpeakerHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	speakerID, err := strconv.Atoi(r.URL.Query().Get("speaker"))
	if err != nil || speakerID < 0 {
		writeJSON(w, false)
		return
	}

	banks, err := loadVoicebanks()
	if err != nil {
		http.Error(w, "UtauTTS unavailable",
			http.StatusBadGateway)
		return
	}

	// 話者IDが一覧に存在するか確認する
	initialized := speakerID < len(banks)

	// JSONの真偽値を返す。文字列の "true" ではない
	writeJSON(w, initialized)
}

func main() {

	http.HandleFunc("/is_initialized", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		// 試作版では、リクエストごとにUtauTTSの稼働状態を確認する
		resp, err := client.Get(utauURL + "/api/health")
		if err != nil {
			writeJSON(w, false)
			return
		}
		defer resp.Body.Close()

		writeJSON(w, resp.StatusCode == http.StatusOK)
	})

	// URLを正規化するための検査
	if _, err := url.ParseRequestURI(utauURL); err != nil {
		log.Fatal(err)
	}

	http.HandleFunc("/speakers", speakersHandler)
	http.HandleFunc("/audio_query", audioQueryHandler)
	http.HandleFunc("/synthesis", synthesisHandler)
	http.HandleFunc("/is_initialized_speaker", initializedSpeakerHandler)

	// バージョン確認用
	http.HandleFunc("/version", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]string{"version": "0.1.0"})
	})

	log.Printf("VOICEVOX compatibility server: http://%s", listenAddr)

	log.Fatal(http.ListenAndServe(listenAddr,
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			log.Printf(
				"REQUEST: %s %s",
				r.Method,
				r.URL.RequestURI(),
			)

			http.DefaultServeMux.ServeHTTP(w, r)
		}),
	))
}
