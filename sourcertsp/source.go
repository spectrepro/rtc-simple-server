package sourcertsp

import (
	"net/url"
	"sync"
	"time"

	"github.com/aler9/gortsplib"
)

const (
	retryInterval = 5 * time.Second
)

type Parent interface {
	Log(string, ...interface{})
	OnSourceReady(gortsplib.Tracks)
	OnSourceNotReady()
	OnFrame(int, gortsplib.StreamType, []byte)
}

type State int

const (
	StateStopped State = iota
	StateRunning
)

type Source struct {
	ur           string
	proto        gortsplib.StreamProtocol
	readTimeout  time.Duration
	writeTimeout time.Duration
	state        State
	parent       Parent

	innerRunning bool

	// in
	innerTerminate chan struct{}
	innerDone      chan struct{}
	stateChange    chan State
	terminate      chan struct{}

	// out
	done chan struct{}
}

func New(ur string,
	proto gortsplib.StreamProtocol,
	readTimeout time.Duration,
	writeTimeout time.Duration,
	state State,
	parent Parent) *Source {
	s := &Source{
		ur:           ur,
		proto:        proto,
		readTimeout:  readTimeout,
		writeTimeout: writeTimeout,
		state:        state,
		parent:       parent,
		stateChange:  make(chan State),
		terminate:    make(chan struct{}),
		done:         make(chan struct{}),
	}

	go s.run(s.state)
	return s
}

func (s *Source) Close() {
	close(s.terminate)
	<-s.done
}

func (s *Source) IsSource() {}

func (s *Source) State() State {
	return s.state
}

func (s *Source) SetState(state State) {
	s.state = state
	s.stateChange <- s.state
}

func (s *Source) run(initialState State) {
	defer close(s.done)

	s.applyState(initialState)

outer:
	for {
		select {
		case state := <-s.stateChange:
			s.applyState(state)

		case <-s.terminate:
			break outer
		}
	}

	if s.innerRunning {
		close(s.innerTerminate)
		<-s.innerDone
	}

	close(s.stateChange)
}

func (s *Source) applyState(state State) {
	if state == StateRunning {
		if !s.innerRunning {
			s.parent.Log("rtsp source started")
			s.innerRunning = true
			s.innerTerminate = make(chan struct{})
			s.innerDone = make(chan struct{})
			go s.runInner()
		}
	} else {
		if s.innerRunning {
			close(s.innerTerminate)
			<-s.innerDone
			s.innerRunning = false
			s.parent.Log("rtsp source stopped")
		}
	}
}

func (s *Source) runInner() {
	defer close(s.innerDone)

outer:
	for {
		ok := s.runInnerInner()
		if !ok {
			break outer
		}

		t := time.NewTimer(retryInterval)
		defer t.Stop()

		select {
		case <-s.innerTerminate:
			break outer
		case <-t.C:
		}
	}
}

func (s *Source) runInnerInner() bool {
	s.parent.Log("connecting to rtsp source")

	u, _ := url.Parse(s.ur)

	var conn *gortsplib.ConnClient
	var err error
	dialDone := make(chan struct{}, 1)
	go func() {
		defer close(dialDone)
		conn, err = gortsplib.NewConnClient(gortsplib.ConnClientConf{
			Host:            u.Host,
			ReadTimeout:     s.readTimeout,
			WriteTimeout:    s.writeTimeout,
			ReadBufferCount: 2,
		})
	}()

	select {
	case <-s.innerTerminate:
		return false
	case <-dialDone:
	}

	if err != nil {
		s.parent.Log("rtsp source ERR: %s", err)
		return true
	}

	_, err = conn.Options(u)
	if err != nil {
		conn.Close()
		s.parent.Log("rtsp source ERR: %s", err)
		return true
	}

	tracks, _, err := conn.Describe(u)
	if err != nil {
		conn.Close()
		s.parent.Log("rtsp source ERR: %s", err)
		return true
	}

	if s.proto == gortsplib.StreamProtocolUDP {
		return s.runUDP(u, conn, tracks)
	} else {
		return s.runTCP(u, conn, tracks)
	}
}

func (s *Source) runUDP(u *url.URL, conn *gortsplib.ConnClient, tracks gortsplib.Tracks) bool {
	for _, track := range tracks {
		_, err := conn.SetupUDP(u, gortsplib.TransportModePlay, track, 0, 0)
		if err != nil {
			conn.Close()
			s.parent.Log("rtsp source ERR: %s", err)
			return true
		}
	}

	_, err := conn.Play(u)
	if err != nil {
		conn.Close()
		s.parent.Log("rtsp source ERR: %s", err)
		return true
	}

	s.parent.OnSourceReady(tracks)
	s.parent.Log("rtsp source ready")

	var wg sync.WaitGroup

	// receive RTP packets
	for trackId := range tracks {
		wg.Add(1)
		go func(trackId int) {
			defer wg.Done()

			for {
				buf, err := conn.ReadFrameUDP(trackId, gortsplib.StreamTypeRtp)
				if err != nil {
					break
				}

				s.parent.OnFrame(trackId, gortsplib.StreamTypeRtp, buf)
			}
		}(trackId)
	}

	// receive RTCP packets
	for trackId := range tracks {
		wg.Add(1)
		go func(trackId int) {
			defer wg.Done()

			for {
				buf, err := conn.ReadFrameUDP(trackId, gortsplib.StreamTypeRtcp)
				if err != nil {
					break
				}

				s.parent.OnFrame(trackId, gortsplib.StreamTypeRtcp, buf)
			}
		}(trackId)
	}

	tcpConnDone := make(chan error)
	go func() {
		tcpConnDone <- conn.LoopUDP()
	}()

	var ret bool

outer:
	for {
		select {
		case <-s.innerTerminate:
			conn.Close()
			<-tcpConnDone
			ret = false
			break outer

		case err := <-tcpConnDone:
			conn.Close()
			s.parent.Log("rtsp source ERR: %s", err)
			ret = true
			break outer
		}
	}

	wg.Wait()

	s.parent.OnSourceNotReady()
	s.parent.Log("rtsp source not ready")

	return ret
}

func (s *Source) runTCP(u *url.URL, conn *gortsplib.ConnClient, tracks gortsplib.Tracks) bool {
	for _, track := range tracks {
		_, err := conn.SetupTCP(u, gortsplib.TransportModePlay, track)
		if err != nil {
			conn.Close()
			s.parent.Log("rtsp source ERR: %s", err)
			return true
		}
	}

	_, err := conn.Play(u)
	if err != nil {
		conn.Close()
		s.parent.Log("rtsp source ERR: %s", err)
		return true
	}

	s.parent.OnSourceReady(tracks)
	s.parent.Log("rtsp source ready")

	tcpConnDone := make(chan error)
	go func() {
		for {
			trackId, streamType, content, err := conn.ReadFrameTCP()
			if err != nil {
				tcpConnDone <- err
				return
			}

			s.parent.OnFrame(trackId, streamType, content)
		}
	}()

	var ret bool

outer:
	for {
		select {
		case <-s.innerTerminate:
			conn.Close()
			<-tcpConnDone
			ret = false
			break outer

		case err := <-tcpConnDone:
			conn.Close()
			s.parent.Log("rtsp source ERR: %s", err)
			ret = true
			break outer
		}
	}

	s.parent.OnSourceNotReady()
	s.parent.Log("rtsp source not ready")

	return ret
}
