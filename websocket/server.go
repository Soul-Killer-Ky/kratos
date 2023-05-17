package websocket

import (
	"context"
	"crypto/tls"
	"errors"
	"net/url"
	"time"

	"github.com/go-kratos/kratos/v2/encoding"
	"github.com/go-kratos/kratos/v2/log"
	"github.com/go-kratos/kratos/v2/transport"
	"github.com/go-kratos/kratos/v2/transport/http"
	ws "github.com/gorilla/websocket"
	encoding2 "github.com/tx7do/kratos-transport/broker"
)

type Binder func() Any

type ConnectHandler func(SessionID, bool)

type MessageHandler func(*Session, MessagePayload) error

type HandlerData struct {
	Handler MessageHandler
	Binder  Binder
}
type MessageHandlerMap map[MessageType]HandlerData

type BusinessIDFunc func(context.Context) interface{}

var (
	_ transport.Server = (*Server)(nil)
)

type Server struct {
	httpSrv  *http.Server
	tlsConf  *tls.Config
	upgrader *ws.Upgrader

	network        string
	address        string
	path           string
	strictSlash    bool
	endpoint       *url.URL
	middleware     http.ServerOption
	businessIDFunc BusinessIDFunc

	timeout time.Duration

	err   error
	codec encoding.Codec

	messageHandlers MessageHandlerMap
	connectHandler  ConnectHandler

	sessions         SessionMap
	businessSessions BusinessSession
	register         chan *Session
	unregister       chan *Session
}

func NewServer(opts ...ServerOption) *Server {
	srv := &Server{
		network:     "tcp",
		address:     ":0",
		timeout:     1 * time.Second,
		strictSlash: true,

		messageHandlers: make(MessageHandlerMap),

		sessions:         SessionMap{},
		businessSessions: BusinessSession{},
		upgrader: &ws.Upgrader{
			ReadBufferSize:  1024,
			WriteBufferSize: 1024,
			CheckOrigin:     func(r *http.Request) bool { return true },
		},

		register:   make(chan *Session),
		unregister: make(chan *Session),
	}
	srv.init(opts...)

	return srv
}

func (s *Server) Name() string {
	return "websocket"
}

func (s *Server) init(opts ...ServerOption) {
	for _, o := range opts {
		o(s)
	}

	httpSrv := http.NewServer(http.Address(s.address), s.middleware)
	router := httpSrv.Route("/")
	router.GET(s.path, s.wsHandler)

	s.httpSrv = httpSrv
}

func (s *Server) SessionCount() int {
	return len(s.sessions)
}

func (s *Server) RegisterMessageHandler(messageType MessageType, handler MessageHandler, binder Binder) {
	if _, ok := s.messageHandlers[messageType]; ok {
		return
	}

	s.messageHandlers[messageType] = HandlerData{
		handler, binder,
	}
}

func (s *Server) DeregisterMessageHandler(messageType MessageType) {
	delete(s.messageHandlers, messageType)
}

func (s *Server) marshalMessage(messageType MessageType, message MessagePayload) ([]byte, error) {
	var err error
	var msg Message
	msg.Type = messageType
	msg.Body, err = encoding2.Marshal(s.codec, message)
	if err != nil {
		return nil, err
	}

	buff, err := msg.Marshal()
	if err != nil {
		return nil, err
	}

	return buff, nil
}

func (s *Server) SendMessage(sessionId SessionID, messageType MessageType, message MessagePayload) {
	c, ok := s.sessions[sessionId]
	if !ok {
		log.Error("[websocket] session not found:", sessionId)
		return
	}

	buf, err := s.marshalMessage(messageType, message)
	if err != nil {
		log.Error("[websocket] marshal message exception:", err)
		return
	}

	c.SendMessage(buf)
}

func (s *Server) SendMessageByBID(businessID interface{}, messageType MessageType, message MessagePayload) bool {
	sessionIDs, ok := s.businessSessions[businessID]
	if !ok {
		log.Errorf("not fond session by business id: %s", businessID)
		return false
	}
	for _, id := range sessionIDs {
		s.SendMessage(id, messageType, message)
	}
	return true
}

func (s *Server) Broadcast(messageType MessageType, message MessagePayload) {
	buf, err := s.marshalMessage(messageType, message)
	if err != nil {
		log.Error(" [websocket] marshal message exception:", err)
		return
	}

	for _, c := range s.sessions {
		c.SendMessage(buf)
	}
}

func (s *Server) messageHandler(session *Session, buf []byte) error {
	var msg Message
	if err := msg.Unmarshal(buf); err != nil {
		log.Errorf("[websocket] decode message exception: %s", err)
		return err
	}

	handlerData, ok := s.messageHandlers[msg.Type]
	if !ok {
		log.Error("[websocket] message type not found:", msg.Type)
		return errors.New("message handler not found")
	}

	var payload MessagePayload

	if handlerData.Binder != nil {
		payload = handlerData.Binder()
	} else {
		payload = msg.Body
	}

	if err := encoding2.Unmarshal(s.codec, msg.Body, &payload); err != nil {
		log.Errorf("[websocket] unmarshal message exception: %s", err)
		return err
	}

	if err := handlerData.Handler(session, payload); err != nil {
		log.Errorf("[websocket] message handler exception: %s", err)
		return err
	}

	return nil
}

func (s *Server) wsHandler(ctx http.Context) error {
	h := ctx.Middleware(func(ctx2 context.Context, req interface{}) (interface{}, error) {
		response, request := ctx.Response(), ctx.Request()
		conn, err := s.upgrader.Upgrade(response, request, nil)
		if err != nil {
			log.Error("[websocket] upgrade exception:", err)
			return nil, err
		}
		var bid BusinessID
		if s.businessIDFunc != nil {
			bid = s.businessIDFunc(ctx2)
		}
		session := NewSession(conn, s, bid)
		session.server.register <- session
		session.Listen()

		return nil, nil
	})
	_, err := h(ctx, ctx.Request())
	if err != nil {
		log.Error("[websocket] handle exception:", err)
		return err
	}

	return nil
}

func (s *Server) run() {
	for {
		select {
		case client := <-s.register:
			s.addSession(client)
		case client := <-s.unregister:
			s.removeSession(client)
		}
	}
}

func (s *Server) Start(ctx context.Context) error {
	log.Infof("[websocket] server listening on: %s", s.address)

	go s.run()

	return s.httpSrv.Start(ctx)
}

func (s *Server) Stop(ctx context.Context) error {
	log.Info("[websocket] server stopping")
	return s.httpSrv.Stop(ctx)
}

func (s *Server) GetSession(sessionID SessionID) (*Session, bool) {
	session, ok := s.sessions[sessionID]
	return session, ok
}

func (s *Server) addSession(c *Session) {
	s.sessions[c.SessionID()] = c
	if _, ok := s.businessSessions[c.businessID]; ok {
		s.businessSessions[c.businessID] = append(s.businessSessions[c.businessID], c.SessionID())
	} else {
		s.businessSessions[c.businessID] = []SessionID{c.SessionID()}
	}

	if s.connectHandler != nil {
		s.connectHandler(c.SessionID(), true)
	}
}

func (s *Server) removeSession(c *Session) {
	for k, v := range s.sessions {
		if c == v {
			if s.connectHandler != nil {
				s.connectHandler(c.SessionID(), false)
			}
			delete(s.sessions, k)
			for bk, sid := range s.businessSessions[v.businessID] {
				if sid == v.SessionID() {
					s.businessSessions[v.businessID] = append(s.businessSessions[v.businessID][:bk], s.businessSessions[v.businessID][bk+1:]...)
				}
			}
			if len(s.businessSessions[v.businessID]) == 0 {
				delete(s.businessSessions, v.businessID)
			}
			return
		}
	}
}
