package context

import (
	"fmt"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/tiktoken-go/tokenizer"
)

const (
	DefaultModelTokenBudget = 9500
	MaxModelTokenBudget     = 16000
)

var (
	codecOnce sync.Once
	codec     tokenizer.Codec
	codecErr  error
)

func o200kCodec() (tokenizer.Codec, error) {
	codecOnce.Do(func() {
		codec, codecErr = tokenizer.Get(tokenizer.O200kBase)
	})
	return codec, codecErr
}

func CountTokens(s string) int {
	if enc, err := o200kCodec(); err == nil {
		if n, e := enc.Count(s); e == nil {
			return n
		}
	}
	return (len(s) + 3) / 4
}

type TruncateInfo struct {
	Truncated      bool
	OriginalBytes  int
	ReturnedBytes  int
	OriginalTokens int
	ReturnedTokens int
	OriginalLines  int
	OmittedTokens  int
	OmittedLines   int
}

func countLines(s string) int {
	if s == "" {
		return 0
	}
	return strings.Count(s, "\n") + 1
}

func TruncateText(s string, budget int) (string, TruncateInfo) {
	if budget <= 0 {
		budget = DefaultModelTokenBudget
	}
	if budget > MaxModelTokenBudget {
		budget = MaxModelTokenBudget
	}

	info := TruncateInfo{
		OriginalBytes:  len(s),
		OriginalTokens: CountTokens(s),
		OriginalLines:  countLines(s),
	}

	if info.OriginalTokens <= budget {
		info.ReturnedBytes = info.OriginalBytes
		info.ReturnedTokens = info.OriginalTokens
		return s, info
	}
	info.Truncated = true

	const markerReserve = 400
	halfBudget := (budget - markerReserve) / 2

	lines := strings.Split(s, "\n")
	if len(lines) <= 1 {
		return truncateNoLines(s, budget, info)
	}

	lineTokens := make([]int, len(lines))
	for i, line := range lines {
		lineTokens[i] = CountTokens(line + "\n")
	}

	headLines, headTokens := 0, 0
	for headLines < len(lines) && headTokens < halfBudget {
		headTokens += lineTokens[headLines]
		headLines++
	}

	tailStart := len(lines)
	tailTokens := 0
	for tailStart > headLines && tailTokens < halfBudget {
		tailStart--
		tailTokens += lineTokens[tailStart]
	}

	headText := strings.Join(lines[:headLines], "\n")
	tailText := strings.Join(lines[tailStart:], "\n")
	omittedLines := tailStart - headLines
	omittedTokens := info.OriginalTokens - headTokens - tailTokens
	if omittedTokens < 0 {
		omittedTokens = 0
	}

	marker := fmt.Sprintf("\n\n... %d tokens omitted (%d lines) ...\n\n", omittedTokens, omittedLines)
	header := fmt.Sprintf("[output truncated: original ~%d tokens / %d lines / %d bytes]\n\n", info.OriginalTokens, info.OriginalLines, info.OriginalBytes)
	result := header + headText + marker + tailText

	info.ReturnedBytes = len(result)
	info.ReturnedTokens = CountTokens(result)
	info.OmittedTokens = omittedTokens
	info.OmittedLines = omittedLines

	if info.ReturnedTokens > budget {
		return truncateNoLines(s, budget, info)
	}
	return result, info
}

func truncateNoLines(s string, budget int, info TruncateInfo) (string, TruncateInfo) {
	if len(s) == 0 {
		return s, info
	}
	totalTokens := info.OriginalTokens
	if totalTokens <= 0 {
		totalTokens = CountTokens(s)
	}
	tokensPerByte := float64(totalTokens) / float64(len(s))
	targetBytes := int(float64(max(budget-200, 1)) / tokensPerByte)
	if targetBytes < 1 {
		targetBytes = 1
	}

	half := targetBytes / 2
	if half < 1 {
		half = 1
	}

	header := fmt.Sprintf("[output truncated: original ~%d tokens / %d lines / %d bytes]\n\n", info.OriginalTokens, info.OriginalLines, info.OriginalBytes)

	var out string
	omittedTokens := 0
	for attempt := 0; attempt < 8; attempt++ {
		head := cutRuneSafe(s, half)
		tail := cutRuneSafeBack(s, half)
		omittedTokens = info.OriginalTokens - CountTokens(head) - CountTokens(tail)
		if omittedTokens < 0 {
			omittedTokens = 0
		}
		marker := fmt.Sprintf("\n\n... %d tokens omitted ...\n\n", omittedTokens)
		out = header + head + marker + tail
		if CountTokens(out) <= budget || half < 2 {
			break
		}
		half = half * 3 / 4
	}

	info.OmittedTokens = omittedTokens
	info.ReturnedBytes = len(out)
	info.ReturnedTokens = CountTokens(out)
	return out, info
}

func cutRuneSafe(s string, n int) string {
	if n >= len(s) {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

func cutRuneSafeBack(s string, n int) string {
	if n >= len(s) {
		return s
	}
	start := len(s) - n
	for start < len(s) && !utf8.RuneStart(s[start]) {
		start++
	}
	return s[start:]
}

func clampBudget(budget int) int {
	if budget <= 0 {
		budget = DefaultModelTokenBudget
	}
	if budget > MaxModelTokenBudget {
		budget = MaxModelTokenBudget
	}
	return budget
}

func ModelLimit(content any, budget int) any {
	budget = clampBudget(budget)
	switch v := content.(type) {
	case string:
		out, _ := TruncateText(v, budget)
		return out
	default:
		return content
	}
}
