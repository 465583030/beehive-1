package actor

import (
	"encoding/gob"
	"net"

	"github.com/golang/glog"
)

type handlerAndDataCh struct {
	dataCh  chan msgAndHandler
	handler Handler
}

func (s *stage) handleConn(conn net.Conn) {
	defer conn.Close()

	dec := gob.NewDecoder(conn)
	enc := gob.NewEncoder(conn)

	var id RcvrId
	dec.Decode(&id)

	a, err := s.actor(id.ActorName)
	if err != nil {
		glog.Errorf("Cannot find actor: %s", id.ActorName)
		return
	}

	resCh := make(chan interface{})
	a.mapper.ctrlCh <- routineCmd{findRcvr, id.Id, resCh}

	res := <-resCh
	if res == nil {
		glog.Errorf("Cannot find receiver: %v", id)
		return
	}

	rcvr := res.(receiver)

	enc.Encode(true)

	handlers := make(map[MsgType][]Handler)
	for {
		m := msg{}
		if err := dec.Decode(&m); err != nil {
			glog.Errorf("Cannot decode message: %v", err)
			return
		}

		hs, ok := handlers[m.Type()]
		if !ok {
			hs = []Handler{}
			for _, mh := range s.mappers[m.Type()] {
				if mh.mapr.ctx.actor.Name() == id.ActorName {
					hs = append(hs, mh.handler)
				}
			}
			handlers[m.Type()] = hs
		}

		for _, h := range hs {
			rcvr.enque(msgAndHandler{&m, h})
		}
	}
}

func (s *stage) listen() {
	l, err := net.Listen("tcp", s.config.StageAddr)
	if err != nil {
		glog.Fatal("Cannot start listener: %v", err)
	}

	glog.V(1).Infof("Network server listening at: %s", s.config.StageAddr)
	for {
		c, err := l.Accept()
		if err != nil {
			glog.Errorf("Error in accept %s", err)
			continue
		}
		glog.V(2).Infof("Accepting a new connection: %v -> %v", c.RemoteAddr(),
			c.LocalAddr())
		go s.handleConn(c)
	}
}
