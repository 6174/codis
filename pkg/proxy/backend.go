// Copyright 2016 CodisLabs. All Rights Reserved.
// Licensed under the MIT (MIT-LICENSE.txt) license.

package proxy

import (
	"bytes"
	"fmt"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/CodisLabs/codis/pkg/proxy/redis"
	"github.com/CodisLabs/codis/pkg/utils/errors"
	"github.com/CodisLabs/codis/pkg/utils/log"
	"github.com/CodisLabs/codis/pkg/utils/math2"
	"github.com/CodisLabs/codis/pkg/utils/sync2/atomic2"
)

const (
	stateConnected = 1
	stateDataStale = 2
)

type BackendConn struct {
	stop sync.Once
	addr string

	dbnum int
	input chan *Request
	retry struct {
		fails int
		delay Delay
	}
	state atomic2.Int64

	closed atomic2.Bool
	config *Config
}

func NewBackendConn(addr string, dbnum int, config *Config) *BackendConn {
	bc := &BackendConn{
		addr: addr, dbnum: dbnum, config: config,
	}
	bc.input = make(chan *Request, 1024)
	bc.retry.delay = &DelayExp2{
		Min: 50, Max: 5000,
		Unit: time.Millisecond,
	}

	go bc.run()

	return bc
}

func (bc *BackendConn) Addr() string {
	return bc.addr
}

func (bc *BackendConn) Close() {
	bc.stop.Do(func() {
		close(bc.input)
	})
	bc.closed.Set(true)
}

func (bc *BackendConn) IsConnected() bool {
	return bc.state.Get() == stateConnected
}

func (bc *BackendConn) PushBack(r *Request) {
	if r.Batch != nil {
		r.Batch.Add(1)
	}
	bc.input <- r
}

func (bc *BackendConn) KeepAlive() bool {
	if bc.state.CompareAndSwap(stateDataStale, stateConnected) {
		log.Warnf("backend conn [%p] to %s, state = Connected (keepalive)", bc, bc.addr)
	}
	if len(bc.input) != 0 {
		return false
	}
	m := &Request{}
	m.Multi = []*redis.Resp{
		redis.NewBulkBytes([]byte("PING")),
	}
	bc.PushBack(m)
	return true
}

func (bc *BackendConn) newBackendReader(round int, config *Config) (*redis.Conn, chan<- *Request, error) {
	c, err := redis.DialTimeout(bc.addr, time.Second*5,
		config.BackendRecvBufsize.Int(),
		config.BackendSendBufsize.Int())
	if err != nil {
		return nil, nil, err
	}
	c.ReaderTimeout = config.BackendRecvTimeout.Get()
	c.WriterTimeout = config.BackendSendTimeout.Get()
	c.SetKeepAlivePeriod(config.BackendKeepAlivePeriod.Get())

	if err := bc.verifyAuth(c, config.ProductAuth); err != nil {
		c.Close()
		return nil, nil, err
	}
	if err := bc.selectDB(c, bc.dbnum); err != nil {
		c.Close()
		return nil, nil, err
	}

	tasks := make(chan *Request, config.BackendMaxPipeline)
	go bc.loopReader(tasks, c, round)

	return c, tasks, nil
}

func (bc *BackendConn) verifyAuth(c *redis.Conn, auth string) error {
	if auth == "" {
		return nil
	}

	multi := []*redis.Resp{
		redis.NewBulkBytes([]byte("AUTH")),
		redis.NewBulkBytes([]byte(auth)),
	}

	if err := c.EncodeMultiBulk(multi, true); err != nil {
		return err
	}

	resp, err := c.Decode()
	switch {
	case err != nil:
		return err
	case resp == nil:
		return ErrRespIsRequired
	case resp.IsError():
		return fmt.Errorf("error resp: %s", resp.Value)
	case resp.IsString():
		return nil
	default:
		return fmt.Errorf("error resp: should be string, but got %s", resp.Type)
	}
}

func (bc *BackendConn) selectDB(c *redis.Conn, dbnum int) error {
	if dbnum == 0 {
		return nil
	}

	multi := []*redis.Resp{
		redis.NewBulkBytes([]byte("SELECT")),
		redis.NewBulkBytes([]byte(strconv.Itoa(dbnum))),
	}

	if err := c.EncodeMultiBulk(multi, true); err != nil {
		return err
	}

	resp, err := c.Decode()
	switch {
	case err != nil:
		return err
	case resp == nil:
		return ErrRespIsRequired
	case resp.IsError():
		return fmt.Errorf("error resp: %s", resp.Value)
	case resp.IsString():
		return nil
	default:
		return fmt.Errorf("error resp: should be string, but got %s", resp.Type)
	}
}

func (bc *BackendConn) setResponse(r *Request, resp *redis.Resp, err error) error {
	r.Resp, r.Err = resp, err
	if r.Group != nil {
		r.Group.Done()
	}
	if r.Batch != nil {
		r.Batch.Done()
	}
	return err
}

var (
	ErrBackendConnReset = errors.New("backend conn reset")
	ErrRequestIsBroken  = errors.New("request is broken")
)

func (bc *BackendConn) run() {
	log.Warnf("backend conn [%p] to %s, start service", bc, bc.addr)
	for k := 0; !bc.closed.Get(); k++ {
		log.Warnf("backend conn [%p] to %s, rounds-[%d]", bc, bc.addr, k)
		if err := bc.loopWriter(k); err != nil {
			bc.delayBeforeRetry()
		}
	}
	log.Warnf("backend conn [%p] to %s, stop and exit", bc, bc.addr)
}

var errMasterDown = []byte("MASTERDOWN")

func (bc *BackendConn) loopReader(tasks <-chan *Request, c *redis.Conn, round int) (err error) {
	defer func() {
		c.Close()
		for r := range tasks {
			bc.setResponse(r, nil, ErrBackendConnReset)
		}
		log.WarnErrorf(err, "backend conn [%p] to %s, reader-[%d] exit", bc, bc.addr, round)
	}()
	for r := range tasks {
		resp, err := c.Decode()
		if err != nil {
			return bc.setResponse(r, nil, fmt.Errorf("backend conn failure, %s", err))
		}
		if resp != nil && resp.IsError() {
			switch {
			case bytes.HasPrefix(resp.Value, errMasterDown):
				if bc.state.CompareAndSwap(stateConnected, stateDataStale) {
					log.Warnf("backend conn [%p] to %s, state = DataStale, caused by 'MASTERDOWN'", bc, bc.addr)
				}
			}
		}
		bc.setResponse(r, resp, nil)
	}
	return nil
}

func (bc *BackendConn) delayBeforeRetry() {
	bc.retry.fails += 1
	if bc.retry.fails <= 10 {
		return
	}
	timeout := bc.retry.delay.After()
	for !bc.closed.Get() {
		select {
		case <-timeout:
			return
		case r, ok := <-bc.input:
			if !ok {
				return
			}
			bc.setResponse(r, nil, ErrBackendConnReset)
		}
	}
}

func (bc *BackendConn) loopWriter(round int) (err error) {
	defer func() {
		for i := len(bc.input); i != 0; i-- {
			r := <-bc.input
			bc.setResponse(r, nil, ErrBackendConnReset)
		}
		log.WarnErrorf(err, "backend conn [%p] to %s, writer-[%d] exit", bc, bc.addr, round)
	}()
	c, tasks, err := bc.newBackendReader(round, bc.config)
	if err != nil {
		return err
	}
	defer close(tasks)

	defer bc.state.Set(0)

	bc.state.Set(stateConnected)
	bc.retry.fails = 0
	bc.retry.delay.Reset()

	p := c.FlushEncoder()
	p.MaxInterval = time.Millisecond
	p.MaxBuffered = math2.MinInt(256, cap(tasks))

	for r := range bc.input {
		if r.IsReadOnly() && r.IsBroken() {
			bc.setResponse(r, nil, ErrRequestIsBroken)
			continue
		}
		if err := p.EncodeMultiBulk(r.Multi); err != nil {
			return bc.setResponse(r, nil, fmt.Errorf("backend conn failure, %s", err))
		}
		if err := p.Flush(len(bc.input) == 0); err != nil {
			return bc.setResponse(r, nil, fmt.Errorf("backend conn failure, %s", err))
		} else {
			tasks <- r
		}
	}
	return nil
}

type sharedBackendConn struct {
	addr string
	host []byte
	port []byte

	owner *sharedBackendConnPool
	conns [][]*BackendConn

	single *BackendConn

	refcnt int
}

func newSharedBackendConn(addr string, config *Config, pool *sharedBackendConnPool) *sharedBackendConn {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		log.ErrorErrorf(err, "split host-port failed, address = %s", addr)
	}
	s := &sharedBackendConn{
		addr: addr,
		host: []byte(host), port: []byte(port),
	}
	s.owner = pool
	s.conns = make([][]*BackendConn, pool.database)
	for dbnum := range s.conns {
		s.conns[dbnum] = make([]*BackendConn, pool.parallel)
		for i := range s.conns[dbnum] {
			s.conns[dbnum][i] = NewBackendConn(addr, dbnum, config)
		}
	}
	if len(s.conns) == 1 && len(s.conns[0]) == 1 {
		s.single = s.conns[0][0]
	}
	s.refcnt = 1
	return s
}

func (s *sharedBackendConn) Addr() string {
	if s == nil {
		return ""
	}
	return s.addr
}

func (s *sharedBackendConn) Release() {
	if s == nil {
		return
	}
	if s.refcnt <= 0 {
		log.Panicf("shared backend conn has been closed, close too many times")
	} else {
		s.refcnt--
	}
	if s.refcnt != 0 {
		return
	}
	for dbnum := range s.conns {
		for i := range s.conns[dbnum] {
			s.conns[dbnum][i].Close()
		}
	}
	delete(s.owner.pool, s.addr)
}

func (s *sharedBackendConn) Retain() *sharedBackendConn {
	if s == nil {
		return nil
	}
	if s.refcnt <= 0 {
		log.Panicf("shared backend conn has been closed")
	} else {
		s.refcnt++
	}
	return s
}

func (s *sharedBackendConn) KeepAlive() {
	if s == nil {
		return
	}
	for dbnum := range s.conns {
		for i := range s.conns[dbnum] {
			s.conns[dbnum][i].KeepAlive()
		}
	}
}

func (s *sharedBackendConn) BackendConn(dbnum int, seed uint, must bool) *BackendConn {
	if s == nil {
		return nil
	}

	if bc := s.single; bc != nil {
		if must || bc.IsConnected() {
			return bc
		} else {
			return nil
		}
	}

	var conns = s.conns[dbnum]
	var i = seed
	for _ = range conns {
		i = (i + 1) % uint(len(conns))
		if bc := conns[i]; bc.IsConnected() {
			return bc
		}
	}
	if !must {
		return nil
	}
	return s.conns[dbnum][0]
}

type sharedBackendConnPool struct {
	parallel int
	database int

	pool map[string]*sharedBackendConn
}

func newSharedBackendConnPool(parallel, database int) *sharedBackendConnPool {
	p := &sharedBackendConnPool{}
	p.parallel = math2.MaxInt(1, parallel)
	p.database = math2.MaxInt(1, database)
	p.pool = make(map[string]*sharedBackendConn)
	return p
}

func (p *sharedBackendConnPool) KeepAlive() {
	for _, bc := range p.pool {
		bc.KeepAlive()
	}
}

func (p *sharedBackendConnPool) Get(addr string) *sharedBackendConn {
	return p.pool[addr]
}

func (p *sharedBackendConnPool) Retain(addr string, config *Config) *sharedBackendConn {
	if bc := p.pool[addr]; bc != nil {
		return bc.Retain()
	} else {
		bc = newSharedBackendConn(addr, config, p)
		p.pool[addr] = bc
		return bc
	}
}
