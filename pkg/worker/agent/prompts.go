package agent

import (
	"fmt"
	"strings"
)

// Candidate is one retrieval hit handed to the LLM inside the system
// prompt. Order = rank; the model is asked to respect this ordering
// when picking. We keep the payload small so the prompt stays cheap
// in tokens — full summary goes in only if it's short.
type Candidate struct {
	Rank      int    `json:"rank"`
	GamePath  string `json:"game_path"`
	System    string `json:"system"`
	Title     string `json:"title"`
	Genre     string `json:"genre,omitempty"`
	Year      int    `json:"year,omitempty"`
	Franchise string `json:"franchise,omitempty"`
}

// HistoryTurn is one prior turn of the conversation. Roles: "user",
// "agent". The agent's previous actions are rendered in the prompt
// as their `text` — the JSON action object itself is not fed back.
type HistoryTurn struct {
	Role string `json:"role"`
	Text string `json:"text"`
}

// systemPrompt is deliberately explicit about the JSON schema. Small
// models do much better with an inline spec + 2-3 shot examples than
// they do with a long prose description.
const systemPrompt = `You are a concise game-selection assistant for a cloud gaming service. A user's personal game library is retrieved for you each turn; your job is to turn the conversation into ONE structured action.

Respond with a single JSON object, no extra text, no markdown fencing. One of:

  {"action":"ask","text":"..."}
     when you need a clarifying question (e.g. multiple reasonable titles fit).
     Example: {"action":"ask","text":"Halo or Halo 2?"}

  {"action":"recommend","text":"...","candidates":[{"game_path":"...","system":"..."}]}
     when you want to offer a short list the user can pick from.
     Each candidate MUST have only game_path and system — no other
     fields. The frontend looks up the rest from the library.
     Example: {"action":"recommend","text":"I have FIFA or Winning Eleven.","candidates":[
         {"game_path":"ps2/FIFA 08.iso","system":"ps2"},
         {"game_path":"ps2/Winning Eleven 9.iso","system":"ps2"}
     ]}

  {"action":"launch","text":"...","game_path":"...","system":"..."}
     when the user's intent is unambiguous and they've named the game.
     Example: {"action":"launch","text":"Launching Halo 2 on Xbox.","game_path":"xbox/Halo 2.iso","system":"xbox"}

  {"action":"say","text":"..."}
     when nothing actionable applies (e.g. the query can't be resolved from the library).
     Example: {"action":"say","text":"I don't see any chess games in your library."}

Rules:
  - "text" is 1-2 sentences, conversational but brief.
  - Prefer "ask" when 2-4 similar titles fit and you can narrow with one question.
  - Prefer "launch" only when the user has named a specific title and you have a clear match.
  - "game_path" and "system" MUST come from the candidates list; never invent paths.
  - If no candidate matches, use "say" — do not hallucinate a launch.
  - Do NOT include any prose outside the JSON object.`

// BuildPrompt constructs the message list sent to /api/chat for this
// turn. The system prompt carries the schema + rules; a single user
// message carries the candidate list + conversation history + the new
// query (easier for small models than many messages).
func BuildPrompt(query string, candidates []Candidate, history []HistoryTurn) []ChatMessage {
	var b strings.Builder
	b.WriteString("Current library candidates (ranked):\n")
	if len(candidates) == 0 {
		b.WriteString("(no matches)\n")
	} else {
		for _, c := range candidates {
			fmt.Fprintf(&b, "%d. %s — %s", c.Rank, c.Title, systemLabel(c.System))
			if c.Genre != "" {
				fmt.Fprintf(&b, " — %s", c.Genre)
			}
			if c.Year > 0 {
				fmt.Fprintf(&b, " (%d)", c.Year)
			}
			if c.Franchise != "" && !strings.EqualFold(c.Franchise, c.Title) {
				fmt.Fprintf(&b, " [series: %s]", c.Franchise)
			}
			fmt.Fprintf(&b, "\n   path=%s system=%s\n", c.GamePath, c.System)
		}
	}
	b.WriteString("\nConversation so far:\n")
	if len(history) == 0 {
		b.WriteString("(new conversation)\n")
	} else {
		for _, h := range history {
			fmt.Fprintf(&b, "%s: %s\n", h.Role, h.Text)
		}
	}
	b.WriteString("\nUser's new message: ")
	b.WriteString(query)
	b.WriteString("\n\nReply with exactly one JSON object per the system prompt.")

	return []ChatMessage{
		{Role: "system", Content: systemPrompt},
		{Role: "user", Content: b.String()},
	}
}

// systemLabel humanizes system slugs for the LLM prompt. Matches the
// enricher's buildEmbeddingText helper so the model sees consistent
// platform names across retrieval context and query.
func systemLabel(sys string) string {
	switch sys {
	case "xbox":
		return "Xbox"
	case "ps2":
		return "PlayStation 2"
	case "pcsx":
		return "PlayStation"
	case "gc":
		return "GameCube"
	case "wii":
		return "Wii"
	case "n64":
		return "Nintendo 64"
	case "snes":
		return "Super Nintendo"
	case "nes":
		return "NES"
	case "gba":
		return "Game Boy Advance"
	case "dreamcast":
		return "Dreamcast"
	case "mame":
		return "Arcade"
	case "dos":
		return "DOS"
	}
	return sys
}
