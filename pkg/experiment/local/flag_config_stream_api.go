package local

import (
	"encoding/json"
	"errors"
	"net/url"
	"sync"
	"time"

	"github.com/amplitude/experiment-go-server/internal/evaluation"
)

const MAX_JITTER = 5 * time.Second

// type flagConfigStreamApi interface {
// 	Connect() error
// 	Close() error
// }

type flagConfigStreamApiV2 struct {
	OnInitUpdate func (map[string]*evaluation.Flag) error
    OnUpdate func (map[string]*evaluation.Flag) error
    OnError func (error)
	DeploymentKey                        string
	ServerURL                            string
    connectionTimeout time.Duration
    keepaliveTimeout time.Duration
    reconnInterval time.Duration
	stopCh chan bool
	lock sync.Mutex
}

func NewFlagConfigStreamApiV2(
	deploymentKey                        string,
	serverURL                            string,
    connectionTimeout time.Duration,
    keepaliveTimeout time.Duration,
    reconnInterval time.Duration,
) *flagConfigStreamApiV2 {
	return &flagConfigStreamApiV2{
		DeploymentKey:                        deploymentKey,
		ServerURL:                            serverURL,
		connectionTimeout: connectionTimeout,
		keepaliveTimeout: keepaliveTimeout,
		reconnInterval: reconnInterval,
		stopCh: nil,
		lock: sync.Mutex{},
	}
}

func (a *flagConfigStreamApiV2) Connect() error {
	a.lock.Lock()
	defer a.lock.Unlock()

	err := a.closeInternal()
	if (err != nil) {
		return err
	}

	// Create URL.
	endpoint, err := url.Parse(a.ServerURL)
	if err != nil {
		return err
	}
	endpoint.Path = "sdk/stream/v1/flags"

	// Create Stream.
	stream := NewSseStream("Api-Key " + a.DeploymentKey, endpoint.String(), a.connectionTimeout, a.keepaliveTimeout, a.reconnInterval, MAX_JITTER)

	streamMsgCh := make(chan StreamEvent)
	streamErrCh := make(chan error)
	// Connect.
	stream.Connect(streamMsgCh, streamErrCh)

	closeStream := func () {
		stream.Cancel()
		close(streamMsgCh)
		close(streamErrCh)
	}

	// Retrieve first flag configs and parse it.
	// If any error here means init error.
	select{
	case msg := <-streamMsgCh:
		// Parse message and verify data correct.
		flags, err := parseData(msg.data)
		if (err != nil) {
			closeStream()
			return errors.New("stream corrupt data, cause: " + err.Error())
		}
		if (a.OnInitUpdate != nil) {
			err = a.OnInitUpdate(flags)
		} else if (a.OnUpdate != nil) {
			err = a.OnUpdate(flags)
		}
		if (err != nil) {
			closeStream()
			return err
		}
	case err := <-streamErrCh:
		// Error when creating the stream.
		closeStream()
		return err
	case <-time.After(a.connectionTimeout):
		// Timed out.
		closeStream()
		return errors.New("stream connect timeout")
	}

	// Prep procedures for stopping.
	stopCh := make(chan bool)
	a.stopCh = stopCh

	closeAllAndNotify := func(err error) {
		a.lock.Lock()
		defer a.lock.Unlock()
		closeStream()
		if (a.stopCh == stopCh) {
			a.stopCh = nil
		}
		close(stopCh)
		if (a.OnError != nil) {
			a.OnError(err)
		}
	}

	// Retrieve and pass on message forever until stopCh closes.
	go func() {
		for {
			select{
			case <-stopCh: // Channel returns immediately when closed. Note the local channel is referred here, so it's guaranteed to not be nil.
				closeStream()
				return
			case msg := <-streamMsgCh:
				// Parse message and verify data correct.
				flags, err := parseData(msg.data)
				if (err != nil) {
					// Error, close everything.
					closeAllAndNotify(errors.New("stream corrupt data, cause: " + err.Error()))
					return
				}
				if (a.OnUpdate != nil) {
					// Deliver async. Don't care about any errors.
					go func() {a.OnUpdate(flags)}()
				}
			case err := <-streamErrCh:
				// Error, close everything.
				closeAllAndNotify(err)
				return
			}
		}
	}()

	return nil
}

func parseData(data []byte) (map[string]*evaluation.Flag, error) {

	var flagsArray []*evaluation.Flag
	err := json.Unmarshal(data, &flagsArray)
	if err != nil {
		return nil, err
	}
	flags := make(map[string]*evaluation.Flag)
	for _, flag := range flagsArray {
		flags[flag.Key] = flag
	}

	return flags, nil
}

func (a *flagConfigStreamApiV2) closeInternal() error {
	if (a.stopCh != nil) {
		close(a.stopCh)
		a.stopCh = nil
	}
	return nil
}
func (a *flagConfigStreamApiV2) Close() error {
	a.lock.Lock()
	defer a.lock.Unlock()

	return a.closeInternal()
}