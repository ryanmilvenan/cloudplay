package worker

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/giongto35/cloud-game/v3/pkg/api"
	"github.com/giongto35/cloud-game/v3/pkg/config"
	"github.com/giongto35/cloud-game/v3/pkg/games"
	"github.com/giongto35/cloud-game/v3/pkg/logger"
	"github.com/giongto35/cloud-game/v3/pkg/monitoring"
	"github.com/giongto35/cloud-game/v3/pkg/network"
	"github.com/giongto35/cloud-game/v3/pkg/network/httpx"
	"github.com/giongto35/cloud-game/v3/pkg/worker/caged"
	"github.com/giongto35/cloud-game/v3/pkg/worker/caged/libretro"
	"github.com/giongto35/cloud-game/v3/pkg/worker/cloud"
	"github.com/giongto35/cloud-game/v3/pkg/worker/agent"
	"github.com/giongto35/cloud-game/v3/pkg/worker/enricher"
	"github.com/giongto35/cloud-game/v3/pkg/worker/rcheevos"
	"github.com/giongto35/cloud-game/v3/pkg/worker/romcache"
	"github.com/giongto35/cloud-game/v3/pkg/worker/room"
	"github.com/giongto35/cloud-game/v3/pkg/worker/search"
)

type Worker struct {
	address  string
	conf     config.WorkerConfig
	cord     *coordinator
	lib      games.GameLibrary
	launcher games.Launcher
	log      *logger.Logger
	mana     *caged.Manager
	router   *room.GameRouter
	services [2]interface {
		Run()
		Stop() error
	}
	storage cloud.Storage

	// rch is the singleton rcheevos client used to log hosts in,
	// evaluate achievements, and post unlocks. Created once per worker
	// process; a failure here is non-fatal — the worker just won't
	// track achievements until the problem is resolved.
	rch *rcheevos.Client

	// hyd extracts .7z-archived ROMs on first play and removes the
	// source archive afterwards so the library sees a normal ISO. Shared
	// across sessions so concurrent launches of the same archive
	// serialize correctly.
	hyd *romcache.Hydrator

	// enr handles per-game IGDB metadata backfill. Nil when
	// conf.Igdb.Enabled is false or credentials aren't readable. When
	// present, it hydrates GameMetadata during library.Scan from its
	// on-disk cache and queues misses for background lookup.
	enr *enricher.Enricher
}

func New(conf config.WorkerConfig, log *logger.Logger) (*Worker, error) {
	manager := caged.NewManager(log)
	if err := manager.Load(caged.Libretro, conf); err != nil {
		return nil, fmt.Errorf("couldn't cage libretro: %v", err)
	}
	// Best-effort xemu bring-up: Load is a no-op when XemuConfig.Enabled
	// is false, so existing deployments without xemu configured still
	// start normally. When enabled, a start failure (BIOS missing,
	// binary not installed) logs but does NOT abort worker startup —
	// libretro still works.
	if err := manager.Load(caged.Xemu, conf); err != nil {
		log.Warn().Err(err).Msg("xemu backend unavailable — xbox games will fail to start")
	}
	// Flycast native backend is best-effort like xemu: disabled-by-default in
	// config so unaffected deploys keep libretro-DC. When enabled, a start
	// failure logs but does NOT abort worker startup.
	if err := manager.Load(caged.Flycast, conf); err != nil {
		log.Warn().Err(err).Msg("flycast native backend unavailable — backend=flycast games will fail to start")
	}

	library := games.NewLib(conf.Library, conf.Emulator, log)

	// IGDB enrichment (Phase 2). Best-effort: a credential file that
	// won't open, a cache path that can't be created, or igdb.enabled=false
	// all leave `enr` nil — library.Scan still runs, just without any
	// genre/year/summary data on game cards. Constructed BEFORE
	// library.Scan so SetEnrichFn fires against the very first scan.
	var enr *enricher.Enricher
	if conf.Igdb.Enabled {
		cli, err := enricher.NewClient(conf.Igdb.CredentialsFile)
		if err != nil {
			log.Warn().Err(err).Msg("IGDB enrichment disabled — couldn't load credentials")
		} else {
			cache, err := enricher.NewCache(conf.Igdb.CachePath)
			if err != nil {
				log.Warn().Err(err).Msg("IGDB enrichment disabled — cache init failed")
			} else {
				enr = enricher.New(cli, cache, conf.Igdb.RequestsPerSecond, conf.Igdb.MinConfidence, log)
				library.SetEnrichFn(func(g *games.GameMetadata) {
					// Hydrate from cache (zero I/O beyond SQLite); queue for
					// background lookup when the cache has nothing yet.
					enr.ApplyCached(g)
					enr.Enqueue(*g)
				})
				log.Info().Msg("[IGDB] enricher armed")
			}
		}
	}

	// Phase-3 semantic search: wire the vLLM embedder + in-memory
	// vector index into the enricher so each IGDB-matched game also
	// gets embedded and indexed. The HTTP handler exposed below
	// reuses the same index. Requires Igdb.Enabled + Search.Enabled
	// — the embedder relies on the IGDB enrichment text as the
	// source of truth for each game.
	var searchIndex *search.Index
	var searchEmbedder *search.Embedder
	if conf.Search.Enabled && enr != nil {
		searchEmbedder = search.NewEmbedder(conf.Search.EmbedURL, conf.Search.EmbedModel)
		searchIndex = search.NewIndex()
		enr.AttachSemanticSearch(searchEmbedder, searchIndex)
		if n, err := enr.LoadEmbeddingsFromCache(); err != nil {
			log.Warn().Err(err).Msg("[SEARCH] failed to seed index from cache")
		} else {
			log.Info().Int("count", n).Msg("[SEARCH] index seeded from cache")
		}
		// Backfill vectors for IGDB rows that predate Phase 3's arrival.
		// Runs in the background at the configured RPS; new-game enrichments
		// continue in parallel through the regular Enqueue path.
		go enr.BackfillEmbeddingsFromIGDB(context.Background())
	}

	library.Scan()

	worker := &Worker{
		conf:     conf,
		lib:      library,
		launcher: games.NewGameLauncher(library),
		log:      log,
		mana:     manager,
		router:   room.NewGameRouter(),
		hyd:      &romcache.Hydrator{Log: log},
		enr:      enr,
	}

	// Kick the enricher's background loop. OnBatchComplete re-broadcasts
	// the library so newly-enriched fields reach connected browsers
	// without a page reload. Uses context.Background — the loop is tied
	// to the process lifetime; worker has no explicit shutdown context
	// to cancel against today.
	if enr != nil {
		enr.OnBatchComplete = func() {
			if worker.cord != nil {
				// Re-apply cache into the library (cheap) so GetAll()
				// returns the enriched rows, then broadcast.
				worker.lib.Scan()
				worker.cord.SendLibrary(worker)
			}
		}
		go enr.Run(context.Background())
	}

	// Phase-4 conversational agent. Wires Ollama + retrieval into
	// POST /v1/agent/turn. Requires Search + IGDB to be useful;
	// deps are nil-tolerant so a mis-configured agent still serves
	// a "say: can't help" reply rather than 5xx.
	var agentHandler *agent.Handler
	if conf.Agent.Enabled {
		ollamaURL := conf.Agent.OllamaURL
		if ollamaURL == "" {
			ollamaURL = "http://localhost:11434"
		}
		model := conf.Agent.Model
		if model == "" {
			model = "gemma4:e4b"
		}
		timeout := time.Duration(conf.Agent.TimeoutMs) * time.Millisecond
		ollama := agent.NewOllamaClient(ollamaURL, model, timeout)
		var cache *enricher.Cache
		if enr != nil {
			cache = enr.CacheHandle()
		}
		agentHandler = agent.NewHandler(ollama, library, cache, searchIndex, searchEmbedder, conf.Agent.TopK, log)
		log.Info().Str("model", model).Str("url", ollamaURL).Msg("[AGENT] armed")
	}

	h, err := httpx.NewServer(
		conf.Worker.GetAddr(),
		func(s *httpx.Server) httpx.Handler {
			mux := s.Mux().HandleW(conf.Worker.Network.PingEndpoint, func(w httpx.ResponseWriter) {
				w.Header().Set("Access-Control-Allow-Origin", "*")
				_, _ = w.Write([]byte{0x65, 0x63, 0x68, 0x6f}) // echo
			})
			// Phase-3 semantic search endpoint. Registered only when
			// the index+embedder are live so probes against a disabled
			// config return 404, not a half-wired 500.
			if searchIndex != nil && searchEmbedder != nil {
				mux.Handle("/v1/search/semantic", search.NewHandler(searchEmbedder, searchIndex, log))
			}
			// Phase-4 conversational agent.
			if agentHandler != nil {
				mux.Handle("/v1/agent/turn", agentHandler)
			}
			return mux
		},
		httpx.WithServerConfig(conf.Worker.Server),
		httpx.HttpsRedirect(false),
		httpx.WithPortRoll(true),
		httpx.WithZone(conf.Worker.Network.Zone),
		httpx.WithLogger(log),
	)
	if err != nil {
		return nil, fmt.Errorf("http init fail: %w", err)
	}
	worker.address = h.Addr
	worker.services[0] = h
	if conf.Worker.Monitoring.IsEnabled() {
		worker.services[1] = monitoring.New(conf.Worker.Monitoring, h.GetHost(), log)
	}
	st, err := cloud.Store(conf.Storage, log)
	if err != nil {
		log.Warn().Err(err).Msgf("cloud storage fail, using no storage")
	}
	worker.storage = st

	if rch, rerr := rcheevos.NewClient(log); rerr != nil {
		log.Warn().Err(rerr).Msgf("rcheevos init fail; achievements disabled")
	} else {
		worker.rch = rch
		// Read RAM from the currently loaded game (if any) into rcheevos.
		rch.SetMemoryReader(func(addr uint32, dst []byte) uint32 {
			ram := libretro.SystemRAM()
			if ram == nil {
				return 0
			}
			if uint64(addr) >= uint64(len(ram)) {
				return 0
			}
			n := copy(dst, ram[addr:])
			return uint32(n)
		})
		// Run trigger evaluation once per emulator tick. Cheap when
		// no game is loaded into rc_client.
		libretro.SetTickHook(rch.DoFrame)
		// Broadcast unlocks to every peer in the current room.
		rch.SetAchievementHandler(func(u rcheevos.Unlock) {
			log.Info().Msgf("achievement unlocked: %s (%d pts)", u.Title, u.Points)
			r := worker.router.Room()
			if r == nil {
				return
			}
			data, err := api.Wrap(api.Out{
				T: uint8(api.AchievementUnlocked),
				Payload: api.AchievementUnlockedResponse{
					ID:          u.ID,
					Title:       u.Title,
					Description: u.Description,
					Points:      u.Points,
					BadgeURL:    u.BadgeURL,
				},
			})
			if err != nil {
				log.Error().Err(err).Msgf("wrap AchievementUnlocked")
				return
			}
			r.Send(data)
		})
	}

	return worker, nil
}

func (w *Worker) Reset() { w.router.Reset() }

func (w *Worker) Start(done chan struct{}) {
	for _, s := range w.services {
		if s != nil {
			s.Run()
		}
	}

	// !to restore alive worker info when coordinator connection was lost
	retry := network.NewRetry()

	onRetryFail := func(err error) {
		w.log.Warn().Err(err).Msgf("socket fail. Retrying in %v", retry.Time())
		retry.Fail().Multiply(2)
	}

	go func() {
		remoteAddr := w.conf.Worker.Network.CoordinatorAddress
		defer func() {
			if w.cord != nil {
				w.cord.Disconnect()
			}
			w.Reset()
		}()

		for {
			select {
			case <-done:
				return
			default:
				w.Reset()
				cord, err := newCoordinatorConnection(remoteAddr, w.conf.Worker, w.address, w.log)
				if err != nil {
					onRetryFail(err)
					continue
				}
				cord.SetErrorHandler(onRetryFail)
				w.cord = cord
				w.cord.log.Info().Msgf("Connected to the coordinator %v", remoteAddr)
				wait := w.cord.HandleRequests(w)
				w.cord.SendLibrary(w)
				w.cord.SendPrevSessions(w)
				<-wait
				retry.Success()
			}
		}
	}()
}

func (w *Worker) Stop() error {
	var err error
	for _, s := range w.services {
		if s != nil {
			err0 := s.Stop()
			err = errors.Join(err, err0)
		}
	}
	if w.rch != nil {
		w.rch.Close()
		w.rch = nil
	}
	return err
}
