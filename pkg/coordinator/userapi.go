package coordinator

import (
	"unsafe"

	"github.com/giongto35/cloud-game/v3/pkg/api"
	"github.com/giongto35/cloud-game/v3/pkg/config"
)

// CheckLatency sends a list of server addresses to the user
// and waits get back this list with tested ping times for each server.
func (u *User) CheckLatency(req api.CheckLatencyUserResponse) (api.CheckLatencyUserRequest, error) {
	dat, err := api.UnwrapChecked[api.CheckLatencyUserRequest](u.Send(api.CheckLatency, req))
	if dat == nil {
		return api.CheckLatencyUserRequest{}, err
	}
	return *dat, nil
}

// InitSession signals the user that the app is ready to go.
// roomId is the active room at the time the worker was selected; it is
// passed explicitly to avoid a data race with the worker's message loop
// which can clear worker.RoomId via HandleCloseRoom at any time.
func (u *User) InitSession(wid string, ice []config.IceServer, games []api.AppMeta, roomId string) {
	u.Notify(api.InitSession, api.InitSessionUserResponse{
		Ice:      *(*[]api.IceServer)(unsafe.Pointer(&ice)), // don't do this at home
		Games:    games,
		Wid:      wid,
		RoomId:   roomId,
		Identity: u.identity,
	})
}

// SendWebrtcOffer sends SDP offer back to the user.
func (u *User) SendWebrtcOffer(sdp string) { u.Notify(api.WebrtcOffer, sdp) }

// SendWebrtcIceCandidate sends remote ICE candidate back to the user.
func (u *User) SendWebrtcIceCandidate(candidate string) { u.Notify(api.WebrtcIce, candidate) }

// StartGame signals the user that everything is ready to start a game.
func (u *User) StartGame(av *api.AppVideoInfo, kbMouse bool) {
	u.Notify(api.StartGame, api.GameStartUserResponse{RoomId: u.w.RoomId, Av: av, KbMouse: kbMouse})
}
