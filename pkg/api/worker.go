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
		Alias  string `json:"alias"`
		Base   string `json:"base"`
		Name   string `json:"name"`
		Path   string `json:"path"`
		System string `json:"system"`
		Type   string `json:"type"`
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
)
