package aof

import (
	"github.com/hdt3213/godis/config"
	"github.com/hdt3213/godis/interface/database"
	"github.com/hdt3213/godis/lib/logger"
	"github.com/hdt3213/godis/lib/utils"
	"github.com/hdt3213/godis/redis/connection"
	"github.com/hdt3213/godis/redis/parser"
	"github.com/hdt3213/godis/redis/protocol"
	"io"
	"os"
	"strconv"
	"sync"
)

// CmdLine is alias for [][]byte, represents a command line
type CmdLine = [][]byte

const (
	aofQueueSize = 1 << 16
)

type payload struct {
	cmdLine CmdLine
	dbIndex int
}

// Listener is a channel receive a replication of all aof payloads
// with a listener we can forward the updates to slave nodes etc.
type Listener chan<- CmdLine

// Handler receive msgs from channel and write to AOF file
type Handler struct {
	db          database.EmbedDB
	tmpDBMaker  func() database.EmbedDB
	aofChan     chan *payload
	aofFile     *os.File
	aofFilename string
	// aof goroutine will send msg to main goroutine through this channel when aof tasks finished and ready to shutdown
	aofFinished chan struct{}
	// pause aof for start/finish aof rewrite progress
	pausingAof sync.RWMutex
	currentDB  int
	listeners  map[Listener]struct{}
}

// NewAOFHandler creates a new aof.Handler
func NewAOFHandler(db database.EmbedDB, tmpDBMaker func() database.EmbedDB) (*Handler, error) {
	handler := &Handler{}
	handler.aofFilename = config.Properties.AppendFilename
	handler.db = db
	handler.tmpDBMaker = tmpDBMaker
	handler.LoadAof(0)
	aofFile, err := os.OpenFile(handler.aofFilename, os.O_APPEND|os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	handler.aofFile = aofFile
	handler.aofChan = make(chan *payload, aofQueueSize)
	handler.aofFinished = make(chan struct{})
	handler.listeners = make(map[Listener]struct{})
	go func() {
		handler.handleAof()
	}()
	return handler, nil
}

// RemoveListener removes a listener from aof handler, so we can close the listener
func (handler *Handler) RemoveListener(listener Listener) {
	handler.pausingAof.Lock()
	defer handler.pausingAof.Unlock()
	delete(handler.listeners, listener)
}

// AddAof send command to aof goroutine through channel
func (handler *Handler) AddAof(dbIndex int, cmdLine CmdLine) {
	if config.Properties.AppendOnly && handler.aofChan != nil {
		handler.aofChan <- &payload{
			cmdLine: cmdLine,
			dbIndex: dbIndex,
		}
	}
}

// handleAof listen aof channel and write into file
func (handler *Handler) handleAof() {
	// serialized execution
	var cmdLines []CmdLine
	handler.currentDB = 0
	for p := range handler.aofChan {
		cmdLines = cmdLines[:0]    // reuse underlying array
		handler.pausingAof.RLock() // prevent other goroutines from pausing aof
		if p.dbIndex != handler.currentDB {
			// select db
			selectCmd := utils.ToCmdLine("SELECT", strconv.Itoa(p.dbIndex))
			cmdLines = append(cmdLines, selectCmd)
			data := protocol.MakeMultiBulkReply(selectCmd).ToBytes()
			_, err := handler.aofFile.Write(data)
			if err != nil {
				logger.Warn(err)
				handler.pausingAof.RUnlock()
				continue // skip this command
			}
			handler.currentDB = p.dbIndex
		}
		data := protocol.MakeMultiBulkReply(p.cmdLine).ToBytes()
		cmdLines = append(cmdLines, p.cmdLine)
		_, err := handler.aofFile.Write(data)
		if err != nil {
			logger.Warn(err)
		}
		handler.pausingAof.RUnlock()
		for listener := range handler.listeners {
			for _, line := range cmdLines {
				listener <- line
			}
		}
	}
	handler.aofFinished <- struct{}{}
}

// LoadAof read aof file
func (handler *Handler) LoadAof(maxBytes int) {
	// delete aofChan to prevent write again
	aofChan := handler.aofChan
	handler.aofChan = nil
	defer func(aofChan chan *payload) {
		handler.aofChan = aofChan
	}(aofChan)

	file, err := os.Open(handler.aofFilename)
	if err != nil {
		if _, ok := err.(*os.PathError); ok {
			return
		}
		logger.Warn(err)
		return
	}
	defer file.Close()

	var reader io.Reader
	if maxBytes > 0 {
		reader = io.LimitReader(file, int64(maxBytes))
	} else {
		reader = file
	}
	ch := parser.ParseStream(reader)
	fakeConn := connection.NewFakeConn() // only used for save dbIndex
	for p := range ch {
		if p.Err != nil {
			if p.Err == io.EOF {
				break
			}
			logger.Error("parse error: " + p.Err.Error())
			continue
		}
		if p.Data == nil {
			logger.Error("empty payload")
			continue
		}
		r, ok := p.Data.(*protocol.MultiBulkReply)
		if !ok {
			logger.Error("require multi bulk protocol")
			continue
		}
		ret := handler.db.Exec(fakeConn, r.Args)
		if protocol.IsErrorReply(ret) {
			logger.Error("exec err", string(ret.ToBytes()))
		}
	}
}

// Close gracefully stops aof persistence procedure
func (handler *Handler) Close() {
	if handler.aofFile != nil {
		close(handler.aofChan)
		<-handler.aofFinished // wait for aof finished
		err := handler.aofFile.Close()
		if err != nil {
			logger.Warn(err)
		}
	}
}
