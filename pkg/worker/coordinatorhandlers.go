package worker

import (
	"encoding/base64"
	"log"
	"path/filepath"
	"sync/atomic"

	"github.com/giongto35/cloud-game/v3/pkg/api"
	"github.com/giongto35/cloud-game/v3/pkg/com"
	"github.com/giongto35/cloud-game/v3/pkg/config"
	"github.com/giongto35/cloud-game/v3/pkg/games"
	"github.com/giongto35/cloud-game/v3/pkg/network/webrtc"
	"github.com/giongto35/cloud-game/v3/pkg/worker/caged"
	xemucage "github.com/giongto35/cloud-game/v3/pkg/worker/caged/xemu"
	"github.com/giongto35/cloud-game/v3/pkg/worker/media"
	"github.com/giongto35/cloud-game/v3/pkg/worker/romcache"
	"github.com/giongto35/cloud-game/v3/pkg/worker/room"
	"github.com/goccy/go-json"
)

// buildConnQuery builds initial connection data query to a coordinator.
func buildConnQuery(id com.Uid, conf config.Worker, address string) (string, error) {
	addr := conf.GetPingAddr(address)
	return toBase64Json(api.ConnectionRequest[com.Uid]{
		Addr:    addr.Hostname(),
		Id:      id,
		IsHTTPS: conf.Server.Https,
		PingURL: addr.String(),
		Port:    conf.GetPort(address),
		Tag:     conf.Tag,
		Zone:    conf.Network.Zone,
	})
}

func (c *coordinator) HandleWebrtcInit(rq api.WebrtcInitRequest, w *Worker, factory *webrtc.ApiFactory) api.Out {
	peer := webrtc.New(c.log, factory)
	localSDP, err := peer.NewCall(w.conf.Encoder.Video.Codec, "opus", func(data any) {
		candidate, err := toBase64Json(data)
		if err != nil {
			c.log.Error().Err(err).Msgf("ICE candidate encode fail for [%v]", data)
			return
		}
		c.IceCandidate(candidate, rq.Id)
	})
	if err != nil {
		c.log.Error().Err(err).Msg("cannot create new webrtc session")
		return api.EmptyPacket
	}
	sdp, err := toBase64Json(localSDP)
	if err != nil {
		c.log.Error().Err(err).Msgf("SDP encode fail fro [%v]", localSDP)
		return api.EmptyPacket
	}

	user := room.NewGameSession(rq.Id, peer) // use user uid from the coordinator
	user.Identity = rq.Identity
	if !rq.Identity.IsAnonymous() {
		c.log.Info().Msgf("Peer connection: %s (user=%s/%s)",
			user.Id(), rq.Identity.Sub, rq.Identity.Username)
	} else {
		c.log.Info().Msgf("Peer connection: %s (anonymous)", user.Id())
	}
	w.router.AddUser(user)

	return api.Out{Payload: sdp}
}

func (c *coordinator) HandleWebrtcAnswer(rq api.WebrtcAnswerRequest, w *Worker) {
	if user := w.router.FindUser(rq.Id); user != nil {
		if err := room.WithWebRTC(user.Session).SetRemoteSDP(rq.Sdp, fromBase64Json); err != nil {
			c.log.Error().Err(err).Msgf("cannot set remote SDP of client [%v]", rq.Id)
		}
	}
}

func (c *coordinator) HandleWebrtcIceCandidate(rs api.WebrtcIceCandidateRequest, w *Worker) {
	if user := w.router.FindUser(rs.Id); user != nil {
		if err := room.WithWebRTC(user.Session).AddCandidate(rs.Candidate, fromBase64Json); err != nil {
			c.log.Error().Err(err).Msgf("cannot add ICE candidate of the client [%v]", rs.Id)
		}
	}
}

func (c *coordinator) HandleGameStart(rq api.StartGameRequest, w *Worker) api.Out {
	user := w.router.FindUser(rq.Id)
	if user == nil {
		c.log.Error().Msgf("no user [%v]", rq.Id)
		return api.EmptyPacket
	}
	user.Index = rq.PlayerIndex

	r := w.router.FindRoom(rq.Rid)

	// +injects game data into the original game request
	// the name of the game either in the `room id` field or
	// it's in the initial request.
	//
	// NOTE: rooms are created with `uid = gameName` when the host starts
	// with an empty Rid (see the `if r == nil` block below), so most
	// joining users arrive with rq.Rid = "NHL 2007 (USA)" rather than
	// the encoded "<hex>___<gameName>" format that ExtractAppNameFromUrl
	// expects. Earlier code hard-errored here, which caused joining
	// players' StartGame to return EmptyPacket before the OnMessage
	// handler below was ever installed — so their WebRTC input packets
	// landed on a nil callback and silently disappeared. Fall back to
	// treating the Rid as the game name when decoding fails.
	gameName := rq.Game
	if rq.Rid != "" {
		name := w.launcher.ExtractAppNameFromUrl(rq.Rid)
		if name == "" {
			name = rq.Rid
		}
		gameName = name
	}

	gameInfo, err := w.launcher.FindAppByName(gameName)
	if err != nil {
		c.log.Error().Err(err).Send()
		return api.EmptyPacket
	}

	if r == nil { // new room
		uid := rq.Rid
		if uid == "" {
			uid = gameName
		}
		game := games.GameMetadata(gameInfo)

		r = room.NewRoom[*room.GameSession](uid, nil, w.router.Users(), nil)
		r.HandleClose = func() {
			c.CloseRoom(uid)
			c.log.Debug().Msgf("room close request %v sent", uid)
		}

		if other := w.router.Room(); other != nil {
			c.log.Error().Msgf("concurrent room creation: %v / %v", uid, w.router.Room().Id())
			return api.EmptyPacket
		}

		w.router.SetRoom(r)
		c.log.Info().Str("room", r.Id()).Str("game", game.Name).Str("backend", game.Backend).Msg("New room")

		// Phase 6 backend dispatch. The libretro path below is the
		// original code unchanged; the xemu path early-returns after
		// setting up its simpler pipeline (no save states, no cloud
		// storage, no recording, no Vulkan zero-copy — all libretro-
		// specific features that don't apply).
		if game.Backend == "xemu" {
			if err := c.startXemuRoom(w, r, game); err != nil {
				c.log.Error().Err(err).Msg("xemu room start failed")
				r.Close()
				w.router.SetRoom(nil)
				return api.EmptyPacket
			}
			goto commonStart
		}

		// start the emulator
		app := room.WithEmulator(w.mana.Get(caged.Libretro))
		app.ReloadFrontend()
		app.SetSessionId(uid)
		app.SetSaveOnClose(true)
		app.EnableCloudStorage(uid, w.storage)
		app.EnableRecording(rq.Record, rq.RecordUser, gameName)

		r.SetApp(app)

		m := media.NewWebRtcMediaPipe(w.conf.Encoder.Audio, w.conf.Encoder.Video, w.log)

		// recreate the video encoder
		app.VideoChangeCb(func() {
			app.ViewportRecalculate()
			m.VideoW, m.VideoH = app.ViewportSize()
			m.VideoScale = app.Scale()

			if m.IsInitialized() {
				if err := m.Reinit(); err != nil {
					c.log.Error().Err(err).Msgf("reinit fail")
				}
			}

			data, err := api.Wrap(api.Out{
				T: uint8(api.AppVideoChange),
				Payload: api.AppVideoInfo{
					W: m.VideoW,
					H: m.VideoH,
					A: app.AspectRatio(),
					S: int(app.Scale()),
				}})
			if err != nil {
				c.log.Error().Err(err).Msgf("wrap")
			}
			r.Send(data)
		})

		w.log.Info().Msgf("Starting the game: %v", gameName)
		if err := app.Load(game, w.conf.Library.BasePath); err != nil {
			c.log.Error().Err(err).Msgf("couldn't load the game %v", game)
			r.Close()
			w.router.SetRoom(nil)
			return api.EmptyPacket
		}

		// Kick off rcheevos game-load async — if the user is logged
		// into RA, rc_client fetches the achievement set for this
		// ROM's hash. Failures (no credentials, unknown system,
		// network) are non-fatal; the game still launches.
		if w.rch != nil {
			fullPath := game.FullPath(w.conf.Library.BasePath)
			system := game.System
			go func() {
				if err := w.rch.LoadGameFromFile(fullPath, system); err != nil {
					w.log.Warn().Err(err).Msgf("rcheevos load fail (%s)", system)
					return
				}
				w.log.Info().Msgf("rcheevos game loaded: %q (%d achievements)", w.rch.GameTitle(), w.rch.AchievementCount())
			}()
		}

		m.AudioSrcHz = app.AudioSampleRate()
		m.AudioFrames = w.conf.Encoder.Audio.Frames
		m.VideoW, m.VideoH = app.ViewportSize()
		m.VideoScale = app.Scale()

		r.SetMedia(m)

		if err := m.Init(); err != nil {
			c.log.Error().Err(err).Msgf("couldn't init the media")
			r.Close()
			w.router.SetRoom(nil)
			return api.EmptyPacket
		}

		if app.Flipped() {
			m.SetVideoFlip(true)
		}
		m.SetPixFmt(app.PixFormat())
		m.SetRot(app.Rotation())

		r.BindAppMedia()

		// BindAppMedia wires the app's data callback to r.Send which
		// broadcasts to every user in the room. That's wrong for rumble
		// packets, which are per-port (encoded [0xFF, port, effect, hi, lo])
		// — every peer was feeling the host's controller rumble. Override
		// the callback here so rumble goes to every user whose Index
		// matches the rumbling port; everything else still broadcasts.
		//
		// Multiple users can legitimately share a port (e.g. four friends
		// taking turns controlling player 1 on a single-player game), so
		// every matching user receives the rumble — the loop does NOT
		// early-return on first match. When users later stripe into
		// distinct slots via HandleChangePlayer, each u.Index updates in
		// place and the rumble naturally follows them.
		r.App().SetDataCb(func(d []byte) {
			if len(d) >= 2 && d[0] == 0xFF {
				targetPort := int(d[1])
				for u := range w.router.Users().Values() {
					if u.Index == targetPort {
						u.SendData(d)
					}
				}
				return
			}
			r.Send(d)
		})

		// Phase 3: attempt to arm the Vulkan→CUDA→NVENC zero-copy encode path.
		//
		// Conditions (all must hold):
		//   1. config.Encoder.Video.ZeroCopy == true  (explicit opt-in; default false)
		//   2. codec == "h264_nvenc"
		//   3. Vulkan HW render context is active and the device supports
		//      VK_KHR_external_memory_fd (NVIDIA Linux with nvenc build tag)
		//
		// If any condition fails, TryArmZeroCopy returns false and the CPU
		// readback path (already set by BindAppMedia) remains active.
		//
		// When armed, ProcessVideo transparently routes frames through
		// GPU-direct NVENC and falls back to CPU on the rare frames where
		// the fd is not yet exported (e.g. the very first frame).
		//
		// Phase 3c: GPU RGBA→NV12 colour conversion is now implemented via
		// embedded PTX kernels (BT.601, JIT-compiled).  Falls back to raw
		// copy if PTX JIT fails (stream stays up, colours may be wrong).
		backend := app.VideoBackend()
		zcConfigEnabled := w.conf.Encoder.Video.ZeroCopy
		zcAvailable := backend != nil && backend.SupportsZeroCopy()
		backendName := "<nil>"
		backendKind := "<nil>"
		if backend != nil {
			backendName = backend.Name()
			backendKind = string(backend.Kind())
		}
		log.Printf("[cloudplay diag] zero-copy arming check: config.ZeroCopy=%v backend=%s kind=%s SupportsZeroCopy=%v codec=%s",
			zcConfigEnabled, backendName, backendKind, zcAvailable, w.conf.Encoder.Video.Codec)
		if zcConfigEnabled && zcAvailable {
			vw, vh := uint(m.VideoW), uint(m.VideoH)
			armed := media.TryArmZeroCopy(
				m,
				w.conf.Encoder.Video,
				vw, vh,
				func(fw, fh uint) (int, uint64, error) { return backend.ZeroCopyFd(fw, fh) },
				func() error { return backend.WaitFrameReady() },
				w.log,
			)
			log.Printf("[cloudplay diag] zero-copy TryArmZeroCopy returned: armed=%v (dims=%dx%d backend=%s)", armed, vw, vh, backendName)
		}

		r.StartApp()
	}

commonStart:
	c.log.Debug().Msg("Start session input poll")

	needsKbMouse := r.App().KbMouseSupport()

	s := room.WithWebRTC(user.Session)
	s.OnPLI = func() {
		if mp := r.Media(); mp != nil {
			mp.IntraRefresh()
		}
	}
	// Per-peer rate-limited input logging so we can see whether joining
	// users' packets actually reach the server. Sampled to avoid flooding.
	c.log.Info().Msgf("[INPUT-DIAG] installing OnMessage user=%s idx=%d", user.Id(), user.Index)
	var inputDiagN uint64
	s.OnMessage = func(data []byte) {
		// Debug hook: synthetic rumble injection for multi-user routing tests.
		// Magic header [0xDE, 0xAD, 0xBE, 0xEF, port] → inject a max-strength
		// rumble packet on the given port through the normal data callback,
		// exercising the same per-port user-targeting path a core would hit.
		if len(data) == 5 && data[0] == 0xDE && data[1] == 0xAD && data[2] == 0xBE && data[3] == 0xEF {
			port := data[4]
			c.log.Info().Msgf("[DEBUG-RUMBLE] synthetic inject sender=%s sender_idx=%d target_port=%d", user.Id(), user.Index, port)
			r.App().EmitData([]byte{0xFF, port, 0, 0xFF, 0xFF})
			return
		}
		n := atomic.AddUint64(&inputDiagN, 1)
		if n <= 10 || n%300 == 0 {
			c.log.Info().Msgf("[INPUT-DIAG] rx user=%s idx=%d n=%d bytes=%d", user.Id(), user.Index, n, len(data))
		}
		r.App().Input(user.Index, byte(caged.RetroPad), data)
	}
	if needsKbMouse {
		_ = s.AddChannel("keyboard", func(data []byte) { r.App().Input(user.Index, byte(caged.Keyboard), data) })
		_ = s.AddChannel("mouse", func(data []byte) { r.App().Input(user.Index, byte(caged.Mouse), data) })
	}

	c.RegisterRoom(r.Id())

	// Broadcast updated roster so every connected peer (including the
	// newly-joined one) sees the current slot/identity for everyone in
	// the room. Called after OnMessage install so the new peer can
	// actually receive the broadcast on its data channel.
	c.broadcastRoomMembers(w, r)

	response := api.StartGameResponse{
		Room:    api.Room{Rid: r.Id()},
		Record:  w.conf.Recording.Enabled,
		KbMouse: needsKbMouse,
	}
	if r.App().AspectEnabled() {
		ww, hh := r.App().ViewportSize()
		response.AV = &api.AppVideoInfo{W: ww, H: hh, A: r.App().AspectRatio(), S: int(r.App().Scale())}
	}

	return api.Out{Payload: response}
}

// HandleTerminateSession handles cases when a user has been disconnected from the websocket of coordinator.
func (c *coordinator) HandleTerminateSession(rq api.TerminateSessionRequest, w *Worker) {
	if user := w.router.FindUser(rq.Id); user != nil {
		w.router.Remove(user)
		c.log.Debug().Msgf(">>> users: %v", w.router.Users())
		user.Disconnect()
		if r := w.router.Room(); r != nil {
			c.broadcastRoomMembers(w, r)
		}
	}
}

// broadcastRoomMembers sends the current room roster (each user's id,
// slot, and identity) to every peer connected to the worker. Called on
// membership and slot-assignment mutations so clients can keep the
// slot-picker UI (profile avatars, occupied-dot indicators) in sync
// with reality in real time.
//
// Snapshot, not delta — the payload is the full member list so
// late-joining clients and clients that missed a prior broadcast can
// reconcile without special-case replay logic.
func (c *coordinator) broadcastRoomMembers(w *Worker, r *room.Room[*room.GameSession]) {
	members := make([]api.RoomMember, 0, 8)
	for u := range w.router.Users().Values() {
		members = append(members, api.RoomMember{
			UserId:   u.Id().String(),
			Slot:     u.Index,
			Identity: u.Identity,
		})
	}
	data, err := api.Wrap(api.Out{
		T:       uint8(api.RoomMembers),
		Payload: api.RoomMembersResponse{Members: members},
	})
	if err != nil {
		c.log.Warn().Err(err).Msg("roommembers wrap failed")
		return
	}
	r.Send(data)
}

// HandleQuitGame handles cases when a user manually exits the game.
func (c *coordinator) HandleQuitGame(rq api.GameQuitRequest, w *Worker) {
	r := w.router.Room()
	if r == nil || rq.Rid == "" || rq.Rid != r.Id() {
		return
	}
	w.router.Reset()
	c.log.Debug().Msg("shared room killed")
}

func (c *coordinator) HandleResetGame(rq api.ResetGameRequest, w *Worker) api.Out {
	r := w.router.FindRoom(rq.Rid)
	if r == nil {
		return api.ErrPacket
	}
	// Reset/Save/Load are libretro-only plumbing today. The xemu backend
	// doesn't expose these in its app.App surface yet — silently refuse
	// instead of panicking the unsafe cast. Frontend already tolerates
	// ErrPacket here.
	if !room.IsLibretro(r.App()) {
		c.log.Debug().Msg("reset ignored: backend does not support reset")
		return api.ErrPacket
	}
	room.WithEmulator(r.App()).Reset()
	return api.OkPacket
}

func (c *coordinator) HandleSaveGame(rq api.SaveGameRequest, w *Worker) api.Out {
	r := w.router.FindRoom(rq.Rid)
	if r == nil {
		return api.ErrPacket
	}
	if !room.IsLibretro(r.App()) {
		c.log.Debug().Msg("save ignored: backend does not support save state")
		return api.ErrPacket
	}
	if err := room.WithEmulator(r.App()).SaveGameState(); err != nil {
		c.log.Error().Err(err).Msg("cannot save game state")
		return api.ErrPacket
	}
	return api.OkPacket
}

func (c *coordinator) HandleLoadGame(rq api.LoadGameRequest, w *Worker) api.Out {
	r := w.router.FindRoom(rq.Rid)
	if r == nil {
		return api.ErrPacket
	}
	if !room.IsLibretro(r.App()) {
		c.log.Debug().Msg("load ignored: backend does not support load state")
		return api.ErrPacket
	}
	if err := room.WithEmulator(r.App()).RestoreGameState(); err != nil {
		c.log.Error().Err(err).Msg("cannot load game state")
		return api.ErrPacket
	}
	return api.OkPacket
}

// startXemuRoom is the xemu-specific new-room setup path. It resolves the
// game's ISO path, pokes it into the xemu cage's DvdPath, wires the app
// + media pipeline, and starts the cage. Skipped operations vs the
// libretro path: ReloadFrontend, SetSessionId, SetSaveOnClose,
// EnableCloudStorage, EnableRecording, VideoChangeCb, rcheevos load,
// Vulkan zero-copy — none of which the xemu backend supports today.
func (c *coordinator) startXemuRoom(w *Worker, r *room.Room[*room.GameSession], game games.GameMetadata) error {
	appIface := w.mana.Get(caged.Xemu)
	if appIface == nil {
		return &xemuStartErr{what: "xemu backend not loaded (xemu.enabled=false?)"}
	}
	xcage, ok := appIface.(*xemucage.Caged)
	if !ok {
		return &xemuStartErr{what: "mana.Get(Xemu) returned unexpected type"}
	}
	dvdPath := filepath.Join(w.conf.Library.BasePath, game.Path)
	r.SetApp(appIface)

	m := media.NewWebRtcMediaPipe(w.conf.Encoder.Audio, w.conf.Encoder.Video, w.log)
	m.AudioSrcHz = appIface.AudioSampleRate()
	m.AudioFrames = w.conf.Encoder.Audio.Frames
	m.VideoW, m.VideoH = appIface.ViewportSize()
	m.VideoScale = appIface.Scale()
	r.SetMedia(m)
	if err := m.Init(); err != nil {
		return err
	}
	// ffmpeg x11grab -pix_fmt rgba hands us memory order R,G,B,A, which
	// libyuv calls FOURCC_ABGR (little-endian-register view is 0xABGR,
	// memory bytes are RGBA). The encoder's SetPixFormat routes 4+ to
	// the Abgr path. 0/1/2 are libretro-specific (Rgb0, Argb, RGB565);
	// passing 4 explicitly matches our ffmpeg output byte order.
	// x11grab frames are already top-down; no vertical flip needed.
	m.SetPixFmt(4)
	r.BindAppMedia()
	// Broadcast-to-all data-channel sender is fine for xemu: the backend
	// doesn't emit per-port rumble packets today, so the targeted rumble
	// filter from the libretro path is unnecessary.
	appIface.SetDataCb(r.Send)

	// Fast path: nothing to extract. Start xemu inline — same flow as
	// Phase 7 shipped.
	if !romcache.NeedsHydration(dvdPath) {
		xcage.SetDvd(dvdPath)
		w.log.Info().Str("game", game.Name).Str("iso", game.Path).Msg("xemu room ready")
		r.StartApp()
		return nil
	}

	// Slow path: return success immediately so the coordinator's StartGame
	// RPC doesn't time out at 10 s. Hydration (7z extract + optional
	// extract-xiso repack) takes 30–90 s for a typical Xbox title over
	// the NAS. While we're hydrating, the browser completes WebRTC
	// negotiation against the room; xemu hasn't booted yet so no video
	// frames arrive until the goroutine calls StartApp.
	//
	// Staleness guard: if the router has moved on to a different room
	// (user started a different game before this one finished
	// hydrating), we must NOT call SetDvd on the shared xemu singleton —
	// it would clobber whatever game is actually active. Room-equality
	// against the router is close enough; a tighter CAS is possible but
	// unwarranted until we see a case where it matters.
	w.log.Info().Str("game", game.Name).Str("archive", game.Path).
		Msg("[ROMCACHE] room registered; hydrating before launch")
	go func() {
		resolved, err := w.hyd.Resolve(dvdPath)
		if err != nil {
			w.log.Error().Err(err).Msg("[ROMCACHE] hydrate failed; closing room")
			r.Close()
			return
		}
		if active := w.router.Room(); active != r {
			w.log.Warn().Msg("[ROMCACHE] room no longer active after hydrate; abandoning launch")
			return
		}
		xcage.SetDvd(resolved)
		w.log.Info().Str("game", game.Name).Str("iso", resolved).
			Msg("xemu room ready (post-hydration)")
		r.StartApp()
	}()
	return nil
}

type xemuStartErr struct{ what string }

func (e *xemuStartErr) Error() string { return e.what }

func firstN(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func lastN(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

// HandleSetRaCredentials logs the given user into rcheevos. Host-only
// semantics: whichever user last sent credentials is the one whose
// unlocks we track. Runs in a goroutine so it doesn't block the
// coordinator message loop on the RA server round-trip.
func (c *coordinator) HandleSetRaCredentials(rq api.SetRaCredentialsRequest, w *Worker) {
	user := w.router.FindUser(rq.Id)
	if user == nil {
		w.log.Warn().Msgf("RA creds for unknown user id %s", rq.Id)
		return
	}
	if w.rch == nil {
		w.log.Warn().Msgf("RA creds received but rcheevos client is not initialised")
		return
	}
	w.log.Info().Msgf("RA login for user=%q (identity=%s) token.len=%d token.head=%q token.tail=%q",
		rq.User, user.Identity.Sub, len(rq.Token), firstN(rq.Token, 2), lastN(rq.Token, 2))
	go func(client *Worker, raUser, raToken, sub string) {
		if err := client.rch.Login(raUser, raToken); err != nil {
			client.log.Warn().Err(err).Msgf("RA login failed for %s", raUser)
			return
		}
		client.log.Info().Msgf("RA login ok: %s (cloudplay user=%s)", client.rch.User(), sub)
	}(w, rq.User, rq.Token, user.Identity.Sub)
}

func (c *coordinator) HandleChangePlayer(rq api.ChangePlayerRequest, w *Worker) api.Out {
	user := w.router.FindUser(rq.Id)
	r := w.router.FindRoom(rq.Rid)
	if user == nil || r == nil {
		return api.Out{Payload: -1} // semi-predicates
	}
	user.Index = rq.Index
	w.log.Info().Msgf("Updated player index to: %d", rq.Index)
	c.broadcastRoomMembers(w, r)
	return api.Out{Payload: rq.Index}
}

func (c *coordinator) HandleRecordGame(rq api.RecordGameRequest, w *Worker) api.Out {
	if !w.conf.Recording.Enabled {
		return api.ErrPacket
	}
	r := w.router.FindRoom(rq.Rid)
	if r == nil {
		return api.ErrPacket
	}
	room.WithRecorder(r.App()).ToggleRecording(rq.Active, rq.User)
	return api.OkPacket
}

// fromBase64Json decodes data from a URL-encoded Base64+JSON string.
func fromBase64Json(data string, obj any) error {
	b, err := base64.URLEncoding.DecodeString(data)
	if err != nil {
		return err
	}
	err = json.Unmarshal(b, obj)
	if err != nil {
		return err
	}
	return nil
}

// toBase64Json encodes data to a URL-encoded Base64+JSON string.
func toBase64Json(data any) (string, error) {
	if data == nil {
		return "", nil
	}
	b, err := json.Marshal(data)
	if err != nil {
		return "", err
	}
	return base64.URLEncoding.EncodeToString(b), nil
}
