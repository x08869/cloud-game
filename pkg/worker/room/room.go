package room

import (
	"bytes"
	"encoding/gob"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"math"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/giongto35/cloud-game/v2/pkg/config/worker"
	"github.com/giongto35/cloud-game/v2/pkg/emulator"
	"github.com/giongto35/cloud-game/v2/pkg/emulator/libretro/nanoarch"
	"github.com/giongto35/cloud-game/v2/pkg/encoder"
	"github.com/giongto35/cloud-game/v2/pkg/games"
	"github.com/giongto35/cloud-game/v2/pkg/logger"
	"github.com/giongto35/cloud-game/v2/pkg/session"
	"github.com/giongto35/cloud-game/v2/pkg/storage"
	"github.com/giongto35/cloud-game/v2/pkg/webrtc"
)

// Room is a game session. multi webRTC sessions can connect to a same game.
// A room stores all the channel for interaction between all webRTCs session and emulator
type Room struct {
	ID string

	// imageChannel is image stream received from director
	imageChannel <-chan nanoarch.GameFrame
	// audioChannel is audio stream received from director
	audioChannel <-chan []int16
	// inputChannel is input stream send to director. This inputChannel is combined
	// input from webRTC + connection info (player index)
	inputChannel chan<- nanoarch.InputEvent
	// State of room
	IsRunning bool
	// Done channel is to fire exit event when room is closed
	Done chan struct{}
	// List of peer connections in the room
	rtcSessions []*webrtc.WebRTC
	// NOTE: Not in use, lock rtcSessions
	sessionsLock *sync.Mutex
	// Director is emulator
	director emulator.CloudEmulator
	// Cloud storage to store room state online
	onlineStorage storage.CloudStorage

	vPipe *encoder.VideoPipe
	log   *logger.Logger
}

const (
	bufSize        = 245969
	SocketAddrTmpl = "/tmp/cloudretro-retro-%s.sock"
)

// NewVideoImporter return image Channel from stream
func NewVideoImporter(id string, log *logger.Logger) chan nanoarch.GameFrame {
	sockAddr := fmt.Sprintf(SocketAddrTmpl, id)
	imgChan := make(chan nanoarch.GameFrame)

	l, err := net.Listen("unix", sockAddr)
	if err != nil {
		log.Fatal().Err(err).Msg("socket error")
	}

	go func(l net.Listener) {
		defer l.Close()

		conn, err := l.Accept()
		if err != nil {
			log.Fatal().Err(err).Send()
		}
		defer conn.Close()

		log.Info().Msg("Received new conn")

		fullBuf := make([]byte, bufSize*2)
		fullBuf = fullBuf[:0]

		for {
			// TODO: Not reallocate
			buf := make([]byte, bufSize)
			l, err := conn.Read(buf)
			if err != nil {
				if err != io.EOF {
					log.Error().Err(err).Send()
				}
				continue
			}

			buf = buf[:l]
			fullBuf = append(fullBuf, buf...)
			if len(fullBuf) >= bufSize {
				buff := bytes.NewBuffer(fullBuf)
				dec := gob.NewDecoder(buff)

				frame := nanoarch.GameFrame{}
				if err := dec.Decode(&frame); err != nil {
					log.Fatal().Err(err)
				}
				imgChan <- frame
				fullBuf = fullBuf[bufSize:]
			}
		}
	}(l)

	return imgChan
}

// NewRoom creates a new room
func NewRoom(id string, game games.GameMetadata, storage storage.CloudStorage, cfg worker.Config, log *logger.Logger) *Room {
	if id == "" {
		id = session.GenerateRoomID(game.Name)
	}
	log = log.Extend(log.With().Str("room", id[:5]))
	log.Info().Str("game", game.Name).Msg("")

	inputChannel := make(chan nanoarch.InputEvent, 100)
	room := &Room{
		ID:            id,
		inputChannel:  inputChannel,
		imageChannel:  nil,
		rtcSessions:   []*webrtc.WebRTC{},
		sessionsLock:  &sync.Mutex{},
		IsRunning:     true,
		onlineStorage: storage,
		Done:          make(chan struct{}, 1),
		log:           log,
	}

	// Check if room is on local storage, if not, pull from GCS to local storage
	go func(game games.GameMetadata, roomID string) {
		store := nanoarch.Storage{
			Path:     cfg.Emulator.Storage,
			MainSave: roomID,
		}

		// Check room is on local or fetch from server
		log.Info().Msg("Check if the room in the cloud")
		if err := room.saveOnlineRoomToLocal(roomID, store.GetSavePath()); err != nil {
			log.Warn().Err(err).Msg("The room is not in the cloud")
		}

		// If not then load room or create room from local.
		log.Info().Str("game", game.Name).Msg("The room is opened")

		// Spawn new emulator and plug-in all channels
		emuName := cfg.Emulator.GetEmulator(game.Type, game.Path)
		libretroConfig := cfg.Emulator.GetLibretroCoreConfig(emuName)

		// Run without game, image stream is communicated over a unix socket
		if cfg.Encoder.WithoutGame {
			room.imageChannel = NewVideoImporter(roomID, log)
			room.director, _, room.audioChannel = nanoarch.Init(roomID, false, inputChannel, store, libretroConfig, log)
		} else {
			room.director, room.imageChannel, room.audioChannel = nanoarch.Init(roomID, true, inputChannel, store, libretroConfig, log)
		}

		gameMeta := room.director.LoadMeta(filepath.Join(game.Base, game.Path))

		// nwidth, nheight are the WebRTC output size
		var nwidth, nheight int
		emu, ar := cfg.Emulator, cfg.Emulator.AspectRatio

		if ar.Keep {
			baseAspectRatio := float64(gameMeta.BaseWidth) / float64(ar.Height)
			nwidth, nheight = resizeToAspect(baseAspectRatio, ar.Width, ar.Height)
			log.Info().Msgf("Viewport size will be changed from %dx%d (%f) -> %dx%d", ar.Width, ar.Height,
				baseAspectRatio, nwidth, nheight)
		} else {
			nwidth, nheight = gameMeta.BaseWidth, gameMeta.BaseHeight
			log.Info().Msgf("Viewport custom size is disabled, base size will be used instead %dx%d", nwidth, nheight)
		}

		if emu.Scale > 1 {
			nwidth, nheight = nwidth*emu.Scale, nheight*emu.Scale
			log.Info().Msgf("Viewport size has scaled to %dx%d", nwidth, nheight)
		}

		// set game frame size considering its orientation
		encoderW, encoderH := nwidth, nheight
		if gameMeta.Rotation.IsEven {
			encoderW, encoderH = nheight, nwidth
		}

		room.director.SetViewport(encoderW, encoderH)

		go room.startVideo(encoderW, encoderH, func(frame encoder.OutFrame) {
			// TODO: r.rtcSessions is rarely updated. Lock will hold down perf
			// todo fix these races ffs rwmutex
			for _, webRTC := range room.rtcSessions {
				if !webRTC.IsConnected() {
					break
				}
				// NOTE: can block here
				webRTC.ImageChannel <- webrtc.VideoFrame{Data: frame.Data, Timestamp: frame.Timestamp}
			}
		}, cfg.Encoder.Video)

		dur := time.Duration(cfg.Encoder.Audio.Frame) * time.Millisecond
		go room.startAudio(gameMeta.AudioSampleRate, func(audio []byte) {
			for _, p2p := range room.rtcSessions {
				if !p2p.IsConnected() {
					continue
				}
				// NOTE: can block here
				p2p.AudioChannel <- webrtc.AudioFrame{Data: audio, Duration: dur}
			}
		}, cfg.Encoder.Audio)

		room.director.Start()
	}(game, id)
	return room
}

func resizeToAspect(ratio float64, sw int, sh int) (dw int, dh int) {
	// ratio is always > 0
	dw = int(math.Round(float64(sh)*ratio/2) * 2)
	dh = sh
	if dw > sw {
		dw = sw
		dh = int(math.Round(float64(sw)/ratio/2) * 2)
	}
	return
}

func isGameOnLocal(path string) bool {
	file, err := os.Open(path)
	if err == nil {
		defer func() {
			_ = file.Close()
		}()
	}
	return !errors.Is(err, os.ErrNotExist)
}

func (r *Room) AddConnectionToRoom(peer *webrtc.WebRTC) {
	r.rtcSessions = append(r.rtcSessions, peer)
	peer.SetRoom(r.ID)
}

func (r *Room) UpdatePlayerIndex(peerconnection *webrtc.WebRTC, playerIndex int) {
	r.log.Info().Msgf("Updated player index to: %d", playerIndex)
	peerconnection.PlayerIndex = playerIndex
}

func (r *Room) PollUserInput(peerconnection *webrtc.WebRTC) {
	r.log.Debug().Msg("Start user input poll")
	// bug: when input channel here = nil, skip and finish
	for {
		select {
		case <-r.Done:
			r.log.Debug().Msg("Stop user input poll")
			return
		case input := <-peerconnection.InputChannel:
			if !peerconnection.IsConnected() {
				break
			}
			select {
			case r.inputChannel <- nanoarch.InputEvent{RawState: input, PlayerIdx: peerconnection.PlayerIndex, ConnID: peerconnection.GetId()}:
			default:
			}
		}
	}
}

// RemoveSession removes a peerconnection from room and return true if there is no more room
func (r *Room) RemoveSession(w *webrtc.WebRTC) {
	// TODO: get list of r.rtcSessions in lock
	for i, s := range r.rtcSessions {
		if s.GetId() == w.GetId() {
			r.rtcSessions = append(r.rtcSessions[:i], r.rtcSessions[i+1:]...)
			s.RoomID = ""
			r.log.Debug().Str("session", s.GetId()).Msg("Session has been removed")
			break
		}
	}
	// Detach input. Send end signal
	select {
	case r.inputChannel <- nanoarch.InputEvent{RawState: []byte{0xFF, 0xFF}, ConnID: w.GetId()}:
	default:
	}
}

func (r *Room) IsPCInRoom(w *webrtc.WebRTC) bool {
	// TODO: Reuse for remove Session
	if r == nil {
		return false
	}
	for _, s := range r.rtcSessions {
		if s.GetId() == w.GetId() {
			return true
		}
	}
	return false
}

func (r *Room) Close() {
	if !r.IsRunning {
		return
	}

	r.IsRunning = false
	r.log.Debug().Msg("Closing the room")

	// Save game before quit. Only save for game which was previous saved to avoid flooding database
	if r.isRoomExisted() {
		r.log.Debug().Msg("Save game before closing room")
		// use goroutine here because SaveGame attempt to acquire an emulator lock.
		// the lock is holding before coming to close, so it will cause deadlock if SaveGame is synchronous
		go func() {
			// Save before close, so save can have correct state (Not sure) may again cause deadlock
			if err := r.SaveGame(); err != nil {
				r.log.Error().Err(err).Msg("couldn't save the game during close")
			}
			r.director.Close()
		}()
	} else {
		r.director.Close()
	}
	r.log.Debug().Msg("Closing input of the room ")
	close(r.inputChannel)
	close(r.Done)
	// Close here is a bit wrong because this read channel
	// Just don't close it, let it be gc
}

func (r *Room) isRoomExisted() bool {
	// Check if room is in online storage
	_, err := r.onlineStorage.Load(r.ID)
	if err == nil {
		return true
	}
	return isGameOnLocal(r.director.GetHashPath())
}

// SaveGame writes save state on the disk as well as
// uploads it to a cloud storage.
func (r *Room) SaveGame() error {
	// TODO: Move to game view
	if err := r.director.SaveGame(); err != nil {
		return err
	}
	if err := r.onlineStorage.Save(r.ID, r.director.GetHashPath()); err != nil {
		return err
	}
	r.log.Debug().Msg("Cloud save is successful")
	return nil
}

// saveOnlineRoomToLocal save online room to local.
// !Supports only one file of main save state.
func (r *Room) saveOnlineRoomToLocal(roomID string, savePath string) error {
	data, err := r.onlineStorage.Load(roomID)
	if err != nil {
		return err
	}
	// Save the data fetched from a cloud provider to the local server
	if data != nil {
		if err := ioutil.WriteFile(savePath, data, 0644); err != nil {
			return err
		}
		r.log.Debug().Msg("Successfully downloaded cloud save")
	}
	return nil
}

func (r *Room) LoadGame() error { return r.director.LoadGame() }

func (r *Room) ToggleMultitap() error { return r.director.ToggleMultitap() }

func (r *Room) IsEmpty() bool { return len(r.rtcSessions) == 0 }

func (r *Room) HasRunningSessions() bool {
	for _, s := range r.rtcSessions {
		if s.IsConnected() {
			return true
		}
	}
	return false
}
