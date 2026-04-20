package api

type (
	ChangePlayerUserRequest  int
	CheckLatencyUserResponse []string
	CheckLatencyUserRequest  map[string]int64
	GameStartUserRequest     struct {
		GameName    string `json:"game_name"`
		RoomId      string `json:"room_id"`
		Record      bool   `json:"record,omitempty"`
		RecordUser  string `json:"record_user,omitempty"`
		PlayerIndex int    `json:"player_index"`
	}
	GameStartUserResponse struct {
		RoomId  string        `json:"roomId"`
		Av      *AppVideoInfo `json:"av"`
		KbMouse bool          `json:"kb_mouse"`
	}
	IceServer struct {
		Urls       string `json:"urls,omitempty"`
		Username   string `json:"username,omitempty"`
		Credential string `json:"credential,omitempty"`
	}
	InitSessionUserResponse struct {
		Ice    []IceServer `json:"ice"`
		Games  []AppMeta   `json:"games"`
		Wid    string      `json:"wid"`
		RoomId string      `json:"roomId,omitempty"`
		// Identity the coordinator parsed from the WS upgrade headers.
		// Lets the client know who it is (for the user-preferences panel,
		// per-user state, etc.) without waiting on a roster broadcast.
		Identity Identity `json:"identity,omitempty"`
	}
	AppMeta struct {
		Alias  string `json:"alias,omitempty"`
		Title  string `json:"title"`
		System string `json:"system"`
		// Path is the library-relative filename (e.g.
		// "xbox/Halo.xiso.iso"). Exposed to the browser so the
		// Phase-3 semantic-search blend can match incoming hits
		// (which key on {game_path, system}) against the local
		// library shown in the search bar. Not used for anything
		// else client-side.
		Path string `json:"path,omitempty"`
		// CoverURL is the IGDB cover art (cover_big, 264x374) the
		// Phase-1 search cards render as a thumbnail. Empty string
		// when the game hasn't been IGDB-matched yet or the match
		// had no cover asset.
		CoverURL string `json:"cover_url,omitempty"`
	}
	WebrtcAnswerUserRequest string
	WebrtcUserIceCandidate  string
	// SetRaCredentialsUserRequest is sent by the client whenever the
	// user saves their RetroAchievements credentials in the overlay
	// preferences panel. Token is the RA API token (not a password).
	// The worker uses these to log into rcheevos and enable per-user
	// achievement tracking.
	SetRaCredentialsUserRequest struct {
		User  string `json:"user"`
		Token string `json:"token"`
	}
)
