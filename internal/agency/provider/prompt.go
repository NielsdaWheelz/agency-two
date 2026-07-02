package provider

import "strings"

// PromptHint is the supervisor-facing result of inspecting a rendered pane.
// Status is three-valued so a stale prompt can be cleared without fabricating
// one:
//   - "NeedsApproval"/"NeedsInput": a prompt phrase is present in the bottom region.
//   - PromptStatusNone: the bottom region is readable but shows no prompt (the
//     agent is working/idle). Positive evidence used to clear a recorded prompt pin.
//   - "": uncertain — the pane was blank/unavailable, so nothing is asserted and
//     status degrades to the derived Live/Quiet.
//
// Detection never fabricates a NeedsInput/NeedsApproval it cannot see.
type PromptHint struct {
	Status string // "NeedsApproval" | "NeedsInput" | PromptStatusNone | "" (uncertain)
	Detail string
}

// PromptStatusNone is the readable-but-not-prompting hint. It is a supervisor
// projection signal (clear a stale pin), never a persisted run status.
const PromptStatusNone = "None"

// DetectPromptState inspects the rendered tmux/PTY snapshot for a provider and
// returns a best-effort hint. It reads only the rendered snapshot (never a
// provider-private transcript) and scans the bottom region of the screen where
// the live prompt sits. A blank/unavailable pane yields "" (uncertain); a
// readable pane with no prompt phrase yields PromptStatusNone so a prior prompt
// pin can be cleared. The same phrase set governs set and clear, so a prompt
// still on screen is never cleared, and one that has scrolled off is.
func DetectPromptState(providerKey, rendered string) PromptHint {
	region := bottomRegion(rendered, 12)
	if region == "" {
		return PromptHint{}
	}
	for _, phrase := range approvalPhrases(providerKey) {
		if strings.Contains(region, phrase) {
			return PromptHint{Status: "NeedsApproval", Detail: phrase}
		}
	}
	for _, phrase := range inputPhrases(providerKey) {
		if strings.Contains(region, phrase) {
			return PromptHint{Status: "NeedsInput", Detail: phrase}
		}
	}
	return PromptHint{Status: PromptStatusNone}
}

// FingerprintKnown reports whether the rendered snapshot looks like a recognized
// provider TUI at all. Doctor can use this to raise a defect when the heuristics
// no longer match a known fingerprint, so breakage is observable, not silent.
func FingerprintKnown(providerKey, rendered string) bool {
	lower := strings.ToLower(rendered)
	if strings.TrimSpace(lower) == "" {
		return false
	}
	for _, marker := range fingerprintMarkers(providerKey) {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

// bottomRegion returns the last n non-empty rendered lines, lowercased and
// joined, which is where a live prompt appears. Scanning only the bottom avoids
// false positives from scrollback history.
func bottomRegion(rendered string, n int) string {
	lines := strings.Split(rendered, "\n")
	var kept []string
	for i := len(lines) - 1; i >= 0 && len(kept) < n; i-- {
		trimmed := strings.TrimSpace(lines[i])
		if trimmed == "" {
			continue
		}
		kept = append(kept, strings.ToLower(trimmed))
	}
	return strings.Join(kept, "\n")
}

func approvalPhrases(providerKey string) []string {
	common := []string{
		"waiting for approval",
		"approve this action",
		"requires approval",
		"approval required",
		"allow this command",
		"do you want to proceed",
		"(y/n)",
		"[y/n]",
	}
	switch providerKey {
	case KeyClaude:
		// Claude Code renders every blocking decision (trust gate, tool approval,
		// onboarding menus) with an "Enter to confirm · Esc to cancel" footer and a
		// "❯" selector; its idle input box does not, so these reliably mark a run
		// that is waiting on the user. Verified against the real TUI (see
		// prompt_test.go golden captures).
		return append(common, "do you want to allow", "permission to", "esc to cancel", "enter to confirm")
	case KeyCodex:
		return append(common, "approve command", "run this command?")
	default:
		return common
	}
}

func inputPhrases(providerKey string) []string {
	common := []string{
		"waiting for input",
		"input required",
		"enter your prompt",
		"type your message",
		"press enter to continue",
	}
	switch providerKey {
	case KeyClaude:
		return append(common, "how can i help", "what would you like")
	case KeyCodex:
		return append(common, "what should i do", "send a message")
	default:
		return common
	}
}

func fingerprintMarkers(providerKey string) []string {
	switch providerKey {
	case KeyClaude:
		return []string{"claude", "anthropic", "> ", "esc to interrupt"}
	case KeyCodex:
		return []string{"codex", "openai", "> ", "esc to interrupt"}
	default:
		return []string{"> "}
	}
}
