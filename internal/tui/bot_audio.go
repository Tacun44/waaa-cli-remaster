package tui

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/codebyoketch/wa-cli/internal/msgstore"
	"github.com/codebyoketch/wa-cli/internal/whatsapp"
)

const deepgramListenURL = "https://api.deepgram.com/v1/listen?model=nova-3&language=es&smart_format=true"
const maxBotAudioBytes = 20 << 20

func transcribeBotAudio(ctx context.Context, client *whatsapp.Client, ms *msgstore.Store, jid, messageID string) (string, error) {
	if messageID == "" {
		return "", fmt.Errorf("audio sin id")
	}
	msg, found, err := ms.Get(jid, messageID)
	if err != nil {
		return "", err
	}
	if !found || msg.FromMe || (msg.MediaType != "voice note" && msg.MediaType != "audio") {
		return "", fmt.Errorf("audio recibido no encontrado en el chat activo")
	}
	if transcript := strings.TrimSpace(msg.Transcript); transcript != "" {
		return transcript, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	data, contentType, err := client.DownloadMedia(ctx, msg)
	if err != nil {
		return "", err
	}
	if len(data) == 0 || len(data) > maxBotAudioBytes {
		return "", fmt.Errorf("audio vacío o mayor de 20 MiB")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("localizar carpeta del usuario: %w", err)
	}
	keyPath := filepath.Join(home, ".config", "jarvis", "deepgram.key")
	key, err := os.ReadFile(keyPath)
	if err != nil {
		return "", fmt.Errorf("leer clave de Deepgram: %w", err)
	}
	transcript, err := transcribeDeepgram(ctx, data, contentType, strings.TrimSpace(string(key)), deepgramListenURL, http.DefaultClient)
	if err != nil {
		return "", err
	}
	if err := ms.SetTranscript(jid, messageID, transcript); err != nil {
		return "", fmt.Errorf("guardar transcripción: %w", err)
	}
	return transcript, nil
}

func transcribeDeepgram(ctx context.Context, audio []byte, contentType, key, endpoint string, client *http.Client) (string, error) {
	if key == "" {
		return "", fmt.Errorf("clave de Deepgram vacía")
	}
	if parsed, _, err := mime.ParseMediaType(contentType); err == nil {
		contentType = parsed
	}
	if !strings.HasPrefix(contentType, "audio/") {
		contentType = "application/octet-stream"
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(audio))
	if err != nil {
		return "", err
	}
	request.Header.Set("Authorization", "Token "+key)
	request.Header.Set("Content-Type", contentType)
	response, err := client.Do(request)
	if err != nil {
		return "", fmt.Errorf("Deepgram no disponible: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("Deepgram devolvió HTTP %d", response.StatusCode)
	}
	var result struct {
		Results struct {
			Channels []struct {
				Alternatives []struct {
					Transcript string `json:"transcript"`
				} `json:"alternatives"`
			} `json:"channels"`
		} `json:"results"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&result); err != nil {
		return "", fmt.Errorf("respuesta inválida de Deepgram: %w", err)
	}
	if len(result.Results.Channels) == 0 || len(result.Results.Channels[0].Alternatives) == 0 {
		return "", fmt.Errorf("Deepgram no devolvió alternativas")
	}
	transcript := strings.TrimSpace(result.Results.Channels[0].Alternatives[0].Transcript)
	if transcript == "" {
		return "", fmt.Errorf("Deepgram no detectó voz")
	}
	return transcript, nil
}
