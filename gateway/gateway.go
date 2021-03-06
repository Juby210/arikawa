// Package gateway handles the Discord gateway (or Websocket) connection, its
// events, and everything related to it. This includes logging into the
// Websocket.
//
// This package does not abstract events and function handlers; instead, it
// leaves that to the session package. This package exposes only a single Events
// channel.
package gateway

import (
	"context"
	"log"
	"net/url"
	"runtime"
	"sync"
	"time"

	"github.com/diamondburned/arikawa/api"
	"github.com/diamondburned/arikawa/internal/httputil"
	"github.com/diamondburned/arikawa/internal/json"
	"github.com/diamondburned/arikawa/internal/wsutil"
	"github.com/pkg/errors"
)

const (
	EndpointGateway    = api.Endpoint + "gateway"
	EndpointGatewayBot = api.EndpointGateway + "/bot"

	Version  = "6"
	Encoding = "json"
)

var (
	// WSTimeout is the timeout for connecting and writing to the Websocket,
	// before Gateway cancels and fails.
	WSTimeout = wsutil.DefaultTimeout
	// WSBuffer is the size of the Event channel. This has to be at least 1 to
	// make space for the first Event: Ready or Resumed.
	WSBuffer = 10
	// WSError is the default error handler
	WSError = func(err error) { log.Println("Gateway error:", err) }
	// WSFatal is the default fatal handler, which is called when the Gateway
	// can't recover.
	WSFatal = func(err error) { log.Fatalln("Gateway failed:", err) }
	// WSExtraReadTimeout is the duration to be added to Hello, as a read
	// timeout for the websocket.
	WSExtraReadTimeout = time.Second

	WSDebug = func(v ...interface{}) {}
)

var (
	ErrMissingForResume = errors.New(
		"missing session ID or sequence for resuming")
	ErrWSMaxTries = errors.New("max tries reached")
)

func GatewayURL() (string, error) {
	var Gateway struct {
		URL string `json:"url"`
	}

	return Gateway.URL, httputil.DefaultClient.RequestJSON(
		&Gateway, "GET", EndpointGateway)
}

// Identity is used as the default identity when initializing a new Gateway.
var Identity = IdentifyProperties{
	OS:      runtime.GOOS,
	Browser: "Arikawa",
	Device:  "Arikawa",
}

type Gateway struct {
	WS *wsutil.Websocket
	json.Driver

	// Timeout for connecting and writing to the Websocket, uses default
	// WSTimeout (global).
	WSTimeout time.Duration

	// All events sent over are pointers to Event structs (structs suffixed with
	// "Event"). This shouldn't be accessed if the Gateway is created with a
	// Session.
	Events chan Event

	SessionID string

	Identifier *Identifier
	Pacemaker  *Pacemaker
	Sequence   *Sequence

	ErrorLog func(err error) // default to log.Println
	FatalLog func(err error) // called when the WS can't reconnect and resume

	// Only use for debugging

	// If this channel is non-nil, all incoming OP packets will also be sent
	// here. This should be buffered, so to not block the main loop.
	OP chan Event

	// Filled by methods, internal use
	paceDeath chan error
	waitGroup *sync.WaitGroup
}

// NewGateway starts a new Gateway with the default stdlib JSON driver. For more
// information, refer to NewGatewayWithDriver.
func NewGateway(token string) (*Gateway, error) {
	return NewGatewayWithDriver(token, json.Default{})
}

// NewGatewayWithDriver connects to the Gateway and authenticates automatically.
func NewGatewayWithDriver(token string, driver json.Driver) (*Gateway, error) {
	URL, err := GatewayURL()
	if err != nil {
		return nil, errors.Wrap(err, "Failed to get gateway endpoint")
	}

	g := &Gateway{
		Driver:     driver,
		WSTimeout:  WSTimeout,
		Events:     make(chan Event, WSBuffer),
		Identifier: DefaultIdentifier(token),
		Sequence:   NewSequence(),
		ErrorLog:   WSError,
		FatalLog:   WSFatal,
	}

	// Parameters for the gateway
	param := url.Values{}
	param.Set("v", Version)
	param.Set("encoding", Encoding)
	// Append the form to the URL
	URL += "?" + param.Encode()

	ctx, cancel := context.WithTimeout(context.Background(), g.WSTimeout)
	defer cancel()

	// Create a new undialed Websocket.
	ws, err := wsutil.NewCustom(ctx, wsutil.NewConn(driver), URL)
	if err != nil {
		return nil, errors.Wrap(err, "Failed to connect to Gateway "+URL)
	}
	g.WS = ws

	// Try and dial it
	return g, nil
}

// Close closes the underlying Websocket connection.
func (g *Gateway) Close() error {
	WSDebug("Stopping pacemaker...")

	// If the pacemaker is running:
	// Stop the pacemaker and the event handler
	g.Pacemaker.Stop()

	WSDebug("Stopped pacemaker. Waiting for WaitGroup to be done.")

	// This should work, since Pacemaker should signal its loop to stop, which
	// would also exit our event loop. Both would be 2.
	g.waitGroup.Wait()

	// Mark g.waitGroup as empty:
	g.waitGroup = nil

	// Stop the Websocket
	return g.WS.Close(nil)
}

// Reconnects and resumes.
func (g *Gateway) Reconnect() error {
	WSDebug("Reconnecting...")

	// If the event loop is not dead:
	if g.paceDeath != nil {
		WSDebug("Gateway is not closed, closing before reconnecting...")
		g.Close()
		WSDebug("Gateway is closed asynchronously. Goroutine may not be exited.")
	}

	// Actually a reconnect at this point.
	return g.Open()
}

func (g *Gateway) Open() error {
	// Reconnect timeout
	// ctx, cancel := context.WithTimeout(context.Background(), g.WSTimeout)
	// defer cancel()

	// TODO: this could be of some use.
	ctx := context.Background()

	for i := 0; ; i++ {
		/* Context doesn't time out.

		// Check if context is expired
		if err := ctx.Err(); err != nil {
			// Close the connection
			g.Close()

			// Don't bother if it's expired
			return err
		}

		*/

		WSDebug("Trying to dial...", i)

		// Reconnect to the Gateway
		if err := g.WS.Dial(ctx); err != nil {
			// Save the error, retry again
			g.ErrorLog(errors.Wrap(err, "Failed to reconnect"))
			continue
		}

		WSDebug("Trying to start...", i)

		// Try to resume the connection
		if err := g.Start(); err != nil {
			// If the connection is rate limited (documented behavior):
			// https://discordapp.com/developers/docs/topics/gateway#rate-limiting
			if err == ErrInvalidSession {
				continue
			}

			// Else, keep retrying
			g.ErrorLog(errors.Wrap(err, "Failed to start gateway"))
			continue
		}

		WSDebug("Started after attempt:", i)
		// Started successfully, return
		return nil
	}
}

// Start authenticates with the websocket, or resume from a dead Websocket
// connection. This function doesn't block.
func (g *Gateway) Start() error {
	if err := g.start(); err != nil {
		WSDebug("Start failed:", err)
		if err := g.Close(); err != nil {
			WSDebug("Failed to close after start fail:", err)
		}
		return err
	}
	return nil
}

func (g *Gateway) start() error {
	// This is where we'll get our events
	ch := g.WS.Listen()

	// Wait for an OP 10 Hello
	var hello HelloEvent
	if _, err := AssertEvent(g, <-ch, HelloOP, &hello); err != nil {
		return errors.Wrap(err, "Error at Hello")
	}

	// Make a new WaitGroup for use in background loops:
	g.waitGroup = new(sync.WaitGroup)

	// Start the pacemaker with the heartrate received from Hello:
	g.Pacemaker = &Pacemaker{
		Heartrate: hello.HeartbeatInterval.Duration(),
		Pace:      g.Heartbeat,
		OnDead:    g.Reconnect,
	}
	// Pacemaker dies here, only when it's fatal.
	g.paceDeath = g.Pacemaker.StartAsync(g.waitGroup)

	// Send Discord either the Identify packet (if it's a fresh connection), or
	// a Resume packet (if it's a dead connection).
	if g.SessionID == "" {
		// SessionID is empty, so this is a completely new session.
		if err := g.Identify(); err != nil {
			return errors.Wrap(err, "Failed to identify")
		}
	} else {
		if err := g.Resume(); err != nil {
			return errors.Wrap(err, "Failed to resume")
		}
	}

	// Expect at least one event
	ev := <-ch

	// Check for error
	if ev.Error != nil {
		return errors.Wrap(ev.Error, "First error")
	}

	// Handle the event
	if err := HandleEvent(g, ev.Data); err != nil {
		return errors.Wrap(err, "WS handler error on first event")
	}

	// Start the event handler
	g.waitGroup.Add(1)
	go g.handleWS()

	WSDebug("Started successfully.")

	return nil
}

// handleWS uses the Websocket and parses them into g.Events.
func (g *Gateway) handleWS() {
	err := g.eventLoop()
	g.waitGroup.Done()

	if err != nil {
		if err := g.Reconnect(); err != nil {
			g.FatalLog(errors.Wrap(err, "Failed to reconnect"))
		}

		// Reconnect should spawn another eventLoop in its Start function.
	}
}

func (g *Gateway) eventLoop() error {
	ch := g.WS.Listen()

	for {
		select {
		case err := <-g.paceDeath:
			// Got a paceDeath, we're exiting from here on out.
			g.paceDeath = nil // mark

			if err == nil {
				WSDebug("Pacemaker stopped without errors.")
				// No error, just exit normally.
				return nil
			}

			return errors.New("Pacemaker died, reconnecting.")

		case ev := <-ch:
			// Check for error
			if ev.Error != nil {
				g.ErrorLog(ev.Error)
				continue
			}

			if len(ev.Data) == 0 {
				return errors.New("Event data is empty, reconnecting.")
			}

			// Handle the event
			if err := HandleEvent(g, ev.Data); err != nil {
				g.ErrorLog(errors.Wrap(err, "WS handler error"))
			}
		}
	}
}

func (g *Gateway) Send(code OPCode, v interface{}) error {
	var op = OP{
		Code: code,
	}

	if v != nil {
		b, err := g.Driver.Marshal(v)
		if err != nil {
			return errors.Wrap(err, "Failed to encode v")
		}

		op.Data = b
	}

	b, err := g.Driver.Marshal(op)
	if err != nil {
		return errors.Wrap(err, "Failed to encode payload")
	}

	ctx, cancel := context.WithTimeout(context.Background(), g.WSTimeout)
	defer cancel()

	return g.WS.Send(ctx, b)
}
