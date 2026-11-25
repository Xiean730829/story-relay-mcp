package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"

	"github.com/qiniu/go-sdk/v7/storagev2/uploader"
	"github.com/qiniu/go-sdk/v7/storagev2/uptoken"
)

const (
	callUploadPrefix = "story-relay-call"
	callUploadPath   = "/v1/upload-token?is_private=true&prefix=" + callUploadPrefix
	maxCallAudio     = 4 << 20
)

type callMedia struct {
	history callHistory
	ffmpeg  string
}

func newCallMedia(history callHistory) (callMedia, error) {
	program := strings.TrimSpace(os.Getenv("STORY_CALL_FFMPEG"))
	if program == "" {
		program = "ffmpeg"
	}
	path, err := exec.LookPath(program)
	if err != nil {
		return callMedia{}, fmt.Errorf("单人电话验收需要 FFmpeg 转换录音，请设置 STORY_CALL_FFMPEG: %w", err)
	}
	return callMedia{history: history, ffmpeg: path}, nil
}

func (m callMedia) prepare(ctx context.Context, ref callVoice) (string, error) {
	source, err := m.history.signed(ctx, ref.URL)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, source, nil)
	if err != nil {
		return "", fmt.Errorf("prepare original audio download")
	}
	resp, err := m.history.api.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("download original audio failed")
	}
	defer closeBody(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download original audio returned HTTP %d", resp.StatusCode)
	}
	original, err := io.ReadAll(io.LimitReader(resp.Body, maxCallAudio+1))
	if err != nil {
		return "", fmt.Errorf("read original audio failed")
	}
	if len(original) > maxCallAudio {
		return "", fmt.Errorf("original audio exceeds call demo limit")
	}
	mp3, err := encodeCallMP3(ctx, m.ffmpeg, original)
	if err != nil {
		return "", err
	}
	return m.upload(ctx, ref.Turn+".mp3", mp3)
}

func encodeCallMP3(ctx context.Context, ffmpeg string, original []byte) ([]byte, error) {
	command := exec.CommandContext(ctx, ffmpeg,
		"-hide_banner", "-loglevel", "error", "-nostdin", "-protocol_whitelist", "pipe",
		"-i", "pipe:0", "-vn", "-map_metadata", "-1", "-ac", "1", "-ar", "24000",
		"-c:a", "libmp3lame", "-b:a", "48k", "-f", "mp3", "pipe:1",
	)
	command.Stdin = bytes.NewReader(original)
	data, err := command.Output()
	if err != nil {
		return nil, fmt.Errorf("encode original audio as MP3: %w", err)
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("encode original audio returned an empty file")
	}
	return data, nil
}

func (m callMedia) upload(ctx context.Context, filename string, audio []byte) (string, error) {
	var upload struct {
		Token  string `json:"token"`
		Domain string `json:"domain"`
	}
	err := m.history.api.request(ctx, http.MethodGet, callUploadPath, nil, &upload)
	if err != nil {
		return "", fmt.Errorf("request private audio upload: %w", err)
	}
	domain := strings.TrimSpace(upload.Domain)
	if !strings.Contains(domain, "://") {
		domain = "https://" + domain
	}
	base, err := url.Parse(domain)
	if err != nil || base.Scheme != "https" || base.Host == "" || upload.Token == "" {
		return "", fmt.Errorf("missing private audio upload details")
	}
	form := uploader.NewFormUploader(nil)
	var uploaded struct {
		Key string `json:"key"`
	}
	if err := form.UploadReader(
		ctx,
		bytes.NewReader(audio),
		&uploader.ObjectOptions{
			UpToken: uptoken.NewParser(upload.Token), FileName: filename, ContentType: "audio/mpeg",
		},
		&uploaded,
	); err != nil {
		return "", fmt.Errorf("upload private MP3 failed")
	}
	if !strings.HasPrefix(uploaded.Key, callUploadPrefix+"/") {
		return "", fmt.Errorf("private MP3 upload returned an unexpected key")
	}
	return base.JoinPath(uploaded.Key).String(), nil
}
