package dht

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/libp2p/go-libp2p-core/network"
	"github.com/libp2p/go-libp2p-core/peer"

	logging "github.com/ipfs/go-log"
	todoctr "github.com/ipfs/go-todocounter"
	process "github.com/jbenet/goprocess"
	ctxproc "github.com/jbenet/goprocess/context"
	kb "github.com/libp2p/go-libp2p-kbucket"

	pstore "github.com/libp2p/go-libp2p-core/peerstore"
	routing "github.com/libp2p/go-libp2p-core/routing"
	tracefmt "github.com/libp2p/go-libp2p-kad-dht/tracefmt"
	queue "github.com/libp2p/go-libp2p-peerstore/queue"
	notif "github.com/libp2p/go-libp2p-routing/notifications"
	// ks "github.com/whyrusleeping/go-keyspace"
)

// ErrNoPeersQueried is returned when we failed to connect to any peers.
var ErrNoPeersQueried = errors.New("failed to query any peers")

var maxQueryConcurrency = AlphaValue

type dhtQuery struct {
	dht         *IpfsDHT
	key         string    // the key we're querying for
	qfunc       queryFunc // the function to execute per peer
	concurrency int       // the concurrency parameter
}

type dhtQueryResult struct {
	peer        *peer.AddrInfo   // FindPeer
	closerPeers []*peer.AddrInfo // *
	success     bool

	finalSet   *peer.Set
	queriedSet *peer.Set
}

// constructs query
func (dht *IpfsDHT) newQuery(k string, f queryFunc) *dhtQuery {
	return &dhtQuery{
		key:         k,
		dht:         dht,
		qfunc:       f,
		concurrency: maxQueryConcurrency,
	}
}

// QueryFunc is a function that runs a particular query with a given peer.
// It returns either:
// - the value
// - a list of peers potentially better able to serve the query
// - an error
type queryFunc func(context.Context, peer.ID) (*dhtQueryResult, error)

// Run runs the query at hand. pass in a list of peers to use first.
func (q *dhtQuery) Run(ctx context.Context, peers []peer.ID) (*dhtQueryResult, error) {
	if len(peers) == 0 {
		logger.Warning("Running query with no peers!")
		return nil, kb.ErrLookupFailure
	}

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	runner := newQueryRunner(q)
	return runner.Run(ctx, peers)
}

type dhtQueryRunner struct {
	query          *dhtQuery        // query to run
	peersSeen      *peer.Set        // all peers queried. prevent querying same peer 2x
	peersQueried   *peer.Set        // peers successfully connected to and queried
	peersDialed    *dialQueue       // peers we have dialed to
	peersToQuery   *queue.ChanQueue // peers remaining to be queried
	peersRemaining todoctr.Counter  // peersToQuery + currently processing

	result *dhtQueryResult // query result

	rateLimit chan struct{} // processing semaphore
	log       logging.EventLogger

	runCtx context.Context

	startTime time.Time
	endTime   time.Time

	proc process.Process
	sync.RWMutex
}

func (r *dhtQueryRunner) Loggable() map[string]interface{} {
	return r.TraceValue().Loggable()
}

func PeerIDsFromPeerAddrs(l1 []*peer.AddrInfo) []peer.ID {
	l2 := make([]peer.ID, len(l1))
	for i := 0; i < len(l1); i++ {
		l2[i] = l1[i].ID
	}
	return l2
}

func (r *dhtQueryRunner) TraceValue() *tracefmt.QueryRunnerState {

	r.Lock()
	qrs := &tracefmt.QueryRunnerState{}
	qrs.Query.Key = r.query.key
	qrs.PeersSeen = r.peersSeen.Peers()
	qrs.PeersQueried = r.peersQueried.Peers()
	qrs.PeersDialedLen = r.peersDialed.out.Queue.Len()
	// qrs.PeersToQuery = r.peersToQuery.Peers() // todo
	qrs.PeersToQueryLen = r.peersToQuery.Queue.Len()
	// qrs.PeersRemaining =  r.peersRemaining// todo
	
	if r.result != nil {
		qrs.Result = tracefmt.QueryResult{
			Success:     r.result.success,
			FoundPeer:   r.result.peer.ID,
			CloserPeers: PeerIDsFromPeerAddrs(r.result.closerPeers),
			// FinalSet:    r.result.finalSet.Peers() //todo,
			// QueriedSet:  r.result.queriedSet.Peers(),
		}
	}

	qrs.RateLimit.Capacity = r.query.concurrency
	qrs.RateLimit.Length = len(r.rateLimit)

	qrs.StartTime = r.startTime
	qrs.CurrTime = time.Now()
	qrs.EndTime = r.endTime

	r.Unlock()

	return qrs
}

func newQueryRunner(q *dhtQuery) *dhtQueryRunner {
	proc := process.WithParent(process.Background())
	ctx := ctxproc.OnClosingContext(proc)
	peersToQuery := queue.NewChanQueue(ctx, queue.NewXORDistancePQ(string(q.key)))
	r := &dhtQueryRunner{
		query:          q,
		peersRemaining: todoctr.NewSyncCounter(),
		peersSeen:      peer.NewSet(),
		peersQueried:   peer.NewSet(),
		rateLimit:      make(chan struct{}, q.concurrency),
		peersToQuery:   peersToQuery,
		proc:           proc,
	}
	dq, err := newDialQueue(&dqParams{
		ctx:    ctx,
		target: q.key,
		in:     peersToQuery,
		dialFn: r.dialPeer,
		config: dqDefaultConfig(),
	})
	if err != nil {
		panic(err)
	}
	r.peersDialed = dq
	return r
}

func (r *dhtQueryRunner) Run(ctx context.Context, peers []peer.ID) (*dhtQueryResult, error) {
	r.Lock()
	r.startTime = time.Now()
	r.Unlock()

	logger.EventBegin(ctx, "dhtQueryRunnerStart", logging.LoggableMap{
		"started": time.Now(),
		"query": r.query.key,
	}).Done()

	eip := logger.EventBegin(ctx, "dhtQueryRunner")
	defer eip.Done()

	r.log = logger
	r.runCtx = ctx

	// setup concurrency rate limiting
	for i := 0; i < r.query.concurrency; i++ {
		r.rateLimit <- struct{}{}
	}

	// add all the peers we got first.
	for _, p := range peers {
		r.addPeerToQuery(p)
	}

	// start the dial queue only after we've added the initial set of peers.
	// this is to avoid race conditions that could cause the peersRemaining todoctr
	// to be done too early if the initial dial fails before others make it into the queue.
	r.peersDialed.Start()

	// go do this thing.
	// do it as a child proc to make sure Run exits
	// ONLY AFTER spawn workers has exited.
	r.proc.Go(r.spawnWorkers)

	// wait until they're done.
	var err error

	// now, if the context finishes, close the proc.
	// we have to do it here because the logic before is setup, which
	// should run without closing the proc.
	ctxproc.CloseAfterContext(r.proc, ctx)

	select {
	case <-r.peersRemaining.Done():
		r.proc.Close()
		if r.peersQueried.Size() == 0 {
			err = ErrNoPeersQueried
		} else {
			err = routing.ErrNotFound
		}

	case <-r.proc.Closed():
		err = r.runCtx.Err()
	}

	r.Lock()
	r.endTime = time.Now()
	r.Unlock()

	r.RLock()
	defer r.RUnlock()

	eip.Append(r)

	if r.result != nil && r.result.success {
		return r.result, nil
	}


	return &dhtQueryResult{
		finalSet:   r.peersSeen,
		queriedSet: r.peersQueried,
	}, err
}

func (r *dhtQueryRunner) addPeerToQuery(next peer.ID) {
	// if new peer is ourselves...
	if next == r.query.dht.self {
		r.log.Debug("addPeerToQuery skip self")
		return
	}

	if !r.peersSeen.TryAdd(next) {
		return
	}

	notif.PublishQueryEvent(r.runCtx, &notif.QueryEvent{
		Type: notif.AddingPeer,
		ID:   next,
	})

	r.peersRemaining.Increment(1)
	select {
	case r.peersToQuery.EnqChan <- next:
	case <-r.proc.Closing():
	}
}

func (r *dhtQueryRunner) spawnWorkers(proc process.Process) {
	for {
		select {
		case <-r.peersRemaining.Done():
			return

		case <-r.proc.Closing():
			return

		case <-r.rateLimit:
			ch := r.peersDialed.Consume()
			select {
			case p, ok := <-ch:
				if !ok {
					// this signals context cancellation.
					return
				}
				// do it as a child func to make sure Run exits
				// ONLY AFTER spawn workers has exited.
				proc.Go(func(proc process.Process) {
					r.queryPeer(proc, p)
				})
			case <-r.proc.Closing():
				return
			case <-r.peersRemaining.Done():
				return
			}
		}
	}
}

func (r *dhtQueryRunner) dialPeer(ctx context.Context, p peer.ID) error {
	// short-circuit if we're already connected.
	eip := logger.EventBegin(ctx, "dialPeer!")
	defer eip.Done()
	if r.query.dht.host.Network().Connectedness(p) == network.Connected {
		eip.Append(logging.LoggableMap{
			"peer":   p,
			"started": time.Now(),
			"query": r.query.key,
			"dialSuccess": "already connected",
		})
		return nil
	}

	logger.Debug("not connected. dialing.")
	notif.PublishQueryEvent(r.runCtx, &notif.QueryEvent{
		Type: notif.DialingPeer,
		ID:   p,
	})

	pi := peer.AddrInfo{ID: p}
	if err := r.query.dht.host.Connect(ctx, pi); err != nil {
		logger.Debugf("error connecting: %s", err)
		notif.PublishQueryEvent(r.runCtx, &notif.QueryEvent{
			Type:  notif.QueryError,
			Extra: err.Error(),
			ID:    p,
		})

		// This peer is dropping out of the race.
		r.peersRemaining.Decrement(1)
		eip.Append(logging.LoggableMap{
			"peer":   p,
			"started": time.Now(),
			"query": r.query.key,
			"dialSuccess": "failure",
		})
		return err
	}
	eip.Append(logging.LoggableMap{
		"peer":   p,
		"started": time.Now(),
		"query": r.query.key,
		"dialSuccess": "success",
	})
	logger.Debugf("connected. dial success.")
	return nil
}

func bits(bs []byte) []int {
	r := make([]int, len(bs)*8)
	for i, b := range bs {
			for j := 0; j < 8; j++ {
					r[i*8+j] = int(b >> uint(7-j) & 0x01)
			}
	}
	return r
}

func (r *dhtQueryRunner) queryPeer(proc process.Process, p peer.ID) {
	// ok let's do this!
	
	// create a context from our proc.
	ctx := ctxproc.OnClosingContext(proc)

	eip := logger.EventBegin(ctx, "queryPeer!")

	// make sure we do this when we exit
	defer func() {
		// signal we're done processing peer p
		r.peersRemaining.Decrement(1)
		r.rateLimit <- struct{}{}
	}()

	// finally, run the query against this peer
	res, err := r.query.qfunc(ctx, p)

	conn := r.query.dht.connForPeer(p)

	r.peersQueried.Add(p)

	// todo-- just for logs, not the best place for this
	// first getting distance that seems to be used for the peer queue, but seems to be a different xor representation then in the viz 
	// from := ks.XORKeySpace.Key([]byte(string(r.query.key)))
	// distance := ks.XORKeySpace.Key([]byte(p)).Distance(from)
	keybytes := bits([]byte(string(r.query.key)))
	pbytes := bits([]byte(p))
	n := len(keybytes)
	distance := 0
	for i := 0; i < n; i++ {
		distance += (keybytes[i]^pbytes[i])
	}


	if err != nil {
		logger.Debugf("ERROR worker for: %v %v", p, err)
	} else if res.success {
		logger.Debugf("SUCCESS worker for: %v %s", p, res)
		r.Lock()
		r.result = res
		r.Unlock()
		if res.peer != nil {
			r.query.dht.peerstore.AddAddrs(res.peer.ID, res.peer.Addrs, pstore.TempAddrTTL)
		}
		eip.Append(logging.LoggableMap{
			"peer":   p,
			"success": res.success,
			"query": r.query.key,
			"XOR": distance,
		})
		defer eip.Done()
		go r.proc.Close() // signal to everyone that we're done.
		// must be async, as we're one of the children, and Close blocks.
	} else if filtered := filterCandidatesPtr(conn, res.closerPeers); len(filtered) > 0 {
		logger.Debugf("PEERS CLOSER -- worker for: %v (%d closer filtered peers; unfiltered: %d)", p,
			len(filtered), len(res.closerPeers))
			eip.Append(logging.LoggableMap{
				"peer":   p,
				"success": res.success,
				"closerPeers": len(res.closerPeers),
				"filteredPeers": len(filtered),
				"filteredPeersArr": PeerIDsFromPeerAddrs(filtered),
				"query": r.query.key,
				"XOR": distance,
			})
			defer eip.Done()
		for _, next := range filtered {
			if next.ID == r.query.dht.self { // don't add self.
				logger.Debugf("PEERS CLOSER -- worker for: %v found self", p)
				continue
			}

			// add their addresses to the dialer's peerstore
			r.query.dht.peerstore.AddAddrs(next.ID, next.Addrs, pstore.TempAddrTTL)
			r.addPeerToQuery(next.ID)
			// logger.Debugf("PEERS CLOSER -- worker for: %v added %v (%v)", p, next.ID, next.Addrs)
		}
	} else {
		logger.Debugf("QUERY worker for: %v - not found, and no closer (filtered) peers.", p)
	}
	
}
