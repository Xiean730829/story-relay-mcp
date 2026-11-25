package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	mathrand "math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

const (
	callClipPrefix     = "voice-"
	maxCallCandidates  = 4
	minCallRelatedness = 2
)

type callVoice struct {
	Turn       string    `json:"turn_key"`
	Marker     string    `json:"history_marker,omitempty"`
	Text       string    `json:"text"`
	URL        string    `json:"audio_url,omitempty"`
	Story      string    `json:"story,omitempty"`
	Title      string    `json:"title,omitempty"`
	Before     string    `json:"before,omitempty"`
	CapturedAt time.Time `json:"captured_at,omitempty"`
}

// automaticCallReplies only accepts close wording to the Buddy's drafted reply.
// Less certain candidates still need the model to judge their story context.
func automaticCallReplies(candidates []callVoice, reply string) []string {
	replyText := normalizedCallText(reply)
	replyPairs := callBigrams(replyText)
	if len(replyPairs) < 3 || strings.ContainsAny(reply, "?？") {
		return nil
	}
	ids := []string{}
	for _, voice := range candidates {
		text := normalizedCallText(voice.Text)
		pairs := callBigrams(text)
		if voice.URL == "" || len(pairs) < 3 || strings.ContainsAny(voice.Text, "?？") {
			continue
		}
		// Opposite statements can share almost all their characters.
		if strings.Contains(replyText, "不") != strings.Contains(text, "不") ||
			strings.Contains(replyText, "没") != strings.Contains(text, "没") {
			continue
		}
		overlap := callOverlap(replyPairs, text)
		if 10*2*overlap >= 7*(len(replyPairs)+len(pairs)) {
			ids = append(ids, voice.Turn)
		}
	}
	return ids
}

type callArchive struct {
	Latest callVoice `json:"latest"`
	Saved  callVoice `json:"saved"`
}

type callPool struct {
	mu     sync.RWMutex
	dir    string
	voices map[string]callVoice
}

func openCallPool(root, agent string) (*callPool, error) {
	sum := sha256.Sum256([]byte(agent))
	dir := filepath.Join(root, "call-demo", hex.EncodeToString(sum[:]))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create private call pool: %w", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read private call pool: %w", err)
	}
	p := &callPool{dir: dir, voices: make(map[string]callVoice)}
	for _, entry := range entries {
		name := entry.Name()
		index := strings.HasPrefix(name, callClipPrefix) && filepath.Ext(name) == ".json"
		if !index || !entry.Type().IsRegular() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return nil, fmt.Errorf("read call voice index: %w", err)
		}
		var voice callVoice
		if err := json.Unmarshal(data, &voice); err != nil {
			return nil, fmt.Errorf("decode call voice index: %w", err)
		}
		if voice.CapturedAt.IsZero() {
			info, err := entry.Info()
			if err != nil {
				return nil, fmt.Errorf("read call voice timestamp: %w", err)
			}
			voice.CapturedAt = info.ModTime()
		}
		p.voices[voice.Turn] = voice
	}
	return p, nil
}

func (p *callPool) save(voice callVoice) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if existing, ok := p.voices[voice.Turn]; ok && existing.URL != "" && voice.URL == "" {
		voice.URL = existing.URL
	}
	if voice.CapturedAt.IsZero() {
		voice.CapturedAt = time.Now()
	}
	data, err := json.Marshal(voice)
	if err != nil {
		return err
	}
	sum := sha256.Sum256([]byte(voice.Turn))
	final := filepath.Join(p.dir, callClipPrefix+hex.EncodeToString(sum[:])+".json")
	tmp := filepath.Join(p.dir, rand.Text()+".part")
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return removeStagedVoice(tmp, err)
	}
	if err := os.Rename(tmp, final); err != nil {
		return removeStagedVoice(tmp, err)
	}
	p.voices[voice.Turn] = voice
	return nil
}

func (p *callPool) opening(title string) (callVoice, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	var selected callVoice
	for _, voice := range p.voices {
		if normalizedCallText(voice.Title) != normalizedCallText(title) || voice.Before == "" {
			continue
		}
		if selected.Turn == "" || voice.CapturedAt.After(selected.CapturedAt) {
			selected = voice
		}
	}
	return selected, selected.Turn != ""
}

func (p *callPool) candidates(current callVoice, used map[string]bool) ([]callVoice, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	type scoredVoice struct {
		voice callVoice
		score int
	}
	title := normalizedCallText(current.Title)
	child := normalizedCallText(current.Text)
	words := callBigrams(child)
	picks := []scoredVoice{}
	for _, voice := range p.voices {
		sameGame := voice.Story == current.Story
		repeated := normalizedCallText(voice.Text) == child
		if sameGame || used[voice.Turn] || repeated {
			continue
		}
		otherTitle := normalizedCallText(voice.Title)
		titleOverlap := callOverlap(callBigrams(otherTitle), title)
		contextOverlap := callOverlap(words, normalizedCallText(voice.Before))
		replyOverlap := callOverlap(words, normalizedCallText(voice.Text))
		if otherTitle != title && max(titleOverlap, contextOverlap, replyOverlap) < minCallRelatedness {
			continue
		}
		picks = append(picks, scoredVoice{
			voice: voice,
			score: 2*(contextOverlap+replyOverlap) + titleOverlap,
		})
	}

	// Old phone demos stored only the latest/cached voice per title. Keep those
	// recordings eligible under that title and let the story judge continuity.
	legacy, err := p.legacy(title)
	for _, voice := range legacy {
		_, alreadyIndexed := p.voices[voice.Turn]
		if alreadyIndexed || used[voice.Turn] || normalizedCallText(voice.Text) == child {
			continue
		}
		voice.Title = current.Title
		picks = append(picks, scoredVoice{voice: voice})
	}

	mathrand.Shuffle(len(picks), func(i, j int) { picks[i], picks[j] = picks[j], picks[i] })
	slices.SortStableFunc(picks, func(a, b scoredVoice) int { return b.score - a.score })
	picks = picks[:min(len(picks), maxCallCandidates)]
	candidates := make([]callVoice, 0, len(picks))
	for _, pick := range picks {
		candidates = append(candidates, pick.voice)
	}
	return candidates, err
}

func (p *callPool) unresolved() []callVoice {
	p.mu.RLock()
	defer p.mu.RUnlock()
	voices := make([]callVoice, 0, len(p.voices))
	for _, voice := range p.voices {
		if voice.URL == "" {
			voices = append(voices, voice)
		}
	}
	slices.SortFunc(voices, func(a, b callVoice) int {
		return b.CapturedAt.Compare(a.CapturedAt)
	})
	return voices
}

func (p *callPool) counts() (int, int) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	var ready int
	for _, voice := range p.voices {
		if voice.URL != "" {
			ready++
		}
	}
	return ready, len(p.voices) - ready
}

func (p *callPool) legacy(title string) ([]callVoice, error) {
	voices := []callVoice{}
	sum := sha256.Sum256([]byte(title))
	path := filepath.Join(p.dir, hex.EncodeToString(sum[:])+".json")
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return voices, nil
	}
	if err != nil {
		return voices, err
	}
	var archive callArchive
	if err := json.Unmarshal(data, &archive); err != nil {
		return voices, err
	}
	if archive.Saved.Turn != "" {
		voices = append(voices, archive.Saved)
	}
	if archive.Latest.Turn != "" && archive.Latest.Turn != archive.Saved.Turn {
		voices = append(voices, archive.Latest)
	}
	return voices, nil
}

func callBigrams(text string) map[string]struct{} {
	letters := []rune(text)
	result := make(map[string]struct{}, max(len(letters)-1, 0))
	for i := 1; i < len(letters); i++ {
		result[string(letters[i-1:i+1])] = struct{}{}
	}
	return result
}

func callOverlap(bigrams map[string]struct{}, text string) int {
	var count int
	for bigram := range bigrams {
		if strings.Contains(text, bigram) {
			count++
		}
	}
	return count
}
