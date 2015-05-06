// Copyright 2015 Reborndb Org. All Rights Reserved.
// Licensed under the MIT (MIT-LICENSE.txt) license.

package service

import (
	"crypto/rand"
	"io"
	"net"
	"sync"

	"github.com/reborndb/go/atomic2"
	"github.com/reborndb/go/errors"
	"github.com/reborndb/go/log"
	"github.com/reborndb/go/redis/handler"
	redis "github.com/reborndb/go/redis/resp"
	"github.com/reborndb/go/ring"
	"github.com/reborndb/go/sync2"
	"github.com/reborndb/qdb/pkg/binlog"
)

func Serve(config *Config, bl *binlog.Binlog) error {
	h := &Handler{
		config: config,
		master: make(chan *conn, 0),
		signal: make(chan int, 0),
	}
	defer func() {
		close(h.signal)
	}()

	h.runID = make([]byte, 40)
	getRandomHex(h.runID)
	log.Infof("server runid is %s", h.runID)

	l, err := net.Listen("tcp", config.Listen)
	if err != nil {
		return errors.Trace(err)
	}
	defer l.Close()

	if err = h.initReplication(bl); err != nil {
		return errors.Trace(err)
	}
	defer h.closeReplication()

	if h.htable, err = handler.NewHandlerTable(h); err != nil {
		return err
	} else {
		go h.daemonSyncMaster()
	}

	log.Infof("open listen address '%s' and start service", l.Addr())

	for {
		if nc, err := l.Accept(); err != nil {
			return errors.Trace(err)
		} else {
			h.counters.clientsAccepted.Add(1)
			go func() {
				h.counters.clients.Add(1)
				defer h.counters.clients.Sub(1)
				c := newConn(nc, bl, h.config.ConnTimeout)
				defer c.Close()
				log.Infof("new connection: %s", c)
				if err := c.serve(h); err != nil {
					if errors.Equal(err, io.EOF) {
						log.Infof("connection lost: %s [io.EOF]", c)
					} else {
						log.InfoErrorf(err, "connection lost: %s", c)
					}
				} else {
					log.Infof("connection exit: %s", c)
				}
			}()
		}
	}
}

type Session interface {
	DB() uint32
	SetDB(db uint32)
	Binlog() *binlog.Binlog
}

type Handler struct {
	config *Config
	htable handler.HandlerTable

	syncto       string
	syncto_since int64

	master chan *conn
	signal chan int

	counters struct {
		bgsave          atomic2.Int64
		clients         atomic2.Int64
		clientsAccepted atomic2.Int64
		commands        atomic2.Int64
		commandsFailed  atomic2.Int64
		syncRdbRemains  atomic2.Int64
		syncCacheBytes  atomic2.Int64
		syncTotalBytes  atomic2.Int64
		syncFull        atomic2.Int64
		syncPartialOK   atomic2.Int64
		syncPartialErr  atomic2.Int64
	}

	// 40 bytes, hex random run id for different server
	runID []byte

	repl struct {
		sync.RWMutex

		// replication backlog buffer
		backlogBuf *ring.Ring

		// global master replication offset
		masterOffset int64

		// replication offset of first byte in the backlog buffer
		backlogOffset int64

		lastSelectDB atomic2.Int64

		slaves map[*conn]chan struct{}

		fullSyncSema *sync2.Semaphore
	}
}

func toRespError(err error) (redis.Resp, error) {
	return redis.NewError(err), err
}

func toRespErrorf(format string, args ...interface{}) (redis.Resp, error) {
	err := errors.Errorf(format, args...)
	return toRespError(err)
}

func session(arg0 interface{}, args [][]byte) (Session, error) {
	s, _ := arg0.(Session)
	if s == nil {
		return nil, errors.New("invalid session")
	}
	for i, v := range args {
		if len(v) == 0 {
			return nil, errors.Errorf("args[%d] is nil", i)
		}
	}
	return s, nil
}

func iconvert(args [][]byte) []interface{} {
	iargs := make([]interface{}, len(args))
	for i, v := range args {
		iargs[i] = v
	}
	return iargs
}

func getRandomHex(buf []byte) []byte {
	charsets := "0123456789abcdef"

	rand.Read(buf)

	for i := range buf {
		buf[i] = charsets[buf[i]&0x0F]
	}

	return buf
}
