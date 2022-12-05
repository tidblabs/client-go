package tenantcost

import (
	"context"
	"sync"
	"time"

	"github.com/pingcap/errors"
	"github.com/pingcap/kvproto/pkg/pdpb"
	"github.com/pingcap/log"
	"go.uber.org/zap"
)

// mainLoopUpdateInterval is the period at which we collect CPU usage and
// evaluate whether we need to send a new token request.
const mainLoopUpdateInterval = 1 * time.Second

// movingAvgFactor is the weight applied to a new "sample" of RU usage (with one
// sample per mainLoopUpdateInterval).
//
// If we want a factor of 0.5 per second, this should be:
//
//	0.5^(1 second / mainLoopUpdateInterval)
const movingAvgFactor = 0.5

const notifyFraction = 0.1

// If we have less than this many RUs to report, extend the reporting period to
// reduce load on the host cluster.
const consumptionReportingThreshold = 100

// The extended reporting period is this factor times the normal period.
const extendedReportingPeriodFactor = 4

const bufferRUs = 5000

type TokenBucketProvider interface {
	TokenBucket(
		ctx context.Context, in *pdpb.TokenBucketRequest,
	) (*pdpb.TokenBucketResponse, error)
}

const initialRquestUnits = 10000
const initialRate = 100

func newTenantSideCostController(
	tenantID uint64,
	provider TokenBucketProvider,
) (*tenantSideCostController, error) {
	c := &tenantSideCostController{
		tenantID:        tenantID,
		provider:        provider,
		responseChan:    make(chan *pdpb.TokenBucketResponse, 1),
		lowRUNotifyChan: make(chan struct{}, 1),
	}
	c.limiter = NewLimiter(initialRate, initialRquestUnits, c.lowRUNotifyChan)

	c.costCfg = DefaultConfig()
	return c, nil
}

// NewTenantSideCostController creates an object which implements the
// server.TenantSideCostController interface.
func NewTenantSideCostController(
	tenantID uint64, provider TokenBucketProvider,
) (*tenantSideCostController, error) {
	return newTenantSideCostController(tenantID, provider)
}

type tenantSideCostController struct {
	tenantID            uint64
	provider            TokenBucketProvider
	limiter             *Limiter
	instanceFingerprint string
	costCfg             Config

	mu struct {
		sync.Mutex

		consumption pdpb.Consumption
	}

	// responseChan is used to receive results from token bucket requests, which
	// are run in a separate goroutine. A nil response indicates an error.
	responseChan chan *pdpb.TokenBucketResponse

	// lowRUNotifyChan is used when the number of available RUs is running low and
	// we need to send an early token bucket request.
	lowRUNotifyChan chan struct{}

	// run contains the state that is updated by the main loop.
	run struct {
		now time.Time
		// cpuUsage is the last CPU usage of the instance returned by UserCPUSecs.
		cpuUsage float64
		// consumption stores the last value of mu.consumption.
		consumption pdpb.Consumption

		// targetPeriod stores the value of the TargetPeriodSetting setting at the
		// last update.
		targetPeriod time.Duration

		// initialRequestCompleted is set to true when the first token bucket
		// request completes successfully.
		initialRequestCompleted bool

		// requestInProgress is true if we are in the process of sending a request;
		// it gets set to false when we process the response (in the main loop),
		// even in error cases.
		requestInProgress bool

		// requestNeedsRetry is set if the last token bucket request encountered an
		// error. This triggers a retry attempt on the next tick.
		//
		// Note: requestNeedsRetry and requestInProgress are never true at the same
		// time.
		requestNeedsRetry bool

		lastRequestTime         time.Time
		lastReportedConsumption pdpb.Consumption

		lastDeadline time.Time
		lastRate     float64

		// avgRUPerSec is an exponentially-weighted moving average of the RU
		// consumption per second; used to estimate the RU requirements for the next
		// request.
		avgRUPerSec float64
		// lastSecRU is the consumption.RU value when avgRUPerSec was last updated.
		avgRUPerSecLastRU float64

		setupNotificationCh        <-chan time.Time
		setupNotificationThreshold float64
		setupNotificationTimer     *time.Timer

		fallbackRate      float64
		fallbackRateStart time.Time
	}
}

// Start is part of multitenant.TenantSideCostController.
func (c *tenantSideCostController) Start(
	ctx context.Context,
	instanceFingerprint string,
) error {
	if len(instanceFingerprint) == 0 {
		return errors.New("invalid SQLInstanceID")
	}
	c.instanceFingerprint = instanceFingerprint

	go c.mainLoop(ctx)
	return nil
}

func (c *tenantSideCostController) initRunState(ctx context.Context) {
	c.run.targetPeriod = 10 * time.Second

	now := time.Now()
	c.run.now = now
	c.run.cpuUsage = UserCPUSecs(ctx)
	c.run.lastRequestTime = now
	c.run.avgRUPerSec = initialRquestUnits / c.run.targetPeriod.Seconds()
}

const CPUUsageAllowance = 10 * time.Millisecond

// updateRunState is called whenever the main loop awakens and accounts for the
// CPU usage in the interim.
func (c *tenantSideCostController) updateRunState(ctx context.Context) {
	c.run.targetPeriod = 10 * time.Second

	newTime := time.Now()
	newCPUUsage := UserCPUSecs(ctx)
	// Update CPU consumption.
	deltaCPU := newCPUUsage - c.run.cpuUsage

	// Subtract any allowance that we consider free background usage.
	if deltaTime := newTime.Sub(c.run.now); deltaTime > 0 {
		deltaCPU -= CPUUsageAllowance.Seconds() * deltaTime.Seconds()
	}
	if deltaCPU < 0 {
		deltaCPU = 0
	}
	ru := deltaCPU * float64(c.costCfg.PodCPUSecond)

	// KV RUs are not included here, these metrics correspond only to the SQL pod.
	c.mu.Lock()
	c.mu.consumption.PodsCpuSeconds += deltaCPU
	c.mu.consumption.RU += ru
	newConsumption := c.mu.consumption
	c.mu.Unlock()

	c.run.now = newTime
	c.run.consumption = newConsumption
	c.run.cpuUsage = newCPUUsage

	c.limiter.RemoveTokens(newTime, float64(RequestUnit(ru)))
	log.Info("[tenant controllor] update run state, use cpu second", zap.Float64("deltaCPU", deltaCPU), zap.Float64("ru", ru), zap.Float64("newCPUUsage", newCPUUsage), zap.Float64("oldCPUUsage", c.run.cpuUsage), zap.Float64("remaining", c.limiter.AvailableTokens(newTime)), zap.Float64("limit", float64(c.limiter.Limit())))
}

// updateAvgRUPerSec is called exactly once per mainLoopUpdateInterval.
func (c *tenantSideCostController) updateAvgRUPerSec() {
	delta := c.run.consumption.RU - c.run.avgRUPerSecLastRU
	c.run.avgRUPerSec = movingAvgFactor*c.run.avgRUPerSec + (1-movingAvgFactor)*delta
	c.run.avgRUPerSecLastRU = c.run.consumption.RU
	log.Info("[tenant controllor] update avg ru per sec", zap.Float64("avgRUPerSec", c.run.avgRUPerSec), zap.Float64("avgRUPerSecLastRU", c.run.avgRUPerSecLastRU), zap.Any("consumption", c.run.consumption))
}

// shouldReportConsumption decides if it's time to send a token bucket request
// to report consumption.
func (c *tenantSideCostController) shouldReportConsumption() bool {
	if c.run.requestInProgress {
		return false
	}

	timeSinceLastRequest := c.run.now.Sub(c.run.lastRequestTime)
	if timeSinceLastRequest >= c.run.targetPeriod {
		consumptionToReport := c.run.consumption.RU - c.run.lastReportedConsumption.RU
		if consumptionToReport >= consumptionReportingThreshold {
			return true
		}
		if timeSinceLastRequest >= extendedReportingPeriodFactor*c.run.targetPeriod {
			return true
		}
	}

	return false
}

func (c *tenantSideCostController) sendTokenBucketRequest(ctx context.Context, source string) {
	deltaConsumption := c.run.consumption
	Sub(&deltaConsumption, &c.run.lastReportedConsumption)

	var requested float64

	if !c.run.initialRequestCompleted {
		requested = initialRquestUnits
	} else {
		requested = c.run.avgRUPerSec*c.run.targetPeriod.Seconds() + bufferRUs

		requested -= float64(c.limiter.AvailableTokens(c.run.now))
		if requested < 0 {
			requested = 0
		}
	}

	req := pdpb.TokenBucketRequest{
		TenantId:                    c.tenantID,
		InstanceFingerprint:         c.instanceFingerprint,
		ConsumptionSinceLastRequest: deltaConsumption,
		RequestedRU:                 requested,
		TargetRequestPeriodSeconds:  uint64(c.run.targetPeriod.Seconds()),
	}
	now := time.Now()
	log.Info("[tenant controllor] send token bucket request", zap.Time("now", now), zap.Any("req", req), zap.String("source", source))

	c.run.lastRequestTime = c.run.now
	c.run.lastReportedConsumption = c.run.consumption
	c.run.requestInProgress = true
	go func() {
		resp, err := c.provider.TokenBucket(ctx, &req)
		if err != nil {
			// Don't log any errors caused by the stopper canceling the context.
			if !errors.ErrorEqual(err, context.Canceled) {
				log.L().Sugar().Infof("TokenBucket RPC error: %v", err)
			}
			resp = nil
		} else if resp.Header.Error != nil {
			// This is a "logic" error which indicates a configuration problem on the
			// host side. We will keep retrying periodically.
			log.L().Sugar().Infof("TokenBucket error: %v", resp.Header.Error)
			resp = nil
		}
		log.Info("[tenant controllor] token bucket response", zap.Time("now", time.Now()), zap.Any("resp", resp), zap.String("source", source), zap.Duration("latency", time.Since(now)))
		c.responseChan <- resp
	}()
}

func (c *tenantSideCostController) handleTokenBucketResponse(
	ctx context.Context, resp *pdpb.TokenBucketResponse,
) {
	c.run.fallbackRate = resp.FallbackRate
	if !c.run.initialRequestCompleted {
		c.run.initialRequestCompleted = true
		// This is the first successful request. Take back the initial RUs that we
		// used to pre-fill the bucket.
		c.limiter.RemoveTokens(c.run.now, initialRquestUnits)
	}

	granted := resp.GrantedRU
	if granted == 0 {
		// We don't have any RUs to give back.
		if !c.run.fallbackRateStart.IsZero() {
			c.sendTokenBucketRequest(ctx, "resp_0_granted")
		}
		return
	}
	c.run.fallbackRateStart = time.Time{}

	if !c.run.lastDeadline.IsZero() {
		// If last request came with a trickle duration, we may have RUs that were
		// not made available to the bucket yet; throw them together with the newly
		// granted RUs.
		if since := c.run.lastDeadline.Sub(c.run.now); since > 0 {
			granted += c.run.lastRate * since.Seconds()
		}
	}
	if c.run.setupNotificationTimer != nil {
		c.run.setupNotificationTimer.Stop()
		c.run.setupNotificationTimer = nil
		c.run.setupNotificationCh = nil
	}
	notifyThreshold := granted * notifyFraction
	if notifyThreshold < bufferRUs {
		notifyThreshold = bufferRUs
	}

	var cfg tokenBucketReconfigureArgs
	if resp.TrickleDurationSeconds == 0 {
		cfg.NewTokens = granted
		cfg.NewRate = 0
		cfg.NotifyThreshold = notifyThreshold
		c.run.lastDeadline = time.Time{}
	} else {
		// We received a batch of tokens that can only be used over the
		// TrickleDuration. Set up the token bucket to notify us a bit before this
		// period elapses (unless we accumulate enough unused tokens, in which case
		// we get notified when the tokens are running low).
		deadline := c.run.now.Add(time.Duration(resp.TrickleDurationSeconds) * time.Second)
		cfg.NewRate = granted / float64(resp.TrickleDurationSeconds)

		timerDuration := resp.TrickleDurationSeconds - 1
		if timerDuration <= 0 {
			timerDuration = (resp.TrickleDurationSeconds + 1) / 2
		}
		c.run.setupNotificationTimer = time.NewTimer(time.Duration(timerDuration) * time.Second)
		c.run.setupNotificationCh = c.run.setupNotificationTimer.C
		c.run.setupNotificationThreshold = notifyThreshold

		c.run.lastDeadline = deadline
	}
	c.run.lastRate = cfg.NewRate
	c.limiter.Reconfigure(c.run.now, cfg)
	log.Info("[tenant controllor] update local token bucket", zap.Float64("granted", granted), zap.Float64("rate", cfg.NewRate), zap.Float64("tokens", cfg.NewTokens), zap.Float64("notifyThreshold", cfg.NotifyThreshold))

}

func (c *tenantSideCostController) mainLoop(ctx context.Context) {
	interval := mainLoopUpdateInterval
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	tickerCh := ticker.C

	c.initRunState(ctx)
	c.sendTokenBucketRequest(ctx, "init")

	for {
		select {
		case <-tickerCh:
			c.updateRunState(ctx)
			c.updateAvgRUPerSec()
			// Switch to the fallback rate, if necessary.
			if !c.run.fallbackRateStart.IsZero() && !c.run.now.Before(c.run.fallbackRateStart) {
				log.Info("[tenant controllor] switching to fallback rate", zap.Float64("rate", c.run.fallbackRate))
				c.limiter.SetLimitAt(c.run.now, Limit(c.run.fallbackRate))
				c.run.fallbackRateStart = time.Time{}
			}
			if c.run.requestNeedsRetry || c.shouldReportConsumption() {
				c.run.requestNeedsRetry = false
				c.sendTokenBucketRequest(ctx, "report")
			}
		case resp := <-c.responseChan:
			c.run.requestInProgress = false
			if resp != nil {
				c.updateRunState(ctx)
				c.handleTokenBucketResponse(ctx, resp)
			} else {
				// A nil response indicates a failure (which would have been logged).
				c.run.requestNeedsRetry = true
			}
		case <-c.run.setupNotificationCh:
			c.run.setupNotificationTimer = nil
			c.run.setupNotificationCh = nil

			c.updateRunState(ctx)
			c.limiter.SetupNotification(c.run.now, float64(c.run.setupNotificationThreshold))

		case <-c.lowRUNotifyChan:
			c.updateRunState(ctx)
			c.run.fallbackRateStart = c.run.now.Add(200 * time.Millisecond)
			if !c.run.requestInProgress {
				log.Warn("[tenant controllor] low RU notification", zap.Float64("threshold", c.run.setupNotificationThreshold), zap.Time("now", c.run.now), zap.Time("rate-start", c.run.fallbackRateStart))
				c.sendTokenBucketRequest(ctx, "low_ru")
			}
		case <-ctx.Done():

			return
		}
	}
}

// OnRequestWait is part of the multitenant.TenantSideKVInterceptor
// interface.
func (c *tenantSideCostController) OnRequestWait(
	ctx context.Context, info RequestInfo,
) error {
	return c.limiter.WaitN(ctx, int(c.costCfg.RequestCost(info)))
}

// OnResponse is part of the multitenant.TenantSideBatchInterceptor interface.
//
// the RequestCost to the bucket).
func (c *tenantSideCostController) OnResponse(
	ctx context.Context, req RequestInfo, resp ResponseInfo,
) {

	if resp.CPUTime() > 0 || resp.ReadBytes() > 0 {
		c.limiter.RemoveTokens(time.Now(), float64(c.costCfg.ResponseCost(resp)))
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if isWrite, writeBytes := req.IsWrite(); isWrite {
		c.mu.consumption.WriteRequests++
		c.mu.consumption.WriteBytes += uint64(writeBytes)
		writeRU := float64(c.costCfg.KVWriteCost(writeBytes))
		c.mu.consumption.RU += writeRU
	} else {
		c.mu.consumption.ReadRequests++
		readBytes := resp.ReadBytes()
		c.mu.consumption.ReadBytes += uint64(readBytes)
		readRU := float64(c.costCfg.KVReadCost(readBytes))
		c.mu.consumption.RU += readRU
	}
}
