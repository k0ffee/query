//  Copyright 2014-Present Couchbase, Inc.
//
//  Use of this software is governed by the Business Source License included
//  in the file licenses/BSL-Couchbase.txt.  As of the Change Date specified
//  in that file, in accordance with the Business Source License, use of this
//  software will be governed by the Apache License, Version 2.0, included in
//  the file licenses/APL2.txt.

package server

import (
	"net/http"
	"sync"
	"time"

	atomic "github.com/couchbase/go-couchbase/platform"
	"github.com/couchbase/query/auth"
	"github.com/couchbase/query/datastore"
	"github.com/couchbase/query/errors"
	"github.com/couchbase/query/execution"
	"github.com/couchbase/query/plan"
	"github.com/couchbase/query/timestamp"
	"github.com/couchbase/query/util"
	"github.com/couchbase/query/value"
)

type State int32

const (
	SUBMITTED State = iota
	RUNNING
	SUCCESS
	ERRORS
	COMPLETED
	STOPPED
	TIMEOUT
	CLOSED
	FATAL
	ABORTED
)

var states = [...]string{
	"submitted",
	"running",
	"success",
	"errors",
	"completed",
	"stopped",
	"timeout",
	"closed",
	"fatal",
	"aborted",
}

type Request interface {
	Id() RequestID
	ClientID() ClientContextID
	SetClientID(id string)
	Statement() string
	SetStatement(statement string)
	Prepared() *plan.Prepared
	SetPrepared(prepared *plan.Prepared)
	Type() string
	SetType(string)
	IsPrepare() bool
	SetIsPrepare(bool)
	NamedArgs() map[string]value.Value
	SetNamedArgs(args map[string]value.Value)
	PositionalArgs() value.Values
	SetPositionalArgs(args value.Values)
	Namespace() string
	SetNamespace(namespace string)
	Timeout() time.Duration
	SetTimeout(timeout time.Duration)
	SetTimer(*time.Timer)
	MaxParallelism() int
	SetMaxParallelism(maxParallelism int)
	ScanCap() int64
	SetScanCap(scanCap int64)
	PipelineCap() int64
	SetPipelineCap(pipelineCap int64)
	PipelineBatch() int
	SetPipelineBatch(pipelineBatch int)
	Readonly() value.Tristate
	SetReadonly(readonly value.Tristate)
	Metrics() value.Tristate
	SetMetrics(metrics value.Tristate)
	Signature() value.Tristate
	SetSignature(signature value.Tristate)
	Pretty() value.Tristate
	SetPretty(pretty value.Tristate)
	Controls() value.Tristate
	SetControls(controls value.Tristate)
	Profile() Profile
	SetProfile(p Profile)
	ScanConsistency() datastore.ScanConsistency
	SetScanConfiguration(consistency ScanConfiguration)
	OriginalScanConsistency() datastore.ScanConsistency
	SetScanConsistency(consistency datastore.ScanConsistency)
	ScanVectorSource() timestamp.ScanVectorSource
	IndexApiVersion() int
	SetIndexApiVersion(ver int)
	FeatureControls() uint64
	SetFeatureControls(controls uint64)
	AutoPrepare() value.Tristate
	SetAutoPrepare(a value.Tristate)
	AutoExecute() value.Tristate
	SetAutoExecute(a value.Tristate)
	SetQueryContext(s string)
	QueryContext() string
	UseFts() bool
	SetUseFts(a bool)
	UseCBO() bool
	SetUseCBO(useCBO bool)
	UseReplica() bool
	SetUseReplica(useReplica bool)
	MemoryQuota() uint64
	SetMemoryQuota(q uint64)
	UsedMemory() uint64
	TxId() string
	SetTxId(s string)
	TxImplicit() bool
	SetTxImplicit(b bool)
	TxStmtNum() int64
	SetTxStmtNum(n int64)
	TxTimeout() time.Duration
	SetTxTimeout(d time.Duration)
	TxData() []byte
	SetTxData(b []byte)
	DurabilityLevel() datastore.DurabilityLevel
	SetDurabilityLevel(l datastore.DurabilityLevel)
	DurabilityTimeout() time.Duration
	SetDurabilityTimeout(d time.Duration)
	KvTimeout() time.Duration
	SetKvTimeout(d time.Duration)
	AtrCollection() string
	SetAtrCollection(s string)
	NumAtrs() int
	SetNumAtrs(n int)
	PreserveExpiry() bool
	SetPreserveExpiry(a bool)
	ExecutionContext() *execution.Context
	SetExecutionContext(ctx *execution.Context)
	SetExecTime(time time.Time)
	RequestTime() time.Time
	ServiceTime() time.Time
	TransactionStartTime() time.Time
	SetTransactionStartTime(t time.Time)
	Output() execution.Output
	Servicing()
	Fail(err errors.Error)
	Error(err errors.Error)
	Execute(server *Server, context *execution.Context, reqType string, signature value.Value, startTx bool)
	NotifyStop(stop execution.Operator)
	Failed(server *Server)
	Expire(state State, timeout time.Duration)
	SortCount() uint64
	State() State
	Halted() bool
	Credentials() *auth.Credentials
	SetCredentials(credentials *auth.Credentials)
	RemoteAddr() string
	SetRemoteAddr(remoteAddr string)
	UserAgent() string
	SetUserAgent(userAgent string)
	SetTimings(o execution.Operator)
	GetTimings() execution.Operator
	SetFmtTimings(e []byte)
	GetFmtTimings() []byte
	SetFmtOptimizerEstimates(t []byte)
	GetFmtOptimizerEstimates() []byte
	IsAdHoc() bool
	SetErrorLimit(limit int)
	GetErrorLimit() int

	setSleep() // internal methods for load control
	sleep()
	release()
}

type RequestID interface {
	String() string
}

type ClientContextID interface {
	IsValid() bool
	String() string
}

type ScanConsistency int

const (
	NOT_SET ScanConsistency = iota
	NOT_BOUNDED
	REQUEST_PLUS
	STATEMENT_PLUS
	AT_PLUS
	UNDEFINED_CONSISTENCY
)

type ScanConfiguration interface {
	ScanConsistency() datastore.ScanConsistency
	ScanWait() time.Duration
	ScanVectorSource() timestamp.ScanVectorSource
	SetScanConsistency(consistency datastore.ScanConsistency) interface{}
}

// API for tracking active requests
type ActiveRequests interface {

	// adds a request to the active queue
	Put(Request) errors.Error

	// processes a request
	Get(string, func(Request)) errors.Error

	// removes a request from the active queue / returns success
	Delete(string, bool) bool

	// request count
	Count() (int, errors.Error)

	// processes all requests
	// first function processes within lock (must be non blocking)
	// second function processes outside of a lock (can be blocking)
	// both return false if no more processing should be done
	ForEach(func(string, Request) bool, func() bool)

	// current active request server load
	Load() int
}

var actives ActiveRequests

func ActiveRequestsCount() (int, errors.Error) {
	return actives.Count()
}

func ActiveRequestsDelete(id string) bool {
	return actives.Delete(id, true)
}

func ActiveRequestsGet(id string, f func(Request)) errors.Error {
	return actives.Get(id, f)
}

func ActiveRequestsForEach(nonBlocking func(string, Request) bool, blocking func() bool) {
	actives.ForEach(nonBlocking, blocking)
}

func ActiveRequestsLoad() int {
	return actives.Load()
}

func SetActives(ar ActiveRequests) {
	actives = ar
}

type BaseRequest struct {
	// Aligned ints need to be declared right at the top
	// of the struct to avoid alignment issues on x86 platforms
	inUseMemory   atomic.AlignedUint64
	usedMemory    atomic.AlignedUint64
	mutationCount atomic.AlignedUint64
	sortCount     atomic.AlignedUint64
	phaseStats    [execution.PHASES]phaseStat

	sync.RWMutex
	id                   requestIDImpl
	client_id            clientContextIDImpl
	statement            string
	prepared             *plan.Prepared
	reqType              string
	isPrepare            bool
	namedArgs            map[string]value.Value
	positionalArgs       value.Values
	namespace            string
	timeout              time.Duration
	timer                *time.Timer
	maxParallelism       int
	scanCap              int64
	pipelineCap          int64
	pipelineBatch        int
	readonly             value.Tristate
	signature            value.Tristate
	metrics              value.Tristate
	pretty               value.Tristate
	consistency          ScanConfiguration
	credentials          *auth.Credentials
	remoteAddr           string
	userAgent            string
	requestTime          time.Time
	serviceTime          time.Time
	execTime             time.Time
	transactionStartTime time.Time
	state                State
	aborted              bool
	errorLimit           int
	errorCount           int
	duplicateErrorCount  int
	warningCount         int
	errors               []errors.Error
	warnings             []errors.Error
	stopGate             sync.WaitGroup
	servicerGate         sync.WaitGroup
	stopResult           chan bool          // stop consuming results
	stopExecute          chan bool          // stop executing request
	stopOperator         execution.Operator // notified when request execution stops
	timings              execution.Operator
	fmtTimings           []byte
	fmtEstimates         []byte
	controls             value.Tristate
	profile              Profile
	indexApiVersion      int    // Index API version
	featureControls      uint64 // feature bit controls
	autoPrepare          value.Tristate
	autoExecute          value.Tristate
	useFts               bool
	useCBO               bool
	useReplica           bool
	queryContext         string
	memoryQuota          uint64
	txId                 string
	txImplicit           bool
	txStmtNum            int64
	txTimeout            time.Duration
	txData               []byte
	durabilityTimeout    time.Duration
	durabilityLevel      datastore.DurabilityLevel
	kvTimeout            time.Duration
	atrCollection        string
	numAtrs              int
	preserveExpiry       bool
	executionContext     *execution.Context
	resultCount          int64
	resultSize           int64
	serviceDuration      time.Duration
}

type requestIDImpl struct {
	id string
}

type phaseStat struct {
	count     atomic.AlignedUint64
	operators atomic.AlignedUint64
	duration  atomic.AlignedUint64
}

// requestIDImpl implements the RequestID interface
func (r *requestIDImpl) String() string {
	return r.id
}

type clientContextIDImpl struct {
	id string
}

func (this *clientContextIDImpl) IsValid() bool {
	return len(this.id) > 0
}

func (this *clientContextIDImpl) String() string {
	return this.id
}

func NewBaseRequest(rv *BaseRequest) {
	rv.timeout = -1
	rv.txTimeout = datastore.DEF_TXTIMEOUT
	rv.serviceTime = time.Now()
	rv.state = SUBMITTED
	rv.aborted = false
	rv.stopResult = make(chan bool, 1)
	rv.stopExecute = make(chan bool, 1)
	rv.metrics = value.NONE
	rv.pretty = value.NONE
	rv.readonly = value.NONE
	rv.signature = value.NONE
	rv.profile = ProfUnset
	rv.controls = value.NONE
	rv.autoPrepare = value.NONE
	rv.indexApiVersion = util.GetMaxIndexAPI()
	rv.featureControls = util.GetN1qlFeatureControl()
	rv.id.id, _ = util.UUIDV4()
	rv.client_id.id = ""
	rv.SetMaxParallelism(1)
	rv.useCBO = util.GetUseCBO()
	rv.useReplica = false
	rv.durabilityTimeout = datastore.DEF_DURABILITY_TIMEOUT
	rv.kvTimeout = datastore.DEF_KVTIMEOUT
	rv.durabilityLevel = datastore.DL_UNSET
	rv.errorLimit = -1
}

func (this *BaseRequest) SetRequestTime(time time.Time) {
	this.requestTime = time
}

func (this *BaseRequest) SetExecTime(time time.Time) {
	this.execTime = time
}

func (this *BaseRequest) SetTimer(timer *time.Timer) {
	this.timer = timer
}

func (this *BaseRequest) Id() RequestID {
	return &this.id
}

func (this *BaseRequest) ClientID() ClientContextID {
	return &this.client_id
}

func (this *BaseRequest) SetClientID(id string) {
	this.client_id.id = id
}

func (this *BaseRequest) Statement() string {
	return this.statement
}

func (this *BaseRequest) SetStatement(statement string) {
	this.statement = statement
}

func (this *BaseRequest) Prepared() *plan.Prepared {
	return this.prepared
}

func (this *BaseRequest) Type() string {
	return this.reqType
}

func (this *BaseRequest) IsPrepare() bool {
	return this.isPrepare
}

func (this *BaseRequest) NamedArgs() map[string]value.Value {
	return this.namedArgs
}

func (this *BaseRequest) SetNamedArgs(args map[string]value.Value) {
	this.namedArgs = args
}

func (this *BaseRequest) PositionalArgs() value.Values {
	return this.positionalArgs
}

func (this *BaseRequest) SetPositionalArgs(args value.Values) {
	this.positionalArgs = args
}

func (this *BaseRequest) Namespace() string {
	return this.namespace
}

func (this *BaseRequest) SetNamespace(namespace string) {
	this.namespace = namespace
}

func (this *BaseRequest) Timeout() time.Duration {
	return this.timeout
}

func (this *BaseRequest) SetTimeout(timeout time.Duration) {
	this.timeout = timeout
}

func (this *BaseRequest) MaxParallelism() int {
	return this.maxParallelism
}

func (this *BaseRequest) SetMaxParallelism(maxParallelism int) {
	if maxParallelism <= 0 {
		maxParallelism = util.NumCPU()
	}
	this.maxParallelism = maxParallelism
}

func (this *BaseRequest) ScanCap() int64 {
	return this.scanCap
}

func (this *BaseRequest) SetScanCap(scanCap int64) {
	this.scanCap = scanCap
}

func (this *BaseRequest) PipelineCap() int64 {
	return this.pipelineCap
}

func (this *BaseRequest) SetPipelineCap(pipelineCap int64) {
	this.pipelineCap = pipelineCap
}

func (this *BaseRequest) PipelineBatch() int {
	return this.pipelineBatch
}

func (this *BaseRequest) SetPipelineBatch(pipelineBatch int) {
	this.pipelineBatch = pipelineBatch
}

func (this *BaseRequest) Readonly() value.Tristate {
	return this.readonly
}

func (this *BaseRequest) SetReadonly(readonly value.Tristate) {
	this.readonly = readonly
}

func (this *BaseRequest) Signature() value.Tristate {
	return this.signature
}

func (this *BaseRequest) SetSignature(signature value.Tristate) {
	this.signature = signature
}

func (this *BaseRequest) Metrics() value.Tristate {
	return this.metrics
}

func (this *BaseRequest) SetMetrics(metrics value.Tristate) {
	this.metrics = metrics
}

func (this *BaseRequest) Pretty() value.Tristate {
	return this.pretty
}

func (this *BaseRequest) SetPretty(pretty value.Tristate) {
	this.pretty = pretty
}

func (this *BaseRequest) OriginalScanConsistency() datastore.ScanConsistency {
	if this.consistency == nil {
		return datastore.NOT_SET
	}
	return this.consistency.ScanConsistency()
}

func (this *BaseRequest) SetScanConsistency(consistency datastore.ScanConsistency) {
	this.consistency = this.consistency.SetScanConsistency(consistency).(ScanConfiguration)
}

func (this *BaseRequest) ScanConsistency() datastore.ScanConsistency {
	consistency := this.OriginalScanConsistency()
	if consistency == datastore.NOT_SET {
		consistency = datastore.UNBOUNDED
	}
	return consistency
}

func (this *BaseRequest) SetScanConfiguration(consistency ScanConfiguration) {
	this.consistency = consistency
}

func (this *BaseRequest) ScanVectorSource() timestamp.ScanVectorSource {
	if this.consistency == nil {
		return nil
	}
	return this.consistency.ScanVectorSource()
}

func (this *BaseRequest) RequestTime() time.Time {
	return this.requestTime
}

func (this *BaseRequest) ServiceTime() time.Time {
	return this.serviceTime
}

func (this *BaseRequest) ExecTime() time.Time {
	return this.execTime
}

func (this *BaseRequest) TransactionStartTime() time.Time {
	return this.transactionStartTime
}

func (this *BaseRequest) SetTransactionStartTime(t time.Time) {
	this.transactionStartTime = t
}

func (this *BaseRequest) SetPrepared(prepared *plan.Prepared) {
	this.Lock()
	defer this.Unlock()
	this.prepared = prepared
}

func (this *BaseRequest) SetType(reqType string) {
	this.Lock()
	defer this.Unlock()
	this.reqType = reqType
}

func (this *BaseRequest) SetIsPrepare(ip bool) {
	this.Lock()
	defer this.Unlock()
	this.isPrepare = ip
}

func (this *BaseRequest) SetState(state State) {
	this.Lock()
	defer this.Unlock()

	// Once we transition to TIMEOUT or CLOSE, we don't transition
	// to STOPPED or COMPLETED to allow the request to close
	// gracefully on timeout or network errors and report the
	// right state. Ditto for FATAL.
	if this.state == FATAL ||
		((this.state == TIMEOUT || this.state == CLOSED || this.state == STOPPED) &&
			(state == STOPPED || state == COMPLETED)) {
		return
	}
	this.state = state
}

func (this *BaseRequest) State() State {
	this.RLock()
	defer this.RUnlock()
	if this.aborted {
		return ABORTED
	}
	return this.state
}

func (this State) StateName() string {
	return states[int(this)]
}

func (this *BaseRequest) Halted() bool {

	// we purposly do not take the lock
	// as this is used repeatedly in Execution()
	// if we mistakenly report the State as RUNNING,
	// we'll catch the right state in other places...
	state := State(atomic.LoadInt32((*int32)(&this.state)))
	return state != RUNNING && state != SUBMITTED
}

func (this *BaseRequest) Credentials() *auth.Credentials {
	return this.credentials
}

func (this *BaseRequest) SetCredentials(credentials *auth.Credentials) {
	this.credentials = credentials
}

func (this *BaseRequest) RemoteAddr() string {
	return this.remoteAddr
}

func (this *BaseRequest) SetRemoteAddr(remoteAddr string) {
	this.remoteAddr = remoteAddr
}

func (this *BaseRequest) UserAgent() string {
	return this.userAgent
}

func (this *BaseRequest) SetUserAgent(userAgent string) {
	this.userAgent = userAgent
}

func (this *BaseRequest) Servicing() {
	this.serviceTime = time.Now()
	this.state = RUNNING
}

func (this *BaseRequest) Fatal(err errors.Error) {
	this.Error(err)
	this.Stop(FATAL)
}

func (this *BaseRequest) Abort(err errors.Error) {
	this.aborted = true
	this.Error(err)
	this.Stop(FATAL)
}

func (this *BaseRequest) SetErrorLimit(limit int) {
	if limit < 0 {
		limit = 0
	}
	this.errorLimit = limit
}

func (this *BaseRequest) GetErrorLimit() int {
	return this.errorLimit
}

func (this *BaseRequest) GetErrorCount() int {
	return this.errorCount
}

func (this *BaseRequest) GetWarningCount() int {
	return this.warningCount
}

func (this *BaseRequest) Error(err errors.Error) {
	this.Lock()
	if this.errorLimit > 0 && this.errorCount+this.duplicateErrorCount >= this.errorLimit {
		this.errors = append(this.errors, errors.NewErrorLimit(this.errorLimit, this.errorCount, this.duplicateErrorCount,
			this.MutationCount()))
		this.errorCount++
		this.aborted = true
		this.Unlock()
		this.Stop(FATAL)
		return
	}
	defer this.Unlock()
	// don't add duplicate errors
	for _, e := range this.errors {
		if err.Code() != 0 && err.Code() == e.Code() && err.Error() == e.Error() {
			this.duplicateErrorCount++
			return
		}
	}
	this.errors = append(this.errors, err)
	this.errorCount++
}

func (this *BaseRequest) Warning(wrn errors.Error) {
	this.Lock()
	defer this.Unlock()
	// de-duplicate warnings
	if wrn.OnceOnly() {
		for _, w := range this.warnings {
			if wrn.Code() == w.Code() && wrn.Error() == w.Error() {
				return
			}
		}
	}
	this.warnings = append(this.warnings, wrn)
	this.warningCount++
}

func (this *BaseRequest) AddMutationCount(i uint64) {
	atomic.AddUint64(&this.mutationCount, i)
}

func (this *BaseRequest) MutationCount() uint64 {
	return atomic.LoadUint64(&this.mutationCount)
}

func (this *BaseRequest) SetSortCount(i uint64) {
	atomic.StoreUint64(&this.sortCount, i)
}

func (this *BaseRequest) SortCount() uint64 {
	return atomic.LoadUint64(&this.sortCount)
}

func (this *BaseRequest) AddPhaseCount(p execution.Phases, c uint64) {
	atomic.AddUint64(&this.phaseStats[p].count, c)
}

func (this *BaseRequest) AddPhaseOperator(p execution.Phases) {
	atomic.AddUint64(&this.phaseStats[p].operators, 1)
}

func (this *BaseRequest) PhaseOperator(p execution.Phases) uint64 {
	return uint64(this.phaseStats[p].operators)
}

func (this *BaseRequest) FmtPhaseCounts() map[string]interface{} {
	var p map[string]interface{} = nil

	// Use simple iteration rather than a range clause to avoid a spurious
	// data race report. MB-20692
	nr := len(this.phaseStats)
	for i := 0; i < nr; i++ {
		count := atomic.LoadUint64(&this.phaseStats[i].count)
		if count > 0 {
			if p == nil {
				p = make(map[string]interface{},
					execution.PHASES)
			}
			p[execution.Phases(i).String()] = count
		}
	}
	return p
}

func (this *BaseRequest) FmtPhaseOperators() map[string]interface{} {
	var p map[string]interface{} = nil

	// Use simple iteration rather than a range clause to avoid a spurious
	// data race report. MB-20692
	nr := len(this.phaseStats)
	for i := 0; i < nr; i++ {
		operators := atomic.LoadUint64(&this.phaseStats[i].operators)
		if operators > 0 {
			if p == nil {
				p = make(map[string]interface{},
					execution.PHASES)
			}
			p[execution.Phases(i).String()] = operators
		}
	}
	return p
}

func (this *BaseRequest) AddPhaseTime(phase execution.Phases, duration time.Duration) {
	atomic.AddUint64(&(this.phaseStats[phase].duration), uint64(duration))
}

func (this *BaseRequest) FmtPhaseTimes() map[string]interface{} {
	var p map[string]interface{} = nil

	nr := len(this.phaseStats)
	for i := 0; i < nr; i++ {
		duration := atomic.LoadUint64(&this.phaseStats[i].duration)
		if duration > 0 {
			if p == nil {
				p = make(map[string]interface{},
					execution.PHASES)
			}
			p[execution.Phases(i).String()] = time.Duration(duration).String()
		}
	}
	return p
}

func (this *BaseRequest) FmtOptimizerEstimates(op execution.Operator) map[string]interface{} {
	var p map[string]interface{} = nil

	if op != nil {
		planOp := op.PlanOp()
		if planOp != nil && planOp.Cost() > 0.0 && planOp.Cardinality() > 0.0 {
			p = make(map[string]interface{}, 2)
			p["cost"] = planOp.Cost()
			p["cardinality"] = planOp.Cardinality()
		}
	}

	return p
}

func (this *BaseRequest) TrackMemory(size uint64) {
	util.TestAndSetUint64(&this.usedMemory, size,
		func(old, new uint64) bool { return old < new }, 1)
}

func (this *BaseRequest) ReleaseValueSize(size uint64) {
	atomic.AddUint64(&this.inUseMemory, ^(size - 1))
}

func (this *BaseRequest) UsedMemory() uint64 {
	return uint64(this.usedMemory)
}

func (this *BaseRequest) SetTimings(o execution.Operator) {
	this.timings = o
}

func (this *BaseRequest) GetTimings() execution.Operator {
	return this.timings
}

func (this *BaseRequest) SetFmtTimings(t []byte) {
	this.fmtTimings = t
}

func (this *BaseRequest) GetFmtTimings() []byte {
	return this.fmtTimings
}

func (this *BaseRequest) SetFmtOptimizerEstimates(e []byte) {
	this.fmtEstimates = e
}

func (this *BaseRequest) GetFmtOptimizerEstimates() []byte {
	return this.fmtEstimates
}

func (this *BaseRequest) SetControls(c value.Tristate) {
	this.controls = c
}

func (this *BaseRequest) Controls() value.Tristate {
	return this.controls
}

func (this *BaseRequest) SetProfile(p Profile) {
	this.profile = p
}

func (this *BaseRequest) Profile() Profile {
	return this.profile
}

func (this *BaseRequest) SetIndexApiVersion(ver int) {
	// By default this.indexApiVersion is Server level. request level needs to be lessthan server level
	if ver < this.indexApiVersion {
		this.indexApiVersion = ver
	}
}

func (this *BaseRequest) IndexApiVersion() int {
	return this.indexApiVersion
}

func (this *BaseRequest) SetFeatureControls(controls uint64) {
	// By default this.featureControls is Server level. request level can only turn off server level
	this.featureControls = this.featureControls | controls
}

func (this *BaseRequest) FeatureControls() uint64 {
	return this.featureControls
}

func (this *BaseRequest) SetAutoPrepare(a value.Tristate) {
	this.autoPrepare = a
}

func (this *BaseRequest) AutoPrepare() value.Tristate {
	return this.autoPrepare
}

func (this *BaseRequest) SetAutoExecute(a value.Tristate) {
	this.autoExecute = a
}

func (this *BaseRequest) AutoExecute() value.Tristate {
	return this.autoExecute
}

func (this *BaseRequest) SetUseFts(a bool) {
	this.useFts = a
}

func (this *BaseRequest) UseFts() bool {
	return this.useFts && util.IsFeatureEnabled(this.featureControls, util.N1QL_FLEXINDEX)
}

func (this *BaseRequest) SetMemoryQuota(q uint64) {
	this.memoryQuota = q
}

func (this *BaseRequest) MemoryQuota() uint64 {
	return this.memoryQuota
}

func (this *BaseRequest) SetQueryContext(s string) {
	this.queryContext = s
}

func (this *BaseRequest) QueryContext() string {
	return this.queryContext
}

func (this *BaseRequest) UseCBO() bool {
	return this.useCBO && util.IsFeatureEnabled(this.featureControls, util.N1QL_CBO)
}

func (this *BaseRequest) SetUseCBO(useCBO bool) {
	// use-cbo can only be set at request level if it is not turned off in n1ql-feat-ctrl
	if util.IsFeatureEnabled(this.featureControls, util.N1QL_CBO) {
		this.useCBO = useCBO
	}
}

func (this *BaseRequest) UseReplica() bool {
	return this.useReplica
}

func (this *BaseRequest) SetUseReplica(useReplica bool) {
	this.useReplica = useReplica
}

func (this *BaseRequest) SetTxId(s string) {
	this.txId = s
}

func (this *BaseRequest) TxId() string {
	return this.txId
}

func (this *BaseRequest) SetTxImplicit(b bool) {
	this.txImplicit = b
}

func (this *BaseRequest) TxImplicit() bool {
	if this.txId == "" {
		return this.txImplicit
	}
	return false
}

func (this *BaseRequest) SetTxStmtNum(n int64) {
	this.txStmtNum = n
}

func (this *BaseRequest) TxStmtNum() int64 {
	return this.txStmtNum
}

func (this *BaseRequest) SetTxTimeout(d time.Duration) {
	if d > 0 {
		this.txTimeout = d
	}
}

func (this *BaseRequest) TxTimeout() time.Duration {
	return this.txTimeout
}

func (this *BaseRequest) SetTxData(b []byte) {
	this.txData = b
}

func (this *BaseRequest) TxData() []byte {
	return this.txData
}

func (this *BaseRequest) SetDurabilityLevel(l datastore.DurabilityLevel) {
	this.durabilityLevel = l
}

func (this *BaseRequest) DurabilityLevel() datastore.DurabilityLevel {
	return this.durabilityLevel
}

func (this *BaseRequest) SetDurabilityTimeout(d time.Duration) {
	this.durabilityTimeout = d
}

func (this *BaseRequest) DurabilityTimeout() time.Duration {
	return this.durabilityTimeout
}

func (this *BaseRequest) SetKvTimeout(d time.Duration) {
	this.kvTimeout = d
}

func (this *BaseRequest) KvTimeout() time.Duration {
	return this.kvTimeout
}

func (this *BaseRequest) SetAtrCollection(s string) {
	this.atrCollection = s
}

func (this *BaseRequest) AtrCollection() string {
	return this.atrCollection
}

func (this *BaseRequest) SetNumAtrs(n int) {
	this.numAtrs = n
}

func (this *BaseRequest) NumAtrs() int {
	return this.numAtrs
}

func (this *BaseRequest) SetPreserveExpiry(a bool) {
	this.preserveExpiry = a
}

func (this *BaseRequest) PreserveExpiry() bool {
	return this.preserveExpiry
}

func (this *BaseRequest) SetExecutionContext(ctx *execution.Context) {
	this.executionContext = ctx
}

func (this *BaseRequest) ExecutionContext() *execution.Context {
	return this.executionContext
}

func (this *BaseRequest) Results() chan bool {
	return this.stopResult
}

func (this *BaseRequest) CloseResults() {
	sendStop(this.stopResult)
}

func (this *BaseRequest) Errors() []errors.Error {
	return this.errors
}

func (this *BaseRequest) Warnings() []errors.Error {
	return this.warnings
}

func (this *BaseRequest) NotifyStop(o execution.Operator) {
	this.Lock()
	defer this.Unlock()
	this.stopOperator = o
}

func (this *BaseRequest) StopNotify() execution.Operator {
	this.RLock()
	defer this.RUnlock()
	return this.stopOperator
}

func (this *BaseRequest) StopExecute() chan bool {
	return this.stopExecute
}

func (this *BaseRequest) Stop(state State) {
	this.SetState(state)

	// guard against the root operator not being set (eg fatal error)
	if this.stopOperator != nil {

		// only one in between Stop() and Done() can happen at any one time
		this.stopGate.Wait()
		this.stopGate.Add(1)

		// make sure that a stop can only be sent once (eg close OR timeout)
		if this.stopOperator != nil {
			execution.OpStop(this.stopOperator)
		}
		this.stopGate.Done()
		this.stopOperator = nil
	}
	sendStop(this.stopExecute)
}

// load control gate
func (this *BaseRequest) setSleep() {
	this.servicerGate.Add(1)
}
func (this *BaseRequest) sleep() {
	this.servicerGate.Wait()
}

func (this *BaseRequest) release() {
	this.servicerGate.Done()
}

// this logs the request if needed and takes any other action required to
// put this request to rest
func (this *BaseRequest) CompleteRequest(requestTime, serviceTime, transaction_time time.Duration,
	resultCount int, resultSize int, errorCount int, req *http.Request, server *Server) {

	if this.timer != nil {
		this.timer.Stop()
		this.timer = nil
	}
	LogRequest(requestTime, serviceTime, transaction_time, resultCount,
		resultSize, errorCount, req, this, server)

	// Request Profiling - signal that request has completed and
	// resources can be pooled / released as necessary
	if this.timings != nil {

		// only one in between Stop() and Done() can happen at any one time
		this.stopGate.Wait()
		this.stopGate.Add(1)
		this.timings.Done()

		// sending a stop is illegal after this point
		this.stopOperator = nil
		this.stopGate.Done()
		this.timings = nil
	}
}

func sendStop(ch chan bool) {
	select {
	case ch <- false:
	default:
	}
}

// For audit.Auditable interface.
func (this *BaseRequest) EventStatement() string {
	prep := this.Prepared()
	if prep != nil {
		return prep.Text()
	}
	return this.Statement()
}

// For audit.Auditable interface.
func (this *BaseRequest) EventErrorMessage() []errors.Error {
	return this.errors
}

// For audit.Auditable interface.
func (this *BaseRequest) EventQueryContext() string {
	return this.QueryContext()
}

// For audit.Auditable interface.
func (this *BaseRequest) EventTxId() string {
	return this.TxId()
}

// For audit.Auditable interface.
func (this *BaseRequest) PreparedId() string {
	prep := this.Prepared()
	if prep != nil {
		return prep.Name()
	}
	return ""
}

// For audit.Auditable interface.
func (this *BaseRequest) EventId() string {
	return this.Id().String()
}

// For audit.Auditable interface.
func (this *BaseRequest) EventType() string {
	t := this.Type()
	if t == "" && this.IsPrepare() {
		t = "PREPARE"
	}
	return t
}

// For audit.Auditable interface.
func (this *BaseRequest) EventUsers() []string {
	return datastore.CredsArray(this.credentials)
}

// For audit.Auditable interface.
func (this *BaseRequest) EventNamedArgs() map[string]interface{} {
	argsMap := this.NamedArgs()
	ret := make(map[string]interface{}, len(argsMap))
	for name, argValue := range argsMap {
		ret[name] = argValue.Actual()
	}
	return ret
}

// For audit.Auditable interface.
func (this *BaseRequest) EventPositionalArgs() []interface{} {
	args := this.PositionalArgs()
	ret := make([]interface{}, len(args))
	for i, v := range args {
		ret[i] = v.Actual()
	}
	return ret
}

// For audit.Auditable interface.
func (this *BaseRequest) IsAdHoc() bool {
	return this.Prepared() == nil
}

// For audit.Auditable interface.
func (this *BaseRequest) ClientContextId() string {
	return this.ClientID().String()
}
