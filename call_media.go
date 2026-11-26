package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/qiniu/go-sdk/v7/storagev2/uploader"
	"github.com/qiniu/go-sdk/v7/storagev2/uptoken"
)

const (
	callUploadPrefix = "story-relay-call"
	callUploadPath   = "/v1/upload-token?is_private=true&prefix=" + callUploadPrefix
	maxCallAudio     = 4 << 20
	callPlaybackLead = "adelay=4000:all=1"
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
	return m.prepareAudio(ctx, ref, false)
}

func (m callMedia) preparePlayback(ctx context.Context, ref callVoice) (string, error) {
	return m.prepareAudio(ctx, ref, true)
}

// prepareStoryPlayback combines all prior recordings into one playback action.
func (m callMedia) prepareStoryPlayback(ctx context.Context, voices []callVoice) (string, error) {
	if len(voices) == 0 {
		return "", fmt.Errorf("story has no recordings")
	}
	dir, err := os.MkdirTemp("", "story-chain-audio-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)
	args := []string{"-hide_banner", "-loglevel", "error", "-nostdin"}
	for i, voice := range voices {
		source, err := m.history.signed(ctx, voice.URL)
		if err != nil {
			return "", err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, source, nil)
		if err != nil {
			return "", err
		}
		resp, err := m.history.api.http.Do(req)
		if err != nil {
			return "", fmt.Errorf("download story recording: %w", err)
		}
		data, readErr := io.ReadAll(io.LimitReader(resp.Body, maxCallAudio+1))
		closeBody(resp.Body)
		if resp.StatusCode != http.StatusOK {
			return "", fmt.Errorf("story recording returned HTTP %d", resp.StatusCode)
		}
		if readErr != nil {
			return "", readErr
		}
		if len(data) == 0 || len(data) > maxCallAudio {
			return "", fmt.Errorf("invalid story recording size")
		}
		path := filepath.Join(dir, strconv.Itoa(i)+".mp3")
		if err := os.WriteFile(path, data, 0o600); err != nil {
			return "", err
		}
		args = append(args, "-i", path)
	}
	var filter strings.Builder
	for i := range voices {
		fmt.Fprintf(&filter, "[%d:a]", i)
	}
	fmt.Fprintf(&filter, "concat=n=%d:v=0:a=1,%s[out]", len(voices), callPlaybackLead)
	args = append(args, "-filter_complex", filter.String(), "-map", "[out]", "-map_metadata", "-1", "-ac", "1", "-ar", "24000", "-c:a", "libmp3lame", "-b:a", "48k", "-f", "mp3", "pipe:1")
	output, err := exec.CommandContext(ctx, m.ffmpeg, args...).Output()
	if err != nil || len(output) == 0 {
		return "", fmt.Errorf("combine story recordings: %w", err)
	}
	object, err := m.upload(ctx, "story-"+rand.Text()+".mp3", output)
	if err != nil {
		return "", err
	}
	signed, err := m.history.signed(ctx, object)
	if err != nil {
		return "", err
	}
	if err := m.history.checkMP3(ctx, signed); err != nil {
		return "", err
	}
	return signed, nil
}

func (m callMedia) prepareAudio(ctx context.Context, ref callVoice, delay bool) (string, error) {
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
	mp3, err := encodeCallMP3(ctx, m.ffmpeg, original, delay)
	if err != nil {
		return "", err
	}
	filename := ref.Turn + ".mp3"
	if delay {
		filename = ref.Turn + "-playback.mp3"
	}
	return m.upload(ctx, filename, mp3)
}

func encodeCallMP3(ctx context.Context, ffmpeg string, original []byte, delay bool) ([]byte, error) {
	args := []string{
		"-hide_banner", "-loglevel", "error", "-nostdin", "-protocol_whitelist", "pipe",
		"-i", "pipe:0", "-vn", "-map_metadata", "-1",
	}
	if delay {
		args = append(args, "-af", callPlaybackLead)
	}
	args = append(args, "-ac", "1", "-ar", "24000", "-c:a", "libmp3lame", "-b:a", "48k", "-f", "mp3", "pipe:1")
	command := exec.CommandContext(ctx, ffmpeg, args...)
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
