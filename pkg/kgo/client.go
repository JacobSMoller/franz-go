// Package kgo provides a pure Go efficient Kafka client for Kafka 0.8.0+ with
// support for transactions, regex topic consuming, the latest partition
// strategies, and more. This client supports all client related KIPs.
//
// This client aims to be simple to use while still interacting with Kafka in a
// near ideal way. For more overview of the entire client itself, please see
// the README on the project's Github page.
package kgo

import (
	"context"
	"errors"
	"fmt"
	"hash/crc32"
	"math/rand"
	"net"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/twmb/franz-go/pkg/kerr"
	"github.com/twmb/franz-go/pkg/kmsg"
)

var crc32c = crc32.MakeTable(crc32.Castagnoli) // record crc's use Castagnoli table; for consuming/producing

// Client issues requests and handles responses to a Kafka cluster.
type Client struct {
	cfg cfg

	ctx       context.Context
	ctxCancel func()

	rng *rand.Rand

	brokersMu    sync.RWMutex
	brokers      []*broker // ordered by broker ID
	seeds        []*broker // seed brokers, also ordered by ID
	anyBrokerIdx int32
	anySeedIdx   int32
	stopBrokers  bool // set to true on close to stop updateBrokers

	// A sink and a source is created once per node ID and persists
	// forever. We expect the list to be small.
	//
	// The mutex only exists to allow consumer session stopping to read
	// sources to notify when starting a session; all writes happen in the
	// metadata loop.
	sinksAndSourcesMu sync.Mutex
	sinksAndSources   map[int32]sinkAndSource

	reqFormatter  *kmsg.RequestFormatter
	connTimeouter connTimeouter

	bufPool bufPool // for to brokers to share underlying reusable request buffers
	pnrPool pnrPool // for sinks to reuse []promisedNumberedRecord

	controllerIDMu sync.Mutex
	controllerID   int32

	// The following two ensure that we only have one fetchBrokerMetadata
	// at once. This avoids unnecessary broker metadata requests and
	// metadata trampling.
	fetchingBrokersMu sync.Mutex
	fetchingBrokers   *struct {
		done chan struct{}
		err  error
	}

	producer producer
	consumer consumer

	compressor   *compressor
	decompressor *decompressor

	coordinatorsMu sync.Mutex
	coordinators   map[coordinatorKey]*coordinatorLoad

	updateMetadataCh    chan string
	updateMetadataNowCh chan string // like above, but with high priority
	metawait            metawait
	metadone            chan struct{}
}

func (cl *Client) idempotent() bool { return !cl.cfg.disableIdempotency }

type sinkAndSource struct {
	sink   *sink
	source *source
}

func (cl *Client) allSinksAndSources(fn func(sns sinkAndSource)) {
	cl.sinksAndSourcesMu.Lock()
	defer cl.sinksAndSourcesMu.Unlock()

	for _, sns := range cl.sinksAndSources {
		fn(sns)
	}
}

type hostport struct {
	host string
	port int32
}

const defaultKafkaPort = 9092

// NewClient returns a new Kafka client with the given options or an error if
// the options are invalid. Connections to brokers are lazily created only when
// requests are written to them.
//
// By default, the client uses the latest stable request versions when talking
// to Kafka. If you use a broker older than 0.10.0, then you need to manually
// set a MaxVersions option. Otherwise, there is usually no harm in defaulting
// to the latest API versions, although occasionally Kafka introduces new
// required parameters that do not have zero value defaults.
//
// NewClient also launches a goroutine which periodically updates the cached
// topic metadata.
func NewClient(opts ...Opt) (*Client, error) {
	cfg := defaultCfg()
	for _, opt := range opts {
		opt.apply(&cfg)
	}

	if cfg.retryTimeout == nil {
		cfg.retryTimeout = func(key int16) time.Duration {
			switch key {
			case ((*kmsg.JoinGroupRequest)(nil)).Key(),
				((*kmsg.SyncGroupRequest)(nil)).Key(),
				((*kmsg.HeartbeatRequest)(nil)).Key():
				return cfg.sessionTimeout
			}
			return 30 * time.Second
		}
	}

	if err := cfg.validate(); err != nil {
		return nil, err
	}

	seeds := make([]hostport, 0, len(cfg.seedBrokers))
	for _, seedBroker := range cfg.seedBrokers {
		hp, err := parseBrokerAddr(seedBroker)
		if err != nil {
			return nil, err
		}
		seeds = append(seeds, hp)
	}

	ctx, cancel := context.WithCancel(context.Background())

	cl := &Client{
		cfg:       cfg,
		ctx:       ctx,
		ctxCancel: cancel,
		rng:       rand.New(rand.NewSource(time.Now().UnixNano())),

		controllerID: unknownControllerID,

		sinksAndSources: make(map[int32]sinkAndSource),

		reqFormatter:  kmsg.NewRequestFormatter(),
		connTimeouter: connTimeouter{def: cfg.requestTimeoutOverhead},

		bufPool: newBufPool(),
		pnrPool: newPnrPool(),

		decompressor: newDecompressor(),

		coordinators: make(map[coordinatorKey]*coordinatorLoad),

		updateMetadataCh:    make(chan string, 1),
		updateMetadataNowCh: make(chan string, 1),
		metadone:            make(chan struct{}),
	}

	compressor, err := newCompressor(cl.cfg.compression...)
	if err != nil {
		return nil, err
	}
	cl.compressor = compressor

	// Before we start any goroutines below, we must notify any interested
	// hooks of our existence.
	cl.cfg.hooks.each(func(h Hook) {
		if h, ok := h.(HookNewClient); ok {
			h.OnNewClient(cl)
		}
	})

	cl.producer.init(cl)
	cl.consumer.init(cl)
	cl.metawait.init()

	if cfg.id != nil {
		cl.reqFormatter = kmsg.NewRequestFormatter(kmsg.FormatterClientID(*cfg.id))
	}

	for i, seed := range seeds {
		b := cl.newBroker(unknownSeedID(i), seed.host, seed.port, nil)
		cl.seeds = append(cl.seeds, b)
	}
	sort.Slice(cl.seeds, func(i, j int) bool { return cl.seeds[i].meta.NodeID < cl.seeds[j].meta.NodeID })
	go cl.updateMetadataLoop()
	go cl.reapConnectionsLoop()

	return cl, nil
}

// Ping returns whether any broker is reachable, iterating over any discovered
// broker or seed broker until one returns a successful response to an
// ApiVersions request. No discovered broker nor seed broker is attempted more
// than once. If all requests fail, this returns final error.
func (cl *Client) Ping(ctx context.Context) error {
	req := kmsg.NewPtrApiVersionsRequest()
	req.ClientSoftwareName = cl.cfg.softwareName
	req.ClientSoftwareVersion = cl.cfg.softwareVersion

	cl.brokersMu.RLock()
	brokers := append([]*broker(nil), cl.brokers...)
	cl.brokersMu.RUnlock()

	var lastErr error
	for _, brs := range [2][]*broker{
		brokers,
		cl.seeds,
	} {
		for _, br := range brs {
			_, err := br.waitResp(ctx, req)
			if lastErr = err; lastErr == nil {
				return nil
			}
		}
	}
	return lastErr
}

// Parse broker IP/host and port from a string, using the default Kafka port if
// unspecified. Supported address formats:
//
// - IPv4 host/IP without port: "127.0.0.1", "localhost"
// - IPv4 host/IP with port: "127.0.0.1:1234", "localhost:1234"
// - IPv6 IP without port:  "[2001:1000:2000::1]", "::1"
// - IPv6 IP with port: "[2001:1000:2000::1]:1234"
func parseBrokerAddr(addr string) (hostport, error) {
	// Bracketed IPv6
	if strings.IndexByte(addr, '[') == 0 {
		parts := strings.Split(addr[1:], "]")
		if len(parts) != 2 {
			return hostport{}, fmt.Errorf("invalid addr: %s", addr)
		}
		// No port specified -> use default
		if len(parts[1]) == 0 {
			return hostport{parts[0], defaultKafkaPort}, nil
		}
		port, err := strconv.ParseInt(parts[1][1:], 10, 32)
		if err != nil {
			return hostport{}, fmt.Errorf("unable to parse port from addr: %w", err)
		}
		return hostport{parts[0], int32(port)}, nil
	}

	// IPv4 with no port
	if strings.IndexByte(addr, ':') == -1 {
		return hostport{addr, defaultKafkaPort}, nil
	}

	// Either a IPv6 literal ("::1"), IP:port or host:port
	// Try to parse as IP:port or host:port
	h, p, err := net.SplitHostPort(addr)
	if err != nil {
		// IPV6 literal - use default Kafka port
		return hostport{addr, defaultKafkaPort}, nil
	}
	port, err := strconv.ParseInt(p, 10, 32)
	if err != nil {
		return hostport{}, fmt.Errorf("unable to parse port from addr: %w", err)
	}
	return hostport{h, int32(port)}, nil
}

type connTimeouter struct {
	def                  time.Duration
	joinMu               sync.Mutex
	lastRebalanceTimeout time.Duration
}

func (c *connTimeouter) timeouts(req kmsg.Request) (r, w time.Duration) {
	def := c.def
	millis := func(m int32) time.Duration { return time.Duration(m) * time.Millisecond }
	switch t := req.(type) {
	default:
		if timeoutRequest, ok := req.(kmsg.TimeoutRequest); ok {
			timeoutMillis := timeoutRequest.Timeout()
			return def + millis(timeoutMillis), def
		}
		return def, def

	case *produceRequest:
		return def + millis(t.timeout), def
	case *fetchRequest:
		return def + millis(t.maxWait), def
	case *kmsg.FetchRequest:
		return def + millis(t.MaxWaitMillis), def

	// Join and sync can take a long time. Sync has no notion of
	// timeouts, but since the flow of requests should be first
	// join, then sync, we can stash the timeout from the join.

	case *kmsg.JoinGroupRequest:
		c.joinMu.Lock()
		c.lastRebalanceTimeout = millis(t.RebalanceTimeoutMillis)
		c.joinMu.Unlock()

		return def + millis(t.RebalanceTimeoutMillis), def
	case *kmsg.SyncGroupRequest:
		read := def
		c.joinMu.Lock()
		if c.lastRebalanceTimeout != 0 {
			read = c.lastRebalanceTimeout
		}
		c.joinMu.Unlock()

		return read, def
	}
}

// broker returns a random broker from all brokers ever known.
func (cl *Client) broker() *broker {
	cl.brokersMu.Lock() // full lock needed for anyBrokerIdx below
	defer cl.brokersMu.Unlock()

	var b *broker
	if len(cl.brokers) > 0 {
		cl.anyBrokerIdx %= int32(len(cl.brokers))
		b = cl.brokers[cl.anyBrokerIdx]
		cl.anyBrokerIdx++
	} else {
		cl.anySeedIdx %= int32(len(cl.seeds))
		b = cl.seeds[cl.anySeedIdx]
		cl.anySeedIdx++
	}
	return b
}

func (cl *Client) waitTries(ctx context.Context, backoff time.Duration) bool {
	after := time.NewTimer(backoff)
	defer after.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-cl.ctx.Done():
		return false
	case <-after.C:
		return true
	}
}

// A broker may sometimes indicate it supports offset for leader epoch v2+ when
// it does not. We need to catch that and avoid issuing offset for leader
// epoch, because we will just loop continuously failing.
//
// We do not catch every case, such as when a person explicitly assigns offsets
// with epochs, but we catch a few areas that would be returned from a broker
// itself.
//
// This should only be used *after* at least one successful response.
func (cl *Client) supportsOffsetForLeaderEpoch() bool {
	cl.brokersMu.RLock()
	defer cl.brokersMu.RUnlock()

	for _, b := range cl.brokers {
		if v := b.loadVersions(); v != nil && v.versions[23] >= 2 {
			return true
		}
	}
	return false
}

// fetchBrokerMetadata issues a metadata request solely for broker information.
func (cl *Client) fetchBrokerMetadata(ctx context.Context) error {
	cl.fetchingBrokersMu.Lock()
	wait := cl.fetchingBrokers
	if wait != nil {
		cl.fetchingBrokersMu.Unlock()
		<-wait.done
		return wait.err
	}
	wait = &struct {
		done chan struct{}
		err  error
	}{done: make(chan struct{})}
	cl.fetchingBrokers = wait
	cl.fetchingBrokersMu.Unlock()

	defer func() {
		cl.fetchingBrokersMu.Lock()
		defer cl.fetchingBrokersMu.Unlock()
		cl.fetchingBrokers = nil
		close(wait.done)
	}()

	_, _, wait.err = cl.fetchMetadata(ctx, kmsg.NewPtrMetadataRequest(), true)
	return wait.err
}

func (cl *Client) fetchMetadataForTopics(ctx context.Context, all bool, topics []string) (*broker, *kmsg.MetadataResponse, error) {
	req := kmsg.NewPtrMetadataRequest()
	req.AllowAutoTopicCreation = cl.cfg.allowAutoTopicCreation
	if all {
		req.Topics = nil
	} else if len(topics) == 0 {
		req.Topics = []kmsg.MetadataRequestTopic{}
	} else {
		for _, topic := range topics {
			reqTopic := kmsg.NewMetadataRequestTopic()
			reqTopic.Topic = kmsg.StringPtr(topic)
			req.Topics = append(req.Topics, reqTopic)
		}
	}
	return cl.fetchMetadata(ctx, req, true)
}

func (cl *Client) fetchMetadata(ctx context.Context, req *kmsg.MetadataRequest, limitRetries bool) (*broker, *kmsg.MetadataResponse, error) {
	r := cl.retriable()

	// We limit retries for internal metadata refreshes, because these do
	// not need to retry forever and are usually blocking *other* requests.
	// e.g., producing bumps load errors when metadata returns, so 3
	// failures here will correspond to 1 bumped error count. To make the
	// number more accurate, we should *never* retry here, but this is
	// pretty intolerant of immediately-temporary network issues. Rather,
	// we use a small count of 3 retries, which with the default backoff,
	// will be <2s of retrying. This is still intolerant of temporary
	// failures, but it does allow recovery from a dns issue / bad path.
	if limitRetries {
		r.limitRetries = 3
	}

	meta, err := req.RequestWith(ctx, r)
	if err == nil {
		if meta.ControllerID >= 0 {
			cl.controllerIDMu.Lock()
			cl.controllerID = meta.ControllerID
			cl.controllerIDMu.Unlock()
		}
		cl.updateBrokers(meta.Brokers)
	}
	return r.last, meta, err
}

// updateBrokers is called with the broker portion of every metadata response.
// All metadata responses contain all known live brokers, so we can always
// use the response.
func (cl *Client) updateBrokers(brokers []kmsg.MetadataResponseBroker) {
	sort.Slice(brokers, func(i, j int) bool { return brokers[i].NodeID < brokers[j].NodeID })
	newBrokers := make([]*broker, 0, len(brokers))

	cl.brokersMu.Lock()
	defer cl.brokersMu.Unlock()

	if cl.stopBrokers {
		return
	}

	for len(brokers) > 0 && len(cl.brokers) > 0 {
		ob := cl.brokers[0]
		nb := brokers[0]

		switch {
		case ob.meta.NodeID < nb.NodeID:
			ob.stopForever()
			cl.brokers = cl.brokers[1:]

		case ob.meta.NodeID == nb.NodeID:
			if !ob.meta.equals(nb) {
				ob.stopForever()
				ob = cl.newBroker(nb.NodeID, nb.Host, nb.Port, nb.Rack)
			}
			newBrokers = append(newBrokers, ob)
			cl.brokers = cl.brokers[1:]
			brokers = brokers[1:]

		case ob.meta.NodeID > nb.NodeID:
			newBrokers = append(newBrokers, cl.newBroker(nb.NodeID, nb.Host, nb.Port, nb.Rack))
			brokers = brokers[1:]
		}
	}

	for len(cl.brokers) > 0 {
		ob := cl.brokers[0]
		ob.stopForever()
		cl.brokers = cl.brokers[1:]
	}

	for len(brokers) > 0 {
		nb := brokers[0]
		newBrokers = append(newBrokers, cl.newBroker(nb.NodeID, nb.Host, nb.Port, nb.Rack))
		brokers = brokers[1:]
	}

	cl.brokers = newBrokers
}

// Close leaves any group and closes all connections and goroutines.
//
// If you are group consuming and have overridden the default OnRevoked, you
// must manually commit offsets before closing the client.
func (cl *Client) Close() {
	cl.LeaveGroup()
	// After LeaveGroup, consumers cannot consume anymore. LeaveGroup
	// internally assigns noTopicsPartitions, which uses noConsmerSession,
	// which prevents loopFetch from starting. Assigning also waits for the
	// prior session to be complete, meaning loopFetch cannot be running.

	// Now we kill the client context and all brokers, ensuring all
	// requests fail. This will finish all producer callbacks and
	// stop the metadata loop.
	cl.ctxCancel()
	cl.brokersMu.Lock()
	cl.stopBrokers = true
	for _, broker := range cl.brokers {
		broker.stopForever()
	}
	cl.brokersMu.Unlock()
	for _, broker := range cl.seeds {
		broker.stopForever()
	}

	// Wait for metadata to quit so we know no more erroring topic
	// partitions will be created. After metadata has quit, we can
	// safely stop sinks and sources, as no more will be made.
	<-cl.metadone

	for _, sns := range cl.sinksAndSources {
		sns.sink.maybeDrain()     // awaken anything in backoff
		sns.source.maybeConsume() // same
	}

	cl.failBufferedRecords(ErrClientClosed)

	// We need one final poll: if any sources buffered a fetch, then the
	// manageFetchConcurrency loop only exits when all fetches have been
	// drained, because draining a fetch is what decrements an "active"
	// fetch. PollFetches with `nil` is instant.
	cl.PollFetches(nil)
}

// Request issues a request to Kafka, waiting for and returning the response.
// If a retriable network error occurs, or if a retriable group / transaction
// coordinator error occurs, the request is retried. All other errors are
// returned.
//
// If the request is an admin request, this will issue it to the Kafka
// controller. If the controller ID is unknown, this will attempt to fetch it.
// If the fetch errors, this will return an unknown controller error.
//
// If the request is a group or transaction coordinator request, this will
// issue the request to the appropriate group or transaction coordinator.
//
// For transaction requests, the request is issued to the transaction
// coordinator. However, if the request is an init producer ID request and the
// request has no transactional ID, the request goes to any broker.
//
// Some requests need to be split and sent to many brokers. For these requests,
// it is *highly* recommended to use RequestSharded. Not all responses from
// many brokers can be cleanly merged. However, for the requests that are
// split, this does attempt to merge them in a sane way.
//
// The following requests are split:
//
//     ListOffsets
//     OffsetFetch (if using v8+ for Kafka 3.0+)
//     DescribeGroups
//     ListGroups
//     DeleteRecords
//     OffsetForLeaderEpoch
//     DescribeConfigs
//     AlterConfigs
//     AlterReplicaLogDirs
//     DescribeLogDirs
//     DeleteGroups
//     IncrementalAlterConfigs
//     DescribeProducers
//     DescribeTransactions
//     ListTransactions
//
// Kafka 3.0 introduced batch OffsetFetch and batch FindCoordinator requests.
// This function is forward-compatible for the old, singular OffsetFetch and
// FindCoordinator requests, but is not backward-compatible for batched
// requests. It is recommended to only use the old format unless you know you
// are speaking to Kafka 3.0+.
//
// In short, this method tries to do the correct thing depending on what type
// of request is being issued.
//
// The passed context can be used to cancel a request and return early. Note
// that if the request was written to Kafka but the context canceled before a
// response is received, Kafka may still operate on the received request.
//
// If using this function to issue kmsg.ProduceRequest's, you must configure
// the client with the same RequiredAcks option that you use in the request.
// If you are issuing produce requests with 0 acks, you must configure the
// client with the same timeout you use in the request. The client will
// internally rewrite the incoming request's acks to match the client's
// configuration, and it will rewrite the timeout millis if the acks is 0. It
// is strongly recommended to not issue raw kmsg.ProduceRequest's.
func (cl *Client) Request(ctx context.Context, req kmsg.Request) (kmsg.Response, error) {
	resps, merge := cl.shardedRequest(ctx, req)
	// If there is no merge function, only one request was issued directly
	// to a broker. Return the resp and err directly.
	if merge == nil {
		return resps[0].Resp, resps[0].Err
	}
	return merge(resps)
}

func (cl *Client) retriable() *retriable {
	return cl.retriableBrokerFn(func() (*broker, error) { return cl.broker(), nil })
}

func (cl *Client) retriableBrokerFn(fn func() (*broker, error)) *retriable {
	return &retriable{cl: cl, br: fn}
}

func (cl *Client) shouldRetry(tries int, err error) bool {
	return (kerr.IsRetriable(err) || isRetriableBrokerErr(err)) && int64(tries) < cl.cfg.retries
}

func (cl *Client) shouldRetryNext(tries int, err error) bool {
	return isSkippableBrokerErr(err) && int64(tries) < cl.cfg.retries
}

type retriable struct {
	cl   *Client
	br   func() (*broker, error)
	last *broker

	// If non-zero, limitRetries may specify a smaller # of retries than
	// the client RequestRetries number. This is used for internal requests
	// that can fail / do not need to retry forever.
	limitRetries int

	// parseRetryErr, if non-nil, can parse a retriable error out of the
	// response and return it. This error is *not* returned from the
	// request if the req cannot be retried due to timeout or retry limits,
	// but it *can* allow a retry if neither limit is hit yet.
	parseRetryErr func(kmsg.Response) error
}

func (r *retriable) Request(ctx context.Context, req kmsg.Request) (kmsg.Response, error) {
	tries := 0
	tryStart := time.Now()
	retryTimeout := r.cl.cfg.retryTimeout(req.Key())

	next, nextErr := r.br()
start:
	tries++
	br, err := next, nextErr
	r.last = br
	var resp kmsg.Response
	var retryErr error
	if err == nil {
		resp, err = r.last.waitResp(ctx, req)
		if err == nil && r.parseRetryErr != nil {
			retryErr = r.parseRetryErr(resp)
		}
	}

	if err != nil || retryErr != nil {
		if r.limitRetries == 0 || tries < r.limitRetries {
			backoff := r.cl.cfg.retryBackoff(tries)
			if retryTimeout == 0 || time.Now().Add(backoff).Sub(tryStart) <= retryTimeout {
				// If this broker / request had a retriable error, we can
				// just retry now. If the error is *not* retriable but
				// is a broker-specific network error, and the next
				// broker is different than the current, we also retry.
				if r.cl.shouldRetry(tries, err) || r.cl.shouldRetry(tries, retryErr) {
					if r.cl.waitTries(ctx, backoff) {
						next, nextErr = r.br()
						goto start
					}
				} else if r.cl.shouldRetryNext(tries, err) {
					next, nextErr = r.br()
					if next != br && r.cl.waitTries(ctx, backoff) {
						goto start
					}
				}
			}
		}
	}
	return resp, err
}

// ResponseShard ties together a request with either the response it received
// or an error that prevented a response from being received.
type ResponseShard struct {
	// Meta contains the broker that this request was issued to, or an
	// unknown (node ID -1) metadata if the request could not be issued.
	//
	// Requests can fail to even be issued if an appropriate broker cannot
	// be loaded of if the client cannot understand the request.
	Meta BrokerMetadata

	// Req is the request that was issued to this broker.
	Req kmsg.Request

	// Resp is the response received from the broker, if any.
	Resp kmsg.Response

	// Err, if non-nil, is the error that prevented a response from being
	// received or the request from being issued.
	Err error
}

// RequestSharded performs the same logic as Request, but returns all responses
// from any broker that the request was split to. This always returns at least
// one shard. If the request does not need to be issued (describing no groups),
// this issues the request to a random broker just to ensure that one shard
// exists.
//
// There are only a few requests that are strongly recommended to explicitly
// use RequestSharded; the rest can by default use Request. These few requests
// are mentioned in the documentation for Request.
//
// If, in the process of splitting a request, some topics or partitions are
// found to not exist, or Kafka replies that a request should go to a broker
// that does not exist, all those non-existent pieces are grouped into one
// request to the first seed broker. This will show up as a seed broker node ID
// (min int32) and the response will likely contain purely errors.
//
// The response shards are ordered by broker metadata.
func (cl *Client) RequestSharded(ctx context.Context, req kmsg.Request) []ResponseShard {
	resps, _ := cl.shardedRequest(ctx, req)
	sort.Slice(resps, func(i, j int) bool {
		l := &resps[i].Meta
		r := &resps[j].Meta

		if l.NodeID < r.NodeID {
			return true
		}
		if r.NodeID < l.NodeID {
			return false
		}
		if l.Host < r.Host {
			return true
		}
		if r.Host < l.Host {
			return false
		}
		if l.Port < r.Port {
			return true
		}
		if r.Port < l.Port {
			return false
		}
		if l.Rack == nil {
			return true
		}
		if r.Rack == nil {
			return false
		}
		return *l.Rack < *r.Rack
	})
	return resps
}

type shardMerge func([]ResponseShard) (kmsg.Response, error)

func (cl *Client) shardedRequest(ctx context.Context, req kmsg.Request) ([]ResponseShard, shardMerge) {
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	defer close(done)
	go func() {
		defer cancel()
		select {
		case <-done:
		case <-ctx.Done():
		case <-cl.ctx.Done():
		}
	}()

	// First, handle any sharded request. This comes before the conditional
	// below because this handles two group requests, which we do not want
	// to fall into the handleCoordinatorReq logic.
	switch t := req.(type) {
	case *kmsg.ListOffsetsRequest, // key 2
		*kmsg.OffsetFetchRequest,             // key 9
		*kmsg.DescribeGroupsRequest,          // key 15
		*kmsg.ListGroupsRequest,              // key 16
		*kmsg.DeleteRecordsRequest,           // key 21
		*kmsg.OffsetForLeaderEpochRequest,    // key 23
		*kmsg.DescribeConfigsRequest,         // key 32
		*kmsg.AlterConfigsRequest,            // key 33
		*kmsg.AlterReplicaLogDirsRequest,     // key 34
		*kmsg.DescribeLogDirsRequest,         // key 35
		*kmsg.DeleteGroupsRequest,            // key 42
		*kmsg.IncrementalAlterConfigsRequest, // key 44
		*kmsg.DescribeProducersRequest,       // key 61
		*kmsg.DescribeTransactionsRequest,    // key 65
		*kmsg.ListTransactionsRequest:        // key 66
		return cl.handleShardedReq(ctx, req)

	// We support being forward-compatible with FindCoordinator, so we need
	// to use our special hijack function that batches a singular key.
	case *kmsg.FindCoordinatorRequest:
		last, resp, err := cl.findCoordinator(ctx, t)
		return shards(shard(last, req, resp, err)), nil
	}

	switch t := req.(type) {
	case *kmsg.MetadataRequest:
		// We hijack any metadata request so as to populate our
		// own brokers and controller ID.
		br, resp, err := cl.fetchMetadata(ctx, t, false)
		return shards(shard(br, req, resp, err)), nil
	case kmsg.AdminRequest:
		return shards(cl.handleAdminReq(ctx, t)), nil
	case kmsg.GroupCoordinatorRequest,
		kmsg.TxnCoordinatorRequest:
		return shards(cl.handleCoordinatorReq(ctx, t)), nil
	case *kmsg.ApiVersionsRequest:
		// As of v3, software name and version are required.
		// If they are missing, we use the config options.
		if t.ClientSoftwareName == "" && t.ClientSoftwareVersion == "" {
			dup := *t
			dup.ClientSoftwareName = cl.cfg.softwareName
			dup.ClientSoftwareVersion = cl.cfg.softwareVersion
			req = &dup
		}
	}

	// All other requests not handled above can be issued to any broker
	// with the default retriable logic.
	r := cl.retriable()
	resp, err := r.Request(ctx, req)
	return shards(shard(r.last, req, resp, err)), nil
}

func shard(br *broker, req kmsg.Request, resp kmsg.Response, err error) ResponseShard {
	if br == nil { // the broker could be nil if loading the broker failed.
		return ResponseShard{unknownBrokerMetadata, req, resp, err}
	}
	return ResponseShard{br.meta, req, resp, err}
}

func shards(shard ...ResponseShard) []ResponseShard {
	return shard
}

func findBroker(candidates []*broker, node int32) *broker {
	n := sort.Search(len(candidates), func(n int) bool { return candidates[n].meta.NodeID >= node })
	var b *broker
	if n < len(candidates) {
		c := candidates[n]
		if c.meta.NodeID == node {
			b = c
		}
	}
	return b
}

// brokerOrErr returns the broker for ID or the error if the broker does not
// exist.
//
// If tryLoad is true and the broker does not exist, this attempts a broker
// metadata load once before failing. If the metadata load fails, this returns
// that error.
func (cl *Client) brokerOrErr(ctx context.Context, id int32, err error) (*broker, error) {
	if id < 0 {
		return nil, err
	}

	tryLoad := ctx != nil
	tries := 0
start:
	var broker *broker
	if id < 0 {
		broker = findBroker(cl.seeds, id)
	} else {
		cl.brokersMu.RLock()
		broker = findBroker(cl.brokers, id)
		cl.brokersMu.RUnlock()
	}

	if broker == nil {
		if tryLoad {
			if loadErr := cl.fetchBrokerMetadata(ctx); loadErr != nil {
				return nil, loadErr
			}
			// We will retry loading up to two times, if we load broker
			// metadata twice successfully but neither load has the broker
			// we are looking for, then we say our broker does not exist.
			tries++
			if tries < 2 {
				goto start
			}
		}
		return nil, err
	}
	return broker, nil
}

// controller returns the controller broker, forcing a broker load if
// necessary.
func (cl *Client) controller(ctx context.Context) (*broker, error) {
	get := func() int32 {
		cl.controllerIDMu.Lock()
		defer cl.controllerIDMu.Unlock()
		return cl.controllerID
	}

	var id int32
	if id = get(); id < 0 {
		if err := cl.fetchBrokerMetadata(ctx); err != nil {
			return nil, err
		}
		if id = get(); id < 0 {
			return nil, &errUnknownController{id}
		}
	}

	return cl.brokerOrErr(nil, id, &errUnknownController{id})
}

// forgetControllerID is called once an admin requests sees NOT_CONTROLLER.
func (cl *Client) forgetControllerID(id int32) {
	cl.controllerIDMu.Lock()
	defer cl.controllerIDMu.Unlock()

	if cl.controllerID == id {
		cl.controllerID = unknownControllerID
	}
}

const (
	coordinatorTypeGroup int8 = 0
	coordinatorTypeTxn   int8 = 1
)

type coordinatorKey struct {
	name string
	typ  int8
}

type coordinatorLoad struct {
	done chan struct{}
	node int32
	err  error
}

// findCoordinator is allows FindCoordinator request to be forward compatible,
// by duplicating a top level request into a single-element batch request, and
// downconverting the response.
func (cl *Client) findCoordinator(ctx context.Context, req *kmsg.FindCoordinatorRequest) (*broker, *kmsg.FindCoordinatorResponse, error) {
	var compat bool
	if len(req.CoordinatorKeys) == 0 {
		req.CoordinatorKeys = []string{req.CoordinatorKey}
		compat = true
	}
	r := cl.retriable()
	resp, err := req.RequestWith(ctx, r)
	if resp != nil {
		if compat && resp.Version >= 4 {
			if l := len(resp.Coordinators); l != 1 {
				return r.last, resp, fmt.Errorf("unexpectedly received %d coordinators when requesting 1", l)
			}

			first := resp.Coordinators[0]
			resp.ErrorCode = first.ErrorCode
			resp.ErrorMessage = first.ErrorMessage
			resp.NodeID = first.NodeID
			resp.Host = first.Host
			resp.Port = first.Port
		}
	}
	return r.last, resp, err
}

func (cl *Client) deleteStaleCoordinatorIfEqual(key coordinatorKey, current *coordinatorLoad) {
	cl.coordinatorsMu.Lock()
	defer cl.coordinatorsMu.Unlock()
	if existing, ok := cl.coordinators[key]; ok && current == existing {
		delete(cl.coordinators, key)
	}
}

// loadController returns the group/txn coordinator for the given key, retrying
// as necessary. Any non-retriable error does not cache the coordinator.
func (cl *Client) loadCoordinator(ctx context.Context, key coordinatorKey) (*broker, error) {
	var restarted bool
start:
	cl.coordinatorsMu.Lock()
	c, ok := cl.coordinators[key]

	if !ok {
		c = &coordinatorLoad{
			done: make(chan struct{}), // all requests for the same coordinator get collapsed into one
		}
		defer func() {
			// If our load fails, we avoid caching the coordinator,
			// but only if something else has not already replaced
			// our pointer.
			if c.err != nil {
				cl.deleteStaleCoordinatorIfEqual(key, c)
			}
			close(c.done)
		}()
		cl.coordinators[key] = c
	}
	cl.coordinatorsMu.Unlock()

	if ok {
		<-c.done
		if c.err != nil {
			return nil, c.err
		}

		// If brokerOrErr returns an error, then our cached coordinator
		// is using metadata that has updated and removed knowledge of
		// that coordinator. We delete the stale coordinator here and
		// retry once. The retry will force a coordinator reload, and
		// everything will be fresh. Any errors after that we keep.
		b, err := cl.brokerOrErr(nil, c.node, &errUnknownCoordinator{c.node, key})
		if err != nil && !restarted {
			restarted = true
			cl.deleteStaleCoordinatorIfEqual(key, c)
			goto start
		}
		return b, err
	}

	var resp *kmsg.FindCoordinatorResponse
	req := kmsg.NewPtrFindCoordinatorRequest()
	req.CoordinatorKey = key.name
	req.CoordinatorType = key.typ
	_, resp, c.err = cl.findCoordinator(ctx, req)
	if c.err != nil {
		return nil, c.err
	}
	if c.err = kerr.ErrorForCode(resp.ErrorCode); c.err != nil {
		return nil, c.err
	}
	c.node = resp.NodeID

	var b *broker
	b, c.err = cl.brokerOrErr(ctx, c.node, &errUnknownCoordinator{c.node, key})
	return b, c.err
}

func (cl *Client) maybeDeleteStaleCoordinator(name string, typ int8, err error) bool {
	switch {
	case errors.Is(err, kerr.CoordinatorNotAvailable),
		errors.Is(err, kerr.CoordinatorLoadInProgress),
		errors.Is(err, kerr.NotCoordinator):

		cl.coordinatorsMu.Lock()
		delete(cl.coordinators, coordinatorKey{
			name: name,
			typ:  typ,
		})
		cl.coordinatorsMu.Unlock()
		return true
	}
	return false
}

type brokerOrErr struct {
	b   *broker
	err error
}

// loadCoordinators does a concurrent load of many coordinators.
func (cl *Client) loadCoordinators(typ int8, names ...string) map[string]brokerOrErr {
	uniq := make(map[string]struct{})
	for _, name := range names {
		uniq[name] = struct{}{}
	}

	var mu sync.Mutex
	m := make(map[string]brokerOrErr)

	var wg sync.WaitGroup
	for uniqName := range uniq {
		myName := uniqName
		wg.Add(1)
		go func() {
			defer wg.Done()
			coordinator, err := cl.loadCoordinator(cl.ctx, coordinatorKey{
				name: myName,
				typ:  typ,
			})

			mu.Lock()
			defer mu.Unlock()
			m[myName] = brokerOrErr{coordinator, err}
		}()
	}
	wg.Wait()

	return m
}

func (cl *Client) handleAdminReq(ctx context.Context, req kmsg.Request) ResponseShard {
	// Loading a controller can perform some wait; we accept that and do
	// not account for the retries or the time to load the controller as
	// part of the retries / time to issue the req.
	r := cl.retriableBrokerFn(func() (*broker, error) {
		return cl.controller(ctx)
	})

	r.parseRetryErr = func(resp kmsg.Response) error {
		var code int16
		switch t := resp.(type) {
		case *kmsg.CreateTopicsResponse:
			if len(t.Topics) > 0 {
				code = t.Topics[0].ErrorCode
			}
		case *kmsg.DeleteTopicsResponse:
			if len(t.Topics) > 0 {
				code = t.Topics[0].ErrorCode
			}
		case *kmsg.CreatePartitionsResponse:
			if len(t.Topics) > 0 {
				code = t.Topics[0].ErrorCode
			}
		case *kmsg.ElectLeadersResponse:
			if len(t.Topics) > 0 && len(t.Topics[0].Partitions) > 0 {
				code = t.Topics[0].Partitions[0].ErrorCode
			}
		case *kmsg.AlterPartitionAssignmentsResponse:
			code = t.ErrorCode
		case *kmsg.ListPartitionReassignmentsResponse:
			code = t.ErrorCode
		case *kmsg.AlterUserSCRAMCredentialsResponse:
			if len(t.Results) > 0 {
				code = t.Results[0].ErrorCode
			}
		case *kmsg.VoteResponse:
			code = t.ErrorCode
		case *kmsg.BeginQuorumEpochResponse:
			code = t.ErrorCode
		case *kmsg.EndQuorumEpochResponse:
			code = t.ErrorCode
		case *kmsg.DescribeQuorumResponse:
			code = t.ErrorCode
		case *kmsg.AlterISRResponse:
			code = t.ErrorCode
		case *kmsg.UpdateFeaturesResponse:
			code = t.ErrorCode
		case *kmsg.EnvelopeResponse:
			code = t.ErrorCode
		}
		if err := kerr.ErrorForCode(code); errors.Is(err, kerr.NotController) {
			// There must be a last broker if we were able to issue
			// the request and get a response.
			cl.forgetControllerID(r.last.meta.NodeID)
			return err
		}
		return nil
	}

	resp, err := r.Request(ctx, req)
	return shard(r.last, req, resp, err)
}

// handleCoordinatorReq issues simple (non-shardable) group or txn requests.
func (cl *Client) handleCoordinatorReq(ctx context.Context, req kmsg.Request) ResponseShard {
	switch t := req.(type) {
	default:
		// All group requests should be listed below, so if it isn't,
		// then we do not know what this request is.
		return shard(nil, req, nil, errors.New("client is too old; this client does not know what to do with this request"))

	/////////
	// TXN // -- all txn reqs are simple
	/////////

	case *kmsg.InitProducerIDRequest:
		if t.TransactionalID != nil {
			return cl.handleCoordinatorReqSimple(ctx, coordinatorTypeTxn, *t.TransactionalID, req)
		}
		// InitProducerID can go to any broker if the transactional ID
		// is nil. By using handleReqWithCoordinator, we get the
		// retriable-error parsing, even though we are not actually
		// using a defined txn coordinator. This is fine; by passing no
		// names, we delete no coordinator.
		coordinator, resp, err := cl.handleReqWithCoordinator(ctx, func() (*broker, error) { return cl.broker(), nil }, coordinatorTypeTxn, "", req)
		return shard(coordinator, req, resp, err)
	case *kmsg.AddPartitionsToTxnRequest:
		return cl.handleCoordinatorReqSimple(ctx, coordinatorTypeTxn, t.TransactionalID, req)
	case *kmsg.AddOffsetsToTxnRequest:
		return cl.handleCoordinatorReqSimple(ctx, coordinatorTypeTxn, t.TransactionalID, req)
	case *kmsg.EndTxnRequest:
		return cl.handleCoordinatorReqSimple(ctx, coordinatorTypeTxn, t.TransactionalID, req)

	///////////
	// GROUP // -- most group reqs are simple
	///////////

	case *kmsg.OffsetCommitRequest:
		return cl.handleCoordinatorReqSimple(ctx, coordinatorTypeGroup, t.Group, req)
	case *kmsg.TxnOffsetCommitRequest:
		return cl.handleCoordinatorReqSimple(ctx, coordinatorTypeGroup, t.Group, req)
	case *kmsg.JoinGroupRequest:
		return cl.handleCoordinatorReqSimple(ctx, coordinatorTypeGroup, t.Group, req)
	case *kmsg.HeartbeatRequest:
		return cl.handleCoordinatorReqSimple(ctx, coordinatorTypeGroup, t.Group, req)
	case *kmsg.LeaveGroupRequest:
		return cl.handleCoordinatorReqSimple(ctx, coordinatorTypeGroup, t.Group, req)
	case *kmsg.SyncGroupRequest:
		return cl.handleCoordinatorReqSimple(ctx, coordinatorTypeGroup, t.Group, req)
	case *kmsg.OffsetDeleteRequest:
		return cl.handleCoordinatorReqSimple(ctx, coordinatorTypeGroup, t.Group, req)
	}
}

// handleCoordinatorReqSimple issues a request that contains a single group or
// txn to its coordinator.
//
// The error is inspected to see if it is a retriable error and, if so, the
// coordinator is deleted.
func (cl *Client) handleCoordinatorReqSimple(ctx context.Context, typ int8, name string, req kmsg.Request) ResponseShard {
	coordinator, resp, err := cl.handleReqWithCoordinator(ctx, func() (*broker, error) {
		return cl.loadCoordinator(ctx, coordinatorKey{
			name: name,
			typ:  typ,
		})
	}, typ, name, req)
	return shard(coordinator, req, resp, err)
}

// handleReqWithCoordinator actually issues a request to a coordinator and
// does retry handling.
//
// This avoids retries on the two group requests that need to be sharded.
func (cl *Client) handleReqWithCoordinator(
	ctx context.Context,
	coordinator func() (*broker, error),
	typ int8,
	name string, // group ID or the transactional id
	req kmsg.Request,
) (*broker, kmsg.Response, error) {
	r := cl.retriableBrokerFn(coordinator)
	r.parseRetryErr = func(resp kmsg.Response) error {
		var code int16
		switch t := resp.(type) {
		// TXN
		case *kmsg.InitProducerIDResponse:
			code = t.ErrorCode
		case *kmsg.AddPartitionsToTxnResponse:
			if len(t.Topics) > 0 && len(t.Topics[0].Partitions) > 0 {
				code = t.Topics[0].Partitions[0].ErrorCode
			}
		case *kmsg.AddOffsetsToTxnResponse:
			code = t.ErrorCode
		case *kmsg.EndTxnResponse:
			code = t.ErrorCode

		// GROUP
		case *kmsg.OffsetCommitResponse:
			if len(t.Topics) > 0 && len(t.Topics[0].Partitions) > 0 {
				code = t.Topics[0].Partitions[0].ErrorCode
			}
		case *kmsg.TxnOffsetCommitResponse:
			if len(t.Topics) > 0 && len(t.Topics[0].Partitions) > 0 {
				code = t.Topics[0].Partitions[0].ErrorCode
			}
		case *kmsg.JoinGroupResponse:
			code = t.ErrorCode
		case *kmsg.HeartbeatResponse:
			code = t.ErrorCode
		case *kmsg.LeaveGroupResponse:
			code = t.ErrorCode
		case *kmsg.SyncGroupResponse:
			code = t.ErrorCode
		}

		// ListGroups, OffsetFetch, DeleteGroups, DescribeGroups, and
		// DescribeTransactions handled in sharding.

		if err := kerr.ErrorForCode(code); cl.maybeDeleteStaleCoordinator(name, typ, err) {
			return err
		}
		return nil
	}

	resp, err := r.Request(ctx, req)
	return r.last, resp, err
}

// Broker returns a handle to a specific broker to directly issue requests to.
// Note that there is no guarantee that this broker exists; if it does not,
// requests will fail with with an unknown broker error.
func (cl *Client) Broker(id int) *Broker {
	return &Broker{
		id: int32(id),
		cl: cl,
	}
}

// DiscoveredBrokers returns all brokers that were discovered from prior
// metadata responses. This does not actually issue a metadata request to load
// brokers; if you wish to ensure this returns all brokers, be sure to manually
// issue a metadata request before this. This also does not include seed
// brokers, which are internally saved under special internal broker IDs (but,
// it does include those brokers under their normal IDs as returned from a
// metadata response).
func (cl *Client) DiscoveredBrokers() []*Broker {
	cl.brokersMu.RLock()
	defer cl.brokersMu.RUnlock()

	var bs []*Broker
	for _, broker := range cl.brokers {
		bs = append(bs, &Broker{id: broker.meta.NodeID, cl: cl})
	}
	return bs
}

// SeedBrokers returns the all seed brokers.
func (cl *Client) SeedBrokers() []*Broker {
	var bs []*Broker
	for _, broker := range cl.seeds {
		bs = append(bs, &Broker{id: broker.meta.NodeID, cl: cl})
	}
	return bs
}

// Broker pairs a broker ID with a client to directly issue requests to a
// specific broker.
type Broker struct {
	id int32
	cl *Client
}

// Request issues a request to a broker. If the broker does not exist in the
// client, this returns an unknown broker error. Requests are not retried.
//
// The passed context can be used to cancel a request and return early.
// Note that if the request is not canceled before it is written to Kafka,
// you may just end up canceling and not receiving the response to what Kafka
// inevitably does.
//
// It is more beneficial to always use RetriableRequest.
func (b *Broker) Request(ctx context.Context, req kmsg.Request) (kmsg.Response, error) {
	return b.request(ctx, false, req)
}

// RetriableRequest issues a request to a broker the same as Broker, but
// retries in the face of retriable broker connection errors. This does not
// retry on response internal errors.
func (b *Broker) RetriableRequest(ctx context.Context, req kmsg.Request) (kmsg.Response, error) {
	return b.request(ctx, true, req)
}

func (b *Broker) request(ctx context.Context, retry bool, req kmsg.Request) (kmsg.Response, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var resp kmsg.Response
	var err error
	done := make(chan struct{})

	go func() {
		defer close(done)

		if !retry {
			var br *broker
			br, err = b.cl.brokerOrErr(ctx, b.id, errUnknownBroker)
			if err == nil {
				resp, err = br.waitResp(ctx, req)
			}
		} else {
			resp, err = b.cl.retriableBrokerFn(func() (*broker, error) {
				return b.cl.brokerOrErr(ctx, b.id, errUnknownBroker)
			}).Request(ctx, req)
		}
	}()

	select {
	case <-done:
		return resp, err
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-b.cl.ctx.Done():
		return nil, b.cl.ctx.Err()
	}
}

//////////////////////
// REQUEST SHARDING //
//////////////////////

// Below here lies all logic to handle requests that need to be split and sent
// to many brokers. A lot of the logic for each sharding function is very
// similar, but each sharding function uses slightly different types.

// issueShard is a request that has been split and is ready to be sent to the
// given broker ID.
type issueShard struct {
	req    kmsg.Request
	broker int32
	any    bool

	// if non-nil, we could not map this request shard to any broker, and
	// this error is the reason.
	err error
}

// sharder splits a request.
type sharder interface {
	// shard splits a request and returns the requests to issue tied to the
	// brokers to issue the requests to. This can return an error if there
	// is some pre-loading that needs to happen. If an error is returned,
	// the request that was intended for splitting is failed wholesale.
	//
	// Due to sharded requests not being retriable if a response is
	// received, to avoid stale coordinator errors, this function should
	// not use any previously cached metadata.
	shard(context.Context, kmsg.Request) ([]issueShard, bool, error)

	// onResp is called on a successful response to investigate the
	// response and potentially perform cleanup, and potentially returns an
	// error signifying to retry.
	//
	// We consider a request entirely retriable if there is at least one
	// retriable error, and all other errors are retriable or not an error.
	// Any non-retriable error makes the request un-retriable.
	//
	// Generally we only perform this logic for group requests, because for
	// non-group requests (topic / partition based requests), we load
	// metadata immediately before issuing the request and thus we expect
	// how we originally mapped the request to still be valid. For group
	// requests, we use cached coordinators, so we may receive not
	// coordinator errors once, after which we will delete the stale
	// coordinator and remap.
	//
	// The most thorough approach would be to split the original request
	// into retriable pieces and unretriable pieces, but this gets complicated
	// fast. We would have to:
	//   - pair all request partitions to the response partition (maybe the
	//     response is missing some pieces because of a buggy kafka)
	//   - split non-retriable pieces of the request & response:
	//     - any missing response pieces have a request piece that is not
	//       retriable
	//     - any matching piece can be retriable if the response piece err
	//       is retriable
	//   - return the non-retriable request & response piece, and the retriable
	//     request piece and err.
	onResp(kmsg.Request, kmsg.Response) error

	// merge is a function that can be used to merge sharded responses into
	// one response. This is used by the client.Request method.
	merge([]ResponseShard) (kmsg.Response, error)
}

// handleShardedReq splits and issues requests to brokers, recursively
// splitting as necessary if requests fail and need remapping.
func (cl *Client) handleShardedReq(ctx context.Context, req kmsg.Request) ([]ResponseShard, shardMerge) {
	// First, determine our sharder.
	var sharder sharder
	switch req.(type) {
	case *kmsg.ListOffsetsRequest:
		sharder = &listOffsetsSharder{cl}
	case *kmsg.OffsetFetchRequest:
		sharder = &offsetFetchSharder{cl}
	case *kmsg.DescribeGroupsRequest:
		sharder = &describeGroupsSharder{cl}
	case *kmsg.ListGroupsRequest:
		sharder = &listGroupsSharder{cl}
	case *kmsg.DeleteRecordsRequest:
		sharder = &deleteRecordsSharder{cl}
	case *kmsg.OffsetForLeaderEpochRequest:
		sharder = &offsetForLeaderEpochSharder{cl}
	case *kmsg.DescribeConfigsRequest:
		sharder = &describeConfigsSharder{cl}
	case *kmsg.AlterConfigsRequest:
		sharder = &alterConfigsSharder{cl}
	case *kmsg.AlterReplicaLogDirsRequest:
		sharder = &alterReplicaLogDirsSharder{cl}
	case *kmsg.DescribeLogDirsRequest:
		sharder = &describeLogDirsSharder{cl}
	case *kmsg.DeleteGroupsRequest:
		sharder = &deleteGroupsSharder{cl}
	case *kmsg.IncrementalAlterConfigsRequest:
		sharder = &incrementalAlterConfigsSharder{cl}
	case *kmsg.DescribeProducersRequest:
		sharder = &describeProducersSharder{cl}
	case *kmsg.DescribeTransactionsRequest:
		sharder = &describeTransactionsSharder{cl}
	case *kmsg.ListTransactionsRequest:
		sharder = &listTransactionsSharder{cl}
	}

	// If a request fails, we re-shard it (in case it needs to be split
	// again). reqTry tracks how many total tries a request piece has had;
	// we quit at either the max configured tries or max configured time.
	type reqTry struct {
		tries int
		req   kmsg.Request
	}

	var (
		shardsMu sync.Mutex
		shards   []ResponseShard

		addShard = func(shard ResponseShard) {
			shardsMu.Lock()
			defer shardsMu.Unlock()
			shards = append(shards, shard)
		}

		start        = time.Now()
		retryTimeout = cl.cfg.retryTimeout(req.Key())

		wg    sync.WaitGroup
		issue func(reqTry)
	)

	l := cl.cfg.logger
	debug := l.Level() >= LogLevelDebug

	// issue is called to progressively split and issue requests.
	//
	// This recursively calls itself if a request fails and can be retried.
	issue = func(try reqTry) {
		issues, reshardable, err := sharder.shard(ctx, try.req)
		if err != nil {
			l.Log(LogLevelDebug, "unable to shard request", "previous_tries", try.tries, "err", err)
			addShard(shard(nil, try.req, nil, err)) // failure to shard means data loading failed; this request is failed
			return
		}

		// If the request actually does not need to be issued, we issue
		// it to a random broker. There is no benefit to this, but at
		// least we will return one shard.
		if len(issues) == 0 {
			issues = []issueShard{{
				req: try.req,
				any: true,
			}}
			reshardable = true
		}

		if debug {
			var brokerAnys []string
			for _, issue := range issues {
				if issue.err != nil {
					brokerAnys = append(brokerAnys, "err")
				} else if issue.any {
					brokerAnys = append(brokerAnys, "any")
				} else {
					brokerAnys = append(brokerAnys, fmt.Sprintf("%d", issue.broker))
				}
			}
			l.Log(LogLevelDebug, "sharded request", "destinations", brokerAnys)
		}

		for i := range issues {
			myIssue := issues[i]

			if myIssue.err != nil {
				addShard(shard(nil, myIssue.req, nil, myIssue.err))
				continue
			}

			tries := try.tries
			wg.Add(1)
			go func() {
				defer wg.Done()
			start:
				tries++

				broker := cl.broker()
				var err error
				if !myIssue.any {
					broker, err = cl.brokerOrErr(ctx, myIssue.broker, errUnknownBroker)
				}
				if err != nil {
					addShard(shard(nil, myIssue.req, nil, err)) // failure to load a broker is a failure to issue a request
					return
				}

				resp, err := broker.waitResp(ctx, myIssue.req)
				if err == nil {
					err = sharder.onResp(myIssue.req, resp) // perform some potential cleanup, and potentially receive an error to retry
				}

				// If we failed to issue the request, we *maybe* will retry.
				// We could have failed to even issue the request or receive
				// a response, which is retriable.
				backoff := cl.cfg.retryBackoff(tries)
				if err != nil && (retryTimeout == 0 || time.Now().Add(backoff).Sub(start) < retryTimeout) && cl.shouldRetry(tries, err) && cl.waitTries(ctx, backoff) {
					// Non-reshardable re-requests just jump back to the
					// top where the broker is loaded. This is the case on
					// requests where the original request is split to
					// dedicated brokers; we do not want to re-shard that.
					if !reshardable {
						l.Log(LogLevelDebug, "sharded request failed, reissuing without resharding", "time_since_start", time.Since(start), "tries", try.tries, "err", err)
						goto start
					}
					l.Log(LogLevelDebug, "sharded request failed, resharding and reissuing", "time_since_start", time.Since(start), "tries", try.tries, "err", err)
					issue(reqTry{tries, myIssue.req})
					return
				}

				addShard(shard(broker, myIssue.req, resp, err)) // the error was not retriable
			}()
		}
	}

	issue(reqTry{0, req})
	wg.Wait()

	return shards, sharder.merge
}

// Sets the error any amount of times to a retriable error, but sets to a
// non-retriable error once.
func onRespShardErr(err *error, newKerr error) {
	if newKerr == nil || *err != nil && !kerr.IsRetriable(*err) {
		return
	}
	*err = newKerr
}

// a convenience function for when a request needs to be issued identically to
// all brokers.
func (cl *Client) allBrokersShardedReq(ctx context.Context, fn func() kmsg.Request) ([]issueShard, bool, error) {
	if err := cl.fetchBrokerMetadata(ctx); err != nil {
		return nil, false, err
	}

	var issues []issueShard
	cl.brokersMu.RLock()
	for _, broker := range cl.brokers {
		issues = append(issues, issueShard{
			req:    fn(),
			broker: broker.meta.NodeID,
		})
	}
	cl.brokersMu.RUnlock()

	return issues, false, nil // we do NOT re-shard these requests request
}

// a convenience function for saving the first ResponseShard error.
func firstErrMerger(sresps []ResponseShard, merge func(kresp kmsg.Response)) error {
	var firstErr error
	for _, sresp := range sresps {
		if sresp.Err != nil {
			if firstErr == nil {
				firstErr = sresp.Err
			}
			continue
		}
		merge(sresp.Resp)
	}
	return firstErr
}

type mappedMetadataTopic struct {
	topic   kmsg.MetadataResponseTopic
	mapping map[int32]kmsg.MetadataResponseTopicPartition
}

// fetchMappedMetadata provides a convenience type of working with metadata;
// this is garbage heavy, so it is only used in one off requests in this
// package.
func (cl *Client) fetchMappedMetadata(ctx context.Context, topics []string) (map[string]mappedMetadataTopic, error) {
	_, meta, err := cl.fetchMetadataForTopics(ctx, false, topics)
	if err != nil {
		return nil, err
	}
	mapping := make(map[string]mappedMetadataTopic)
	for _, topic := range meta.Topics {
		if topic.Topic == nil {
			// We do not request with topic IDs, so we should not
			// receive topic IDs in the response.
			continue
		}
		t := mappedMetadataTopic{
			topic:   topic,
			mapping: make(map[int32]kmsg.MetadataResponseTopicPartition),
		}
		mapping[*topic.Topic] = t
		for _, partition := range topic.Partitions {
			t.mapping[partition.Partition] = partition
		}
	}
	return mapping, nil
}

// These errors pair with "collect" below for wording.
var (
	errMissingTopic     = errors.New("topic was not returned when looking up its metadata")
	errMissingPartition = errors.New("partition was not returned when looking up its metadata")
	errNoLeader         = errors.New("partition has no leader from metadata lookup")
)

func missingOrCodeT(exists bool, code int16) error {
	if !exists {
		return errMissingTopic
	}
	return kerr.ErrorForCode(code)
}

func missingOrCodeP(exists bool, code int16) error {
	if !exists {
		return errMissingPartition
	}
	return kerr.ErrorForCode(code)
}

func noLeader(l int32) error {
	if l < 0 {
		return errNoLeader
	}
	return nil
}

// This is a helper for the sharded requests below; if mapping metadata fails
// to load topics or partitions, we group the failures by error.
//
// We use a lot of reflect magic to make the actual usage much nicer.
type unknownErrShards struct {
	// load err => topic => mystery slice type
	//
	// The mystery type is basically just []Partition, where Partition can
	// be any kmsg type.
	mapped map[error]map[string]reflect.Value
}

// err stores a new failing partition with its failing error.
//
// partition's type is equal to the arg1 type of l.fn.
func (l *unknownErrShards) err(err error, topic string, partition interface{}) {
	if l.mapped == nil {
		l.mapped = make(map[error]map[string]reflect.Value)
	}
	t := l.mapped[err]
	if t == nil {
		t = make(map[string]reflect.Value)
		l.mapped[err] = t
	}
	slice, ok := t[topic]
	if !ok {
		// We make a slice of the input partition type.
		slice = reflect.MakeSlice(reflect.SliceOf(reflect.TypeOf(partition)), 0, 1)
	}

	t[topic] = reflect.Append(slice, reflect.ValueOf(partition))
}

// errs takes an input slice of partitions and stores each with its failing
// error.
//
// partitions is a slice where each element has type of arg1 of l.fn.
func (l *unknownErrShards) errs(err error, topic string, partitions interface{}) {
	v := reflect.ValueOf(partitions)
	for i := 0; i < v.Len(); i++ {
		l.err(err, topic, v.Index(i).Interface())
	}
}

// Returns issueShards for each error stored in l.
//
// This takes a factory function: the first return is a new kmsg.Request, the
// second is a function that adds a topic and its partitions to that request.
//
// Thus, fn is of type func() (kmsg.Request, func(string, []P))
func (l *unknownErrShards) collect(mkreq, mergeParts interface{}) []issueShard {
	if len(l.mapped) == 0 {
		return nil
	}

	var shards []issueShard

	factory := reflect.ValueOf(mkreq)
	perTopic := reflect.ValueOf(mergeParts)
	for err, topics := range l.mapped {
		req := factory.Call(nil)[0]

		var ntopics, npartitions int
		for topic, partitions := range topics {
			ntopics++
			npartitions += partitions.Len()
			perTopic.Call([]reflect.Value{req, reflect.ValueOf(topic), partitions})
		}

		switch {
		case errors.Is(err, errMissingTopic):
			if ntopics == 1 {
				err = errors.New("1 topic was not returned when lookup up its metadata")
			} else if ntopics > 1 {
				err = fmt.Errorf("%d topics were not returned when lookup up their metadata", ntopics)
			}
		case errors.Is(err, errMissingPartition):
			if npartitions == 1 {
				err = errors.New("1 partition was not returned when looking up its metadata")
			} else if npartitions > 1 {
				err = fmt.Errorf("%d partitions were not returned when looking up their metadata", npartitions)
			}
		case errors.Is(err, errNoLeader):
			if npartitions == 1 {
				err = errors.New("1 partition has no leader from a metadata lookup")
			} else if npartitions > 1 {
				err = fmt.Errorf("%d partitions have no leader from a metadata lookup", npartitions)
			}
		}

		shards = append(shards, issueShard{
			req: req.Interface().(kmsg.Request),
			err: err,
		})
	}

	return shards
}

// handles sharding ListOffsetsRequest
type listOffsetsSharder struct{ *Client }

func (cl *listOffsetsSharder) shard(ctx context.Context, kreq kmsg.Request) ([]issueShard, bool, error) {
	req := kreq.(*kmsg.ListOffsetsRequest)

	// For listing offsets, we need the broker leader for each partition we
	// are listing. Thus, we first load metadata for the topics.
	//
	// Metadata loading performs retries; if we fail here, the we do not
	// issue sharded requests.
	var need []string
	for _, topic := range req.Topics {
		need = append(need, topic.Topic)
	}
	mapping, err := cl.fetchMappedMetadata(ctx, need)
	if err != nil {
		return nil, false, err
	}

	brokerReqs := make(map[int32]map[string][]kmsg.ListOffsetsRequestTopicPartition)
	var unknowns unknownErrShards

	// For any topic or partition that had an error load, we blindly issue
	// a load to the first seed broker. We expect the list to fail, but it
	// is the best we could do.
	for _, topic := range req.Topics {
		t := topic.Topic
		tmapping, exists := mapping[t]
		if err := missingOrCodeT(exists, tmapping.topic.ErrorCode); err != nil {
			unknowns.errs(err, t, topic.Partitions)
			continue
		}
		for _, partition := range topic.Partitions {
			p, exists := tmapping.mapping[partition.Partition]
			if err := missingOrCodeP(exists, p.ErrorCode); err != nil {
				unknowns.err(err, t, partition)
				continue
			}
			if err := noLeader(p.Leader); err != nil {
				unknowns.err(err, t, partition)
				continue
			}

			brokerReq := brokerReqs[p.Leader]
			if brokerReq == nil {
				brokerReq = make(map[string][]kmsg.ListOffsetsRequestTopicPartition)
				brokerReqs[p.Leader] = brokerReq
			}
			brokerReq[t] = append(brokerReq[t], partition)
		}
	}

	mkreq := func() *kmsg.ListOffsetsRequest {
		r := kmsg.NewPtrListOffsetsRequest()
		r.ReplicaID = req.ReplicaID
		r.IsolationLevel = req.IsolationLevel
		return r
	}

	var issues []issueShard
	for brokerID, brokerReq := range brokerReqs {
		req := mkreq()
		for topic, parts := range brokerReq {
			reqTopic := kmsg.NewListOffsetsRequestTopic()
			reqTopic.Topic = topic
			reqTopic.Partitions = parts
			req.Topics = append(req.Topics, reqTopic)
		}
		issues = append(issues, issueShard{
			req:    req,
			broker: brokerID,
		})
	}

	return append(issues, unknowns.collect(mkreq, func(r *kmsg.ListOffsetsRequest, topic string, parts []kmsg.ListOffsetsRequestTopicPartition) {
		reqTopic := kmsg.NewListOffsetsRequestTopic()
		reqTopic.Topic = topic
		reqTopic.Partitions = parts
		r.Topics = append(r.Topics, reqTopic)
	})...), true, nil // this is reshardable
}

func (*listOffsetsSharder) onResp(kmsg.Request, kmsg.Response) error { return nil } // topic / partitions: not retried

func (*listOffsetsSharder) merge(sresps []ResponseShard) (kmsg.Response, error) {
	merged := kmsg.NewPtrListOffsetsResponse()
	topics := make(map[string][]kmsg.ListOffsetsResponseTopicPartition)

	firstErr := firstErrMerger(sresps, func(kresp kmsg.Response) {
		resp := kresp.(*kmsg.ListOffsetsResponse)
		merged.Version = resp.Version
		merged.ThrottleMillis = resp.ThrottleMillis

		for _, topic := range resp.Topics {
			topics[topic.Topic] = append(topics[topic.Topic], topic.Partitions...)
		}
	})
	for topic, partitions := range topics {
		respTopic := kmsg.NewListOffsetsResponseTopic()
		respTopic.Topic = topic
		respTopic.Partitions = partitions
		merged.Topics = append(merged.Topics, respTopic)
	}
	return merged, firstErr
}

// handles sharding OffsetFetchRequest
type offsetFetchSharder struct{ *Client }

func offsetFetchReqToGroup(req *kmsg.OffsetFetchRequest) kmsg.OffsetFetchRequestGroup {
	g := kmsg.NewOffsetFetchRequestGroup()
	g.Group = req.Group
	for _, topic := range req.Topics {
		reqTopic := kmsg.NewOffsetFetchRequestGroupTopic()
		reqTopic.Topic = topic.Topic
		reqTopic.Partitions = topic.Partitions
		g.Topics = append(g.Topics, reqTopic)
	}
	return g
}

func offsetFetchRespToGroup(req *kmsg.OffsetFetchRequest, resp *kmsg.OffsetFetchResponse) kmsg.OffsetFetchResponseGroup {
	g := kmsg.NewOffsetFetchResponseGroup()
	g.Group = req.Group
	g.ErrorCode = resp.ErrorCode
	for _, topic := range resp.Topics {
		t := kmsg.NewOffsetFetchResponseGroupTopic()
		t.Topic = topic.Topic
		for _, partition := range topic.Partitions {
			p := kmsg.NewOffsetFetchResponseGroupTopicPartition()
			p.Partition = partition.Partition
			p.Offset = partition.Offset
			p.LeaderEpoch = partition.LeaderEpoch
			p.Metadata = partition.Metadata
			p.ErrorCode = partition.ErrorCode
			t.Partitions = append(t.Partitions, p)
		}
		g.Topics = append(g.Topics, t)
	}
	return g
}

func offsetFetchRespGroupIntoResp(g kmsg.OffsetFetchResponseGroup, into *kmsg.OffsetFetchResponse) {
	into.ErrorCode = g.ErrorCode
	into.Topics = into.Topics[:0]
	for _, topic := range g.Topics {
		t := kmsg.NewOffsetFetchResponseTopic()
		t.Topic = topic.Topic
		for _, partition := range topic.Partitions {
			p := kmsg.NewOffsetFetchResponseTopicPartition()
			p.Partition = partition.Partition
			p.Offset = partition.Offset
			p.LeaderEpoch = partition.LeaderEpoch
			p.Metadata = partition.Metadata
			p.ErrorCode = partition.ErrorCode
			t.Partitions = append(t.Partitions, p)
		}
		into.Topics = append(into.Topics, t)
	}
}

func (cl *offsetFetchSharder) shard(_ context.Context, kreq kmsg.Request) ([]issueShard, bool, error) {
	req := kreq.(*kmsg.OffsetFetchRequest)

	groups := make([]string, 0, len(req.Groups)+1)
	if len(req.Groups) == 0 { // v0-v7
		groups = append(groups, req.Group)
	}
	for i := range req.Groups { // v8+
		groups = append(groups, req.Groups[i].Group)
	}

	coordinators := cl.loadCoordinators(coordinatorTypeGroup, groups...)

	// If there is only the top level group, then we simply return our
	// request mapped to its specific broker. For forward compatibility, we
	// also embed the top level request into the Groups list: this allows
	// operators of old request versions (v0-v7) to issue a v8 request
	// appropriately. On response, if the length of groups is 1, we merge
	// the first item back to the top level.
	if len(req.Groups) == 0 {
		berr := coordinators[req.Group]
		if berr.err != nil {
			return []issueShard{{
				req: req,
				err: berr.err,
			}}, false, nil // not reshardable, because this is an error
		}

		dup := *req
		brokerReq := &dup
		brokerReq.Groups = append(brokerReq.Groups, offsetFetchReqToGroup(req))

		return []issueShard{{
			req:    brokerReq,
			broker: berr.b.meta.NodeID,
		}}, false, nil // reshardable to reload correct coordinator
	}

	// v8+ behavior: we have multiple groups.
	//
	// Loading coordinators can have each group fail with its unique error,
	// or with a kerr.Error that can be merged. Unique errors get their own
	// failure shard, while kerr.Error's get merged.
	type unkerr struct {
		err   error
		group kmsg.OffsetFetchRequestGroup
	}
	var (
		brokerReqs = make(map[int32]*kmsg.OffsetFetchRequest)
		kerrs      = make(map[*kerr.Error][]kmsg.OffsetFetchRequestGroup)
		unkerrs    []unkerr
	)

	newReq := func(groups ...kmsg.OffsetFetchRequestGroup) *kmsg.OffsetFetchRequest {
		newReq := kmsg.NewPtrOffsetFetchRequest()
		newReq.RequireStable = req.RequireStable
		newReq.Groups = groups
		return newReq
	}

	for _, group := range req.Groups {
		berr := coordinators[group.Group]
		var ke *kerr.Error
		switch {
		case berr.err == nil:
			brokerReq := brokerReqs[berr.b.meta.NodeID]
			if brokerReq == nil {
				brokerReq = newReq()
				brokerReqs[berr.b.meta.NodeID] = brokerReq
			}
			brokerReq.Groups = append(brokerReq.Groups, group)
		case errors.As(berr.err, &ke):
			kerrs[ke] = append(kerrs[ke], group)
		default:
			unkerrs = append(unkerrs, unkerr{berr.err, group})
		}
	}

	var issues []issueShard
	for id, req := range brokerReqs {
		issues = append(issues, issueShard{
			req:    req,
			broker: id,
		})
	}
	for _, unkerr := range unkerrs {
		issues = append(issues, issueShard{
			req: newReq(unkerr.group),
			err: unkerr.err,
		})
	}
	for kerr, groups := range kerrs {
		issues = append(issues, issueShard{
			req: newReq(groups...),
			err: kerr,
		})
	}

	return issues, true, nil // reshardable to load correct coordinators
}

func (cl *offsetFetchSharder) onResp(kreq kmsg.Request, kresp kmsg.Response) error {
	req := kreq.(*kmsg.OffsetFetchRequest)
	resp := kresp.(*kmsg.OffsetFetchResponse)

	switch len(resp.Groups) {
	case 0:
		resp.Groups = append(resp.Groups, offsetFetchRespToGroup(req, resp))
	case 1:
		offsetFetchRespGroupIntoResp(resp.Groups[0], resp)
	default:
	}

	var retErr error
	for i := range resp.Groups {
		group := &resp.Groups[i]
		err := kerr.ErrorForCode(group.ErrorCode)
		cl.maybeDeleteStaleCoordinator(group.Group, coordinatorTypeGroup, err)
		onRespShardErr(&retErr, err)
	}

	// For a final bit of extra fun, v0 and v1 do not have a top level
	// error code but instead a per-partition error code. If the
	// coordinator is loading &c, then all per-partition error codes are
	// the same so we only need to look at the first partition.
	if resp.Version < 2 && len(resp.Topics) > 0 && len(resp.Topics[0].Partitions) > 0 {
		code := resp.Topics[0].Partitions[0].ErrorCode
		err := kerr.ErrorForCode(code)
		cl.maybeDeleteStaleCoordinator(req.Group, coordinatorTypeGroup, err)
		onRespShardErr(&retErr, err)
	}

	return retErr
}

func (*offsetFetchSharder) merge(sresps []ResponseShard) (kmsg.Response, error) {
	merged := kmsg.NewPtrOffsetFetchResponse()
	return merged, firstErrMerger(sresps, func(kresp kmsg.Response) {
		resp := kresp.(*kmsg.OffsetFetchResponse)
		merged.Version = resp.Version
		merged.ThrottleMillis = resp.ThrottleMillis
		merged.Groups = append(merged.Groups, resp.Groups...)

		if len(resp.Groups) == 1 {
			offsetFetchRespGroupIntoResp(resp.Groups[0], merged)
		}
	})
}

// handles sharding DescribeGroupsRequest
type describeGroupsSharder struct{ *Client }

func (cl *describeGroupsSharder) shard(_ context.Context, kreq kmsg.Request) ([]issueShard, bool, error) {
	req := kreq.(*kmsg.DescribeGroupsRequest)

	coordinators := cl.loadCoordinators(coordinatorTypeGroup, req.Groups...)
	type unkerr struct {
		err   error
		group string
	}
	var (
		brokerReqs = make(map[int32]*kmsg.DescribeGroupsRequest)
		kerrs      = make(map[*kerr.Error][]string)
		unkerrs    []unkerr
	)

	newReq := func(groups ...string) *kmsg.DescribeGroupsRequest {
		newReq := kmsg.NewPtrDescribeGroupsRequest()
		newReq.IncludeAuthorizedOperations = req.IncludeAuthorizedOperations
		newReq.Groups = groups
		return newReq
	}

	for _, group := range req.Groups {
		berr := coordinators[group]
		var ke *kerr.Error
		switch {
		case berr.err == nil:
			brokerReq := brokerReqs[berr.b.meta.NodeID]
			if brokerReq == nil {
				brokerReq = newReq()
				brokerReqs[berr.b.meta.NodeID] = brokerReq
			}
			brokerReq.Groups = append(brokerReq.Groups, group)
		case errors.As(berr.err, &ke):
			kerrs[ke] = append(kerrs[ke], group)
		default:
			unkerrs = append(unkerrs, unkerr{berr.err, group})
		}
	}

	var issues []issueShard
	for id, req := range brokerReqs {
		issues = append(issues, issueShard{
			req:    req,
			broker: id,
		})
	}
	for _, unkerr := range unkerrs {
		issues = append(issues, issueShard{
			req: newReq(unkerr.group),
			err: unkerr.err,
		})
	}
	for kerr, groups := range kerrs {
		issues = append(issues, issueShard{
			req: newReq(groups...),
			err: kerr,
		})
	}

	return issues, true, nil // reshardable to load correct coordinators
}

func (cl *describeGroupsSharder) onResp(_ kmsg.Request, kresp kmsg.Response) error { // cleanup any stale groups
	resp := kresp.(*kmsg.DescribeGroupsResponse)
	var retErr error
	for i := range resp.Groups {
		group := &resp.Groups[i]
		err := kerr.ErrorForCode(group.ErrorCode)
		cl.maybeDeleteStaleCoordinator(group.Group, coordinatorTypeGroup, err)
		onRespShardErr(&retErr, err)
	}
	return retErr
}

func (*describeGroupsSharder) merge(sresps []ResponseShard) (kmsg.Response, error) {
	merged := kmsg.NewPtrDescribeGroupsResponse()
	return merged, firstErrMerger(sresps, func(kresp kmsg.Response) {
		resp := kresp.(*kmsg.DescribeGroupsResponse)
		merged.Version = resp.Version
		merged.ThrottleMillis = resp.ThrottleMillis
		merged.Groups = append(merged.Groups, resp.Groups...)
	})
}

// handles sharding ListGroupsRequest
type listGroupsSharder struct{ *Client }

func (cl *listGroupsSharder) shard(ctx context.Context, kreq kmsg.Request) ([]issueShard, bool, error) {
	req := kreq.(*kmsg.ListGroupsRequest)
	return cl.allBrokersShardedReq(ctx, func() kmsg.Request {
		dup := *req
		return &dup
	})
}

func (*listGroupsSharder) onResp(_ kmsg.Request, kresp kmsg.Response) error {
	resp := kresp.(*kmsg.ListGroupsResponse)
	return kerr.ErrorForCode(resp.ErrorCode)
}

func (*listGroupsSharder) merge(sresps []ResponseShard) (kmsg.Response, error) {
	merged := kmsg.NewPtrListGroupsResponse()
	return merged, firstErrMerger(sresps, func(kresp kmsg.Response) {
		resp := kresp.(*kmsg.ListGroupsResponse)
		merged.Version = resp.Version
		merged.ThrottleMillis = resp.ThrottleMillis
		if merged.ErrorCode == 0 {
			merged.ErrorCode = resp.ErrorCode
		}
		merged.Groups = append(merged.Groups, resp.Groups...)
	})
}

// handle sharding DeleteRecordsRequest
type deleteRecordsSharder struct{ *Client }

func (cl *deleteRecordsSharder) shard(ctx context.Context, kreq kmsg.Request) ([]issueShard, bool, error) {
	req := kreq.(*kmsg.DeleteRecordsRequest)

	var need []string
	for _, topic := range req.Topics {
		need = append(need, topic.Topic)
	}
	mapping, err := cl.fetchMappedMetadata(ctx, need)
	if err != nil {
		return nil, false, err
	}

	brokerReqs := make(map[int32]map[string][]kmsg.DeleteRecordsRequestTopicPartition)
	var unknowns unknownErrShards

	for _, topic := range req.Topics {
		t := topic.Topic
		tmapping, exists := mapping[t]
		if err := missingOrCodeT(exists, tmapping.topic.ErrorCode); err != nil {
			unknowns.errs(err, t, topic.Partitions)
			continue
		}
		for _, partition := range topic.Partitions {
			p, exists := tmapping.mapping[partition.Partition]
			if err := missingOrCodeP(exists, p.ErrorCode); err != nil {
				unknowns.err(err, t, partition)
				continue
			}
			if err := noLeader(p.Leader); err != nil {
				unknowns.err(err, t, partition)
				continue
			}

			brokerReq := brokerReqs[p.Leader]
			if brokerReq == nil {
				brokerReq = make(map[string][]kmsg.DeleteRecordsRequestTopicPartition)
				brokerReqs[p.Leader] = brokerReq
			}
			brokerReq[t] = append(brokerReq[t], partition)
		}
	}

	mkreq := func() *kmsg.DeleteRecordsRequest {
		r := kmsg.NewPtrDeleteRecordsRequest()
		r.TimeoutMillis = req.TimeoutMillis
		return r
	}

	var issues []issueShard
	for brokerID, brokerReq := range brokerReqs {
		req := mkreq()
		for topic, parts := range brokerReq {
			reqTopic := kmsg.NewDeleteRecordsRequestTopic()
			reqTopic.Topic = topic
			reqTopic.Partitions = parts
			req.Topics = append(req.Topics, reqTopic)
		}
		issues = append(issues, issueShard{
			req:    req,
			broker: brokerID,
		})
	}

	return append(issues, unknowns.collect(mkreq, func(r *kmsg.DeleteRecordsRequest, topic string, parts []kmsg.DeleteRecordsRequestTopicPartition) {
		reqTopic := kmsg.NewDeleteRecordsRequestTopic()
		reqTopic.Topic = topic
		reqTopic.Partitions = parts
		r.Topics = append(r.Topics, reqTopic)
	})...), true, nil // this is reshardable
}

func (*deleteRecordsSharder) onResp(kmsg.Request, kmsg.Response) error { return nil } // topic / partitions: not retried

func (*deleteRecordsSharder) merge(sresps []ResponseShard) (kmsg.Response, error) {
	merged := kmsg.NewPtrDeleteRecordsResponse()
	topics := make(map[string][]kmsg.DeleteRecordsResponseTopicPartition)

	firstErr := firstErrMerger(sresps, func(kresp kmsg.Response) {
		resp := kresp.(*kmsg.DeleteRecordsResponse)
		merged.Version = resp.Version
		merged.ThrottleMillis = resp.ThrottleMillis

		for _, topic := range resp.Topics {
			topics[topic.Topic] = append(topics[topic.Topic], topic.Partitions...)
		}
	})
	for topic, partitions := range topics {
		respTopic := kmsg.NewDeleteRecordsResponseTopic()
		respTopic.Topic = topic
		respTopic.Partitions = partitions
		merged.Topics = append(merged.Topics, respTopic)
	}
	return merged, firstErr
}

// handle sharding OffsetForLeaderEpochRequest
type offsetForLeaderEpochSharder struct{ *Client }

func (cl *offsetForLeaderEpochSharder) shard(ctx context.Context, kreq kmsg.Request) ([]issueShard, bool, error) {
	req := kreq.(*kmsg.OffsetForLeaderEpochRequest)

	var need []string
	for _, topic := range req.Topics {
		need = append(need, topic.Topic)
	}
	mapping, err := cl.fetchMappedMetadata(ctx, need)
	if err != nil {
		return nil, false, err
	}

	brokerReqs := make(map[int32]map[string][]kmsg.OffsetForLeaderEpochRequestTopicPartition)
	var unknowns unknownErrShards

	for _, topic := range req.Topics {
		t := topic.Topic
		tmapping, exists := mapping[t]
		if err := missingOrCodeT(exists, tmapping.topic.ErrorCode); err != nil {
			unknowns.errs(err, t, topic.Partitions)
			continue
		}
		for _, partition := range topic.Partitions {
			p, exists := tmapping.mapping[partition.Partition]
			if err := missingOrCodeP(exists, p.ErrorCode); err != nil {
				unknowns.err(err, t, partition)
				continue
			}
			if err := noLeader(p.Leader); err != nil {
				unknowns.err(err, t, partition)
				continue
			}

			brokerReq := brokerReqs[p.Leader]
			if brokerReq == nil {
				brokerReq = make(map[string][]kmsg.OffsetForLeaderEpochRequestTopicPartition)
				brokerReqs[p.Leader] = brokerReq
			}
			brokerReq[topic.Topic] = append(brokerReq[topic.Topic], partition)
		}
	}

	mkreq := func() *kmsg.OffsetForLeaderEpochRequest {
		r := kmsg.NewPtrOffsetForLeaderEpochRequest()
		r.ReplicaID = req.ReplicaID
		return r
	}

	var issues []issueShard
	for brokerID, brokerReq := range brokerReqs {
		req := mkreq()
		for topic, parts := range brokerReq {
			reqTopic := kmsg.NewOffsetForLeaderEpochRequestTopic()
			reqTopic.Topic = topic
			reqTopic.Partitions = parts
			req.Topics = append(req.Topics, reqTopic)
		}
		issues = append(issues, issueShard{
			req:    req,
			broker: brokerID,
		})
	}

	return append(issues, unknowns.collect(mkreq, func(r *kmsg.OffsetForLeaderEpochRequest, topic string, parts []kmsg.OffsetForLeaderEpochRequestTopicPartition) {
		reqTopic := kmsg.NewOffsetForLeaderEpochRequestTopic()
		reqTopic.Topic = topic
		reqTopic.Partitions = parts
		r.Topics = append(r.Topics, reqTopic)
	})...), true, nil // this is reshardable
}

func (*offsetForLeaderEpochSharder) onResp(kmsg.Request, kmsg.Response) error { return nil } // topic / partitions: not retried

func (*offsetForLeaderEpochSharder) merge(sresps []ResponseShard) (kmsg.Response, error) {
	merged := kmsg.NewPtrOffsetForLeaderEpochResponse()
	topics := make(map[string][]kmsg.OffsetForLeaderEpochResponseTopicPartition)

	firstErr := firstErrMerger(sresps, func(kresp kmsg.Response) {
		resp := kresp.(*kmsg.OffsetForLeaderEpochResponse)
		merged.Version = resp.Version
		merged.ThrottleMillis = resp.ThrottleMillis

		for _, topic := range resp.Topics {
			topics[topic.Topic] = append(topics[topic.Topic], topic.Partitions...)
		}
	})
	for topic, partitions := range topics {
		respTopic := kmsg.NewOffsetForLeaderEpochResponseTopic()
		respTopic.Topic = topic
		respTopic.Partitions = partitions
		merged.Topics = append(merged.Topics, respTopic)
	}
	return merged, firstErr
}

// handle sharding DescribeConfigsRequest
type describeConfigsSharder struct{ *Client }

func (*describeConfigsSharder) shard(_ context.Context, kreq kmsg.Request) ([]issueShard, bool, error) {
	req := kreq.(*kmsg.DescribeConfigsRequest)

	brokerReqs := make(map[int32][]kmsg.DescribeConfigsRequestResource)
	var any []kmsg.DescribeConfigsRequestResource

	for i := range req.Resources {
		resource := req.Resources[i]
		switch resource.ResourceType {
		case kmsg.ConfigResourceTypeBroker:
		case kmsg.ConfigResourceTypeBrokerLogger:
		default:
			any = append(any, resource)
			continue
		}
		id, err := strconv.ParseInt(resource.ResourceName, 10, 32)
		if err != nil || id < 0 {
			any = append(any, resource)
			continue
		}
		brokerReqs[int32(id)] = append(brokerReqs[int32(id)], resource)
	}

	var issues []issueShard
	for brokerID, brokerReq := range brokerReqs {
		newReq := kmsg.NewPtrDescribeConfigsRequest()
		newReq.Resources = brokerReq
		newReq.IncludeSynonyms = req.IncludeSynonyms
		newReq.IncludeDocumentation = req.IncludeDocumentation

		issues = append(issues, issueShard{
			req:    newReq,
			broker: brokerID,
		})
	}

	if len(any) > 0 {
		newReq := kmsg.NewPtrDescribeConfigsRequest()
		newReq.Resources = any
		newReq.IncludeSynonyms = req.IncludeSynonyms
		newReq.IncludeDocumentation = req.IncludeDocumentation
		issues = append(issues, issueShard{
			req: newReq,
			any: true,
		})
	}

	return issues, false, nil // this is not reshardable, but the any block can go anywhere
}

func (*describeConfigsSharder) onResp(kmsg.Request, kmsg.Response) error { return nil } // configs: nothing retriable

func (*describeConfigsSharder) merge(sresps []ResponseShard) (kmsg.Response, error) {
	merged := kmsg.NewPtrDescribeConfigsResponse()
	return merged, firstErrMerger(sresps, func(kresp kmsg.Response) {
		resp := kresp.(*kmsg.DescribeConfigsResponse)
		merged.Version = resp.Version
		merged.ThrottleMillis = resp.ThrottleMillis
		merged.Resources = append(merged.Resources, resp.Resources...)
	})
}

// handle sharding AlterConfigsRequest
type alterConfigsSharder struct{ *Client }

func (*alterConfigsSharder) shard(_ context.Context, kreq kmsg.Request) ([]issueShard, bool, error) {
	req := kreq.(*kmsg.AlterConfigsRequest)

	brokerReqs := make(map[int32][]kmsg.AlterConfigsRequestResource)
	var any []kmsg.AlterConfigsRequestResource

	for i := range req.Resources {
		resource := req.Resources[i]
		switch resource.ResourceType {
		case kmsg.ConfigResourceTypeBroker:
		case kmsg.ConfigResourceTypeBrokerLogger:
		default:
			any = append(any, resource)
			continue
		}
		id, err := strconv.ParseInt(resource.ResourceName, 10, 32)
		if err != nil || id < 0 {
			any = append(any, resource)
			continue
		}
		brokerReqs[int32(id)] = append(brokerReqs[int32(id)], resource)
	}

	var issues []issueShard
	for brokerID, brokerReq := range brokerReqs {
		newReq := kmsg.NewPtrAlterConfigsRequest()
		newReq.Resources = brokerReq
		newReq.ValidateOnly = req.ValidateOnly

		issues = append(issues, issueShard{
			req:    newReq,
			broker: brokerID,
		})
	}

	if len(any) > 0 {
		newReq := kmsg.NewPtrAlterConfigsRequest()
		newReq.Resources = any
		newReq.ValidateOnly = req.ValidateOnly
		issues = append(issues, issueShard{
			req: newReq,
			any: true,
		})
	}

	return issues, false, nil // this is not reshardable, but the any block can go anywhere
}

func (*alterConfigsSharder) onResp(kmsg.Request, kmsg.Response) error { return nil } // configs: nothing retriable

func (*alterConfigsSharder) merge(sresps []ResponseShard) (kmsg.Response, error) {
	merged := kmsg.NewPtrAlterConfigsResponse()
	return merged, firstErrMerger(sresps, func(kresp kmsg.Response) {
		resp := kresp.(*kmsg.AlterConfigsResponse)
		merged.Version = resp.Version
		merged.ThrottleMillis = resp.ThrottleMillis
		merged.Resources = append(merged.Resources, resp.Resources...)
	})
}

// handles sharding AlterReplicaLogDirsRequest
type alterReplicaLogDirsSharder struct{ *Client }

func (cl *alterReplicaLogDirsSharder) shard(ctx context.Context, kreq kmsg.Request) ([]issueShard, bool, error) {
	req := kreq.(*kmsg.AlterReplicaLogDirsRequest)

	needMap := make(map[string]struct{})
	for _, dir := range req.Dirs {
		for _, topic := range dir.Topics {
			needMap[topic.Topic] = struct{}{}
		}
	}
	var need []string
	for topic := range needMap {
		need = append(need, topic)
	}
	mapping, err := cl.fetchMappedMetadata(ctx, need)
	if err != nil {
		return nil, false, err
	}

	brokerReqs := make(map[int32]map[string]map[string][]int32) // broker => dir => topic => partitions
	unknowns := make(map[error]map[string]map[string][]int32)   // err => dir => topic => partitions

	addBroker := func(broker int32, dir, topic string, partition int32) {
		brokerDirs := brokerReqs[broker]
		if brokerDirs == nil {
			brokerDirs = make(map[string]map[string][]int32)
			brokerReqs[broker] = brokerDirs
		}
		dirTopics := brokerDirs[dir]
		if dirTopics == nil {
			dirTopics = make(map[string][]int32)
			brokerDirs[dir] = dirTopics
		}
		dirTopics[topic] = append(dirTopics[topic], partition)
	}

	addUnknown := func(err error, dir, topic string, partition int32) {
		dirs := unknowns[err]
		if dirs == nil {
			dirs = make(map[string]map[string][]int32)
			unknowns[err] = dirs
		}
		dirTopics := dirs[dir]
		if dirTopics == nil {
			dirTopics = make(map[string][]int32)
			dirs[dir] = dirTopics
		}
		dirTopics[topic] = append(dirTopics[topic], partition)
	}

	for _, dir := range req.Dirs {
		for _, topic := range dir.Topics {
			t := topic.Topic
			tmapping, exists := mapping[t]
			if err := missingOrCodeT(exists, tmapping.topic.ErrorCode); err != nil {
				for _, partition := range topic.Partitions {
					addUnknown(err, dir.Dir, t, partition)
				}
				continue
			}
			for _, partition := range topic.Partitions {
				p, exists := tmapping.mapping[partition]
				if err := missingOrCodeP(exists, p.ErrorCode); err != nil {
					addUnknown(err, dir.Dir, t, partition)
					continue
				}

				for _, replica := range p.Replicas {
					addBroker(replica, dir.Dir, t, partition)
				}
			}
		}
	}

	var issues []issueShard
	for brokerID, brokerReq := range brokerReqs {
		req := kmsg.NewPtrAlterReplicaLogDirsRequest()
		for dir, topics := range brokerReq {
			rd := kmsg.NewAlterReplicaLogDirsRequestDir()
			rd.Dir = dir
			for topic, partitions := range topics {
				rdTopic := kmsg.NewAlterReplicaLogDirsRequestDirTopic()
				rdTopic.Topic = topic
				rdTopic.Partitions = partitions
				rd.Topics = append(rd.Topics, rdTopic)
			}
			req.Dirs = append(req.Dirs, rd)
		}

		issues = append(issues, issueShard{
			req:    req,
			broker: brokerID,
		})
	}

	for err, dirs := range unknowns {
		req := kmsg.NewPtrAlterReplicaLogDirsRequest()
		for dir, topics := range dirs {
			rd := kmsg.NewAlterReplicaLogDirsRequestDir()
			rd.Dir = dir
			for topic, partitions := range topics {
				rdTopic := kmsg.NewAlterReplicaLogDirsRequestDirTopic()
				rdTopic.Topic = topic
				rdTopic.Partitions = partitions
				rd.Topics = append(rd.Topics, rdTopic)
			}
			req.Dirs = append(req.Dirs, rd)
		}

		issues = append(issues, issueShard{
			req: req,
			err: err,
		})
	}

	return issues, true, nil // this is reshardable
}

func (*alterReplicaLogDirsSharder) onResp(kmsg.Request, kmsg.Response) error { return nil } // topic / partitions: not retried

// merge does not make sense for this function, but we provide a one anyway.
func (*alterReplicaLogDirsSharder) merge(sresps []ResponseShard) (kmsg.Response, error) {
	merged := kmsg.NewPtrAlterReplicaLogDirsResponse()
	topics := make(map[string][]kmsg.AlterReplicaLogDirsResponseTopicPartition)

	firstErr := firstErrMerger(sresps, func(kresp kmsg.Response) {
		resp := kresp.(*kmsg.AlterReplicaLogDirsResponse)
		merged.Version = resp.Version
		merged.ThrottleMillis = resp.ThrottleMillis

		for _, topic := range resp.Topics {
			topics[topic.Topic] = append(topics[topic.Topic], topic.Partitions...)
		}
	})
	for topic, partitions := range topics {
		respTopic := kmsg.NewAlterReplicaLogDirsResponseTopic()
		respTopic.Topic = topic
		respTopic.Partitions = partitions
		merged.Topics = append(merged.Topics, respTopic)
	}
	return merged, firstErr
}

// handles sharding DescribeLogDirsRequest
type describeLogDirsSharder struct{ *Client }

func (cl *describeLogDirsSharder) shard(ctx context.Context, kreq kmsg.Request) ([]issueShard, bool, error) {
	req := kreq.(*kmsg.DescribeLogDirsRequest)

	// If req.Topics is nil, the request is to describe all logdirs. Thus,
	// we will issue the request to all brokers (similar to ListGroups).
	if req.Topics == nil {
		return cl.allBrokersShardedReq(ctx, func() kmsg.Request {
			dup := *req
			return &dup
		})
	}

	var need []string
	for _, topic := range req.Topics {
		need = append(need, topic.Topic)
	}
	mapping, err := cl.fetchMappedMetadata(ctx, need)
	if err != nil {
		return nil, false, err
	}

	brokerReqs := make(map[int32]map[string][]int32)
	var unknowns unknownErrShards

	for _, topic := range req.Topics {
		t := topic.Topic
		tmapping, exists := mapping[t]
		if err := missingOrCodeT(exists, tmapping.topic.ErrorCode); err != nil {
			unknowns.errs(err, t, topic.Partitions)
			continue
		}
		for _, partition := range topic.Partitions {
			p, exists := tmapping.mapping[partition]
			if err := missingOrCodeP(exists, p.ErrorCode); err != nil {
				unknowns.err(err, t, partition)
				continue
			}

			for _, replica := range p.Replicas {
				brokerReq := brokerReqs[replica]
				if brokerReq == nil {
					brokerReq = make(map[string][]int32)
					brokerReqs[replica] = brokerReq
				}
				brokerReq[topic.Topic] = append(brokerReq[topic.Topic], partition)
			}
		}
	}

	mkreq := func() *kmsg.DescribeLogDirsRequest {
		return kmsg.NewPtrDescribeLogDirsRequest()
	}

	var issues []issueShard
	for brokerID, brokerReq := range brokerReqs {
		req := mkreq()
		for topic, parts := range brokerReq {
			reqTopic := kmsg.NewDescribeLogDirsRequestTopic()
			reqTopic.Topic = topic
			reqTopic.Partitions = parts
			req.Topics = append(req.Topics, reqTopic)
		}
		issues = append(issues, issueShard{
			req:    req,
			broker: brokerID,
		})
	}

	return append(issues, unknowns.collect(mkreq, func(r *kmsg.DescribeLogDirsRequest, topic string, parts []int32) {
		reqTopic := kmsg.NewDescribeLogDirsRequestTopic()
		reqTopic.Topic = topic
		reqTopic.Partitions = parts
		r.Topics = append(r.Topics, reqTopic)
	})...), true, nil // this is reshardable
}

func (*describeLogDirsSharder) onResp(kmsg.Request, kmsg.Response) error { return nil } // topic / configs: not retried

// merge does not make sense for this function, but we provide one anyway.
// We lose the error code for directories.
func (*describeLogDirsSharder) merge(sresps []ResponseShard) (kmsg.Response, error) {
	merged := kmsg.NewPtrDescribeLogDirsResponse()
	dirs := make(map[string]map[string][]kmsg.DescribeLogDirsResponseDirTopicPartition)

	firstErr := firstErrMerger(sresps, func(kresp kmsg.Response) {
		resp := kresp.(*kmsg.DescribeLogDirsResponse)
		merged.Version = resp.Version
		merged.ThrottleMillis = resp.ThrottleMillis

		for _, dir := range resp.Dirs {
			mergeDir := dirs[dir.Dir]
			if mergeDir == nil {
				mergeDir = make(map[string][]kmsg.DescribeLogDirsResponseDirTopicPartition)
				dirs[dir.Dir] = mergeDir
			}
			for _, topic := range dir.Topics {
				mergeDir[topic.Topic] = append(mergeDir[topic.Topic], topic.Partitions...)
			}
		}
	})
	for dir, topics := range dirs {
		md := kmsg.NewDescribeLogDirsResponseDir()
		md.Dir = dir
		for topic, partitions := range topics {
			mdTopic := kmsg.NewDescribeLogDirsResponseDirTopic()
			mdTopic.Topic = topic
			mdTopic.Partitions = partitions
			md.Topics = append(md.Topics, mdTopic)
		}
		merged.Dirs = append(merged.Dirs, md)
	}
	return merged, firstErr
}

// handles sharding DeleteGroupsRequest
type deleteGroupsSharder struct{ *Client }

func (cl *deleteGroupsSharder) shard(_ context.Context, kreq kmsg.Request) ([]issueShard, bool, error) {
	req := kreq.(*kmsg.DeleteGroupsRequest)

	coordinators := cl.loadCoordinators(coordinatorTypeGroup, req.Groups...)
	type unkerr struct {
		err   error
		group string
	}
	var (
		brokerReqs = make(map[int32]*kmsg.DeleteGroupsRequest)
		kerrs      = make(map[*kerr.Error][]string)
		unkerrs    []unkerr
	)

	newReq := func(groups ...string) *kmsg.DeleteGroupsRequest {
		newReq := kmsg.NewPtrDeleteGroupsRequest()
		newReq.Groups = groups
		return newReq
	}

	for _, group := range req.Groups {
		berr := coordinators[group]
		var ke *kerr.Error
		switch {
		case berr.err == nil:
			brokerReq := brokerReqs[berr.b.meta.NodeID]
			if brokerReq == nil {
				brokerReq = newReq()
				brokerReqs[berr.b.meta.NodeID] = brokerReq
			}
			brokerReq.Groups = append(brokerReq.Groups, group)
		case errors.As(berr.err, &ke):
			kerrs[ke] = append(kerrs[ke], group)
		default:
			unkerrs = append(unkerrs, unkerr{berr.err, group})
		}
	}

	var issues []issueShard
	for id, req := range brokerReqs {
		issues = append(issues, issueShard{
			req:    req,
			broker: id,
		})
	}
	for _, unkerr := range unkerrs {
		issues = append(issues, issueShard{
			req: newReq(unkerr.group),
			err: unkerr.err,
		})
	}
	for kerr, groups := range kerrs {
		issues = append(issues, issueShard{
			req: newReq(groups...),
			err: kerr,
		})
	}

	return issues, true, nil // reshardable to load correct coordinators
}

func (cl *deleteGroupsSharder) onResp(_ kmsg.Request, kresp kmsg.Response) error {
	resp := kresp.(*kmsg.DeleteGroupsResponse)
	var retErr error
	for i := range resp.Groups {
		group := &resp.Groups[i]
		err := kerr.ErrorForCode(group.ErrorCode)
		cl.maybeDeleteStaleCoordinator(group.Group, coordinatorTypeGroup, err)
		onRespShardErr(&retErr, err)
	}
	return retErr
}

func (*deleteGroupsSharder) merge(sresps []ResponseShard) (kmsg.Response, error) {
	merged := kmsg.NewPtrDeleteGroupsResponse()
	return merged, firstErrMerger(sresps, func(kresp kmsg.Response) {
		resp := kresp.(*kmsg.DeleteGroupsResponse)
		merged.Version = resp.Version
		merged.ThrottleMillis = resp.ThrottleMillis
		merged.Groups = append(merged.Groups, resp.Groups...)
	})
}

// handle sharding IncrementalAlterConfigsRequest
type incrementalAlterConfigsSharder struct{ *Client }

func (*incrementalAlterConfigsSharder) shard(_ context.Context, kreq kmsg.Request) ([]issueShard, bool, error) {
	req := kreq.(*kmsg.IncrementalAlterConfigsRequest)

	brokerReqs := make(map[int32][]kmsg.IncrementalAlterConfigsRequestResource)
	var any []kmsg.IncrementalAlterConfigsRequestResource

	for i := range req.Resources {
		resource := req.Resources[i]
		switch resource.ResourceType {
		case kmsg.ConfigResourceTypeBroker:
		case kmsg.ConfigResourceTypeBrokerLogger:
		default:
			any = append(any, resource)
			continue
		}
		id, err := strconv.ParseInt(resource.ResourceName, 10, 32)
		if err != nil || id < 0 {
			any = append(any, resource)
			continue
		}
		brokerReqs[int32(id)] = append(brokerReqs[int32(id)], resource)
	}

	var issues []issueShard
	for brokerID, brokerReq := range brokerReqs {
		newReq := kmsg.NewPtrIncrementalAlterConfigsRequest()
		newReq.Resources = brokerReq
		newReq.ValidateOnly = req.ValidateOnly

		issues = append(issues, issueShard{
			req:    newReq,
			broker: brokerID,
		})
	}

	if len(any) > 0 {
		newReq := kmsg.NewPtrIncrementalAlterConfigsRequest()
		newReq.Resources = any
		newReq.ValidateOnly = req.ValidateOnly
		issues = append(issues, issueShard{
			req: newReq,
			any: true,
		})
	}

	return issues, false, nil // this is not reshardable, but the any block can go anywhere
}

func (*incrementalAlterConfigsSharder) onResp(kmsg.Request, kmsg.Response) error { return nil } // config: nothing retriable

func (*incrementalAlterConfigsSharder) merge(sresps []ResponseShard) (kmsg.Response, error) {
	merged := kmsg.NewPtrIncrementalAlterConfigsResponse()
	return merged, firstErrMerger(sresps, func(kresp kmsg.Response) {
		resp := kresp.(*kmsg.IncrementalAlterConfigsResponse)
		merged.Version = resp.Version
		merged.ThrottleMillis = resp.ThrottleMillis
		merged.Resources = append(merged.Resources, resp.Resources...)
	})
}

// handle sharding DescribeProducersRequest
type describeProducersSharder struct{ *Client }

func (cl *describeProducersSharder) shard(ctx context.Context, kreq kmsg.Request) ([]issueShard, bool, error) {
	req := kreq.(*kmsg.DescribeProducersRequest)

	var need []string
	for _, topic := range req.Topics {
		need = append(need, topic.Topic)
	}
	mapping, err := cl.fetchMappedMetadata(ctx, need)
	if err != nil {
		return nil, false, err
	}

	brokerReqs := make(map[int32]map[string][]int32) // broker => topic => partitions
	var unknowns unknownErrShards

	for _, topic := range req.Topics {
		t := topic.Topic
		tmapping, exists := mapping[t]
		if err := missingOrCodeT(exists, tmapping.topic.ErrorCode); err != nil {
			unknowns.errs(err, t, topic.Partitions)
			continue
		}
		for _, partition := range topic.Partitions {
			p, exists := tmapping.mapping[partition]
			if err := missingOrCodeP(exists, p.ErrorCode); err != nil {
				unknowns.err(err, t, partition)
				continue
			}

			brokerReq := brokerReqs[p.Leader]
			if brokerReq == nil {
				brokerReq = make(map[string][]int32)
				brokerReqs[p.Leader] = brokerReq
			}
			brokerReq[topic.Topic] = append(brokerReq[topic.Topic], partition)
		}
	}

	mkreq := func() *kmsg.DescribeProducersRequest {
		return kmsg.NewPtrDescribeProducersRequest()
	}

	var issues []issueShard
	for brokerID, brokerReq := range brokerReqs {
		req := mkreq()
		for topic, parts := range brokerReq {
			reqTopic := kmsg.NewDescribeProducersRequestTopic()
			reqTopic.Topic = topic
			reqTopic.Partitions = parts
			req.Topics = append(req.Topics, reqTopic)
		}
		issues = append(issues, issueShard{
			req:    req,
			broker: brokerID,
		})
	}

	return append(issues, unknowns.collect(mkreq, func(r *kmsg.DescribeProducersRequest, topic string, parts []int32) {
		reqTopic := kmsg.NewDescribeProducersRequestTopic()
		reqTopic.Topic = topic
		reqTopic.Partitions = parts
		r.Topics = append(r.Topics, reqTopic)
	})...), true, nil // this is reshardable
}

func (*describeProducersSharder) onResp(kmsg.Request, kmsg.Response) error { return nil } // topic / partitions: not retriable

func (*describeProducersSharder) merge(sresps []ResponseShard) (kmsg.Response, error) {
	merged := kmsg.NewPtrDescribeProducersResponse()
	topics := make(map[string][]kmsg.DescribeProducersResponseTopicPartition)
	firstErr := firstErrMerger(sresps, func(kresp kmsg.Response) {
		resp := kresp.(*kmsg.DescribeProducersResponse)
		merged.Version = resp.Version
		merged.ThrottleMillis = resp.ThrottleMillis

		for _, topic := range resp.Topics {
			topics[topic.Topic] = append(topics[topic.Topic], topic.Partitions...)
		}
	})
	for topic, partitions := range topics {
		respTopic := kmsg.NewDescribeProducersResponseTopic()
		respTopic.Topic = topic
		respTopic.Partitions = partitions
		merged.Topics = append(merged.Topics, respTopic)
	}
	return merged, firstErr
}

// handles sharding DescribeTransactionsRequest
type describeTransactionsSharder struct{ *Client }

func (cl *describeTransactionsSharder) shard(_ context.Context, kreq kmsg.Request) ([]issueShard, bool, error) {
	req := kreq.(*kmsg.DescribeTransactionsRequest)

	coordinators := cl.loadCoordinators(coordinatorTypeTxn, req.TransactionalIDs...)
	type unkerr struct {
		err   error
		txnID string
	}
	var (
		brokerReqs = make(map[int32]*kmsg.DescribeTransactionsRequest)
		kerrs      = make(map[*kerr.Error][]string)
		unkerrs    []unkerr
	)

	newReq := func(txnIDs ...string) *kmsg.DescribeTransactionsRequest {
		r := kmsg.NewPtrDescribeTransactionsRequest()
		r.TransactionalIDs = txnIDs
		return r
	}

	for _, txnID := range req.TransactionalIDs {
		berr := coordinators[txnID]
		var ke *kerr.Error
		switch {
		case berr.err == nil:
			brokerReq := brokerReqs[berr.b.meta.NodeID]
			if brokerReq == nil {
				brokerReq = newReq()
				brokerReqs[berr.b.meta.NodeID] = brokerReq
			}
			brokerReq.TransactionalIDs = append(brokerReq.TransactionalIDs, txnID)
		case errors.As(berr.err, &ke):
			kerrs[ke] = append(kerrs[ke], txnID)
		default:
			unkerrs = append(unkerrs, unkerr{berr.err, txnID})
		}
	}

	var issues []issueShard
	for id, req := range brokerReqs {
		issues = append(issues, issueShard{
			req:    req,
			broker: id,
		})
	}
	for _, unkerr := range unkerrs {
		issues = append(issues, issueShard{
			req: newReq(unkerr.txnID),
			err: unkerr.err,
		})
	}
	for kerr, txnIDs := range kerrs {
		issues = append(issues, issueShard{
			req: newReq(txnIDs...),
			err: kerr,
		})
	}

	return issues, true, nil // reshardable to load correct coordinators
}

func (cl *describeTransactionsSharder) onResp(_ kmsg.Request, kresp kmsg.Response) error { // cleanup any stale coordinators
	resp := kresp.(*kmsg.DescribeTransactionsResponse)
	var retErr error
	for i := range resp.TransactionStates {
		txnState := &resp.TransactionStates[i]
		err := kerr.ErrorForCode(txnState.ErrorCode)
		cl.maybeDeleteStaleCoordinator(txnState.TransactionalID, coordinatorTypeTxn, err)
		onRespShardErr(&retErr, err)
	}
	return retErr
}

func (*describeTransactionsSharder) merge(sresps []ResponseShard) (kmsg.Response, error) {
	merged := kmsg.NewPtrDescribeTransactionsResponse()
	return merged, firstErrMerger(sresps, func(kresp kmsg.Response) {
		resp := kresp.(*kmsg.DescribeTransactionsResponse)
		merged.Version = resp.Version
		merged.ThrottleMillis = resp.ThrottleMillis
		merged.TransactionStates = append(merged.TransactionStates, resp.TransactionStates...)
	})
}

// handles sharding ListTransactionsRequest
type listTransactionsSharder struct{ *Client }

func (cl *listTransactionsSharder) shard(ctx context.Context, kreq kmsg.Request) ([]issueShard, bool, error) {
	req := kreq.(*kmsg.ListTransactionsRequest)
	return cl.allBrokersShardedReq(ctx, func() kmsg.Request {
		dup := *req
		return &dup
	})
}

func (*listTransactionsSharder) onResp(_ kmsg.Request, kresp kmsg.Response) error {
	resp := kresp.(*kmsg.ListTransactionsResponse)
	return kerr.ErrorForCode(resp.ErrorCode)
}

func (*listTransactionsSharder) merge(sresps []ResponseShard) (kmsg.Response, error) {
	merged := kmsg.NewPtrListTransactionsResponse()

	unknownStates := make(map[string]struct{})

	firstErr := firstErrMerger(sresps, func(kresp kmsg.Response) {
		resp := kresp.(*kmsg.ListTransactionsResponse)
		merged.Version = resp.Version
		merged.ThrottleMillis = resp.ThrottleMillis
		if merged.ErrorCode == 0 {
			merged.ErrorCode = resp.ErrorCode
		}
		for _, state := range resp.UnknownStateFilters {
			unknownStates[state] = struct{}{}
		}
		merged.TransactionStates = append(merged.TransactionStates, resp.TransactionStates...)
	})
	for unknownState := range unknownStates {
		merged.UnknownStateFilters = append(merged.UnknownStateFilters, unknownState)
	}

	return merged, firstErr
}
