package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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
	callClipPrefix       = "voice-"
	maxCallCandidates    = 12
	callRecentSceneLines = 3
)

type callVoice struct {
	Turn         string    `json:"turn_key"`
	Marker       string    `json:"history_marker,omitempty"`
	Text         string    `json:"text"`
	URL          string    `json:"audio_url,omitempty"`
	PlaybackURL  string    `json:"playback_url,omitempty"`
	Story        string    `json:"story,omitempty"`
	Title        string    `json:"title,omitempty"`
	Before       string    `json:"before,omitempty"`
	CapturedAt   time.Time `json:"captured_at,omitempty"`
	ChainPending bool      `json:"chain_pending,omitempty"`
	ChainID      string    `json:"chain_id,omitempty"`
	ChainVersion int       `json:"chain_version,omitempty"`
	ChainTheme   string    `json:"chain_theme,omitempty"`
}

func isCallGameControl(text string) bool {
	command := normalizedCallText(text)
	for _, prefix := range []string{"我还是想", "我想要", "我想", "我要", "请帮我", "帮我", "请"} {
		if remainder, found := strings.CutPrefix(command, prefix); found {
			command = remainder
			break
		}
	}
	for _, prefix := range []string{"退出游戏", "结束游戏", "停止游戏", "换个游戏", "匹配在线", "换成历史伙伴", "用历史录音玩"} {
		if strings.HasPrefix(command, prefix) {
			return true
		}
	}
	if command == "不玩了" {
		return true
	}
	partner, found := strings.CutPrefix(command, "和")
	if !found {
		partner, found = strings.CutPrefix(command, "跟")
	}
	if !found {
		return false
	}
	for _, prefix := range []string{"以前的朋友", "以前的小伙伴", "以前的伙伴", "之前的小伙伴", "历史伙伴", "你", "ai", "buddy"} {
		if activity, found := strings.CutPrefix(partner, prefix); found {
			activity = strings.TrimPrefix(activity, "一起")
			return activity == "玩" || strings.HasPrefix(activity, "玩故事接龙")
		}
	}
	return false
}

type callArchive struct {
	Latest callVoice `json:"latest"`
	Saved  callVoice `json:"saved"`
}

type callPool struct {
	mu             sync.RWMutex
	dir            string
	voices         map[string]callVoice
	byTitle        map[string]callTitleIndex
	legacyArchives map[string]callArchive
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
	p := &callPool{
		dir: dir, voices: make(map[string]callVoice), byTitle: make(map[string]callTitleIndex),
		legacyArchives: make(map[string]callArchive),
	}
	for _, entry := range entries {
		name := entry.Name()
		if filepath.Ext(name) != ".json" || !entry.Type().IsRegular() {
			continue
		}
		key := strings.TrimSuffix(name, ".json")
		index := strings.HasPrefix(name, callClipPrefix)
		if !index {
			if _, err := hex.DecodeString(key); len(key) != sha256.Size*2 || err != nil {
				continue
			}
		}
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return nil, fmt.Errorf("read call voice index: %w", err)
		}
		if !index {
			var archive callArchive
			if err := json.Unmarshal(data, &archive); err != nil {
				return nil, fmt.Errorf("decode legacy call voice index: %w", err)
			}
			p.legacyArchives[key] = archive
			continue
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
		p.indexVoice(voice)
	}
	return p, nil
}

func (p *callPool) save(voice callVoice) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if existing, ok := p.voices[voice.Turn]; ok {
		if voice.URL == "" {
			voice.URL = existing.URL
		}
		if voice.PlaybackURL == "" && voice.URL == existing.URL {
			voice.PlaybackURL = existing.PlaybackURL
		}
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
	p.indexVoice(voice)
	return nil
}

func (p *callPool) voice(turn string) (callVoice, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	voice, ok := p.voices[turn]
	return voice, ok
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

func (p *callPool) candidates(current callVoice, used map[string]bool, earlier []storyLine) []callVoice {
	title := callTitleKey(current.Title)
	terms := callSceneQuery(current, earlier)
	if title == "" {
		return []callVoice{}
	}

	p.mu.RLock()
	defer p.mu.RUnlock()

	type scoredVoice struct {
		voice callVoice
		score int
	}
	child := normalizedCallText(current.Text)
	eligible := func(voice callVoice) bool {
		sameGame := voice.Story != "" && voice.Story == current.Story
		reply := normalizedCallText(voice.Text)
		return !sameGame && !used[voice.Turn] && reply != "" && reply != child && voice.URL != "" && !isCallGameControl(voice.Text)
	}

	index := p.byTitle[title]
	matches := make(map[string]callSceneMatches)
	for term := range terms {
		for turn, evidence := range index[term] {
			match := matches[turn]
			match.add(evidence)
			matches[turn] = match
		}
	}
	picks := []scoredVoice{}
	for _, voice := range p.voices {
		if callTitleKey(voice.Title) == title && eligible(voice) {
			picks = append(picks, scoredVoice{voice: voice, score: matches[voice.Turn].score()})
		}
	}

	// Legacy archives are keyed by title but may omit it inside the record.
	legacyTitles := []string{title}
	if scenario := callStoryByTitle[title]; scenario != nil {
		for _, alias := range scenario.Aliases {
			legacyTitles = append(legacyTitles, normalizedCallText(alias))
		}
	}
	legacyTurns := make(map[string]bool)
	for _, legacyTitle := range legacyTitles {
		for _, voice := range p.legacy(legacyTitle) {
			_, alreadyIndexed := p.voices[voice.Turn]
			wrongTitle := voice.Title != "" && callTitleKey(voice.Title) != title
			skip := alreadyIndexed || legacyTurns[voice.Turn] || wrongTitle || voice.URL == ""
			if skip || !eligible(voice) || isCallGameControl(voice.Text) {
				continue
			}
			legacyTurns[voice.Turn] = true
			recordingTerms := callSceneTerms(voice)
			var match callSceneMatches
			for term := range terms {
				match.add(recordingTerms[term])
			}
			voice.Title = current.Title
			picks = append(picks, scoredVoice{voice: voice, score: match.score()})
		}
	}

	mathrand.Shuffle(len(picks), func(i, j int) { picks[i], picks[j] = picks[j], picks[i] })
	slices.SortStableFunc(picks, func(a, b scoredVoice) int { return b.score - a.score })
	candidates := make([]callVoice, 0, len(picks))
	for _, pick := range picks {
		candidates = append(candidates, pick.voice)
	}
	return candidates
}

func (p *callPool) unresolved() []callVoice {
	p.mu.RLock()
	defer p.mu.RUnlock()
	voices := make([]callVoice, 0, len(p.voices))
	for _, voice := range p.voices {
		if voice.URL == "" || voice.ChainPending {
			voices = append(voices, voice)
		}
	}
	slices.SortFunc(voices, func(a, b callVoice) int {
		return b.CapturedAt.Compare(a.CapturedAt)
	})
	return voices
}

func (p *callPool) pendingChains() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	count := 0
	for _, voice := range p.voices {
		if voice.ChainPending {
			count++
		}
	}
	return count
}

type callAudioCounts struct {
	Ready   int `json:"ready"`
	Pending int `json:"pending"`
}

func (p *callPool) counts() (callAudioCounts, map[string]callAudioCounts) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	byTitle := make(map[string]callAudioCounts, len(callStoryScenarios))
	for _, scenario := range callStoryScenarios {
		byTitle[scenario.Title] = callAudioCounts{}
	}
	var total callAudioCounts
	for _, voice := range p.voices {
		if voice.URL != "" {
			total.Ready++
		}
		scenario := callStoryByTitle[normalizedCallText(voice.Title)]
		if scenario == nil {
			continue
		}
		counts := byTitle[scenario.Title]
		if voice.URL == "" {
			counts.Pending++
		} else {
			counts.Ready++
		}
		byTitle[scenario.Title] = counts
	}
	total.Pending = len(p.voices) - total.Ready
	return total, byTitle
}

func (p *callPool) legacy(title string) []callVoice {
	voices := []callVoice{}
	sum := sha256.Sum256([]byte(title))
	archive := p.legacyArchives[hex.EncodeToString(sum[:])]
	if archive.Saved.Turn != "" {
		voices = append(voices, archive.Saved)
	}
	if archive.Latest.Turn != "" && archive.Latest.Turn != archive.Saved.Turn {
		voices = append(voices, archive.Latest)
	}
	return voices
}

func callBigrams(text string) map[string]struct{} {
	letters := []rune(text)
	result := make(map[string]struct{}, max(len(letters)-1, 0))
	for i := 1; i < len(letters); i++ {
		result[string(letters[i-1:i+1])] = struct{}{}
	}
	return result
}
