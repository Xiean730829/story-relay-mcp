package main

import (
	"strings"
)

// Remove common question and transition words before counting shared terms.
var callSceneFillers = strings.NewReplacer(
	"为什么", "", "怎么回事", "", "怎么办", "", "接下来", "", "什么", "", "然后", "", "到底", "",
	"怎么样", "", "在哪里", "", "在哪", "", "有吗", "", "是不是", "", "停了下来", "", "它", "", "呢", "", "呀", "", "吗", "", "那", "",
	"的", "", "了", "", "是", "",
)

func callSceneWords(text string) map[string]struct{} {
	return callBigrams(callSceneFillers.Replace(text))
}

func callSceneQuery(current callVoice, earlier []storyLine) map[string]struct{} {
	terms := make(map[string]struct{})
	lines := append([]storyLine{}, earlier...)
	lines = append(lines, storyLine{Text: current.Before}, storyLine{Text: current.Text})
	for _, line := range lines {
		text := line.Text
		for word := range callSceneWords(normalizedCallText(text)) {
			terms[word] = struct{}{}
		}
	}
	return terms
}

type callSceneEvidence uint8

const (
	callSceneBefore callSceneEvidence = 1 << iota
	callSceneReply
)

func callSceneTerms(voice callVoice) map[string]callSceneEvidence {
	reply := normalizedCallText(voice.Text)
	before := normalizedCallText(voice.Before)
	terms := make(map[string]callSceneEvidence)
	for term := range callSceneWords(reply) {
		terms[term] |= callSceneReply
	}
	for term := range callSceneWords(before) {
		terms[term] |= callSceneBefore
	}
	return terms
}

type callTitleIndex map[string]map[string]callSceneEvidence

func (p *callPool) indexVoice(voice callVoice) {
	if previous, found := p.voices[voice.Turn]; found {
		index := p.byTitle[callTitleKey(previous.Title)]
		for term := range callSceneTerms(previous) {
			delete(index[term], previous.Turn)
			if len(index[term]) == 0 {
				delete(index, term)
			}
		}
	}
	p.voices[voice.Turn] = voice
	title := callTitleKey(voice.Title)
	if title == "" || voice.URL == "" || isCallGameControl(voice.Text) {
		return
	}
	index := p.byTitle[title]
	if index == nil {
		index = make(callTitleIndex)
		p.byTitle[title] = index
	}
	for term, evidence := range callSceneTerms(voice) {
		if index[term] == nil {
			index[term] = make(map[string]callSceneEvidence)
		}
		index[term][voice.Turn] = evidence
	}
}

type callSceneMatches struct {
	reply, before int
}

func (m *callSceneMatches) add(evidence callSceneEvidence) {
	if evidence&callSceneReply != 0 {
		m.reply++
	}
	if evidence&callSceneBefore != 0 {
		m.before++
	}
}

func (m callSceneMatches) score() int {
	return 2*m.reply + m.before
}

func callTitleKey(title string) string {
	key := normalizedCallText(title)
	if scenario := callStoryByTitle[key]; scenario != nil {
		return normalizedCallText(scenario.Title)
	}
	return key
}
