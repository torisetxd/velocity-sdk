package context

import (
	"bytes"
	stdctx "context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

const (
	DefaultMaxRetainedFiles        = 5
	DefaultMaxRetainedLinesPerFile = 400
	DefaultMaxRetainedTotalChars   = 64 * 1024
	maxRetainedFileSizeBytes       = 1 * 1024 * 1024
)

type FileSpec struct {
	Path      string
	StartLine int
	EndLine   int
	HasRange  bool
}

var (
	filesToRetainBlockRe = regexp.MustCompile(`(?is)<files_to_retain>\s*(.*?)\s*</files_to_retain>`)
	lineRangeSuffixRe    = regexp.MustCompile(`:(\d+)(?:-(\d+))?$`)
	lineRangeParensRe    = regexp.MustCompile(`\s+\(\s*lines?\s+(\d+)(?:\s*-\s*(\d+))?\s*\)$`)
)

func ExtractFilesToRetainBlock(summary string) (cleanedSummary string, rawRetained string) {
	loc := filesToRetainBlockRe.FindStringSubmatchIndex(summary)
	if loc == nil {
		return strings.TrimSpace(summary), ""
	}
	rawRetained = strings.TrimSpace(summary[loc[2]:loc[3]])
	cleaned := strings.TrimSpace(summary[:loc[0]] + summary[loc[1]:])
	return cleaned, rawRetained
}

func ParseFileSpecs(raw string) []FileSpec {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	lines := strings.Split(raw, "\n")
	specs := make([]FileSpec, 0, len(lines))
	seen := make(map[string]bool)

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "//") {
			continue
		}

		line = strings.TrimPrefix(line, "- ")
		line = strings.TrimPrefix(line, "* ")
		line = strings.TrimPrefix(line, "+ ")
		line = strings.TrimPrefix(line, "• ")
		for i := 1; i <= 9; i++ {
			line = strings.TrimPrefix(line, fmt.Sprintf("%d. ", i))
		}
		line = strings.TrimPrefix(line, "File:")
		line = strings.TrimPrefix(line, "file:")
		line = strings.Trim(line, "`\"' ")

		if line == "" {
			continue
		}

		var startLine, endLine int
		hasRange := false

		if m := lineRangeParensRe.FindStringSubmatchIndex(line); m != nil {
			startLine, _ = strconv.Atoi(line[m[2]:m[3]])
			if m[4] != -1 && m[5] != -1 {
				endLine, _ = strconv.Atoi(line[m[4]:m[5]])
			}
			hasRange = true
			line = strings.TrimSpace(line[:m[0]])
		} else if m := lineRangeSuffixRe.FindStringSubmatchIndex(line); m != nil {
			startLine, _ = strconv.Atoi(line[m[2]:m[3]])
			if m[4] != -1 && m[5] != -1 {
				endLine, _ = strconv.Atoi(line[m[4]:m[5]])
			}
			hasRange = true
			line = strings.TrimSpace(line[:m[0]])
		}

		line = strings.Trim(line, "`\"' ")
		if line == "" {
			continue
		}

		cleanPath := path.Clean(strings.ReplaceAll(line, "\\", "/"))
		if seen[cleanPath] {
			continue
		}
		seen[cleanPath] = true

		specs = append(specs, FileSpec{
			Path:      cleanPath,
			StartLine: startLine,
			EndLine:   endLine,
			HasRange:  hasRange,
		})
	}

	return specs
}

func LoadRetainedFiles(ctx stdctx.Context, root string, raw string, reader FileReader) []RetainedFile {
	specs := ParseFileSpecs(raw)
	if len(specs) == 0 {
		return nil
	}

	if reader == nil {
		baseDir := strings.TrimSpace(root)
		if baseDir == "" {
			baseDir = "."
		}
		absBase, err := filepath.Abs(baseDir)
		if err != nil {
			absBase = baseDir
		}
		reader = func(_ stdctx.Context, p string) ([]byte, error) {
			var targetPath string
			if filepath.IsAbs(p) {
				targetPath = filepath.Clean(p)
			} else {
				targetPath = filepath.Clean(filepath.Join(absBase, p))
			}
			rel, err := filepath.Rel(absBase, targetPath)
			if err != nil || strings.HasPrefix(rel, "..") || rel == ".." {
				return nil, fmt.Errorf("path outside root")
			}
			info, err := os.Stat(targetPath)
			if err != nil || info.IsDir() || info.Size() == 0 || info.Size() > maxRetainedFileSizeBytes {
				return nil, fmt.Errorf("invalid file")
			}
			return os.ReadFile(targetPath)
		}
	}

	retained := make([]RetainedFile, 0, len(specs))
	totalChars := 0

	for _, spec := range specs {
		if len(retained) >= DefaultMaxRetainedFiles {
			break
		}

		data, err := reader(ctx, spec.Path)
		if err != nil || len(data) == 0 || len(data) > maxRetainedFileSizeBytes {
			continue
		}

		checkLen := len(data)
		if checkLen > 1024 {
			checkLen = 1024
		}
		if bytes.ContainsRune(data[:checkLen], 0) || !utf8.Valid(data) {
			continue
		}

		lines := strings.Split(string(data), "\n")
		totalLines := len(lines)
		if totalLines == 0 {
			continue
		}

		var selectedContent string
		var rangeStr string

		if spec.HasRange && spec.StartLine > 0 {
			start := spec.StartLine
			if start > totalLines {
				continue
			}
			end := spec.EndLine
			if end <= 0 || end > totalLines {
				end = totalLines
			}
			if start > end {
				start, end = end, start
			}
			if end-start+1 > DefaultMaxRetainedLinesPerFile {
				end = start + DefaultMaxRetainedLinesPerFile - 1
			}

			selectedLines := lines[start-1 : end]
			selectedContent = strings.Join(selectedLines, "\n")
			rangeStr = fmt.Sprintf("%d-%d", start, end)
		} else {
			if totalLines > DefaultMaxRetainedLinesPerFile {
				selectedLines := lines[:DefaultMaxRetainedLinesPerFile]
				notice := fmt.Sprintf("// [truncated: lines 1-%d shown of %d total lines. Use read tool if other sections are needed]", DefaultMaxRetainedLinesPerFile, totalLines)
				selectedContent = strings.Join(selectedLines, "\n") + "\n" + notice
				rangeStr = fmt.Sprintf("1-%d (truncated)", DefaultMaxRetainedLinesPerFile)
			} else {
				selectedContent = string(data)
				rangeStr = ""
			}
		}

		if totalChars+len(selectedContent) > DefaultMaxRetainedTotalChars {
			remainingChars := DefaultMaxRetainedTotalChars - totalChars
			if remainingChars > 500 && len(retained) == 0 {
				selectedContent = selectedContent[:remainingChars] + "\n// [truncated due to compression budget]"
				rangeStr += " (truncated)"
			} else {
				break
			}
		}

		totalChars += len(selectedContent)
		displayPath := strings.TrimPrefix(spec.Path, "/workspace/")

		retained = append(retained, RetainedFile{
			Path:    displayPath,
			Content: selectedContent,
			Range:   rangeStr,
		})
	}

	return retained
}
