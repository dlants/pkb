package chunk

import "strings"

// TargetChunkSize is the default max chunk size (~500 tokens ≈ 2000 chars).
const TargetChunkSize = 2000

const characterSplitOverlap = 200

type hardBlock struct {
	text           string
	start          Position
	end            Position
	headingContext string
}

type softChunk struct {
	text  string
	start Position
	end   Position
}

type paragraph struct {
	lines        []string
	startLineIdx int
}

func endPosOfLine(lineNum int, line string) Position {
	col := len(line)
	if col == 0 {
		col = 1
	}
	return Position{Line: lineNum, Col: col}
}

type heading struct {
	depth int
	title string
}

func parseHeading(line string) (heading, bool) {
	hashCount := 0
	for i := 0; i < len(line) && i < 7; i++ {
		if line[i] == '#' {
			hashCount++
		} else {
			break
		}
	}
	if hashCount > 0 && hashCount <= 6 && len(line) > hashCount && line[hashCount] == ' ' {
		return heading{depth: hashCount, title: strings.TrimSpace(line[hashCount+1:])}, true
	}
	return heading{}, false
}

func isCodeFence(line string) bool {
	return strings.HasPrefix(line, "```")
}

func getHeadingContext(hierarchy map[int]string) string {
	var parts []string
	for i := 1; i <= 6; i++ {
		if title, ok := hierarchy[i]; ok && title != "" {
			parts = append(parts, strings.Repeat("#", i)+" "+title)
		} else {
			break
		}
	}
	return strings.Join(parts, " > ")
}

func splitIntoHardBlocks(text string) []hardBlock {
	lines := strings.Split(text, "\n")
	var blocks []hardBlock
	hierarchy := map[int]string{}

	var currentBlock []string
	blockStartLine := 1
	blockHeadingContext := ""
	inCodeBlock := false

	flushBlock := func(endLineNum int) {
		if len(currentBlock) > 0 {
			lastLine := currentBlock[len(currentBlock)-1]
			blocks = append(blocks, hardBlock{
				text:           strings.Join(currentBlock, "\n"),
				start:          Position{Line: blockStartLine, Col: 1},
				end:            endPosOfLine(endLineNum, lastLine),
				headingContext: blockHeadingContext,
			})
			currentBlock = nil
		}
	}

	for i, line := range lines {
		lineNum := i + 1

		if isCodeFence(line) {
			inCodeBlock = !inCodeBlock
			currentBlock = append(currentBlock, line)
			continue
		}
		if inCodeBlock {
			currentBlock = append(currentBlock, line)
			continue
		}

		if h, ok := parseHeading(line); ok {
			flushBlock(lineNum - 1)
			hierarchy[h.depth] = h.title
			for d := h.depth + 1; d <= 6; d++ {
				delete(hierarchy, d)
			}
			currentBlock = []string{line}
			blockStartLine = lineNum
			blockHeadingContext = getHeadingContext(hierarchy)
			continue
		}

		currentBlock = append(currentBlock, line)
	}

	flushBlock(len(lines))
	return blocks
}

func splitIntoParagraphs(lines []string) []paragraph {
	var paragraphs []paragraph
	var current []string
	startIdx := 0

	for i, line := range lines {
		if strings.TrimSpace(line) == "" {
			if len(current) > 0 {
				paragraphs = append(paragraphs, paragraph{lines: current, startLineIdx: startIdx})
				current = nil
			}
			startIdx = i + 1
		} else {
			current = append(current, line)
		}
	}
	if len(current) > 0 {
		paragraphs = append(paragraphs, paragraph{lines: current, startLineIdx: startIdx})
	}
	return paragraphs
}

func positionAtOffset(text string, start Position, offset int) Position {
	line := start.Line
	col := start.Col
	for i := 0; i < offset && i < len(text); i++ {
		if text[i] == '\n' {
			line++
			col = 1
		} else {
			col++
		}
	}
	return Position{Line: line, Col: col}
}

func splitByCharacters(text string, start Position, maxChunkSize, overlap int) []softChunk {
	var chunks []softChunk
	step := maxChunkSize - overlap
	if step <= 0 {
		step = maxChunkSize
	}
	i := 0
	for i < len(text) {
		end := i + maxChunkSize
		if end > len(text) {
			end = len(text)
		}
		chunkText := text[i:end]
		chunkStart := positionAtOffset(text, start, i)
		chunkEnd := positionAtOffset(text, start, i+len(chunkText)-1)
		chunks = append(chunks, softChunk{text: chunkText, start: chunkStart, end: chunkEnd})
		i += step
	}
	return chunks
}

func softSplitBlock(text string, blockStart Position, maxChunkSize int) []softChunk {
	if len(text) <= maxChunkSize {
		lines := strings.Split(text, "\n")
		lastLine := lines[len(lines)-1]
		return []softChunk{{
			text:  text,
			start: blockStart,
			end:   endPosOfLine(blockStart.Line+len(lines)-1, lastLine),
		}}
	}

	var chunks []softChunk
	lines := strings.Split(text, "\n")
	paragraphs := splitIntoParagraphs(lines)

	var currentParas []paragraph
	currentSize := 0

	flushChunk := func() {
		if len(currentParas) == 0 {
			return
		}
		var chunkLines []string
		for i, p := range currentParas {
			if i > 0 {
				chunkLines = append(chunkLines, "")
			}
			chunkLines = append(chunkLines, p.lines...)
		}
		startLine := blockStart.Line + currentParas[0].startLineIdx
		lastPara := currentParas[len(currentParas)-1]
		endLineIdx := lastPara.startLineIdx + len(lastPara.lines) - 1
		endLine := blockStart.Line + endLineIdx
		chunks = append(chunks, softChunk{
			text:  strings.Join(chunkLines, "\n"),
			start: Position{Line: startLine, Col: 1},
			end:   endPosOfLine(endLine, lastPara.lines[len(lastPara.lines)-1]),
		})
		currentParas = nil
		currentSize = 0
	}

	for _, para := range paragraphs {
		paraText := strings.Join(para.lines, "\n")
		addedSize := len(paraText)
		if len(currentParas) > 0 {
			addedSize += 2
		}

		if currentSize+addedSize > maxChunkSize && len(currentParas) > 0 {
			flushChunk()
		}

		if len(paraText) <= maxChunkSize {
			currentParas = append(currentParas, para)
			currentSize += addedSize
		} else {
			flushChunk()
			paraStart := Position{Line: blockStart.Line + para.startLineIdx, Col: 1}
			chunks = append(chunks, splitByCharacters(paraText, paraStart, maxChunkSize, characterSplitOverlap)...)
		}
	}

	flushChunk()
	return chunks
}

// ChunkMarkdown splits markdown text into chunks along heading and paragraph
// boundaries, carrying heading breadcrumbs as HeadingContext.
func ChunkMarkdown(text string, maxChunkSize int) []ChunkInfo {
	if maxChunkSize <= 0 {
		maxChunkSize = TargetChunkSize
	}
	if strings.TrimSpace(text) == "" {
		return nil
	}

	hardBlocks := splitIntoHardBlocks(text)
	var chunks []ChunkInfo

	for _, block := range hardBlocks {
		blockText := block.text
		blockStart := block.start
		firstLine := strings.SplitN(blockText, "\n", 2)[0]
		if _, ok := parseHeading(firstLine); ok {
			newlineIdx := strings.Index(blockText, "\n")
			if newlineIdx != -1 {
				blockText = blockText[newlineIdx+1:]
				blockStart = Position{Line: block.start.Line + 1, Col: 1}
			} else {
				continue
			}
		}

		if strings.TrimSpace(blockText) == "" {
			continue
		}

		for _, sub := range softSplitBlock(blockText, blockStart, maxChunkSize) {
			chunks = append(chunks, ChunkInfo{
				Text:           sub.text,
				HeadingContext: block.headingContext,
				Start:          sub.start,
				End:            sub.end,
			})
		}
	}

	return chunks
}
