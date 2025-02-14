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
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/url"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
	"unsafe"

	"github.com/couchbase/cbauth"
	"github.com/couchbase/gomemcached" // package name is 'gomemcached'
	ntls "github.com/couchbase/goutils/tls"
	"github.com/couchbase/query/logging"
)

// HTTPClient to use for REST and view operations.
var MaxIdleConnsPerHost = 256
var ClientTimeOut = 10 * time.Second
var HTTPTransport = &http.Transport{MaxIdleConnsPerHost: MaxIdleConnsPerHost}
var HTTPClient = &http.Client{Transport: HTTPTransport, Timeout: ClientTimeOut}

// PoolSize is the size of each connection pool (per host).
var PoolSize = 64

// PoolOverflow is the number of overflow connections allowed in a
// pool.
var PoolOverflow = 16

// AsynchronousCloser turns on asynchronous closing for overflow connections
var AsynchronousCloser = false

// TCP KeepAlive enabled/disabled
var TCPKeepalive = false

// Enable MutationToken
var EnableMutationToken = false

// Enable Data Type response
var EnableDataType = false

// Enable Xattr
var EnableXattr = false

// Enable SyncReplication
var EnableSyncReplication = false

// Enable Collections
var EnableCollections = false

// Enable Preserve Expiry
var EnablePreserveExpiry = false

// Enable KV error map
var EnableXerror = false

// TCP keepalive interval in seconds. Default 30 minutes
var TCPKeepaliveInterval = 30 * 60

// Used to decide whether to skip verification of certificates when
// connecting to an ssl port.
var skipVerify = true
var certFile = ""
var keyFile = ""
var caFile = ""
var privateKeyPassphrase = []byte{}

func SetSkipVerify(skip bool) {
	skipVerify = skip
}

func SetCertFile(cert string) {
	certFile = cert
}

func SetCaFile(cacert string) {
	caFile = cacert
}

func SetKeyFile(key string) {
	keyFile = key
}

func SetPrivateKeyPassphrase(passphrase []byte) {
	privateKeyPassphrase = passphrase
}

func UnsetCertSettings() {
	caFile = ""
	certFile = ""
	keyFile = ""
	privateKeyPassphrase = []byte{}
}

// Allow applications to speciify the Poolsize and Overflow
func SetConnectionPoolParams(size, overflow int) {

	if size > 0 {
		PoolSize = size
	}

	if overflow > 0 {
		PoolOverflow = overflow
	}
}

// Turn off overflow connections
func DisableOverflowConnections() {
	PoolOverflow = 0
}

// Toggle asynchronous overflow closer
func EnableAsynchronousCloser(closer bool) {
	AsynchronousCloser = closer
}

// Allow TCP keepalive parameters to be set by the application
func SetTcpKeepalive(enabled bool, interval int) {

	TCPKeepalive = enabled

	if interval > 0 {
		TCPKeepaliveInterval = interval
	}
}

// AuthHandler is a callback that gets the auth username and password
// for the given bucket.
type AuthHandler interface {
	GetCredentials() (string, string, string)
}

// HTTPAuthHandler is kind of AuthHandler that performs more general
// for outgoing http requests than is possible via simple
// GetCredentials() call (i.e. digest auth or different auth per
// different destinations).
type HTTPAuthHandler interface {
	AuthHandler
	SetCredsForRequest(req *http.Request) error
}

// RestPool represents a single pool returned from the pools REST API.
type RestPool struct {
	Name         string `json:"name"`
	StreamingURI string `json:"streamingUri"`
	URI          string `json:"uri"`
}

// Pools represents the collection of pools as returned from the REST API.
type Pools struct {
	ComponentsVersion     map[string]string `json:"componentsVersion,omitempty"`
	ImplementationVersion string            `json:"implementationVersion"`
	IsAdmin               bool              `json:"isAdminCreds"`
	UUID                  string            `json:"uuid"`
	Pools                 []RestPool        `json:"pools"`
}

// A Node is a computer in a cluster running the couchbase software.
type Node struct {
	ClusterCompatibility int                           `json:"clusterCompatibility"`
	ClusterMembership    string                        `json:"clusterMembership"`
	CouchAPIBase         string                        `json:"couchApiBase"`
	NodeUUID             string                        `json:"nodeUUID"`
	Hostname             string                        `json:"hostname"`
	AlternateNames       map[string]NodeAlternateNames `json:"alternateAddresses"`
	InterestingStats     map[string]float64            `json:"interestingStats,omitempty"`
	MCDMemoryAllocated   float64                       `json:"mcdMemoryAllocated"`
	MCDMemoryReserved    float64                       `json:"mcdMemoryReserved"`
	MemoryFree           float64                       `json:"memoryFree"`
	MemoryTotal          float64                       `json:"memoryTotal"`
	OS                   string                        `json:"os"`
	Ports                map[string]int                `json:"ports"`
	Services             []string                      `json:"services"`
	Status               string                        `json:"status"`
	Uptime               int                           `json:"uptime,string"`
	Version              string                        `json:"version"`
	ThisNode             bool                          `json:"thisNode,omitempty"`
}

// A Pool of nodes and buckets.
type Pool struct {
	BucketMap map[string]*Bucket
	Nodes     []Node

	BucketURL map[string]string `json:"buckets"`

	MemoryQuota         float64 `json:"memoryQuota"`
	CbasMemoryQuota     float64 `json:"cbasMemoryQuota"`
	EventingMemoryQuota float64 `json:"eventingMemoryQuota"`
	FtsMemoryQuota      float64 `json:"ftsMemoryQuota"`
	IndexMemoryQuota    float64 `json:"indexMemoryQuota"`

	client *Client
}

// VBucketServerMap is the a mapping of vbuckets to nodes.
type VBucketServerMap struct {
	HashAlgorithm string   `json:"hashAlgorithm"`
	NumReplicas   int      `json:"numReplicas"`
	ServerList    []string `json:"serverList"`
	VBucketMap    [][]int  `json:"vBucketMap"`
	DownNodes     []bool
	sync.Mutex
}

// Bucket is the primary entry point for most data operations.
// Bucket is a locked data structure. All access to its fields should be done using read or write locking,
// as appropriate.
//
// Some access methods require locking, but rely on the caller to do so. These are appropriate
// for calls from methods that have already locked the structure. Methods like this
// take a boolean parameter "bucketLocked".
type Bucket struct {
	sync.RWMutex
	AuthType               string             `json:"authType"`
	Capabilities           []string           `json:"bucketCapabilities"`
	CapabilitiesVersion    string             `json:"bucketCapabilitiesVer"`
	CollectionsManifestUid string             `json:"collectionsManifestUid"`
	Type                   string             `json:"bucketType"`
	Name                   string             `json:"name"`
	NodeLocator            string             `json:"nodeLocator"`
	Quota                  map[string]float64 `json:"quota,omitempty"`
	Replicas               int                `json:"replicaNumber"`
	URI                    string             `json:"uri"`
	StreamingURI           string             `json:"streamingUri"`
	LocalRandomKeyURI      string             `json:"localRandomKeyUri,omitempty"`
	UUID                   string             `json:"uuid"`
	ConflictResolutionType string             `json:"conflictResolutionType,omitempty"`
	DDocs                  struct {
		URI string `json:"uri"`
	} `json:"ddocs,omitempty"`
	BasicStats  map[string]interface{} `json:"basicStats,omitempty"`
	Controllers map[string]interface{} `json:"controllers,omitempty"`

	// These are used for JSON IO, but isn't used for processing
	// since it needs to be swapped out safely.
	VBSMJson  VBucketServerMap `json:"vBucketServerMap"`
	NodesJSON []Node           `json:"nodes"`

	// used to detect a new vbmap
	Version int

	pool             *Pool
	connPools        unsafe.Pointer // *[]*connectionPool
	vBucketServerMap unsafe.Pointer // *VBucketServerMap
	nodeList         unsafe.Pointer // *[]Node
	commonSufix      string
	ah               AuthHandler // auth handler
	closed           bool
}

// PoolServices is all the bucket-independent services in a pool
type PoolServices struct {
	Rev          int             `json:"rev"`
	NodesExt     []NodeServices  `json:"nodesExt"`
	Capabilities json.RawMessage `json:"clusterCapabilities"`
}

// NodeServices is all the bucket-independent services running on
// a node (given by Hostname)
type NodeServices struct {
	Services       map[string]int                `json:"services,omitempty"`
	Hostname       string                        `json:"hostname"`
	ThisNode       bool                          `json:"thisNode"`
	AlternateNames map[string]NodeAlternateNames `json:"alternateAddresses"`
}

type NodeAlternateNames struct {
	Hostname string         `json:"hostname"`
	Ports    map[string]int `json:"ports"`
}

type BucketNotFoundError struct {
	bucket string
}

func (e *BucketNotFoundError) Error() string {
	return fmt.Sprint("No bucket named " + e.bucket)
}

func (v *VBucketServerMap) IsDown(node int) bool {
	return len(v.DownNodes) != 0 && v.DownNodes[node]
}

func (v *VBucketServerMap) MarkDown(vb uint32, replica int) {
	v.Lock()
	defer v.Unlock()
	if v.DownNodes == nil {
		v.DownNodes = make([]bool, len(v.ServerList))
	}
	for i := 0; i < replica; i++ {
		n := v.VBucketMap[vb][i]
		if n >= 0 {
			v.DownNodes[n] = true
		}
	}
}

// VBServerMap returns the current VBucketServerMap.
func (b *Bucket) VBServerMap() *VBucketServerMap {
	b.RLock()
	defer b.RUnlock()
	ret := (*VBucketServerMap)(b.vBucketServerMap)
	return ret
}

func (b *Bucket) ChangedVBServerMap(new *VBucketServerMap) bool {
	b.RLock()
	defer b.RUnlock()
	return b.changedVBServerMap(new)
}

func (b *Bucket) changedVBServerMap(new *VBucketServerMap) bool {
	old := (*VBucketServerMap)(b.vBucketServerMap)
	if new.NumReplicas != old.NumReplicas {
		return true
	}
	if len(new.ServerList) != len(old.ServerList) {
		return true
	}

	// this will also catch the same server list in different order,
	// but better safe than sorry
	for i, s := range new.ServerList {
		if s != old.ServerList[i] {
			return true
		}
	}

	for i, v := range new.VBucketMap {
		for j, n := range v {
			if old.VBucketMap[i][j] != n {
				return true
			}
		}
	}
	return false
}

// true if node is not on the bucket VBmap
func (b *Bucket) obsoleteNode(node string) bool {
	vbmap := b.VBServerMap()
	servers := vbmap.ServerList

	for _, idxs := range vbmap.VBucketMap {
		if len(idxs) == 0 {
			return true
		} else if idxs[0] < 0 || idxs[0] >= len(servers) {
			return true
		}
		if servers[idxs[0]] == node {
			return false
		}
	}
	return true
}

func (b *Bucket) GetName() string {
	b.RLock()
	defer b.RUnlock()
	ret := b.Name
	return ret
}

// Nodes returns the current list of nodes servicing this bucket.
func (b *Bucket) Nodes() []Node {
	b.RLock()
	defer b.RUnlock()
	ret := *(*[]Node)(b.nodeList)
	return ret
}

func (b *Bucket) getConnPools(bucketLocked bool) []*connectionPool {
	if !bucketLocked {
		b.RLock()
		defer b.RUnlock()
	}
	if b.connPools != nil {
		return *(*[]*connectionPool)(b.connPools)
	} else {
		return nil
	}
}

func (b *Bucket) replaceConnPools(with []*connectionPool) {
	b.Lock()
	defer b.Unlock()

	old := b.connPools
	b.connPools = unsafe.Pointer(&with)
	if old != nil {
		for _, pool := range *(*[]*connectionPool)(old) {
			if pool != nil {
				pool.Close()
			}
		}
	}
	return
}

func (b *Bucket) getConnPool(i int) *connectionPool {

	if i < 0 {
		return nil
	}

	p := b.getConnPools(false /* not already locked */)
	if len(p) > i {
		return p[i]
	}

	return nil
}

func (b *Bucket) getConnPoolByHost(host string, bucketLocked bool) *connectionPool {
	pools := b.getConnPools(bucketLocked)
	for _, p := range pools {
		if p != nil && p.host == host {
			return p
		}
	}

	return nil
}

// Bucket DDL
func uriAdj(s string) string {
	return strings.Replace(s, "%", "%25", -1)
}

func (b *Bucket) CreateScope(scope string) error {
	b.RLock()
	pool := b.pool
	client := pool.client
	b.RUnlock()
	args := map[string]interface{}{"name": scope}
	return client.parsePostURLResponseTerse("/pools/default/buckets/"+uriAdj(b.Name)+"/scopes", args, nil)
}

func (b *Bucket) DropScope(scope string) error {
	b.RLock()
	pool := b.pool
	client := pool.client
	b.RUnlock()
	return client.parseDeleteURLResponseTerse("/pools/default/buckets/"+uriAdj(b.Name)+"/scopes/"+uriAdj(scope), nil, nil)
}

func (b *Bucket) CreateCollection(scope string, collection string) error {
	b.RLock()
	pool := b.pool
	client := pool.client
	b.RUnlock()
	args := map[string]interface{}{"name": collection}
	return client.parsePostURLResponseTerse("/pools/default/buckets/"+uriAdj(b.Name)+"/scopes/"+uriAdj(scope)+"/collections", args, nil)
}

func (b *Bucket) DropCollection(scope string, collection string) error {
	b.RLock()
	pool := b.pool
	client := pool.client
	b.RUnlock()
	return client.parseDeleteURLResponseTerse("/pools/default/buckets/"+uriAdj(b.Name)+"/scopes/"+uriAdj(scope)+"/collections/"+uriAdj(collection), nil, nil)
}

func (b *Bucket) FlushCollection(scope string, collection string) error {
	b.RLock()
	pool := b.pool
	client := pool.client
	b.RUnlock()
	args := map[string]interface{}{"name": collection, "scope": scope}
	return client.parsePostURLResponseTerse("/pools/default/buckets/"+uriAdj(b.Name)+"/collections-flush", args, nil)
}

func (b *Bucket) authHandler(bucketLocked bool) (ah AuthHandler) {
	if !bucketLocked {
		b.RLock()
		defer b.RUnlock()
	}
	pool := b.pool
	name := b.Name

	if pool != nil {
		ah = pool.client.ah
	}
	if ah == nil {
		ah = &basicAuth{name, ""}
	} else if cbah, ok := ah.(*cbauth.AuthHandler); ok {
		return cbah.ForBucket(name)
	}

	return
}

// A Client is the starting point for all services across all buckets
// in a Couchbase cluster.
type Client struct {
	BaseURL            *url.URL
	ah                 AuthHandler
	Info               Pools
	tlsConfig          *tls.Config
	disableNonSSLPorts bool
}

func maybeAddAuth(req *http.Request, ah AuthHandler) error {
	if hah, ok := ah.(HTTPAuthHandler); ok {
		return hah.SetCredsForRequest(req)
	}
	if ah != nil {
		user, pass, _ := ah.GetCredentials()
		req.Header.Set("Authorization", "Basic "+
			base64.StdEncoding.EncodeToString([]byte(user+":"+pass)))
	}
	return nil
}

// arbitary number, may need to be tuned #FIXME
const HTTP_MAX_RETRY = 5

// Someday golang network packages will implement standard
// error codes. Until then #sigh
func isHttpConnError(err error) bool {

	estr := err.Error()
	return strings.Contains(estr, "broken pipe") ||
		strings.Contains(estr, "broken connection") ||
		strings.Contains(estr, "connection reset")
}

var client *http.Client

func ClientConfigForX509(caFile, certFile, keyFile string, passphrase []byte) (*tls.Config, error) {
	cfg := &tls.Config{}

	if certFile != "" && keyFile != "" {
		tlsCert, err := ntls.LoadX509KeyPair(certFile, keyFile, passphrase)
		if err != nil {
			return nil, err
		}
		cfg.Certificates = []tls.Certificate{tlsCert}
	} else {
		//error need to pass both certfile and keyfile
		return nil, fmt.Errorf("N1QL: Need to pass both certfile and keyfile")
	}

	var caCert []byte
	var err1 error

	caCertPool := x509.NewCertPool()
	if caFile != "" {
		// Read that value in
		caCert, err1 = ioutil.ReadFile(caFile)
		if err1 != nil {
			return nil, fmt.Errorf(" Error in reading cacert file, err: %v", err1)
		}
		caCertPool.AppendCertsFromPEM(caCert)
	}

	cfg.RootCAs = caCertPool
	return cfg, nil
}

func doHTTPRequest(req *http.Request) (*http.Response, error) {

	var err error
	var res *http.Response

	// we need a client that ignores certificate errors, since we self-sign
	// our certs
	if client == nil && req.URL.Scheme == "https" {
		var tr *http.Transport

		if skipVerify {
			tr = &http.Transport{
				TLSClientConfig:     &tls.Config{InsecureSkipVerify: true},
				MaxIdleConnsPerHost: MaxIdleConnsPerHost,
			}
		} else {
			cfg, err := ClientConfigForX509(caFile, certFile, keyFile, privateKeyPassphrase)
			if err != nil {
				return nil, err
			}

			tr = &http.Transport{
				TLSClientConfig:     cfg,
				MaxIdleConnsPerHost: MaxIdleConnsPerHost,
			}
		}

		client = &http.Client{Transport: tr, Timeout: ClientTimeOut}

	} else if client == nil {
		client = HTTPClient
	}

	for i := 0; i < HTTP_MAX_RETRY; i++ {
		res, err = client.Do(req)
		if err != nil && isHttpConnError(err) {
			continue
		}
		break
	}

	if err != nil {
		return nil, err
	}

	return res, err
}

func doPutAPI(baseURL *url.URL, path string, params map[string]interface{}, authHandler AuthHandler, out interface{}, terse bool) error {
	return doOutputAPI("PUT", baseURL, path, params, authHandler, out, terse)
}

func doPostAPI(baseURL *url.URL, path string, params map[string]interface{}, authHandler AuthHandler, out interface{}, terse bool) error {
	return doOutputAPI("POST", baseURL, path, params, authHandler, out, terse)
}

func doDeleteAPI(baseURL *url.URL, path string, params map[string]interface{}, authHandler AuthHandler, out interface{}, terse bool) error {
	return doOutputAPI("DELETE", baseURL, path, params, authHandler, out, terse)
}

func doOutputAPI(
	httpVerb string,
	baseURL *url.URL,
	path string,
	params map[string]interface{},
	authHandler AuthHandler,
	out interface{},
	terse bool) error {

	var requestUrl string

	if q := strings.Index(path, "?"); q > 0 {
		requestUrl = baseURL.Scheme + "://" + baseURL.Host + path[:q] + "?" + path[q+1:]
	} else {
		requestUrl = baseURL.Scheme + "://" + baseURL.Host + path
	}

	postData := url.Values{}
	for k, v := range params {
		postData.Set(k, fmt.Sprintf("%v", v))
	}

	req, err := http.NewRequest(httpVerb, requestUrl, bytes.NewBufferString(postData.Encode()))
	if err != nil {
		return err
	}

	req.Header.Add("Content-Type", "application/x-www-form-urlencoded")

	err = maybeAddAuth(req, authHandler)
	if err != nil {
		return err
	}

	res, err := doHTTPRequest(req)
	if err != nil {
		return err
	}

	defer res.Body.Close()
	// 200 - ok, 202 - accepted (asynchronously)
	if res.StatusCode != 200 && res.StatusCode != 202 {
		bod, _ := ioutil.ReadAll(io.LimitReader(res.Body, 512))
		if terse {
			var outBuf interface{}

			err := json.Unmarshal(bod, &outBuf)
			if err == nil && outBuf != nil {
				switch errText := outBuf.(type) {
				case string:
					return fmt.Errorf("%s", errText)
				case map[string]interface{}:
					errField := errText["errors"]
					if errField != nil {

						// remove annoying 'map' prefix
						return fmt.Errorf("%s", strings.TrimPrefix(fmt.Sprintf("%v", errField), "map"))
					}
				}
			}
			return fmt.Errorf("%s", string(bod))
		}
		return fmt.Errorf("HTTP error %v getting %q: %s",
			res.Status, requestUrl, bod)
	}

	d := json.NewDecoder(res.Body)
	// PUT/POST/DELETE request may not have a response body
	if d.More() {
		if err = d.Decode(&out); err != nil {
			return err
		}
	}

	return nil
}

func queryRestAPI(
	baseURL *url.URL,
	path string,
	authHandler AuthHandler,
	out interface{},
	terse bool) error {

	var requestUrl string

	if q := strings.Index(path, "?"); q > 0 {
		requestUrl = baseURL.Scheme + "://" + baseURL.Host + path[:q] + "?" + path[q+1:]
	} else {
		requestUrl = baseURL.Scheme + "://" + baseURL.Host + path
	}

	req, err := http.NewRequest("GET", requestUrl, nil)
	if err != nil {
		return err
	}

	err = maybeAddAuth(req, authHandler)
	if err != nil {
		return err
	}

	res, err := doHTTPRequest(req)
	if err != nil {
		return err
	}

	defer res.Body.Close()
	if res.StatusCode != 200 {
		bod, _ := ioutil.ReadAll(io.LimitReader(res.Body, 512))
		if terse {
			var outBuf interface{}

			err := json.Unmarshal(bod, &outBuf)
			if err == nil && outBuf != nil {
				errText, ok := outBuf.(string)
				if ok {
					return fmt.Errorf(errText)
				}
			}
			return fmt.Errorf(string(bod))
		}
		return fmt.Errorf("HTTP error %v getting %q: %s",
			res.Status, requestUrl, bod)
	}

	d := json.NewDecoder(res.Body)
	// GET request should have a response body
	if err = d.Decode(&out); err != nil {
		return fmt.Errorf("json decode err: %#v, for requestUrl: %s",
			err, requestUrl)
	}
	return nil
}

func (c *Client) parseURLResponse(path string, out interface{}) error {
	return queryRestAPI(c.BaseURL, path, c.ah, out, false)
}

func (c *Client) parsePostURLResponseTerse(path string, params map[string]interface{}, out interface{}) error {
	return doPostAPI(c.BaseURL, path, params, c.ah, out, true)
}

func (c *Client) parseDeleteURLResponseTerse(path string, params map[string]interface{}, out interface{}) error {
	return doDeleteAPI(c.BaseURL, path, params, c.ah, out, true)
}

func (c *Client) parsePutURLResponse(path string, params map[string]interface{}, out interface{}) error {
	return doPutAPI(c.BaseURL, path, params, c.ah, out, false)
}

type basicAuth struct {
	u, p string
}

func (b basicAuth) GetCredentials() (string, string, string) {
	return b.u, b.p, b.u
}

func BasicAuthFromURL(us string) (ah AuthHandler) {
	u, err := ParseURL(us)
	if err != nil {
		return
	}
	if user := u.User; user != nil {
		pw, _ := user.Password()
		ah = basicAuth{user.Username(), pw}
	}
	return
}

// ConnectWithAuth connects to a couchbase cluster with the given
// authentication handler.
func ConnectWithAuth(baseU string, ah AuthHandler) (c Client, err error) {
	c.BaseURL, err = ParseURL(baseU)
	if err != nil {
		return
	}
	c.ah = ah

	return c, c.parseURLResponse("/pools", &c.Info)
}

// Call this method with a TLS certificate file name to make communication
// with the KV engine encrypted.
//
// This method should be called immediately after a Connect*() method.
func (c *Client) InitTLS(caFile, certFile, keyfile string, disableNonSSLPorts bool, passphrase []byte) error {
	// Set the values for certs
	SetCaFile(caFile)
	SetCertFile(certFile)
	SetKeyFile(keyFile)
	SetPrivateKeyPassphrase(passphrase)

	if len(caFile) > 0 {
		certFile = caFile
	}
	serverCert, err := ioutil.ReadFile(certFile)
	if err != nil {
		return err
	}
	CA_Pool := x509.NewCertPool()
	CA_Pool.AppendCertsFromPEM(serverCert)
	c.tlsConfig = &tls.Config{RootCAs: CA_Pool}
	c.disableNonSSLPorts = disableNonSSLPorts
	return nil
}

func (c *Client) ClearTLS() {
	c.tlsConfig = nil
	c.disableNonSSLPorts = false
	UnsetCertSettings()
}

// Connect to a couchbase cluster.  An authentication handler will be
// created from the userinfo in the URL if provided.
func Connect(baseU string) (Client, error) {
	return ConnectWithAuth(baseU, BasicAuthFromURL(baseU))
}

// Sample data for scopes and collections as returned from the
// /pooles/default/$BUCKET_NAME/collections API.
// {"myScope2":{"myCollectionC":{}},"myScope1":{"myCollectionB":{},"myCollectionA":{}},"_default":{"_default":{}}}

// Structures for parsing collections manifest.
// The map key is the name of the scope.
// Example data:
// {"uid":"b","scopes":[
//    {"name":"_default","uid":"0","collections":[
//       {"name":"_default","uid":"0"}]},
//    {"name":"myScope1","uid":"8","collections":[
//       {"name":"myCollectionB","uid":"c"},
//       {"name":"myCollectionA","uid":"b"}]},
//    {"name":"myScope2","uid":"9","collections":[
//       {"name":"myCollectionC","uid":"d"}]}]}
type InputManifest struct {
	Uid    string
	Scopes []InputScope
}
type InputScope struct {
	Name        string
	Uid         string
	Collections []InputCollection
}
type InputCollection struct {
	Name string
	Uid  string
}

// Structures for storing collections information.
type Manifest struct {
	Uid    uint64
	Scopes map[string]*Scope // map by name
}
type Scope struct {
	Name        string
	Uid         uint64
	Collections map[string]*Collection // map by name
}
type Collection struct {
	Name string
	Uid  uint64
}

var _EMPTY_MANIFEST *Manifest = &Manifest{Uid: 0, Scopes: map[string]*Scope{}}

func parseCollectionsManifest(res *gomemcached.MCResponse) (*Manifest, error) {
	if !EnableCollections {
		return _EMPTY_MANIFEST, nil
	}

	var im InputManifest
	err := json.Unmarshal(res.Body, &im)
	if err != nil {
		return nil, err
	}

	uid, err := strconv.ParseUint(im.Uid, 16, 64)
	if err != nil {
		return nil, err
	}
	mani := &Manifest{Uid: uid, Scopes: make(map[string]*Scope, len(im.Scopes))}
	for _, iscope := range im.Scopes {
		scope_uid, err := strconv.ParseUint(iscope.Uid, 16, 64)
		if err != nil {
			return nil, err
		}
		scope := &Scope{Uid: scope_uid, Name: iscope.Name, Collections: make(map[string]*Collection, len(iscope.Collections))}
		mani.Scopes[iscope.Name] = scope
		for _, icoll := range iscope.Collections {
			coll_uid, err := strconv.ParseUint(icoll.Uid, 16, 64)
			if err != nil {
				return nil, err
			}
			coll := &Collection{Uid: coll_uid, Name: icoll.Name}
			scope.Collections[icoll.Name] = coll
		}
	}

	return mani, nil
}

// This function assumes the bucket is locked.
func (b *Bucket) GetCollectionsManifest() (*Manifest, error) {
	// Collections not used?
	if !EnableCollections {
		return nil, fmt.Errorf("Collections not enabled.")
	}

	b.RLock()
	pools := b.getConnPools(true /* already locked */)
	if len(pools) == 0 {
		b.RUnlock()
		return nil, fmt.Errorf("Unable to get connection to retrieve collections manifest: no connection pool. No collections access to bucket %s.", b.Name)
	}
	pool := pools[0] // Any pool will do, so use the first one.
	b.RUnlock()
	client, err := pool.Get()
	if err != nil {
		return nil, fmt.Errorf("Unable to get connection to retrieve collections manifest: %v. No collections access to bucket %s.", err, b.Name)
	}
	client.SetDeadline(getDeadline(time.Time{}, DefaultTimeout))

	// We need to select the bucket before GetCollectionsManifest()
	// will work. This is sometimes done at startup (see defaultMkConn())
	// but not always, depending on the auth type.
	// Doing this is safe because we collect the the connections
	// by bucket, so the bucket being selected will never change.
	_, err = client.SelectBucket(b.Name)
	if err != nil {
		pool.Return(client)
		return nil, fmt.Errorf("Unable to select bucket %s: %v. No collections access to bucket %s.", err, b.Name, b.Name)
	}

	res, err := client.GetCollectionsManifest()
	if err != nil {
		pool.Return(client)
		return nil, fmt.Errorf("Unable to retrieve collections manifest: %v. No collections access to bucket %s.", err, b.Name)
	}
	mani, err := parseCollectionsManifest(res)
	if err != nil {
		pool.Return(client)
		return nil, fmt.Errorf("Unable to parse collections manifest: %v. No collections access to bucket %s.", err, b.Name)
	}

	pool.Return(client)
	return mani, nil
}

func (b *Bucket) RefreshFully() error {
	return b.refresh(false)
}

func (b *Bucket) Refresh() error {
	return b.refresh(true)
}

func (b *Bucket) refresh(preserveConnections bool) error {
	b.RLock()
	pool := b.pool
	uri := b.URI
	client := pool.client
	b.RUnlock()

	var poolServices PoolServices
	var err error
	if client.tlsConfig != nil {
		poolServices, err = client.GetPoolServices("default")
		if err != nil {
			return err
		}
	}

	tmpb := &Bucket{}
	err = pool.client.parseURLResponse(uri, tmpb)
	if err != nil {
		return err
	}

	pools := b.getConnPools(false /* bucket not already locked */)

	// We need this lock to ensure that bucket refreshes happening because
	// of NMVb errors received during bulkGet do not end up over-writing
	// pool.inUse.
	b.Lock()

	for _, pool := range pools {
		if pool != nil {
			pool.inUse = false
		}
	}

	newcps := make([]*connectionPool, len(tmpb.VBSMJson.ServerList))
	for i := range newcps {
		hostport := tmpb.VBSMJson.ServerList[i]
		if preserveConnections {
			pool := b.getConnPoolByHost(hostport, true /* bucket already locked */)
			if pool != nil && pool.inUse == false && (!pool.encrypted || pool.tlsConfig == client.tlsConfig) {
				// if the hostname and index is unchanged then reuse this pool
				newcps[i] = pool
				pool.inUse = true
				continue
			}
		}

		var encrypted bool
		if client.tlsConfig != nil {
			hostport, encrypted, err = MapKVtoSSLExt(hostport, &poolServices, client.disableNonSSLPorts)
			if err != nil {
				b.Unlock()
				return err
			}
		}

		if b.ah != nil {
			newcps[i] = newConnectionPool(hostport,
				b.ah, AsynchronousCloser, PoolSize, PoolOverflow, client.tlsConfig, b.Name, encrypted)

		} else {
			newcps[i] = newConnectionPool(hostport,
				b.authHandler(true /* bucket already locked */),
				AsynchronousCloser, PoolSize, PoolOverflow, client.tlsConfig, b.Name, encrypted)
		}
	}
	b.replaceConnPools2(newcps, true /* bucket already locked */)
	tmpb.ah = b.ah
	if b.vBucketServerMap != nil && b.changedVBServerMap(&tmpb.VBSMJson) {
		b.Version++
	}
	b.vBucketServerMap = unsafe.Pointer(&tmpb.VBSMJson)
	b.nodeList = unsafe.Pointer(&tmpb.NodesJSON)

	b.Unlock()
	return nil
}

func (p *Pool) refresh() (err error) {
	p.BucketMap = make(map[string]*Bucket)

	buckets := []Bucket{}
	err = p.client.parseURLResponse(p.BucketURL["uri"], &buckets)
	if err != nil {
		return err
	}
	for i, _ := range buckets {
		b := new(Bucket)
		*b = buckets[i]
		b.pool = p
		b.nodeList = unsafe.Pointer(&b.NodesJSON)

		// MB-33185 this is merely defensive, just in case
		// refresh() gets called on a perfectly node pool
		ob, ok := p.BucketMap[b.Name]
		if ok && ob.connPools != nil {
			ob.Close()
		}
		b.replaceConnPools(make([]*connectionPool, len(b.VBSMJson.ServerList)))
		b.Version = 0
		p.BucketMap[b.Name] = b
		runtime.SetFinalizer(b, bucketFinalizer)
	}
	buckets = nil
	return nil
}

// GetPool gets a pool from within the couchbase cluster (usually "default").
func (c *Client) GetPool(name string) (p Pool, err error) {
	var poolURI string

	for _, p := range c.Info.Pools {
		if p.Name == name {
			poolURI = p.URI
			break
		}
	}
	if poolURI == "" {
		return p, errors.New("No pool named " + name)
	}

	err = c.parseURLResponse(poolURI, &p)
	if err != nil {
		return p, err
	}

	p.client = c

	err = p.refresh()
	return
}

// GetPoolServices returns all the bucket-independent services in a pool.
// (See "Exposing services outside of bucket context" in http://goo.gl/uuXRkV)
func (c *Client) GetPoolServices(name string) (ps PoolServices, err error) {
	var poolName string
	for _, p := range c.Info.Pools {
		if p.Name == name {
			poolName = p.Name
		}
	}
	if poolName == "" {
		return ps, errors.New("No pool named " + name)
	}

	poolURI := "/pools/" + poolName + "/nodeServices"
	err = c.parseURLResponse(poolURI, &ps)

	return
}

// Close marks this bucket as no longer needed, closing connections it
// may have open.
func (b *Bucket) Close() {
	b.Lock()
	defer b.Unlock()
	if b.connPools != nil {
		for _, c := range b.getConnPools(true /* already locked */) {
			if c != nil {
				c.Close()
			}
		}
		b.connPools = nil
	}
}

func bucketFinalizer(b *Bucket) {
	if b.connPools != nil {
		if !b.closed {
			logging.Warnf("Finalizing a bucket with active connections.")
		}

		// MB-33185 do not leak connection pools
		b.Close()
	}
}

// GetBucket gets a bucket from within this pool.
func (p *Pool) GetBucket(name string) (*Bucket, error) {
	rv, ok := p.BucketMap[name]
	if !ok {
		return nil, &BucketNotFoundError{bucket: name}
	}
	err := rv.Refresh()
	if err != nil {
		return nil, err
	}
	return rv, nil
}

// Release bucket connections when the pool is no longer in use
func (p *Pool) Close() {

	// MB-36186 make the bucket map inaccessible
	bucketMap := p.BucketMap
	p.BucketMap = nil

	// fine to loop through the buckets unlocked
	// locking happens at the bucket level
	for b, _ := range bucketMap {

		// MB-36186 make the bucket unreachable and avoid concurrent read/write map panics
		bucket := bucketMap[b]
		bucketMap[b] = nil

		bucket.Lock()

		// MB-33208 defer closing connection pools until the bucket is no longer used
		// MB-36186 if the bucket is unused make it unreachable straight away
		needClose := bucket.connPools == nil && !bucket.closed
		if needClose {
			runtime.SetFinalizer(&bucket, nil)
		}
		bucket.closed = true
		bucket.Unlock()
		if needClose {
			bucket.Close()
		}
	}
}

func GetSystemBucket(c *Client, p *Pool, name string) (*Bucket, error) {
	bucket, err := p.GetBucket(name)
	if err != nil {
		if _, ok := err.(*BucketNotFoundError); !ok {
			return nil, err
		}

		// create the bucket if not found
		args := map[string]interface{}{
			"bucketType": "couchbase",
			"name":       name,
			"ramQuotaMB": 100,
		}
		var ret interface{}
		// allow "bucket already exists" error in case duplicate create
		// (e.g. two query nodes starting at same time)
		err = c.parsePostURLResponseTerse("/pools/default/buckets", args, &ret)
		if err != nil && !AlreadyExistsError(err) {
			return nil, err
		}

		// bucket created asynchronously, try to get the bucket
		maxRetry := 8
		interval := 100 * time.Millisecond
		for i := 0; i < maxRetry; i++ {
			time.Sleep(interval)
			interval *= 2
			err = p.refresh()
			if err != nil {
				return nil, err
			}
			bucket, err = p.GetBucket(name)
			if bucket != nil {
				bucket.RLock()
				ok := !bucket.closed && len(bucket.getConnPools(true /* already locked */)) > 0
				bucket.RUnlock()
				if ok {
					break
				}
			} else if err != nil {
				if _, ok := err.(*BucketNotFoundError); !ok {
					break
				}
			}
		}
	}

	return bucket, err
}

func DropSystemBucket(c *Client, name string) error {
	err := c.parseDeleteURLResponseTerse("/pools/default/buckets/"+name, nil, nil)
	return err
}

func AlreadyExistsError(err error) bool {
	// Bucket error:     Bucket with given name already exists
	// Scope error:      Scope with this name already exists
	// Collection error: Collection with this name already exists
	return strings.Contains(err.Error(), " name already exists")
}

const _BACK_OFF_INTERVAL = 500 * time.Millisecond

func InvokeEndpointWithRetry(url string, u string, p string, cmd string, ctype string, data string, retries int) ([]byte, error) {

	dataReader := strings.NewReader(data)
	req, err := http.NewRequest(cmd, url, dataReader)
	if err != nil {
		return nil, err
	}
	if ctype != "" {
		req.Header.Add("Content-Type", ctype)
	}
	req.SetBasicAuth(u, p)

	client := &http.Client{}

	extra := ""
	backoffSleep := _BACK_OFF_INTERVAL
	exponential := true
	for i := 0; ; i++ {
		dataReader.Seek(0, io.SeekStart)
		resp, err := client.Do(req)
		if err == nil && resp != nil {
			if resp.StatusCode == 200 {
				body, err := ioutil.ReadAll(resp.Body)
				if err != nil {
					return nil, err
				}
				resp.Body.Close()
				return body, nil
			}
			delay := resp.Header.Get("Retry-After")
			if delay != "" {
				secs, err := strconv.Atoi(delay)
				if err != nil {
					ts, err := time.Parse(time.RFC1123, delay)
					if err == nil {
						secs = int(ts.Sub(time.Now()).Seconds())
					}
				}
				if secs > 0 {
					backoffSleep = time.Duration(secs) * time.Second
					exponential = false
				}
			}
		}
		if resp != nil && resp.Body != nil {
			if resp.StatusCode != 200 {
				body, err := ioutil.ReadAll(resp.Body)
				if err == nil {
					extra = string(body)
				}
			}
			resp.Body.Close()
		}
		if i < retries {
			time.Sleep(backoffSleep)
			if exponential {
				backoffSleep *= 2
			} else {
				backoffSleep = _BACK_OFF_INTERVAL
				exponential = true
			}
		} else {
			if err == nil {
				if resp == nil {
					return nil, fmt.Errorf("Unknown error")
				} else {
					if len(extra) > 0 {
						return nil, fmt.Errorf("%s [%s]", resp.Status, extra)
					} else {
						return nil, fmt.Errorf("%s", resp.Status)
					}
				}
			}
			return nil, err
		}
	}
}
