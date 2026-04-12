package coordinator

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/url"

	"github.com/giongto35/cloud-game/v3/pkg/api"
	"github.com/giongto35/cloud-game/v3/pkg/com"
	"github.com/giongto35/cloud-game/v3/pkg/config"
	"github.com/giongto35/cloud-game/v3/pkg/logger"
)

type Connection interface {
	Disconnect()
	Id() com.Uid
	ProcessPackets(func(api.In[com.Uid]) error) chan struct{}

	Send(api.PT, any) ([]byte, error)
	Notify(api.PT, any)
}

type Hub struct {
	conf    config.CoordinatorConfig
	log     *logger.Logger
	users   com.NetMap[com.Uid, *User]
	workers com.NetMap[com.Uid, *Worker]
}

func NewHub(conf config.CoordinatorConfig, log *logger.Logger) *Hub {
	return &Hub{
		conf:    conf,
		users:   com.NewNetMap[com.Uid, *User](),
		workers: com.NewNetMap[com.Uid, *Worker](),
		log:     log,
	}
}

// handleUserConnection handles all connections from user/frontend.
func (h *Hub) handleUserConnection() http.HandlerFunc {
	var connector com.Server
	connector.Origin(h.conf.Coordinator.Origin.UserWs)

	log := h.log.Extend(h.log.With().
		Str(logger.ClientField, "u").
		Str(logger.DirectionField, logger.MarkIn),
	)

	return func(w http.ResponseWriter, r *http.Request) {
		// Parse ingress-injected identity headers *before* the WS
		// upgrade, while r.Header still carries them. These come from
		// oauth2-proxy (real users) or the chain-claude-test bypass
		// middleware (the Claude agent); see pkg/api/identity.go.
		identity := api.IdentityFromHeaders(r.Header)

		h.log.Debug().Msgf("Handshake %v identity=%s/%s anon=%v",
			r.Host, identity.Sub, identity.Username, identity.IsAnonymous())

		conn, err := connector.Connect(w, r)
		if err != nil {
			h.log.Error().Err(err).Msg("user connection fail")
			return
		}

		user := NewUser(conn, log)
		user.SetIdentity(identity)
		defer h.users.RemoveDisconnect(user)
		done := user.HandleRequests(h, h.conf)
		params := r.URL.Query()

		worker := h.findWorkerFor(user, params, h.log.Extend(h.log.With().Str("cid", user.Id().Short())))
		if worker == nil {
			user.Notify(api.ErrNoFreeSlots, "")
			h.log.Info().Msg("no free workers")
			return
		}

		// Snapshot the active room ID now, before any concurrent goroutine
		// (e.g. the worker's message loop) can clear it via HandleCloseRoom.
		// Without this snapshot, InitSession reads worker.RoomId under a race.
		activeRoomId := worker.RoomId

		// Link the user to the selected worker. Slot reservation is handled later
		// on game start; this keeps connections lightweight and lets deep-link
		// joins share a worker without consuming its single game slot.
		user.w = worker

		h.users.Add(user)

		apps := worker.AppNames()
		list := make([]api.AppMeta, len(apps))
		for i := range apps {
			list[i] = api.AppMeta{Alias: apps[i].Alias, Title: apps[i].Name, System: apps[i].System}
		}

		user.InitSession(worker.Id().String(), h.conf.Webrtc.IceServers, list, activeRoomId)
		log.Info().Str(logger.DirectionField, logger.MarkPlus).Msgf("user %s", user.Id())
		<-done
	}
}

func RequestToHandshake(data string) (*api.ConnectionRequest[com.Uid], error) {
	if data == "" {
		return nil, api.ErrMalformed
	}
	handshake, err := api.UnwrapChecked[api.ConnectionRequest[com.Uid]](base64.URLEncoding.DecodeString(data))
	if err != nil || handshake == nil {
		return nil, fmt.Errorf("%w (%v)", err, handshake)
	}
	return handshake, nil
}

// handleWorkerConnection handles all connections from a new worker to coordinator.
func (h *Hub) handleWorkerConnection() http.HandlerFunc {
	var connector com.Server
	connector.Origin(h.conf.Coordinator.Origin.WorkerWs)

	log := h.log.Extend(h.log.With().
		Str(logger.ClientField, "w").
		Str(logger.DirectionField, logger.MarkIn),
	)

	h.log.Debug().Msgf("WS max message size: %vb", h.conf.Coordinator.MaxWsSize)

	return func(w http.ResponseWriter, r *http.Request) {
		h.log.Debug().Msgf("Handshake %v", r.Host)

		handshake, err := RequestToHandshake(r.URL.Query().Get(api.DataQueryParam))
		if err != nil {
			h.log.Error().Err(err).Msg("handshake fail")
			return
		}

		if handshake.PingURL == "" {
			h.log.Warn().Msg("Ping address is not set")
		}

		if h.conf.Coordinator.Server.Https && !handshake.IsHTTPS {
			h.log.Warn().Msg("Unsecure worker connection. Unsecure to secure may be bad.")
		}

		// set connection uid from the handshake
		if handshake.Id != com.NilUid {
			h.log.Debug().Msgf("Worker uid will be set to %v", handshake.Id)
		}

		conn, err := connector.Connect(w, r)
		if err != nil {
			log.Error().Err(err).Msg("worker connection fail")
			return
		}
		conn.SetMaxReadSize(h.conf.Coordinator.MaxWsSize)

		worker := NewWorker(conn, *handshake, log)
		defer func() {
			h.DetachWorkerUsers(worker.Id())
			h.workers.RemoveDisconnect(worker)
		}()
		done := worker.HandleRequests(h)
		h.workers.Add(worker)
		log.Info().
			Str(logger.DirectionField, logger.MarkPlus).
			Msgf("worker %s", worker.PrintInfo())
		<-done
	}
}

func (h *Hub) Find(id string) *User {
	return h.users.Find(id)
}

// NotifyWorkerUsers sends a packet to every user currently linked to the given worker.
func (h *Hub) NotifyWorkerUsers(wid com.Uid, t api.PT, payload any) {
	for u := range h.users.Values() {
		if u.w != nil && u.w.Id() == wid {
			u.Notify(t, payload)
		}
	}
}

func (h *Hub) DetachWorkerUsers(wid com.Uid) {
	for u := range h.users.Values() {
		if u.w != nil && u.w.Id() == wid {
			u.w = nil
			u.Notify(api.QuitGame, nil)
		}
	}
}

func (h *Hub) PickAvailableWorker() *Worker {
	return h.find1stFreeWorker("")
}

func (h *Hub) GetServerList() (r []api.Server) {
	debug := h.conf.Coordinator.Debug
	for w := range h.workers.Values() {
		server := api.Server{
			Addr:    w.Addr,
			Id:      w.Id(),
			IsBusy:  !w.HasSlot(),
			Machine: string(w.Id().Machine()),
			PingURL: w.PingServer,
			Port:    w.Port,
			Tag:     w.Tag,
			Zone:    w.Zone,
		}
		if debug {
			server.Room = w.RoomId
		}
		r = append(r, server)
	}
	return
}

// findWorkerFor searches a free worker for the user depending on
// various conditions.
func (h *Hub) findWorkerFor(usr *User, q url.Values, log *logger.Logger) *Worker {
	log.Debug().Msg("Search available workers")
	roomIdRaw := q.Get(api.RoomIdQueryParam)
	sessionId, deepRoomId := api.ExplodeDeepLink(roomIdRaw)
	roomId := roomIdRaw
	if deepRoomId != "" {
		roomId = deepRoomId
	}
	zone := q.Get(api.ZoneQueryParam)
	wid := q.Get(api.WorkerIdParam)

	var worker *Worker

	if wid != "" {
		if worker = h.findWorkerById(wid, h.conf.Coordinator.Debug); worker != nil {
			log.Debug().Msgf("Worker with id: %v has been found", wid)
			return worker
		} else {
			return nil
		}
	}

	if worker = h.findWorkerByRoom(roomIdRaw, roomId, zone); worker != nil {
		log.Debug().Str("room", roomId).Msg("An existing worker has been found")
	} else if worker = h.findWorkerByPreviousRoom(sessionId); worker != nil {
		log.Debug().Msgf("Worker %v with the previous room: %v is found", wid, roomId)
	} else if roomIdRaw == "" {
		// No room specified: prefer a fresh free worker first.
		//
		// This avoids routing plain visits onto a stale "active" room when a
		// crashed/restarting worker briefly lingers in coordinator state. Users
		// who explicitly intend to join an existing session should arrive with a
		// room id / deep link instead.
		if worker = h.find1stFreeWorker(zone); worker != nil {
			log.Debug().Msg("Found next free worker")
		} else if worker = h.find1stActiveWorker(zone); worker != nil {
			log.Debug().Str("room", worker.RoomId).Msg("Found active shared room (no free worker available)")
		}
	} else {
		// Room ID was specified but no exact match found. Try to join an active
		// shared session first (covers stale localStorage room IDs from prior
		// sessions), then fall back to a free worker or ping-based selection.
		if worker = h.find1stActiveWorker(zone); worker != nil {
			log.Debug().Str("room", worker.RoomId).Msg("Joined active shared room (stale room_id fallback)")
		} else {
			switch h.conf.Coordinator.Selector {
			case config.SelectByPing:
				log.Debug().Msgf("Searching fastest free worker...")
				if worker = h.findFastestWorker(zone,
					func(servers []string) (map[string]int64, error) { return usr.CheckLatency(servers) }); worker != nil {
					log.Debug().Msg("The fastest worker has been found")
				}
			default:
				log.Debug().Msgf("Searching any free worker...")
				if worker = h.find1stFreeWorker(zone); worker != nil {
					log.Debug().Msgf("Found next free worker")
				}
			}
		}
	}
	return worker
}

func (h *Hub) find1stActiveWorker(region string) *Worker {
	for w := range h.workers.Values() {
		if w.RoomId != "" && w.In(region) {
			return w
		}
	}
	return nil
}

func (h *Hub) findWorkerByPreviousRoom(id string) *Worker {
	if id == "" {
		return nil
	}
	w, _ := h.workers.FindBy(func(w *Worker) bool {
		// session and room id are the same
		return w.HadSession(id) && w.HasSlot()
	})
	return w
}

func (h *Hub) findWorkerByRoom(id string, deepId string, region string) *Worker {
	if id == "" && deepId == "" {
		return nil
	}
	// if there is zone param, we need to ensure the worker in that zone,
	// if not we consider the room is missing
	w, _ := h.workers.FindBy(func(w *Worker) bool {
		matchId := w.RoomId == id
		if !matchId && deepId != "" {
			matchId = w.RoomId == deepId
		}
		return matchId && w.In(region)
	})
	return w
}

func (h *Hub) getAvailableWorkers(region string) []*Worker {
	var workers []*Worker
	for w := range h.workers.Values() {
		if w.HasSlot() && w.In(region) {
			workers = append(workers, w)
		}
	}
	return workers
}

func (h *Hub) find1stFreeWorker(region string) *Worker {
	workers := h.getAvailableWorkers(region)
	if len(workers) > 0 {
		return workers[0]
	}
	return nil
}

// findFastestWorker returns the best server for a session.
// All workers addresses are sent to user and user will ping to get latency.
// !to rewrite
func (h *Hub) findFastestWorker(region string, fn func(addresses []string) (map[string]int64, error)) *Worker {
	workers := h.getAvailableWorkers(region)
	if len(workers) == 0 {
		return nil
	}

	var addresses []string
	group := map[string][]struct{}{}
	for _, w := range workers {
		if _, ok := group[w.PingServer]; !ok {
			addresses = append(addresses, w.PingServer)
		}
		group[w.PingServer] = append(group[w.PingServer], struct{}{})
	}

	latencies, err := fn(addresses)
	if len(latencies) == 0 || err != nil {
		return nil
	}

	workers = h.getAvailableWorkers(region)
	if len(workers) == 0 {
		return nil
	}

	var bestWorker *Worker
	var minLatency int64 = 1<<31 - 1
	// get a worker with the lowest latency
	for addr, ping := range latencies {
		if ping < minLatency {
			for _, w := range workers {
				if w.PingServer == addr {
					bestWorker = w
				}
			}
			minLatency = ping
		}
	}
	return bestWorker
}

func (h *Hub) findWorkerById(id string, useAllWorkers bool) *Worker {
	if id == "" {
		return nil
	}

	uid, err := com.UidFromString(id)
	if err != nil {
		return nil
	}

	for _, w := range h.getAvailableWorkers("") {
		if w.Id() == com.NilUid {
			continue
		}
		if useAllWorkers {
			if uid == w.Id() {
				return w
			}
		} else {
			// select any worker on the same machine when workers are grouped on the client
			if bytes.Equal(uid.Machine(), w.Id().Machine()) {
				return w
			}
		}
	}

	return nil
}
