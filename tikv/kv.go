// Copyright 2021 TiKV Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// NOTE: The code in this file is based on code from the
// TiDB project, licensed under the Apache License v 2.0
//
// https://github.com/pingcap/tidb/tree/cc5e161ac06827589c4966674597c137cc9e809c/store/tikv/kv.go
//

// Copyright 2016 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package tikv

import (
	"context"
	"crypto/tls"
	"fmt"
	"math"
	"math/rand"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/opentracing/opentracing-go"
	"github.com/pingcap/kvproto/pkg/kvrpcpb"
	"github.com/pingcap/kvproto/pkg/metapb"
	"github.com/pkg/errors"
	"github.com/tikv/client-go/v2/config"
	tikverr "github.com/tikv/client-go/v2/error"
	"github.com/tikv/client-go/v2/internal/client"
	"github.com/tikv/client-go/v2/internal/latch"
	"github.com/tikv/client-go/v2/internal/locate"
	"github.com/tikv/client-go/v2/internal/logutil"
	"github.com/tikv/client-go/v2/internal/retry"
	"github.com/tikv/client-go/v2/kv"
	"github.com/tikv/client-go/v2/metrics"
	"github.com/tikv/client-go/v2/oracle"
	"github.com/tikv/client-go/v2/oracle/oracles"
	"github.com/tikv/client-go/v2/tikvrpc"
	"github.com/tikv/client-go/v2/txnkv/rangetask"
	"github.com/tikv/client-go/v2/txnkv/transaction"
	"github.com/tikv/client-go/v2/txnkv/txnlock"
	"github.com/tikv/client-go/v2/txnkv/txnsnapshot"
	"github.com/tikv/client-go/v2/util"
	pd "github.com/tikv/pd/client"
	clientv3 "go.etcd.io/etcd/client/v3"
	atomicutil "go.uber.org/atomic"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/keepalive"
)

// DCLabelKey indicates the key of label which represents the dc for Store.
const DCLabelKey = "zone"

func createEtcdKV(addrs []string, tlsConfig *tls.Config) (*clientv3.Client, error) {
	cfg := config.GetGlobalConfig()
	cli, err := clientv3.New(clientv3.Config{
		Endpoints:            addrs,
		AutoSyncInterval:     30 * time.Second,
		DialTimeout:          5 * time.Second,
		TLS:                  tlsConfig,
		DialKeepAliveTime:    time.Second * time.Duration(cfg.TiKVClient.GrpcKeepAliveTime),
		DialKeepAliveTimeout: time.Second * time.Duration(cfg.TiKVClient.GrpcKeepAliveTimeout),
	})
	if err != nil {
		return nil, errors.WithStack(err)
	}
	return cli, nil
}

// update oracle's lastTS every 2000ms.
var oracleUpdateInterval = 2000

// KVStore contains methods to interact with a TiKV cluster.
type KVStore struct {
	clusterID uint64
	uuid      string
	oracle    oracle.Oracle
	clientMu  struct {
		sync.RWMutex
		client Client
	}
	pdClient     pd.Client
	regionCache  *locate.RegionCache
	lockResolver *txnlock.LockResolver
	txnLatches   *latch.LatchesScheduler

	mock bool

	kv        SafePointKV
	safePoint uint64
	spTime    time.Time
	spMutex   sync.RWMutex // this is used to update safePoint and spTime

	// storeID -> safeTS, stored as map[uint64]uint64
	// safeTS here will be used during the Stale Read process,
	// it indicates the safe timestamp point that can be used to read consistent but may not the latest data.
	safeTSMap sync.Map

	replicaReadSeed uint32 // this is used to load balance followers / learners when replica read is enabled

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	close  atomicutil.Bool
}

// UpdateSPCache updates cached safepoint.
func (s *KVStore) UpdateSPCache(cachedSP uint64, cachedTime time.Time) {
	s.spMutex.Lock()
	s.safePoint = cachedSP
	s.spTime = cachedTime
	s.spMutex.Unlock()
}

// CheckVisibility checks if it is safe to read using given ts.
func (s *KVStore) CheckVisibility(startTime uint64) error {
	s.spMutex.RLock()
	cachedSafePoint := s.safePoint
	cachedTime := s.spTime
	s.spMutex.RUnlock()
	diff := time.Since(cachedTime)

	if diff > (GcSafePointCacheInterval - gcCPUTimeInaccuracyBound) {
		return tikverr.NewErrPDServerTimeout("start timestamp may fall behind safe point")
	}

	if startTime < cachedSafePoint {
		t1 := oracle.GetTimeFromTS(startTime)
		t2 := oracle.GetTimeFromTS(cachedSafePoint)
		return &tikverr.ErrGCTooEarly{
			TxnStartTS:  t1,
			GCSafePoint: t2,
		}
	}

	return nil
}

// NewKVStore creates a new TiKV store instance.
func NewKVStore(uuid string, pdClient pd.Client, spkv SafePointKV, tikvclient Client) (*KVStore, error) {
	o, err := oracles.NewPdOracle(pdClient, time.Duration(oracleUpdateInterval)*time.Millisecond)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	store := &KVStore{
		clusterID:       pdClient.GetClusterID(context.TODO()),
		uuid:            uuid,
		oracle:          o,
		pdClient:        pdClient,
		regionCache:     locate.NewRegionCache(pdClient),
		kv:              spkv,
		safePoint:       0,
		spTime:          time.Now(),
		replicaReadSeed: rand.Uint32(),
		ctx:             ctx,
		cancel:          cancel,
	}
	store.clientMu.client = client.NewReqCollapse(client.NewInterceptedClient(tikvclient))
	store.lockResolver = txnlock.NewLockResolver(store)

	store.wg.Add(2)
	go store.runSafePointChecker()
	go store.safeTSUpdater()

	return store, nil
}

// NewPDClient creates pd.Client with pdAddrs.
func NewPDClient(pdAddrs []string) (pd.Client, error) {
	cfg := config.GetGlobalConfig()
	// init pd-client
	pdCli, err := pd.NewClient(pdAddrs, pd.SecurityOption{
		CAPath:   cfg.Security.ClusterSSLCA,
		CertPath: cfg.Security.ClusterSSLCert,
		KeyPath:  cfg.Security.ClusterSSLKey,
	},
		pd.WithGRPCDialOptions(
			grpc.WithKeepaliveParams(keepalive.ClientParameters{
				Time:    time.Duration(cfg.TiKVClient.GrpcKeepAliveTime) * time.Second,
				Timeout: time.Duration(cfg.TiKVClient.GrpcKeepAliveTimeout) * time.Second,
			}),
		),
		pd.WithCustomTimeoutOption(time.Duration(cfg.PDClient.PDServerTimeout)*time.Second),
		pd.WithForwardingOption(config.GetGlobalConfig().EnableForwarding))
	if err != nil {
		return nil, errors.WithStack(err)
	}
	pdClient := &CodecPDClient{Client: util.InterceptedPDClient{Client: pdCli}}
	return pdClient, nil
}

// EnableTxnLocalLatches enables txn latch. It should be called before using
// the store to serve any requests.
func (s *KVStore) EnableTxnLocalLatches(size uint) {
	s.txnLatches = latch.NewScheduler(size)
}

// IsLatchEnabled is used by mockstore.TestConfig.
func (s *KVStore) IsLatchEnabled() bool {
	return s.txnLatches != nil
}

func (s *KVStore) runSafePointChecker() {
	defer s.wg.Done()
	d := gcSafePointUpdateInterval
	for {
		select {
		case spCachedTime := <-time.After(d):
			cachedSafePoint, err := loadSafePoint(s.GetSafePointKV())
			if err == nil {
				metrics.TiKVLoadSafepointCounter.WithLabelValues("ok").Inc()
				s.UpdateSPCache(cachedSafePoint, spCachedTime)
				d = gcSafePointUpdateInterval
			} else {
				metrics.TiKVLoadSafepointCounter.WithLabelValues("fail").Inc()
				logutil.BgLogger().Error("fail to load safepoint from pd", zap.Error(err))
				d = gcSafePointQuickRepeatInterval
			}
		case <-s.ctx.Done():
			return
		}
	}
}

// Begin a global transaction.
func (s *KVStore) Begin(opts ...TxnOption) (txn *transaction.KVTxn, err error) {
	options := &transaction.TxnOptions{}
	// Inject the options
	for _, opt := range opts {
		opt(options)
	}

	defer func() {
		if err == nil && txn != nil && options.MemoryFootprintChangeHook != nil {
			txn.SetMemoryFootprintChangeHook(options.MemoryFootprintChangeHook)
		}
	}()
	if options.TxnScope == "" {
		options.TxnScope = oracle.GlobalTxnScope
	}
	var (
		startTS uint64
	)
	if options.StartTS != nil {
		startTS = *options.StartTS
	} else {
		bo := retry.NewBackofferWithVars(context.Background(), transaction.TsoMaxBackoff, nil)
		startTS, err = s.getTimestampWithRetry(bo, options.TxnScope)
		if err != nil {
			return nil, err
		}
	}

	snapshot := txnsnapshot.NewTiKVSnapshot(s, startTS, s.nextReplicaReadSeed())
	return transaction.NewTiKVTxn(s, snapshot, startTS, options)
}

// DeleteRange delete all versions of all keys in the range[startKey,endKey) immediately.
// Be careful while using this API. This API doesn't keep recent MVCC versions, but will delete all versions of all keys
// in the range immediately. Also notice that frequent invocation to this API may cause performance problems to TiKV.
func (s *KVStore) DeleteRange(ctx context.Context, startKey []byte, endKey []byte, concurrency int) (completedRegions int, err error) {
	task := rangetask.NewDeleteRangeTask(s, startKey, endKey, concurrency)
	err = task.Execute(ctx)
	if err == nil {
		completedRegions = task.CompletedRegions()
	}
	return completedRegions, err
}

// GetSnapshot gets a snapshot that is able to read any data which data is <= the given ts.
// If the given ts is greater than the current TSO timestamp, the snapshot is not guaranteed
// to be consistent.
// Specially, it is useful to set ts to math.MaxUint64 to point get the latest committed data.
func (s *KVStore) GetSnapshot(ts uint64) *txnsnapshot.KVSnapshot {
	snapshot := txnsnapshot.NewTiKVSnapshot(s, ts, s.nextReplicaReadSeed())
	return snapshot
}

// Close store
func (s *KVStore) Close() error {
	s.close.Store(true)
	s.cancel()
	s.wg.Wait()

	s.oracle.Close()
	s.pdClient.Close()
	s.lockResolver.Close()

	if err := s.GetTiKVClient().Close(); err != nil {
		return err
	}

	if s.txnLatches != nil {
		s.txnLatches.Close()
	}
	s.regionCache.Close()

	if err := s.kv.Close(); err != nil {
		return err
	}
	return nil
}

// UUID return a unique ID which represents a Storage.
func (s *KVStore) UUID() string {
	return s.uuid
}

// CurrentTimestamp returns current timestamp with the given txnScope (local or global).
func (s *KVStore) CurrentTimestamp(txnScope string) (uint64, error) {
	bo := retry.NewBackofferWithVars(context.Background(), transaction.TsoMaxBackoff, nil)
	startTS, err := s.getTimestampWithRetry(bo, txnScope)
	if err != nil {
		return 0, err
	}
	return startTS, nil
}

// GetTimestampWithRetry returns latest timestamp.
func (s *KVStore) GetTimestampWithRetry(bo *Backoffer, scope string) (uint64, error) {
	return s.getTimestampWithRetry(bo, scope)
}

func (s *KVStore) getTimestampWithRetry(bo *Backoffer, txnScope string) (uint64, error) {
	if span := opentracing.SpanFromContext(bo.GetCtx()); span != nil && span.Tracer() != nil {
		span1 := span.Tracer().StartSpan("TiKVStore.getTimestampWithRetry", opentracing.ChildOf(span.Context()))
		defer span1.Finish()
		bo.SetCtx(opentracing.ContextWithSpan(bo.GetCtx(), span1))
	}

	for {
		startTS, err := s.oracle.GetTimestamp(bo.GetCtx(), &oracle.Option{TxnScope: txnScope})
		// mockGetTSErrorInRetry should wait MockCommitErrorOnce first, then will run into retry() logic.
		// Then mockGetTSErrorInRetry will return retryable error when first retry.
		// Before PR #8743, we don't cleanup txn after meet error such as error like: PD server timeout
		// This may cause duplicate data to be written.
		if val, e := util.EvalFailpoint("mockGetTSErrorInRetry"); e == nil && val.(bool) {
			if _, e := util.EvalFailpoint("mockCommitErrorOpt"); e != nil {
				err = tikverr.NewErrPDServerTimeout("mock PD timeout")
			}
		}

		if err == nil {
			return startTS, nil
		}
		err = bo.Backoff(retry.BoPDRPC, errors.Errorf("get timestamp failed: %v", err))
		if err != nil {
			return 0, err
		}
	}
}

func (s *KVStore) nextReplicaReadSeed() uint32 {
	return atomic.AddUint32(&s.replicaReadSeed, 1)
}

// GetOracle gets a timestamp oracle client.
func (s *KVStore) GetOracle() oracle.Oracle {
	return s.oracle
}

// GetPDClient returns the PD client.
func (s *KVStore) GetPDClient() pd.Client {
	return s.pdClient
}

// SupportDeleteRange gets the storage support delete range or not.
func (s *KVStore) SupportDeleteRange() (supported bool) {
	return !s.mock
}

// SendReq sends a request to locate.
func (s *KVStore) SendReq(bo *Backoffer, req *tikvrpc.Request, regionID locate.RegionVerID, timeout time.Duration) (*tikvrpc.Response, error) {
	sender := locate.NewRegionRequestSender(s.regionCache, s.GetTiKVClient())
	return sender.SendReq(bo, req, regionID, timeout)
}

// GetRegionCache returns the region cache instance.
func (s *KVStore) GetRegionCache() *locate.RegionCache {
	return s.regionCache
}

// GetLockResolver returns the lock resolver instance.
func (s *KVStore) GetLockResolver() *txnlock.LockResolver {
	return s.lockResolver
}

// Closed returns a channel that indicates if the store is closed.
func (s *KVStore) Closed() <-chan struct{} {
	return s.ctx.Done()
}

// GetSafePointKV returns the kv store that used for safepoint.
func (s *KVStore) GetSafePointKV() SafePointKV {
	return s.kv
}

// SetOracle resets the oracle instance.
func (s *KVStore) SetOracle(oracle oracle.Oracle) {
	s.oracle = oracle
}

// SetTiKVClient resets the client instance.
func (s *KVStore) SetTiKVClient(client Client) {
	s.clientMu.Lock()
	defer s.clientMu.Unlock()
	s.clientMu.client = client
}

// GetTiKVClient gets the client instance.
func (s *KVStore) GetTiKVClient() (client Client) {
	s.clientMu.RLock()
	defer s.clientMu.RUnlock()
	return s.clientMu.client
}

// GetMinSafeTS return the minimal safeTS of the storage with given txnScope.
func (s *KVStore) GetMinSafeTS(txnScope string) uint64 {
	stores := make([]*locate.Store, 0)
	allStores := s.regionCache.GetStoresByType(tikvrpc.TiKV)
	if txnScope != oracle.GlobalTxnScope {
		for _, store := range allStores {
			if store.IsLabelsMatch([]*metapb.StoreLabel{
				{
					Key:   DCLabelKey,
					Value: txnScope,
				},
			}) {
				stores = append(stores, store)
			}
		}
	} else {
		stores = allStores
	}
	return s.getMinSafeTSByStores(stores)
}

// Ctx returns ctx.
func (s *KVStore) Ctx() context.Context {
	return s.ctx
}

// IsClose checks whether the store is closed.
func (s *KVStore) IsClose() bool {
	return s.close.Load()
}

// WaitGroup returns wg
func (s *KVStore) WaitGroup() *sync.WaitGroup {
	return &s.wg
}

// TxnLatches returns txnLatches.
func (s *KVStore) TxnLatches() *latch.LatchesScheduler {
	return s.txnLatches
}

// GetClusterID returns store's cluster id.
func (s *KVStore) GetClusterID() uint64 {
	return s.clusterID
}

func (s *KVStore) getSafeTS(storeID uint64) (bool, uint64) {
	safeTS, ok := s.safeTSMap.Load(storeID)
	if !ok {
		return false, 0
	}
	return true, safeTS.(uint64)
}

// setSafeTS sets safeTs for store storeID, export for testing
func (s *KVStore) setSafeTS(storeID, safeTS uint64) {
	s.safeTSMap.Store(storeID, safeTS)
}

func (s *KVStore) getMinSafeTSByStores(stores []*locate.Store) uint64 {
	if val, err := util.EvalFailpoint("injectSafeTS"); err == nil {
		injectTS := val.(int)
		return uint64(injectTS)
	}
	minSafeTS := uint64(math.MaxUint64)
	// when there is no store, return 0 in order to let minStartTS become startTS directly
	if len(stores) < 1 {
		return 0
	}
	for _, store := range stores {
		ok, safeTS := s.getSafeTS(store.StoreID())
		if ok {
			if safeTS != 0 && safeTS < minSafeTS {
				minSafeTS = safeTS
			}
		} else {
			minSafeTS = 0
		}
	}
	return minSafeTS
}

func (s *KVStore) safeTSUpdater() {
	defer s.wg.Done()
	t := time.NewTicker(time.Second * 2)
	defer t.Stop()
	ctx, cancel := context.WithCancel(s.ctx)
	ctx = util.WithInternalSourceType(ctx, util.InternalTxnGC)
	defer cancel()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-t.C:
			s.updateSafeTS(ctx)
		}
	}
}

func (s *KVStore) updateSafeTS(ctx context.Context) {
	stores := s.regionCache.GetStoresByType(tikvrpc.TiKV)
	tikvClient := s.GetTiKVClient()
	wg := &sync.WaitGroup{}
	wg.Add(len(stores))
	for _, store := range stores {
		storeID := store.StoreID()
		storeAddr := store.GetAddr()
		go func(ctx context.Context, wg *sync.WaitGroup, storeID uint64, storeAddr string) {
			defer wg.Done()
			resp, err := tikvClient.SendRequest(ctx, storeAddr, tikvrpc.NewRequest(tikvrpc.CmdStoreSafeTS, &kvrpcpb.StoreSafeTSRequest{KeyRange: &kvrpcpb.KeyRange{
				StartKey: []byte(""),
				EndKey:   []byte(""),
			}}, kvrpcpb.Context{
				RequestSource: util.RequestSourceFromCtx(ctx),
			}), client.ReadTimeoutShort)
			storeIDStr := strconv.Itoa(int(storeID))
			if err != nil {
				metrics.TiKVSafeTSUpdateCounter.WithLabelValues("fail", storeIDStr).Inc()
				logutil.BgLogger().Debug("update safeTS failed", zap.Error(err), zap.Uint64("store-id", storeID))
				return
			}
			safeTS := resp.Resp.(*kvrpcpb.StoreSafeTSResponse).GetSafeTs()
			_, preSafeTS := s.getSafeTS(storeID)
			if preSafeTS > safeTS {
				metrics.TiKVSafeTSUpdateCounter.WithLabelValues("skip", storeIDStr).Inc()
				preSafeTSTime := oracle.GetTimeFromTS(preSafeTS)
				metrics.TiKVMinSafeTSGapSeconds.WithLabelValues(storeIDStr).Set(time.Since(preSafeTSTime).Seconds())
				return
			}
			s.setSafeTS(storeID, safeTS)
			metrics.TiKVSafeTSUpdateCounter.WithLabelValues("success", storeIDStr).Inc()
			safeTSTime := oracle.GetTimeFromTS(safeTS)
			metrics.TiKVMinSafeTSGapSeconds.WithLabelValues(storeIDStr).Set(time.Since(safeTSTime).Seconds())
		}(ctx, wg, storeID, storeAddr)
	}
	wg.Wait()
}

// Variables defines the variables used by TiKV storage.
type Variables = kv.Variables

// NewLockResolver is exported for other pkg to use, suppress unused warning.
var _ = NewLockResolver

// NewLockResolver creates a LockResolver.
// It is exported for other pkg to use. For instance, binlog service needs
// to determine a transaction's commit state.
func NewLockResolver(etcdAddrs []string, security config.Security, opts ...pd.ClientOption) (*txnlock.LockResolver, error) {
	pdCli, err := pd.NewClient(etcdAddrs, pd.SecurityOption{
		CAPath:   security.ClusterSSLCA,
		CertPath: security.ClusterSSLCert,
		KeyPath:  security.ClusterSSLKey,
	}, opts...)
	if err != nil {
		return nil, errors.WithStack(err)
	}
	pdCli = util.InterceptedPDClient{Client: pdCli}
	uuid := fmt.Sprintf("tikv-%v", pdCli.GetClusterID(context.TODO()))

	tlsConfig, err := security.ToTLSConfig()
	if err != nil {
		return nil, err
	}

	spkv, err := NewEtcdSafePointKV(etcdAddrs, tlsConfig)
	if err != nil {
		return nil, err
	}

	s, err := NewKVStore(uuid, locate.NewCodeCPDClient(pdCli), spkv, client.NewRPCClient(WithSecurity(security)))
	if err != nil {
		return nil, err
	}
	return s.lockResolver, nil
}

// TxnOption configures Transaction
type TxnOption func(*transaction.TxnOptions)

// WithTxnScope sets the TxnScope to txnScope
func WithTxnScope(txnScope string) TxnOption {
	return func(st *transaction.TxnOptions) {
		st.TxnScope = txnScope
	}
}

// WithStartTS sets the StartTS to startTS
func WithStartTS(startTS uint64) TxnOption {
	return func(st *transaction.TxnOptions) {
		st.StartTS = &startTS
	}
}

// TODO: remove once tidb and br are ready

// KVTxn contains methods to interact with a TiKV transaction.
type KVTxn = transaction.KVTxn

// BinlogWriteResult defines the result of prewrite binlog.
type BinlogWriteResult = transaction.BinlogWriteResult

// KVFilter is a filter that filters out unnecessary KV pairs.
type KVFilter = transaction.KVFilter

// SchemaLeaseChecker is used to validate schema version is not changed during transaction execution.
type SchemaLeaseChecker = transaction.SchemaLeaseChecker

// SchemaVer is the infoSchema which will return the schema version.
type SchemaVer = transaction.SchemaVer

// SchemaAmender is used by pessimistic transactions to amend commit mutations for schema change during 2pc.
type SchemaAmender = transaction.SchemaAmender

// MaxTxnTimeUse is the max time a Txn may use (in ms) from its begin to commit.
// We use it to abort the transaction to guarantee GC worker will not influence it.
const MaxTxnTimeUse = transaction.MaxTxnTimeUse
