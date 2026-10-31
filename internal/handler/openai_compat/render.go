package openai_compat

import (
	"errors"
	"fmt"
	"strings"

	"github.com/Tencent/WeKnora/internal/types"
)

// Renderer limits (docs/design/agent-model-gateway-m1.md §7.2). The client
// sends the whole conversation every call; the gateway folds it into the
// single query the one-shot temporary session is asked. The limits keep a
// pathological client history from dwarfing the agent's own context.
const (
	// systemBlockMaxRunes caps ONE system/developer message inside the
	// caller-instructions block; the tail is marked 截断.
	systemBlockMaxRunes = 8000
	// historyMaxMessages keeps at most the N most recent history entries.
	historyMaxMessages = 40
	// historyMaxRunes is the total budget of the history block; entries are
	// dropped from the oldest side until it fits.
	historyMaxRunes = 32000
	// toolTextMaxRunes caps one client-tool call/result rendering.
	toolTextMaxRunes = 2000
	// toolArgsMaxRunes caps the argument digest inside a tool-call line.
	toolArgsMaxRunes = 300
	// maxImagesPerRequest is the multimodal ceiling per completion call.
	maxImagesPerRequest = 4
	// maxImageBytes is the decoded ceiling for one data-URL image (10MB).
	maxImageBytes = 10 << 20
)

// Renderer failures map to 400 invalid_request_error upstream.
var (
	errNoUserQuery    = errors.New("messages must include a final user message with text or an image")
	errTooManyImages  = errors.New("at most 4 images are allowed per request")
	errImageTooLarge  = errors.New("an image exceeds the 10MB limit")
	errBadImageSource = errors.New("image_url must be a data: URL or an http(s) URL")
)

// renderedTurn is the renderer's output: the single query for the temporary
// session, the extracted image URLs, and client-facing warnings (echoed in
// the response meta so callers can self-check what was adjusted).
type renderedTurn struct {
	Query    string
	Images   []string
	Warnings []string
	// ClientTools are the caller's bridgeable tool definitions (M3-1),
	// validated and policy-gated; nil keeps the M1 ignored-tools behavior.
	ClientTools []types.ClientToolSpec
}

// renderMessages folds an OpenAI messages array into one gateway query:
//
//	[调用方指令]  — all system/developer messages, joined
//	[对话历史]    — every user/assistant message but the last user one
//	[本次问题]    — the final user message text
//
// Client tool traffic is textualized (the gateway never executes client
// tools), and image parts become ImageURLs for the existing upload chain.
// The renderer is a pure function: same input, same output, no I/O.
// lastUserMessageIndex returns the index of the last user-role message, or
// -1 when none exists. M2-3 continuation renders from this slice boundary:
// only the newest user turn is sent; WeKnora session history supplies the rest.
func lastUserMessageIndex(messages []ChatMessage) int {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "user" {
			return i
		}
	}
	return -1
}

func renderMessages(messages []ChatMessage, imagesAllowed bool) (*renderedTurn, error) {
	turn := &renderedTurn{}

	// Pass 1: split the array into instruction / history / final-question
	// buckets and collect images. "Last user message" is the LAST message
	// with role user, even if assistant/tool messages trail it — OpenAI
	// clients terminate tool sequences with tool results, not user turns.
	lastUser := -1
	for i := range messages {
		if messages[i].Role == "user" {
			lastUser = i
		}
	}
	var (
		instructions []string
		history      []ChatMessage
		finalUser    *ChatMessage
	)
	for i := range messages {
		msg := messages[i]
		switch msg.Role {
		case "system", "developer":
			text := truncateMarkRunes(msg.Content.Text, systemBlockMaxRunes)
			if strings.TrimSpace(text) != "" {
				instructions = append(instructions, text)
			}
		case "user":
			if i == lastUser {
				finalUser = &messages[i]
			} else {
				history = append(history, msg)
			}
			for _, part := range msg.Content.Parts {
				if part.Type == PartTypeImage && part.ImageURL != nil {
					url, err := normalizeImageURL(part.ImageURL.URL)
					if err != nil {
						return nil, err
					}
					turn.Images = append(turn.Images, url)
				}
			}
		case "assistant", "tool":
			history = append(history, msg)
		default:
			// Unknown role: keep it in history as plain text so nothing a
			// client sends is silently dropped from the transcript.
			history = append(history, msg)
		}
	}
	if len(turn.Images) > maxImagesPerRequest {
		return nil, errTooManyImages
	}
	for _, url := range turn.Images {
		if err := checkImageSize(url); err != nil {
			return nil, err
		}
	}
	if !imagesAllowed && len(turn.Images) > 0 {
		turn.Images = nil
		turn.Warnings = append(turn.Warnings, "[图片已忽略:智能体未开启图片能力]")
	}

	// Pass 2: last user text. Empty text is legal only when images carry the
	// turn (upload-only question), mirroring the web behavior.
	var finalText string
	if finalUser != nil {
		finalText = strings.TrimSpace(finalUser.Content.Text)
	}
	if finalText == "" && len(turn.Images) == 0 {
		return nil, errNoUserQuery
	}
	if finalText == "" {
		finalText = "[调用方未输入文字,请根据所附图片回答]"
	}

	// Pass 3: assemble the blocks. Tool result lines want the function name
	// recorded by the assistant tool_calls they answer, so build the map from
	// the full transcript first.
	toolNames := make(map[string]string)
	for i := range messages {
		for _, call := range messages[i].ToolCalls {
			if call.ID != "" && call.Function.Name != "" {
				toolNames[call.ID] = call.Function.Name
			}
		}
	}
	var b strings.Builder
	if len(instructions) > 0 {
		b.WriteString("[调用方指令]\n")
		b.WriteString(strings.Join(instructions, "\n"))
		b.WriteString("\n\n")
	}
	if len(history) > 0 {
		rendered, omitted := renderHistory(history, toolNames)
		if rendered != "" {
			b.WriteString(rendered)
			b.WriteString("\n")
		}
		if omitted > 0 {
			fmt.Fprintf(&b, "…(更早 %d 条已省略)…\n\n", omitted)
		}
	}
	for _, warning := range turn.Warnings {
		b.WriteString(warning)
		b.WriteString("\n")
	}
	b.WriteString("[本次问题]\n")
	b.WriteString(finalText)
	turn.Query = b.String()
	return turn, nil
}

// renderHistory textualizes history entries and trims the window. It returns
// the [对话历史] block (without header) and how many oldest entries were
// dropped by the message-count and total-size budgets. toolNames maps a tool
// call ID to its function name so result lines can name the tool.
func renderHistory(history []ChatMessage, toolNames map[string]string) (string, int) {
	// Keep the most recent N entries.
	omitted := 0
	if len(history) > historyMaxMessages {
		omitted = len(history) - historyMaxMessages
		history = history[len(history)-historyMaxMessages:]
	}
	lines := make([]string, 0, len(history))
	for _, msg := range history {
		lines = append(lines, renderHistoryLine(msg, toolNames))
	}
	// Trim total size from the oldest side.
	total := 0
	for _, line := range lines {
		total += len([]rune(line)) + 1
	}
	for total > historyMaxRunes && len(lines) > 1 {
		total -= len([]rune(lines[0])) + 1
		lines = lines[1:]
		omitted++
	}
	if len(lines) == 0 {
		return "", omitted
	}
	return "[对话历史]\n" + strings.Join(lines, "\n"), omitted
}

// renderHistoryLine textualizes one history entry. Client tool traffic
// becomes bracketed text (M1 never executes client tools); reasoning traces
// are never replayed.
func renderHistoryLine(msg ChatMessage, toolNames map[string]string) string {
	switch msg.Role {
	case "assistant":
		text := strings.TrimSpace(msg.Content.Text)
		for _, call := range msg.ToolCalls {
			line := fmt.Sprintf("[客户端工具 %s] 参数: %s",
				call.Function.Name, truncateRunes(call.Function.Arguments, toolArgsMaxRunes))
			if text == "" {
				text = line
			} else {
				text += "\n" + line
			}
		}
		if text == "" {
			text = "(空)"
		}
		return "assistant: " + text
	case "tool":
		name := toolNames[msg.ToolCallID]
		if name == "" {
			name = "结果"
		}
		output := truncateMarkRunes(msg.Content.Text, toolTextMaxRunes)
		return fmt.Sprintf("[客户端工具 %s] → %s", name, output)
	default:
		text := strings.TrimSpace(msg.Content.Text)
		if text == "" {
			text = "(空)"
		}
		return msg.Role + ": " + text
	}
}

// normalizeImageURL validates an image reference: data URLs are kept for the
// upload chain, remote URLs must be http(s).
func normalizeImageURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errBadImageSource
	}
	lower := strings.ToLower(raw)
	if strings.HasPrefix(lower, "data:") {
		return raw, nil
	}
	if strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://") {
		return raw, nil
	}
	return "", errBadImageSource
}

// checkImageSize enforces the 10MB per-image ceiling on data URLs (the only
// form whose size is knowable without fetching).
func checkImageSize(url string) error {
	comma := strings.Index(url, ",")
	if comma < 0 {
		return nil // non-canonical data URL; let the upload chain reject it
	}
	meta := strings.ToLower(url[:comma])
	if !strings.Contains(meta, ";base64") {
		return nil // plain data URL, size is the payload itself
	}
	payload := url[comma+1:]
	size := len(payload) * 3 / 4 // decoded base64 size, upper bound
	if size > maxImageBytes {
		return errImageTooLarge
	}
	return nil
}

// truncateRunes bounds a string to max runes without splitting a rune.
func truncateRunes(s string, max int) string {
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max])
}

// truncateMarkRunes truncates and marks the cut, per the design table's
// "(截断)" convention for system blocks and tool bodies.
func truncateMarkRunes(s string, max int) string {
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max]) + "(截断)"
}
