package api

type (
	ChangePlayerRequest struct {
		StatefulRoom
		Index int `json:"index"`
	}
	ChangePlayerResponse int
	GameQuitRequest      StatefulRoom
	LoadGameRequest      StatefulRoom
	LoadGameResponse     string
	ResetGameRequest     StatefulRoom
	ResetGameResponse    string
	SaveGameRequest      StatefulRoom
	SaveGameResponse     string
	StartGameRequest     struct {
		StatefulRoom
		Record      bool
		RecordUser  string
		Game        string `json:"game"`
		PlayerIndex int    `json:"player_index"`
	}
	GameInfo struct {
		Alias   string `json:"alias"`
		Base    string `json:"base"`
		Name    string `json:"name"`
		Path    string `json:"path"`
		System  string `json:"system"`
		Type    string `json:"type"`
		Backend string `json:"backend,omitempty"`
		// IGDB-sourced enrichment (Phase 2). All optional — the cache
		// may be empty, or a ROM may not match any IGDB record. The
		// frontend and the semantic-search index (Phase 3) treat these
		// as best-effort context, never required.
		Genre     string `json:"genre,omitempty"`
		Franchise string `json:"franchise,omitempty"`
		Year      int    `json:"year,omitempty"`
		Summary   string `json:"summary,omitempty"`
		CoverURL  string `json:"cover_url,omitempty"`
	}
	StartGameResponse struct {
		Room
		AV      *AppVideoInfo `json:"av"`
		Record  bool          `json:"record"`
		KbMouse bool          `json:"kb_mouse"`
	}
	RecordGameRequest struct {
		StatefulRoom
		Active bool   `json:"active"`
		User   string `json:"user"`
	}
	RecordGameResponse      string
	TerminateSessionRequest Stateful
	WebrtcAnswerRequest     struct {
		Stateful
		Sdp string `json:"sdp"`
	}
	WebrtcIceCandidateRequest struct {
		Stateful
		Candidate string `json:"candidate"` // Base64-encoded ICE candidate
	}
	WebrtcInitRequest struct {
		Stateful
		// Identity is injected by the coordinator on WS upgrade from
		// X-Auth-Request-* headers (set by oauth2-proxy or the
		// chain-claude-test bypass). Optional — zero value means
		// anonymous; the worker falls back to treating the user as a
		// guest of the room host in that case.
		Identity Identity `json:"identity,omitempty"`
	}
	WebrtcInitResponse string

	// SetRaCredentialsRequest is forwarded from the coordinator to the
	// worker. Id identifies which user (GameSession) these credentials
	// belong to.
	SetRaCredentialsRequest struct {
		Stateful
		User  string `json:"user"`
		Token string `json:"token"`
	}

	AppVideoInfo struct {
		W int     `json:"w"`
		H int     `json:"h"`
		S int     `json:"s"`
		A float32 `json:"a"`
	}

	LibGameListInfo struct {
		T    int
		List []GameInfo
	}

	PrevSessionInfo struct {
		List []string
	}

	// RoomMember is one occupant of the current game room.
	// Multiple members can share a slot (free-form slot assignment is
	// an intentional product feature — two people on slot 1 is legal).
	RoomMember struct {
		UserId   string   `json:"user_id"` // GameSession id (coordinator uid)
		Slot     int      `json:"slot"`    // 0-based player index
		Identity Identity `json:"identity"`
	}

	// RoomMembersResponse is broadcast to every connected peer when
	// room membership changes (user connects/disconnects) or when any
	// user's slot assignment changes. Snapshot, not a delta.
	RoomMembersResponse struct {
		Members []RoomMember `json:"members"`
	}

	// AchievementUnlockedResponse is broadcast when the rcheevos
	// event handler fires RC_CLIENT_EVENT_ACHIEVEMENT_TRIGGERED for
	// the host's account. All peers in the room get the toast.
	AchievementUnlockedResponse struct {
		ID          uint32 `json:"id"`
		Title       string `json:"title"`
		Description string `json:"description"`
		Points      uint32 `json:"points"`
		BadgeURL    string `json:"badge_url,omitempty"`
	}

	// RoomHydrateProgressResponse fires during async ROM hydration
	// (pkg/worker/romcache) so the browser can render a loading UI
	// while the 30–90 s extract/repack runs. Stage names:
	//
	//   "extract"  — 7z decompression (Percent 0-100)
	//   "repack"   — extract-xiso building an XISO from the loose
	//                filesystem tree (no percent; Percent stays -1)
	//   "done"     — hydration finished, xemu about to start (100)
	//
	// Extras is a human-readable postfix (e.g. "Halo - Combat Evolved"
	// or "1.2 GB extracted"), shown verbatim in the UI.
	RoomHydrateProgressResponse struct {
		Stage   string `json:"stage"`
		Percent int    `json:"percent"`
		Extras  string `json:"extras,omitempty"`
	}
)
