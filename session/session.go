// Package session abstracts around the REST API and the Gateway, managing both
// at once. It offers a handler interface similar to that in discordgo for
// Gateway events.
package session

import (
	"log"

	"github.com/diamondburned/arikawa/api"
	"github.com/diamondburned/arikawa/gateway"
	"github.com/diamondburned/arikawa/handler"
	"github.com/pkg/errors"
)

var ErrMFA = errors.New("Account has 2FA enabled")

// Session manages both the API and Gateway. As such, Session inherits all of
// API's methods, as well has the Handler used for Gateway.
type Session struct {
	*api.Client
	Gateway *gateway.Gateway

	// ErrorLog logs errors, including Gateway errors.
	ErrorLog func(err error) // default to log.Println

	// Command handler with inherited methods.
	*handler.Handler

	// MFA only fields
	MFA    bool
	Ticket string

	hstop chan struct{}
}

func New(token string) (*Session, error) {
	// Initialize the session and the API interface
	s := &Session{}
	s.Handler = handler.New()
	s.Client = api.NewClient(token)

	// Default logger
	s.ErrorLog = func(err error) {
		log.Println("Arikawa/session error:", err)
	}

	// Open a gateway
	g, err := gateway.NewGateway(token)
	if err != nil {
		return nil, errors.Wrap(err, "Failed to connect to Gateway")
	}
	s.Gateway = g
	s.Gateway.ErrorLog = func(err error) {
		s.ErrorLog(err)
	}

	return s, nil
}

// Login tries to log in as a normal user account; MFA is optional.
func Login(email, password, mfa string) (*Session, error) {
	// Make a scratch HTTP client without a token
	client := api.NewClient("")

	// Try to login without TOTP
	l, err := client.Login(email, password)
	if err != nil {
		return nil, errors.Wrap(err, "Failed to login")
	}

	if l.Token != "" && !l.MFA {
		// We got the token, return with a new Session.
		return New(l.Token)
	}

	// Discord requests MFA, so we need the MFA token.
	if mfa == "" {
		return nil, ErrMFA
	}

	// Retry logging in with a 2FA token
	l, err = client.TOTP(mfa, l.Ticket)
	if err != nil {
		return nil, errors.Wrap(err, "Failed to login with 2FA")
	}

	return New(l.Token)
}

func NewWithGateway(gw *gateway.Gateway) *Session {
	s := &Session{
		// Nab off gateway's token
		Client: api.NewClient(gw.Identifier.Token),
		ErrorLog: func(err error) {
			log.Println("Arikawa/session error:", err)
		},
		Handler: handler.New(),
	}

	gw.ErrorLog = func(err error) {
		s.ErrorLog(err)
	}

	return s
}

func (s *Session) Open() error {
	if err := s.Gateway.Open(); err != nil {
		return errors.Wrap(err, "Failed to start gateway")
	}

	stop := make(chan struct{})
	s.hstop = stop
	go s.startHandler(stop)

	return nil
}

func (s *Session) startHandler(stop <-chan struct{}) {
	for {
		select {
		case <-stop:
			return
		case ev := <-s.Gateway.Events:
			s.Handler.Call(ev)
		}
	}
}

func (s *Session) Close() error {
	// Stop the event handler
	if s.hstop != nil {
		close(s.hstop)
	}

	// Close the websocket
	return s.Gateway.Close()
}
