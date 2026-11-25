package main

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	mathrand "math/rand/v2"
	"os"
	"path/filepath"
	"sync"
)

type lobbyClip struct {
	ID         string `json:"id"`
	SourceRoom string `json:"source_room"`
	Topic      string `json:"topic"`
	Owner      string `json:"owner"`
	Author     string `json:"author"`
	Text       string `json:"text,omitempty"`
	PCM        []byte `json:"pcm_s16le"`
}

type lobbyPool struct {
	mu    sync.RWMutex
	dir   string
	clips []lobbyClip
}

func openLobbyPool(dir string) (*lobbyPool, error) {
	dir = filepath.Join(dir, "clips")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create voice pool: %w", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("list voice pool: %w", err)
	}
	p := &lobbyPool{dir: dir, clips: make([]lobbyClip, 0, len(entries))}
	for _, entry := range entries {
		if !entry.Type().IsRegular() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			return nil, fmt.Errorf("read saved voice %q: %w", entry.Name(), err)
		}
		var clip lobbyClip
		if err := json.Unmarshal(data, &clip); err != nil {
			return nil, fmt.Errorf("decode saved voice %q: %w", entry.Name(), err)
		}
		p.clips = append(p.clips, clip)
	}
	return p, nil
}

// take 取一条同语境里别人的原声：跳过本局已经听过的和自己的声音，
// 历史伙伴应当来自以前一起玩过的伙伴，而不是自己。
func (p *lobbyPool) take(topic, requester string, used map[string]bool) (lobbyClip, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	others := make([]lobbyClip, 0, len(p.clips))
	for _, clip := range p.clips {
		if clip.Topic != topic || len(clip.PCM) == 0 || used[clip.ID] || clip.Owner == requester {
			continue
		}
		others = append(others, clip)
	}
	if len(others) == 0 {
		return lobbyClip{}, false
	}
	return others[mathrand.IntN(len(others))], true
}

func (p *lobbyPool) save(clip lobbyClip) error {
	data, err := json.Marshal(clip)
	if err != nil {
		return fmt.Errorf("encode voice: %w", err)
	}
	tmp := filepath.Join(p.dir, rand.Text()+".part")
	final := filepath.Join(p.dir, clip.ID+".json")
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("stage voice: %w", removeStagedVoice(tmp, err))
	}
	if err := os.Rename(tmp, final); err != nil {
		return fmt.Errorf("publish voice: %w", removeStagedVoice(tmp, err))
	}
	p.mu.Lock()
	p.clips = append([]lobbyClip{clip}, p.clips...)
	p.mu.Unlock()
	return nil
}

func removeStagedVoice(path string, original error) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return errors.Join(original, err)
	}
	return original
}
