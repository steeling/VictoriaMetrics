package common

import (
	"flag"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/VictoriaMetrics/VictoriaMetrics/app/vmstorage"
	"github.com/VictoriaMetrics/VictoriaMetrics/lib/bytesutil"
	"github.com/VictoriaMetrics/VictoriaMetrics/lib/fasttime"
	"github.com/VictoriaMetrics/VictoriaMetrics/lib/logger"
	"github.com/VictoriaMetrics/VictoriaMetrics/lib/procutil"
	"github.com/VictoriaMetrics/VictoriaMetrics/lib/prompbmarshal"
	"github.com/VictoriaMetrics/VictoriaMetrics/lib/storage"
	"github.com/VictoriaMetrics/VictoriaMetrics/lib/streamaggr"
	"github.com/VictoriaMetrics/metrics"
)

var (
	streamAggrConfig = flag.String("streamAggr.config", "", "Optional path to file with stream aggregation config. "+
		"See https://docs.victoriametrics.com/stream-aggregation.html . "+
		"See also -streamAggr.keepInput, -streamAggr.dropInput and -streamAggr.dedupInterval")
	streamAggrKeepInput = flag.Bool("streamAggr.keepInput", false, "Whether to keep all the input samples after the aggregation with -streamAggr.config. "+
		"By default, only aggregated samples are dropped, while the remaining samples are stored in the database. "+
		"See also -streamAggr.dropInput and https://docs.victoriametrics.com/stream-aggregation.html")
	streamAggrDropInput = flag.Bool("streamAggr.dropInput", false, "Whether to drop all the input samples after the aggregation with -streamAggr.config. "+
		"By default, only aggregated samples are dropped, while the remaining samples are stored in the database. "+
		"See also -streamAggr.keepInput and https://docs.victoriametrics.com/stream-aggregation.html")
	streamAggrDedupInterval = flag.Duration("streamAggr.dedupInterval", 0, "Input samples are de-duplicated with this interval before being aggregated "+
		"by stream aggregation. Only the last sample per each time series per each interval is aggregated if the interval is greater than zero. "+
		"See https://docs.victoriametrics.com/stream-aggregation.html")
)

var (
	saCfgReloaderStopCh chan struct{}
	saCfgReloaderWG     sync.WaitGroup

	saCfgReloads   = metrics.NewCounter(`vminsert_streamagg_config_reloads_total`)
	saCfgReloadErr = metrics.NewCounter(`vminsert_streamagg_config_reloads_errors_total`)
	saCfgSuccess   = metrics.NewGauge(`vminsert_streamagg_config_last_reload_successful`, nil)
	saCfgTimestamp = metrics.NewCounter(`vminsert_streamagg_config_last_reload_success_timestamp_seconds`)

	sasGlobal atomic.Pointer[streamaggr.Aggregators]
)

// CheckStreamAggrConfig checks config pointed by -stramaggr.config
func CheckStreamAggrConfig() error {
	if *streamAggrConfig == "" {
		return nil
	}
	pushNoop := func(tss []prompbmarshal.TimeSeries) {}
	opts := &streamaggr.Options{
		DedupInterval: *streamAggrDedupInterval,
	}
	sas, err := streamaggr.LoadFromFile(*streamAggrConfig, pushNoop, opts)
	if err != nil {
		return fmt.Errorf("error when loading -streamAggr.config=%q: %w", *streamAggrConfig, err)
	}
	sas.MustStop()
	return nil
}

// InitStreamAggr must be called after flag.Parse and before using the common package.
//
// MustStopStreamAggr must be called when stream aggr is no longer needed.
func InitStreamAggr() {
	saCfgReloaderStopCh = make(chan struct{})

	if *streamAggrConfig == "" {
		return
	}

	sighupCh := procutil.NewSighupChan()

	opts := &streamaggr.Options{
		DedupInterval: *streamAggrDedupInterval,
	}
	sas, err := streamaggr.LoadFromFile(*streamAggrConfig, pushAggregateSeries, opts)
	if err != nil {
		logger.Fatalf("cannot load -streamAggr.config=%q: %s", *streamAggrConfig, err)
	}
	sasGlobal.Store(sas)
	saCfgSuccess.Set(1)
	saCfgTimestamp.Set(fasttime.UnixTimestamp())

	// Start config reloader.
	saCfgReloaderWG.Add(1)
	go func() {
		defer saCfgReloaderWG.Done()
		for {
			select {
			case <-sighupCh:
			case <-saCfgReloaderStopCh:
				return
			}
			reloadStreamAggrConfig()
		}
	}()
}

func reloadStreamAggrConfig() {
	logger.Infof("reloading -streamAggr.config=%q", *streamAggrConfig)
	saCfgReloads.Inc()

	opts := &streamaggr.Options{
		DedupInterval: *streamAggrDedupInterval,
	}
	sasNew, err := streamaggr.LoadFromFile(*streamAggrConfig, pushAggregateSeries, opts)
	if err != nil {
		saCfgSuccess.Set(0)
		saCfgReloadErr.Inc()
		logger.Errorf("cannot reload -streamAggr.config=%q: use the previously loaded config; error: %s", *streamAggrConfig, err)
		return
	}
	sas := sasGlobal.Load()
	if !sasNew.Equal(sas) {
		sasOld := sasGlobal.Swap(sasNew)
		sasOld.MustStop()
		logger.Infof("successfully reloaded stream aggregation config at -streamAggr.config=%q", *streamAggrConfig)
	} else {
		logger.Infof("nothing changed in -streamAggr.config=%q", *streamAggrConfig)
		sasNew.MustStop()
	}
	saCfgSuccess.Set(1)
	saCfgTimestamp.Set(fasttime.UnixTimestamp())
}

// MustStopStreamAggr stops stream aggregators.
func MustStopStreamAggr() {
	close(saCfgReloaderStopCh)
	saCfgReloaderWG.Wait()

	sas := sasGlobal.Swap(nil)
	sas.MustStop()
}

type streamAggrCtx struct {
	mn      storage.MetricName
	tss     []prompbmarshal.TimeSeries
	labels  []prompbmarshal.Label
	samples []prompbmarshal.Sample
	buf     []byte
}

func (ctx *streamAggrCtx) Reset() {
	ctx.mn.Reset()

	clear(ctx.tss)
	ctx.tss = ctx.tss[:0]

	clear(ctx.labels)
	ctx.labels = ctx.labels[:0]

	ctx.samples = ctx.samples[:0]
	ctx.buf = ctx.buf[:0]
}

func (ctx *streamAggrCtx) push(mrs []storage.MetricRow, matchIdxs []byte) []byte {
	mn := &ctx.mn
	tss := ctx.tss
	labels := ctx.labels
	samples := ctx.samples
	buf := ctx.buf

	tssLen := len(tss)
	for _, mr := range mrs {
		if err := mn.UnmarshalRaw(mr.MetricNameRaw); err != nil {
			logger.Panicf("BUG: cannot unmarshal recently marshaled MetricName: %s", err)
		}

		labelsLen := len(labels)

		bufLen := len(buf)
		buf = append(buf, mn.MetricGroup...)
		metricGroup := bytesutil.ToUnsafeString(buf[bufLen:])
		labels = append(labels, prompbmarshal.Label{
			Name:  "__name__",
			Value: metricGroup,
		})

		for _, tag := range mn.Tags {
			bufLen = len(buf)
			buf = append(buf, tag.Key...)
			name := bytesutil.ToUnsafeString(buf[bufLen:])

			bufLen = len(buf)
			buf = append(buf, tag.Value...)
			value := bytesutil.ToUnsafeString(buf[bufLen:])
			labels = append(labels, prompbmarshal.Label{
				Name:  name,
				Value: value,
			})
		}

		samplesLen := len(samples)
		samples = append(samples, prompbmarshal.Sample{
			Timestamp: mr.Timestamp,
			Value:     mr.Value,
		})

		tss = append(tss, prompbmarshal.TimeSeries{
			Labels:  labels[labelsLen:],
			Samples: samples[samplesLen:],
		})
	}
	ctx.tss = tss
	ctx.labels = labels
	ctx.samples = samples
	ctx.buf = buf

	tss = tss[tssLen:]
	matchIdxs = bytesutil.ResizeNoCopyMayOverallocate(matchIdxs, len(tss))
	for i := 0; i < len(matchIdxs); i++ {
		matchIdxs[i] = 0
	}
	sas := sasGlobal.Load()
	matchIdxs = sas.Push(tss, matchIdxs)

	ctx.Reset()

	return matchIdxs
}

func pushAggregateSeries(tss []prompbmarshal.TimeSeries) {
	currentTimestamp := int64(fasttime.UnixTimestamp()) * 1000
	var ctx InsertCtx
	ctx.Reset(len(tss))
	ctx.skipStreamAggr = true
	for _, ts := range tss {
		labels := ts.Labels
		ctx.Labels = ctx.Labels[:0]
		for _, label := range labels {
			name := label.Name
			if name == "__name__" {
				name = ""
			}
			ctx.AddLabel(name, label.Value)
		}
		value := ts.Samples[0].Value
		if err := ctx.WriteDataPoint(nil, ctx.Labels, currentTimestamp, value); err != nil {
			logger.Errorf("cannot store aggregate series: %s", err)
			// Do not continue pushing the remaining samples, since it is likely they will return the same error.
			return
		}
	}
	// There is no need in limiting the number of concurrent calls to vmstorage.AddRows() here,
	// since the number of concurrent pushAggregateSeries() calls should be already limited by lib/streamaggr.
	if err := vmstorage.AddRows(ctx.mrs); err != nil {
		logger.Errorf("cannot flush aggregate series: %s", err)
	}
}
