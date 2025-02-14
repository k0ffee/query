//  Copyright 2021-Present Couchbase, Inc.
//
//  Use of this software is governed by the Business Source License included
//  in the file licenses/BSL-Couchbase.txt.  As of the Change Date specified
//  in that file, in accordance with the Business Source License, use of this
//  software will be governed by the Apache License, Version 2.0, included in
//  the file licenses/APL2.txt.

// package couchbase provides low level access to the KV store and the orchestrator
package couchbase

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/couchbase/gomemcached"
	"github.com/couchbase/gomemcached/client" // package name is 'memcached'
	"github.com/couchbase/query/logging"
	"github.com/couchbase/query/util"
)

// Mutation Token
type MutationToken struct {
	VBid  uint16 // vbucket id
	Guard uint64 // vbuuid
	Value uint64 // sequence number
}

// Maximum number of times to retry a chunk of a bulk get on error.
const maxBulkRetries = 5000
const backOffDuration time.Duration = 100 * time.Millisecond
const minBackOffRetriesLimit = 25  // exponentail backOff result in over 30sec (25*13*0.1s)
const maxBackOffRetriesLimit = 100 // exponentail backOff result in over 2min (100*13*0.1s)

// Return true if error is KEY_EEXISTS
func IsKeyEExistsError(err error) bool {

	res, ok := err.(*gomemcached.MCResponse)
	if ok && res.Status == gomemcached.KEY_EEXISTS {
		return true
	}

	return false
}

// Return true if error is KEY_ENOENT
func IsKeyNoEntError(err error) bool {

	res, ok := err.(*gomemcached.MCResponse)
	if ok && res.Status == gomemcached.KEY_ENOENT {
		return true
	}

	return false
}

// Return true if error suggests a bucket refresh is required
func IsRefreshRequired(err error) bool {

	res, ok := err.(*gomemcached.MCResponse)
	if ok && (res.Status == gomemcached.NO_BUCKET || res.Status == gomemcached.NOT_MY_VBUCKET) {
		return true
	}

	return false
}

// Return true if a collection is not known
func IsUnknownCollection(err error) bool {

	res, ok := err.(*gomemcached.MCResponse)
	if ok && (res.Status == gomemcached.UNKNOWN_COLLECTION) {
		return true
	}

	return false
}

// infrastructure for GetBulk() and Do()
type doDescriptor struct {
	retry           bool
	discard         bool
	useReplicas     bool
	version         int
	replica         int
	backOffAttempts int
	attempts        int
	maxTries        int
	errorString     string
	amendReplica    bool
	pool            *connectionPool
}

// Given a vbucket number, returns a memcached connection to it.
// The connection must be returned to its pool after use.
func (b *Bucket) getConnectionToVBucket(vb uint32, desc *doDescriptor) (*memcached.Client, *connectionPool, error) {
	vbm := b.VBServerMap()
	if len(vbm.VBucketMap) < int(vb) {
		return nil, nil, fmt.Errorf("primitives/couchbase: vbmap smaller than vbucket list: %v vs. %v",
			vb, vbm.VBucketMap)
	}
	for {
		if desc.replica+1 > len(vbm.VBucketMap[vb]) {
			return nil, nil, fmt.Errorf("primitives/couchbase: invalid vbmap entry for vb %v (len %v, replicas %v)", vb, len(vbm.VBucketMap[vb]), vbm.NumReplicas)
		}
		masterId := vbm.VBucketMap[vb][desc.replica]
		if masterId < 0 {
			return nil, nil, fmt.Errorf("primitives/couchbase: No master for vbucket %d", vb)
		}
		if desc.useReplicas && vbm.IsDown(masterId) && desc.replica < vbm.NumReplicas {
			desc.replica++
			continue
		}
		pool := b.getConnPool(masterId)
		conn, err := pool.Get()
		if err != errClosedPool {
			return conn, pool, err
		}
		// If conn pool was closed, because another goroutine refreshed the vbucket map, retry...
	}
}

// first part of the retry loop: get a connection and handle errors
func (b *Bucket) getVbConnection(vb uint32, desc *doDescriptor) (conn *memcached.Client, pool *connectionPool, err error) {
	desc.errorString = ""

	// if we had a NMVB and have successfully identified the pool for the correct node, use it
	// if it doesn't work out, fall back to the old method
	if desc.pool != nil {
		pool = desc.pool
		desc.pool = nil
		conn, err = pool.Get()
		if conn == nil {
			conn, pool, err = b.getConnectionToVBucket(vb, desc)
		}
	} else {
		conn, pool, err = b.getConnectionToVBucket(vb, desc)
	}
	desc.retry = false
	if err == nil {
		return conn, pool, nil
	} else if err == errNoPool {
		if backOff(desc.backOffAttempts, desc.maxTries, backOffDuration, true) {
			b.Refresh()
			desc.backOffAttempts++
			desc.retry = true
		} else {
			desc.errorString = "Connection Error no pool %v : %v"
		}
	} else if isConnError(err) {
		if backOff(desc.backOffAttempts, desc.maxTries, backOffDuration, true) {

			// check for a new vbmap
			if desc.version == b.Version {
				b.Refresh()
			}

			// if one's available, assume the master is up
			if desc.version != b.Version {
				desc.version = b.Version
				desc.replica = 0
				desc.amendReplica = false
			} else if desc.useReplicas && desc.replica < b.VBServerMap().NumReplicas {

				// see if we can use a replica
				desc.replica++
				desc.amendReplica = true
			}
			desc.backOffAttempts++
			desc.retry = true
		} else {
			desc.errorString = "Connection Error %v : %v"
		}
	} else if isAddrNotAvailable(err) {
		if backOff(desc.backOffAttempts, desc.maxTries, backOffDuration, true) {
			b.Refresh()
			desc.backOffAttempts++
			desc.retry = true
		} else {
			desc.errorString = "Out of ephemeral ports: %v : %v"
		}
	} else if isTimeoutError(err) {
		desc.retry = true

		// check for a new vbmap
		if desc.version == b.Version {
			b.Refresh()
		}

		// if one's available, assume the master is up
		if desc.version != b.Version {
			desc.version = b.Version
			desc.replica = 0
			desc.amendReplica = false
		} else if desc.useReplicas {

			// see if we can use a replica
			desc.replica++
			desc.amendReplica = true
			desc.retry = desc.replica <= b.VBServerMap().NumReplicas
		} else {
			desc.retry = false
		}

		if !desc.retry {
			desc.errorString = "Connection Error %v : %v"
		}
	}
	return nil, nil, err
}

// second part of the retry loop: handle the command errors and retry strategy
func (b *Bucket) processOpError(vb uint32, lastError error, node string, desc *doDescriptor) {
	desc.retry = false
	desc.discard = false

	// MB-30967 / MB-31001 implement back off for transient errors
	if resp, ok := lastError.(*gomemcached.MCResponse); ok {
		switch resp.Status {
		case gomemcached.NOT_MY_VBUCKET:

			// first, can we use the NMVB response vbmap entry?
			newPool, newNode := b.handleNMVB(vb, resp)

			// KV response might still send old map
			// go the old way if we don't have a different node
			if newPool != nil && newNode != node {
				desc.retry = true
				desc.pool = newPool
				desc.discard = b.obsoleteNode(node)
				return
			}

			// if it didn't work out, refresh and retry until we get a new vbmap
			desc.backOffAttempts++
			desc.retry = backOff(desc.backOffAttempts, desc.maxTries, backOffDuration, false)
			if desc.retry && desc.version == b.Version {
				b.Refresh()
			}
			desc.version = b.Version

			// MB-28842: in case of NMVB, check if the node is still part of the map
			// and ditch the connection if it isn't.
			desc.discard = b.obsoleteNode(node)
		case gomemcached.NOT_SUPPORTED:
			b.Refresh()
			desc.discard = b.obsoleteNode(node)
			desc.backOffAttempts++
			desc.retry = backOff(desc.backOffAttempts, desc.maxTries, backOffDuration, true)
		case gomemcached.EBUSY, gomemcached.LOCKED:
			// TODO backOff instead?
			if (desc.attempts % (maxBulkRetries / 100)) == 0 {
				desc.errorString = "Retrying Memcached error (%v) FOR %v(vbid:%d, keys:<ud>%v</ud>)"
			}
			desc.retry = true
		case gomemcached.ENOMEM, gomemcached.TMPFAIL:
			desc.errorString = "Retrying Memcached error (%v) FOR %v(vbid:%d, keys:<ud>%v</ud>)"
			desc.backOffAttempts++
			desc.retry = backOff(desc.backOffAttempts, desc.maxTries, backOffDuration, true)
		}
	} else if lastError != nil {
		if isOutOfBoundsError(lastError) {

			// We got an out of bounds error or a read timeout error; retry the operation
			desc.discard = true
			desc.retry = true
		} else if isConnError(lastError) {
			desc.discard = true
			desc.backOffAttempts++
			desc.retry = backOff(desc.backOffAttempts, desc.maxTries, backOffDuration, true)
		} else if IsReadTimeOutError(lastError) {
			desc.discard = true
			desc.retry = true

			// check for a new vbmap
			if desc.version == b.Version {
				b.Refresh()
			}

			// if one's available, assume the master is up
			if desc.version != b.Version {
				desc.version = b.Version
				desc.replica = 0
				desc.amendReplica = false
			} else if desc.useReplicas {

				// see if we can use a replica
				desc.replica++
				desc.amendReplica = true
				desc.retry = desc.replica <= b.VBServerMap().NumReplicas
			} else {
				desc.retry = false
			}

		}
	} else if desc.amendReplica {

		// if we successfully used a replica, mark the node down, so that until the next refresh
		// we start from the same place
		b.VBServerMap().MarkDown(vb, desc.replica)
	}
}

// dummy type to extract the VB map from a memcached error
type refreshVB struct {
	VBSMJson VBucketServerMap `json:"vBucketServerMap"`
}

func (b *Bucket) handleNMVB(vb uint32, resp *gomemcached.MCResponse) (*connectionPool, string) {
	if resp != nil && len(resp.Body) > 0 {
		tmpVB := &refreshVB{}
		if json.Unmarshal(resp.Body, &tmpVB) == nil {
			if len(tmpVB.VBSMJson.ServerList) > 0 && len(tmpVB.VBSMJson.VBucketMap) > 0 {

				// if the vbmap is good and we find the node in the current node list, use that pool
				if int(vb) < len(tmpVB.VBSMJson.VBucketMap) {
					masterId := tmpVB.VBSMJson.VBucketMap[int(vb)][0]
					nodes := b.VBServerMap().ServerList
					node := tmpVB.VBSMJson.ServerList[masterId]
					if masterId < len(nodes) && nodes[masterId] == node {
						return b.getConnPool(masterId), node
					}
					for i := range nodes {
						if nodes[i] == node {
							return b.getConnPool(i), node
						}
					}
				}
			}
		}
	}
	return nil, ""
}

func (b *Bucket) backOffRetries() int {
	res := 2 * len(b.Nodes())
	if res < minBackOffRetriesLimit {
		return minBackOffRetriesLimit
	}
	if res > maxBackOffRetriesLimit {
		return maxBackOffRetriesLimit
	}
	return res
}

// Do executes a function on a memcached connection to the node owning key "k"
//
// Note that this automatically handles transient errors by replaying
// your function on a "not-my-vbucket" error, so don't assume
// your command will only be executed once.
func (b *Bucket) Do(k string, f func(mc *memcached.Client, vb uint16) error) (err error) {
	return b.Do2(k, f, true, false, 2*len(b.Nodes()))
}

func (b *Bucket) Do2(k string, f func(mc *memcached.Client, vb uint16) error, deadline bool, useReplicas bool, backOffRetries int) (err error) {
	var lastError error

	vb := b.VBHash(k)
	desc := &doDescriptor{useReplicas: useReplicas, version: b.Version, maxTries: backOffRetries}

	for desc.attempts = 0; desc.attempts < desc.maxTries; desc.attempts++ {
		conn, pool, err := b.getVbConnection(uint32(vb), desc)
		if err != nil {
			if desc.retry {
				continue
			}
			return err
		}
		if deadline && DefaultTimeout > 0 {
			conn.SetDeadline(getDeadline(noDeadline, DefaultTimeout))
		} else {
			conn.SetDeadline(noDeadline)
		}
		if desc.replica > 0 {
			conn.SetReplica(true)
		}
		lastError = f(conn, uint16(vb))
		b.processOpError(uint32(vb), lastError, pool.Node(), desc)

		if desc.discard {
			pool.Discard(conn)
		} else {
			conn.SetReplica(false)
			pool.Return(conn)
		}

		if lastError == nil {
			return nil
		}
		if !desc.retry {
			desc.attempts++
			break
		}
	}

	if resp, ok := lastError.(*gomemcached.MCResponse); ok {
		err := gomemcached.StatusNames[resp.Status]
		if err == "" {
			err = fmt.Sprintf("KV status %v", resp.Status)
		}
		return fmt.Errorf("unable to complete action after %v attempts: %v", desc.attempts, err)
	} else {
		return fmt.Errorf("unable to complete action after %v attempts: %v", desc.attempts, lastError)
	}
}

type GatheredStats struct {
	Server string
	Stats  map[string]string
	Err    error
}

func getStatsParallelFunc(fn func(key, val []byte), sn string, b *Bucket, offset int, which string,
	ch chan<- GatheredStats) {
	pool := b.getConnPool(offset)

	conn, err := pool.Get()

	if err == nil {
		conn.SetDeadline(getDeadline(time.Time{}, DefaultTimeout))
		err = conn.StatsFunc(which, fn)
		pool.Return(conn)
	}
	ch <- GatheredStats{Server: sn, Err: err}
}

// GatherStats returns a map of server ID -> GatheredStats from all servers.
func (b *Bucket) GatherStatsFunc(which string, fn func(key, val []byte)) map[string]error {
	var errMap map[string]error

	vsm := b.VBServerMap()
	if vsm.ServerList == nil {
		return errMap
	}

	// Go grab all the things at once.
	ch := make(chan GatheredStats, len(vsm.ServerList))
	for i, sn := range vsm.ServerList {
		go getStatsParallelFunc(fn, sn, b, i, which, ch)
	}

	// Gather the results
	for range vsm.ServerList {
		gs := <-ch
		if gs.Err != nil {
			if errMap == nil {
				errMap = make(map[string]error)
				errMap[gs.Server] = gs.Err
			}
		}
	}
	return errMap
}

type BucketStats int

const (
	StatCount = BucketStats(iota)
	StatSize
)

var bucketStatString = []string{
	"curr_items",
	"ep_value_size",
}

var collectionStatString = []string{
	"items",
	"data_size",
}

// Get selected bucket or collection stats
func (b *Bucket) GetIntStats(refresh bool, which []BucketStats, context ...*memcached.ClientContext) ([]int64, error) {
	if refresh {
		b.Refresh()
	}

	var vals []int64 = make([]int64, len(which))
	if len(vals) == 0 {
		return vals, nil
	}

	var outErr error
	if len(context) > 0 {

		collKey := fmt.Sprintf("collections-byid 0x%x", context[0].CollId)
		errs := b.GatherStatsFunc(collKey, func(key, val []byte) {
			for i, f := range which {
				lk := len(key)
				ls := len(collectionStatString[f])
				if lk >= ls && string(key[lk-ls:]) == collectionStatString[f] {
					v, err := strconv.ParseInt(string(val), 10, 64)
					if err == nil {
						atomic.AddInt64(&vals[i], v)
					} else if outErr == nil {
						outErr = err
					}
				}
			}
		})

		// have to use a range to access any one element of a map
		for _, err := range errs {
			return nil, err
		}
	} else {
		errs := b.GatherStatsFunc("", func(key, val []byte) {
			for i, f := range which {
				if string(key) == bucketStatString[f] {
					v, err := strconv.ParseInt(string(val), 10, 64)
					if err == nil {
						atomic.AddInt64(&vals[i], v)
					} else if outErr == nil {
						outErr = err
					}
				}
			}
		})

		// have to use a range to access any one element of a map
		for _, err := range errs {
			return nil, err
		}
	}

	return vals, outErr
}

func isAuthError(err error) bool {
	estr := err.Error()
	return strings.Contains(estr, "Auth failure")
}

func IsReadTimeOutError(err error) bool {
	if err == nil {
		return false
	}
	estr := err.Error()
	return strings.Contains(estr, "read tcp") ||
		strings.Contains(estr, "i/o timeout")
}

func isTimeoutError(err error) bool {
	estr := err.Error()
	return strings.Contains(estr, "i/o timeout") ||
		strings.Contains(estr, "connection timed out") ||
		strings.Contains(estr, "no route to host")
}

// Errors that are not considered fatal for our fetch loop
func isConnError(err error) bool {
	if err == io.EOF {
		return true
	}
	estr := err.Error()
	return strings.Contains(estr, "broken pipe") ||
		strings.Contains(estr, "connection reset") ||
		strings.Contains(estr, "connection refused") ||
		strings.Contains(estr, "connection pool is closed")
}

func isOutOfBoundsError(err error) bool {
	return err != nil && strings.Contains(err.Error(), "Out of Bounds error")

}

func isAddrNotAvailable(err error) bool {
	if err == nil {
		return false
	}
	estr := err.Error()
	return strings.Contains(estr, "cannot assign requested address")
}

func getDeadline(reqDeadline time.Time, duration time.Duration) time.Time {
	if reqDeadline.IsZero() {
		if duration > 0 {
			return time.Unix(time.Now().Unix(), 0).Add(duration)
		} else {
			return noDeadline
		}
	}
	return reqDeadline
}

func backOff(attempt, maxAttempts int, duration time.Duration, exponential bool) bool {
	if attempt < maxAttempts {
		// 0th attempt return immediately
		if attempt > 0 {
			if exponential {
				duration = time.Duration(attempt) * duration
			}
			time.Sleep(duration)
		}
		return true
	}

	return false
}

func (b *Bucket) doBulkGet(vb uint16, keys []string, reqDeadline time.Time,
	ch chan<- map[string]*gomemcached.MCResponse, ech chan<- error, subPaths []string,
	useReplica bool, eStatus *errorStatus, context ...*memcached.ClientContext) {

	rv := _STRING_MCRESPONSE_POOL.Get()
	done := false
	bname := b.Name
	desc := &doDescriptor{useReplicas: useReplica, version: b.Version, maxTries: b.backOffRetries()}
	var lastError error
	for ; desc.attempts < maxBulkRetries && !done && !eStatus.errStatus; desc.attempts++ {

		// This stack frame exists to ensure we can clean up
		// connection at a reasonable time.
		err := func() error {
			conn, pool, err := b.getVbConnection(uint32(vb), desc)
			if err != nil {
				if !desc.retry {
					ech <- err
					if desc.errorString != "" {
						logging.Infof(desc.errorString, b.Name, err)
					}
					return err
				}
				if lastError == nil || err.Error() != lastError.Error() || maxBulkRetries-1 == desc.attempts {
					if lastError != nil {
						logging.Infof("(... attempt: %v) Pool Get returned %v: %v", desc.attempts-1, bname, err)
					}
					logging.Infof("(Attempt: %v) Pool Get returned %v: %v", desc.attempts, bname, err)
					lastError = err
				}

				return nil
			}
			lastError = nil

			conn.SetDeadline(getDeadline(reqDeadline, DefaultTimeout))
			if desc.replica > 0 {
				conn.SetReplica(true)
			}
			err = conn.GetBulk(vb, keys, rv, subPaths, context...)

			defer func() {
				if desc.discard {
					pool.Discard(conn)
				} else {
					conn.SetReplica(false)
					pool.Return(conn)
				}
			}()

			b.processOpError(uint32(vb), err, pool.Node(), desc)
			if desc.errorString != "" {
				logging.Infof(desc.errorString, err.Error(), bname, vb, keys)
			}
			if desc.retry {
				err = nil
			} else if err == nil {
				done = true
			} else {
				ech <- err
			}
			return err
		}()

		if err != nil {
			return
		}
	}

	if desc.attempts >= maxBulkRetries {
		err := fmt.Errorf("bulkget exceeded MaxBulkRetries for %v(vbid:%d,keys:<ud>%v</ud>)", bname, vb, keys)
		logging.Errorf("%v", err.Error())
		ech <- err
	}

	ch <- rv
}

type errorStatus struct {
	errStatus bool
}

type vbBulkGet struct {
	b           *Bucket
	ch          chan<- map[string]*gomemcached.MCResponse
	ech         chan<- error
	k           uint16
	keys        []string
	reqDeadline time.Time
	wg          *sync.WaitGroup
	subPaths    []string
	useReplica  bool
	groupError  *errorStatus
	context     []*memcached.ClientContext
}

const _NUM_CHANNELS = 5

var _NUM_CHANNEL_WORKERS = (util.NumCPU() + 1) / 2
var DefaultDialTimeout = time.Duration(0)
var DefaultTimeout = time.Duration(0)
var noDeadline = time.Time{}

// Buffer 4k requests per worker
var _VB_BULK_GET_CHANNELS []chan *vbBulkGet

func InitBulkGet() {

	DefaultDialTimeout = 20 * time.Second
	DefaultTimeout = 120 * time.Second

	memcached.SetDefaultDialTimeout(DefaultDialTimeout)

	_VB_BULK_GET_CHANNELS = make([]chan *vbBulkGet, _NUM_CHANNELS)

	for i := 0; i < _NUM_CHANNELS; i++ {
		channel := make(chan *vbBulkGet, 16*1024*_NUM_CHANNEL_WORKERS)
		_VB_BULK_GET_CHANNELS[i] = channel

		for j := 0; j < _NUM_CHANNEL_WORKERS; j++ {
			go vbBulkGetWorker(channel)
		}
	}
}

func vbBulkGetWorker(ch chan *vbBulkGet) {
	defer func() {
		// Workers cannot panic and die
		recover()
		go vbBulkGetWorker(ch)
	}()

	for vbg := range ch {
		vbDoBulkGet(vbg)
	}
}

func vbDoBulkGet(vbg *vbBulkGet) {
	defer vbg.wg.Done()
	defer func() {
		// Workers cannot panic and die
		recover()
	}()
	vbg.b.doBulkGet(vbg.k, vbg.keys, vbg.reqDeadline, vbg.ch, vbg.ech, vbg.subPaths, vbg.useReplica, vbg.groupError, vbg.context...)
}

var _ERR_CHAN_FULL = fmt.Errorf("Data request queue full, aborting query.")

func (b *Bucket) processBulkGet(kdm map[uint16][]string, reqDeadline time.Time,
	ch chan<- map[string]*gomemcached.MCResponse, ech chan<- error, subPaths []string,
	useReplica bool, eStatus *errorStatus, context ...*memcached.ClientContext) {

	defer close(ch)
	defer close(ech)

	wg := &sync.WaitGroup{}

	for k, keys := range kdm {

		// GetBulk() group has error donot Queue items for this group
		if eStatus.errStatus {
			break
		}

		vbg := &vbBulkGet{
			b:           b,
			ch:          ch,
			ech:         ech,
			k:           k,
			keys:        keys,
			reqDeadline: reqDeadline,
			wg:          wg,
			subPaths:    subPaths,
			useReplica:  useReplica,
			groupError:  eStatus,
			context:     context,
		}

		wg.Add(1)

		// Random int
		// Right shift to avoid 8-byte alignment, and take low bits
		c := (uintptr(unsafe.Pointer(vbg)) >> 4) % _NUM_CHANNELS

		select {
		case _VB_BULK_GET_CHANNELS[c] <- vbg:
			// No-op
		default:
			// Buffer full, abandon the bulk get
			ech <- _ERR_CHAN_FULL
			wg.Add(-1)
		}
	}

	// Wait for my vb bulk gets
	wg.Wait()
}

type multiError []error

func (m multiError) Error() string {
	if len(m) == 0 {
		panic("Error of none")
	}

	return fmt.Sprintf("{%v errors, starting with %v}", len(m), m[0].Error())
}

// Convert a stream of errors from ech into a multiError (or nil) and
// send down eout.
//
// At least one send is guaranteed on eout, but two is possible, so
// buffer the out channel appropriately.
func errorCollector(ech <-chan error, eout chan<- error, eStatus *errorStatus) {
	defer func() { eout <- nil }()
	var errs multiError
	for e := range ech {
		if !eStatus.errStatus && !IsKeyNoEntError(e) {
			eStatus.errStatus = true
		}

		errs = append(errs, e)
	}

	if len(errs) > 0 {
		eout <- errs
	}
}

// GetBulk fetches multiple keys concurrently.
//
// Unlike more convenient GETs, the entire response is returned in the
// map array for each key.  Keys that were not found will not be included in
// the map.

func (b *Bucket) GetBulk(keys []string, reqDeadline time.Time, subPaths []string, useReplica bool, context ...*memcached.ClientContext) (map[string]*gomemcached.MCResponse, error) {
	return b.getBulk(keys, reqDeadline, subPaths, useReplica, context...)
}

func (b *Bucket) ReleaseGetBulkPools(rv map[string]*gomemcached.MCResponse) {
	_STRING_MCRESPONSE_POOL.Put(rv)
}

func (b *Bucket) getBulk(keys []string, reqDeadline time.Time, subPaths []string, useReplica bool, context ...*memcached.ClientContext) (map[string]*gomemcached.MCResponse, error) {
	kdm := _VB_STRING_POOL.Get()
	defer _VB_STRING_POOL.Put(kdm)
	for _, k := range keys {
		if k != "" {
			vb := uint16(b.VBHash(k))
			a, ok1 := kdm[vb]
			if !ok1 {
				a = _STRING_POOL.Get()
			}
			kdm[vb] = append(a, k)
		}
	}

	eout := make(chan error, 2)
	groupErrorStatus := &errorStatus{}

	// processBulkGet will own both of these channels and
	// guarantee they're closed before it returns.
	ch := make(chan map[string]*gomemcached.MCResponse)
	ech := make(chan error)

	go errorCollector(ech, eout, groupErrorStatus)
	go b.processBulkGet(kdm, reqDeadline, ch, ech, subPaths, useReplica, groupErrorStatus, context...)

	var rv map[string]*gomemcached.MCResponse

	for m := range ch {
		if rv == nil {
			rv = m
			continue
		}

		for k, v := range m {
			rv[k] = v
		}
		_STRING_MCRESPONSE_POOL.Put(m)
	}

	return rv, <-eout
}

// WriteOptions is the set of option flags availble for the Write
// method.  They are ORed together to specify the desired request.
type WriteOptions int

const (
	// Raw specifies that the value is raw []byte or nil; don't
	// JSON-encode it.
	Raw = WriteOptions(1 << iota)
	// AddOnly indicates an item should only be written if it
	// doesn't exist, otherwise ErrKeyExists is returned.
	AddOnly
	// Persist causes the operation to block until the server
	// confirms the item is persisted.
	Persist
	// Indexable causes the operation to block until it's availble via the index.
	Indexable
	// Append indicates the given value should be appended to the
	// existing value for the given key.
	Append
)

var optNames = []struct {
	opt  WriteOptions
	name string
}{
	{Raw, "raw"},
	{AddOnly, "addonly"}, {Persist, "persist"},
	{Indexable, "indexable"}, {Append, "append"},
}

// String representation of WriteOptions
func (w WriteOptions) String() string {
	f := []string{}
	for _, on := range optNames {
		if w&on.opt != 0 {
			f = append(f, on.name)
			w &= ^on.opt
		}
	}
	if len(f) == 0 || w != 0 {
		f = append(f, fmt.Sprintf("0x%x", int(w)))
	}
	return strings.Join(f, "|")
}

// Error returned from Write with AddOnly flag, when key already exists in the bucket.
var ErrKeyExists = errors.New("key exists")

// General-purpose value setter.
//
// The Set, Add and Delete methods are just wrappers around this.  The
// interpretation of `v` depends on whether the `Raw` option is
// given. If it is, v must be a byte array or nil. (A nil value causes
// a delete.) If `Raw` is not given, `v` will be marshaled as JSON
// before being written. It must be JSON-marshalable and it must not
// be nil.
func (b *Bucket) Write(k string, flags, exp int, v interface{},
	opt WriteOptions, context ...*memcached.ClientContext) (err error) {

	_, err = b.WriteWithCAS(k, flags, exp, v, opt, context...)

	return err
}

func (b *Bucket) WriteWithCAS(k string, flags, exp int, v interface{},
	opt WriteOptions, context ...*memcached.ClientContext) (cas uint64, err error) {

	var data []byte
	if opt&Raw == 0 {
		data, err = json.Marshal(v)
		if err != nil {
			return cas, err
		}
	} else if v != nil {
		data = v.([]byte)
	}

	var res *gomemcached.MCResponse
	err = b.Do(k, func(mc *memcached.Client, vb uint16) error {
		if opt&AddOnly != 0 {
			res, err = memcached.UnwrapMemcachedError(
				mc.Add(vb, k, flags, exp, data, context...))
			if err == nil && res.Status != gomemcached.SUCCESS {
				if res.Status == gomemcached.KEY_EEXISTS {
					err = ErrKeyExists
				} else {
					err = res
				}
			}
		} else if opt&Append != 0 {
			res, err = mc.Append(vb, k, data, context...)
		} else if data == nil {
			res, err = mc.Del(vb, k, context...)
		} else {
			res, err = mc.Set(vb, k, flags, exp, data, context...)
		}

		if err == nil {
			cas = res.Cas
		}

		return err
	})

	if err == nil && (opt&(Persist|Indexable) != 0) {
		err = b.WaitForPersistence(k, cas, data == nil)
	}

	return cas, err
}

// Extended CAS operation. These functions will return the mutation token, i.e vbuuid & guard
func (b *Bucket) CasWithMeta(k string, flags int, exp int, cas uint64, v interface{}, context ...*memcached.ClientContext) (uint64, *MutationToken, error) {
	return b.WriteCasWithMT(k, flags, exp, cas, v, 0, context...)
}

func (b *Bucket) WriteCasWithMT(k string, flags, exp int, cas uint64, v interface{},
	opt WriteOptions, context ...*memcached.ClientContext) (newCas uint64, mt *MutationToken, err error) {

	var data []byte
	if opt&Raw == 0 {
		data, err = json.Marshal(v)
		if err != nil {
			return 0, nil, err
		}
	} else if v != nil {
		data = v.([]byte)
	}

	var res *gomemcached.MCResponse
	err = b.Do(k, func(mc *memcached.Client, vb uint16) error {
		res, err = mc.SetCas(vb, k, flags, exp, cas, data, context...)
		return err
	})

	if err != nil {
		return 0, nil, err
	}

	// check for extras
	if len(res.Extras) >= 16 {
		vbuuid := uint64(binary.BigEndian.Uint64(res.Extras[0:8]))
		seqNo := uint64(binary.BigEndian.Uint64(res.Extras[8:16]))
		vb := b.VBHash(k)
		mt = &MutationToken{VBid: uint16(vb), Guard: vbuuid, Value: seqNo}
	}

	if err == nil && (opt&(Persist|Indexable) != 0) {
		err = b.WaitForPersistence(k, res.Cas, data == nil)
	}

	return res.Cas, mt, err
}

// Set a value in this bucket.
func (b *Bucket) SetWithCAS(k string, exp int, v interface{}, context ...*memcached.ClientContext) (uint64, error) {
	return b.WriteWithCAS(k, 0, exp, v, 0, context...)
}

// Add adds a value to this bucket; like Set except that nothing
// happens if the key exists. Return the CAS value.
func (b *Bucket) AddWithCAS(k string, exp int, v interface{}, context ...*memcached.ClientContext) (bool, uint64, error) {
	cas, err := b.WriteWithCAS(k, 0, exp, v, AddOnly, context...)
	if err == ErrKeyExists {
		return false, 0, nil
	}
	return (err == nil), cas, err
}

// Returns collectionUid, manifestUid, error.
func (b *Bucket) GetCollectionCID(scope string, collection string, reqDeadline time.Time) (uint32, uint32, error) {
	var err error
	var response *gomemcached.MCResponse

	var key = "DUMMY" // Contact any server.
	var manifestUid uint32
	var collUid uint32
	err = b.Do2(key, func(mc *memcached.Client, vb uint16) error {
		var err1 error

		mc.SetDeadline(getDeadline(reqDeadline, DefaultTimeout))
		_, err1 = mc.SelectBucket(b.Name)
		if err1 != nil {
			return err1
		}

		response, err1 = mc.CollectionsGetCID(scope, collection)
		if err1 != nil {
			return err1
		}

		manifestUid = binary.BigEndian.Uint32(response.Extras[4:8])
		collUid = binary.BigEndian.Uint32(response.Extras[8:12])

		return nil
	}, false, false, b.backOffRetries())

	return collUid, manifestUid, err
}

// Get a value straight from Memcached
func (b *Bucket) GetsMC(key string, reqDeadline time.Time, useReplica bool, context ...*memcached.ClientContext) (*gomemcached.MCResponse, error) {
	var err error
	var response *gomemcached.MCResponse

	if key == "" {
		return nil, nil
	}

	err = b.Do2(key, func(mc *memcached.Client, vb uint16) error {
		var err1 error

		mc.SetDeadline(getDeadline(reqDeadline, DefaultTimeout))
		response, err1 = mc.Get(vb, key, context...)
		if err1 != nil {
			return err1
		}
		return nil
	}, false, useReplica, b.backOffRetries())
	return response, err
}

// Get a value through the subdoc API
func (b *Bucket) GetsSubDoc(key string, reqDeadline time.Time, subPaths []string, context ...*memcached.ClientContext) (*gomemcached.MCResponse, error) {
	var err error
	var response *gomemcached.MCResponse

	if key == "" {
		return nil, nil
	}

	err = b.Do2(key, func(mc *memcached.Client, vb uint16) error {
		var err1 error

		mc.SetDeadline(getDeadline(reqDeadline, DefaultTimeout))
		response, err1 = mc.GetSubdoc(vb, key, subPaths, context...)
		if err1 != nil {
			return err1
		}
		return nil
	}, false, false, b.backOffRetries())
	return response, err
}

// To get random documents, we need to cover all the nodes, so select a connection at random.
func (b *Bucket) getRandomConnection() (*memcached.Client, *connectionPool, error) {
	for {
		var currentPool = 0
		pools := b.getConnPools(false /* not already locked */)
		if len(pools) == 0 {
			return nil, nil, fmt.Errorf("No connection pool found")
		} else if len(pools) > 1 { // choose a random connection
			currentPool = rand.Intn(len(pools))
		} // if only one pool, currentPool defaults to 0, i.e., the only pool

		// get the pool
		pool := pools[currentPool]
		conn, err := pool.Get()
		if err != errClosedPool {
			return conn, pool, err
		}

		// If conn pool was closed, because another goroutine refreshed the vbucket map, retry...
	}
}

// Get a random document from a bucket. Since the bucket may be distributed
// across nodes, we must first select a random connection, and then use the
// Client.GetRandomDoc() call to get a random document from that node.
func (b *Bucket) GetRandomDoc(context ...*memcached.ClientContext) (*gomemcached.MCResponse, error) {

	// get a connection from the pool
	conn, pool, err := b.getRandomConnection()
	if err != nil {
		return nil, err
	}
	conn.SetDeadline(getDeadline(time.Time{}, DefaultTimeout))

	// We may need to select the bucket before GetRandomDoc()
	// will work. This is sometimes done at startup (see defaultMkConn())
	// but not always, depending on the auth type.
	if conn.LastBucket() != b.Name {
		_, err = conn.SelectBucket(b.Name)
		if err != nil {
			return nil, err
		}
	}

	// get a randomm document from the connection
	doc, err := conn.GetRandomDoc(context...)

	// need to return the connection to the pool
	pool.Return(conn)
	return doc, err
}

// Delete a key from this bucket.
func (b *Bucket) Delete(k string, context ...*memcached.ClientContext) error {
	return b.Write(k, 0, 0, nil, Raw, context...)
}

// Incr increments the value at a given key by amt and defaults to def if no value present.
func (b *Bucket) Incr(k string, amt, def uint64, exp int, context ...*memcached.ClientContext) (val uint64, err error) {
	var rv uint64
	err = b.Do(k, func(mc *memcached.Client, vb uint16) error {
		res, err := mc.Incr(vb, k, amt, def, exp, context...)
		if err != nil {
			return err
		}
		rv = res
		return nil
	})
	return rv, err
}

// Decr decrements the value at a given key by amt and defaults to def if no value present
func (b *Bucket) Decr(k string, amt, def uint64, exp int, context ...*memcached.ClientContext) (val uint64, err error) {
	var rv uint64
	err = b.Do(k, func(mc *memcached.Client, vb uint16) error {
		res, err := mc.Decr(vb, k, amt, def, exp, context...)
		if err != nil {
			return err
		}
		rv = res
		return nil
	})
	return rv, err
}

// Wrapper around memcached.CASNext()
func (b *Bucket) casNext(k string, exp int, state *memcached.CASState) bool {
	keepGoing := false
	state.Err = b.Do(k, func(mc *memcached.Client, vb uint16) error {
		keepGoing = mc.CASNext(vb, k, exp, state)
		return state.Err
	})
	return keepGoing && state.Err == nil
}

// An UpdateFunc is a callback function to update a document
type UpdateFunc func(current []byte) (updated []byte, err error)

// Return this as the error from an UpdateFunc to cancel the Update
// operation.
const UpdateCancel = memcached.CASQuit

// Update performs a Safe update of a document, avoiding conflicts by
// using CAS.
//
// The callback function will be invoked with the current raw document
// contents (or nil if the document doesn't exist); it should return
// the updated raw contents (or nil to delete.)  If it decides not to
// change anything it can return UpdateCancel as the error.
//
// If another writer modifies the document between the get and the
// set, the callback will be invoked again with the newer value.
func (b *Bucket) Update(k string, exp int, callback UpdateFunc) error {
	_, err := b.update(k, exp, callback)
	return err
}

// internal version of Update that returns a CAS value
func (b *Bucket) update(k string, exp int, callback UpdateFunc) (newCas uint64, err error) {
	var state memcached.CASState
	for b.casNext(k, exp, &state) {
		var err error
		if state.Value, err = callback(state.Value); err != nil {
			return 0, err
		}
	}
	return state.Cas, state.Err
}

// Observe observes the current state of a document.
func (b *Bucket) Observe(k string) (result memcached.ObserveResult, err error) {
	err = b.Do(k, func(mc *memcached.Client, vb uint16) error {
		result, err = mc.Observe(vb, k)
		return err
	})
	return
}

// Returned from WaitForPersistence (or Write, if the Persistent or Indexable flag is used)
// if the value has been overwritten by another before being persisted.
var ErrOverwritten = errors.New("overwritten")

// Returned from WaitForPersistence (or Write, if the Persistent or Indexable flag is used)
// if the value hasn't been persisted by the timeout interval
var ErrTimeout = errors.New("timeout")

// WaitForPersistence waits for an item to be considered durable.
//
// Besides transport errors, ErrOverwritten may be returned if the
// item is overwritten before it reaches durability.  ErrTimeout may
// occur if the item isn't found durable in a reasonable amount of
// time.
func (b *Bucket) WaitForPersistence(k string, cas uint64, deletion bool) error {
	timeout := 10 * time.Second
	sleepDelay := 5 * time.Millisecond
	start := time.Now()
	for {
		time.Sleep(sleepDelay)
		sleepDelay += sleepDelay / 2 // multiply delay by 1.5 every time

		result, err := b.Observe(k)
		if err != nil {
			return err
		}
		if persisted, overwritten := result.CheckPersistence(cas, deletion); overwritten {
			return ErrOverwritten
		} else if persisted {
			return nil
		}

		if result.PersistenceTime > 0 {
			timeout = 2 * result.PersistenceTime
		}
		if time.Since(start) >= timeout-sleepDelay {
			return ErrTimeout
		}
	}
}

var _STRING_MCRESPONSE_POOL = gomemcached.NewStringMCResponsePool(16)
var _STRING_POOL = util.NewStringPool(16)

type vbStringPool struct {
	pool    util.FastPool
	strPool *util.StringPool
}

func newVBStringPool(size int, sp *util.StringPool) *vbStringPool {
	rv := &vbStringPool{
		strPool: sp,
	}
	util.NewFastPool(&rv.pool, func() interface{} {
		return make(map[uint16][]string, size)
	})
	return rv
}

func (this *vbStringPool) Get() map[uint16][]string {
	return this.pool.Get().(map[uint16][]string)
}

func (this *vbStringPool) Put(s map[uint16][]string) {
	if s == nil {
		return
	}

	for k, v := range s {
		delete(s, k)
		this.strPool.Put(v)
	}
	this.pool.Put(s)
}

var _VB_STRING_POOL = newVBStringPool(16, _STRING_POOL)
