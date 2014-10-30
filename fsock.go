/*
fsock.go is released under the MIT License <http://www.opensource.org/licenses/mit-license.php
Copyright (C) ITsysCOM. All Rights Reserved.

Provides FreeSWITCH socket communication.

*/

package fsock

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"log/syslog"
	"net"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

func IndexStringAll(origStr, srchd string) []int {
	foundIdxs := make([]int, 0)
	lenSearched := len(srchd)
	startIdx := 0
	for {
		idxFound := strings.Index(origStr[startIdx:], srchd)
		if idxFound == -1 {
			break
		} else {
			idxFound += startIdx
			foundIdxs = append(foundIdxs, idxFound)
			startIdx = idxFound + lenSearched // Skip the characters found on next check
		}
	}
	return foundIdxs
}

// Split considering {}[] which cancel separator
func SplitIgnoreGroups(origStr, sep string) []string {
	if len(origStr) == 0 {
		return []string{}
	} else if len(sep) == 0 {
		return []string{origStr}
	}
	retSplit := make([]string, 0)
	cmIdxs := IndexStringAll(origStr, ",") // Main indexes of separators
	if len(cmIdxs) == 0 {
		return []string{origStr}
	}
	oCrlyIdxs := IndexStringAll(origStr, "{") // Index  { for exceptions
	cCrlyIdxs := IndexStringAll(origStr, "}") // Index  } for exceptions closing
	oBrktIdxs := IndexStringAll(origStr, "[") // Index [ for exceptions
	cBrktIdxs := IndexStringAll(origStr, "]") // Index ] for exceptions closing
	lastNonexcludedIdx := 0
	for i, cmdIdx := range cmIdxs {
		if len(oCrlyIdxs) == len(cCrlyIdxs) && len(oBrktIdxs) == len(cBrktIdxs) { // We assume exceptions and closing them are symetrical, otherwise don't handle exceptions
			exceptFound := false
			for iCrlyIdx := range oCrlyIdxs {
				if oCrlyIdxs[iCrlyIdx] < cmdIdx && cCrlyIdxs[iCrlyIdx] > cmdIdx { // Parentheses canceling indexing found
					exceptFound = true
					break
				}
			}
			for oBrktIdx := range oBrktIdxs {
				if oBrktIdxs[oBrktIdx] < cmdIdx && cBrktIdxs[oBrktIdx] > cmdIdx { // Parentheses canceling indexing found
					exceptFound = true
					break
				}
			}
			if exceptFound {
				continue
			}
		}
		switch i {
		case 0: // First one
			retSplit = append(retSplit, origStr[:cmIdxs[i]])
		case len(cmIdxs) - 1: // Last one
			postpendStr := ""
			if len(origStr) > cmIdxs[i]+1 { // Our separator is not the last character in the string
				postpendStr = origStr[cmIdxs[i]+1:]
			}
			retSplit = append(retSplit, origStr[cmIdxs[lastNonexcludedIdx]+1:cmIdxs[i]], postpendStr)
		default:
			retSplit = append(retSplit, origStr[cmIdxs[lastNonexcludedIdx]+1:cmIdxs[i]]) // Discard the separator from end string
		}
		lastNonexcludedIdx = i
	}
	return retSplit
}

// Extracts value of a header from anywhere in content string
func headerVal(hdrs, hdr string) string {
	var hdrSIdx, hdrEIdx int
	if hdrSIdx = strings.Index(hdrs, hdr); hdrSIdx == -1 {
		return ""
	} else if hdrEIdx = strings.Index(hdrs[hdrSIdx:], "\n"); hdrEIdx == -1 {
		hdrEIdx = len(hdrs[hdrSIdx:])
	}
	splt := strings.SplitN(hdrs[hdrSIdx:hdrSIdx+hdrEIdx], ": ", 2)
	if len(splt) != 2 {
		return ""
	}
	return strings.TrimSpace(strings.TrimRight(splt[1], "\n"))
}

// FS event header values are urlencoded. Use this to decode them. On error, use original value
func urlDecode(hdrVal string) string {
	if valUnescaped, errUnescaping := url.QueryUnescape(hdrVal); errUnescaping == nil {
		hdrVal = valUnescaped
	}
	return hdrVal
}

// Binary string search in slice
func isSliceMember(ss []string, s string) bool {
	sort.Strings(ss)
	if i := sort.SearchStrings(ss, s); i < len(ss) && ss[i] == s {
		return true
	}
	return false
}

// Convert fseventStr into fseventMap
func FSEventStrToMap(fsevstr string, headers []string) map[string]string {
	fsevent := make(map[string]string)
	filtered := false
	if len(headers) != 0 {
		filtered = true
	}
	for _, strLn := range strings.Split(fsevstr, "\n") {
		if hdrVal := strings.SplitN(strLn, ": ", 2); len(hdrVal) == 2 {
			if filtered && isSliceMember(headers, hdrVal[0]) {
				continue // Loop again since we only work on filtered fields
			}
			fsevent[hdrVal[0]] = urlDecode(strings.TrimSpace(strings.TrimRight(hdrVal[1], "\n")))
		}
	}
	return fsevent
}

// Converts string received from fsock into a list of channel info, each represented in a map
func MapChanData(chanInfoStr string) []map[string]string {
	chansInfoMap := make([]map[string]string, 0)
	spltChanInfo := strings.Split(chanInfoStr, "\n")
	if len(spltChanInfo) <= 5 {
		return chansInfoMap
	}
	hdrs := strings.Split(spltChanInfo[0], ",")
	for _, chanInfoLn := range spltChanInfo[1 : len(spltChanInfo)-3] {
		chanInfo := SplitIgnoreGroups(chanInfoLn, ",")
		if len(hdrs) != len(chanInfo) {
			continue
		}
		chnMp := make(map[string]string, 0)
		for iHdr, hdr := range hdrs {
			chnMp[hdr] = chanInfo[iHdr]
		}
		chansInfoMap = append(chansInfoMap, chnMp)
	}
	return chansInfoMap
}

// successive Fibonacci numbers.
func fib() func() int {
	a, b := 0, 1
	return func() int {
		a, b = b, a+b
		return a
	}
}

var FS *FSock // Used to share FS connection via package globals

// Connection to FreeSWITCH Socket
type FSock struct {
	conn               net.Conn
	buffer             *bufio.Reader
	fsaddress, fspaswd string
	eventHandlers      map[string][]func(string)
	eventFilters       map[string]string
	apiChan, cmdChan   chan string
	reconnects         int
	delayFunc          func() int
	logger             *syslog.Writer
}

// Reads headers until delimiter reached
func (self *FSock) readHeaders() (s string, err error) {
	bytesRead := make([]byte, 0)
	var readLine []byte
	for {
		readLine, err = self.buffer.ReadBytes('\n')
		if err != nil {
			if self.logger != nil {
				self.logger.Err(fmt.Sprintf("<FSock> Error reading headers: <%s>", err.Error()))
			}
			self.Disconnect()
			return
		}
		// No Error, add received to localread buffer
		if len(bytes.TrimSpace(readLine)) == 0 {
			break
		}
		bytesRead = append(bytesRead, readLine...)
	}
	return string(bytesRead), nil
}

// Reads the body from buffer, ln is given by content-length of headers
func (self *FSock) readBody(ln int) (string, error) {
	bytesRead := make([]byte, ln)
	for i := 0; i < ln; i++ {
		if readByte, err := self.buffer.ReadByte(); err != nil {
			if self.logger != nil {
				self.logger.Err(fmt.Sprintf("<FSock> Error reading message body: <%s>", err.Error()))
			}
			self.Disconnect()
			return "", err
		} else { // No Error, add received to localread buffer
			bytesRead[i] = readByte // Add received line to the local read buffer
		}
	}
	return string(bytesRead), nil
}

// Event is made out of headers and body (if present)
func (self *FSock) readEvent() (string, string, error) {
	var hdrs, body string
	var cl int
	var err error

	if hdrs, err = self.readHeaders(); err != nil {
		return "", "", err
	}
	if !strings.Contains(hdrs, "Content-Length") { //No body
		return hdrs, "", nil
	}
	clStr := headerVal(hdrs, "Content-Length")
	if cl, err = strconv.Atoi(clStr); err != nil {
		return "", "", errors.New("Cannot extract content length")
	}
	if body, err = self.readBody(cl); err != nil {
		return "", "", err
	}
	return hdrs, body, nil
}

// Checks if socket connected. Can be extended with pings
func (self *FSock) Connected() bool {
	if self.conn == nil {
		return false
	}
	return true
}

// Disconnects from socket
func (self *FSock) Disconnect() (err error) {
	if self.conn != nil {
		if self.logger != nil {
			self.logger.Info("<FSock> Disconnecting from FreeSWITCH!")
		}
		err = self.conn.Close()
		self.conn = nil
	}
	return
}

// Auth to FS
func (self *FSock) auth() error {
	authCmd := fmt.Sprintf("auth %s\n\n", self.fspaswd)
	fmt.Fprint(self.conn, authCmd)
	if rply, err := self.readHeaders(); err != nil {
		return err
	} else if !strings.Contains(rply, "Reply-Text: +OK accepted") {
		return fmt.Errorf("Unexpected auth reply received: <%s>", rply)
	}
	return nil
}

// Subscribe to events
func (self *FSock) eventsPlain(events []string) error {
	if len(events) == 0 {
		return nil
	}
	eventsCmd := "event plain"
	for _, ev := range events {
		if ev == "ALL" {
			eventsCmd = "event plain all"
			break
		}
		eventsCmd += " " + ev
	}
	eventsCmd += "\n\n"
	fmt.Fprint(self.conn, eventsCmd)
	if rply, err := self.readHeaders(); err != nil {
		return err
	} else if !strings.Contains(rply, "Reply-Text: +OK") {
		self.Disconnect()
		return fmt.Errorf("Unexpected events-subscribe reply received: <%s>", rply)
	}
	return nil
}

// Enable filters
func (self *FSock) filterEvents(filters map[string]string) error {
	if len(filters) == 0 { //Nothing to filter
		return nil
	}

	for hdr, val := range filters {
		cmd := "filter " + hdr + " " + val + "\n\n"
		fmt.Fprint(self.conn, cmd)
		if rply, err := self.readHeaders(); err != nil {
			return err
		} else if !strings.Contains(rply, "Reply-Text: +OK") {
			return fmt.Errorf("Unexpected filter-events reply received: <%s>", rply)
		}
	}

	return nil
}

// Connect or reconnect
func (self *FSock) Connect() error {
	if self.Connected() {
		self.Disconnect()
	}
	var conErr error
	for i := 0; i < self.reconnects; i++ {
		self.conn, conErr = net.Dial("tcp", self.fsaddress)
		if conErr == nil {
			if self.logger != nil {
				self.logger.Info("<FSock> Successfully connected to FreeSWITCH!")
			}
			// Connected, init buffer, auth and subscribe to desired events and filters
			self.buffer = bufio.NewReaderSize(self.conn, 8192) // reinit buffer
			if authChg, err := self.readHeaders(); err != nil || !strings.Contains(authChg, "auth/request") {
				return errors.New("No auth challenge received")
			} else if errAuth := self.auth(); errAuth != nil { // Auth did not succeed
				return errAuth
			}
			// Subscribe to events handled by event handlers
			handledEvs := make([]string, len(self.eventHandlers))
			j := 0
			for k := range self.eventHandlers {
				handledEvs[j] = k
				j++
			}
			if subscribeErr := self.eventsPlain(handledEvs); subscribeErr != nil {
				return subscribeErr
			}
			if filterErr := self.filterEvents(self.eventFilters); filterErr != nil {
				return filterErr
			}
			return nil
		}
		time.Sleep(time.Duration(self.delayFunc()) * time.Second)
	}
	return conErr
}

// Send API command
func (self *FSock) SendApiCmd(cmdStr string) (string, error) {
	if !self.Connected() {
		return "", errors.New("Not connected to FS")
	}
	cmd := fmt.Sprintf("api %s\n\n", cmdStr)
	fmt.Fprint(self.conn, cmd)
	resEvent := <-self.apiChan
	if strings.Contains(resEvent, "-ERR") {
		return "", errors.New("Command failed")
	}
	return resEvent, nil
}

// SendMessage command
func (self *FSock) SendMsgCmd(uuid string, cmdargs map[string]string) error {
	if len(cmdargs) == 0 {
		return errors.New("Need command arguments")
	}
	if !self.Connected() {
		return errors.New("Not connected to FS")
	}
	argStr := ""
	for k, v := range cmdargs {
		argStr += fmt.Sprintf("%s:%s\n", k, v)
	}
	fmt.Fprint(self.conn, fmt.Sprintf("sendmsg %s\n%s\n", uuid, argStr))
	replyTxt := <-self.cmdChan
	if strings.HasPrefix(replyTxt, "-ERR") {
		return fmt.Errorf("SendMessage: %s", replyTxt)
	}
	return nil
}

// Reads events from socket
func (self *FSock) ReadEvents() {
	// Read events from buffer, firing them up further
	for {
		hdr, body, err := self.readEvent()
		if err != nil {
			if self.logger != nil {
				self.logger.Err(fmt.Sprintf("<FSock> Error reading events: <%s>", err.Error()))
			}
			connErr := self.Connect()
			if connErr != nil {
				return
			}
			continue // Connection reset
		}
		if strings.Contains(hdr, "api/response") {
			self.apiChan <- body
		} else if strings.Contains(hdr, "command/reply") {
			self.cmdChan <- headerVal(hdr, "Reply-Text")
		} else if body != "" { // We got a body, could be event, try dispatching it
			self.dispatchEvent(body)
		}
	}
	return
}

// Dispatch events to handlers in async mode
func (self *FSock) dispatchEvent(event string) {
	eventName := headerVal(event, "Event-Name")
	handleNames := []string{eventName, "ALL"}
	dispatched := false
	for _, handleName := range handleNames {
		if _, hasHandlers := self.eventHandlers[handleName]; hasHandlers {
			// We have handlers, dispatch to all of them
			for _, handlerFunc := range self.eventHandlers[handleName] {
				go handlerFunc(event)
				dispatched = true
				return
			}
		}
	}
	if !dispatched && self.logger != nil {
		self.logger.Warning(fmt.Sprintf("<FSock> No dispatcher for event: <%+v>", event))
	}
}

// Connects to FS and starts buffering input
func NewFSock(fsaddr, fspaswd string, reconnects int, eventHandlers map[string][]func(string), eventFilters map[string]string, l *syslog.Writer) (*FSock, error) {
	fsock := FSock{fsaddress: fsaddr, fspaswd: fspaswd, eventHandlers: eventHandlers, eventFilters: eventFilters, reconnects: reconnects, logger: l}
	fsock.apiChan = make(chan string) // Init apichan so we can use it to pass api replies
	fsock.cmdChan = make(chan string)
	fsock.delayFunc = fib()
	errConn := fsock.Connect()
	if errConn != nil {
		return nil, errConn
	}
	return &fsock, nil
}

// Connection handler for commands sent to FreeSWITCH
type FSockPool struct {
	fsAddr, fsPasswd string
	reconnects       int
	eventHandlers    map[string][]func(string)
	eventFilters     map[string]string
	readEvents       bool // Fork reading events when creating the socket
	logger           *syslog.Writer
	allowedConns     chan struct{} // Will be populated with members allowed
	fSocks           chan *FSock   // Keep here reference towards the list of opened sockets
}

func (self *FSockPool) PopFSock() (*FSock, error) {
	if len(self.fSocks) != 0 { // Select directly if available, so we avoid randomness of selection
		fsock := <-self.fSocks
		return fsock, nil
	}
	var fsock *FSock
	var err error
	select { // No fsock available in the pool, wait for first one showing up
	case fsock = <-self.fSocks:
	case <-self.allowedConns:
		fsock, err = NewFSock(self.fsAddr, self.fsPasswd, 1, self.eventHandlers, self.eventFilters, self.logger)
		if err != nil {
			return nil, err
		}
		if self.readEvents {
			go fsock.ReadEvents() // Read events permanently, errors will be detected on connection returned to the pool
		}
		return fsock, nil
	}

	return fsock, nil
}

func (self *FSockPool) PushFSock(fsk *FSock) {
	if fsk.Connected() { // We only add it back if the socket is still connected
		self.fSocks <- fsk
	} else {
		self.allowedConns <- struct{}{}
	}
}

// Instantiates a new FSockPool
func NewFSockPool(maxFSocks int, readEvents bool,
	fsaddr, fspasswd string, reconnects int, eventHandlers map[string][]func(string), eventFilters map[string]string, l *syslog.Writer) (*FSockPool, error) {
	pool := &FSockPool{fsAddr: fsaddr, fsPasswd: fspasswd, reconnects: reconnects, eventHandlers: eventHandlers, eventFilters: eventFilters, readEvents: readEvents, logger: l}
	pool.allowedConns = make(chan struct{}, maxFSocks)
	var emptyConn struct{}
	for i := 0; i < maxFSocks; i++ {
		pool.allowedConns <- emptyConn // Empty initiate so we do not need to wait later when we pop
	}
	pool.fSocks = make(chan *FSock, maxFSocks)
	return pool, nil
}
