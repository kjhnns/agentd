package media

import (
	"strconv"
	"strings"
	"unicode"
)

// Hallucination guard (OPS-81). Speech-to-text models invent text on silent,
// noisy or music-only audio: a few words repeated ("GERRARD STREET GERRARD
// STREET"), a caption credit ("Thanks for watching"), or a language the user
// never speaks. Such a transcript must not reach the agent as the user's
// words. Unreliable returns why a transcript looks invented, "" when it looks
// like real speech.

// knownLanguages are the languages the user speaks. whisper-1 reports full
// names ("english"); ISO codes are accepted too. An empty language (the
// gpt-4o models report none) is not judged.
var knownLanguages = map[string]bool{
	"english": true, "german": true, "portuguese": true, "spanish": true,
	"en": true, "de": true, "pt": true, "es": true,
}

// stockPhrases are caption credits the models learned from subtitled video.
var stockPhrases = []string{
	"thanks for watching", "thank you for watching", "please subscribe",
	"subscribe to my channel", "amara.org", "untertitel im auftrag",
	"untertitelung", "ご視聴ありがとうございました", "チャンネル登録",
}

// Unreliable judges a transcript of an audio clip of durationS seconds (0 =
// unknown).
func Unreliable(text, language string, durationS int) string {
	low := strings.ToLower(text)
	words := strings.FieldsFunc(low, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsNumber(r) && r != '\''
	})
	n := len(words)
	// slow: under one word a second, where real speech runs 2 to 3. Language
	// and repetition only count together with it, so a fluent French note or
	// "call Mom, call Dad, call Mom" said at normal pace still passes.
	slow := durationS > 0 && n < durationS
	lang := strings.ToLower(strings.TrimSpace(language))
	if lang != "" && !knownLanguages[lang] && (slow || durationS == 0 && n <= 12) {
		return "detected language " + lang
	}
	// A caption credit counts only when it is (nearly) the whole transcript,
	// not when the user talks about one.
	if n <= 8 {
		for _, p := range stockPhrases {
			if strings.Contains(low, p) {
				return "stock caption phrase \"" + p + "\""
			}
		}
	}
	if n == 0 {
		if durationS >= 5 {
			return "empty transcript"
		}
		return ""
	}
	// Normal speech is about 2 to 3 words a second; fewer than one word per
	// 10 seconds on a long clip is noise that came back as a few words.
	if durationS >= 20 && n*10 < durationS {
		return "only " + strconv.Itoa(n) + " words for " + strconv.Itoa(durationS) + "s of audio"
	}
	// Short text made of few distinct words, each said again and again,
	// slowly.
	if slow && n >= 6 && n <= 40 {
		uniq := map[string]bool{}
		for _, w := range words {
			uniq[w] = true
		}
		if len(uniq)*2 <= n {
			return "repetitive text"
		}
	}
	// Long text: one phrase looping (the classic decoder loop).
	if n >= 12 {
		counts := map[string]int{}
		top := 0
		for i := 0; i+3 <= n; i++ {
			k := words[i] + " " + words[i+1] + " " + words[i+2]
			counts[k]++
			if counts[k] > top {
				top = counts[k]
			}
		}
		// The looped phrase covers at least 30% of the words.
		if top >= 4 && top*10 >= n {
			return "one phrase repeated " + strconv.Itoa(top) + " times"
		}
	}
	return ""
}
