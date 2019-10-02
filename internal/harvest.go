package internal

import (
	"strings"
	"sync"
	"time"
)

// Harvestable is something that can be merged into a Harvest.
type Harvestable interface {
	MergeIntoHarvest(h *Harvest)
}

// harvestTypes is a bit set used to indicate which data types are ready to be
// reported.
type harvestTypes uint

const (
	harvestMetricsTraces harvestTypes = 1 << iota
	harvestSpanEvents
	harvestCustomEvents
	harvestTxnEvents
	harvestErrorEvents
)

const (
	harvestTypesEvents = harvestSpanEvents | harvestCustomEvents | harvestTxnEvents | harvestErrorEvents
	harvestTypesAll    = harvestMetricsTraces | harvestTypesEvents
)

type harvestTimer struct {
	periods     map[harvestTypes]time.Duration
	lastHarvest map[harvestTypes]time.Time
}

func newHarvestTimer(now time.Time, periods map[harvestTypes]time.Duration) *harvestTimer {
	lastHarvest := make(map[harvestTypes]time.Time, len(periods))
	for tp := range periods {
		lastHarvest[tp] = now
	}
	return &harvestTimer{periods: periods, lastHarvest: lastHarvest}
}

func (timer *harvestTimer) ready(now time.Time) (ready harvestTypes) {
	for tp, period := range timer.periods {
		if deadline := timer.lastHarvest[tp].Add(period); now.After(deadline) {
			timer.lastHarvest[tp] = deadline
			ready |= tp
		}
	}
	return
}

// Harvest contains collected data.
type Harvest struct {
	timer *harvestTimer

	Metrics      *metricTable
	ErrorTraces  harvestErrors
	TxnTraces    *harvestTraces
	SlowSQLs     *slowQueries
	SpanEvents   *spanEvents
	CustomEvents *customEvents
	TxnEvents    *txnEvents
	ErrorEvents  *errorEvents
}

const (
	// txnEventPayloadlimit is the maximum number of events that should be
	// sent up in one post.
	txnEventPayloadlimit = 5000
)

// Ready returns a new Harvest which contains the data types ready for harvest,
// or nil if no data is ready for harvest.
func (h *Harvest) Ready(now time.Time) *Harvest {
	ready := &Harvest{}

	types := h.timer.ready(now)
	if 0 == types {
		return nil
	}

	if 0 != types&harvestCustomEvents {
		h.Metrics.addCount(customEventsSeen, h.CustomEvents.NumSeen(), forced)
		h.Metrics.addCount(customEventsSent, h.CustomEvents.NumSaved(), forced)
		ready.CustomEvents = h.CustomEvents
		h.CustomEvents = newCustomEvents(h.CustomEvents.capacity())
	}
	if 0 != types&harvestTxnEvents {
		h.Metrics.addCount(txnEventsSeen, h.TxnEvents.NumSeen(), forced)
		h.Metrics.addCount(txnEventsSent, h.TxnEvents.NumSaved(), forced)
		ready.TxnEvents = h.TxnEvents
		h.TxnEvents = newTxnEvents(h.TxnEvents.capacity())
	}
	if 0 != types&harvestErrorEvents {
		h.Metrics.addCount(errorEventsSeen, h.ErrorEvents.NumSeen(), forced)
		h.Metrics.addCount(errorEventsSent, h.ErrorEvents.NumSaved(), forced)
		ready.ErrorEvents = h.ErrorEvents
		h.ErrorEvents = newErrorEvents(h.ErrorEvents.capacity())
	}
	if 0 != types&harvestSpanEvents {
		h.Metrics.addCount(spanEventsSeen, h.SpanEvents.NumSeen(), forced)
		h.Metrics.addCount(spanEventsSent, h.SpanEvents.NumSaved(), forced)
		ready.SpanEvents = h.SpanEvents
		h.SpanEvents = newSpanEvents(h.SpanEvents.capacity())
	}
	// NOTE! Metrics must happen after the event harvest conditionals to
	// ensure that the metrics contain the event supportability metrics.
	if 0 != types&harvestMetricsTraces {
		ready.Metrics = h.Metrics
		ready.ErrorTraces = h.ErrorTraces
		ready.SlowSQLs = h.SlowSQLs
		ready.TxnTraces = h.TxnTraces
		h.Metrics = newMetricTable(maxMetrics, now)
		h.ErrorTraces = newHarvestErrors(maxHarvestErrors)
		h.SlowSQLs = newSlowQueries(maxHarvestSlowSQLs)
		h.TxnTraces = newHarvestTraces()
	}
	return ready
}

// Payloads returns a slice of payload creators.
func (h *Harvest) Payloads(splitLargeTxnEvents bool) (ps []PayloadCreator) {
	if nil == h {
		return
	}
	if nil != h.CustomEvents {
		ps = append(ps, h.CustomEvents)
	}
	if nil != h.ErrorEvents {
		ps = append(ps, h.ErrorEvents)
	}
	if nil != h.SpanEvents {
		ps = append(ps, h.SpanEvents)
	}
	if nil != h.Metrics {
		ps = append(ps, h.Metrics)
	}
	if nil != h.ErrorTraces {
		ps = append(ps, h.ErrorTraces)
	}
	if nil != h.TxnTraces {
		ps = append(ps, h.TxnTraces)
	}
	if nil != h.SlowSQLs {
		ps = append(ps, h.SlowSQLs)
	}
	if nil != h.TxnEvents {
		if splitLargeTxnEvents {
			ps = append(ps, h.TxnEvents.payloads(txnEventPayloadlimit)...)
		} else {
			ps = append(ps, h.TxnEvents)
		}
	}
	return
}

// NewHarvest returns a new Harvest.
func NewHarvest(now time.Time, reply *ConnectReply) *Harvest {
	return &Harvest{
		timer:        newHarvestTimer(now, reply.reportPeriods()),
		Metrics:      newMetricTable(maxMetrics, now),
		ErrorTraces:  newHarvestErrors(maxHarvestErrors),
		TxnTraces:    newHarvestTraces(),
		SlowSQLs:     newSlowQueries(maxHarvestSlowSQLs),
		SpanEvents:   newSpanEvents(reply.maxSpanEvents()),
		CustomEvents: newCustomEvents(reply.maxCustomEvents()),
		TxnEvents:    newTxnEvents(reply.maxTxnEvents()),
		ErrorEvents:  newErrorEvents(reply.maxErrorEvents()),
	}
}

var (
	trackMutex   sync.Mutex
	trackMetrics []string
)

// TrackUsage helps track which integration packages are used.
func TrackUsage(s ...string) {
	trackMutex.Lock()
	defer trackMutex.Unlock()

	m := "Supportability/" + strings.Join(s, "/")
	trackMetrics = append(trackMetrics, m)
}

func createTrackUsageMetrics(metrics *metricTable) {
	trackMutex.Lock()
	defer trackMutex.Unlock()

	for _, m := range trackMetrics {
		metrics.addSingleCount(m, forced)
	}
}

// CreateFinalMetrics creates extra metrics at harvest time.
func (h *Harvest) CreateFinalMetrics(reply *ConnectReply) {
	if nil == h {
		return
	}
	if nil == h.Metrics {
		return
	}

	h.Metrics.addSingleCount(instanceReporting, forced)

	// Configurable event harvest supportability metrics:
	// https://source.datanerd.us/agents/agent-specs/blob/master/Connect-LEGACY.md#event-harvest-config
	period := reply.configurablePeriod()
	h.Metrics.addDuration(supportReportPeriod, "", period, period, forced)
	h.Metrics.addValue(supportTxnEventLimit, "", float64(reply.maxTxnEvents()), forced)
	h.Metrics.addValue(supportCustomEventLimit, "", float64(reply.maxCustomEvents()), forced)
	h.Metrics.addValue(supportErrorEventLimit, "", float64(reply.maxErrorEvents()), forced)
	h.Metrics.addValue(supportSpanEventLimit, "", float64(reply.maxSpanEvents()), forced)

	createTrackUsageMetrics(h.Metrics)

	h.Metrics = h.Metrics.ApplyRules(reply.MetricRules)
}

// PayloadCreator is a data type in the harvest.
type PayloadCreator interface {
	// In the event of a rpm request failure (hopefully simply an
	// intermittent collector issue) the payload may be merged into the next
	// time period's harvest.
	Harvestable
	// Data prepares JSON in the format expected by the collector endpoint.
	// This method should return (nil, nil) if the payload is empty and no
	// rpm request is necessary.
	Data(agentRunID string, harvestStart time.Time) ([]byte, error)
	// EndpointMethod is used for the "method" query parameter when posting
	// the data.
	EndpointMethod() string
}

func supportMetric(metrics *metricTable, b bool, metricName string) {
	if b {
		metrics.addSingleCount(metricName, forced)
	}
}

// CreateTxnMetrics creates metrics for a transaction.
func CreateTxnMetrics(args *TxnData, metrics *metricTable) {
	withoutFirstSegment := removeFirstSegment(args.FinalName)

	// Duration Metrics
	var durationRollup string
	var totalTimeRollup string
	if args.IsWeb {
		durationRollup = webRollup
		totalTimeRollup = totalTimeWeb
		metrics.addDuration(dispatcherMetric, "", args.Duration, 0, forced)
	} else {
		durationRollup = backgroundRollup
		totalTimeRollup = totalTimeBackground
	}

	metrics.addDuration(args.FinalName, "", args.Duration, 0, forced)
	metrics.addDuration(durationRollup, "", args.Duration, 0, forced)

	metrics.addDuration(totalTimeRollup, "", args.TotalTime, args.TotalTime, forced)
	metrics.addDuration(totalTimeRollup+"/"+withoutFirstSegment, "", args.TotalTime, args.TotalTime, unforced)

	// Better CAT Metrics
	if cat := args.BetterCAT; cat.Enabled {
		caller := callerUnknown
		if nil != cat.Inbound {
			caller = cat.Inbound.payloadCaller
		}
		m := durationByCallerMetric(caller)
		metrics.addDuration(m.all, "", args.Duration, args.Duration, unforced)
		metrics.addDuration(m.webOrOther(args.IsWeb), "", args.Duration, args.Duration, unforced)

		// Transport Duration Metric
		if nil != cat.Inbound {
			d := cat.Inbound.TransportDuration
			m = transportDurationMetric(caller)
			metrics.addDuration(m.all, "", d, d, unforced)
			metrics.addDuration(m.webOrOther(args.IsWeb), "", d, d, unforced)
		}

		// CAT Error Metrics
		if args.HasErrors() {
			m = errorsByCallerMetric(caller)
			metrics.addSingleCount(m.all, unforced)
			metrics.addSingleCount(m.webOrOther(args.IsWeb), unforced)
		}

		supportMetric(metrics, args.AcceptPayloadSuccess, supportTracingAcceptSuccess)
		supportMetric(metrics, args.AcceptPayloadException, supportTracingAcceptException)
		supportMetric(metrics, args.AcceptPayloadParseException, supportTracingAcceptParseException)
		supportMetric(metrics, args.AcceptPayloadCreateBeforeAccept, supportTracingCreateBeforeAccept)
		supportMetric(metrics, args.AcceptPayloadIgnoredMultiple, supportTracingIgnoredMultiple)
		supportMetric(metrics, args.AcceptPayloadIgnoredVersion, supportTracingIgnoredVersion)
		supportMetric(metrics, args.AcceptPayloadUntrustedAccount, supportTracingAcceptUntrustedAccount)
		supportMetric(metrics, args.AcceptPayloadNullPayload, supportTracingAcceptNull)
		supportMetric(metrics, args.CreatePayloadSuccess, supportTracingCreatePayloadSuccess)
		supportMetric(metrics, args.CreatePayloadException, supportTracingCreatePayloadException)
	}

	// Apdex Metrics
	if args.Zone != ApdexNone {
		metrics.addApdex(apdexRollup, "", args.ApdexThreshold, args.Zone, forced)

		mname := apdexPrefix + withoutFirstSegment
		metrics.addApdex(mname, "", args.ApdexThreshold, args.Zone, unforced)
	}

	// Error Metrics
	if args.HasErrors() {
		metrics.addSingleCount(errorsRollupMetric.all, forced)
		metrics.addSingleCount(errorsRollupMetric.webOrOther(args.IsWeb), forced)
		metrics.addSingleCount(errorsPrefix+args.FinalName, forced)
	}

	// Queueing Metrics
	if args.Queuing > 0 {
		metrics.addDuration(queueMetric, "", args.Queuing, args.Queuing, forced)
	}
}
